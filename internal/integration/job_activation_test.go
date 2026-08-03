package integration_test

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/adkagent"
	metricsadapter "github.com/Dauno/slack-local-agent/internal/adapter/metrics"
	modelcalllimiter "github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/adapter/toolfactory"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
	externalagent "github.com/Dauno/slack-local-agent/internal/usecase/externalagent"
)

func TestJobCompletionActivationEndToEnd(t *testing.T) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "activation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Add(-time.Minute)
	key := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1710000000.000001")
	job := integrationDetachedJob("job_activation_e2e", now)
	job.ConversationKey = key
	job.OriginalCallID = "original-call-e2e"
	job.RequestSHA256 = domain.ExternalAgentJobRequestDigest(domain.ExternalAgentJobRequest{
		Provider: job.Provider, Profile: job.Profile, PrimaryProject: job.PrimaryProject,
		RegistryRevision: job.RegistryRevision, Task: job.Task, Mode: job.Mode,
		Actor: job.Actor, TeamID: job.TeamID, ConversationKey: job.ConversationKey,
	})
	jobs := adaptersqlite.NewExternalAgentJobStore(store)
	created, _, err := jobs.CreateIfAbsent(t.Context(), job)
	if err != nil || !created {
		t.Fatalf("create job = %v, err=%v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "execution-worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim job = %#v, err=%v", claimed, err)
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, integrationMarkdownResult(job.ID, "result available to root"), "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	jobService, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute,
		PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1,
	}, externalagent.Dependencies{Store: jobs, Runtime: &activationJobRuntime{}})
	if err != nil {
		t.Fatal(err)
	}
	metrics := metricsadapter.NewRecorder()
	notificationPublisher := &jobActivationNotificationPublisher{}
	notificationWorker, err := externalagent.NewNotificationWorker(externalagent.NotificationConfig{
		PollInterval: time.Millisecond, LeaseTTL: time.Minute,
	}, externalagent.NotificationDependencies{
		Store: jobs, Publisher: notificationPublisher, HostCompleter: jobService,
		Logger: integrationLogger{}, Metrics: metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := notificationWorker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if notificationPublisher.calls != 1 {
		t.Fatalf("notification publishes = %d, want 1", notificationPublisher.calls)
	}

	var statusRevision int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT status_revision FROM external_agent_job_notifications WHERE job_id = ?`, job.ID).Scan(&statusRevision); err != nil {
		t.Fatal(err)
	}
	activationID := domain.ExternalAgentJobActivationID(job.ID, statusRevision, domain.JobNotificationTerminal)
	activation, err := jobs.GetActivation(t.Context(), activationID)
	if err != nil || activation == nil {
		t.Fatalf("activation after publication = %#v, err=%v", activation, err)
	}
	if activation.State != domain.ActivationPending || activation.ConversationKey != key {
		t.Fatalf("activation after publication = %#v", activation)
	}

	rootModel := &activationRootModel{}
	toolFactory := toolfactory.New(store, nil, nil, nil).WithExternalAgentJobs(jobService)
	sessionService := adaptersqlite.NewAdkSessionService(store)
	runtime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{
		AgentName: "root_agent", Model: rootModel, SessionService: sessionService,
		ToolFactory: toolFactory, ProviderFamily: domain.ProviderFamilyOpenAICompatible,
	})
	if err != nil {
		t.Fatal(err)
	}
	responsePublisher := &jobActivationResponsePublisher{}
	bot, err := botusecase.New(botusecase.Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{job.Actor}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 20, MaxChars: 20_000},
		RetainMessages: 50, MaxConcurrentCalls: 1, ModelTimeout: time.Minute,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}, botusecase.Dependencies{
		Store: store, Runtime: runtime, ActivationStore: jobs, Publisher: responsePublisher,
		Logger: integrationLogger{}, Exchange: store, ModelCalls: modelcalllimiter.New(1),
		SanitizeContent: func(value string) string { return value },
	})
	if err != nil {
		t.Fatal(err)
	}
	activationWorker, err := externalagent.NewActivationWorker(externalagent.ActivationConfig{
		PollInterval: time.Millisecond, LeaseTTL: time.Minute, StuckThreshold: time.Minute,
	}, externalagent.ActivationDependencies{
		Store: jobs, Handler: bot, Logger: integrationLogger{}, Metrics: metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := activationWorker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}

	completed, err := jobs.GetActivation(t.Context(), activationID)
	if err != nil || completed == nil {
		t.Fatalf("completed activation = %#v, err=%v", completed, err)
	}
	if completed.State != domain.ActivationCompleted {
		t.Fatalf("activation state = %q, want completed", completed.State)
	}
	if len(responsePublisher.calls) != 1 || responsePublisher.calls[0].target.ChannelID != "D12345678" || responsePublisher.calls[0].target.ThreadTS != "1710000000.000001" || responsePublisher.calls[0].text != "root synthesis" {
		t.Fatalf("root delivery = %#v", responsePublisher.calls)
	}

	messages, err := store.RecentMessages(t.Context(), key, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Source != domain.MessageSourceJobCompletion || messages[0].ExternalTS != activationID || messages[1].Role != domain.RoleAssistant || messages[1].Source != domain.MessageSourceAssistant {
		t.Fatalf("activation transcript = %#v", messages)
	}
	var memoryOutbox int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM memory_outbox`).Scan(&memoryOutbox); err != nil {
		t.Fatal(err)
	}
	if memoryOutbox != 0 {
		t.Fatalf("activation created memory work = %d", memoryOutbox)
	}

	loaded, err := sessionService.Get(t.Context(), &session.GetRequest{
		AppName: "local-agent", UserID: "local_user", SessionID: "adk:" + string(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.Session == nil || loaded.Session.Events().Len() != 2 {
		t.Fatalf("durable ADK session = %#v", loaded)
	}
	for index := 0; index < loaded.Session.Events().Len(); index++ {
		metadata := loaded.Session.Events().At(index).CustomMetadata
		if metadata[port.AgentTurnOriginMetadataKey] != string(port.AgentTurnOriginJobCompletion) || metadata[port.AgentTurnActivationIDMetadataKey] != activationID {
			t.Fatalf("event %d origin metadata = %#v", index, metadata)
		}
	}
	modelRequest, modelContext, contextOK, calls := rootModel.snapshot()
	if !contextOK || modelContext.Origin.Kind != port.AgentTurnOriginJobCompletion || modelContext.Origin.Actor != job.Actor || modelContext.Origin.ActivationID != activationID || calls != 1 {
		t.Fatalf("root model context = %#v, present=%t, calls=%d", modelContext, contextOK, calls)
	}
	if modelRequest == nil || len(modelRequest.Tools) != 2 {
		t.Fatalf("activation model tools = %#v, want job_status and read_job_result_chunk", modelRequest)
	}
	if _, ok := modelRequest.Tools["job_status"]; !ok {
		t.Fatalf("job_status tool missing: %#v", modelRequest.Tools)
	}
	if _, ok := modelRequest.Tools["read_job_result_chunk"]; !ok {
		t.Fatalf("read_job_result_chunk tool missing: %#v", modelRequest.Tools)
	}

	health, err := activationWorker.SnapshotHealth(t.Context(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if health.Processed != 1 || health.Completed != 1 || health.Pending != 0 || health.CompletionUnknown != 0 || health.Stuck != 0 {
		t.Fatalf("activation health = %#v", health)
	}
	assertMetricAtLeast(t, metrics.Snapshot(), domain.MetricExternalAgentActivationClaimTotal, 1)
	assertMetricAtLeast(t, metrics.Snapshot(), domain.MetricExternalAgentActivationTotal, 1)
}

func TestActivationWorkerShutdownDrainsCurrentActivation(t *testing.T) {
	store := &drainActivationStore{activation: domain.ExternalAgentJobActivation{
		ActivationID: "activation-drain", JobID: "job-drain", Kind: domain.JobNotificationTerminal,
		State: domain.ActivationPending,
	}}
	handler := &drainActivationHandler{store: store, started: make(chan struct{}), release: make(chan struct{})}
	worker, err := externalagent.NewActivationWorker(externalagent.ActivationConfig{
		PollInterval: time.Millisecond, LeaseTTL: time.Minute,
	}, externalagent.ActivationDependencies{Store: store, Handler: handler, Logger: integrationLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go worker.Run(ctx)
	select {
	case <-handler.started:
	case <-time.After(time.Second):
		t.Fatal("activation worker did not claim the current activation")
	}
	worker.StopAdmission()
	close(handler.release)
	waitCtx, waitCancel := context.WithTimeout(t.Context(), time.Second)
	defer waitCancel()
	if err := worker.WaitStopped(waitCtx); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	claims, state := store.claims, store.activation.State
	store.mu.Unlock()
	if claims != 1 || state != domain.ActivationFailed {
		t.Fatalf("shutdown drain claims=%d state=%q", claims, state)
	}
}

type activationRootModel struct {
	mu        sync.Mutex
	request   *model.LLMRequest
	context   port.AgentTurnContext
	contextOK bool
	calls     int
}

func (*activationRootModel) Name() string { return "activation-root-model" }

func (m *activationRootModel) GenerateContent(ctx context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		turnContext, contextOK := port.AgentTurnContextFromContext(ctx)
		m.mu.Lock()
		m.request = request
		m.context = turnContext
		m.contextOK = contextOK
		m.calls++
		m.mu.Unlock()
		yield(&model.LLMResponse{Content: genai.NewContentFromText("root synthesis", genai.RoleModel), TurnComplete: true}, nil)
	}
}

func (m *activationRootModel) snapshot() (*model.LLMRequest, port.AgentTurnContext, bool, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.request, m.context, m.contextOK, m.calls
}

type activationJobRuntime struct{}

func (*activationJobRuntime) Run(context.Context, domain.ExternalAgentJob) (domain.AcpInvocationResult, error) {
	return domain.AcpInvocationResult{}, errors.New("ACP runtime must not run during host completion")
}

type jobActivationNotificationPublisher struct {
	calls int
}

func (p *jobActivationNotificationPublisher) Publish(context.Context, domain.ExternalAgentJobNotification) (port.PublishedResponse, error) {
	p.calls++
	return port.PublishedResponse{LastMessageTS: "1710000000.000002"}, nil
}

func (*jobActivationNotificationPublisher) Reconcile(context.Context, domain.ExternalAgentJobNotification) (string, bool, error) {
	return "", false, nil
}

type jobActivationResponsePublisher struct {
	calls []jobActivationPublishedCall
}

type jobActivationPublishedCall struct {
	target domain.ReplyTarget
	text   string
}

func (p *jobActivationResponsePublisher) Publish(_ context.Context, target domain.ReplyTarget, text string) (port.PublishedResponse, error) {
	p.calls = append(p.calls, jobActivationPublishedCall{target: target, text: text})
	return port.PublishedResponse{LastMessageTS: "1710000000.000003"}, nil
}

func assertMetricAtLeast(t *testing.T, samples []port.MetricSample, name string, value float64) {
	t.Helper()
	for _, sample := range samples {
		if sample.Name == name && sample.Value >= value {
			return
		}
	}
	t.Fatalf("metric %q not found at value >= %v: %#v", name, value, samples)
}

type drainActivationStore struct {
	mu         sync.Mutex
	activation domain.ExternalAgentJobActivation
	claims     int
}

func (s *drainActivationStore) ClaimNextActivation(context.Context, time.Time, string, time.Duration) (*domain.ExternalAgentJobActivation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	if s.claims > 1 {
		return nil, nil
	}
	s.activation.State = domain.ActivationProcessing
	s.activation.Attempt = 1
	s.activation.LeaseOwner = "drain-worker"
	return copyActivation(s.activation), nil
}

func (s *drainActivationStore) RetryActivation(context.Context, *domain.ExternalAgentJobActivation, string, time.Time, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activation.State = domain.ActivationPending
	return nil
}

func (s *drainActivationStore) GetActivation(context.Context, string) (*domain.ExternalAgentJobActivation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyActivation(s.activation), nil
}

func (s *drainActivationStore) MarkActivationModelStarted(context.Context, *domain.ExternalAgentJobActivation, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activation.State = domain.ActivationModelStarted
	return nil
}

func (s *drainActivationStore) PrepareActivationResponse(context.Context, *domain.ExternalAgentJobActivation, string, string, string, string, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activation.State = domain.ActivationResponsePrepared
	return nil
}

func (s *drainActivationStore) CompleteActivation(context.Context, *domain.ExternalAgentJobActivation, string, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activation.State = domain.ActivationCompleted
	return nil
}

func (s *drainActivationStore) FailActivation(context.Context, *domain.ExternalAgentJobActivation, string, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activation.State = domain.ActivationFailed
	return nil
}

func (s *drainActivationStore) MarkActivationCompletionUnknown(context.Context, *domain.ExternalAgentJobActivation, string, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activation.State = domain.ActivationCompletionUnknown
	return nil
}

func copyActivation(value domain.ExternalAgentJobActivation) *domain.ExternalAgentJobActivation {
	copy := value
	return &copy
}

type drainActivationHandler struct {
	store   *drainActivationStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *drainActivationHandler) HandleJobCompletion(context.Context, domain.ExternalAgentJobActivation) error {
	h.once.Do(func() { close(h.started) })
	<-h.release
	h.store.mu.Lock()
	h.store.activation.State = domain.ActivationFailed
	h.store.mu.Unlock()
	return nil
}

func (*drainActivationHandler) ReconcileJobCompletion(context.Context, domain.ExternalAgentJobActivation) error {
	return nil
}

var _ model.LLM = (*activationRootModel)(nil)
var _ port.ExternalAgentJobRuntime = (*activationJobRuntime)(nil)
var _ port.JobNotificationPublisher = (*jobActivationNotificationPublisher)(nil)
var _ port.ResponsePublisher = (*jobActivationResponsePublisher)(nil)
var _ port.ExternalAgentJobActivationStore = (*drainActivationStore)(nil)
var _ port.ExternalAgentJobCompletionHandler = (*drainActivationHandler)(nil)
