package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

// permitExhaustedAnalyzer always reports the process-wide model-call limit
// reached, without ever touching a source store.
type permitExhaustedAnalyzer struct{}

func (permitExhaustedAnalyzer) AnalyzeLeaf(context.Context, port.AnalysisLeafInput) (domain.AnalysisLeaf, error) {
	return domain.AnalysisLeaf{}, port.ErrModelCallLimitReached
}

func (permitExhaustedAnalyzer) Reduce(context.Context, port.AnalysisReductionInput) (domain.AnalysisPacket, error) {
	return domain.AnalysisPacket{}, port.ErrModelCallLimitReached
}

// TestRunLeafReleasesOnModelCallLimitWithoutConsumingAttempt is criterion 9,
// run against the real SQLite-backed AnalysisStepStore, not a fake: a
// non-blocking model-call limiter with no free permit must release the
// claimed step back to prepared without spending its attempt budget.
func TestRunLeafReleasesOnModelCallLimitWithoutConsumingAttempt(t *testing.T) {
	steps, analysisID := newAnalysisStepStoreFixture(t)
	now := time.Now().UTC()
	if _, err := steps.Prepare(context.Background(), stepFixture(analysisID, "leaf-0", now)); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	claimed, ok, err := steps.ClaimNext(context.Background(), analysisID, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claimed.Attempt != 0 {
		t.Fatalf("attempt before run = %d, want 0", claimed.Attempt)
	}
	claim := domain.AnalysisStepClaim{AnalysisID: analysisID, StepID: claimed.StepID, Generation: claimed.Generation, LeaseUntil: claimed.LeaseUntil}

	runner := resultanalysis.LeafRunner{Steps: steps, Analyzer: permitExhaustedAnalyzer{}}
	input := port.AnalysisLeafInput{
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1, ObjectiveText: "objective",
		SegmentOrdinal: 0, SegmentText: "segment text", PromptVersion: "leaf_v1",
	}
	segment := domain.AnalysisSegment{Ordinal: 0, OffsetBytes: 0, LengthBytes: 12, SHA256: strings.Repeat("a", 64), SegmenterVersion: "text_v1"}
	scope := domain.ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:U1", Project: "workspace"}

	if _, _, err := runner.RunLeaf(context.Background(), claim, segment, input, "src-id", scope, testAnalysisLimits(), now); err != nil {
		t.Fatalf("run leaf: %v", err)
	}

	after, err := steps.List(context.Background(), analysisID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("expected exactly one step, got %d", len(after))
	}
	if after[0].State != domain.AnalysisStepPrepared {
		t.Fatalf("state after permit exhaustion = %q, want prepared", after[0].State)
	}
	if after[0].Attempt != 0 {
		t.Fatalf("attempt after permit exhaustion = %d, want 0 (consumeAttempt=false)", after[0].Attempt)
	}
}
