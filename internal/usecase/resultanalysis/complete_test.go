package resultanalysis_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

type fakeCompletionStore struct {
	calls int
	input port.AnalysisCompletionInput
}

func (f *fakeCompletionStore) CompleteAnalysis(_ context.Context, input port.AnalysisCompletionInput, _ time.Time) error {
	f.calls++
	f.input = input
	return nil
}

func validTerminalPacket() domain.AnalysisPacket {
	return domain.AnalysisPacket{
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1, ObjectiveDigest: strings.Repeat("c", 64),
		Findings:     []domain.AnalysisStatement{{Text: "the answer"}},
		SourceSHA256: strings.Repeat("b", 64),
		Coverage:     domain.AnalysisCoverage{CoveredBytes: 100, Complete: true},
		Lineage:      []string{"leaf-0"},
	}
}

func completedSteps() []port.AnalysisStep {
	return []port.AnalysisStep{
		{StepID: "leaf-0", State: domain.AnalysisStepCompleted},
		{StepID: "reduce-1-0", State: domain.AnalysisStepCompleted},
	}
}

func TestCompleteTerminalAnalysisCommitsOnAFullyValidPacket(t *testing.T) {
	store := &fakeCompletionStore{}
	input := resultanalysis.CompletionInput{
		AnalysisID: strings.Repeat("a", 64), Scope: testScope(), SourceResultID: strings.Repeat("a", 64),
		SourceBytes: 1024, PromptVersion: "reduction_v1", Packet: validTerminalPacket(),
	}
	if err := resultanalysis.CompleteTerminalAnalysis(context.Background(), store, completedSteps(), input, time.Now()); err != nil {
		t.Fatalf("complete terminal analysis: %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("expected exactly one completion store call, got %d", store.calls)
	}
	if store.input.SourceSHA256 != input.Packet.SourceSHA256 {
		t.Fatalf("completion source sha256 = %q, want %q", store.input.SourceSHA256, input.Packet.SourceSHA256)
	}
}

// TestCompleteTerminalAnalysisRejectsIncompleteCoverage is criterion 7 at
// the completion boundary: even a packet that otherwise validates must not
// reach the completion store when its own coverage is not complete.
func TestCompleteTerminalAnalysisRejectsIncompleteCoverage(t *testing.T) {
	store := &fakeCompletionStore{}
	packet := validTerminalPacket()
	packet.Coverage = domain.AnalysisCoverage{CoveredBytes: 50, Complete: false}
	input := resultanalysis.CompletionInput{
		AnalysisID: strings.Repeat("a", 64), Scope: testScope(), SourceResultID: strings.Repeat("a", 64),
		SourceBytes: 1024, PromptVersion: "reduction_v1", Packet: packet,
	}
	err := resultanalysis.CompleteTerminalAnalysis(context.Background(), store, completedSteps(), input, time.Now())
	if !errors.Is(err, domain.ErrAnalysisValidation) {
		t.Fatalf("incomplete coverage error = %v, want ErrAnalysisValidation", err)
	}
	if store.calls != 0 {
		t.Fatalf("expected the completion store to never be called, got %d calls", store.calls)
	}
}

// TestCompleteTerminalAnalysisRejectsAnUnknownStepState covers "ningun step
// en estado desconocido": a step that is not yet completed must block
// completion even when the packet itself is otherwise valid.
func TestCompleteTerminalAnalysisRejectsAnUnknownStepState(t *testing.T) {
	store := &fakeCompletionStore{}
	steps := []port.AnalysisStep{
		{StepID: "leaf-0", State: domain.AnalysisStepCompleted},
		{StepID: "reduce-1-0", State: domain.AnalysisStepClaimed},
	}
	input := resultanalysis.CompletionInput{
		AnalysisID: strings.Repeat("a", 64), Scope: testScope(), SourceResultID: strings.Repeat("a", 64),
		SourceBytes: 1024, PromptVersion: "reduction_v1", Packet: validTerminalPacket(),
	}
	err := resultanalysis.CompleteTerminalAnalysis(context.Background(), store, steps, input, time.Now())
	if !errors.Is(err, domain.ErrAnalysisValidation) {
		t.Fatalf("unknown step state error = %v, want ErrAnalysisValidation", err)
	}
	if store.calls != 0 {
		t.Fatalf("expected the completion store to never be called, got %d calls", store.calls)
	}
}
