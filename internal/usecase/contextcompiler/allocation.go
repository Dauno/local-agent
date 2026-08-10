package contextcompiler

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func assembleCompilation(state compilationState) (compilationState, error) {
	state.result.Contents = assembleContents(state.summary, state.recent, state.capsule, state.active)
	if err := validateProjectedContents(state.result.Contents, state.request.OpenInvocationIDs); err != nil {
		return state, fmt.Errorf("context compiler: validate projection: %w", err)
	}
	return state, nil
}

func reassembleCompilation(state compilationState) compilationState {
	state.result.Contents = assembleContents(state.summary, state.recent, state.capsule, state.active)
	return state
}

// evictOptionalContext removes lower-priority history before active response
// reduction. It performs no counting and does not mutate the prior state.
func evictOptionalContext(state compilationState) compilationState {
	state.summary = nil
	state.recent = nil
	state.optionalEvicted = true
	state.diagnostics.RecentTurnsRetained = 0
	return reassembleCompilation(state)
}

func evictContinuityAndExcerpts(state compilationState) compilationState {
	state.capsule = nil
	state.diagnostics.ContinuityTokens = 0
	state.diagnostics.ContinuityCodePoints = 0
	state.optionalEvicted = true
	state.active = domain.CloneContents(state.active)
	stripProjectedExcerpts(state.active)
	return reassembleCompilation(state)
}

// prepareReduction builds the dry-run minimum and the allocation candidates.
// Counting and durable storage remain outside this pure preparation phase.
func prepareReduction(state compilationState) ([]reduciblePart, []int, []domain.Content) {
	reducibleForAlloc := make([]reduciblePart, 0, len(state.reducible))
	minEnvForAlloc := make([]int, 0, len(state.reducible))
	for _, part := range state.reducible {
		if part.cost <= part.minimumCost {
			continue
		}
		reducibleForAlloc = append(reducibleForAlloc, part)
		minEnvForAlloc = append(minEnvForAlloc, part.minimumCost)
	}
	minimumContents := assembleContents(nil, nil, nil, dryRunActiveContents(state.active, reducibleForAlloc))
	return reducibleForAlloc, minEnvForAlloc, minimumContents
}

func (c *Compiler) reduceCompilation(ctx context.Context, state compilationState) (compilationState, error) {
	if state.count.Tokens <= state.allocationLimit {
		return state, nil
	}

	var (
		reducibleForAlloc     []reduciblePart
		minEnvForAlloc        []int
		minimumContents       []domain.Content
		plannedProjections    []projectionMutation
		responseCountBefore   int
		responseTokensRemoved int
	)
	state = evictOptionalContext(state)
	var err error
	state, err = c.countCompilation(ctx, state, false)
	if err != nil {
		return state, err
	}
	state.diagnostics.RecountPasses++

	if state.count.Tokens > state.allocationLimit {
		state = evictContinuityAndExcerpts(state)
		state, err = c.countCompilation(ctx, state, false)
		if err != nil {
			return state, err
		}
		state.diagnostics.RecountPasses++
	}

	if state.count.Tokens > state.allocationLimit {
		reducibleForAlloc, minEnvForAlloc, minimumContents = prepareReduction(state)
		if len(reducibleForAlloc) > 0 {
			minimumCount, countErr := c.countProjection(ctx, minimumContents, state.request.FixedRequestTokens)
			if countErr != nil {
				return state, countErr
			}
			state.diagnostics.RecountPasses++
			// ProtectedTokens remains the legacy minimum-request metric. The
			// explicit code-point diagnostics describe allocation costs.
			state.diagnostics.ProtectedTokens = minimumCount.Tokens
			if minimumCount.Tokens > state.hardLimit {
				state.diagnostics.RequestTokensAfter = minimumCount.Tokens
				state.diagnostics.ReductionReason = "irreducible"
				state.diagnostics.ReductionStage = "min_irreducible"
				state.exposeDiagnosticsOnError = true
				state.diagnosticsIrreducible = true
				return state, &domain.IrreducibleContextError{MinimumTokens: minimumCount.Tokens, HardTokens: state.hardLimit}
			}
		}
	}

	if state.count.Tokens > state.allocationLimit && len(reducibleForAlloc) > 0 {
		optionalCost := sumCosts([]int{turnCost(state.summary), turnCost(state.recent), turnCost(state.capsule)})
		available := state.allocationLimit - state.request.FixedRequestTokens - state.protectedCost - state.totalMinimumCost - optionalCost
		if available < 0 {
			available = 0
		}
		allocations := allocateResponseBudgets(reducibleForAlloc, minEnvForAlloc, available)
		planned, planErr := planProjections(reducibleForAlloc, allocations)
		if planErr != nil {
			return state, planErr
		}
		plannedProjections = planned
		if len(planned) > 0 {
			var removed, externalized int
			state.active, removed, externalized, err = c.materializeProjections(ctx, state.request, state.active, planned)
			if err != nil {
				return state, err
			}
			state.diagnostics.ResponsesExternalized = externalized
			state.diagnostics.ResponseCodePointsRemoved = removed
		}
	}

	if state.diagnostics.ResponsesExternalized > 0 {
		responseCountBefore = state.count.Tokens
		state = reassembleCompilation(state)
		state, err = c.countCompilation(ctx, state, false)
		if err != nil {
			return state, err
		}
		state.diagnostics.RecountPasses++
		if responseCountBefore > state.count.Tokens {
			responseTokensRemoved = responseCountBefore - state.count.Tokens
		}
	}

	if state.count.Tokens > state.allocationLimit && state.diagnostics.ResponsesExternalized > 0 {
		state.active = domain.CloneContents(state.active)
		stripProjectedExcerpts(state.active)
		state = reassembleCompilation(state)
		state, err = c.countCompilation(ctx, state, false)
		if err != nil {
			return state, err
		}
		state.diagnostics.RecountPasses++
		if responseCountBefore > state.count.Tokens {
			responseTokensRemoved = responseCountBefore - state.count.Tokens
		}
	}
	state.diagnostics.ResponseTokensRemoved = responseTokensRemoved

	if state.count.Tokens > state.hardLimit {
		lateContents, lateRemoved, lateExternalized, lateErr := c.lateExternalize(ctx, state.request, state.active, state.reducible, plannedProjections, state.hardLimit)
		if lateErr != nil {
			state.diagnostics.RequestTokensAfter = lateRemoved
			state.diagnostics.ReductionReason = "min_irreducible"
			state.exposeDiagnosticsOnError = true
			if irreducible, ok := lateErr.(*domain.IrreducibleContextError); ok {
				state.diagnostics.ReductionReason = "irreducible"
				state.diagnostics.ReductionStage = "min_irreducible"
				state.diagnosticsIrreducible = true
				return state, irreducible
			}
			return state, lateErr
		}
		if lateExternalized > 0 {
			state.diagnostics.LateExternalized = true
			state.diagnostics.ReductionStage = "late"
			state.active = lateContents
			state.diagnostics.ResponsesExternalized += lateExternalized
			state.diagnostics.ResponseCodePointsRemoved += lateRemoved
			state.result.Contents = state.active
			beforeLateCount := state.count.Tokens
			state, err = c.countCompilation(ctx, state, false)
			if err != nil {
				return state, err
			}
			state.diagnostics.RecountPasses++
			if beforeLateCount > state.count.Tokens {
				responseTokensRemoved += beforeLateCount - state.count.Tokens
				state.diagnostics.ResponseTokensRemoved = responseTokensRemoved
			}
		}
	}
	return state, nil
}

// planProjections plans oversized FunctionResponse payloads without writing
// results or changing active content. Costs in the water-fill allocation are
// Unicode code-point costs; model token admission is performed separately.
func planProjections(parts []reduciblePart, allocations []int) ([]projectionMutation, error) {
	var projections []projectionMutation
	for i, part := range parts {
		currentCost := part.cost
		allocation := allocations[i]
		if currentCost <= allocation {
			continue
		}

		projection, planErr := newProjectionMutation(part, allocation, "request_budget")
		if planErr != nil {
			return nil, planErr
		}
		projections = append(projections, projection)
	}
	return projections, nil
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

func sumCosts(costs []int) int {
	total := 0
	for _, cost := range costs {
		if cost > 0 && total > math.MaxInt-cost {
			return math.MaxInt
		}
		total += cost
	}
	return total
}

// selectRecentTurns selects completed turns newest-first while they fit.
func selectRecentTurns(turns []domain.ConversationTurn, remaining, limit int) ([]domain.Content, int) {
	if remaining <= 0 || len(turns) == 0 {
		return nil, 0
	}
	var selected []domain.ConversationTurn
	codePointsLeft := remaining
	for i := len(turns) - 1; i >= 0 && (limit <= 0 || len(selected) < limit); i-- {
		turn := turns[i]
		if !turn.Closed {
			continue
		}
		if turn.CharCount > codePointsLeft {
			break
		}
		selected = append(selected, turn.Clone())
		codePointsLeft -= turn.CharCount
	}
	slices.Reverse(selected)
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

func assembleContents(summary, recent, capsule, active []domain.Content) []domain.Content {
	result := make([]domain.Content, 0, len(summary)+len(recent)+len(capsule)+len(active))
	result = append(result, summary...)
	result = append(result, recent...)
	result = append(result, capsule...)
	return append(result, active...)
}

func summaryReference(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return "[UNTRUSTED CONVERSATION SUMMARY REFERENCE]\n" + text + "\n[/UNTRUSTED CONVERSATION SUMMARY REFERENCE]"
}

func validateProjectedContents(contents []domain.Content, openInvocationIDs map[string]struct{}) error {
	turns, activeStart, err := domain.ClassifyConversationTurns(contents,
		domain.TurnClassificationOptions{OpenInvocationIDs: openInvocationIDs})
	if err != nil {
		return fmt.Errorf("validate projected history: %w", err)
	}
	if len(turns) == 0 {
		return nil
	}
	activeIndex := turnIndexForContentStart(turns, activeStart)
	if activeIndex < 0 {
		activeIndex = 0
	}
	if activeIndex > 0 {
		if err := domain.ValidateContentProtocol(domain.FlattenTurns(turns[:activeIndex]), domain.ProtocolValidationOptions{
			RequireComplete:            true,
			AllowConfirmationLifecycle: true,
		}); err != nil {
			return err
		}
	}
	return domain.ValidateContentProtocol(domain.FlattenTurns(turns[activeIndex:]), domain.ProtocolValidationOptions{
		AllowConfirmationLifecycle: true,
	})
}
