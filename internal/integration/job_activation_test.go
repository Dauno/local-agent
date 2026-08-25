package integration_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
	workstreamusecase "github.com/Dauno/slack-local-agent/internal/usecase/workstream"
)

// TestJobCompletionActivationEndToEnd proves the detached root awareness
// contract (FR-02, FR-12): a detached job executes exactly once through the
// durable ACP runtime, its multibyte result is consumed by the scripted root
// model in bounded verified UTF-8 chunks (per-chunk SHA-256 against the
// status identity and a recomputed total digest) and only then answered with
// a single root synthesis. Exactly one notification, one activation, one ACP
// execution and one root response are asserted. The test fails when the chunk
// reader is broken even though the read_job_result_chunk tool is registered,
// because every model-side verification failure fails the activation turn.
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
	job.WorkstreamID = "ws-activation-e2e"
	job.TaskID = "task-activation-e2e"
	job.AdmissionRevision = 0
	jobs := adaptersqlite.NewExternalAgentJobStore(store)
	workstreams, err := workstreamusecase.New(workstreamusecase.Config{
		Enabled: true, AllowedProjects: map[string]struct{}{job.PrimaryProject: {}},
	}, workstreamusecase.Dependencies{Store: adaptersqlite.NewWorkstreamStore(store)})
	if err != nil {
		t.Fatal(err)
	}
	// The binding must be reachable through public routes: a human creates and
	// activates the workstream, proposes the task, and starts it for execution.
	// The host generates the execution identity.
	binding := port.WorkstreamBinding{Actor: job.Actor, ConversationKey: job.ConversationKey, Project: job.PrimaryProject}
	if _, err := workstreams.CreateHuman(t.Context(), binding, job.WorkstreamID, "activation objective", "slack-human:ws-activation-create"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workstreams.ApplyHuman(t.Context(), binding, domain.WorkstreamTransition{
		WorkstreamID: job.WorkstreamID, ExpectedRevision: 0, Project: job.PrimaryProject,
		Action: domain.WorkstreamActionActivateWorkstream, SourceID: "slack-human:ws-activation-activate",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workstreams.ApplyHuman(t.Context(), binding, domain.WorkstreamTransition{
		WorkstreamID: job.WorkstreamID, ExpectedRevision: 1, Project: job.PrimaryProject,
		Action: domain.WorkstreamActionProposeTask, SourceID: "slack-human:ws-activation-propose",
		Task: &domain.WorkstreamTask{ID: job.TaskID, Project: job.PrimaryProject, Description: job.Task, Status: domain.TaskProposed},
	}); err != nil {
		t.Fatal(err)
	}
	_, started, err := workstreams.ApplyHuman(t.Context(), binding, domain.WorkstreamTransition{
		WorkstreamID: job.WorkstreamID, ExpectedRevision: 2, Project: job.PrimaryProject,
		Action: domain.WorkstreamActionStartTask, TaskID: job.TaskID, SourceID: "slack-human:ws-activation-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range started.Tasks {
		if task.ID == job.TaskID {
			job.ExecutionIdentity = task.ExecutionIdentity
		}
	}
	if job.ExecutionIdentity == "" {
		t.Fatal("started task has no host execution identity")
	}
	job.AdmissionRevision = 3
	job.RequestSHA256 = domain.ExternalAgentJobRequestDigest(domain.ExternalAgentJobRequest{
		Provider: job.Provider, Profile: job.Profile, PrimaryProject: job.PrimaryProject,
		RegistryRevision: job.RegistryRevision, Task: job.Task, Mode: job.Mode,
		Actor: job.Actor, TeamID: job.TeamID, ConversationKey: job.ConversationKey,
		WorkstreamID: job.WorkstreamID, TaskID: job.TaskID, ExecutionIdentity: job.ExecutionIdentity,
	})
	created, _, err := jobs.CreateIfAbsent(t.Context(), job)
	if err != nil || !created {
		t.Fatalf("create job = %v, err=%v", created, err)
	}

	// The provider returns multibyte UTF-8 content (accents, emoji, CJK,
	// symbols). The persisted identity is computed over the sanitized text,
	// so the root model must reconstruct and verify exactly that text through
	// the activation-scoped chunk reader.
	const rawResult = "café ☕ síntesis — áéíóú ñ ü 漢字 🔥 done ✓"
	expectedText, err := domain.SanitizeResultText(rawResult)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(expectedText) || len([]byte(expectedText)) <= 7 {
		t.Fatalf("fixture must be multibyte and exceed one 7-byte chunk: %q", expectedText)
	}
	expectedDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(expectedText)))

	acp := &detachedFakeExternalAgent{raw: rawResult}
	jobService, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute,
		PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1,
	}, externalagent.Dependencies{
		Store: jobs, Runtime: &detachedDispatcherRuntime{acp: acp},
		MaxResultBytes: 1 << 20, MaxResultChunkBytes: 7, Logger: integrationLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	t.Cleanup(stopWorker)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		jobService.Run(workerCtx)
	}()
	t.Cleanup(func() {
		stopWorker()
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			t.Error("durable job worker did not stop")
		}
	})
	waitForJobStatus(t, jobs, job.ID, domain.JobCompleted)
	if calls, jobID := acp.callStats(); calls != 1 || jobID != job.ID {
		t.Fatalf("detached ACP executions = %d with jobID %q, want exactly 1 with %q", calls, jobID, job.ID)
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
	if notificationPublisher.notification.CanonicalMarkdown != "OpenCode job `job_activation_e2e` completed. Root integration is pending." {
		t.Fatalf("terminal notification repeated result prose: %q", notificationPublisher.notification.CanonicalMarkdown)
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

	rootModel := newActivationRootModel(job.ID, expectedText, expectedDigest, int64(len([]byte(expectedText))), 7)
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
		Store: store, Runtime: runtime, ActivationStore: jobs, CompletionReader: jobService, Publisher: responsePublisher,
		Logger: integrationLogger{}, Exchange: store, ModelCalls: modelcalllimiter.New(1),
		SanitizeContent: func(value string) string { return value }, Workstreams: workstreams,
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
		if classified, ok := errors.AsType[*port.ActivationProcessError](err); ok {
			t.Fatalf("activation processing: %s: %v", classified.Code, classified.Err)
		}
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

	var foregroundActivations, detachedActivations int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM external_agent_job_activations a JOIN external_agent_jobs j ON j.job_id = a.job_id WHERE j.mode = 'foreground'`).Scan(&foregroundActivations); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM external_agent_job_activations a JOIN external_agent_jobs j ON j.job_id = a.job_id WHERE j.mode = 'detached'`).Scan(&detachedActivations); err != nil {
		t.Fatal(err)
	}
	if foregroundActivations != 0 || detachedActivations != 1 {
		t.Fatalf("activation rows = %d foreground, %d detached, want 0 and 1", foregroundActivations, detachedActivations)
	}

	messages, err := store.RecentMessages(t.Context(), key, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != domain.RoleAssistant || messages[0].Source != domain.MessageSourceAssistant {
		t.Fatalf("activation transcript = %#v", messages)
	}
	loaded, err := sessionService.Get(t.Context(), &session.GetRequest{
		AppName: "local-agent", UserID: "local_user", SessionID: "adk:activation:" + activationID,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, calls := rootModel.snapshot()
	chunkCalls, reconstructed, verified, failures := rootModel.chunkStats()
	if !verified {
		t.Fatalf("root model chunk verification failed: %#v", failures)
	}
	if chunkCalls != 0 || reconstructed != "" {
		t.Fatalf("root model used result readers: calls=%d reconstructed=%q", chunkCalls, reconstructed)
	}
	// One transient frame input and one final root response.
	expectedEvents := 2
	if loaded == nil || loaded.Session == nil || loaded.Session.Events().Len() != expectedEvents {
		t.Fatalf("durable ADK session = %#v, want %d events", loaded, expectedEvents)
	}
	finalEvent := loaded.Session.Events().At(expectedEvents - 1)
	if finalEvent == nil || finalEvent.Content == nil || len(finalEvent.Content.Parts) == 0 || finalEvent.Content.Parts[0].Text != "root synthesis" {
		t.Fatalf("final session event = %#v, want the root synthesis text", finalEvent)
	}
	for index := 0; index < loaded.Session.Events().Len(); index++ {
		metadata := loaded.Session.Events().At(index).CustomMetadata
		if metadata[port.AgentTurnOriginMetadataKey] != string(port.AgentTurnOriginJobCompletion) || metadata[port.AgentTurnActivationIDMetadataKey] != activationID {
			t.Fatalf("event %d origin metadata = %#v", index, metadata)
		}
	}
	modelRequest, modelContext, contextOK, _ := rootModel.snapshot()
	if !contextOK || modelContext.Origin.Kind != port.AgentTurnOriginJobCompletion || modelContext.Origin.Actor != job.Actor || modelContext.Origin.ActivationID != activationID || calls != 1 {
		t.Fatalf("root model context = %#v, present=%t, calls=%d (want 1)", modelContext, contextOK, calls)
	}
	if modelRequest == nil || len(modelRequest.Tools) != 0 {
		t.Fatalf("activation model tools = %#v, want none", modelRequest)
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

// TestJobCompletionProposalPathEndToEnd proves the text-only proposal
// contract (FR-09): a detached completion publishes the terminal marker and
// root's integrated response carries exactly one informational proposal.
// The proposal never mutates the durable workstream and never creates a
// confirmation; only a later explicit human `workstream-human` command goes
// through the trusted host path and commits the transition.
func TestJobCompletionProposalPathEndToEnd(t *testing.T) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "proposal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Add(-time.Minute)
	key := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1710000000.000001")
	job := integrationDetachedJob("job_proposal_e2e", now)
	job.ConversationKey = key
	job.OriginalCallID = "original-call-proposal-e2e"
	job.WorkstreamID = "ws-proposal-e2e"
	job.TaskID = "task-proposal-e2e"
	job.AdmissionRevision = 0
	jobs := adaptersqlite.NewExternalAgentJobStore(store)
	workstreams, err := workstreamusecase.New(workstreamusecase.Config{
		Enabled: true, AllowedProjects: map[string]struct{}{job.PrimaryProject: {}},
	}, workstreamusecase.Dependencies{Store: adaptersqlite.NewWorkstreamStore(store)})
	if err != nil {
		t.Fatal(err)
	}
	binding := port.WorkstreamBinding{Actor: job.Actor, ConversationKey: job.ConversationKey, Project: job.PrimaryProject}
	if _, err := workstreams.CreateHuman(t.Context(), binding, job.WorkstreamID, "proposal objective", "slack-human:ws-proposal-create"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workstreams.ApplyHuman(t.Context(), binding, domain.WorkstreamTransition{
		WorkstreamID: job.WorkstreamID, ExpectedRevision: 0, Project: job.PrimaryProject,
		Action: domain.WorkstreamActionActivateWorkstream, SourceID: "slack-human:ws-proposal-activate",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workstreams.ApplyHuman(t.Context(), binding, domain.WorkstreamTransition{
		WorkstreamID: job.WorkstreamID, ExpectedRevision: 1, Project: job.PrimaryProject,
		Action: domain.WorkstreamActionProposeTask, SourceID: "slack-human:ws-proposal-propose",
		Task: &domain.WorkstreamTask{ID: job.TaskID, Project: job.PrimaryProject, Description: job.Task, Status: domain.TaskProposed},
	}); err != nil {
		t.Fatal(err)
	}
	_, started, err := workstreams.ApplyHuman(t.Context(), binding, domain.WorkstreamTransition{
		WorkstreamID: job.WorkstreamID, ExpectedRevision: 2, Project: job.PrimaryProject,
		Action: domain.WorkstreamActionStartTask, TaskID: job.TaskID, SourceID: "slack-human:ws-proposal-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range started.Tasks {
		if task.ID == job.TaskID {
			job.ExecutionIdentity = task.ExecutionIdentity
		}
	}
	if job.ExecutionIdentity == "" {
		t.Fatal("started task has no host execution identity")
	}
	job.AdmissionRevision = 3
	job.RequestSHA256 = domain.ExternalAgentJobRequestDigest(domain.ExternalAgentJobRequest{
		Provider: job.Provider, Profile: job.Profile, PrimaryProject: job.PrimaryProject,
		RegistryRevision: job.RegistryRevision, Task: job.Task, Mode: job.Mode,
		Actor: job.Actor, TeamID: job.TeamID, ConversationKey: job.ConversationKey,
		WorkstreamID: job.WorkstreamID, TaskID: job.TaskID, ExecutionIdentity: job.ExecutionIdentity,
	})
	created, _, err := jobs.CreateIfAbsent(t.Context(), job)
	if err != nil || !created {
		t.Fatalf("create job = %v, err=%v", created, err)
	}

	const rawResult = "proposal-path verified result"
	expectedText, err := domain.SanitizeResultText(rawResult)
	if err != nil {
		t.Fatal(err)
	}
	expectedDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(expectedText)))

	acp := &detachedFakeExternalAgent{raw: rawResult}
	jobService, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute,
		PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1,
	}, externalagent.Dependencies{
		Store: jobs, Runtime: &detachedDispatcherRuntime{acp: acp},
		MaxResultBytes: 1 << 20, MaxResultChunkBytes: 7, Logger: integrationLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	t.Cleanup(stopWorker)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		jobService.Run(workerCtx)
	}()
	t.Cleanup(func() {
		stopWorker()
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			t.Error("durable job worker did not stop")
		}
	})
	waitForJobStatus(t, jobs, job.ID, domain.JobCompleted)

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
	if notificationPublisher.notification.CanonicalMarkdown != "OpenCode job `job_proposal_e2e` completed. Root integration is pending." {
		t.Fatalf("terminal notification repeated result prose: %q", notificationPublisher.notification.CanonicalMarkdown)
	}
	var statusRevision int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT status_revision FROM external_agent_job_notifications WHERE job_id = ?`, job.ID).Scan(&statusRevision); err != nil {
		t.Fatal(err)
	}
	activationID := domain.ExternalAgentJobActivationID(job.ID, statusRevision, domain.JobNotificationTerminal)
	activation, err := jobs.GetActivation(t.Context(), activationID)
	if err != nil || activation == nil || activation.State != domain.ActivationPending {
		t.Fatalf("activation after publication = %#v, err=%v", activation, err)
	}

	const proposal = "The inspection completed cleanly.\nProposal: add a verification task before closing the objective."
	rootModel := newActivationRootModel(job.ID, expectedText, expectedDigest, int64(len([]byte(expectedText))), 7)
	rootModel.response = proposal
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
		Store: store, Runtime: runtime, ActivationStore: jobs, CompletionReader: jobService, Publisher: responsePublisher,
		Logger: integrationLogger{}, Exchange: store, ModelCalls: modelcalllimiter.New(1),
		SanitizeContent: func(value string) string { return value }, Workstreams: workstreams,
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
		if classified, ok := errors.AsType[*port.ActivationProcessError](err); ok {
			t.Fatalf("activation processing: %s: %v", classified.Code, classified.Err)
		}
		t.Fatal(err)
	}

	completed, err := jobs.GetActivation(t.Context(), activationID)
	if err != nil || completed == nil || completed.State != domain.ActivationCompleted {
		t.Fatalf("completed activation = %#v, err=%v", completed, err)
	}
	if len(responsePublisher.calls) != 1 || responsePublisher.calls[0].text != proposal {
		t.Fatalf("proposal delivery = %#v", responsePublisher.calls)
	}

	// The informational proposal never mutated the durable workstream or
	// created a confirmation.
	var revision, taskCount, confirmationCount int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT revision FROM workstreams WHERE workstream_id = ?`, job.WorkstreamID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM workstream_tasks WHERE workstream_id = ?`, job.WorkstreamID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM tool_confirmation_deliveries`).Scan(&confirmationCount); err != nil {
		t.Fatal(err)
	}
	if revision != 3 || taskCount != 1 || confirmationCount != 0 {
		t.Fatalf("proposal mutated durable state: revision=%d tasks=%d confirmations=%d", revision, taskCount, confirmationCount)
	}

	// The later explicit human command owns validation and mutation. It carries
	// the source result identity of the completed execution so the dependent
	// task keeps its provenance bound as a required input.
	const sourceResultID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	humanCommand := `workstream-human {"project":"workspace","workstream_id":"ws-proposal-e2e","expected_revision":3,"action":"propose_task","task_id":"task-proposal-e2e-2","task_description":"verify the proposal integration","source_result_identity":"` + sourceResultID + `"}`
	humanInvocation := foregroundThreadedDMInvocation("Ev-proposal-human-01", humanCommand, key)
	if outcome, err := bot.Handle(t.Context(), humanInvocation); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("human command outcome=%q err=%v", outcome, err)
	}
	if calls := responsePublisher.snapshot(); len(calls) != 2 || !strings.Contains(calls[1].text, "applied human action `propose_task` at revision `4`") {
		t.Fatalf("human command publication = %#v", calls)
	}
	if err := store.DB().QueryRowContext(t.Context(), `SELECT revision FROM workstreams WHERE workstream_id = ?`, job.WorkstreamID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM workstream_tasks WHERE workstream_id = ?`, job.WorkstreamID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if revision != 4 || taskCount != 2 {
		t.Fatalf("human command did not commit through the trusted path: revision=%d tasks=%d", revision, taskCount)
	}
	var sourceInputs int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM workstream_task_inputs WHERE workstream_id = ? AND task_id = ? AND input_identity = ?`, job.WorkstreamID, "task-proposal-e2e-2", sourceResultID).Scan(&sourceInputs); err != nil {
		t.Fatal(err)
	}
	if sourceInputs != 1 {
		t.Fatalf("source result identity was not bound as a required input: %d", sourceInputs)
	}
	if _, _, _, calls := rootModel.snapshot(); calls != 1 {
		t.Fatalf("human command crossed the root model: calls=%d", calls)
	}
}

// TestJobCompletionUnavailableResultPublishesTerminalFallback proves the
// fail-closed completion UX: the terminal marker is followed by an activation
// whose oversized markdown result cannot be represented, so the host publishes
// one deterministic fallback update through the activation worker's fallback
// claim instead of leaving the conversation visibly open. Root is never
// invoked and the producing ACP job is never replayed.
func TestJobCompletionUnavailableResultPublishesTerminalFallback(t *testing.T) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "fallback.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Add(-time.Minute)
	key := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1710000000.000001")
	job := integrationDetachedJob("job_fallback_e2e", now)
	job.ConversationKey = key
	job.OriginalCallID = "original-call-fallback-e2e"
	job.WorkstreamID = "ws-fallback-e2e"
	job.TaskID = "task-fallback-e2e"
	job.AdmissionRevision = 0
	jobs := adaptersqlite.NewExternalAgentJobStore(store)
	workstreams, err := workstreamusecase.New(workstreamusecase.Config{
		Enabled: true, AllowedProjects: map[string]struct{}{job.PrimaryProject: {}},
	}, workstreamusecase.Dependencies{Store: adaptersqlite.NewWorkstreamStore(store)})
	if err != nil {
		t.Fatal(err)
	}
	binding := port.WorkstreamBinding{Actor: job.Actor, ConversationKey: job.ConversationKey, Project: job.PrimaryProject}
	if _, err := workstreams.CreateHuman(t.Context(), binding, job.WorkstreamID, "fallback objective", "slack-human:ws-fallback-create"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workstreams.ApplyHuman(t.Context(), binding, domain.WorkstreamTransition{
		WorkstreamID: job.WorkstreamID, ExpectedRevision: 0, Project: job.PrimaryProject,
		Action: domain.WorkstreamActionActivateWorkstream, SourceID: "slack-human:ws-fallback-activate",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workstreams.ApplyHuman(t.Context(), binding, domain.WorkstreamTransition{
		WorkstreamID: job.WorkstreamID, ExpectedRevision: 1, Project: job.PrimaryProject,
		Action: domain.WorkstreamActionProposeTask, SourceID: "slack-human:ws-fallback-propose",
		Task: &domain.WorkstreamTask{ID: job.TaskID, Project: job.PrimaryProject, Description: job.Task, Status: domain.TaskProposed},
	}); err != nil {
		t.Fatal(err)
	}
	_, started, err := workstreams.ApplyHuman(t.Context(), binding, domain.WorkstreamTransition{
		WorkstreamID: job.WorkstreamID, ExpectedRevision: 2, Project: job.PrimaryProject,
		Action: domain.WorkstreamActionStartTask, TaskID: job.TaskID, SourceID: "slack-human:ws-fallback-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range started.Tasks {
		if task.ID == job.TaskID {
			job.ExecutionIdentity = task.ExecutionIdentity
		}
	}
	job.AdmissionRevision = 3
	job.RequestSHA256 = domain.ExternalAgentJobRequestDigest(domain.ExternalAgentJobRequest{
		Provider: job.Provider, Profile: job.Profile, PrimaryProject: job.PrimaryProject,
		RegistryRevision: job.RegistryRevision, Task: job.Task, Mode: job.Mode,
		Actor: job.Actor, TeamID: job.TeamID, ConversationKey: job.ConversationKey,
		WorkstreamID: job.WorkstreamID, TaskID: job.TaskID, ExecutionIdentity: job.ExecutionIdentity,
	})
	created, _, err := jobs.CreateIfAbsent(t.Context(), job)
	if err != nil || !created {
		t.Fatalf("create job = %v, err=%v", created, err)
	}

	rawResult := strings.Repeat("x", domain.MaxActivationFrameRunes+1)
	acp := &detachedFakeExternalAgent{raw: rawResult}
	jobService, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute,
		PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1,
	}, externalagent.Dependencies{
		Store: jobs, Runtime: &detachedDispatcherRuntime{acp: acp},
		MaxResultBytes: 1 << 20, MaxResultChunkBytes: 7, Logger: integrationLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	t.Cleanup(stopWorker)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		jobService.Run(workerCtx)
	}()
	t.Cleanup(func() {
		stopWorker()
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			t.Error("durable job worker did not stop")
		}
	})
	waitForJobStatus(t, jobs, job.ID, domain.JobCompleted)
	if calls, _ := acp.callStats(); calls != 1 {
		t.Fatalf("ACP executions = %d, want exactly 1", calls)
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
	var statusRevision int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT status_revision FROM external_agent_job_notifications WHERE job_id = ?`, job.ID).Scan(&statusRevision); err != nil {
		t.Fatal(err)
	}
	activationID := domain.ExternalAgentJobActivationID(job.ID, statusRevision, domain.JobNotificationTerminal)

	rootModel := newActivationRootModel(job.ID, "", "", 0, 7)
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
		Store: store, Runtime: runtime, ActivationStore: jobs, CompletionReader: jobService, Publisher: responsePublisher,
		Logger: integrationLogger{}, Exchange: store, ModelCalls: modelcalllimiter.New(1),
		SanitizeContent: func(value string) string { return value }, Workstreams: workstreams,
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

	// The unavailable representation fails the activation terminally before
	// model contact.
	if err := activationWorker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	failed, err := jobs.GetActivation(t.Context(), activationID)
	if err != nil || failed == nil {
		t.Fatalf("failed activation = %#v, err=%v", failed, err)
	}
	if failed.State != domain.ActivationFailed || failed.LastErrorCode != "activation_result_unavailable" || !failed.FallbackRequired {
		t.Fatalf("unavailable activation state = %#v", failed)
	}
	if _, _, _, calls := rootModel.snapshot(); calls != 0 {
		t.Fatalf("unavailable activation crossed the root model: calls=%d", calls)
	}

	// The next poll claims the terminal fallback and publishes the host-owned
	// closing update.
	if err := activationWorker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	fallbackPublished, err := jobs.GetActivation(t.Context(), activationID)
	if err != nil || fallbackPublished == nil {
		t.Fatalf("fallback activation = %#v, err=%v", fallbackPublished, err)
	}
	if fallbackPublished.State != domain.ActivationFailed || fallbackPublished.FallbackSlackTS == "" {
		t.Fatalf("fallback publication state = %#v", fallbackPublished)
	}
	calls := responsePublisher.snapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "integrated root response is unavailable") || !strings.Contains(calls[0].text, "job_fallback_e2e") {
		t.Fatalf("fallback publication = %#v", calls)
	}

	// The fallback is reconciled at most once: a third poll publishes nothing
	// and never replays the producing job.
	if err := activationWorker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(responsePublisher.snapshot()) != 1 {
		t.Fatalf("fallback was republished: %#v", responsePublisher.snapshot())
	}
	if calls, _ := acp.callStats(); calls != 1 {
		t.Fatalf("producing ACP job was replayed: %d calls", calls)
	}
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

	acp := &foregroundFakeExternalAgent{raw: rawText}
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
	externalAgentTool := newForegroundExternalAgentTool(t, facade, key, actor)
	sessionService := adaptersqlite.NewAdkSessionService(store)
	runtime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{
		AgentName: "root_agent", Model: rootModel, SessionService: sessionService,
		StaticTools: []tool.Tool{externalAgentTool}, ProviderFamily: domain.ProviderFamilyOpenAICompatible,
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

// activationRootModel verifies that the host supplies the complete direct
// result in the transient frame and that no activation readers are registered.
type activationRootModel struct {
	mu             sync.Mutex
	jobID          string
	expectedText   string
	expectedDigest string
	expectedBytes  int64
	chunkMaxBytes  int64

	request       *model.LLMRequest
	context       port.AgentTurnContext
	contextOK     bool
	calls         int
	chunkCalls    int
	reconstructed strings.Builder
	revision      int
	verified      bool
	failures      []string
	directInline  bool
	response      string
}

func newActivationRootModel(jobID, expectedText, expectedDigest string, expectedBytes, chunkMaxBytes int64) *activationRootModel {
	return &activationRootModel{
		jobID: jobID, expectedText: expectedText, expectedDigest: expectedDigest,
		expectedBytes: expectedBytes, chunkMaxBytes: chunkMaxBytes, directInline: true,
		response: "root synthesis",
	}
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
		call := m.calls
		m.mu.Unlock()
		if m.directInline {
			if call != 1 {
				m.failLoud(yield, "activation made %d model calls", call)
				return
			}
			if request == nil || len(request.Tools) != 0 {
				toolCount := 0
				if request != nil {
					toolCount = len(request.Tools)
				}
				m.failLoud(yield, "activation tools = %d, want none", toolCount)
				return
			}
			foundResult := false
			if request != nil {
				for _, content := range request.Contents {
					if content == nil {
						continue
					}
					for _, part := range content.Parts {
						if part != nil && strings.Contains(part.Text, m.expectedText) {
							foundResult = true
						}
					}
				}
			}
			if !foundResult {
				m.failLoud(yield, "direct result was not present in activation frame")
				return
			}
			m.mu.Lock()
			m.verified = true
			m.mu.Unlock()
			yield(&model.LLMResponse{Content: genai.NewContentFromText(m.response, genai.RoleModel), TurnComplete: true}, nil)
			return
		}
		if call == 1 {
			yield(functionCallResponse("call_job_status_001", "job_status", map[string]any{"job_id": m.jobID}), nil)
			return
		}
		if call == 2 {
			m.mu.Lock()
			m.reconstructed.Reset()
			m.verified = false
			m.mu.Unlock()
			status, revision, available, resultSHA, resultBytes, deliveryMode, ok := jobStatusFromResponse(lastFunctionResponse(request, "job_status"))
			if !ok {
				m.failLoud(yield, "job_status response missing on model call %d", call)
				return
			}
			if status != string(domain.JobCompleted) || !available || deliveryMode != string(domain.JobResultDeliveryMarkdown) || revision <= 0 || resultBytes <= 0 || !isLowerHexSHA256(resultSHA) {
				m.failLoud(yield, "job_status identity mismatch: status=%q available=%t mode=%q revision=%d bytes=%d sha=%q", status, available, deliveryMode, revision, resultBytes, resultSHA)
				return
			}
			if resultSHA != m.expectedDigest || resultBytes != m.expectedBytes {
				m.failLoud(yield, "job_status identity differs from the host fixture: sha=%q want %q bytes=%d want %d", resultSHA, m.expectedDigest, resultBytes, m.expectedBytes)
				return
			}
			m.mu.Lock()
			m.revision = revision
			m.mu.Unlock()
			yield(functionCallResponse("call_chunk_001", "read_job_result_chunk", map[string]any{"job_id": m.jobID, "offset_bytes": int64(0), "max_bytes": m.chunkMaxBytes}), nil)
			return
		}
		m.mu.Lock()
		m.chunkCalls++
		chunkNumber := m.chunkCalls
		m.mu.Unlock()
		content, offset, nextOffset, eof, chunkSHA, ok := chunkFromResponse(lastFunctionResponse(request, "read_job_result_chunk"))
		if !ok {
			m.failLoud(yield, "read_job_result_chunk response missing on model call %d", call)
			return
		}
		m.mu.Lock()
		wantOffset := int64(len([]byte(m.reconstructed.String())))
		m.mu.Unlock()
		if chunkSHA != m.expectedDigest {
			m.failLoud(yield, "chunk %d digest %q does not match the status identity %q", chunkNumber, chunkSHA, m.expectedDigest)
			return
		}
		if !utf8.ValidString(content) || offset != wantOffset || nextOffset <= offset {
			m.failLoud(yield, "chunk %d range invalid: offset=%d want %d next=%d valid=%t", chunkNumber, offset, wantOffset, nextOffset, utf8.ValidString(content))
			return
		}
		if nextOffset > m.expectedBytes {
			m.failLoud(yield, "chunk %d next offset %d exceeds the identity size %d", chunkNumber, nextOffset, m.expectedBytes)
			return
		}
		m.mu.Lock()
		m.reconstructed.WriteString(content)
		reconstructed := m.reconstructed.String()
		m.mu.Unlock()
		if eof {
			if nextOffset != m.expectedBytes {
				m.failLoud(yield, "chunk %d reports EOF at %d, want %d", chunkNumber, nextOffset, m.expectedBytes)
				return
			}
			totalDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(reconstructed)))
			if totalDigest != m.expectedDigest || reconstructed != m.expectedText {
				m.failLoud(yield, "reconstructed result digest %q or text %q does not match the identity %q / %q", totalDigest, reconstructed, m.expectedDigest, m.expectedText)
				return
			}
			m.mu.Lock()
			m.verified = true
			m.mu.Unlock()
			yield(&model.LLMResponse{Content: genai.NewContentFromText(m.response, genai.RoleModel), TurnComplete: true}, nil)
			return
		}
		yield(functionCallResponse(fmt.Sprintf("call_chunk_%03d", chunkNumber+1), "read_job_result_chunk", map[string]any{"job_id": m.jobID, "offset_bytes": nextOffset, "max_bytes": m.chunkMaxBytes}), nil)
	}
}

// failLoud records the verification failure and fails the ADK turn so the
// activation cannot complete while the chunk reader is broken.
func (m *activationRootModel) failLoud(yield func(*model.LLMResponse, error) bool, format string, args ...any) {
	m.mu.Lock()
	m.failures = append(m.failures, fmt.Sprintf(format, args...))
	m.mu.Unlock()
	yield(nil, fmt.Errorf(format, args...))
}

func (m *activationRootModel) snapshot() (*model.LLMRequest, port.AgentTurnContext, bool, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.request, m.context, m.contextOK, m.calls
}

func (m *activationRootModel) chunkStats() (chunkCalls int, reconstructed string, verified bool, failures []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chunkCalls, m.reconstructed.String(), m.verified, append([]string(nil), m.failures...)
}

// functionCallResponse builds one complete model turn that requests a tool.
func functionCallResponse(id, name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			ID: id, Name: name, Args: args,
		}}},
	}, TurnComplete: true}
}

// lastFunctionResponse returns the response map of the most recent function
// response with the given tool name in the request contents.
func lastFunctionResponse(request *model.LLMRequest, name string) (map[string]any, bool) {
	if request == nil {
		return nil, false
	}
	for _, content := range slices.Backward(request.Contents) {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part != nil && part.FunctionResponse != nil && part.FunctionResponse.Name == name {
				return part.FunctionResponse.Response, true
			}
		}
	}
	return nil, false
}

func isLowerHexSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// jobStatusFromResponse decodes the activation job_status tool response. JSON
// numbers arrive as float64; the response is only accepted when every field
// the scripted model must verify is present with its expected type.
func jobStatusFromResponse(response map[string]any, ok bool) (status string, revision int, available bool, resultSHA string, resultBytes int64, deliveryMode string, valid bool) {
	if !ok {
		return "", 0, false, "", 0, "", false
	}
	status, ok1 := response["status"].(string)
	revisionF, ok2 := response["status_revision"].(float64)
	available, ok3 := response["result_available"].(bool)
	resultSHA, ok4 := response["result_sha256"].(string)
	bytesF, ok5 := response["result_bytes"].(float64)
	deliveryMode, ok6 := response["delivery_mode"].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
		return "", 0, false, "", 0, "", false
	}
	return status, int(revisionF), available, resultSHA, int64(bytesF), deliveryMode, true
}

// chunkFromResponse decodes one activation read_job_result_chunk tool
// response. JSON numbers arrive as float64; the response is only accepted
// when every field the scripted model must verify is present.
func chunkFromResponse(response map[string]any, ok bool) (content string, offset, nextOffset int64, eof bool, chunkSHA string, valid bool) {
	if !ok {
		return "", 0, 0, false, "", false
	}
	content, ok1 := response["content"].(string)
	offsetF, ok2 := response["offset_bytes"].(float64)
	nextF, ok3 := response["next_offset_bytes"].(float64)
	eof, ok4 := response["eof"].(bool)
	chunkSHA, ok5 := response["sha256"].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		return "", 0, 0, false, "", false
	}
	return content, int64(offsetF), int64(nextF), eof, chunkSHA, true
}

// detachedFakeACP is the direct ACP runtime behind the detached dispatcher.
// It returns raw multibyte provider text and records exactly how many
// invocations ran and with which durable job ID.
type detachedFakeExternalAgent struct {
	mu        sync.Mutex
	raw       string
	calls     int
	lastJobID string
}

func (r *detachedFakeExternalAgent) Run(_ context.Context, request domain.ExternalAgentInvocationRequest) (domain.ExternalAgentInvocationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastJobID = request.JobID
	return domain.ExternalAgentInvocationResult{Text: r.raw}, nil
}

func (r *detachedFakeExternalAgent) callStats() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.lastJobID
}

// detachedDispatcherRuntime mirrors the detached branch of the real
// acpJobDispatcher.Run: the durable worker executes the job exactly once
// through the direct ACP runtime carrying the job ID and then normalizes the
// raw text into the complete detached delivery identity (host redactor,
// domain.SanitizeResultText, exact UTF-8 bytes, lowercase hex SHA-256,
// canonical Markdown, policy fields). It is the test twin of the detached
// materialize path and produces the same persisted identity as the real host.
// externalAgentInvoker is the shape the durable dispatcher needs from a
// prepared external agent. The production interface it used to mirror was
// removed with the ACP transport; this fake keeps the same contract so the
// detached delivery identity stays under test.
type externalAgentInvoker interface {
	Run(ctx context.Context, request domain.ExternalAgentInvocationRequest) (domain.ExternalAgentInvocationResult, error)
}

type detachedDispatcherRuntime struct {
	acp    externalAgentInvoker
	redact func(string) string
}

func (d *detachedDispatcherRuntime) Run(ctx context.Context, job domain.ExternalAgentJob) (domain.ExternalAgentInvocationResult, error) {
	if job.Mode != domain.JobDetached {
		return domain.ExternalAgentInvocationResult{}, errors.New("detached dispatcher runtime received a non-detached job")
	}
	result, err := d.acp.Run(ctx, domain.ExternalAgentInvocationRequest{
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
		return domain.ExternalAgentInvocationResult{}, err
	}
	size := int64(len([]byte(text)))
	if size <= 0 {
		return domain.ExternalAgentInvocationResult{}, errors.New("detached dispatcher result is empty")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	result.Text = text
	result.Inline = true
	result.ArtifactRef = ""
	result.ResultSHA256 = digest
	result.ResultBytes = size
	result.DeliveryMode = domain.JobResultDeliveryMarkdown
	result.DeliveryCanonicalMarkdown = fmt.Sprintf("OpenCode job `%s` completed.\n\n%s", job.ID, text)
	result.DeliveryPolicyVersion = domain.JobDeliveryPolicyV1
	result.DeliveryMaxMarkdownParts = 6
	result.DeliveryContentSHA256 = digest
	result.DeliveryContentBytes = size
	return result, nil
}

// waitForJobStatus polls the durable store until the job reaches the wanted
// terminal status or fails loudly.
func waitForJobStatus(t *testing.T, jobs *adaptersqlite.ExternalAgentJobStore, jobID string, want domain.ExternalAgentJobStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := jobs.GetJob(t.Context(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		if current == nil {
			t.Fatalf("job %q disappeared while waiting for %q", jobID, want)
		}
		if current.Status == want {
			return
		}
		if current.Status == domain.JobFailed || current.Status == domain.JobCancelled || current.Status == domain.JobAbandoned {
			t.Fatalf("job %q ended with %q (error %q) before %q", jobID, current.Status, current.ErrorCode, want)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job %q did not reach %q", jobID, want)
}

type jobActivationNotificationPublisher struct {
	calls        int
	notification domain.ExternalAgentJobNotification
}

func (p *jobActivationNotificationPublisher) Publish(_ context.Context, notification domain.ExternalAgentJobNotification) (port.PublishedResponse, error) {
	p.calls++
	p.notification = notification
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
type foregroundFakeExternalAgent struct {
	mu        sync.Mutex
	raw       string
	calls     int
	lastJobID string
}

func (r *foregroundFakeExternalAgent) Run(_ context.Context, request domain.ExternalAgentInvocationRequest) (domain.ExternalAgentInvocationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastJobID = request.JobID
	return domain.ExternalAgentInvocationResult{Text: r.raw}, nil
}

func (r *foregroundFakeExternalAgent) callStats() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.lastJobID
}

// foregroundDispatcherRuntime mirrors the acpJobDispatcher foreground branch:
// the durable worker runs the job through the direct ACP runtime and computes
// bytes and SHA-256 only over the final post-redaction, post-sanitization
// text. It is the test twin of normalizeForegroundResult (FR-03).
type foregroundDispatcherRuntime struct {
	acp    externalAgentInvoker
	redact func(string) string
}

func (d *foregroundDispatcherRuntime) Run(ctx context.Context, job domain.ExternalAgentJob) (domain.ExternalAgentInvocationResult, error) {
	if job.Mode != domain.JobForeground {
		return domain.ExternalAgentInvocationResult{}, errors.New("foreground dispatcher runtime received a non-foreground job")
	}
	result, err := d.acp.Run(ctx, domain.ExternalAgentInvocationRequest{
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
		return domain.ExternalAgentInvocationResult{}, err
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
	direct externalAgentInvoker
	jobs   foregroundSynchronousJobRunner
}

type foregroundSynchronousJobRunner interface {
	StartAndWait(context.Context, domain.ExternalAgentJobRequest) (domain.ExternalAgentInvocationResult, error)
}

func (r *foregroundFacadeRuntime) Run(ctx context.Context, request domain.ExternalAgentInvocationRequest) (domain.ExternalAgentInvocationResult, error) {
	if request.JobID != "" || r.jobs == nil || request.Actor == "" || request.ConversationKey == "" {
		return r.direct.Run(ctx, request)
	}
	provider := request.ProviderName
	if provider == "" {
		provider, _, _ = strings.Cut(request.ProfileName, "/")
	}
	return r.jobs.StartAndWait(ctx, domain.ExternalAgentJobRequest{
		Provider: provider, Profile: request.ProfileName,
		PrimaryProject: request.PrimaryProject, RegistryRevision: request.RegistryRevision,
		Task: request.Task, Mode: domain.JobForeground,
		PermissionOptionKind: request.PermissionOptionKind, Timeout: request.Timeout,
		PrimaryPath:   request.PrimaryPath,
		WrapperCallID: request.OriginalCallID, OriginalCallID: request.OriginalCallID,
		Actor: request.Actor, TeamID: request.TeamID, ConversationKey: request.ConversationKey,
	})
}

var _ externalAgentInvoker = (*foregroundFacadeRuntime)(nil)

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

type foregroundExternalAgentToolArgs struct {
	Project string `json:"project" jsonschema:"registered project name"`
	Task    string `json:"task" jsonschema:"complete bounded task"`
}

type foregroundExternalAgentToolResult struct {
	Result string `json:"result"`
}

// newForegroundACPTool builds the confirmable root tool that invokes the
// durable facade, mirroring the foreground branch of the ACP agent tool.
func newForegroundExternalAgentTool(t *testing.T, runtime externalAgentInvoker, key domain.ConversationKey, actor string) tool.Tool {
	t.Helper()
	projectRoot := t.TempDir()
	created, err := functiontool.New(functiontool.Config{
		Name:                "foreground_acp",
		Description:         "Runs a foreground durable ACP task in this conversation.",
		RequireConfirmation: true,
	}, func(_ agent.Context, args foregroundExternalAgentToolArgs) (foregroundExternalAgentToolResult, error) {
		if strings.TrimSpace(args.Project) == "" || strings.TrimSpace(args.Task) == "" {
			return foregroundExternalAgentToolResult{}, errors.New("foreground ACP project and task are required")
		}
		result, err := runtime.Run(context.Background(), domain.ExternalAgentInvocationRequest{
			PrimaryProject: args.Project, PrimaryPath: projectRoot,
			ProfileName: "opencode/build", ProviderName: "opencode", RegistryRevision: "r1",
			Task: args.Task, Actor: actor, TeamID: "T12345678", ConversationKey: key,
		})
		if err != nil {
			return foregroundExternalAgentToolResult{}, err
		}
		return foregroundExternalAgentToolResult{Result: result.Text}, nil
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
var _ port.ExternalAgentJobRuntime = (*detachedDispatcherRuntime)(nil)
var _ externalAgentInvoker = (*detachedFakeExternalAgent)(nil)
var _ port.JobNotificationPublisher = (*jobActivationNotificationPublisher)(nil)
var _ port.ResponsePublisher = (*jobActivationResponsePublisher)(nil)
var _ port.ExternalAgentJobActivationStore = (*drainActivationStore)(nil)
var _ port.ExternalAgentJobCompletionHandler = (*drainActivationHandler)(nil)
