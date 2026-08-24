package contextcompiler

import (
	"context"
	"fmt"
	"sort"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// knowledgeBlockContents renders the selected cards into the single
// attributed untrusted [KNOWLEDGE DATA] source block. The block is attached
// to the current plain user input as exactly one additional text part, so
// it travels inside the current user message and the provider conversion
// sends it as user role, never as an assistant or model message.
func knowledgeBlockContents(selected []domain.KnowledgeFrameCard) []domain.Content {
	rendered := domain.RenderKnowledgeFrameCards(selected)
	if rendered == "" {
		return nil
	}
	return []domain.Content{{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: rendered}}}}
}

// assembleActiveWithKnowledge attaches the knowledge block inside the
// current model-facing user turn: exactly one rendered ContentPart is
// appended to the current plain user input content (the first content of
// the active turn), cloned so the original input is never mutated. The
// block never creates another content or conversation turn, and eviction
// simply reassembles without it, restoring the original user content.
func assembleActiveWithKnowledge(active []domain.Content, knowledge []domain.Content) []domain.Content {
	if len(knowledge) == 0 || len(knowledge[0].Parts) == 0 || len(active) == 0 {
		return active
	}
	if !active[0].HasPlainText() {
		// The current turn must start with a plain user input; without one
		// there is no user turn to attach the block to.
		return active
	}
	block := knowledge[0].Parts[0]
	assembled := make([]domain.Content, 0, len(active))
	first := domain.Content{
		Role:  active[0].Role,
		Parts: append(append([]domain.ContentPart(nil), active[0].Parts...), block),
	}
	assembled = append(assembled, first)
	assembled = append(assembled, active[1:]...)
	return assembled
}

// applyKnowledgeSelection validates the delivered candidates, fits whole
// cards cumulatively under the validated source budget measured as a
// provider-shaped token delta against the same request envelope used by
// final admission (the existing conservative counter is the fallback), and
// attaches the selected block to the current turn. A source counter failure
// omits all cards and continues the root request; an oversized card is
// omitted whole and a later smaller card can still fit.
func (c *Compiler) applyKnowledgeSelection(ctx context.Context, state *compilationState) error {
	if state == nil {
		return nil
	}
	if state.request.KnowledgeBudgetTokens < 0 {
		return fmt.Errorf("context compiler: knowledge budget tokens must not be negative, got %d", state.request.KnowledgeBudgetTokens)
	}
	if state.request.KnowledgeBudgetTokens <= 0 || len(state.request.Knowledge) == 0 {
		return nil
	}
	// Cards are validated before fitting: an invalid candidate is omitted
	// whole and can never reach the model.
	valid := make([]domain.KnowledgeFrameCard, 0, len(state.request.Knowledge))
	for _, card := range state.request.Knowledge {
		if err := card.Validate(); err != nil {
			continue
		}
		valid = append(valid, card)
	}
	state.knowledgeApplied = true
	if len(valid) == 0 {
		return nil
	}
	cost := c.knowledgeCardCostFunc(ctx, state)
	// The final selected cost is captured during fitting so the source
	// token delta is recorded without recounting the frame again. The key
	// is the exact ordered selection's canonical render, not its length:
	// correction and refinement can each measure a different selection of
	// equal length, and a length-keyed map would let the later measurement
	// silently overwrite the cost of the selection actually returned
	// (FIND-120).
	selectionCosts := make(map[string]int, len(valid))
	fitCost := func(selected []domain.KnowledgeFrameCard) (int, error) {
		delta, err := cost(selected)
		if err != nil {
			return 0, err
		}
		if delta >= 0 {
			selectionCosts[domain.RenderKnowledgeFrameCards(selected)] = delta
		}
		return delta, err
	}
	selected, err := domain.FitKnowledgeFrameCards(valid, state.request.KnowledgeBudgetTokens, fitCost)
	if err != nil {
		// A source counter failure omits all knowledge rather than failing
		// the root request. Final request admission remains authoritative.
		return nil
	}
	state.knowledgeCards = selected
	if len(selected) > 0 {
		delta, ok := selectionCosts[domain.RenderKnowledgeFrameCards(selected)]
		if !ok {
			state.knowledgeCards = nil
			return nil
		}
		state.knowledgeSourceTokens = delta
		state.knowledge = knowledgeBlockContents(selected)
	}
	return nil
}

// knowledgeCardCostFunc builds the cumulative source cost function for
// FitKnowledgeFrameCards: the token delta of the rendered block against the
// same request envelope used by final admission. With a provider frame
// counter the envelope is the provider-shaped request; without one the
// existing conservative request counter is the fallback. Any counting error
// fails the selection closed, which omits all cards.
//
// DEC-08-4 describes an in-memory context cost cache for the knowledge cost
// path this function builds; this checkpoint does not build one. The fitter
// FitKnowledgeFrameCards calls into now spends a small, bounded number of
// authoritative calls (Gate C, checkpoint 4 repair 2, FIND-116), not one per
// candidate card as it did before checkpoint 4, so there is little left for
// a cache to save here, and only on an exact repeat of the same rendered
// content. The real per-turn
// counting envelope (model, tool set, generation config) is assembled
// per-invocation by the owning adapter
// (internal/adapter/adkagent/context_compiler.go's providerFrameCounter,
// built fresh per call from ToolsForInvocation) and is not reachable from
// here without widening port.ContextFrameCounter with a profile identifier,
// which is out of scope for this repair (FIND-118). Caching on content
// digest alone, without that envelope in the key, lets two turns with the
// same contents but a different tool set share a cache entry and hand the
// second turn the first turn's number: with a delta-based cost function the
// error does not cancel. Given the small, turn-scoped saving and the real
// collision risk, this checkpoint removes the cache instead of building it
// with an unreachable key component.
func (c *Compiler) knowledgeCardCostFunc(ctx context.Context, state *compilationState) domain.KnowledgeFrameCardCostFunc {
	base := assembleContents(state.summary, state.recent, state.capsule, state.active)
	baseTokens, err := c.countKnowledgeCandidateTokens(ctx, state, base)
	if err != nil {
		return func([]domain.KnowledgeFrameCard) (int, error) { return 0, err }
	}
	return func(selected []domain.KnowledgeFrameCard) (int, error) {
		candidate := assembleContents(state.summary, state.recent, state.capsule, assembleActiveWithKnowledge(state.active, knowledgeBlockContents(selected)))
		tokens, err := c.countKnowledgeCandidateTokens(ctx, state, candidate)
		if err != nil {
			return 0, err
		}
		return tokens - baseTokens, nil
	}
}

// countKnowledgeCandidateTokens performs one real count: the provider-shaped
// frame count when a frame counter is available, otherwise the conservative
// request-projection count.
func (c *Compiler) countKnowledgeCandidateTokens(ctx context.Context, state *compilationState, contents []domain.Content) (int, error) {
	if state.frameCounter != nil {
		count, err := state.frameCounter.CountContextFrame(ctx, contents)
		if err != nil {
			return 0, err
		}
		if count.Tokens < 0 {
			return 0, fmt.Errorf("frame counter returned a negative token count")
		}
		return count.Tokens, nil
	}
	count, err := c.countProjection(ctx, contents, state.request.FixedRequestTokens, nil)
	if err != nil {
		return 0, err
	}
	return count.Tokens, nil
}

// evictKnowledge removes the entire selected knowledge block as one unit
// during total-pressure reduction. It runs after summary and old completed
// turns and before continuity and protected active protocol content, so
// knowledge can never make an otherwise admissible request irreducible.
func evictKnowledge(state compilationState) compilationState {
	state.knowledge = nil
	state.knowledgeCards = nil
	state.knowledgeSourceTokens = 0
	state.optionalEvicted = true
	return reassembleCompilation(state)
}

// knowledgeResultFields derives the final content-free CompileResult facts
// from the cards still selected after any total-pressure eviction: sorted
// unique identities plus the selected count. Omitted counts every candidate
// delivered to the compiler that is absent from the final request.
func (state compilationState) knowledgeResultFields() ([]string, int, int) {
	identities := make([]string, 0, len(state.knowledgeCards))
	seen := make(map[string]bool, len(state.knowledgeCards))
	for _, card := range state.knowledgeCards {
		identity := card.Identity()
		if identity == "" || seen[identity] {
			continue
		}
		seen[identity] = true
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	selected := len(state.knowledgeCards)
	omitted := len(state.request.Knowledge) - selected
	if omitted < 0 {
		omitted = 0
	}
	return identities, selected, omitted
}

// completeEmptyKnowledgeResult fills the CompileResult when the request has
// no classified turns: the selection is discarded and every delivered
// candidate is omitted from the empty final request.
func (state compilationState) completeEmptyKnowledgeResult() domain.CompileResult {
	result := domain.CompileResult{Contents: nil, Diagnostics: state.diagnostics}
	result.KnowledgeIdentities, result.KnowledgeSelectedCount, result.KnowledgeOmittedCount = state.knowledgeResultFields()
	result.WorkstreamSnapshotIncluded, result.WorkstreamOmissionReason = state.workstreamResultFields()
	result.SummaryIncluded = len(state.summary) > 0
	return result
}

// knowledgeRetrievalReason extracts the final card attribution reason used
// for the selected-channel metric.
func knowledgeRetrievalReason(card domain.KnowledgeFrameCard) string {
	switch card.Kind {
	case domain.KnowledgeRetrievalClaim:
		if card.Claim != nil {
			return card.Claim.RetrievalReason
		}
	case domain.KnowledgeRetrievalPreference:
		if card.Preference != nil {
			return card.Preference.RetrievalReason
		}
	case domain.KnowledgeRetrievalDocument:
		if card.Document != nil {
			return card.Document.RetrievalReason
		}
	}
	return ""
}

// recordKnowledgeSelection emits the closed final-selection metrics: one
// selected counter per card channel derived from the final card reason and
// one observation of the final source token delta.
func (c *Compiler) recordKnowledgeSelection(state compilationState) {
	if c == nil || c.metrics == nil || !state.knowledgeApplied {
		return
	}
	for _, card := range state.knowledgeCards {
		channel, ok := domain.KnowledgeRetrievalChannelForReason(domain.KnowledgeRetrievalReason(knowledgeRetrievalReason(card)))
		if !ok {
			continue
		}
		c.metrics.AddCounter(domain.MetricKnowledgeRetrievalSelected, 1, port.MetricLabels{domain.MetricLabelChannel: string(channel)})
	}
	c.metrics.Observe(domain.MetricKnowledgeRetrievalCardTokens, float64(state.knowledgeSourceTokens), nil)
}
