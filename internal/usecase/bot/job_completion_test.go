package bot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type fakeActivationStore struct {
	activation        domain.ExternalAgentJobActivation
	modelStartedCalls int
	prepareCalls      int
	completeCalls     int
	failedCalls       int
	unknownCalls      int
	prepareErr        error
	stateMutationErr  error
	lastErrorCode     string
	responseSlackTS   string
}

func (s *fakeActivationStore) ClaimNextActivation(context.Context, time.Time, string, time.Duration) (*domain.ExternalAgentJobActivation, error) {
	return nil, nil
}

func (s *fakeActivationStore) RetryActivation(context.Context, *domain.ExternalAgentJobActivation, string, time.Time, time.Time) error {
	return nil
}

func (s *fakeActivationStore) GetActivation(context.Context, string) (*domain.ExternalAgentJobActivation, error) {
	activation := s.activation
	return &activation, nil
}

func (s *fakeActivationStore) MarkActivationModelStarted(_ context.Context, activation *domain.ExternalAgentJobActivation, _ time.Time) error {
	s.modelStartedCalls++
	if s.stateMutationErr != nil {
		return s.stateMutationErr
	}
	s.activation.State = domain.ActivationModelStarted
	activation.State = domain.ActivationModelStarted
	return nil
}

func (s *fakeActivationStore) PrepareActivationResponse(_ context.Context, activation *domain.ExternalAgentJobActivation, body, digest, intentID, correlationID string, _ time.Time) error {
	s.prepareCalls++
	if s.prepareErr != nil {
		return s.prepareErr
	}
	s.activation.State = domain.ActivationResponsePrepared
	s.activation.ResponseBody = body
	s.activation.ResponseSHA256 = digest
	s.activation.ExchangeIntentID = intentID
	s.activation.CorrelationID = correlationID
	activation.State = domain.ActivationResponsePrepared
	activation.ResponseBody = body
	activation.ResponseSHA256 = digest
	activation.ExchangeIntentID = intentID
	activation.CorrelationID = correlationID
	return nil
}

func (s *fakeActivationStore) CompleteActivation(_ context.Context, activation *domain.ExternalAgentJobActivation, responseSlackTS string, _ time.Time) error {
	s.completeCalls++
	if s.stateMutationErr != nil {
		return s.stateMutationErr
	}
	s.activation.State = domain.ActivationCompleted
	s.activation.ResponseSlackTS = responseSlackTS
	activation.State = domain.ActivationCompleted
	activation.ResponseSlackTS = responseSlackTS
	s.responseSlackTS = responseSlackTS
	return nil
}

func (s *fakeActivationStore) FailActivation(_ context.Context, activation *domain.ExternalAgentJobActivation, errorCode string, _ time.Time) error {
	s.failedCalls++
	if s.stateMutationErr != nil {
		return s.stateMutationErr
	}
	s.activation.State = domain.ActivationFailed
	s.activation.LastErrorCode = errorCode
	activation.State = domain.ActivationFailed
	s.lastErrorCode = errorCode
	return nil
}

func (s *fakeActivationStore) MarkActivationCompletionUnknown(_ context.Context, activation *domain.ExternalAgentJobActivation, errorCode string, _ time.Time) error {
	s.unknownCalls++
	if s.stateMutationErr != nil {
		return s.stateMutationErr
	}
	s.activation.State = domain.ActivationCompletionUnknown
	s.activation.LastErrorCode = errorCode
	activation.State = domain.ActivationCompletionUnknown
	s.lastErrorCode = errorCode
	return nil
}

type fakeCompletionFinder struct {
	timestamp string
	found     bool
	calls     int
}

type activationRecoveryRuntime struct {
	*fakeRuntime
	turn         port.AgentTurn
	found        bool
	recoveryErr  error
	recoveryCall int
}

func (r *activationRecoveryRuntime) RecoverActivation(context.Context, domain.ConversationKey, string) (port.AgentTurn, bool, error) {
	r.recoveryCall++
	return r.turn, r.found, r.recoveryErr
}

type blockingCompletionFinder struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *blockingCompletionFinder) FindPublishedAssistantExchange(context.Context, port.AssistantExchangeIntent) (string, bool, error) {
	f.once.Do(func() { close(f.started) })
	<-f.release
	return "", false, nil
}

func (f *fakeCompletionFinder) FindPublishedAssistantExchange(_ context.Context, _ port.AssistantExchangeIntent) (string, bool, error) {
	f.calls++
	return f.timestamp, f.found, nil
}

func completionActivation(now time.Time) domain.ExternalAgentJobActivation {
	activation := domain.ExternalAgentJobActivation{
		JobID: "job-completion-1", StatusRevision: 1, Kind: domain.JobNotificationTerminal,
		TerminalStatus: domain.JobCompleted, NotificationSHA256: strings.Repeat("a", 64),
		Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678",
		OriginalCallID: "call-1", DeliveryMode: domain.JobResultDeliveryMarkdown, ContentBytes: 64,
		SlackMessageTS: "1710000000.000001", PublishedAt: now.Add(-time.Second),
		State: domain.ActivationProcessing, Attempt: 1, LeaseOwner: "activation-owner", LeaseExpiry: now.Add(time.Minute),
		NextAttemptAt: now.Add(-time.Second), CreatedAt: now.Add(-time.Minute), UpdatedAt: now,
	}
	activation.ActivationID = domain.ExternalAgentJobActivationID(activation.JobID, activation.StatusRevision, activation.Kind)
	return activation
}

func completionService(t *testing.T, activationStore *fakeActivationStore, runtime *fakeRuntime, publisher *fakePublisher) *Service {
	t.Helper()
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, nil)
	service.activationStore = activationStore
	return service
}

func activationErrorCode(t *testing.T, err error) string {
	t.Helper()
	var classified *port.ActivationProcessError
	if !errors.As(err, &classified) {
		t.Fatalf("error %v is not an ActivationProcessError", err)
	}
	return classified.Code
}

func TestHandleJobCompletionUsesDurableBindingAndDisablesMemory(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "synthesis"}}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, runtime, publisher)
	exchange := &fakeExchangeWriter{}
	service.AddMemory(fakeRecall{}, exchange)
	service.clock = fakeClock{now: now}

	if err := service.HandleJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if runtime.runCalls != 1 || len(runtime.runRequest.Messages) != 1 {
		t.Fatalf("runtime calls/context = %d %#v", runtime.runCalls, runtime.runRequest)
	}
	message := runtime.runRequest.Messages[0]
	if message.Role != domain.RoleUser || message.Source != domain.MessageSourceJobCompletion || message.UserID != activation.Actor || message.ExternalTS != activation.ActivationID {
		t.Fatalf("job completion message = %#v", message)
	}
	if runtime.runRequest.ConversationKey != activation.ConversationKey || len(runtime.runRequest.Memory) != 0 {
		t.Fatalf("runtime binding/context = %#v", runtime.runRequest)
	}
	if exchange.prepares != 1 || exchange.memoryEligible {
		t.Fatalf("exchange preparation = %#v", exchange)
	}
	if activationStore.prepareCalls != 1 || activationStore.completeCalls != 1 || activationStore.activation.State != domain.ActivationCompleted {
		t.Fatalf("activation lifecycle = %#v", activationStore)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].target.ChannelID != "D12345678" || publisher.calls[0].text != "synthesis" {
		t.Fatalf("published completion = %#v", publisher.calls)
	}

	if err := service.HandleJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if runtime.runCalls != 1 || len(publisher.calls) != 1 {
		t.Fatalf("retry duplicated model or response: runtime=%d publishes=%d", runtime.runCalls, len(publisher.calls))
	}
}

func TestHandleJobCompletionRejectsSuppliedActorAndConversationMismatch(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	for _, test := range []struct {
		name   string
		mutate func(*domain.ExternalAgentJobActivation)
	}{
		{name: "actor", mutate: func(a *domain.ExternalAgentJobActivation) { a.Actor = "U99999999" }},
		{name: "conversation", mutate: func(a *domain.ExternalAgentJobActivation) { a.ConversationKey = "slack:T12345678:dm:D99999999" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			stored := completionActivation(now)
			activationStore := &fakeActivationStore{activation: stored}
			service := completionService(t, activationStore, &fakeRuntime{runTurn: port.AgentTurn{Text: "must not run"}}, &fakePublisher{})
			supplied := stored
			test.mutate(&supplied)
			err := service.HandleJobCompletion(t.Context(), supplied)
			if got := activationErrorCode(t, err); got != "activation_identity_invalid" {
				t.Fatalf("error code = %q", got)
			}
			if activationStore.modelStartedCalls != 0 {
				t.Fatal("mismatched activation crossed model boundary")
			}
		})
	}
}

func TestHandleJobCompletionRequeuesConversationBusyWithoutSlack(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "must not run"}}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, runtime, publisher)
	release, acquired := service.limiter.TryAcquire(string(activation.ConversationKey))
	if !acquired {
		t.Fatal("failed to occupy conversation limiter")
	}
	defer release()

	err := service.HandleJobCompletion(t.Context(), activation)
	if got := activationErrorCode(t, err); got != "conversation_busy" {
		t.Fatalf("error code = %q", got)
	}
	if runtime.runCalls != 0 || activationStore.modelStartedCalls != 0 || len(publisher.calls) != 0 {
		t.Fatalf("busy activation had side effects: runtime=%d model_started=%d publishes=%d", runtime.runCalls, activationStore.modelStartedCalls, len(publisher.calls))
	}
}

func TestHandleJobCompletionRevokedActorFailsDurably(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "must not run"}}
	service := completionService(t, activationStore, runtime, &fakePublisher{})
	service.cfg.AccessPolicy.AllowedUserIDs = []string{"U99999999"}

	err := service.HandleJobCompletion(t.Context(), activation)
	if got := activationErrorCode(t, err); got != "actor_revoked" {
		t.Fatalf("error code = %q", got)
	}
	if activationStore.failedCalls != 1 || activationStore.lastErrorCode != "actor_revoked" || runtime.runCalls != 0 {
		t.Fatalf("revoked actor handling = %#v", activationStore)
	}
}

func TestResponsePreparedRetryUsesPublishedEvidenceWithoutModelReplay(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activation.State = domain.ActivationResponsePrepared
	activation.ResponseBody = "durable synthesis"
	activation.ResponseSHA256 = sha256Hex(activation.ResponseBody)
	activation.ExchangeIntentID = "intent-1"
	activation.CorrelationID = "corr-1"
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "must not run"}}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, runtime, publisher)
	service.exchange = &fakeExchangeWriter{}
	service.exchangeFinder = &fakeCompletionFinder{timestamp: "1710000002.000001", found: true}

	if err := service.ReconcileJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if runtime.runCalls != 0 || len(publisher.calls) != 0 || activationStore.completeCalls != 1 || activationStore.activation.State != domain.ActivationCompleted {
		t.Fatalf("response reconciliation = runtime=%d publishes=%d store=%#v", runtime.runCalls, len(publisher.calls), activationStore)
	}
	if err := service.ReconcileJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if activationStore.completeCalls != 1 || len(publisher.calls) != 0 {
		t.Fatalf("reconciliation retry duplicated completion: completes=%d publishes=%d", activationStore.completeCalls, len(publisher.calls))
	}
}

func TestModelStartedReconcilesDurableADKFinalWithoutModelReplay(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activation.State = domain.ActivationModelStarted
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &activationRecoveryRuntime{fakeRuntime: &fakeRuntime{runTurn: port.AgentTurn{Text: "must not run"}}, turn: port.AgentTurn{Text: "recovered synthesis"}, found: true}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, runtime.fakeRuntime, publisher)
	service.runtime = runtime
	service.exchange = &fakeExchangeWriter{}

	if err := service.ReconcileJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if runtime.recoveryCall != 1 || runtime.runCalls != 0 || activationStore.activation.State != domain.ActivationCompleted {
		t.Fatalf("recovery lifecycle = calls:%d run:%d state:%q", runtime.recoveryCall, runtime.runCalls, activationStore.activation.State)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != "recovered synthesis" {
		t.Fatalf("recovered publication = %#v", publisher.calls)
	}
}

func TestModelStartedWithoutDurableFinalBecomesCompletionUnknown(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activation.State = domain.ActivationModelStarted
	activationStore := &fakeActivationStore{activation: activation}
	service := completionService(t, activationStore, &fakeRuntime{runTurn: port.AgentTurn{Text: "must not run"}}, &fakePublisher{})

	if err := service.ReconcileJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if activationStore.unknownCalls != 1 || activationStore.lastErrorCode != "completion_unknown" || activationStore.activation.State != domain.ActivationCompletionUnknown {
		t.Fatalf("unknown lifecycle = %#v", activationStore)
	}
}

func TestRetryMovesExistingCompletionEnvelopeAfterInterleavedHumanTurn(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "human response"}}
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	service := completionService(t, activationStore, runtime, &fakePublisher{})
	service.store = store
	service.exchange = &fakeExchangeWriter{}
	modelLimiter := &trackingModelCallLimiter{}
	service.modelCalls = modelLimiter
	release, acquired := modelLimiter.TryAcquire()
	if !acquired {
		t.Fatal("failed to occupy model limiter")
	}
	err := service.HandleJobCompletion(t.Context(), activation)
	release()
	if got := activationErrorCode(t, err); got != "model_busy" {
		t.Fatalf("busy error code = %q", got)
	}
	if countJobCompletionMessages(store.appended) != 1 {
		t.Fatalf("persisted envelope count after busy = %d", countJobCompletionMessages(store.appended))
	}

	// The human turn is persisted after the busy activation attempt.
	store.recent[activation.ConversationKey] = append([]domain.Message(nil), store.appended...)
	invocation := botInvocation()
	invocation.EventID = "human-interleaved"
	invocation.Text = "human follow-up"
	if outcome, err := service.Handle(t.Context(), invocation); err != nil || outcome != OutcomeResponded {
		t.Fatalf("human interleave outcome=%q err=%v", outcome, err)
	}
	store.recent[activation.ConversationKey] = append([]domain.Message(nil), store.appended...)

	runtime.runTurn = port.AgentTurn{Text: "activation retry response"}
	if err := service.HandleJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if runtime.runRequest.Messages[len(runtime.runRequest.Messages)-1].Source != domain.MessageSourceJobCompletion || runtime.runRequest.Messages[len(runtime.runRequest.Messages)-1].ExternalTS != activation.ActivationID {
		t.Fatalf("retry current input = %#v", runtime.runRequest.Messages[len(runtime.runRequest.Messages)-1])
	}
	if countJobCompletionMessages(runtime.runRequest.Messages) != 1 || countJobCompletionMessages(store.appended) != 1 {
		t.Fatalf("retry duplicated envelope: model=%d durable=%d", countJobCompletionMessages(runtime.runRequest.Messages), countJobCompletionMessages(store.appended))
	}
}

func TestReconcileResponsePreparedUsesConversationCoordinator(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activation.State = domain.ActivationResponsePrepared
	activation.ResponseBody = "prepared response"
	activation.ResponseSHA256 = sha256Hex(activation.ResponseBody)
	activation.ExchangeIntentID = "intent-1"
	activation.CorrelationID = "corr-1"
	activationStore := &fakeActivationStore{activation: activation}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, &fakeRuntime{runTurn: port.AgentTurn{Text: "must not run"}}, publisher)
	service.exchange = &fakeExchangeWriter{}
	finder := &blockingCompletionFinder{started: make(chan struct{}), release: make(chan struct{})}
	service.exchangeFinder = finder

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- service.ReconcileJobCompletion(t.Context(), activation) }()
	select {
	case <-finder.started:
	case <-time.After(time.Second):
		t.Fatal("reconciliation did not reach the durable exchange lookup")
	}

	human := botInvocation()
	human.EventID = "human-during-reconcile"
	if outcome, err := service.Handle(t.Context(), human); err != nil || outcome != OutcomeBusy {
		t.Fatalf("human during reconciliation outcome=%q err=%v", outcome, err)
	}
	close(finder.release)
	if err := <-reconcileDone; err != nil {
		t.Fatal(err)
	}
	if runtime, ok := service.runtime.(*fakeRuntime); !ok || runtime.runCalls != 0 {
		t.Fatal("human turn crossed coordinator while reconciliation was active")
	}
}

func countJobCompletionMessages(messages []domain.Message) int {
	count := 0
	for _, message := range messages {
		if message.Source == domain.MessageSourceJobCompletion {
			count++
		}
	}
	return count
}

func TestResponsePreparationSurvivesFinalizeCrashWithoutRepublishing(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "durable synthesis"}}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, runtime, publisher)
	writer := &fakeExchangeWriter{err: errors.New("crash after Slack acceptance")}
	service.exchange = writer

	if err := service.HandleJobCompletion(t.Context(), activation); err == nil {
		t.Fatal("expected finalize failure")
	}
	if activationStore.activation.State != domain.ActivationResponsePrepared || runtime.runCalls != 1 || len(publisher.calls) != 1 {
		t.Fatalf("post-crash durable state = %#v runtime=%d publishes=%d", activationStore, runtime.runCalls, len(publisher.calls))
	}

	writer.err = nil
	service.exchangeFinder = &fakeCompletionFinder{timestamp: "1700000002.000003", found: true}
	if err := service.ReconcileJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if runtime.runCalls != 1 || len(publisher.calls) != 1 || activationStore.activation.State != domain.ActivationCompleted {
		t.Fatalf("restarted completion duplicated work: runtime=%d publishes=%d activation=%#v", runtime.runCalls, len(publisher.calls), activationStore)
	}
}

func TestPendingConfirmationCannotCreateActivationPrompt(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{PendingConfirmation: &domain.PendingConfirmation{WrapperCallID: "wrapper", OriginalCallID: "call", Summary: "write"}}}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, runtime, publisher)
	service.exchange = &fakeExchangeWriter{}

	if err := service.HandleJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if activationStore.unknownCalls != 1 || activationStore.lastErrorCode != "activation_confirmation_not_allowed" || len(publisher.calls) != 0 {
		t.Fatalf("pending confirmation outcome = %#v publishes=%d", activationStore, len(publisher.calls))
	}
}
