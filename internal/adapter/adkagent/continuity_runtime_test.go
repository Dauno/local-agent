package adkagent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/adk/v2/session"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type recordingContinuityStore struct {
	capsule   domain.ContinuityCapsule
	commits   int
	err       error
	latestErr error
}

func (s *recordingContinuityStore) Latest(context.Context, string) (domain.ContinuityCapsule, error) {
	if s.latestErr != nil {
		return domain.ContinuityCapsule{}, s.latestErr
	}
	return s.capsule, nil
}

func (s *recordingContinuityStore) Commit(_ context.Context, _ string, capsule domain.ContinuityCapsule, expectedRevision int64) error {
	if s.err != nil {
		return s.err
	}
	if s.capsule.Revision != expectedRevision {
		return errors.New("revision conflict")
	}
	s.capsule = capsule
	s.commits++
	return nil
}

type continuityMetricCapture struct {
	counts map[string]int64
}

func (m *continuityMetricCapture) AddCounter(name string, delta int64, _ port.MetricLabels) {
	if m.counts == nil {
		m.counts = make(map[string]int64)
	}
	m.counts[name] += delta
}
func (*continuityMetricCapture) SetGauge(string, int64, port.MetricLabels)  {}
func (*continuityMetricCapture) Observe(string, float64, port.MetricLabels) {}
func (m *continuityMetricCapture) Snapshot() []port.MetricSample            { return nil }

func TestApplyContinuityOutcomeClassifiesStructuredProgress(t *testing.T) {
	capsule := domain.ContinuityCapsule{}
	applyContinuityOutcome(&capsule, "Constraint: Context stays bounded\nDecision: Use stable references\nPending: Wire LSP\nOpen question: Which server is available?\nCompleted: Added ranged reads", 12, 20, "0123456789abcdef0123456789abcdef")
	if len(capsule.Constraints) != 1 || len(capsule.Decisions) != 1 || len(capsule.Pending) != 1 || len(capsule.OpenQuestions) != 1 || len(capsule.Completed) != 1 {
		t.Fatalf("classified capsule = %#v", capsule)
	}
	for _, item := range []domain.ContinuityItem{capsule.Constraints[0], capsule.Decisions[0], capsule.Pending[0], capsule.OpenQuestions[0], capsule.Completed[0]} {
		if item.SourceEventOrdinal != 12 || item.SourceSessionRevision != 20 {
			t.Fatalf("item provenance = %#v", item)
		}
	}
}

func TestRuntimePreservesSupersededObjective(t *testing.T) {
	store := &recordingContinuityStore{}
	runtime, err := NewRuntime(RuntimeConfig{AgentName: "Dev Agent", Model: &fakeLLM{}, SessionService: session.InMemoryService(), ContinuityStore: store})
	if err != nil {
		t.Fatal(err)
	}
	key := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1700000000.000001")
	for _, content := range []string{"initial objective", "corrected objective"} {
		if _, err := runtime.Run(t.Context(), port.AgentRequest{ConversationKey: key, Messages: []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: content}}}); err != nil {
			t.Fatal(err)
		}
	}
	if store.capsule.Objective == nil || store.capsule.Objective.Text != "corrected objective" {
		t.Fatalf("objective = %#v", store.capsule.Objective)
	}
	if len(store.capsule.Superseded) != 1 || store.capsule.Superseded[0].Text != "initial objective" || store.capsule.Superseded[0].Status != domain.ContinuityStatusSuperseded {
		t.Fatalf("superseded objectives = %#v", store.capsule.Superseded)
	}
}

func TestRuntimeClassifiesContinuityCommitFailuresWithoutFailingTurn(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		metric string
	}{
		{name: "cas", err: port.ErrContinuityCASConflict, metric: domain.MetricContinuityCheckpointCASConflictTotal},
		{name: "validation", err: port.ErrContinuityValidation, metric: domain.MetricContinuityCheckpointValidationFailure},
		{name: "fallback", err: port.ErrContinuityUnavailable, metric: domain.MetricContinuityCheckpointFallbackTotal},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &recordingContinuityStore{err: test.err}
			capture := &continuityMetricCapture{}
			runtime, err := NewRuntime(RuntimeConfig{AgentName: "Dev Agent", Model: &fakeLLM{}, SessionService: session.InMemoryService(), ContinuityStore: store, Metrics: capture})
			if err != nil {
				t.Fatal(err)
			}
			turn, err := runtime.Run(t.Context(), port.AgentRequest{ConversationKey: "slack:T12345678:dm:D12345678", Messages: []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: "safe turn"}}})
			if err != nil || turn.Text == "" {
				t.Fatalf("Run() = %#v, error = %v", turn, err)
			}
			if capture.counts[test.metric] != 1 {
				t.Fatalf("metrics = %#v, want %q", capture.counts, test.metric)
			}
		})
	}
}

func TestRuntimeClassifiesContinuityLoadValidationFailure(t *testing.T) {
	store := &recordingContinuityStore{latestErr: fmt.Errorf("stored capsule: %w", port.ErrContinuityValidation)}
	capture := &continuityMetricCapture{}
	runtime, err := NewRuntime(RuntimeConfig{AgentName: "Dev Agent", Model: &fakeLLM{}, SessionService: session.InMemoryService(), ContinuityStore: store, Metrics: capture})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := runtime.Run(t.Context(), port.AgentRequest{ConversationKey: "slack:T12345678:dm:D12345678", Messages: []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: "safe turn"}}})
	if err != nil || turn.Text == "" {
		t.Fatalf("Run() = %#v, error = %v", turn, err)
	}
	if capture.counts[domain.MetricContinuityCheckpointValidationFailure] != 1 || capture.counts[domain.MetricContinuityCheckpointFallbackTotal] != 0 {
		t.Fatalf("metrics = %#v", capture.counts)
	}
}

func TestRuntimeRecordsSuccessfulContinuityCommit(t *testing.T) {
	store := &recordingContinuityStore{}
	capture := &continuityMetricCapture{}
	runtime, err := NewRuntime(RuntimeConfig{AgentName: "Dev Agent", Model: &fakeLLM{}, SessionService: session.InMemoryService(), ContinuityStore: store, Metrics: capture})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), port.AgentRequest{ConversationKey: "slack:T12345678:dm:D12345678", Messages: []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: "safe turn"}}}); err != nil {
		t.Fatal(err)
	}
	if store.commits != 1 || capture.counts[domain.MetricContinuityCheckpointCommitTotal] != 1 {
		t.Fatalf("commits = %d, metrics = %#v", store.commits, capture.counts)
	}
}
