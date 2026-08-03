package externalagent

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestActivationWorkerRetriesBusyBeforeModelWithBackoff(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	store := newActivationWorkerStore(testWorkerActivation(now, domain.ActivationPending))
	handler := &fakeActivationHandler{handleErr: port.NewActivationProcessError("limiter_busy", true, errors.New("busy"))}
	worker, err := NewActivationWorker(ActivationConfig{
		PollInterval: time.Second,
		LeaseTTL:     time.Minute,
		RetryBase:    2 * time.Second,
		RetryMax:     time.Minute,
	}, ActivationDependencies{Store: store, Handler: handler, Clock: fixedClock{now: now}})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	activation := store.activation(t, "activation-job-1")
	if handler.handleCalls != 1 || handler.reconcileCalls != 0 {
		t.Fatalf("handler calls = handle:%d reconcile:%d", handler.handleCalls, handler.reconcileCalls)
	}
	if store.retryCalls != 1 || store.failCalls != 0 {
		t.Fatalf("durable failure calls = retry:%d fail:%d", store.retryCalls, store.failCalls)
	}
	if activation.State != domain.ActivationPending || !activation.NextAttemptAt.Equal(now.Add(2*time.Second)) {
		t.Fatalf("retry state = %#v", activation)
	}
}

func TestActivationWorkerFailsPermanentBeforeModelWithoutBackoff(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	store := newActivationWorkerStore(testWorkerActivation(now, domain.ActivationPending))
	handler := &fakeActivationHandler{handleErr: port.NewActivationProcessError("actor_revoked", false, errors.New("revoked"))}
	worker, err := NewActivationWorker(ActivationConfig{PollInterval: time.Second, LeaseTTL: time.Minute}, ActivationDependencies{
		Store: store, Handler: handler, Clock: fixedClock{now: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	activation := store.activation(t, "activation-job-1")
	if activation.State != domain.ActivationFailed || activation.LastErrorCode != "actor_revoked" {
		t.Fatalf("permanent failure state = %#v", activation)
	}
	if store.retryCalls != 0 || store.failCalls != 1 {
		t.Fatalf("permanent failure calls = retry:%d fail:%d", store.retryCalls, store.failCalls)
	}
}

func TestActivationWorkerReclaimsOrphanBeforeModel(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	activation := testWorkerActivation(now, domain.ActivationProcessing)
	activation.Attempt = 1
	activation.LeaseOwner = "crashed-worker"
	activation.LeaseExpiry = now.Add(-time.Second)
	store := newActivationWorkerStore(activation)
	handler := &fakeActivationHandler{onHandle: func(a domain.ExternalAgentJobActivation) {
		store.setState(a.ActivationID, domain.ActivationCompleted)
	}}
	worker, err := NewActivationWorker(ActivationConfig{PollInterval: time.Second, LeaseTTL: time.Minute}, ActivationDependencies{
		Store: store, Handler: handler, Clock: fixedClock{now: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	claimed := store.activation(t, "activation-job-1")
	if handler.handleCalls != 1 || handler.reconcileCalls != 0 || claimed.Attempt != 2 || claimed.State != domain.ActivationCompleted {
		t.Fatalf("reclaimed activation = %#v, handler = %#v", claimed, handler)
	}
}

func TestActivationWorkerNeverRerunsModelAfterModelStarted(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	activation := testWorkerActivation(now, domain.ActivationModelStarted)
	activation.Attempt = 1
	activation.LeaseOwner = "crashed-worker"
	activation.LeaseExpiry = now.Add(-time.Second)
	store := newActivationWorkerStore(activation)
	handler := &fakeActivationHandler{reconcileErr: errors.New("final event is unavailable")}
	worker, err := NewActivationWorker(ActivationConfig{PollInterval: time.Second, LeaseTTL: time.Minute}, ActivationDependencies{
		Store: store, Handler: handler, Clock: fixedClock{now: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	claimed := store.activation(t, "activation-job-1")
	if handler.handleCalls != 0 || handler.reconcileCalls != 1 {
		t.Fatalf("model replayed: handler = %#v", handler)
	}
	if claimed.State != domain.ActivationCompletionUnknown || claimed.LastErrorCode != "completion_unknown" {
		t.Fatalf("unknown state = %#v", claimed)
	}
	if store.retryCalls != 0 {
		t.Fatalf("post-model activation was retried: %d", store.retryCalls)
	}
}

func TestActivationWorkerSerializesSameConversationClaims(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	first := testWorkerActivation(now, domain.ActivationPending)
	second := testWorkerActivation(now.Add(time.Second), domain.ActivationPending)
	second.ActivationID = "activation-job-2"
	second.JobID = "job-2"
	second.StatusRevision = 2
	second.NextAttemptAt = now
	store := newActivationWorkerStore(first, second)
	handler := &fakeActivationHandler{block: make(chan struct{}), entered: make(chan struct{})}
	handler.onHandle = func(a domain.ExternalAgentJobActivation) { store.setState(a.ActivationID, domain.ActivationCompleted) }
	worker, err := NewActivationWorker(ActivationConfig{PollInterval: time.Second, LeaseTTL: time.Minute}, ActivationDependencies{
		Store: store, Handler: handler, Clock: fixedClock{now: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- worker.ProcessOne(t.Context()) }()
	select {
	case <-handler.entered:
	case <-time.After(time.Second):
		t.Fatal("first activation was not claimed")
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if handler.handleCalls != 1 {
		t.Fatalf("later same-conversation activation bypassed first claim: %d calls", handler.handleCalls)
	}
	close(handler.block)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if handler.handleCalls != 2 || store.activation(t, "activation-job-2").State != domain.ActivationCompleted {
		t.Fatalf("second activation was not processed after first: calls=%d second=%#v", handler.handleCalls, store.activation(t, "activation-job-2"))
	}
}

func TestActivationWorkerPreparedResponseUsesReconciliationOnly(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	activation := testWorkerActivation(now, domain.ActivationResponsePrepared)
	activation.Attempt = 1
	activation.LeaseOwner = "crashed-worker"
	activation.LeaseExpiry = now.Add(-time.Second)
	store := newActivationWorkerStore(activation)
	handler := &fakeActivationHandler{onReconcile: func(a domain.ExternalAgentJobActivation) {
		store.setState(a.ActivationID, domain.ActivationCompleted)
	}}
	worker, err := NewActivationWorker(ActivationConfig{PollInterval: time.Second, LeaseTTL: time.Minute}, ActivationDependencies{
		Store: store, Handler: handler, Clock: fixedClock{now: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if handler.handleCalls != 0 || handler.reconcileCalls != 1 || store.activation(t, "activation-job-1").State != domain.ActivationCompleted {
		t.Fatalf("prepared response reconciliation = handler:%#v activation:%#v", handler, store.activation(t, "activation-job-1"))
	}
}

func testWorkerActivation(now time.Time, state domain.ExternalAgentJobActivationState) domain.ExternalAgentJobActivation {
	return domain.ExternalAgentJobActivation{
		ActivationID:       "activation-job-1",
		JobID:              "job-1",
		StatusRevision:     1,
		Kind:               domain.JobNotificationTerminal,
		TerminalStatus:     domain.JobCompleted,
		NotificationSHA256: strings.Repeat("a", 64),
		Actor:              "actor-1",
		TeamID:             "team-1",
		ConversationKey:    "conversation-1",
		OriginalCallID:     "call-1",
		DeliveryMode:       domain.JobResultDeliveryMarkdown,
		SlackMessageTS:     "1710000000.000001",
		PublishedAt:        now,
		State:              state,
		NextAttemptAt:      now,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
}

type fakeActivationHandler struct {
	mu             sync.Mutex
	handleCalls    int
	reconcileCalls int
	handleErr      error
	reconcileErr   error
	onHandle       func(domain.ExternalAgentJobActivation)
	onReconcile    func(domain.ExternalAgentJobActivation)
	block          chan struct{}
	entered        chan struct{}
	enteredOnce    sync.Once
}

func (h *fakeActivationHandler) HandleJobCompletion(ctx context.Context, activation domain.ExternalAgentJobActivation) error {
	h.mu.Lock()
	h.handleCalls++
	err := h.handleErr
	onHandle := h.onHandle
	h.mu.Unlock()
	h.enteredOnce.Do(func() {
		if h.entered != nil {
			close(h.entered)
		}
	})
	if h.block != nil {
		select {
		case <-h.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if onHandle != nil {
		onHandle(activation)
	}
	return err
}

func (h *fakeActivationHandler) ReconcileJobCompletion(_ context.Context, activation domain.ExternalAgentJobActivation) error {
	h.mu.Lock()
	h.reconcileCalls++
	err := h.reconcileErr
	onReconcile := h.onReconcile
	h.mu.Unlock()
	if onReconcile != nil {
		onReconcile(activation)
	}
	return err
}

type activationWorkerStore struct {
	mu          sync.Mutex
	activations map[string]*domain.ExternalAgentJobActivation
	retryCalls  int
	failCalls   int
}

func newActivationWorkerStore(activations ...domain.ExternalAgentJobActivation) *activationWorkerStore {
	store := &activationWorkerStore{activations: make(map[string]*domain.ExternalAgentJobActivation, len(activations))}
	for _, activation := range activations {
		copy := activation
		store.activations[activation.ActivationID] = &copy
	}
	return store
}

func (s *activationWorkerStore) ClaimNextActivation(_ context.Context, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJobActivation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidates := make([]*domain.ExternalAgentJobActivation, 0, len(s.activations))
	for _, activation := range s.activations {
		if !s.claimable(activation, now) || s.blockedByEarlier(activation) {
			continue
		}
		candidates = append(candidates, activation)
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if !left.PublishedAt.Equal(right.PublishedAt) {
			return left.PublishedAt.Before(right.PublishedAt)
		}
		if left.StatusRevision != right.StatusRevision {
			return left.StatusRevision < right.StatusRevision
		}
		return left.JobID < right.JobID
	})
	if len(candidates) == 0 {
		return nil, nil
	}
	activation := candidates[0]
	if activation.State == domain.ActivationPending {
		activation.State = domain.ActivationProcessing
	}
	activation.Attempt++
	activation.LeaseOwner = owner
	activation.LeaseExpiry = now.Add(leaseTTL)
	copy := *activation
	return &copy, nil
}

func (s *activationWorkerStore) claimable(activation *domain.ExternalAgentJobActivation, now time.Time) bool {
	if activation.State == domain.ActivationPending {
		return activation.NextAttemptAt.IsZero() || !activation.NextAttemptAt.After(now)
	}
	if activation.State != domain.ActivationProcessing && activation.State != domain.ActivationModelStarted && activation.State != domain.ActivationResponsePrepared {
		return false
	}
	return activation.LeaseExpiry.IsZero() || !activation.LeaseExpiry.After(now)
}

func (s *activationWorkerStore) blockedByEarlier(activation *domain.ExternalAgentJobActivation) bool {
	for _, prior := range s.activations {
		if prior.ConversationKey != activation.ConversationKey || prior.ActivationID == activation.ActivationID || isWorkerTerminal(prior.State) {
			continue
		}
		if prior.PublishedAt.Before(activation.PublishedAt) ||
			(prior.PublishedAt.Equal(activation.PublishedAt) && prior.StatusRevision < activation.StatusRevision) ||
			(prior.PublishedAt.Equal(activation.PublishedAt) && prior.StatusRevision == activation.StatusRevision && prior.JobID < activation.JobID) {
			return true
		}
	}
	return false
}

func (s *activationWorkerStore) GetActivation(_ context.Context, activationID string) (*domain.ExternalAgentJobActivation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	activation := s.activations[activationID]
	if activation == nil {
		return nil, nil
	}
	copy := *activation
	return &copy, nil
}

func (s *activationWorkerStore) RetryActivation(_ context.Context, activation *domain.ExternalAgentJobActivation, errorCode string, nextAttemptAt, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.activations[activation.ActivationID]
	if current == nil || current.State != domain.ActivationProcessing || current.LeaseOwner != activation.LeaseOwner || current.Attempt != activation.Attempt {
		return port.ErrActivationStateConflict
	}
	current.State = domain.ActivationPending
	current.LeaseOwner = ""
	current.LeaseExpiry = time.Time{}
	current.NextAttemptAt = nextAttemptAt
	current.LastErrorCode = errorCode
	s.retryCalls++
	return nil
}

func (s *activationWorkerStore) MarkActivationModelStarted(_ context.Context, activation *domain.ExternalAgentJobActivation, _ time.Time) error {
	return s.transition(activation, domain.ActivationModelStarted)
}

func (s *activationWorkerStore) PrepareActivationResponse(_ context.Context, activation *domain.ExternalAgentJobActivation, _, _, _, _ string, _ time.Time) error {
	return s.transition(activation, domain.ActivationResponsePrepared)
}

func (s *activationWorkerStore) CompleteActivation(_ context.Context, activation *domain.ExternalAgentJobActivation, _ string, _ time.Time) error {
	return s.transition(activation, domain.ActivationCompleted)
}

func (s *activationWorkerStore) FailActivation(_ context.Context, activation *domain.ExternalAgentJobActivation, errorCode string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.activations[activation.ActivationID]
	if current == nil || current.LeaseOwner != activation.LeaseOwner || current.Attempt != activation.Attempt {
		return port.ErrActivationStateConflict
	}
	current.State = domain.ActivationFailed
	current.LastErrorCode = errorCode
	current.LeaseOwner = ""
	current.LeaseExpiry = time.Time{}
	s.failCalls++
	return nil
}

func (s *activationWorkerStore) MarkActivationCompletionUnknown(_ context.Context, activation *domain.ExternalAgentJobActivation, errorCode string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.activations[activation.ActivationID]
	if current == nil || current.LeaseOwner != activation.LeaseOwner || current.Attempt != activation.Attempt {
		return port.ErrActivationStateConflict
	}
	current.State = domain.ActivationCompletionUnknown
	current.LastErrorCode = errorCode
	current.LeaseOwner = ""
	current.LeaseExpiry = time.Time{}
	return nil
}

func (s *activationWorkerStore) RenewActivationLease(_ context.Context, activation *domain.ExternalAgentJobActivation, now time.Time, leaseTTL time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.activations[activation.ActivationID]
	if current == nil || current.LeaseOwner != activation.LeaseOwner || current.Attempt != activation.Attempt {
		return port.ErrActivationStateConflict
	}
	current.LeaseExpiry = now.Add(leaseTTL)
	return nil
}

func (s *activationWorkerStore) transition(activation *domain.ExternalAgentJobActivation, state domain.ExternalAgentJobActivationState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.activations[activation.ActivationID]
	if current == nil || current.LeaseOwner != activation.LeaseOwner || current.Attempt != activation.Attempt {
		return port.ErrActivationStateConflict
	}
	current.State = state
	return nil
}

func (s *activationWorkerStore) setState(activationID string, state domain.ExternalAgentJobActivationState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if activation := s.activations[activationID]; activation != nil {
		activation.State = state
	}
}

func (s *activationWorkerStore) activation(t *testing.T, activationID string) domain.ExternalAgentJobActivation {
	t.Helper()
	activation, err := s.GetActivation(t.Context(), activationID)
	if err != nil || activation == nil {
		t.Fatalf("activation %q = %#v, err=%v", activationID, activation, err)
	}
	return *activation
}

func isWorkerTerminal(state domain.ExternalAgentJobActivationState) bool {
	return state == domain.ActivationCompleted || state == domain.ActivationCompletionUnknown || state == domain.ActivationFailed
}
