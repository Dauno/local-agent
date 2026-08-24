package contextcompiler

import (
	"context"
	"fmt"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// Bounded, closed omission-reason categories for the active workstream
// snapshot source. Never a snippet of workstream content.
const (
	workstreamOmissionNone         = ""
	workstreamOmissionNoSnapshot   = "no_snapshot"
	workstreamOmissionDisabled     = "disabled"
	workstreamOmissionSourceBudget = "source_budget"
	workstreamOmissionTotalPress   = "total_pressure"
)

// workstreamBlockContents renders the admitted snapshot into the single
// attributed untrusted [WORKSTREAM DATA] source block, mirroring the
// knowledge block contract: it travels inside the current user turn only.
func workstreamBlockContents(rendered string) []domain.Content {
	if rendered == "" {
		return nil
	}
	return []domain.Content{{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: rendered}}}}
}

// assembleActiveWithWorkstream attaches the workstream block inside the
// current model-facing user turn, exactly like assembleActiveWithKnowledge.
// It is applied before the knowledge block so the rendered order matches
// TRD 03 selection priority (workstream ranks above knowledge cards).
func assembleActiveWithWorkstream(active []domain.Content, workstream []domain.Content) []domain.Content {
	if len(workstream) == 0 || len(workstream[0].Parts) == 0 || len(active) == 0 {
		return active
	}
	if !active[0].HasPlainText() {
		return active
	}
	block := workstream[0].Parts[0]
	assembled := make([]domain.Content, 0, len(active))
	first := domain.Content{
		Role:  active[0].Role,
		Parts: append(append([]domain.ContentPart(nil), active[0].Parts...), block),
	}
	assembled = append(assembled, first)
	assembled = append(assembled, active[1:]...)
	return assembled
}

// applyWorkstreamSelection decides, once per compile, whether the active
// workstream snapshot is admitted as one atomic block under its own source
// budget measured with the same counter used for final admission. The
// snapshot is never split or truncated: it is admitted whole or omitted
// whole, with a bounded diagnostic reason and no snapshot content ever
// reaching diagnostics, logs, or metrics.
func (c *Compiler) applyWorkstreamSelection(ctx context.Context, state *compilationState) error {
	if state == nil {
		return nil
	}
	if state.request.WorkstreamBudgetTokens < 0 {
		return fmt.Errorf("context compiler: workstream budget tokens must not be negative, got %d", state.request.WorkstreamBudgetTokens)
	}
	state.workstreamApplied = true
	if state.request.Workstream == nil {
		state.workstreamOmissionReason = workstreamOmissionNoSnapshot
		return nil
	}
	if state.request.WorkstreamBudgetTokens <= 0 {
		state.workstreamOmissionReason = workstreamOmissionDisabled
		return nil
	}
	rendered, err := domain.RenderWorkstreamSnapshot(*state.request.Workstream)
	if err != nil || rendered == "" {
		state.workstreamOmissionReason = workstreamOmissionNoSnapshot
		return nil
	}
	candidate := workstreamBlockContents(rendered)
	cost := c.workstreamCostFunc(ctx, state)
	delta, err := cost(candidate)
	if err != nil || delta < 0 || delta > state.request.WorkstreamBudgetTokens {
		state.workstreamOmissionReason = workstreamOmissionSourceBudget
		return nil
	}
	state.workstream = candidate
	state.workstreamSourceTokens = delta
	state.workstreamOmissionReason = workstreamOmissionNone
	return nil
}

// workstreamCostFunc measures the provider-shaped token delta of attaching
// the candidate workstream block against the same base envelope used by
// final admission, mirroring knowledgeCardCostFunc.
func (c *Compiler) workstreamCostFunc(ctx context.Context, state *compilationState) func([]domain.Content) (int, error) {
	base := assembleContents(state.summary, state.recent, state.capsule, state.active)
	if state.frameCounter != nil {
		baseCount, err := state.frameCounter.CountContextFrame(ctx, base)
		if err != nil {
			return func([]domain.Content) (int, error) { return 0, err }
		}
		if baseCount.Tokens < 0 {
			return func([]domain.Content) (int, error) {
				return 0, fmt.Errorf("frame counter returned a negative token count")
			}
		}
		return func(candidate []domain.Content) (int, error) {
			assembled := assembleContents(state.summary, state.recent, state.capsule, assembleActiveWithWorkstream(state.active, candidate))
			count, err := state.frameCounter.CountContextFrame(ctx, assembled)
			if err != nil {
				return 0, err
			}
			if count.Tokens < 0 {
				return 0, fmt.Errorf("frame counter returned a negative token count")
			}
			return count.Tokens - baseCount.Tokens, nil
		}
	}
	baseCount, err := c.countProjection(ctx, base, state.request.FixedRequestTokens, nil)
	if err != nil {
		return func([]domain.Content) (int, error) { return 0, err }
	}
	return func(candidate []domain.Content) (int, error) {
		assembled := assembleContents(state.summary, state.recent, state.capsule, assembleActiveWithWorkstream(state.active, candidate))
		count, err := c.countProjection(ctx, assembled, state.request.FixedRequestTokens, nil)
		if err != nil {
			return 0, err
		}
		return count.Tokens - baseCount.Tokens, nil
	}
}

// evictWorkstream removes the entire admitted workstream block as one unit
// during total-pressure reduction. It runs after summary, old completed
// turns, knowledge, and continuity/excerpts, and before protected active
// protocol content, so it outranks every other optional source per TRD 03
// selection priority while never making an otherwise admissible request
// irreducible.
func evictWorkstream(state compilationState) compilationState {
	state.workstream = nil
	state.workstreamSourceTokens = 0
	state.workstreamOmissionReason = workstreamOmissionTotalPress
	state.optionalEvicted = true
	return reassembleCompilation(state)
}

// workstreamResultFields derives the final content-free CompileResult facts.
func (state compilationState) workstreamResultFields() (bool, string) {
	if !state.workstreamApplied {
		return false, workstreamOmissionNone
	}
	return len(state.workstream) > 0, state.workstreamOmissionReason
}
