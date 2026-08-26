package resultanalysis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// LeafRunner ties one claimed leaf step to the no-tool analyzer, host
// evidence resolution, and the matching durable step transition. It never
// reads source content itself: BuildManifest already produced the segment
// manifest, and ResolveEvidence is the only place source bytes are re-read.
//
// Evidence and Payloads are optional (checkpoint 4): a nil value skips
// evidence persistence or payload persistence respectively, preserving the
// checkpoint 3 behavior for any existing caller that has not been wired up
// to the new stores yet. A caller that wants a leaf's findings reachable by
// a later reduction step, after a restart, must set Payloads; a caller that
// wants resolved evidence excerpts durable must set Evidence.
type LeafRunner struct {
	Steps    port.AnalysisStepStore
	Source   port.TrustedResultStore
	Analyzer port.ResultAnalyzer
	Evidence port.AnalysisEvidenceStore
	Payloads port.AnalysisStepPayloadStore
}

// RunLeafRetryBackoff is the fixed delay before a permit-exhausted leaf
// step is retried. It is intentionally short and constant: TryAcquire is
// non-blocking by design (see port.ModelCallLimiter), so the retry is a
// later worker tick, not a wait inside this call.
const RunLeafRetryBackoff = 5 * time.Second

// RunLeaf executes one already-claimed leaf step to exactly one durable
// outcome:
//
//   - port.ErrModelCallLimitReached from the analyzer releases the step
//     back to prepared via Retry(consumeAttempt=false): the TRD is explicit
//     that non-blocking permit exhaustion is not a failure of the analysis.
//   - context.Canceled or context.DeadlineExceeded propagates unchanged,
//     with no store transition: an in-flight cancellation must not be
//     recorded as a step outcome, so a resumed worker sees the step still
//     claimed and can decide based on lease expiry, not a false failure.
//   - Any other AnalyzeLeaf error (typed leaf-schema failure, already
//     bounded-retried inside the analyzer) fails the step typed.
//   - A successful leaf resolves its evidence selectors through
//     ResolveEvidence, persists every accepted excerpt (checkpoint 4, when
//     r.Evidence is set) and the leaf's own structured payload (checkpoint
//     4, when r.Payloads is set), then completes the step with the leaf's
//     digest. Evidence and payload writes happen while the step is still
//     claimed, before Complete, so the v40
//     analysis_steps_completed_immutable trigger can never block them.
func (r *LeafRunner) RunLeaf(
	ctx context.Context,
	claim domain.AnalysisStepClaim,
	segment domain.AnalysisSegment,
	input port.AnalysisLeafInput,
	resultID string,
	scope domain.ResultScope,
	limits domain.AnalysisLimits,
	now time.Time,
) (domain.AnalysisLeaf, EvidenceResolution, error) {
	leaf, err := r.Analyzer.AnalyzeLeaf(ctx, input)
	if err != nil {
		if errors.Is(err, port.ErrModelCallLimitReached) {
			return domain.AnalysisLeaf{}, EvidenceResolution{}, r.Steps.Retry(ctx, claim, now.Add(RunLeafRetryBackoff), false)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return domain.AnalysisLeaf{}, EvidenceResolution{}, err
		}
		return domain.AnalysisLeaf{}, EvidenceResolution{}, r.Steps.Fail(ctx, claim, domain.AnalysisFailureLeafSchemaInvalid, now)
	}

	resolution, err := ResolveEvidence(ctx, r.Source, resultID, scope, segment, leaf.EvidenceSelectors, limits)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return domain.AnalysisLeaf{}, EvidenceResolution{}, err
		}
		return domain.AnalysisLeaf{}, EvidenceResolution{}, r.Steps.Fail(ctx, claim, domain.AnalysisFailureEvidenceSelectorInvalid, now)
	}

	if r.Evidence != nil {
		for _, excerpt := range resolution.Accepted {
			if err := r.Evidence.Write(ctx, claim.AnalysisID, claim.StepID, excerpt, now); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return domain.AnalysisLeaf{}, EvidenceResolution{}, err
				}
				return domain.AnalysisLeaf{}, EvidenceResolution{}, err
			}
		}
	}

	if r.Payloads != nil {
		payload, err := EncodeLeafPayload(leaf)
		if err != nil {
			return domain.AnalysisLeaf{}, EvidenceResolution{}, fmt.Errorf("%w: encode leaf payload: %v", domain.ErrAnalysisValidation, err)
		}
		if err := r.Payloads.WritePayload(ctx, claim, payload, now); err != nil {
			return domain.AnalysisLeaf{}, EvidenceResolution{}, err
		}
	}

	outputDigest := hexSHA256(leafDigestBytes(leaf))
	if _, err := r.Steps.Complete(ctx, claim, outputDigest, now); err != nil {
		return domain.AnalysisLeaf{}, EvidenceResolution{}, err
	}
	return leaf, resolution, nil
}

// leafDigestBytes deterministically serializes the fields of leaf that
// identify its content, in fixed declared order, for output_digest. It is
// not a general-purpose encoding: only used to produce one stable digest.
func leafDigestBytes(leaf domain.AnalysisLeaf) []byte {
	var buf []byte
	writeField := func(s string) {
		buf = append(buf, []byte(s)...)
		buf = append(buf, 0)
	}
	writeField(string(leaf.ObjectiveClass))
	writeField(leaf.ObjectiveDigest)
	for _, f := range leaf.Findings {
		writeField(f.Text)
	}
	for _, c := range leaf.Constraints {
		writeField(c.Text)
	}
	for _, c := range leaf.Contradictions {
		writeField(c.Text)
	}
	for _, q := range leaf.UnresolvedQuestions {
		writeField(q.Text)
	}
	return buf
}
