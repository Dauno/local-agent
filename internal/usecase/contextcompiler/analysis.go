package contextcompiler

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type serializedResponse struct {
	canonicalJSON []byte
	cost          int
}

// reduciblePart carries the complete analysis for one response. cost is a
// code-point cost, not a token count.
type reduciblePart struct {
	contentIndex  int
	partIndex     int
	part          domain.ContentPart
	canonicalJSON []byte
	digest        string
	cost          int
	minimumCost   int
}

type projectionIndex struct {
	contentIndex int
	partIndex    int
}

// compilationState is passed between explicit compiler phases. Each phase
// returns a new state value so active content ownership stays explicit.
type compilationState struct {
	request domain.CompileRequest

	hardLimit       int
	allocationLimit int

	turns     []domain.ConversationTurn
	reducible []reduciblePart

	summary []domain.Content
	recent  []domain.Content
	capsule []domain.Content
	active  []domain.Content

	protectedCost    int
	totalMinimumCost int

	result domain.CompileResult
	count  port.TokenCount

	optionalEvicted bool

	diagnostics              domain.CompileDiagnostics
	exposeDiagnosticsOnError bool
	diagnosticsIrreducible   bool
}

func analyzeCompilation(req domain.CompileRequest) (compilationState, error) {
	return analyzeCompilationWithSerializer(req, fullResponseJSON)
}

type responseSerializer func(*domain.FunctionResponse) ([]byte, error)

func analyzeCompilationWithSerializer(req domain.CompileRequest, serialize responseSerializer) (compilationState, error) {
	if serialize == nil {
		serialize = fullResponseJSON
	}
	state := compilationState{
		request:         req,
		hardLimit:       req.ModelBudget.HardTokens,
		allocationLimit: req.ModelBudget.TargetTokens,
	}

	inputContents := domain.CloneContents(req.Contents)
	if err := normalizeProjectionKeys(inputContents); err != nil {
		return state, fmt.Errorf("context compiler: normalize reserved response keys: %w", err)
	}
	beforeCodePoints, serialized, err := measureContents(inputContents, serialize)
	if err != nil {
		return state, fmt.Errorf("context compiler: measure before: %w", err)
	}
	state.diagnostics = domain.CompileDiagnostics{
		RequestCodePointsBefore: beforeCodePoints,
		HardLimitTokens:         state.hardLimit,
	}
	if state.allocationLimit <= 0 || state.allocationLimit > state.hardLimit {
		state.allocationLimit = state.hardLimit
	}

	turns, activeStart, err := domain.ClassifyConversationTurns(inputContents,
		domain.TurnClassificationOptions{OpenInvocationIDs: req.OpenInvocationIDs})
	if err != nil {
		return state, fmt.Errorf("context compiler: classify turns: %w", err)
	}
	state.turns = turns
	if len(turns) == 0 {
		state.diagnostics.RequestTokensAfter = 0
		state.diagnostics.ReductionReason = "empty"
		return state, nil
	}

	activeTurnIdx := turnIndexForContentStart(turns, activeStart)
	if activeTurnIdx < 0 || activeTurnIdx >= len(turns) {
		return state, fmt.Errorf("context compiler: active start %d maps to no turn", activeStart)
	}
	completed := turns[:activeTurnIdx]
	active := domain.CloneContents(inputContents[activeStart:])
	protected, reducible := classifyActivePartsWithSerializations(active, activeStart, serialized)
	state.active = active
	state.reducible = reducible

	protectedCost, err := domain.ContentCost(protected)
	if err != nil {
		return state, fmt.Errorf("context compiler: measure protected: %w", err)
	}
	state.protectedCost = protectedCost
	minimumCosts := minEnvelopeCosts(reducible)
	effectiveMinimumCosts := make([]int, len(reducible))
	for i, part := range reducible {
		effectiveMinimumCosts[i] = min(part.cost, minimumCosts[i])
	}
	state.totalMinimumCost = sumCosts(effectiveMinimumCosts)
	state.diagnostics.ProtectedCodePoints = sumCosts([]int{protectedCost, state.totalMinimumCost})

	optionalBudget := state.hardLimit - req.FixedRequestTokens
	if req.Compaction.Enabled && req.Compaction.MaxHistoryChars > 0 {
		optionalBudget = req.Compaction.MaxHistoryChars
	} else {
		activeCost, costErr := contentCostWithPlans(active, reducible)
		if costErr != nil {
			return state, fmt.Errorf("context compiler: measure active: %w", costErr)
		}
		optionalBudget -= activeCost
	}
	if optionalBudget < 0 {
		optionalBudget = 0
	}
	remaining := optionalBudget

	// The continuity cost is measured in Unicode code points. The legacy
	// ContinuityTokens diagnostic remains zero for metric compatibility.
	rendered := domain.RenderContinuityCapsule(req.Continuity, remaining)
	capsuleCodePoints := 0
	if rendered != "" {
		candidate := []domain.Content{{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: rendered}}}}
		candidateCost, costErr := domain.ContentCost(candidate)
		if costErr == nil && candidateCost <= remaining {
			state.capsule = candidate
			capsuleCodePoints = candidateCost
			remaining -= candidateCost
		}
	}
	state.diagnostics.ContinuityCodePoints = capsuleCodePoints
	state.diagnostics.ContinuityTokens = 0

	recentLimit := 0
	if req.Compaction.Enabled {
		recentLimit = req.Compaction.RecentTurns
	}
	state.recent, state.diagnostics.RecentTurnsRetained = selectRecentTurns(completed, remaining, recentLimit)
	remaining -= turnCost(state.recent)

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
		candidate := []domain.Content{{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: summaryReference(summaryText)}}}}
		candidateCost, costErr := domain.ContentCost(candidate)
		if costErr == nil && candidateCost <= remaining {
			state.summary = candidate
		}
	}
	return state, nil
}

func measureContents(contents []domain.Content, serialize responseSerializer) (int, map[projectionIndex]serializedResponse, error) {
	serialized := make(map[projectionIndex]serializedResponse)
	total := 0
	for contentIndex, content := range contents {
		for partIndex, part := range content.Parts {
			if part.FunctionResponse != nil {
				encoded, err := serialize(part.FunctionResponse)
				if err != nil {
					return 0, nil, err
				}
				cost := utf8.RuneCount(encoded)
				serialized[projectionIndex{contentIndex: contentIndex, partIndex: partIndex}] = serializedResponse{
					canonicalJSON: encoded,
					cost:          cost,
				}
				total += cost
				continue
			}
			cost, err := part.Cost()
			if err != nil {
				return 0, nil, err
			}
			total += cost
		}
	}
	return total, serialized, nil
}

func fullResponseJSON(response *domain.FunctionResponse) ([]byte, error) {
	return domain.CanonicalJSON(struct {
		FunctionResponse *domain.FunctionResponse `json:"function_response"`
	}{response})
}

func classifyActiveParts(contents []domain.Content) (protected []domain.Content, reducible []reduciblePart) {
	serialized := make(map[projectionIndex]serializedResponse)
	for contentIndex, content := range contents {
		for partIndex, part := range content.Parts {
			if part.FunctionResponse == nil {
				continue
			}
			encoded, err := fullResponseJSON(part.FunctionResponse)
			if err != nil {
				continue
			}
			serialized[projectionIndex{contentIndex: contentIndex, partIndex: partIndex}] = serializedResponse{
				canonicalJSON: encoded,
				cost:          utf8.RuneCount(encoded),
			}
		}
	}
	return classifyActivePartsWithSerializations(contents, 0, serialized)
}

func classifyActivePartsWithSerializations(contents []domain.Content, sourceOffset int, serialized map[projectionIndex]serializedResponse) (protected []domain.Content, reducible []reduciblePart) {
	protected = make([]domain.Content, 0, len(contents))
	for contentIndex, content := range contents {
		protectedContent := domain.Content{Role: content.Role, Parts: make([]domain.ContentPart, 0, len(content.Parts))}
		hasProtected := false
		hasReducible := false
		for partIndex, part := range content.Parts {
			if part.FunctionResponse != nil && part.FunctionResponse.Name != domain.ConfirmationFunctionName {
				serializedResponse := serialized[projectionIndex{contentIndex: sourceOffset + contentIndex, partIndex: partIndex}]
				plan := reduciblePart{
					contentIndex:  contentIndex,
					partIndex:     partIndex,
					part:          part,
					canonicalJSON: serializedResponse.canonicalJSON,
					cost:          serializedResponse.cost,
				}
				if len(plan.canonicalJSON) > 0 {
					digest := sha256.Sum256(plan.canonicalJSON)
					plan.digest = fmt.Sprintf("%x", digest[:])
				}
				plan.minimumCost = minimumResponseCost(part.FunctionResponse, "request_budget", len(plan.canonicalJSON))
				reducible = append(reducible, plan)
				hasReducible = true
			} else {
				protectedContent.Parts = append(protectedContent.Parts, part)
				hasProtected = true
			}
		}
		if hasReducible && !hasProtected {
			// Keep the role skeleton so function-call protocol positions remain stable.
			protected = append(protected, domain.Content{Role: content.Role, Parts: nil})
		} else if hasProtected {
			protected = append(protected, protectedContent)
		} else {
			protected = append(protected, domain.Content{Role: content.Role, Parts: nil})
		}
	}
	return protected, reducible
}

func contentCostWithPlans(contents []domain.Content, parts []reduciblePart) (int, error) {
	plans := make(map[projectionIndex]int, len(parts))
	for _, part := range parts {
		plans[projectionIndex{contentIndex: part.contentIndex, partIndex: part.partIndex}] = part.cost
	}
	total := 0
	for contentIndex, content := range contents {
		for partIndex, part := range content.Parts {
			if part.FunctionResponse != nil {
				if cost, ok := plans[projectionIndex{contentIndex: contentIndex, partIndex: partIndex}]; ok {
					total += cost
					continue
				}
			}
			cost, err := part.Cost()
			if err != nil {
				return 0, err
			}
			total += cost
		}
	}
	return total, nil
}

func minEnvelopeCosts(parts []reduciblePart) []int {
	costs := make([]int, len(parts))
	for i, part := range parts {
		costs[i] = part.minimumCost
	}
	return costs
}

func minimumResponseCost(response *domain.FunctionResponse, reason string, originalBytes int) int {
	minimumResponse := &domain.FunctionResponse{
		ID:           response.ID,
		Name:         response.Name,
		WillContinue: response.WillContinue,
		Response: map[string]any{
			projectionMarkerKey: dryRunProjectionMarker(reason, originalBytes),
		},
	}
	cost, err := (domain.ContentPart{FunctionResponse: minimumResponse}).Cost()
	if err != nil {
		return 300
	}
	return cost
}

// normalizeProjectionKeys moves reserved projection keys from the initial
// input into the tool-data namespace. A collision fails closed.
func normalizeProjectionKeys(contents []domain.Content) error {
	for contentIndex := range contents {
		for partIndex := range contents[contentIndex].Parts {
			response := contents[contentIndex].Parts[partIndex].FunctionResponse
			if response == nil {
				continue
			}
			if _, reserved := response.Response[projectionMarkerKey]; !reserved {
				continue
			}
			if _, collision := response.Response[toolProjectionMarkerKey]; collision {
				return fmt.Errorf("reserved response key collision at content %d part %d", contentIndex, partIndex)
			}
			response.Response[toolProjectionMarkerKey] = response.Response[projectionMarkerKey]
			delete(response.Response, projectionMarkerKey)
		}
	}
	return nil
}
