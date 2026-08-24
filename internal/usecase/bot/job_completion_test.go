package bot

import (
	"context"
	"errors"
	"fmt"
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

type fakeActivationResultReader struct {
	result      domain.ExternalAgentJobResult
	handle      domain.ResultHandle
	found       bool
	err         error
	readCalls   int
	nativeCalls int
}

func (r *fakeActivationResultReader) Status(context.Context, string, string, domain.ConversationKey) (*domain.ExternalAgentJob, error) {
	return nil, nil
}

func (r *fakeActivationResultReader) ReadResult(context.Context, string, string, domain.ConversationKey) (domain.ExternalAgentJobResult, error) {
	r.readCalls++
	return r.result, r.err
}

func (r *fakeActivationResultReader) ReadResultChunk(context.Context, string, string, domain.ConversationKey, int64, int64) (domain.ResultChunk, error) {
	return domain.ResultChunk{}, r.err
}

func (r *fakeActivationResultReader) NativeResultHandleForJob(context.Context, string, string, domain.ConversationKey) (domain.ResultHandle, bool, error) {
	r.nativeCalls++
	return r.handle, r.found, r.err
}

type fakeActivationWorkstreamService struct {
	snapshot        domain.WorkstreamSnapshot
	err             error
	applyHumanCalls []domain.WorkstreamTransition
}

func (s *fakeActivationWorkstreamService) SnapshotForActivation(context.Context, string, string, domain.ConversationKey) (domain.WorkstreamSnapshot, error) {
	return s.snapshot, s.err
}

func (*fakeActivationWorkstreamService) CompletionBindingForTask(context.Context, string, domain.ConversationKey, string, string) (domain.ExternalAgentJobCompletionBinding, bool, error) {
	return domain.ExternalAgentJobCompletionBinding{}, false, nil
}

func (s *fakeActivationWorkstreamService) CreateHuman(context.Context, port.WorkstreamBinding, string, string, string) (domain.WorkstreamSnapshot, error) {
	return domain.WorkstreamSnapshot{}, nil
}

func (s *fakeActivationWorkstreamService) ApplyHuman(_ context.Context, _ port.WorkstreamBinding, transition domain.WorkstreamTransition) (domain.WorkstreamTransitionRecord, domain.WorkstreamSnapshot, error) {
	s.applyHumanCalls = append(s.applyHumanCalls, transition)
	return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, nil
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

type blockingResumeRuntime struct {
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	mu        sync.Mutex
	resumes   int
}

func (r *blockingResumeRuntime) Run(context.Context, port.AgentRequest) (port.AgentTurn, error) {
	return port.AgentTurn{}, nil
}

func (r *blockingResumeRuntime) Resume(ctx context.Context, _ domain.ConfirmationDecision) (port.AgentTurn, error) {
	r.mu.Lock()
	r.resumes++
	r.mu.Unlock()
	r.startOnce.Do(func() { close(r.started) })
	select {
	case <-r.release:
		return port.AgentTurn{Text: "expired confirmation closed"}, nil
	case <-ctx.Done():
		return port.AgentTurn{}, ctx.Err()
	}
}

func (r *blockingResumeRuntime) resumeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resumes
}

type immediateResumeRuntime struct {
	resumes  int
	returned bool
}

func (r *immediateResumeRuntime) Run(context.Context, port.AgentRequest) (port.AgentTurn, error) {
	return port.AgentTurn{}, nil
}

func (r *immediateResumeRuntime) Resume(context.Context, domain.ConfirmationDecision) (port.AgentTurn, error) {
	r.resumes++
	r.returned = true
	return port.AgentTurn{Text: "expired confirmation closed"}, nil
}

type expiredConfirmationStore struct {
	fakeConfirmationStore
	expired []port.ConfirmationDelivery
}

func (s *expiredConfirmationStore) ExpireDelivery(_ context.Context, wrapperCallID string, now time.Time) (bool, error) {
	for index := range s.expired {
		if s.expired[index].WrapperCallID == wrapperCallID &&
			(s.expired[index].Status == port.ConfirmationPending || s.expired[index].Status == port.ConfirmationPublished) &&
			!s.expired[index].Expiry.After(now) {
			s.expired[index].Status = port.ConfirmationExpired
			return true, nil
		}
	}
	if s.delivery != nil && s.delivery.WrapperCallID == wrapperCallID &&
		(s.delivery.Status == port.ConfirmationPending || s.delivery.Status == port.ConfirmationPublished) &&
		!s.delivery.Expiry.After(now) {
		s.delivery.Status = port.ConfirmationExpired
		return true, nil
	}
	return false, nil
}

func (s *expiredConfirmationStore) ListExpired(_ context.Context, now time.Time) ([]port.ConfirmationDelivery, error) {
	var result []port.ConfirmationDelivery
	for _, delivery := range s.expired {
		if (delivery.Status == port.ConfirmationPending || delivery.Status == port.ConfirmationPublished) && !delivery.Expiry.After(now) {
			result = append(result, delivery)
		}
	}
	if s.delivery != nil &&
		(s.delivery.Status == port.ConfirmationPending || s.delivery.Status == port.ConfirmationPublished) &&
		!s.delivery.Expiry.After(now) {
		result = append(result, *s.delivery)
	}
	return result, nil
}

func (f *fakeCompletionFinder) FindPublishedAssistantExchange(_ context.Context, _ port.AssistantExchangeIntent) (string, bool, error) {
	f.calls++
	return f.timestamp, f.found, nil
}

func completionActivation(now time.Time) domain.ExternalAgentJobActivation {
	const resultText = "verified completion result"
	activation := domain.ExternalAgentJobActivation{
		JobID: "job-completion-1", StatusRevision: 1, Kind: domain.JobNotificationTerminal,
		TerminalStatus: domain.JobCompleted, NotificationSHA256: strings.Repeat("a", 64), ResultSHA256: sha256Hex(resultText),
		Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678",
		OriginalCallID: "call-1", DeliveryMode: domain.JobResultDeliveryMarkdown, ContentBytes: int64(len(resultText)),
		SlackMessageTS: "1710000000.000001", PublishedAt: now.Add(-time.Second),
		State: domain.ActivationProcessing, Attempt: 1, LeaseOwner: "activation-owner", LeaseExpiry: now.Add(time.Minute),
		NextAttemptAt: now.Add(-time.Second), CreatedAt: now.Add(-time.Minute), UpdatedAt: now,
	}
	activation.ActivationID = domain.ExternalAgentJobActivationID(activation.JobID, activation.StatusRevision, activation.Kind)
	return activation
}

func TestActivationFrameLoadsTrustedWorkstreamSnapshot(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activation.WorkstreamID = "ws-1"
	activation.TaskID = "task-1"
	activation.ExecutionIdentity = "exec-1"
	task := domain.WorkstreamTask{ID: "task-1", Project: "workspace", Description: "inspect repository", Status: domain.TaskRunning, ExecutionIdentity: "exec-1"}
	service := completionService(t, &fakeActivationStore{activation: activation}, &fakeRuntime{}, &fakePublisher{})
	service.workstreams = &fakeActivationWorkstreamService{snapshot: domain.WorkstreamSnapshot{
		ID: "ws-1", ConversationKey: activation.ConversationKey, OwnerActor: activation.Actor,
		Project: "workspace", Status: domain.WorkstreamActive, Revision: activation.AdmissionRevision,
		Objective: "repository objective", Tasks: []domain.WorkstreamTask{task},
	}}

	frame, err := service.activationFrame(t.Context(), activation)
	if err != nil {
		t.Fatal(err)
	}
	if err := frame.Validate(); err != nil {
		t.Fatalf("frame validation: %v", err)
	}
	if frame.Workstream.Objective != "repository objective" || frame.Task.Description != task.Description {
		t.Fatalf("frame snapshot = %+v task = %+v", frame.Workstream, frame.Task)
	}
}

func TestActivationFrameSelectsNativeHandleAfterInlineBound(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	resultText := strings.Repeat("x", domain.MaxActivationFrameRunes+1)
	resultBytes := int64(len([]byte(resultText)))
	resultSHA := sha256Hex(resultText)
	activation.ContentBytes = resultBytes
	activation.ResultSHA256 = resultSHA
	reader := &fakeActivationResultReader{
		result: domain.ExternalAgentJobResult{JobID: activation.JobID, StatusRevision: activation.StatusRevision, Text: resultText, ContentSHA256: resultSHA, ContentBytes: resultBytes},
		handle: domain.ResultHandle{ResultID: strings.Repeat("b", 64), SHA256: resultSHA, Bytes: resultBytes, MediaType: "text/markdown", Availability: []domain.ResultAvailability{domain.ResultAvailabilityPrivateArtifact}},
		found:  true,
	}
	service := completionService(t, &fakeActivationStore{activation: activation}, &fakeRuntime{}, &fakePublisher{})
	service.completionReader = reader

	frame, err := service.activationFrame(t.Context(), activation)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Representation != domain.ActivationResultNativeHandle || frame.ResultID != reader.handle.ResultID || frame.ResultText != "" {
		t.Fatalf("native frame = %+v", frame)
	}
	if reader.readCalls != 1 || reader.nativeCalls != 1 {
		t.Fatalf("reader calls = read:%d native:%d", reader.readCalls, reader.nativeCalls)
	}
	if err := frame.Validate(); err != nil {
		t.Fatalf("native frame validation: %v", err)
	}
}

func TestActivationFrameAppliesDirectInlineAdmission(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	resultText := strings.Repeat("a", 1024)
	resultBytes := int64(len([]byte(resultText)))
	resultSHA := sha256Hex(resultText)
	activation.ContentBytes = resultBytes
	activation.ResultSHA256 = resultSHA
	handle := domain.ResultHandle{ResultID: strings.Repeat("d", 64), SHA256: resultSHA, Bytes: resultBytes, MediaType: "text/markdown", Availability: []domain.ResultAvailability{domain.ResultAvailabilityPrivateArtifact}}

	build := func(t *testing.T, resultHandlesEnabled bool, maxDirectInlineBytes int64) *Service {
		t.Helper()
		service := completionService(t, &fakeActivationStore{activation: activation}, &fakeRuntime{}, &fakePublisher{})
		service.completionReader = &fakeActivationResultReader{
			result: domain.ExternalAgentJobResult{
				JobID: activation.JobID, StatusRevision: activation.StatusRevision, Text: resultText,
				ContentSHA256: resultSHA, ContentBytes: resultBytes,
			},
			handle: handle, found: true,
		}
		service.cfg.ResultHandlesEnabled = resultHandlesEnabled
		service.cfg.MaxDirectInlineBytes = maxDirectInlineBytes
		return service
	}

	t.Run("admitted", func(t *testing.T) {
		frame, err := build(t, true, 2048).activationFrame(t.Context(), activation)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Representation != domain.ActivationResultDirectInline || frame.ResultText != resultText || frame.ResultID != "" {
			t.Fatalf("admitted inline frame = %+v", frame)
		}
	})
	t.Run("over admission selects native handle", func(t *testing.T) {
		frame, err := build(t, true, 512).activationFrame(t.Context(), activation)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Representation != domain.ActivationResultNativeHandle || frame.ResultText != "" || frame.ResultID != handle.ResultID {
			t.Fatalf("non-admitted frame = %+v", frame)
		}
	})
	t.Run("gate-enabled zero declaration means no inline", func(t *testing.T) {
		frame, err := build(t, true, 0).activationFrame(t.Context(), activation)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Representation != domain.ActivationResultNativeHandle || frame.ResultText != "" {
			t.Fatalf("zero-declaration frame = %+v", frame)
		}
	})
	t.Run("gate-disabled keeps legacy selection", func(t *testing.T) {
		frame, err := build(t, false, 0).activationFrame(t.Context(), activation)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Representation != domain.ActivationResultDirectInline {
			t.Fatalf("legacy selection frame = %+v", frame)
		}
	})
}

func TestActivationFrameSelectsArtifactOnlyForFileDelivery(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	resultText := strings.Repeat("y", domain.MaxActivationFrameRunes+1)
	resultBytes := int64(len([]byte(resultText)))
	resultSHA := sha256Hex(resultText)
	activation.ContentBytes = resultBytes
	activation.ResultSHA256 = resultSHA
	reader := &fakeActivationResultReader{result: domain.ExternalAgentJobResult{
		JobID: activation.JobID, StatusRevision: activation.StatusRevision, Text: resultText,
		ContentSHA256: resultSHA, ContentBytes: resultBytes, DeliveryMode: domain.JobResultDeliveryFile,
	}}
	service := completionService(t, &fakeActivationStore{activation: activation}, &fakeRuntime{}, &fakePublisher{})
	service.completionReader = reader

	frame, err := service.activationFrame(t.Context(), activation)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Representation != domain.ActivationResultArtifactOnly || frame.ResultText != "" || frame.ResultID != "" {
		t.Fatalf("artifact-only frame = %+v", frame)
	}
	if frame.ResultMediaType != "text/markdown" || len(frame.ResultAvailability) != 1 || frame.ResultAvailability[0] != domain.ResultAvailabilityPrivateArtifact {
		t.Fatalf("artifact-only availability = %#v", frame)
	}
	if reader.nativeCalls != 1 {
		t.Fatalf("native reader calls = %d", reader.nativeCalls)
	}
	if err := frame.Validate(); err != nil {
		t.Fatalf("artifact-only frame validation: %v", err)
	}
}

func TestHandleJobCompletionRunsNativeHandleFrameWithoutResultText(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	resultText := strings.Repeat("z", domain.MaxActivationFrameRunes+1)
	resultBytes := int64(len([]byte(resultText)))
	resultSHA := sha256Hex(resultText)
	activation.ContentBytes = resultBytes
	activation.ResultSHA256 = resultSHA
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "handle synthesis"}}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, runtime, publisher)
	service.completionReader = &fakeActivationResultReader{
		result: domain.ExternalAgentJobResult{
			JobID: activation.JobID, StatusRevision: activation.StatusRevision, Text: resultText,
			ContentSHA256: resultSHA, ContentBytes: resultBytes,
		},
		handle: domain.ResultHandle{ResultID: strings.Repeat("c", 64), SHA256: resultSHA, Bytes: resultBytes, MediaType: "text/markdown", Availability: []domain.ResultAvailability{domain.ResultAvailabilityPrivateArtifact}},
		found:  true,
	}
	service.exchange = &fakeExchangeWriter{}

	if err := service.HandleJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if runtime.runCalls != 1 || len(runtime.runRequest.Messages) != 1 {
		t.Fatalf("native-handle run = %d %#v", runtime.runCalls, runtime.runRequest)
	}
	frameText := runtime.runRequest.Messages[0].Content
	if !strings.Contains(frameText, `"result_representation":"native_handle"`) || !strings.Contains(frameText, strings.Repeat("c", 64)) || strings.Contains(frameText, resultText) {
		t.Fatalf("native-handle frame leaked or omitted identity: %q", frameText)
	}
	if activationStore.activation.State != domain.ActivationCompleted || activationStore.prepareCalls != 1 || len(publisher.calls) != 1 || publisher.calls[0].text != "handle synthesis" {
		t.Fatalf("native-handle lifecycle = %#v publishes=%#v", activationStore, publisher.calls)
	}
}

func TestHandleJobCompletionRunsArtifactOnlyFrameForFileDelivery(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	resultText := strings.Repeat("w", domain.MaxActivationFrameRunes+1)
	resultBytes := int64(len([]byte(resultText)))
	resultSHA := sha256Hex(resultText)
	activation.ContentBytes = resultBytes
	activation.ResultSHA256 = resultSHA
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "artifact synthesis"}}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, runtime, publisher)
	service.completionReader = &fakeActivationResultReader{result: domain.ExternalAgentJobResult{
		JobID: activation.JobID, StatusRevision: activation.StatusRevision, Text: resultText,
		ContentSHA256: resultSHA, ContentBytes: resultBytes, DeliveryMode: domain.JobResultDeliveryFile,
	}}
	service.exchange = &fakeExchangeWriter{}

	if err := service.HandleJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if runtime.runCalls != 1 || len(runtime.runRequest.Messages) != 1 {
		t.Fatalf("artifact-only run = %d %#v", runtime.runCalls, runtime.runRequest)
	}
	frameText := runtime.runRequest.Messages[0].Content
	if !strings.Contains(frameText, `"result_representation":"artifact_only"`) || !strings.Contains(frameText, `"result_media_type":"text/markdown"`) || strings.Contains(frameText, resultText) {
		t.Fatalf("artifact-only frame leaked or omitted availability: %q", frameText)
	}
	if activationStore.activation.State != domain.ActivationCompleted || len(publisher.calls) != 1 || publisher.calls[0].text != "artifact synthesis" {
		t.Fatalf("artifact-only lifecycle = %#v publishes=%#v", activationStore, publisher.calls)
	}
}

func TestHandleJobCompletionDoesNotInvokeRootForUnavailableResult(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	resultText := strings.Repeat("x", domain.MaxActivationFrameRunes+1)
	activation.ContentBytes = int64(len(resultText))
	activation.ResultSHA256 = sha256Hex(resultText)
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "must not run"}}
	service := completionService(t, activationStore, runtime, &fakePublisher{})
	service.completionReader = &fakeActivationResultReader{result: domain.ExternalAgentJobResult{
		JobID: activation.JobID, StatusRevision: activation.StatusRevision, Text: resultText,
		ContentSHA256: activation.ResultSHA256, ContentBytes: activation.ContentBytes,
	}}

	err := service.HandleJobCompletion(t.Context(), activation)
	if got := activationErrorCode(t, err); got != "activation_result_unavailable" {
		t.Fatalf("error code = %q", got)
	}
	if runtime.runCalls != 0 || activationStore.modelStartedCalls != 0 || activationStore.failedCalls != 1 {
		t.Fatalf("unavailable activation crossed model boundary: runtime=%d model_started=%d failed=%d", runtime.runCalls, activationStore.modelStartedCalls, activationStore.failedCalls)
	}
}

func TestActivationFrameRejectsMissingTerminalResultIdentity(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activation.ResultSHA256 = ""
	service := completionService(t, &fakeActivationStore{activation: activation}, &fakeRuntime{}, &fakePublisher{})
	service.completionReader = &fakeActivationResultReader{result: domain.ExternalAgentJobResult{
		JobID: activation.JobID, StatusRevision: activation.StatusRevision, Text: "verified completion result",
		ContentSHA256: sha256Hex("verified completion result"), ContentBytes: activation.ContentBytes,
	}}

	if _, err := service.activationFrame(t.Context(), activation); err == nil {
		t.Fatal("activation frame accepted a missing terminal result digest")
	}
}

func completionService(t *testing.T, activationStore *fakeActivationStore, runtime *fakeRuntime, publisher *fakePublisher) *Service {
	t.Helper()
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, nil)
	service.activationStore = activationStore
	const resultText = "verified completion result"
	service.completionReader = &fakeActivationResultReader{result: domain.ExternalAgentJobResult{
		JobID: activationStore.activation.JobID, StatusRevision: activationStore.activation.StatusRevision, Text: resultText,
		ContentSHA256: sha256Hex(resultText), ContentBytes: int64(len(resultText)),
	}}
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
	service.SetExchange(exchange)
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
	if runtime.runRequest.ConversationKey != activation.ConversationKey {
		t.Fatalf("runtime binding/context = %#v", runtime.runRequest)
	}
	if exchange.prepares != 1 {
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

func TestHandleJobCompletionDoesNotLoadAmbientConversation(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	store := &fakeStore{recent: map[domain.ConversationKey][]domain.Message{
		activation.ConversationKey: {
			{Role: domain.RoleUser, Source: domain.MessageSourceHuman, Content: "old human context"},
			{Role: domain.RoleAssistant, Source: domain.MessageSourceAssistant, Content: "old assistant context"},
		},
	}}
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "synthesis"}}
	service := newTestService(t, store, runtime, &fakeHistory{}, &fakePublisher{}, nil)
	service.activationStore = activationStore
	service.clock = fakeClock{now: now}
	service.SetExchange(&fakeExchangeWriter{})
	const resultText = "verified completion result"
	service.completionReader = &fakeActivationResultReader{result: domain.ExternalAgentJobResult{
		JobID: activation.JobID, StatusRevision: activation.StatusRevision, Text: resultText,
		ContentSHA256: activation.ResultSHA256, ContentBytes: activation.ContentBytes,
	}}

	if err := service.HandleJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if len(runtime.runRequest.Messages) != 1 || runtime.runRequest.Messages[0].Source != domain.MessageSourceJobCompletion {
		t.Fatalf("activation model context = %#v, want only current completion frame", runtime.runRequest.Messages)
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

type blockingSnapshotWorkstreamService struct {
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
	snapshot domain.WorkstreamSnapshot
	applied  []domain.WorkstreamTransition
}

func (s *blockingSnapshotWorkstreamService) SnapshotForActivation(ctx context.Context, workstreamID, actor string, key domain.ConversationKey) (domain.WorkstreamSnapshot, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return s.snapshot, nil
	case <-ctx.Done():
		return domain.WorkstreamSnapshot{}, ctx.Err()
	}
}

func (*blockingSnapshotWorkstreamService) CompletionBindingForTask(context.Context, string, domain.ConversationKey, string, string) (domain.ExternalAgentJobCompletionBinding, bool, error) {
	return domain.ExternalAgentJobCompletionBinding{}, false, nil
}

func (*blockingSnapshotWorkstreamService) CreateHuman(context.Context, port.WorkstreamBinding, string, string, string) (domain.WorkstreamSnapshot, error) {
	return domain.WorkstreamSnapshot{}, nil
}

func (s *blockingSnapshotWorkstreamService) ApplyHuman(_ context.Context, _ port.WorkstreamBinding, transition domain.WorkstreamTransition) (domain.WorkstreamTransitionRecord, domain.WorkstreamSnapshot, error) {
	s.applied = append(s.applied, transition)
	return domain.WorkstreamTransitionRecord{WorkstreamID: transition.WorkstreamID, Action: transition.Action, ToRevision: 1}, domain.WorkstreamSnapshot{}, nil
}

func TestJobCompletionFrameReadHoldsConversationCoordinator(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activation.WorkstreamID = "ws-1"
	activation.TaskID = "task-1"
	activation.ExecutionIdentity = "exec-1"
	task := domain.WorkstreamTask{ID: "task-1", Project: "workspace", Description: "inspect repository", Status: domain.TaskRunning, ExecutionIdentity: "exec-1"}
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "synthesis"}}
	publisher := &fakePublisher{}
	store := &fakeStore{claimAll: true, recent: make(map[domain.ConversationKey][]domain.Message)}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, nil)
	service.activationStore = activationStore
	const resultText = "verified completion result"
	service.completionReader = &fakeActivationResultReader{result: domain.ExternalAgentJobResult{
		JobID: activation.JobID, StatusRevision: activation.StatusRevision, Text: resultText,
		ContentSHA256: sha256Hex(resultText), ContentBytes: int64(len(resultText)),
	}}
	service.exchange = &fakeExchangeWriter{}
	workstreams := &blockingSnapshotWorkstreamService{
		started: make(chan struct{}), release: make(chan struct{}),
		snapshot: domain.WorkstreamSnapshot{
			ID: "ws-1", ConversationKey: activation.ConversationKey, OwnerActor: activation.Actor,
			Project: "workspace", Status: domain.WorkstreamActive, Revision: 0, Tasks: []domain.WorkstreamTask{task},
		},
	}
	service.workstreams = workstreams

	completionDone := make(chan error, 1)
	go func() { completionDone <- service.HandleJobCompletion(t.Context(), activation) }()
	select {
	case <-workstreams.started:
	case <-time.After(time.Second):
		t.Fatal("activation did not reach the workstream snapshot read")
	}

	human := botInvocation()
	human.EventID = "human-during-frame-read"
	human.Text = `workstream-human {"project":"workspace","workstream_id":"ws-1","expected_revision":0,"action":"pause_workstream"}`
	if outcome, err := service.Handle(t.Context(), human); err != nil || outcome != OutcomeBusy {
		t.Fatalf("human command during frame read outcome=%q err=%v", outcome, err)
	}
	if len(workstreams.applied) != 0 {
		t.Fatalf("human command mutated workstream during activation frame read: %#v", workstreams.applied)
	}

	close(workstreams.release)
	if err := <-completionDone; err != nil {
		t.Fatal(err)
	}
	if runtime.runCalls != 1 || activationStore.activation.State != domain.ActivationCompleted {
		t.Fatalf("activation after release = runtime:%d %#v", runtime.runCalls, activationStore)
	}

	human.EventID = "human-after-frame-read"
	human.EventTS = "1700000000.000003"
	if outcome, err := service.Handle(t.Context(), human); err != nil || outcome != OutcomeResponded {
		t.Fatalf("human command after release outcome=%q err=%v", outcome, err)
	}
	if len(workstreams.applied) != 1 || workstreams.applied[0].Action != domain.WorkstreamActionPauseWorkstream {
		t.Fatalf("human command did not apply after coordinator release: %#v", workstreams.applied)
	}
}

type fakeActivationFallbackStore struct {
	*fakeActivationStore
	prepared      port.PreparedAssistantExchange
	prepareCalls  int
	prepareErr    error
	completeCalls int
	completeErr   error
	lastMarkedTS  string
}

func (*fakeActivationFallbackStore) ClaimNextActivationFallback(context.Context, time.Time, string, time.Duration) (*domain.ExternalAgentJobActivation, error) {
	return nil, nil
}

func (s *fakeActivationFallbackStore) PrepareActivationFallbackExchange(_ context.Context, _ *domain.ExternalAgentJobActivation, _ domain.ConversationMetadata, _ domain.Message, _ int, _ time.Time) (port.PreparedAssistantExchange, error) {
	s.prepareCalls++
	if s.prepareErr != nil {
		return port.PreparedAssistantExchange{}, s.prepareErr
	}
	return s.prepared, nil
}

func (s *fakeActivationFallbackStore) CompleteActivationFallback(_ context.Context, _ *domain.ExternalAgentJobActivation, _ string, slackTS string, _ time.Time) error {
	s.completeCalls++
	if s.completeErr != nil {
		return s.completeErr
	}
	s.lastMarkedTS = slackTS
	s.activation.FallbackSlackTS = slackTS
	return nil
}

func terminalFallbackActivation(now time.Time) (domain.ExternalAgentJobActivation, *fakeActivationStore) {
	activation := completionActivation(now)
	activation.State = domain.ActivationFailed
	activation.LastErrorCode = "activation_result_unavailable"
	activation.FallbackRequired = true
	activation.LeaseOwner = "fallback-owner"
	activation.Attempt = 1
	activation.LeaseExpiry = now.Add(time.Minute)
	return activation, &fakeActivationStore{activation: activation}
}

func TestPublishActivationFallbackStagesExchangeBeforeSlackPublish(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation, store := terminalFallbackActivation(now)
	fallbackStore := &fakeActivationFallbackStore{
		fakeActivationStore: store,
		prepared:            port.PreparedAssistantExchange{ID: "intent-fallback-1", CorrelationID: "corr-fallback-1"},
	}
	publisher := &fakePublisher{}
	service := completionService(t, store, &fakeRuntime{}, publisher)
	service.activationStore = fallbackStore
	service.exchange = &fakeExchangeWriter{}

	if err := service.PublishActivationFallback(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if fallbackStore.prepareCalls != 1 {
		t.Fatalf("fallback exchange prepares = %d", fallbackStore.prepareCalls)
	}
	if len(publisher.calls) != 1 || !strings.Contains(publisher.calls[0].text, "integrated root response is unavailable") || publisher.calls[0].target.CorrelationID != "corr-fallback-1" {
		t.Fatalf("fallback publish = %#v", publisher.calls)
	}
	if fallbackStore.completeCalls != 1 || fallbackStore.lastMarkedTS != "1700000002.000003" {
		t.Fatalf("fallback mark = calls:%d ts:%q", fallbackStore.completeCalls, fallbackStore.lastMarkedTS)
	}
}

func TestPublishActivationFallbackReconcilesPublishedExchangeWithoutRepublish(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation, store := terminalFallbackActivation(now)
	fallbackStore := &fakeActivationFallbackStore{
		fakeActivationStore: store,
		prepared:            port.PreparedAssistantExchange{ID: "intent-fallback-1", CorrelationID: "corr-fallback-1"},
	}
	publisher := &fakePublisher{}
	service := completionService(t, store, &fakeRuntime{}, publisher)
	service.activationStore = fallbackStore
	service.exchange = &fakeExchangeWriter{}
	service.exchangeFinder = &fakeCompletionFinder{timestamp: "1710000009.000001", found: true}

	if err := service.PublishActivationFallback(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if len(publisher.calls) != 0 {
		t.Fatalf("reconciled fallback republished: %#v", publisher.calls)
	}
	if fallbackStore.completeCalls != 1 || fallbackStore.lastMarkedTS != "1710000009.000001" {
		t.Fatalf("reconciled fallback mark = calls:%d ts:%q", fallbackStore.completeCalls, fallbackStore.lastMarkedTS)
	}
}

func TestPublishActivationFallbackCrashBetweenPublishAndMarkDoesNotRepublish(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation, store := terminalFallbackActivation(now)
	fallbackStore := &fakeActivationFallbackStore{
		fakeActivationStore: store,
		prepared:            port.PreparedAssistantExchange{ID: "intent-fallback-1", CorrelationID: "corr-fallback-1"},
	}
	publisher := &fakePublisher{}
	service := completionService(t, store, &fakeRuntime{}, publisher)
	service.activationStore = fallbackStore
	service.exchange = &fakeExchangeWriter{}
	fallbackStore.completeErr = port.ErrActivationStateConflict

	if err := service.PublishActivationFallback(t.Context(), activation); err == nil {
		t.Fatal("expected the crash window failure to surface")
	}
	if len(publisher.calls) != 1 {
		t.Fatalf("first fallback attempt publishes = %#v", publisher.calls)
	}

	// The retry reconciles the deterministic exchange identity instead of
	// republishing.
	fallbackStore.completeErr = nil
	service.exchangeFinder = &fakeCompletionFinder{timestamp: "1710000009.000002", found: true}
	if err := service.PublishActivationFallback(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if len(publisher.calls) != 1 {
		t.Fatalf("crash retry republished the fallback: %#v", publisher.calls)
	}
	if fallbackStore.lastMarkedTS != "1710000009.000002" {
		t.Fatalf("crash retry marked = %q", fallbackStore.lastMarkedTS)
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
	if countJobCompletionMessages(store.appended) != 0 {
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
	if countJobCompletionMessages(runtime.runRequest.Messages) != 1 || countJobCompletionMessages(store.appended) != 0 {
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

func TestExpiredConfirmationResumeSharesConversationCoordinatorWithReconciliation(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activation.State = domain.ActivationResponsePrepared
	activation.ResponseBody = "prepared response"
	activation.ResponseSHA256 = sha256Hex(activation.ResponseBody)
	activation.ExchangeIntentID = "intent-1"
	activation.CorrelationID = "corr-1"
	activationStore := &fakeActivationStore{activation: activation}
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "expired-wrapper", OriginalCallID: "original", SessionID: "adk:" + string(activation.ConversationKey),
		Actor: activation.Actor, ConversationKey: activation.ConversationKey, Status: port.ConfirmationPublished,
		Expiry: now.Add(-time.Minute),
	}
	confirmations := &expiredConfirmationStore{delivery: &delivery}
	runtime := &blockingResumeRuntime{started: make(chan struct{}), release: make(chan struct{})}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, &fakeRuntime{}, publisher)
	service.runtime = runtime
	service.confirmationStore = confirmations
	service.clock = fakeClock{now: now}

	expiredDone := make(chan Outcome, 1)
	go func() {
		expiredDone <- service.HandleConfirmation(t.Context(), botInvocation(), delivery.WrapperCallID, true)
	}()
	select {
	case <-runtime.started:
	case <-time.After(time.Second):
		t.Fatal("expired confirmation did not reach Resume")
	}

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- service.ReconcileJobCompletion(t.Context(), activation) }()
	select {
	case err := <-reconcileDone:
		if got := activationErrorCode(t, err); got != "conversation_busy" {
			t.Fatalf("concurrent reconciliation error code = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("reconciliation waited instead of respecting the conversation coordinator")
	}
	if got := runtime.resumeCount(); got != 1 {
		t.Fatalf("resume calls while coordinator held = %d", got)
	}
	if len(publisher.calls) != 0 {
		t.Fatalf("reconciliation published while expired resume was active: %#v", publisher.calls)
	}

	close(runtime.release)
	if outcome := <-expiredDone; outcome != OutcomeIgnoredFollowup {
		t.Fatalf("expired confirmation outcome = %q", outcome)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != "This confirmation has expired." {
		t.Fatalf("expired confirmation publication = %#v", publisher.calls)
	}

	service.exchange = &fakeExchangeWriter{}
	service.exchangeFinder = &fakeCompletionFinder{found: false}
	if err := service.ReconcileJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if runtime.resumeCount() != 1 || len(publisher.calls) != 2 || publisher.calls[1].text != activation.ResponseBody {
		t.Fatalf("serialized response publication = resumes:%d calls:%#v", runtime.resumeCount(), publisher.calls)
	}
}

func TestStartupExpiredConfirmationResumeSharesConversationCoordinatorWithReconciliation(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activation.State = domain.ActivationResponsePrepared
	activation.ResponseBody = "prepared response"
	activation.ResponseSHA256 = sha256Hex(activation.ResponseBody)
	activation.ExchangeIntentID = "intent-1"
	activation.CorrelationID = "corr-1"
	activationStore := &fakeActivationStore{activation: activation}
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "startup-expired-wrapper", OriginalCallID: "original", SessionID: "adk:" + string(activation.ConversationKey),
		Actor: activation.Actor, ConversationKey: activation.ConversationKey, Status: port.ConfirmationPublished,
		Expiry: now.Add(-time.Minute),
	}
	confirmations := &expiredConfirmationStore{expired: []port.ConfirmationDelivery{delivery}}
	runtime := &blockingResumeRuntime{started: make(chan struct{}), release: make(chan struct{})}
	service := completionService(t, activationStore, &fakeRuntime{}, &fakePublisher{})
	service.runtime = runtime
	service.confirmationStore = confirmations
	service.clock = fakeClock{now: now}

	startupDone := make(chan error, 1)
	go func() { startupDone <- service.ReconcileConfirmations(t.Context(), nil) }()
	select {
	case <-runtime.started:
	case <-time.After(time.Second):
		t.Fatal("startup expiry did not reach Resume")
	}

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- service.ReconcileJobCompletion(t.Context(), activation) }()
	select {
	case err := <-reconcileDone:
		if got := activationErrorCode(t, err); got != "conversation_busy" {
			t.Fatalf("concurrent startup reconciliation error code = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("reconciliation waited instead of respecting the conversation coordinator")
	}
	if got := runtime.resumeCount(); got != 1 {
		t.Fatalf("startup resume calls while coordinator held = %d", got)
	}

	close(runtime.release)
	if err := <-startupDone; err != nil {
		t.Fatal(err)
	}

	service.exchange = &fakeExchangeWriter{}
	service.exchangeFinder = &fakeCompletionFinder{found: false}
	if err := service.ReconcileJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if runtime.resumeCount() != 1 {
		t.Fatalf("startup expiry replayed Resume: %d", runtime.resumeCount())
	}
}

func TestExpiredConfirmationRemainsRetryableWhenResponseReconciliationOwnsCoordinator(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activation.State = domain.ActivationResponsePrepared
	activation.ResponseBody = "prepared response"
	activation.ResponseSHA256 = sha256Hex(activation.ResponseBody)
	activation.ExchangeIntentID = "intent-1"
	activation.CorrelationID = "corr-1"
	activationStore := &fakeActivationStore{activation: activation}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, &fakeRuntime{}, publisher)
	service.exchange = &fakeExchangeWriter{}
	service.exchangeFinder = &blockingCompletionFinder{started: make(chan struct{}), release: make(chan struct{})}
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "retryable-expired-wrapper", OriginalCallID: "original", SessionID: "adk:" + string(activation.ConversationKey),
		Actor: activation.Actor, ConversationKey: activation.ConversationKey, Status: port.ConfirmationPublished,
		Expiry: now.Add(-time.Minute),
	}
	confirmations := &expiredConfirmationStore{delivery: &delivery}
	runtime := &blockingResumeRuntime{started: make(chan struct{}), release: make(chan struct{})}
	service.runtime = runtime
	service.confirmationStore = confirmations
	service.clock = fakeClock{now: now}

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- service.ReconcileJobCompletion(t.Context(), activation) }()
	select {
	case <-service.exchangeFinder.(*blockingCompletionFinder).started:
	case <-time.After(time.Second):
		t.Fatal("response reconciliation did not acquire the conversation coordinator")
	}

	if err := service.ReconcileConfirmations(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	expired, err := confirmations.ListExpired(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].Status != port.ConfirmationPublished {
		t.Fatalf("startup busy expiry was consumed: %#v", expired)
	}

	if outcome := service.HandleConfirmation(t.Context(), botInvocation(), delivery.WrapperCallID, true); outcome != OutcomeModelFailed {
		t.Fatalf("busy expired confirmation outcome = %q", outcome)
	}
	if runtime.resumeCount() != 0 {
		t.Fatalf("busy expired confirmation resumed %d times", runtime.resumeCount())
	}
	expired, err = confirmations.ListExpired(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].Status != port.ConfirmationPublished {
		t.Fatalf("busy expiry was consumed: %#v", expired)
	}

	close(service.exchangeFinder.(*blockingCompletionFinder).release)
	if err := <-reconcileDone; err != nil {
		t.Fatal(err)
	}

	retryDone := make(chan error, 1)
	go func() { retryDone <- service.ReconcileConfirmations(t.Context(), nil) }()
	select {
	case <-runtime.started:
	case <-time.After(time.Second):
		t.Fatal("expired confirmation was not retried after the coordinator was released")
	}
	if runtime.resumeCount() != 1 {
		t.Fatalf("expired confirmation retry count = %d", runtime.resumeCount())
	}
	close(runtime.release)
	if err := <-retryDone; err != nil {
		t.Fatal(err)
	}
	remaining, err := confirmations.ListExpired(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 || delivery.Status != port.ConfirmationExpired {
		t.Fatalf("retried expiry state = status:%q remaining:%#v", delivery.Status, remaining)
	}
}

func TestExpiredConfirmationKeepsCoordinatorThroughPostResumePublication(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activation.State = domain.ActivationResponsePrepared
	activation.ResponseBody = "prepared response"
	activation.ResponseSHA256 = sha256Hex(activation.ResponseBody)
	activation.ExchangeIntentID = "intent-1"
	activation.CorrelationID = "corr-1"
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &immediateResumeRuntime{}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, &fakeRuntime{}, publisher)
	service.runtime = runtime
	service.confirmationStore = &expiredConfirmationStore{delivery: &port.ConfirmationDelivery{
		WrapperCallID: "post-resume-expired-wrapper", OriginalCallID: "original", SessionID: "adk:" + string(activation.ConversationKey),
		Actor: activation.Actor, ConversationKey: activation.ConversationKey, Status: port.ConfirmationPublished,
		Expiry: now.Add(-time.Minute),
	}}
	service.clock = fakeClock{now: now}
	var interleaved error
	publisher.onPublish = func() {
		if !runtime.returned {
			t.Error("expired confirmation was published before Resume returned")
		}
		interleaved = service.ReconcileJobCompletion(t.Context(), activation)
	}

	if outcome := service.HandleConfirmation(t.Context(), botInvocation(), "post-resume-expired-wrapper", true); outcome != OutcomeIgnoredFollowup {
		t.Fatalf("expired confirmation outcome = %q", outcome)
	}
	if interleaved == nil || activationErrorCode(t, interleaved) != "conversation_busy" {
		t.Fatalf("post-Resume reconciliation interleaved: %v", interleaved)
	}
	if runtime.resumes != 1 || len(publisher.calls) != 1 || publisher.calls[0].text != "This confirmation has expired." {
		t.Fatalf("post-Resume expiry delivery = resumes:%d publishes:%#v", runtime.resumes, publisher.calls)
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

func TestActivationResponseProposalLabelContract(t *testing.T) {
	allowed := []string{
		"Done.\nProposal: add a verification task.",
		"Done. I suggest adding a verification task without a label.",
		"Done. Proposal: an inline mention is informational prose.",
		"Proposal: revise the plan.",
		"Proposal : a space is informational prose, not a label.",
		"done\nproposal: revise the plan.",
	}
	for _, response := range allowed {
		if !activationResponseAllowed(response) {
			t.Fatalf("response rejected: %q", response)
		}
	}
	rejected := []string{
		"Proposal: change A.\nProposal: change B.",
		"proposal: change A.\nproposal: change B.",
		"Proposal: change A.\n proposal: change B.",
	}
	for _, response := range rejected {
		if activationResponseAllowed(response) {
			t.Fatalf("multi-proposal response accepted: %q", response)
		}
	}

	// The machine-recognizable contract is the line-anchored label: an inline
	// mention counts zero proposals and is informational only.
	if got := countProposalLabels("Done. Proposal: an inline mention is informational prose."); got != 0 {
		t.Fatalf("inline mention counted as a proposal: %d", got)
	}
	if got := countProposalLabels("Done.\nProposal: revise the plan."); got != 1 {
		t.Fatalf("anchored label not counted: %d", got)
	}
}

func TestJobCompletionRejectsExecutableHumanCommandInModelResponse(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: `workstream-human {"workstream_id":"ws-1"}`}}
	service := completionService(t, activationStore, runtime, &fakePublisher{})
	service.exchange = &fakeExchangeWriter{}

	if err := service.HandleJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if activationStore.unknownCalls != 1 || activationStore.lastErrorCode != "activation_response_policy_invalid" || activationStore.prepareCalls != 0 {
		t.Fatalf("activation policy outcome = %#v", activationStore)
	}
}

func TestHandleJobCompletionAllowsSingleTextOnlyProposal(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activationStore := &fakeActivationStore{activation: activation}
	const proposal = "The inspection completed cleanly.\nProposal: add a verification task before closing the objective."
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: proposal}}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, runtime, publisher)
	service.exchange = &fakeExchangeWriter{}

	if err := service.HandleJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if activationStore.activation.State != domain.ActivationCompleted || activationStore.prepareCalls != 1 || activationStore.unknownCalls != 0 {
		t.Fatalf("proposal lifecycle = %#v", activationStore)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != proposal {
		t.Fatalf("proposal publication = %#v", publisher.calls)
	}
}

func TestHandleJobCompletionRejectsMultipleTextOnlyProposals(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "Proposal: change the phase.\nProposal: also change the objective."}}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, runtime, publisher)
	service.exchange = &fakeExchangeWriter{}

	if err := service.HandleJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if activationStore.unknownCalls != 1 || activationStore.lastErrorCode != "activation_response_policy_invalid" || activationStore.prepareCalls != 0 || len(publisher.calls) != 0 {
		t.Fatalf("multi-proposal outcome = %#v publishes=%d", activationStore, len(publisher.calls))
	}
}

type irreducibleFailureRuntime struct {
	err error
}

func (r *irreducibleFailureRuntime) Run(context.Context, port.AgentRequest) (port.AgentTurn, error) {
	return port.AgentTurn{}, r.err
}

func (*irreducibleFailureRuntime) Resume(context.Context, domain.ConfirmationDecision) (port.AgentTurn, error) {
	return port.AgentTurn{}, nil
}

func TestHandleJobCompletionFailsTerminalOnIrreducibleFrame(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activationStore := &fakeActivationStore{activation: activation}
	runtime := &irreducibleFailureRuntime{err: fmt.Errorf("compile frame: %w", domain.ErrIrreducibleContext)}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, &fakeRuntime{}, publisher)
	service.runtime = runtime
	service.exchange = &fakeExchangeWriter{}

	err := service.HandleJobCompletion(t.Context(), activation)
	if got := activationErrorCode(t, err); got != "activation_frame_invalid" {
		t.Fatalf("irreducible error code = %q", got)
	}
	if activationStore.failedCalls != 1 || activationStore.lastErrorCode != "activation_frame_invalid" || activationStore.modelStartedCalls != 0 {
		t.Fatalf("irreducible frame outcome = %#v", activationStore)
	}
	var classified *port.ActivationProcessError
	if errors.As(err, &classified) && classified.Retryable {
		t.Fatalf("irreducible frame failure must be terminal: %v", classified)
	}
}

func TestJobCompletionProposalStaysInformationalUntilHumanCommand(t *testing.T) {
	now := time.Unix(1710000000, 0).UTC()
	activation := completionActivation(now)
	activationStore := &fakeActivationStore{activation: activation}
	const proposal = "Proposal: propose a verification task for the completed inspection."
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: proposal}}
	publisher := &fakePublisher{}
	service := completionService(t, activationStore, runtime, publisher)
	service.exchange = &fakeExchangeWriter{}
	workstreams := &fakeActivationWorkstreamService{}
	service.workstreams = workstreams

	if err := service.HandleJobCompletion(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if len(workstreams.applyHumanCalls) != 0 {
		t.Fatalf("model proposal mutated workstream state: %#v", workstreams.applyHumanCalls)
	}
	if runtime.runCalls != 1 || len(publisher.calls) != 1 {
		t.Fatalf("proposal activation delivery = runtime:%d publishes:%#v", runtime.runCalls, publisher.calls)
	}

	invocation := botInvocation()
	invocation.EventID = "human-command-after-proposal"
	invocation.Text = `workstream-human {"project":"workspace","workstream_id":"ws-1","expected_revision":0,"action":"propose_task","task_id":"task-2","task_description":"verify the completed inspection"}`
	if outcome, err := service.Handle(t.Context(), invocation); err != nil || outcome != OutcomeResponded {
		t.Fatalf("human command outcome=%q err=%v", outcome, err)
	}
	if len(workstreams.applyHumanCalls) != 1 || workstreams.applyHumanCalls[0].Action != domain.WorkstreamActionProposeTask {
		t.Fatalf("human command did not reach the trusted path: %#v", workstreams.applyHumanCalls)
	}
	if runtime.runCalls != 1 || len(publisher.calls) != 2 {
		t.Fatalf("human command crossed the model or duplicated publication: runtime=%d publishes=%#v", runtime.runCalls, publisher.calls)
	}
}
