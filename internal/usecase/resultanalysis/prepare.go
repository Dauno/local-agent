package resultanalysis

import (
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// PrepareSteps builds the deterministic, complete set of leaf and reduction
// steps for one segment manifest: the same manifest, analysisID, and limits
// always produce the same step IDs and the same child edges, in the same
// order, with no randomness and no clock read.
//
// Leaf steps are ordered by segment ordinal and named "leaf-{ordinal}".
// Reduction steps are built bottom-up in fixed-size groups of
// limits.MaxReductionFanIn: level 1 groups the leaves, level 2 groups level
// 1's outputs, and so on until exactly one step remains, which is the
// analysis root. A single-leaf manifest still gets one reduction step
// wrapping that leaf, because analysis_packet_v1 (the terminal decision
// shape) is only ever produced by a reduction node, never directly by a
// leaf's own analysis_leaf_v1 output.
//
// The returned slice is in a valid port.AnalysisStepStore.Prepare insertion
// order: every child step appears before the reduction step that
// references it, satisfying the analysis_step_children foreign keys.
func PrepareSteps(analysisID string, manifest domain.AnalysisSegmentManifest, limits domain.AnalysisLimits, now time.Time) ([]port.AnalysisStep, error) {
	if !domain.ValidAnalysisID(analysisID) {
		return nil, fmt.Errorf("%w: step preparation requires a valid analysis id", domain.ErrAnalysisValidation)
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: step preparation requires an injected clock", domain.ErrAnalysisValidation)
	}
	if len(manifest.Segments) > limits.MaxLeaves {
		return nil, fmt.Errorf("%w: %s: manifest has %d segments, more than the configured maximum %d",
			domain.ErrAnalysisValidation, domain.AnalysisFailureSourceTooLarge, len(manifest.Segments), limits.MaxLeaves)
	}

	var steps []port.AnalysisStep
	current := make([]string, len(manifest.Segments))
	for i, segment := range manifest.Segments {
		id := fmt.Sprintf("leaf-%d", segment.Ordinal)
		current[i] = id
		steps = append(steps, port.AnalysisStep{
			AnalysisID: analysisID, StepID: id, Kind: domain.AnalysisStepLeaf,
			SegmentOrdinal: segment.Ordinal, CreatedAt: now, UpdatedAt: now,
		})
	}

	fanIn := limits.MaxReductionFanIn
	level := 1
	for {
		var next []string
		for i := 0; i < len(current); i += fanIn {
			end := i + fanIn
			if end > len(current) {
				end = len(current)
			}
			children := append([]string{}, current[i:end]...)
			id := fmt.Sprintf("reduce-%d-%d", level, len(next))
			steps = append(steps, port.AnalysisStep{
				AnalysisID: analysisID, StepID: id, Kind: domain.AnalysisStepReduction,
				ChildStepIDs: children, CreatedAt: now, UpdatedAt: now,
			})
			next = append(next, id)
		}
		current = next
		if len(current) == 1 {
			break
		}
		level++
		if level > limits.MaxReductionDepth+1 {
			// domain.AnalysisReductionTreeSatisfiable is checked at
			// limits.Validate time (fanIn^depth >= MaxLeaves), so this
			// should be unreachable; it is a defensive typed failure
			// rather than an infinite loop if that invariant is ever
			// violated.
			return nil, fmt.Errorf("%w: reduction tree exceeds the configured depth", domain.ErrAnalysisValidation)
		}
	}
	return steps, nil
}
