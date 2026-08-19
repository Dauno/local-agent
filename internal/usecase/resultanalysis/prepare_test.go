package resultanalysis_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

func manifestOfNSegments(t *testing.T, n int, limits domain.AnalysisLimits) domain.AnalysisSegmentManifest {
	t.Helper()
	source := []byte(strings.Repeat("a", int(limits.MaxSegmentBytes)*n))
	manifest, err := resultanalysis.Segment(resultanalysis.SegmenterTextV1, source, limits)
	if err != nil {
		t.Fatalf("build fixture manifest: %v", err)
	}
	if len(manifest.Segments) != n {
		t.Fatalf("fixture manifest has %d segments, want %d", len(manifest.Segments), n)
	}
	return manifest
}

func TestPrepareStepsIsDeterministic(t *testing.T) {
	limits := reviewLimits(100, 1000, 20, 64)
	limits.MaxReductionFanIn = 3
	manifest := manifestOfNSegments(t, 10, limits)
	now := time.Now().UTC()
	analysisID := strings.Repeat("a", 64)

	first, err := resultanalysis.PrepareSteps(analysisID, manifest, limits, now)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	second, err := resultanalysis.PrepareSteps(analysisID, manifest, limits, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("step counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].StepID != second[i].StepID || first[i].Kind != second[i].Kind {
			t.Fatalf("step %d differs: %+v vs %+v", i, first[i], second[i])
		}
		if strings.Join(first[i].ChildStepIDs, ",") != strings.Join(second[i].ChildStepIDs, ",") {
			t.Fatalf("step %d children differ: %v vs %v", i, first[i].ChildStepIDs, second[i].ChildStepIDs)
		}
	}
}

func TestPrepareStepsLeafOrderAndReductionShape(t *testing.T) {
	limits := reviewLimits(100, 1000, 20, 64)
	limits.MaxReductionFanIn = 3
	manifest := manifestOfNSegments(t, 7, limits) // 7 leaves, fan-in 3: level1 -> 3 nodes (3,3,1), level2 -> 1 node
	now := time.Now().UTC()
	analysisID := strings.Repeat("b", 64)

	steps, err := resultanalysis.PrepareSteps(analysisID, manifest, limits, now)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	byID := map[string]int{}
	for i, s := range steps {
		byID[s.StepID] = i
	}
	wantLeaves := []string{"leaf-0", "leaf-1", "leaf-2", "leaf-3", "leaf-4", "leaf-5", "leaf-6"}
	for i, id := range wantLeaves {
		if steps[i].StepID != id || steps[i].Kind != domain.AnalysisStepLeaf || steps[i].SegmentOrdinal != i {
			t.Fatalf("leaf %d = %+v, want StepID %q ordinal %d", i, steps[i], id, i)
		}
	}
	// Every child must appear strictly before its parent in the slice.
	for _, s := range steps {
		if s.Kind != domain.AnalysisStepReduction {
			continue
		}
		for _, child := range s.ChildStepIDs {
			childIdx, ok := byID[child]
			if !ok {
				t.Fatalf("reduction %s references undeclared child %s", s.StepID, child)
			}
			if childIdx >= byID[s.StepID] {
				t.Fatalf("reduction %s at index %d references child %s at index %d, not yet prepared", s.StepID, byID[s.StepID], child, childIdx)
			}
		}
	}
	var roots []string
	for _, s := range steps {
		if s.Kind == domain.AnalysisStepReduction {
			roots = append(roots, s.StepID)
		}
	}
	if len(roots) == 0 {
		t.Fatal("expected at least one reduction step")
	}
	root := steps[len(steps)-1]
	if root.Kind != domain.AnalysisStepReduction {
		t.Fatalf("last step is not the reduction root: %+v", root)
	}
}

func TestPrepareStepsSingleLeafStillGetsAReductionRoot(t *testing.T) {
	limits := reviewLimits(100, 1000, 20, 64)
	manifest := manifestOfNSegments(t, 1, limits)
	now := time.Now().UTC()

	steps, err := resultanalysis.PrepareSteps(strings.Repeat("c", 64), manifest, limits, now)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("expected exactly 2 steps (one leaf, one reduction root), got %d: %+v", len(steps), steps)
	}
	if steps[0].Kind != domain.AnalysisStepLeaf || steps[1].Kind != domain.AnalysisStepReduction {
		t.Fatalf("expected leaf then reduction, got %+v", steps)
	}
	if len(steps[1].ChildStepIDs) != 1 || steps[1].ChildStepIDs[0] != steps[0].StepID {
		t.Fatalf("reduction root children = %v, want [%s]", steps[1].ChildStepIDs, steps[0].StepID)
	}
}
