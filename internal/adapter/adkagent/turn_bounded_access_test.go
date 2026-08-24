package adkagent

// TRD 08 checkpoint 2, Gate A structural form
// (docs/root-orchestrator-v2/hallazgos/worker-prompt-trd08-checkpoint2.md):
// no path that runs once per normal turn may issue an ADK session Get without
// NumRecentEvents. This file wraps the real sqlite-backed session service
// (the only backend that exposes the bounded fast paths: LatestEventOrdinal,
// SessionExists, LoadEventRange) and fails closed if it ever sees an
// unbounded Get during a normal turn.
//
// The architecture test excludes _test.go files
// (internal/architecture/dependencies_test.go:23), so this file may import
// internal/adapter/sqlite even though production adkagent code must not.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// recordingSessionService forwards every call to a real sqlite-backed
// AdkSessionService and records whether any Get call was unbounded. It also
// forwards the extra methods runtime.go type-asserts for (LatestEventOrdinal,
// SessionExists, LoadEventRange) so the bounded paths actually engage.
type recordingSessionService struct {
	inner *adaptersqlite.AdkSessionService

	mu            sync.Mutex
	unboundedGets int
	totalGets     int
	createCalls   int
	createErrors  int
}

func (w *recordingSessionService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	resp, err := w.inner.Create(ctx, req)
	w.mu.Lock()
	w.createCalls++
	if err != nil {
		w.createErrors++
	}
	w.mu.Unlock()
	return resp, err
}

func (w *recordingSessionService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	w.mu.Lock()
	w.totalGets++
	if req == nil || req.NumRecentEvents <= 0 {
		w.unboundedGets++
	}
	w.mu.Unlock()
	return w.inner.Get(ctx, req)
}

func (w *recordingSessionService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	return w.inner.List(ctx, req)
}

func (w *recordingSessionService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	return w.inner.Delete(ctx, req)
}

func (w *recordingSessionService) AppendEvent(ctx context.Context, sess session.Session, event *session.Event) error {
	return w.inner.AppendEvent(ctx, sess, event)
}

func (w *recordingSessionService) LatestEventOrdinal(ctx context.Context, appName, userID, sessionID string) (int64, error) {
	return w.inner.LatestEventOrdinal(ctx, appName, userID, sessionID)
}

func (w *recordingSessionService) SessionExists(ctx context.Context, appName, userID, sessionID string) (session.Session, bool, error) {
	return w.inner.SessionExists(ctx, appName, userID, sessionID)
}

func (w *recordingSessionService) LoadEventRange(ctx context.Context, appName, userID, sessionID string, afterOrdinal, limit int64) ([]*session.Event, error) {
	return w.inner.LoadEventRange(ctx, appName, userID, sessionID, afterOrdinal, limit)
}

var (
	_ session.Service         = (*recordingSessionService)(nil)
	_ epochEventHeadReader    = (*recordingSessionService)(nil)
	_ sessionExistenceChecker = (*recordingSessionService)(nil)
	_ durableEventRangeLoader = (*recordingSessionService)(nil)
)

func newRecordingSessionService(t *testing.T) *recordingSessionService {
	t.Helper()
	dir := t.TempDir()
	store, err := adaptersqlite.Initialize(context.Background(), filepath.Join(dir, "gate.db"))
	if err != nil {
		t.Fatalf("initialize gate store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc := adaptersqlite.NewAdkSessionService(store)
	if svc == nil {
		t.Fatal("nil ADK session service")
	}
	return &recordingSessionService{inner: svc}
}

// TestGateANoUnboundedGetDuringNormalTurns is Gate A's structural form: run
// several normal turns (Run, then Stream, on the same and on a fresh
// conversation) against the real sqlite-backed session service and assert no
// Get call was ever unbounded.
func TestGateANoUnboundedGetDuringNormalTurns(t *testing.T) {
	recorder := newRecordingSessionService(t)
	llm := &fakeLLM{response: func(*model.LLMRequest) string { return "response" }}
	runtime, err := NewRuntime(RuntimeConfig{AgentName: "Dev Agent", Model: llm, SessionService: recorder, ContinuityStore: &recordingContinuityStore{}})
	if err != nil {
		t.Fatal(err)
	}

	key := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1700000000.000001")
	for i := range 3 {
		if _, err := runtime.Run(t.Context(), port.AgentRequest{
			ConversationKey: key,
			Messages:        []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: "hello again"}},
		}); err != nil {
			t.Fatalf("Run() turn %d: %v", i, err)
		}
	}

	var events []port.AgentStreamEvent
	runtime.Stream(t.Context(), port.AgentRequest{
		ConversationKey: key,
		Messages:        []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: "stream turn"}},
	}, func(event port.AgentStreamEvent) bool {
		events = append(events, event)
		return true
	})
	if len(events) == 0 {
		t.Fatal("Stream produced no events")
	}

	otherKey := domain.ConversationKey("slack:T12345678:dm:D99999999")
	if _, err := runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey: otherKey,
		Messages:        []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: "fresh conversation"}},
	}); err != nil {
		t.Fatalf("Run() on fresh conversation: %v", err)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.totalGets == 0 {
		t.Fatal("no Get calls were recorded: the gate did not exercise the runner's bounded read path")
	}
	if recorder.unboundedGets != 0 {
		t.Fatalf("unbounded Get calls during normal turns = %d, want 0", recorder.unboundedGets)
	}
}

// TestEnsureSessionUsesExistenceCheckNotFailedCreate is acceptance criterion
// 4: ensureSession must use an idempotent get-or-create, not a failed insert,
// as its existing-session path. A second turn on the same conversation must
// not call Create again at all (the existence check finds the session first),
// so Create is called exactly once, and it never fails.
func TestEnsureSessionUsesExistenceCheckNotFailedCreate(t *testing.T) {
	recorder := newRecordingSessionService(t)
	llm := &fakeLLM{response: func(*model.LLMRequest) string { return "response" }}
	runtime, err := NewRuntime(RuntimeConfig{AgentName: "Dev Agent", Model: llm, SessionService: recorder, ContinuityStore: &recordingContinuityStore{}})
	if err != nil {
		t.Fatal(err)
	}

	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	for i := range 3 {
		if _, err := runtime.Run(t.Context(), port.AgentRequest{
			ConversationKey: key,
			Messages:        []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: "hello again"}},
		}); err != nil {
			t.Fatalf("Run() turn %d: %v", i, err)
		}
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.createCalls != 1 {
		t.Fatalf("Create calls across 3 turns on one conversation = %d, want exactly 1", recorder.createCalls)
	}
	if recorder.createErrors != 0 {
		t.Fatalf("Create errors = %d, want 0: the existing-session path must not rely on a failed insert", recorder.createErrors)
	}
}

// TestRecoverActivationUsesBoundedGet is acceptance criterion 5's activation
// half: RecoverActivation must not issue an unbounded Get against the
// per-activation session.
func TestRecoverActivationUsesBoundedGet(t *testing.T) {
	recorder := newRecordingSessionService(t)
	llm := &originContextLLM{}
	runtime, err := NewRuntime(RuntimeConfig{AgentName: "Dev Agent", Model: llm, SessionService: recorder})
	if err != nil {
		t.Fatal(err)
	}
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	activationID := "activation_bounded_check"
	if _, err := runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey: key,
		Origin:          port.AgentTurnOrigin{Kind: port.AgentTurnOriginJobCompletion, Actor: "U12345678", ActivationID: activationID},
		Messages:        []domain.Message{{Role: domain.RoleUser, Source: domain.MessageSourceJobCompletion, Content: "compact envelope", UserID: "U12345678", ExternalTS: activationID}},
	}); err != nil {
		t.Fatal(err)
	}

	recorder.mu.Lock()
	recorder.totalGets, recorder.unboundedGets = 0, 0
	recorder.mu.Unlock()

	turn, found, err := runtime.RecoverActivation(t.Context(), key, activationID)
	if err != nil || !found || turn.Text != "root synthesis" {
		t.Fatalf("recovery = %#v, found=%t, err=%v", turn, found, err)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.totalGets == 0 {
		t.Fatal("RecoverActivation did not call Get at all")
	}
	if recorder.unboundedGets != 0 {
		t.Fatalf("unbounded Get calls during RecoverActivation = %d, want 0", recorder.unboundedGets)
	}
}
