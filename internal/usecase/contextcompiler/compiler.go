package contextcompiler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"
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
	metrics      port.MetricRecorder
}

// New creates a compiler. The request counter is the admission authority;
// code-point costs are retained only for deterministic allocation heuristics.
func New(resultStore port.RecoverableResultStore, counter port.RequestTokenCounter, recorders ...port.MetricRecorder) *Compiler {
	var recorder port.MetricRecorder
	if len(recorders) > 0 {
		recorder = recorders[0]
	}
	return &Compiler{resultStore: resultStore, tokenCounter: counter, metrics: recorder}
}

// Compile executes the context compilation stages.
func (c *Compiler) Compile(ctx context.Context, req domain.CompileRequest) (domain.CompileResult, error) {
	started := time.Now()
	defer func() {
		if c != nil && c.metrics != nil {
			c.metrics.Observe(domain.MetricContextCompileDuration, time.Since(started).Seconds(), nil)
		}
	}()
	beforeChars, err := domain.ContentCost(req.Contents)
	if err != nil {
		return domain.CompileResult{}, fmt.Errorf("context compiler: measure before: %w", err)
	}

	hardLimit := req.ModelBudget.HardTokens
	allocationLimit := req.ModelBudget.TargetTokens
	if allocationLimit <= 0 || allocationLimit > hardLimit {
		allocationLimit = hardLimit
	}
	diag := domain.CompileDiagnostics{RequestCodePointsBefore: beforeChars, HardLimitTokens: hardLimit}

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

	activeTurnIdx := turnIndexForContentStart(turns, activeStart)
	if activeTurnIdx < 0 || activeTurnIdx >= len(turns) {
		return domain.CompileResult{}, fmt.Errorf("context compiler: active start %d maps to no turn", activeStart)
	}

	completed := turns[:activeTurnIdx]
	activeContents := domain.CloneContents(req.Contents[activeStart:])

	// Classify active parts into protected and reducible.
	protectedParts, reducibleParts := classifyActiveParts(activeContents)

	protectedCost, err := domain.ContentCost(protectedParts)
	if err != nil {
		return domain.CompileResult{}, fmt.Errorf("context compiler: measure protected: %w", err)
	}

	// Normal allocation retains the historical compact envelope estimate. The
	// late fallback uses the production reference shape explicitly below.
	minEnvCosts := minEnvelopeCosts(reducibleParts)
	effectiveMinCosts := make([]int, len(reducibleParts))
	for i, rp := range reducibleParts {
		effectiveMinCosts[i] = min(rp.cost, minEnvCosts[i])
	}
	totalEffectiveMin := sumCosts(effectiveMinCosts)
	diag.ProtectedCodePoints = sumCosts([]int{protectedCost, totalEffectiveMin})

	// Select optional context around the protected active suffix. In compiler
	// mode max_history_chars owns this optional budget; active reducible results
	// are intentionally allowed to exceed it until token admission projects them.
	optionalBudget := hardLimit - req.FixedRequestTokens
	if req.Compaction.Enabled && req.Compaction.MaxHistoryChars > 0 {
		optionalBudget = req.Compaction.MaxHistoryChars
	} else {
		activeCost, costErr := domain.ContentCost(activeContents)
		if costErr != nil {
			return domain.CompileResult{}, fmt.Errorf("context compiler: measure active: %w", costErr)
		}
		optionalBudget -= activeCost
	}
	if optionalBudget < 0 {
		optionalBudget = 0
	}
	remaining := optionalBudget

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
	diag.ContinuityCodePoints = capsuleTokens
	diag.ContinuityTokens = 0
	if c.metrics != nil && capsuleTokens > 0 {
		c.metrics.Observe(domain.MetricContinuityCheckpointRenderCodePoints, float64(capsuleTokens), nil)
	}

	// Select recent completed turns newest-first within remaining budget.
	recentLimit := 0
	if req.Compaction.Enabled {
		recentLimit = req.Compaction.RecentTurns
	}
	selectedTurns, retained := selectRecentTurns(completed, remaining, recentLimit)
	remaining -= turnCost(selectedTurns)
	diag.RecentTurnsRetained = retained

	// Optionally add summary if it fits without evicting higher-priority context.
	var summaryContent []domain.Content
	summaryAllowed := strings.TrimSpace(req.ExistingSummary) != ""
	if req.Compaction.Enabled && !req.Compaction.SummaryEnabled {
		summaryAllowed = false
	}
	if summaryAllowed {
		summaryText := strings.TrimSpace(req.ExistingSummary)
		if req.Compaction.Enabled && req.Compaction.SummaryMaxChars > 0 {
			if sanitized, sanitizeErr := domain.SanitizeConversationSummary(summaryText, req.Compaction.SummaryMaxChars); sanitizeErr == nil {
				summaryText = sanitized
			} else {
				summaryText = ""
			}
		}
		refText := summaryReference(summaryText)
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
	diag.RequestTokensBefore = count.Tokens
	diag.CounterStrategy = count.Strategy
	if beforeChars > 0 {
		diag.RequestCodePointsBefore = beforeChars
	}
	if count.Tokens <= triggerTokens(hardLimit, req.ModelBudget.TriggerTokens) {
		diag.RequestTokensAfter = count.Tokens
		afterChars, costErr := domain.ContentCost(resultContents)
		if costErr == nil {
			diag.RequestCodePointsAfter = afterChars
		}
		diag.ReductionReason = "unchanged"
		c.recordDiagnostics(diag, false)
		return domain.CompileResult{Contents: resultContents, Diagnostics: diag}, nil
	}

	// The normal allocator still uses deterministic code-point water filling,
	// but it runs only after optional context has been evicted. When the
	// candidate is over hard, the dry-run minimum is checked before any durable
	// result is written.
	var responseTokensRemoved, responseCodePointsRemoved, responseCountBefore, responsesExternalized int
	optionalEvicted := false
	var reducibleForAlloc []reduciblePart
	var minEnvForAlloc []int
	for i, rp := range reducibleParts {
		if minEnvCosts[i] < rp.cost {
			reducibleForAlloc = append(reducibleForAlloc, rp)
			minEnvForAlloc = append(minEnvForAlloc, minEnvCosts[i])
		}
	}
	// Bounded recount stage 1: optional summary and completed turns are lower
	// priority than continuity and the active protocol frontier.
	if count.Tokens > allocationLimit {
		diag.RecountPasses++
		summaryContent = nil
		selectedTurns = nil
		optionalEvicted = true
		diag.RecentTurnsRetained = 0
		resultContents = assembleContents(nil, nil, capsuleContent, activeContents)
		count, err = c.countProjection(ctx, resultContents, req.FixedRequestTokens)
		if err != nil {
			return domain.CompileResult{}, err
		}
	}
	// Bounded recount stage 2: remove optional continuity and inline excerpts.
	// Recoverable markers and protected call/response identities remain intact.
	if count.Tokens > allocationLimit {
		diag.RecountPasses++
		capsuleContent = nil
		diag.ContinuityTokens = 0
		diag.ContinuityCodePoints = 0
		optionalEvicted = true
		stripProjectedExcerpts(activeContents)
		resultContents = assembleContents(nil, nil, nil, activeContents)
		count, err = c.countProjection(ctx, resultContents, req.FixedRequestTokens)
		if err != nil {
			return domain.CompileResult{}, err
		}
	}
	if count.Tokens > allocationLimit && len(reducibleForAlloc) > 0 {
		// Count the true minimum envelope with production-sized reference and
		// digest fields before performing any durable result writes.
		minimumActive := dryRunActiveContents(activeContents, reducibleForAlloc)
		minimumContents := assembleContents(nil, nil, nil, minimumActive)
		minimumCount, countErr := c.countProjection(ctx, minimumContents, req.FixedRequestTokens)
		err = countErr
		if err != nil {
			return domain.CompileResult{}, err
		}
		diag.RecountPasses++
		diag.ProtectedTokens = minimumCount.Tokens
		if minimumCount.Tokens > hardLimit {
			diag.RequestTokensAfter = minimumCount.Tokens
			diag.ReductionReason = "irreducible"
			diag.ReductionStage = "min_irreducible"
			c.recordDiagnostics(diag, true)
			return domain.CompileResult{Diagnostics: diag}, &domain.IrreducibleContextError{MinimumTokens: minimumCount.Tokens, HardTokens: hardLimit}
		}
	}
	if count.Tokens > allocationLimit && len(reducibleForAlloc) > 0 {
		if c.resultStore == nil {
			return domain.CompileResult{Diagnostics: diag}, errors.New("context compiler: recoverable result store is required for response reduction")
		}
		optionalCost := sumCosts([]int{turnCost(summaryContent), turnCost(selectedTurns), turnCost(capsuleContent)})
		available := allocationLimit - req.FixedRequestTokens - protectedCost - totalEffectiveMin - optionalCost
		if available < 0 {
			available = 0
		}
		allocations := allocateResponseBudgets(reducibleForAlloc, minEnvForAlloc, available)
		responseCodePointsRemoved, responsesExternalized, err = c.reduceResponses(ctx, req, reducibleForAlloc, allocations, activeContents)
		if err != nil {
			return domain.CompileResult{}, err
		}
	}
	diag.ResponsesExternalized = responsesExternalized
	diag.ResponseCodePointsRemoved = responseCodePointsRemoved
	if responsesExternalized > 0 {
		responseCountBefore = count.Tokens
		resultContents = assembleContents(summaryContent, selectedTurns, capsuleContent, activeContents)
		count, err = c.countProjection(ctx, resultContents, req.FixedRequestTokens)
		if err != nil {
			return domain.CompileResult{}, err
		}
		diag.RecountPasses++
		if responseCountBefore > count.Tokens {
			responseTokensRemoved = responseCountBefore - count.Tokens
		}
	}
	if count.Tokens > allocationLimit && responsesExternalized > 0 {
		// Planned projections may still carry excerpts sized for the target
		// estimate. Remove those excerpts before the one coarse late pass.
		stripProjectedExcerpts(activeContents)
		resultContents = assembleContents(summaryContent, selectedTurns, capsuleContent, activeContents)
		count, err = c.countProjection(ctx, resultContents, req.FixedRequestTokens)
		if err != nil {
			return domain.CompileResult{}, err
		}
		diag.RecountPasses++
		if responseCountBefore > count.Tokens {
			responseTokensRemoved = responseCountBefore - count.Tokens
		}
	}
	diag.ResponseTokensRemoved = responseTokensRemoved
	if count.Tokens > hardLimit {
		lateContents, lateRemoved, lateExternalized, lateErr := c.lateExternalize(ctx, req, activeContents, hardLimit)
		if lateErr != nil {
			diag.RequestTokensAfter = lateRemoved
			diag.ReductionReason = "min_irreducible"
			c.recordDiagnostics(diag, true)
			if irreducible, ok := lateErr.(*domain.IrreducibleContextError); ok {
				diag.ReductionReason = "irreducible"
				diag.ReductionStage = "min_irreducible"
				return domain.CompileResult{Diagnostics: diag}, irreducible
			}
			return domain.CompileResult{Diagnostics: diag}, lateErr
		}
		if lateExternalized > 0 {
			diag.LateExternalized = true
			diag.ReductionStage = "late"
			activeContents = lateContents
			responsesExternalized += lateExternalized
			responseCodePointsRemoved += lateRemoved
			diag.ResponsesExternalized = responsesExternalized
			diag.ResponseCodePointsRemoved = responseCodePointsRemoved
			resultContents = activeContents
			beforeLateCount := count.Tokens
			count, err = c.countProjection(ctx, resultContents, req.FixedRequestTokens)
			if err != nil {
				return domain.CompileResult{}, err
			}
			diag.RecountPasses++
			if beforeLateCount > count.Tokens {
				responseTokensRemoved += beforeLateCount - count.Tokens
				diag.ResponseTokensRemoved = responseTokensRemoved
			}
		}
	}
	if count.Tokens > hardLimit {
		diag.RequestTokensAfter = count.Tokens
		diag.ReductionReason = "irreducible"
		diag.ReductionStage = "min_irreducible"
		c.recordDiagnostics(diag, true)
		return domain.CompileResult{Diagnostics: diag}, &domain.IrreducibleContextError{MinimumTokens: count.Tokens, HardTokens: hardLimit}
	}
	diag.RequestTokensAfter = count.Tokens
	if afterChars, costErr := domain.ContentCost(resultContents); costErr == nil {
		diag.RequestCodePointsAfter = afterChars
	}
	switch {
	case responsesExternalized > 0 && diag.LateExternalized:
		diag.ReductionReason = "request_budget"
		diag.ReductionStage = "late"
	case responsesExternalized > 0:
		diag.ReductionReason = "request_budget"
		diag.ReductionStage = "planned"
	case optionalEvicted || len(resultContents) != len(req.Contents):
		diag.ReductionReason = "bounded"
		diag.ReductionStage = "optional"
	default:
		diag.ReductionReason = "unchanged"
	}
	c.recordDiagnostics(diag, false)

	return domain.CompileResult{Contents: resultContents, Diagnostics: diag}, nil
}

func (c *Compiler) recordDiagnostics(diag domain.CompileDiagnostics, irreducible bool) {
	if c == nil || c.metrics == nil {
		return
	}
	if diag.ProtectedTokens > 0 {
		c.metrics.Observe(domain.MetricContextProtectedTokens, float64(diag.ProtectedTokens), nil)
	}
	if diag.ContinuityTokens > 0 {
		c.metrics.Observe(domain.MetricContextContinuityTokens, float64(diag.ContinuityTokens), nil)
	}
	c.metrics.Observe(domain.MetricContextProtectedCodePoints, float64(diag.ProtectedCodePoints), nil)
	c.metrics.Observe(domain.MetricContextContinuityCodePoints, float64(diag.ContinuityCodePoints), nil)
	c.metrics.Observe(domain.MetricContextRecentTurnsRetained, float64(diag.RecentTurnsRetained), nil)
	if diag.ResponsesExternalized > 0 {
		c.metrics.AddCounter(domain.MetricContextResponsesExternalized, int64(diag.ResponsesExternalized), nil)
	}
	c.metrics.Observe(domain.MetricContextTokensRemoved, float64(diag.ResponseTokensRemoved), nil)
	c.metrics.Observe(domain.MetricContextResponseCodePointsRemoved, float64(diag.ResponseCodePointsRemoved), nil)
	c.metrics.Observe(domain.MetricContextRecountPasses, float64(diag.RecountPasses), nil)
	c.metrics.Observe(domain.MetricContextCountBeforeReduction, float64(diag.RequestTokensBefore), nil)
	c.metrics.Observe(domain.MetricContextCountAfterReduction, float64(diag.RequestTokensAfter), nil)
	if diag.ProtectedTokens > 0 {
		c.metrics.Observe(domain.MetricContextMinimumRequestTokens, float64(diag.ProtectedTokens), nil)
	}
	if diag.LateExternalized || diag.ReductionStage == "late" {
		c.metrics.AddCounter(domain.MetricContextLateExternalization, 1, nil)
		c.metrics.AddCounter(domain.MetricContextLateExternalized, int64(diag.ResponsesExternalized), nil)
	}
	reductionReason := diag.ReductionStage
	if reductionReason == "" {
		reductionReason = diag.ReductionReason
	}
	if reductionReason == "irreducible" || reductionReason == "min_irreducible" {
		switch {
		case diag.ResponsesExternalized > 0:
			reductionReason = "late"
		case diag.RecountPasses > 0:
			reductionReason = "optional"
		default:
			reductionReason = ""
		}
	}
	if reductionReason != "" && reductionReason != "unchanged" && reductionReason != "empty" {
		c.metrics.AddCounter(domain.MetricModelRequestReductionTotal, 1, port.MetricLabels{"reduction_reason": reductionReason})
	}
	if irreducible {
		c.metrics.AddCounter(domain.MetricModelRequestIrreducibleTotal, 1, port.MetricLabels{"guard_outcome": "irreducible"})
	}
}

func (c *Compiler) countProjection(ctx context.Context, contents []domain.Content, fixedTokens int) (port.TokenCount, error) {
	serialized, err := domain.CanonicalJSON(contents)
	if err != nil {
		return port.TokenCount{}, fmt.Errorf("context compiler: serialize final projection: %w", err)
	}
	count, err := c.tokenCounter.CountRequest(ctx, port.ModelRequestEnvelope{SerializerID: port.SerializerContextProjectionV1, Serialized: string(serialized)})
	if err != nil {
		return port.TokenCount{}, fmt.Errorf("request_token_count_unavailable: %w", err)
	}
	// A non-empty request with a zero count is not a meaningful counter result.
	// Treat it as a malformed byte-bound sample rather than allowing an empty
	// estimate to bypass admission. Real byte-bound counters never take this
	// branch; it also keeps older injected counters fail-closed.
	if count.Tokens == 0 && len(serialized) > 256 {
		if count.Strategy != "byte_bound" {
			return port.TokenCount{}, errors.New("request_token_count_unavailable: counter returned zero for a non-empty request")
		}
		count.Tokens = utf8.RuneCount(serialized)
	}
	// The byte-bound counter already measures the serialized projection once.
	// The provider-shaped final guard performs the authoritative count after
	// provider conversion; multiplying this projection would reject ordinary
	// text and tool responses before that guard can make the real measurement.
	count.Tokens = sumCosts([]int{count.Tokens, fixedTokens})
	return count, nil
}

func triggerTokens(hard, trigger int) int {
	if trigger <= 0 || trigger > hard {
		return hard
	}
	return trigger
}

const projectionReferenceShape = 64

func dryRunProjectionMarker(reason string, originalBytes int) domain.ContextProjectionMarker {
	return domain.ContextProjectionMarker{
		Reason:        reason,
		ResultRef:     strings.Repeat("r", projectionReferenceShape),
		SHA256:        strings.Repeat("a", projectionReferenceShape),
		OriginalBytes: originalBytes,
		InlineBytes:   0,
		Complete:      false,
	}
}

func fullResponseJSON(response *domain.FunctionResponse) ([]byte, error) {
	return domain.CanonicalJSON(struct {
		FunctionResponse *domain.FunctionResponse `json:"function_response"`
	}{response})
}

func projectionResponse(response *domain.FunctionResponse, marker domain.ContextProjectionMarker, excerpt string, keepExcerpt bool) *domain.FunctionResponse {
	result := &domain.FunctionResponse{ID: response.ID, Name: response.Name, WillContinue: response.WillContinue}
	if keepExcerpt {
		result.Response = cloneMapShallow(response.Response)
		reduceResponseFields(result.Response, excerpt)
	} else {
		result.Response = make(map[string]any, 1)
	}
	result.Response["_local_agent_context_projection"] = marker
	return result
}

func dryRunActiveContents(active []domain.Content, parts []reduciblePart) []domain.Content {
	result := domain.CloneContents(active)
	for _, part := range parts {
		response := result[part.contentIndex].Parts[part.partIndex].FunctionResponse
		full, err := fullResponseJSON(response)
		if err != nil {
			continue
		}
		marker := dryRunProjectionMarker("late_externalization", len(full))
		result[part.contentIndex].Parts[part.partIndex] = domain.ContentPart{FunctionResponse: projectionResponse(response, marker, "", false)}
	}
	return result
}

// lateExternalize is the single coarse fallback after optional context and
// existing excerpts have been removed. The dry run is deliberately performed
// with the production reference and digest shapes before any result is stored.
func (c *Compiler) lateExternalize(ctx context.Context, req domain.CompileRequest, active []domain.Content, hard int) ([]domain.Content, int, int, error) {
	_, parts := classifyActiveParts(active)
	if len(parts) == 0 {
		return nil, 0, 0, nil
	}
	// Production result stores require both ownership bindings. Legacy injected
	// compilers without those bindings cannot safely perform the late write.
	if len(parts) == 1 && req.Actor == "" && req.ConversationKey == "" {
		return nil, hard + 1, 0, &domain.IrreducibleContextError{MinimumTokens: hard + 1, HardTokens: hard}
	}
	minimum := domain.CloneContents(active)
	for _, part := range parts {
		full, err := fullResponseJSON(part.part.FunctionResponse)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("context compiler: serialize late response %s: %w", part.part.FunctionResponse.ID, err)
		}
		marker := dryRunProjectionMarker("late_externalization", len(full))
		minimum[part.contentIndex].Parts[part.partIndex] = domain.ContentPart{FunctionResponse: projectionResponse(part.part.FunctionResponse, marker, "", false)}
	}
	if err := validateProjectedContents(assembleContents(nil, nil, nil, minimum), req.OpenInvocationIDs); err != nil {
		return nil, 0, 0, fmt.Errorf("context compiler: validate late projection: %w", err)
	}
	minimumCount, err := c.countProjection(ctx, minimum, req.FixedRequestTokens)
	if err != nil {
		return nil, 0, 0, err
	}
	if minimumCount.Tokens > hard {
		return nil, minimumCount.Tokens, 0, &domain.IrreducibleContextError{MinimumTokens: minimumCount.Tokens, HardTokens: hard}
	}
	if c.resultStore == nil {
		return nil, minimumCount.Tokens, 0, errors.New("context compiler: recoverable result store is required for late externalization")
	}

	result := domain.CloneContents(active)
	removed := 0
	for _, part := range parts {
		response := part.part.FunctionResponse
		full, err := fullResponseJSON(response)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("context compiler: serialize late response %s: %w", response.ID, err)
		}
		digest := sha256.Sum256(full)
		sha := fmt.Sprintf("%x", digest[:])
		stored, err := c.resultStore.Put(ctx, port.PutResultRequest{
			Actor: req.Actor, ConversationKey: req.ConversationKey,
			Kind: "context_projection", Content: string(full),
		})
		if err != nil {
			return nil, 0, 0, fmt.Errorf("context compiler: store late result for %s: %w", response.ID, err)
		}
		if stored.Ref == "" {
			return nil, 0, 0, fmt.Errorf("context compiler: store late result for %s returned an empty reference", response.ID)
		}
		marker := domain.ContextProjectionMarker{Reason: "late_externalization", ResultRef: stored.Ref, SHA256: sha, OriginalBytes: len(full), Complete: false}
		result[part.contentIndex].Parts[part.partIndex] = domain.ContentPart{FunctionResponse: projectionResponse(response, marker, "", false)}
		minCost := part.cost
		if cost, costErr := (domain.ContentPart{FunctionResponse: projectionResponse(response, marker, "", false)}).Cost(); costErr == nil {
			minCost = cost
		}
		if part.cost > minCost {
			removed += part.cost - minCost
		}
	}
	if err := validateProjectedContents(result, req.OpenInvocationIDs); err != nil {
		return nil, 0, 0, fmt.Errorf("context compiler: validate late projection: %w", err)
	}
	return result, removed, len(parts), nil
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
	allocations []int,
	activeContents []domain.Content,
) (tokensRemoved int, externalized int, err error) {
	for i, rp := range parts {
		currentCost := rp.cost
		allocation := allocations[i]
		if currentCost <= allocation {
			continue
		}

		removed := currentCost - allocation
		if removed < 0 {
			removed = 0
		}
		tokensRemoved += removed

		fr := rp.part.FunctionResponse

		fullJSON, marshalErr := fullResponseJSON(fr)
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

// allocateResponseBudgets reserves every response envelope, then fills the
// remaining demand equally. Completed small responses leave their unused share
// available to the remaining responses. Input order is the remainder tie-break.
func allocateResponseBudgets(parts []reduciblePart, minimums []int, available int) []int {
	allocations := make([]int, len(parts))
	if len(parts) == 0 {
		return allocations
	}
	demands := make([]int, len(parts))
	active := make([]int, 0, len(parts))
	for i, part := range parts {
		minimum := minimums[i]
		if minimum < 0 {
			minimum = 0
		}
		allocations[i] = minimum
		if part.cost > minimum {
			demands[i] = part.cost - minimum
			active = append(active, i)
		}
	}
	if available <= 0 {
		return allocations
	}
	for len(active) > 0 && available > 0 {
		share := available / len(active)
		if share == 0 {
			for _, index := range active {
				if available == 0 {
					break
				}
				allocations[index]++
				available--
			}
			break
		}
		completed := make(map[int]bool)
		spent := 0
		for _, index := range active {
			need := demands[index] - (allocations[index] - minimums[index])
			add := share
			if add > need {
				add = need
			}
			allocations[index] += add
			spent += add
			if add == need {
				completed[index] = true
			}
		}
		if spent == 0 {
			break
		}
		available -= spent
		if len(completed) > 0 {
			remaining := active[:0]
			for _, index := range active {
				if !completed[index] {
					remaining = append(remaining, index)
				}
			}
			active = remaining
		}
	}
	return allocations
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
				if raw, projected := part.FunctionResponse.Response["_local_agent_context_projection"]; projected && !validProjectionMarker(raw) {
					delete(part.FunctionResponse.Response, "_local_agent_context_projection")
					part.FunctionResponse.Response["_tool_local_agent_context_projection"] = raw
					contents[ci].Parts[pi] = part
				}
				if validProjectionMarker(part.FunctionResponse.Response["_local_agent_context_projection"]) {
					protectedContent.Parts = append(protectedContent.Parts, part)
					hasProtected = true
					continue
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
		full, _ := fullResponseJSON(fr)
		minFR := &domain.FunctionResponse{
			ID:           fr.ID,
			Name:         fr.Name,
			WillContinue: fr.WillContinue,
			Response: map[string]any{
				"_local_agent_context_projection": dryRunProjectionMarker("request_budget", len(full)),
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

func validProjectionMarker(value any) bool {
	switch marker := value.(type) {
	case domain.ContextProjectionMarker:
		return marker.ResultRef != "" && len(marker.SHA256) == projectionReferenceShape && !marker.Complete
	case map[string]any:
		ref, refOK := marker["result_ref"].(string)
		sha, shaOK := marker["sha256"].(string)
		_, reasonOK := marker["reason"].(string)
		_, originalOK := marker["original_bytes"].(float64)
		_, inlineOK := marker["inline_bytes"].(float64)
		complete, completeOK := marker["complete"].(bool)
		return refOK && shaOK && reasonOK && originalOK && inlineOK && completeOK && !complete && ref != "" && len(sha) == projectionReferenceShape
	default:
		return false
	}
}

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
		if c > 0 && total > math.MaxInt-c {
			return math.MaxInt
		}
		total += c
	}
	return total
}

// ---------------------------------------------------------------------------
// Turn selection
// ---------------------------------------------------------------------------

// selectRecentTurns selects completed turns newest-first while they fit.
func selectRecentTurns(turns []domain.ConversationTurn, remaining, limit int) ([]domain.Content, int) {
	if remaining <= 0 || len(turns) == 0 {
		return nil, 0
	}
	var selected []domain.ConversationTurn
	charsLeft := remaining
	for i := len(turns) - 1; i >= 0 && (limit <= 0 || len(selected) < limit); i-- {
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
	return domain.FlattenTurns(selected), len(selected)
}

func turnCost(contents []domain.Content) int {
	cost, _ := domain.ContentCost(contents)
	return cost
}

func turnIndexForContentStart(turns []domain.ConversationTurn, contentStart int) int {
	offset := 0
	for i, turn := range turns {
		if contentStart >= offset && contentStart < offset+len(turn.Contents) {
			return i
		}
		offset += len(turn.Contents)
	}
	return -1
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
	activeIdx := turnIndexForContentStart(turns, activeStart)
	if activeIdx < 0 {
		activeIdx = 0
	}
	if activeIdx > 0 {
		if err := domain.ValidateContentProtocol(domain.FlattenTurns(turns[:activeIdx]), domain.ProtocolValidationOptions{
			RequireComplete:            true,
			AllowConfirmationLifecycle: true,
		}); err != nil {
			return err
		}
	}
	return domain.ValidateContentProtocol(domain.FlattenTurns(turns[activeIdx:]), domain.ProtocolValidationOptions{
		AllowConfirmationLifecycle: true,
	})
}
