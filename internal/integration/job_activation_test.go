package integration_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
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

// TestForegroundJobSingleRootResponseEndToEnd reproduces the user-observed
// foreground contract (FR-01, FR-04, FR-08): a foreground ACP job executed
// through the durable facade returns one original tool response and the
// original root turn answers exactly once, while publishing the terminal
// notification creates zero activations, zero activation model calls and zero
// additional root responses. The persisted identity is computed over the
// post-redaction, post-sanitization text and verified by chunk reads; all
// zero-work assertions survive a worker restart over the same database. The
// test fails when MarkNotificationPublished can create an activation for a
// foreground job.
func TestForegroundJobSingleRootResponseEndToEnd(t *testing.T) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "foreground.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := adaptersqlite.NewExternalAgentJobStore(store)

	const actor = "U12345678"
	key := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1710000000.000001")

	// The raw provider text is transformed twice before persistence: the host
	// redactor shortens the secret and domain.SanitizeResultText rewrites '<'
	// and strips control characters. The persisted identity must therefore
	// differ from the raw runtime text (FR-03, FR-13).
	const rawText = "prefix \x01<provider output>\x02 secret-token-123 suffix"
	redact := func(value string) string { return strings.ReplaceAll(value, "secret-token-123", "REDACTED") }
	sanitizedText, err := domain.SanitizeResultText(redact(rawText))
	if err != nil {
		t.Fatal(err)
	}
	if sanitizedText == rawText || strings.Contains(sanitizedText, "<") || strings.ContainsAny(sanitizedText, "\x01\x02") {
		t.Fatalf("fixture text must change under redaction/sanitization: %q", sanitizedText)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(sanitizedText)))

	acp := &foregroundFakeACP{raw: rawText}
	service, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute,
		PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1,
	}, externalagent.Dependencies{
		Store: jobs, Runtime: &foregroundDispatcherRuntime{acp: acp, redact: redact},
		MaxResultBytes: 1 << 20, MaxResultChunkBytes: 7, Logger: integrationLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Durable facade: root tool calls without JobID go through StartAndWait;
	// worker dispatches carry JobID and reach the direct ACP runtime.
	facade := &foregroundFacadeRuntime{direct: acp, jobs: service}

	workerCtx, stopWorker := context.WithCancel(context.Background())
	t.Cleanup(stopWorker)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		service.Run(workerCtx)
	}()
	t.Cleanup(func() {
		stopWorker()
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			t.Error("durable job worker did not stop")
		}
	})

	rootModel := &foregroundRootModel{}
	acpTool := newForegroundACPTool(t, facade, key, actor)
	sessionService := adaptersqlite.NewAdkSessionService(store)
	runtime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{
		AgentName: "root_agent", Model: rootModel, SessionService: sessionService,
		StaticTools: []tool.Tool{acpTool}, ProviderFamily: domain.ProviderFamilyOpenAICompatible,
	})
	if err != nil {
		t.Fatal(err)
	}
	responsePublisher := &jobActivationResponsePublisher{}
	confirmationStore := adaptersqlite.NewConfirmationStore(store)
	bot, err := botusecase.New(botusecase.Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{actor}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 20, MaxChars: 20_000},
		RetainMessages: 50, MaxConcurrentCalls: 1, ModelTimeout: time.Minute,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}, botusecase.Dependencies{
		Store: store, Runtime: runtime, ActivationStore: jobs, Publisher: responsePublisher,
		Logger: integrationLogger{}, Exchange: store, ModelCalls: modelcalllimiter.New(1),
		ConfirmationStore: confirmationStore,
		SanitizeContent:   func(value string) string { return value },
	})
	if err != nil {
		t.Fatal(err)
	}
	metrics := metricsadapter.NewRecorder()
	notificationPublisher := &jobActivationNotificationPublisher{}
	notificationWorker, err := externalagent.NewNotificationWorker(externalagent.NotificationConfig{
		PollInterval: time.Millisecond, LeaseTTL: time.Minute,
	}, externalagent.NotificationDependencies{
		Store: jobs, Publisher: notificationPublisher, HostCompleter: service,
		Logger: integrationLogger{}, Metrics: metrics,
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

	// Steps 1-2: the original root turn requests the foreground task and
	// approves the confirmation. The durable facade runs the job; the model
	// receives exactly one original tool response and answers exactly once.
	outcome, err := bot.Handle(t.Context(), foregroundThreadedDMInvocation("Ev-fg-01", "run the foreground task", key))
	if err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("first root turn outcome=%q err=%v", outcome, err)
	}
	promptCalls := responsePublisher.snapshot()
	if len(promptCalls) != 1 {
		t.Fatalf("root publishes before approval = %d, want 1 confirmation prompt", len(promptCalls))
	}
	wrapperCallID := extractWrapperCallID(promptCalls[0].text)
	if wrapperCallID == "" {
		t.Fatalf("confirmation wrapper call ID missing: %q", promptCalls[0].text)
	}
	if calls, _ := acp.callStats(); calls != 0 {
		t.Fatalf("ACP executed before approval: %d calls", calls)
	}
	approval := foregroundThreadedDMInvocation("Ev-fg-02", "approve "+wrapperCallID, key)
	approval.EventTS = "1710000001.000002"
	outcome, err = bot.Handle(t.Context(), approval)
	if err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("approval turn outcome=%q err=%v", outcome, err)
	}

	toolResponses := rootModel.toolResponsesSnapshot()
	if len(toolResponses) != 1 {
		t.Fatalf("original tool responses = %d, want 1: %#v", len(toolResponses), toolResponses)
	}
	if toolResponses[0] != sanitizedText {
		t.Fatalf("original tool response = %q, want sanitized identity %q", toolResponses[0], sanitizedText)
	}
	if calls, _ := rootModel.snapshot(); calls != 2 {
		t.Fatalf("root model calls = %d, want 2 (one function call + one final response)", calls)
	}
	rootCalls := responsePublisher.snapshot()
	if len(rootCalls) != 2 || rootCalls[1].text != "root synthesis" {
		t.Fatalf("root deliveries = %#v, want [confirmation prompt, single root response]", rootCalls)
	}
	if calls, jobID := acp.callStats(); calls != 1 || jobID == "" {
		t.Fatalf("ACP executions = %d with jobID %q, want exactly 1 worker dispatch", calls, jobID)
	}

	// The persisted row carries the post-transform identity.
	var jobID, summary, persistedSHA string
	var persistedBytes int64
	if err := store.DB().QueryRowContext(t.Context(), `SELECT job_id, result_summary, result_sha256, result_bytes FROM external_agent_jobs WHERE mode = 'foreground'`).Scan(&jobID, &summary, &persistedSHA, &persistedBytes); err != nil {
		t.Fatal(err)
	}
	if summary != sanitizedText || persistedSHA != digest || persistedBytes != int64(len(sanitizedText)) {
		t.Fatalf("persisted identity = %q/%s/%d, want %q/%s/%d", summary, persistedSHA, persistedBytes, sanitizedText, digest, len(sanitizedText))
	}

	// Step 3: process the terminal notification; zero activation work follows.
	if err := notificationWorker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if notificationPublisher.calls != 1 {
		t.Fatalf("terminal notification publishes = %d, want 1", notificationPublisher.calls)
	}
	var activations int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM external_agent_job_activations WHERE job_id = ?`, jobID).Scan(&activations); err != nil {
		t.Fatal(err)
	}
	if activations != 0 {
		t.Fatalf("activation rows after terminal notification = %d, want 0", activations)
	}
	if err := activationWorker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertForegroundZeroAdditionalWork(t, rootModel, responsePublisher, store, jobID)

	// Step 4: read the persisted result by chunks and verify bytes, digest and
	// final text against the verified identity (FR-04).
	reconstructed := readForegroundResultChunks(t, service, jobID, actor, key, digest)
	if reconstructed != sanitizedText {
		t.Fatalf("chunk reconstruction = %q, want %q", reconstructed, sanitizedText)
	}
	if len([]byte(reconstructed)) != int(persistedBytes) {
		t.Fatalf("chunk byte count = %d, want %d", len([]byte(reconstructed)), persistedBytes)
	}
	if result, err := service.ReadResult(t.Context(), jobID, actor, key); err != nil || result.Text != sanitizedText || result.ContentSHA256 != digest || result.ContentBytes != int64(len(sanitizedText)) {
		t.Fatalf("verified full read = %#v err=%v", result, err)
	}

	// Step 5: restart the workers over the same database; no work reappears.
	restartedService, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute,
		PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1,
	}, externalagent.Dependencies{
		Store: jobs, Runtime: &foregroundDispatcherRuntime{acp: acp, redact: redact},
		MaxResultBytes: 1 << 20, MaxResultChunkBytes: 7, Logger: integrationLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	restartedNotification, err := externalagent.NewNotificationWorker(externalagent.NotificationConfig{
		PollInterval: time.Millisecond, LeaseTTL: time.Minute,
	}, externalagent.NotificationDependencies{
		Store: jobs, Publisher: notificationPublisher, HostCompleter: restartedService,
		Logger: integrationLogger{}, Metrics: metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	restartedActivation, err := externalagent.NewActivationWorker(externalagent.ActivationConfig{
		PollInterval: time.Millisecond, LeaseTTL: time.Minute, StuckThreshold: time.Minute,
	}, externalagent.ActivationDependencies{
		Store: jobs, Handler: bot, Logger: integrationLogger{}, Metrics: metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restartedNotification.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := restartedActivation.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertForegroundZeroAdditionalWork(t, rootModel, responsePublisher, store, jobID)
	if notificationPublisher.calls != 1 {
		t.Fatalf("notification publishes after restart = %d, want 1", notificationPublisher.calls)
	}
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

func (p *jobActivationResponsePublisher) snapshot() []jobActivationPublishedCall {
	return append([]jobActivationPublishedCall(nil), p.calls...)
}

func (p *jobActivationResponsePublisher) count() int {
	return len(p.calls)
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

// foregroundFakeACP is the direct ACP runtime behind the durable facade. It
// returns raw provider text that the host redactor and SanitizeResultText
// transform, so the persisted identity necessarily differs from the raw text.
type foregroundFakeACP struct {
	mu        sync.Mutex
	raw       string
	calls     int
	lastJobID string
}

func (r *foregroundFakeACP) Run(_ context.Context, request domain.AcpInvocationRequest) (domain.AcpInvocationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastJobID = request.JobID
	return domain.AcpInvocationResult{Text: r.raw}, nil
}

func (*foregroundFakeACP) Probe(context.Context, string, []string, []domain.ACPConfigOption) error {
	return nil
}

func (*foregroundFakeACP) Describe(context.Context) (domain.ACPInitResult, error) {
	return domain.ACPInitResult{}, nil
}

func (r *foregroundFakeACP) callStats() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.lastJobID
}

// foregroundDispatcherRuntime mirrors the acpJobDispatcher foreground branch:
// the durable worker runs the job through the direct ACP runtime and computes
// bytes and SHA-256 only over the final post-redaction, post-sanitization
// text. It is the test twin of normalizeForegroundResult (FR-03).
type foregroundDispatcherRuntime struct {
	acp    port.ExternalAgentRuntime
	redact func(string) string
}

func (d *foregroundDispatcherRuntime) Run(ctx context.Context, job domain.ExternalAgentJob) (domain.AcpInvocationResult, error) {
	if job.Mode != domain.JobForeground {
		return domain.AcpInvocationResult{}, errors.New("foreground dispatcher runtime received a non-foreground job")
	}
	result, err := d.acp.Run(ctx, domain.AcpInvocationRequest{
		JobID: job.ID, PrimaryProject: job.PrimaryProject,
		ProfileName: job.Profile, ProviderName: job.Provider, Task: job.Task,
		Timeout: time.Until(job.TimeoutAt), Actor: job.Actor, TeamID: job.TeamID,
		ConversationKey: job.ConversationKey,
	})
	if err != nil {
		return result, err
	}
	text := result.Text
	if d.redact != nil {
		text = d.redact(text)
	}
	text, err = domain.SanitizeResultText(text)
	if err != nil {
		return domain.AcpInvocationResult{}, err
	}
	result.Text = text
	result.ResultBytes = int64(len([]byte(text)))
	digest := sha256.Sum256([]byte(text))
	result.ResultSHA256 = fmt.Sprintf("%x", digest)
	return result, nil
}

var _ port.ExternalAgentJobRuntime = (*foregroundDispatcherRuntime)(nil)

// foregroundFacadeRuntime is the test twin of the durable foreground facade:
// a root tool call without JobID becomes a durable foreground job waited on
// synchronously, while worker dispatches carry JobID and reach the direct ACP
// runtime. The facade never runs the direct runtime for the root path.
type foregroundFacadeRuntime struct {
	direct port.ExternalAgentRuntime
	jobs   foregroundSynchronousJobRunner
}

type foregroundSynchronousJobRunner interface {
	StartAndWait(context.Context, domain.ExternalAgentJobRequest) (domain.AcpInvocationResult, error)
}

func (r *foregroundFacadeRuntime) Run(ctx context.Context, request domain.AcpInvocationRequest) (domain.AcpInvocationResult, error) {
	if request.JobID != "" || r.jobs == nil || request.Actor == "" || request.ConversationKey == "" {
		return r.direct.Run(ctx, request)
	}
	provider := request.ProviderName
	if provider == "" {
		provider, _, _ = strings.Cut(request.ProfileName, "/")
	}
	return r.jobs.StartAndWait(ctx, domain.ExternalAgentJobRequest{
		Provider: provider, Profile: request.ProfileName,
		PrimaryProject: request.PrimaryProject, AdditionalProjects: request.AdditionalProjects,
		RegistryRevision: request.RegistryRevision, Task: request.Task, Mode: domain.JobForeground,
		PermissionOptionKind: request.PermissionOptionKind, Timeout: request.Timeout,
		PrimaryPath: request.PrimaryPath, AdditionalPaths: request.AdditionalPaths,
		WrapperCallID: request.OriginalCallID, OriginalCallID: request.OriginalCallID,
		Actor: request.Actor, TeamID: request.TeamID, ConversationKey: request.ConversationKey,
	})
}

func (r *foregroundFacadeRuntime) Probe(ctx context.Context, primaryPath string, additionalPaths []string, options []domain.ACPConfigOption) error {
	return r.direct.Probe(ctx, primaryPath, additionalPaths, options)
}

func (r *foregroundFacadeRuntime) Describe(ctx context.Context) (domain.ACPInitResult, error) {
	return r.direct.Describe(ctx)
}

var _ port.ExternalAgentRuntime = (*foregroundFacadeRuntime)(nil)

// foregroundRootModel scripts the original root turn: one function call to
// the foreground ACP tool, then one final response. Every model call is
// counted, so any activation turn would be visible as an additional call, and
// the tool response delivered to the model is captured verbatim.
type foregroundRootModel struct {
	mu            sync.Mutex
	calls         int
	toolResponses []string
}

func (*foregroundRootModel) Name() string { return "foreground-root-model" }

func (m *foregroundRootModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		m.calls++
		call := m.calls
		for _, content := range request.Contents {
			for _, part := range content.Parts {
				if part == nil || part.FunctionResponse == nil {
					continue
				}
				if text, ok := part.FunctionResponse.Response["result"].(string); ok {
					m.toolResponses = append(m.toolResponses, text)
				}
			}
		}
		m.mu.Unlock()
		if call == 1 {
			yield(&model.LLMResponse{Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
					ID: "call_foreground_001", Name: "foreground_acp",
					Args: map[string]any{"project": "workspace", "task": "generate the durable foreground result"},
				}}},
			}, TurnComplete: true}, nil)
			return
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText("root synthesis", genai.RoleModel), TurnComplete: true}, nil)
	}
}

func (m *foregroundRootModel) snapshot() (int, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls, append([]string(nil), m.toolResponses...)
}

func (m *foregroundRootModel) toolResponsesSnapshot() []string {
	_, responses := m.snapshot()
	return responses
}

var _ model.LLM = (*foregroundRootModel)(nil)

type foregroundACPToolArgs struct {
	Project string `json:"project" jsonschema:"registered project name"`
	Task    string `json:"task" jsonschema:"complete bounded task"`
}

type foregroundACPToolResult struct {
	Result string `json:"result"`
}

// newForegroundACPTool builds the confirmable root tool that invokes the
// durable facade, mirroring the foreground branch of the ACP agent tool.
func newForegroundACPTool(t *testing.T, runtime port.ExternalAgentRuntime, key domain.ConversationKey, actor string) tool.Tool {
	t.Helper()
	projectRoot := t.TempDir()
	created, err := functiontool.New(functiontool.Config{
		Name:                "foreground_acp",
		Description:         "Runs a foreground durable ACP task in this conversation.",
		RequireConfirmation: true,
	}, func(_ agent.Context, args foregroundACPToolArgs) (foregroundACPToolResult, error) {
		if strings.TrimSpace(args.Project) == "" || strings.TrimSpace(args.Task) == "" {
			return foregroundACPToolResult{}, errors.New("foreground ACP project and task are required")
		}
		result, err := runtime.Run(context.Background(), domain.AcpInvocationRequest{
			PrimaryProject: args.Project, PrimaryPath: projectRoot,
			ProfileName: "opencode/build", ProviderName: "opencode", RegistryRevision: "r1",
			Task: args.Task, Actor: actor, TeamID: "T12345678", ConversationKey: key,
		})
		if err != nil {
			return foregroundACPToolResult{}, err
		}
		return foregroundACPToolResult{Result: result.Text}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func foregroundThreadedDMInvocation(eventID, text string, key domain.ConversationKey) domain.Invocation {
	return domain.Invocation{
		EventID: eventID, EventType: "message.im", TeamID: "T12345678",
		ChannelID: "D12345678", ChannelKind: domain.ChannelDM, UserID: "U12345678",
		EventTS: "1710000000.000002", ThreadTS: "1710000000.000001", Text: text,
		Trigger: domain.TriggerDirectMessage, ThreadedDM: true,
	}
}

// assertForegroundZeroAdditionalWork waits a short absence window and then
// asserts the root model, the root publisher and the activation rows all still
// describe exactly the original single root response.
func assertForegroundZeroAdditionalWork(t *testing.T, model *foregroundRootModel, publisher *jobActivationResponsePublisher, store *adaptersqlite.Store, jobID string) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if calls, _ := model.snapshot(); calls > 2 {
			t.Fatalf("root model calls grew to %d after terminal notification", calls)
		}
		if calls := publisher.count(); calls > 2 {
			t.Fatalf("root publishes grew to %d after terminal notification", calls)
		}
		time.Sleep(25 * time.Millisecond)
	}
	calls, _ := model.snapshot()
	if calls != 2 {
		t.Fatalf("root model calls = %d, want 2 (zero activation model calls)", calls)
	}
	if got := publisher.count(); got != 2 {
		t.Fatalf("root publishes = %d, want 2 (confirmation prompt + single root response)", got)
	}
	var activations int
	if err := store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM external_agent_job_activations WHERE job_id = ?`, jobID).Scan(&activations); err != nil {
		t.Fatal(err)
	}
	if activations != 0 {
		t.Fatalf("activation rows = %d, want 0", activations)
	}
}

// readForegroundResultChunks reads the persisted result in bounded verified
// chunks until EOF and returns the reconstructed final text.
func readForegroundResultChunks(t *testing.T, service *externalagent.Service, jobID, actor string, key domain.ConversationKey, digest string) string {
	t.Helper()
	var builder strings.Builder
	offset := int64(0)
	for {
		chunk, err := service.ReadResultChunk(t.Context(), jobID, actor, key, offset, 7)
		if err != nil {
			t.Fatal(err)
		}
		if chunk.SHA256 != digest {
			t.Fatalf("chunk digest = %q, want %q", chunk.SHA256, digest)
		}
		if chunk.NextOffsetBytes <= offset {
			t.Fatalf("chunk did not advance: %#v", chunk)
		}
		builder.WriteString(chunk.Content)
		offset = chunk.NextOffsetBytes
		if chunk.EOF {
			break
		}
	}
	return builder.String()
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
