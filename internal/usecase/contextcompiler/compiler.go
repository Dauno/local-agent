package contextcompiler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// Compiler implements port.ContextCompiler by producing a bounded model-facing
// projection. It externalizes oversized FunctionResponse payloads via a
// RecoverableResultStore while preserving protocol identity and ordering.
type Compiler struct {
	resultStore  port.RecoverableResultStore
	tokenCounter port.RequestTokenCounter
}

// New creates a compiler. tokenCounter is available for final-guard
// serialized-token counting; the internal budget tracking uses code points.
func New(resultStore port.RecoverableResultStore, counter port.RequestTokenCounter) *Compiler {
	return &Compiler{resultStore: resultStore, tokenCounter: counter}
}

// Compile executes the context compilation stages.
func (c *Compiler) Compile(ctx context.Context, req domain.CompileRequest) (domain.CompileResult, error) {
	beforeChars, err := domain.ContentCost(req.Contents)
	if err != nil {
		return domain.CompileResult{}, fmt.Errorf("context compiler: measure before: %w", err)
	}

	hardLimit := req.ModelBudget.HardTokens
	allocationLimit := req.ModelBudget.TargetTokens
	if allocationLimit <= 0 || allocationLimit > hardLimit {
		allocationLimit = hardLimit
	}
	diag := domain.CompileDiagnostics{
		RequestTokensBefore: beforeChars,
		HardLimitTokens:     hardLimit,
	}

	turns, activeStart, err := domain.ClassifyConversationTurns(req.Contents,
		domain.TurnClassificationOptions{OpenInvocationIDs: req.OpenInvocationIDs})
	if err != nil {
		return domain.CompileResult{}, fmt.Errorf("context compiler: classify turns: %w", err)
	}
	if len(turns) == 0 {
		diag.RequestTokensAfter = 0
		diag.ReductionReason = "empty"
		return domain.CompileResult{Contents: nil, Diagnostics: diag}, nil
	}

	activeTurnIdx := turnIndexForContentStart(turns, activeStart, 0)
	if activeTurnIdx < 0 || activeTurnIdx >= len(turns) {
		return domain.CompileResult{}, fmt.Errorf("context compiler: active start %d maps to no turn", activeStart)
	}

	completed := turns[:activeTurnIdx]
	activeContents := cloneContents(req.Contents[activeStart:])

	// Classify active parts into protected and reducible.
	protectedParts, reducibleParts := classifyActiveParts(activeContents)

	protectedCost, err := domain.ContentCost(protectedParts)
	if err != nil {
		return domain.CompileResult{}, fmt.Errorf("context compiler: measure protected: %w", err)
	}

	// Minimum envelope cost for each reducible response (identity + empty marker).
	// Use min(currentCost, minEnvCost) because we never externalize a response
	// whose minimal envelope costs more than the original.
	minEnvCosts := minEnvelopeCosts(reducibleParts)
	effectiveMinCosts := make([]int, len(reducibleParts))
	for i, rp := range reducibleParts {
		effectiveMinCosts[i] = minVal(rp.cost, minEnvCosts[i])
	}
	totalEffectiveMin := sumCosts(effectiveMinCosts)

	minimumRequired := req.FixedRequestTokens + protectedCost + totalEffectiveMin
	if minimumRequired > hardLimit {
		return domain.CompileResult{}, &domain.IrreducibleContextError{
			MinimumTokens: minimumRequired,
			HardTokens:    hardLimit,
		}
	}
	if allocationLimit < minimumRequired {
		allocationLimit = minimumRequired
	}

	available := allocationLimit - req.FixedRequestTokens - protectedCost - totalEffectiveMin
	diag.ProtectedTokens = protectedCost + totalEffectiveMin

	// For budget allocation, only consider responses where minEnvCost < currentCost.
	var reducibleForAlloc []reduciblePart
	var minEnvForAlloc []int
	for i, rp := range reducibleParts {
		if minEnvCosts[i] < rp.cost {
			reducibleForAlloc = append(reducibleForAlloc, rp)
			minEnvForAlloc = append(minEnvForAlloc, minEnvCosts[i])
		}
	}

	var responseTokensRemoved int
	var responsesExternalized int

	if len(reducibleForAlloc) > 0 {
		perResponseBudget := available / len(reducibleForAlloc)
		responseTokensRemoved, responsesExternalized, err = c.reduceResponses(
			ctx, req, reducibleForAlloc, minEnvForAlloc, perResponseBudget, activeContents,
		)
		if err != nil {
			return domain.CompileResult{}, err
		}
	}

	// Compute reduced active suffix cost.
	reducedActiveCost, err := domain.ContentCost(activeContents)
	if err != nil {
		return domain.CompileResult{}, fmt.Errorf("context compiler: measure reduced active: %w", err)
	}

	remaining := hardLimit - req.FixedRequestTokens - reducedActiveCost
	if remaining < 0 {
		remaining = 0
	}

	// Inject continuity capsule if it renders to non-empty text and fits.
	var capsuleContent []domain.Content
	capsuleTokens := 0
	rendered := domain.RenderContinuityCapsule(req.Continuity, remaining)
	if rendered != "" {
		candidate := []domain.Content{{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: rendered}}}}
		candidateCost, costErr := domain.ContentCost(candidate)
		if costErr == nil && candidateCost <= remaining {
			capsuleContent = candidate
			capsuleTokens = candidateCost
			remaining -= candidateCost
		}
	}
	diag.ContinuityTokens = capsuleTokens

	// Select recent completed turns newest-first within remaining budget.
	selectedTurns, retained := selectRecentTurns(completed, remaining)
	remaining -= turnCost(selectedTurns)
	diag.RecentTurnsRetained = retained

	// Optionally add summary if it fits without evicting higher-priority context.
	var summaryContent []domain.Content
	if strings.TrimSpace(req.ExistingSummary) != "" {
		refText := summaryReference(req.ExistingSummary)
		candidate := []domain.Content{{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: refText}}}}
		candidateCost, costErr := domain.ContentCost(candidate)
		if costErr == nil && candidateCost <= remaining {
			summaryContent = candidate
		}
	}

	resultContents := assembleContents(summaryContent, selectedTurns, capsuleContent, activeContents)

	// Validate projected protocol.
	if err := validateProjectedContents(resultContents, req.OpenInvocationIDs); err != nil {
		return domain.CompileResult{}, fmt.Errorf("context compiler: validate projection: %w", err)
	}

	if c.tokenCounter == nil {
		return domain.CompileResult{}, errors.New("context compiler: request token counter is required")
	}
	count, err := c.countProjection(ctx, resultContents, req.FixedRequestTokens)
	if err != nil {
		return domain.CompileResult{}, err
	}
	// Bounded recount stage 1: optional summary and completed turns are lower
	// priority than continuity and the active protocol frontier.
	if count.Tokens > hardLimit {
		summaryContent = nil
		selectedTurns = nil
		diag.RecentTurnsRetained = 0
		resultContents = assembleContents(nil, nil, capsuleContent, activeContents)
		count, err = c.countProjection(ctx, resultContents, req.FixedRequestTokens)
		if err != nil {
			return domain.CompileResult{}, err
		}
	}
	// Bounded recount stage 2: remove optional continuity and inline excerpts.
	// Recoverable markers and protected call/response identities remain intact.
	if count.Tokens > hardLimit {
		capsuleContent = nil
		diag.ContinuityTokens = 0
		stripProjectedExcerpts(activeContents)
		resultContents = assembleContents(nil, nil, nil, activeContents)
		count, err = c.countProjection(ctx, resultContents, req.FixedRequestTokens)
		if err != nil {
			return domain.CompileResult{}, err
		}
	}
	if count.Tokens > hardLimit {
		return domain.CompileResult{}, &domain.IrreducibleContextError{MinimumTokens: count.Tokens, HardTokens: hardLimit}
	}
	diag.RequestTokensAfter = count.Tokens
	diag.ResponsesExternalized = responsesExternalized
	diag.ResponseTokensRemoved = responseTokensRemoved
	switch {
	case responsesExternalized > 0:
		diag.ReductionReason = "request_budget"
	case len(resultContents) != len(req.Contents):
		diag.ReductionReason = "bounded"
	default:
		diag.ReductionReason = "unchanged"
	}

	return domain.CompileResult{Contents: resultContents, Diagnostics: diag}, nil
}

func (c *Compiler) countProjection(ctx context.Context, contents []domain.Content, fixedTokens int) (port.TokenCount, error) {
	serialized, err := domain.CanonicalJSON(contents)
	if err != nil {
		return port.TokenCount{}, fmt.Errorf("context compiler: serialize final projection: %w", err)
	}
	count, err := c.tokenCounter.CountRequest(ctx, port.ModelRequestEnvelope{SerializerID: "context-projection-v1", Serialized: string(serialized)})
	if err != nil {
		return port.TokenCount{}, fmt.Errorf("request_token_count_unavailable: %w", err)
	}
	// Domain projection JSON can become JSON-escaped string content during
	// provider conversion. A 2x byte bound remains conservative for that second
	// serialization; the provider-shaped guard is still authoritative.
	if count.Strategy == "byte_bound" {
		count.Tokens *= 2
	}
	count.Tokens += fixedTokens
	return count, nil
}

func assembleContents(summary, recent, capsule, active []domain.Content) []domain.Content {
	result := make([]domain.Content, 0, len(summary)+len(recent)+len(capsule)+len(active))
	result = append(result, summary...)
	result = append(result, recent...)
	result = append(result, capsule...)
	return append(result, active...)
}

func stripProjectedExcerpts(contents []domain.Content) {
	for ci := range contents {
		for pi := range contents[ci].Parts {
			response := contents[ci].Parts[pi].FunctionResponse
			if response == nil {
				continue
			}
			if _, projected := response.Response["_local_agent_context_projection"]; !projected {
				continue
			}
			for key, value := range response.Response {
				if key == "_local_agent_context_projection" {
					continue
				}
				if text, ok := value.(string); ok && len(text) > 256 {
					response.Response[key] = ""
					continue
				}
				reflected := reflect.ValueOf(value)
				if reflected.IsValid() && (reflected.Kind() == reflect.Map || reflected.Kind() == reflect.Slice || reflected.Kind() == reflect.Array) {
					response.Response[key] = nil
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Response reduction
// ---------------------------------------------------------------------------

type reduciblePart struct {
	contentIndex int
	partIndex    int
	part         domain.ContentPart
	cost         int
}

// reduceResponses externalizes oversized FunctionResponse payloads. It
// modifies activeContents in place. Returns tokens removed and count.
func (c *Compiler) reduceResponses(
	ctx context.Context,
	req domain.CompileRequest,
	parts []reduciblePart,
	minEnvCosts []int,
	perResponseBudget int,
	activeContents []domain.Content,
) (tokensRemoved int, externalized int, err error) {
	for i, rp := range parts {
		currentCost := rp.cost
		allocation := minEnvCosts[i] + perResponseBudget
		if currentCost <= allocation {
			continue
		}

		removed := currentCost - allocation
		if removed < 0 {
			removed = 0
		}
		tokensRemoved += removed

		fr := rp.part.FunctionResponse

		fullJSON, marshalErr := domain.CanonicalJSON(struct {
			FunctionResponse *domain.FunctionResponse `json:"function_response"`
		}{fr})
		if marshalErr != nil {
			return 0, 0, fmt.Errorf("context compiler: serialize response %s: %w", fr.ID, marshalErr)
		}
		digest := sha256.Sum256(fullJSON)
		fullSHA256 := fmt.Sprintf("%x", digest[:])

		putReq := port.PutResultRequest{
			Actor:           req.Actor,
			ConversationKey: req.ConversationKey,
			Kind:            "context_projection",
			Content:         string(fullJSON),
		}
		stored, storeErr := c.resultStore.Put(ctx, putReq)
		if storeErr != nil {
			return 0, 0, fmt.Errorf("context compiler: store result for %s: %w", fr.ID, storeErr)
		}

		// Measure the minimum envelope cost (identity + marker, no inline text).
		placeholderMarker := domain.ContextProjectionMarker{
			Reason:        "request_budget",
			ResultRef:     stored.Ref,
			SHA256:        fullSHA256,
			OriginalBytes: len(fullJSON),
			InlineBytes:   0,
			Complete:      false,
		}
		minFR := &domain.FunctionResponse{
			ID:           fr.ID,
			Name:         fr.Name,
			WillContinue: fr.WillContinue,
			Response:     map[string]any{"_local_agent_context_projection": placeholderMarker},
		}
		minCost, costErr := domain.ContentPart{FunctionResponse: minFR}.Cost()
		if costErr != nil {
			minCost = 300
		}

		inlineBudget := allocation - minCost
		if inlineBudget < 0 {
			inlineBudget = 0
		}

		// Extract primary text from response.
		respText := extractResponseText(fr.Response)
		excerpt := truncateToCodePoints(respText, inlineBudget)

		// Build the reduced response with truncated text and marker.
		reducedResp := cloneMapShallow(fr.Response)
		reduceResponseFields(reducedResp, excerpt)
		reducedResp["_local_agent_context_projection"] = domain.ContextProjectionMarker{
			Reason:        "request_budget",
			ResultRef:     stored.Ref,
			SHA256:        fullSHA256,
			OriginalBytes: len(fullJSON),
			InlineBytes:   utf8.RuneCountInString(excerpt),
			Complete:      false,
		}
		reducedFR := &domain.FunctionResponse{
			ID:           fr.ID,
			Name:         fr.Name,
			WillContinue: fr.WillContinue,
			Response:     reducedResp,
		}

		activeContents[rp.contentIndex].Parts[rp.partIndex] = domain.ContentPart{
			FunctionResponse: reducedFR,
		}
		externalized++
	}
	return tokensRemoved, externalized, nil
}

// ---------------------------------------------------------------------------
// Classification helpers
// ---------------------------------------------------------------------------

// classifyActiveParts splits active contents into protected content and
// reducible FunctionResponse parts. User text, model text, function calls,
// and confirmation responses are protected. Non-confirmation FunctionResponses
// in any role are reducible.
func classifyActiveParts(contents []domain.Content) (protected []domain.Content, reducible []reduciblePart) {
	protected = make([]domain.Content, 0, len(contents))
	for ci, content := range contents {
		protectedContent := domain.Content{Role: content.Role, Parts: make([]domain.ContentPart, 0, len(content.Parts))}
		hasProtected := false
		hasReducible := false
		for pi, part := range content.Parts {
			if part.FunctionResponse != nil && part.FunctionResponse.Name != domain.ConfirmationFunctionName {
				if raw, spoofed := part.FunctionResponse.Response["_local_agent_context_projection"]; spoofed {
					delete(part.FunctionResponse.Response, "_local_agent_context_projection")
					part.FunctionResponse.Response["_tool_local_agent_context_projection"] = raw
					contents[ci].Parts[pi] = part
				}
				cost, err := part.Cost()
				if err != nil {
					cost = 0
				}
				reducible = append(reducible, reduciblePart{
					contentIndex: ci,
					partIndex:    pi,
					part:         part,
					cost:         cost,
				})
				hasReducible = true
			} else {
				protectedContent.Parts = append(protectedContent.Parts, part)
				hasProtected = true
			}
		}
		if hasReducible && !hasProtected {
			// All parts are reducible; the content skeleton is still protected.
			protected = append(protected, domain.Content{Role: content.Role, Parts: nil})
		} else if hasProtected {
			protected = append(protected, protectedContent)
		} else {
			protected = append(protected, domain.Content{Role: content.Role, Parts: nil})
		}
	}
	return protected, reducible
}

// minEnvelopeCosts returns the minimum cost for each reducible response when
// reduced to identity fields plus a projection marker.
func minEnvelopeCosts(parts []reduciblePart) []int {
	costs := make([]int, len(parts))
	for i, rp := range parts {
		fr := rp.part.FunctionResponse
		minFR := &domain.FunctionResponse{
			ID:           fr.ID,
			Name:         fr.Name,
			WillContinue: fr.WillContinue,
			Response: map[string]any{
				"_local_agent_context_projection": domain.ContextProjectionMarker{
					Reason:   "request_budget",
					Complete: false,
				},
			},
		}
		cost, err := domain.ContentPart{FunctionResponse: minFR}.Cost()
		if err != nil {
			cost = 300
		}
		costs[i] = cost
	}
	return costs
}

// ---------------------------------------------------------------------------
// Text and map helpers
// ---------------------------------------------------------------------------

// extractResponseText returns the primary text payload from a response map.
func extractResponseText(response map[string]any) string {
	for _, key := range []string{"text", "content", "response"} {
		if v, ok := response[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	encoded, err := domain.CanonicalJSON(response)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// reduceResponseFields preserves scalar metadata while removing bulk values
// that have already been persisted behind the projection marker.
func reduceResponseFields(m map[string]any, excerpt string) {
	const maxMetaLen = 256
	metaKeys := map[string]bool{
		"path": true, "sha256": true, "sha": true, "digest": true,
		"status": true, "kind": true, "type": true,
		"id": true, "name": true,
		"_local_agent_context_projection": true,
	}
	for k, v := range m {
		if metaKeys[strings.ToLower(k)] {
			continue
		}
		if text, ok := v.(string); ok {
			if len(text) > maxMetaLen {
				m[k] = excerpt
			}
			continue
		}
		value := reflect.ValueOf(v)
		if value.IsValid() && (value.Kind() == reflect.Map || value.Kind() == reflect.Slice || value.Kind() == reflect.Array) {
			m[k] = nil
		}
	}
}

// cloneMapShallow creates a shallow copy of a map suitable for response
// manipulation.
func cloneMapShallow(input map[string]any) map[string]any {
	output := make(map[string]any, len(input)+2)
	for k, v := range input {
		output[k] = v
	}
	return output
}

// truncateToCodePoints returns the first maxCodePoints Unicode code points of s.
func truncateToCodePoints(s string, maxCodePoints int) string {
	if maxCodePoints <= 0 || s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxCodePoints {
		return s
	}
	return string(runes[:maxCodePoints])
}

func sumCosts(costs []int) int {
	total := 0
	for _, c := range costs {
		total += c
	}
	return total
}

func minVal(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Turn selection
// ---------------------------------------------------------------------------

// selectRecentTurns selects completed turns newest-first while they fit.
func selectRecentTurns(turns []domain.ConversationTurn, remaining int) ([]domain.Content, int) {
	if remaining <= 0 || len(turns) == 0 {
		return nil, 0
	}
	var selected []domain.ConversationTurn
	charsLeft := remaining
	for i := len(turns) - 1; i >= 0; i-- {
		turn := turns[i]
		if !turn.Closed {
			continue
		}
		if turn.CharCount > charsLeft {
			break
		}
		selected = append(selected, turn.Clone())
		charsLeft -= turn.CharCount
	}
	// Reverse to chronological order.
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}
	return flattenTurns(selected), len(selected)
}

func turnCost(contents []domain.Content) int {
	cost, _ := domain.ContentCost(contents)
	return cost
}

func flattenTurns(turns []domain.ConversationTurn) []domain.Content {
	var result []domain.Content
	for _, turn := range turns {
		result = append(result, cloneContents(turn.Contents)...)
	}
	return result
}

func turnIndexForContentStart(turns []domain.ConversationTurn, contentStart, prefixContents int) int {
	offset := prefixContents
	for i, turn := range turns {
		if contentStart >= offset && contentStart < offset+len(turn.Contents) {
			return i
		}
		offset += len(turn.Contents)
	}
	return -1
}

func cloneContents(contents []domain.Content) []domain.Content {
	clone := make([]domain.Content, len(contents))
	for i, c := range contents {
		clone[i] = c.Clone()
	}
	return clone
}

// ---------------------------------------------------------------------------
// Summary rendering
// ---------------------------------------------------------------------------

func summaryReference(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return "[UNTRUSTED CONVERSATION SUMMARY REFERENCE]\n" + text + "\n[/UNTRUSTED CONVERSATION SUMMARY REFERENCE]"
}

// ---------------------------------------------------------------------------
// Protocol validation
// ---------------------------------------------------------------------------

func validateProjectedContents(contents []domain.Content, openInvocationIDs map[string]struct{}) error {
	turns, activeStart, err := domain.ClassifyConversationTurns(contents,
		domain.TurnClassificationOptions{OpenInvocationIDs: openInvocationIDs})
	if err != nil {
		return fmt.Errorf("validate projected history: %w", err)
	}
	if len(turns) == 0 {
		return nil
	}
	activeIdx := turnIndexForContentStart(turns, activeStart, 0)
	if activeIdx < 0 {
		activeIdx = 0
	}
	if activeIdx > 0 {
		if err := domain.ValidateContentProtocol(flattenTurns(turns[:activeIdx]), false); err != nil {
			return err
		}
	}
	return domain.ValidateContentProtocol(flattenTurns(turns[activeIdx:]), true)
}
