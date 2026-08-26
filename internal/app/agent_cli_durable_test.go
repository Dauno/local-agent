package app

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/agentcli"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

// captureModel records the request a durable job builds and returns fixed text.
type captureModel struct {
	request *model.LLMRequest
	text    string
	err     error
	runs    int
}

type recoveryCaptureModel struct {
	*captureModel
	project   string
	sessionID string
	result    string
}

func (m *recoveryCaptureModel) Resume(_ context.Context, project, sessionID string) (string, error) {
	m.project, m.sessionID = project, sessionID
	return m.result, nil
}

func (m *captureModel) Name() string { return "capture" }

func (m *captureModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.request = request
	m.runs++
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.err != nil {
			yield(nil, m.err)
			return
		}
		yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText(m.text)}}}, nil)
	}
}

func durableCLIChild(childModel model.LLM) preparedAgentTool {
	return preparedAgentTool{
		definition:       agentdef.AgentDef{Name: "cli_leaf", Model: "codex/build", Instruction: "leaf instruction"},
		model:            childModel,
		cliResolved:      &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "codex", Type: agentdef.ProviderTypeAgentCLI}},
		registryRevision: "sha256:test",
	}
}

func durableCLIJob() domain.ExternalAgentJob {
	return domain.ExternalAgentJob{
		ID: "job-1", Provider: "codex", Profile: "codex/build", PrimaryProject: "local-agent",
		Task: "run the tests", RegistryRevision: "sha256:test", Mode: domain.JobForeground,
	}
}

// The worker owns the turn for a durable job, so the delegation must be built
// in the same {project, task} shape the in-session leaf receives.
func TestDurableAgentCLIJobBuildsDelegation(t *testing.T) {
	t.Parallel()
	captured := &captureModel{text: "  the result  "}
	dispatcher := &externalAgentJobDispatcher{
		children: []preparedAgentTool{durableCLIChild(captured)},
		global:   "global instruction",
		// A foreground result is size-checked like any other, so the bound must
		// be set for the shared normalization to accept it.
		policy: domain.ResultDeliveryPolicy{MaxInlineResultBytes: 4096, MaxResultArtifactBytes: 4096, MaxMarkdownParts: 1, MaxFileBytes: 4096},
	}

	matched, result, err := dispatcher.runAgentCLI(context.Background(), durableCLIJob(), new(bool))
	if !matched || err != nil {
		t.Fatalf("matched = %v, err = %v", matched, err)
	}
	if result.Text != "the result" {
		t.Fatalf("text = %q, want the trimmed CLI result", result.Text)
	}
	prompt := captured.request.Contents[0].Parts[0].Text
	for _, want := range []string{`"project":"local-agent"`, `"task":"run the tests"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("delegation %q is missing %q", prompt, want)
		}
	}
	instruction := captured.request.Config.SystemInstruction.Parts[0].Text
	if !strings.Contains(instruction, "global instruction") || !strings.Contains(instruction, "leaf instruction") {
		t.Fatalf("system instruction = %q, want both the global and leaf text", instruction)
	}
}

// A job whose scope no longer matches the running configuration must not run
// against a changed registry.
func TestDurableAgentCLIJobRejectsStaleRevision(t *testing.T) {
	t.Parallel()
	dispatcher := &externalAgentJobDispatcher{children: []preparedAgentTool{durableCLIChild(&captureModel{text: "x"})}}
	job := durableCLIJob()
	job.RegistryRevision = "sha256:stale"

	matched, _, _ := dispatcher.runAgentCLI(context.Background(), job, new(bool))
	if matched {
		t.Fatal("a stale revision must not be dispatched")
	}
}

func TestDurableAgentCLIJobPropagatesFailure(t *testing.T) {
	t.Parallel()
	dispatcher := &externalAgentJobDispatcher{children: []preparedAgentTool{durableCLIChild(&captureModel{err: errors.New("cli failed")})}}

	matched, _, err := dispatcher.runAgentCLI(context.Background(), durableCLIJob(), new(bool))
	if !matched || err == nil || !strings.Contains(err.Error(), "cli failed") {
		t.Fatalf("matched = %v, err = %v", matched, err)
	}
}

// The durable worker is composed from one child list. An external-agent leaf
// used to be dropped from it, so its enqueued jobs found no runtime and failed
// within milliseconds of being approved.
func TestExternalAgentChildrenSelectsExternalLeaves(t *testing.T) {
	t.Parallel()
	prepared := []preparedAgentTool{
		{definition: agentdef.AgentDef{Name: "plain_leaf", Model: "deepseek/chat"}, model: &captureModel{text: "x"}},
		durableCLIChild(&captureModel{text: "x"}),
	}

	children := externalAgentChildren(prepared)
	if len(children) != 1 || children[0].definition.Name != "cli_leaf" {
		t.Fatalf("children = %#v, want only the external-agent leaf", children)
	}
}

// The Slack scope precondition for durable delivery applies to both families.
func TestDurableConfiguredCoversAgentCLI(t *testing.T) {
	t.Parallel()
	child := durableCLIChild(&captureModel{text: "x"})
	child.definition.ExecutionMode = agentdef.ExecutionModeDurableJob
	models := newRuntimeModels()
	models.preparedAgentTools = []preparedAgentTool{child}

	if !durableExternalAgentConfigured(models) {
		t.Fatal("a durable agent_cli leaf must require the durable delivery scope")
	}
}

// A dispatcher with no session descriptor must not expose the optional recovery
// interface.
func TestDurableAgentCLIReconcileReportsNoSessionRecovery(t *testing.T) {
	t.Parallel()
	dispatcher := &externalAgentJobDispatcher{children: []preparedAgentTool{durableCLIChild(&captureModel{text: "x"})}}

	if _, ok := any(dispatcher).(interface {
		Reconcile(context.Context, domain.ExternalAgentJob) (domain.ExternalAgentInvocationResult, error)
	}); ok {
		t.Fatal("a descriptor without session must not expose session recovery")
	}
}

func TestDurableRuntimeCompositionExposesRecoveryOnlyForSessionDescriptors(t *testing.T) {
	t.Parallel()
	without := &externalAgentJobDispatcher{children: []preparedAgentTool{durableCLIChild(&captureModel{text: "x"})}}
	if _, ok := jobRuntimeForDispatcher(without).(interface {
		Reconcile(context.Context, domain.ExternalAgentJob) (domain.ExternalAgentInvocationResult, error)
	}); ok {
		t.Fatal("runtime without a session descriptor exposed recovery")
	}
	child := durableCLIChild(&captureModel{text: "x"})
	child.cliResolved.Session = &agentdef.CLISession{}
	with := &externalAgentJobDispatcher{children: []preparedAgentTool{child}}
	if _, ok := jobRuntimeForDispatcher(with).(interface {
		Reconcile(context.Context, domain.ExternalAgentJob) (domain.ExternalAgentInvocationResult, error)
	}); !ok {
		t.Fatal("runtime with a session descriptor did not expose recovery")
	}
}

func TestDurableAgentCLIReconcileResumesPersistedSessionWithoutTask(t *testing.T) {
	t.Parallel()
	model := &recoveryCaptureModel{captureModel: &captureModel{}, result: "recovered result"}
	child := durableCLIChild(model)
	child.cliResolved.Session = &agentdef.CLISession{}
	dispatcher := &externalAgentJobDispatcher{
		children: []preparedAgentTool{child},
		policy:   domain.ResultDeliveryPolicy{MaxInlineResultBytes: 4096, MaxResultArtifactBytes: 4096, MaxMarkdownParts: 1, MaxFileBytes: 4096},
	}
	job := durableCLIJob()
	job.ExternalAgentSessionID = "session-1234"
	result, err := (&recoverableExternalAgentJobDispatcher{externalAgentJobDispatcher: dispatcher}).Reconcile(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "recovered result" || model.project != job.PrimaryProject || model.sessionID != job.ExternalAgentSessionID {
		t.Fatalf("result = %+v, recovery = %q/%q", result, model.project, model.sessionID)
	}
	if model.runs != 0 {
		t.Fatal("reconciliation replayed the normal delegated task")
	}
}

// This test crosses the production composition boundary: the real generic CLI
// parser reports a session event, and the durable dispatcher writes it through
// the lease-bound SQLite store.
func TestDurableDispatcherPersistsSessionAndTranscriptFromRealCLIStream(t *testing.T) {
	workspace := t.TempDir()
	transcriptRoot := t.TempDir()
	transcript := filepath.Join(transcriptRoot, "nested", "rollout-session-compose.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "agent.sh")
	if err := os.WriteFile(
		script,
		[]byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"session-compose\"}' '{\"type\":\"result\",\"result\":\"done\"}'\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	session := &agentdef.CLISession{
		ID:         agentdef.CLISessionID{When: map[string]string{"type": "thread.started"}, Path: "thread_id"},
		Transcript: agentdef.CLISessionTranscript{PathGlob: filepath.Join(transcriptRoot, "**", "rollout-{{session_id}}.jsonl")},
		Resume:     agentdef.CLISessionResume{ResumeFlag: []string{"--resume", "{{session_id}}"}},
	}
	provider := agentdef.Provider{
		Name:       "codex",
		Type:       agentdef.ProviderTypeAgentCLI,
		Version:    &agentdef.CLIVersion{Command: []string{"--version"}, Pattern: `(?P<version>\d+\.\d+\.\d+)`, Min: "1.0.0"},
		Invocation: &agentdef.CLIInvocation{Prompt: "stdin", Args: []string{script}},
		Stream: &agentdef.CLIStream{
			Format:        "ndjson",
			IgnoreTypes:   []string{"thread.started"},
			FinalText:     agentdef.CLIFinalText{When: map[string]string{"type": "result"}, Path: "result"},
			Failure:       agentdef.CLIFailure{WhenAny: []map[string]string{{"type": "error"}}},
			Activity:      &agentdef.CLIActivity{When: map[string]string{"type": "activity"}, TypeField: "kind", DiscardTypes: []string{}},
			TerminalTypes: []string{"result"},
		},
		Session: session,
	}
	cliModel, err := agentcli.New(agentcli.Config{
		Command: "/bin/sh", Provider: provider, Profile: agentdef.Profile{Model: "fake"},
		Workspace: domain.Workspace{WorkingDirectory: workspace, Projects: []domain.Project{{Name: "workspace", Path: workspace}}}, WorkingDir: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	database, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	store := adaptersqlite.NewExternalAgentJobStore(database)
	now := time.Now().UTC()
	job := domain.ExternalAgentJob{
		ID: "job-compose", Mode: domain.JobForeground, Provider: "codex", Profile: "codex/fake", PrimaryProject: "workspace",
		RegistryRevision: "sha256:compose", Task: "task", RequestSHA256: "request", OriginalCallID: "call-compose",
		Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678",
		Status: domain.JobQueued, TimeoutAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now,
	}
	if created, _, err := store.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create = %v, err = %v", created, err)
	}
	claimed, err := store.ClaimNext(t.Context(), now, "worker-compose", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, err = %v", claimed, err)
	}
	child := preparedAgentTool{
		definition: agentdef.AgentDef{Name: "worker", Model: "codex/fake"}, model: cliModel,
		cliResolved: &agentdef.ResolvedModel{Provider: provider, Session: session}, registryRevision: "sha256:compose",
	}
	dispatcher := &externalAgentJobDispatcher{
		children: []preparedAgentTool{child},
		store:    store,
		policy:   domain.ResultDeliveryPolicy{MaxInlineResultBytes: 4096, MaxResultArtifactBytes: 4096, MaxMarkdownParts: 1, MaxFileBytes: 4096},
	}
	if _, err := dispatcher.Run(t.Context(), *claimed); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetJob(t.Context(), job.ID)
	if err != nil || stored == nil || stored.ExternalAgentSessionID != "session-compose" || stored.TranscriptPath != transcript {
		t.Fatalf("stored job = %#v, err = %v", stored, err)
	}
}

// An external-agent job must never be served by a CLI child, and the reverse.
func TestDurableDispatcherKeepsFamiliesApart(t *testing.T) {
	t.Parallel()
	dispatcher := &externalAgentJobDispatcher{children: []preparedAgentTool{durableCLIChild(&captureModel{text: "x"})}}
	job := durableCLIJob()
	job.Provider = "opencode"

	matched, _, _ := dispatcher.runAgentCLI(context.Background(), job, new(bool))
	if matched {
		t.Fatal("a job for another provider must not match the CLI child")
	}
}

// A CLI reports a step that already finished, so the tool identity stays nil.
// The projection reads that as meaningful progress without tracking a call
// from pending to terminal, which keeps the stall warning honest.
func TestAgentCLIProgressEventMapping(t *testing.T) {
	t.Parallel()
	started := agentCLIProgressEvent(agentcli.Activity{Kind: agentcli.ActivityProcessStarted, PID: 4321})
	if started.Kind != domain.ExternalAgentEventProcessStarted || started.PID != 4321 {
		t.Fatalf("started = %+v, want the process event and its pid", started)
	}

	step := agentCLIProgressEvent(agentcli.Activity{Kind: agentcli.ActivityStep, Step: "command_execution"})
	if step.Kind != domain.ExternalAgentEventToolCall || step.Tool != nil {
		t.Fatalf("step = %+v, want a tool call with no tracked identity", step)
	}

	var projection domain.ExternalAgentJobProgress
	now := time.Now().UTC()
	projection.Apply(step, now)
	if projection.LastMeaningfulProgressAt != now {
		t.Fatal("a reported step must count as meaningful progress")
	}
	if projection.ActiveToolCount != 0 {
		t.Fatalf("active tools = %d, want none tracked", projection.ActiveToolCount)
	}
}

// The failure class is bounded and never carries the error text.
func TestAgentCLIFailureClassIsBounded(t *testing.T) {
	t.Parallel()
	plain := externalAgentFailureClass(errors.New("codex wrote /etc/passwd"))
	if plain != string(domain.ExternalAgentErrorProcessExit) {
		t.Fatalf("class = %q, want the default process-exit class", plain)
	}
	typed := externalAgentFailureClass(&domain.ExternalAgentError{Code: domain.ExternalAgentErrorResultTooLarge, Err: errors.New("secret")})
	if typed != string(domain.ExternalAgentErrorResultTooLarge) {
		t.Fatalf("class = %q, want the typed code", typed)
	}
}
