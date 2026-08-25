package app

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/agentcli"
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
	dispatcher := &acpJobDispatcher{
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
	dispatcher := &acpJobDispatcher{children: []preparedAgentTool{durableCLIChild(&captureModel{text: "x"})}}
	job := durableCLIJob()
	job.RegistryRevision = "sha256:stale"

	matched, _, _ := dispatcher.runAgentCLI(context.Background(), job, new(bool))
	if matched {
		t.Fatal("a stale revision must not be dispatched")
	}
}

func TestDurableAgentCLIJobPropagatesFailure(t *testing.T) {
	t.Parallel()
	dispatcher := &acpJobDispatcher{children: []preparedAgentTool{durableCLIChild(&captureModel{err: errors.New("cli failed")})}}

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

// Reconciliation resumes a session. An agent CLI has none, so the operator must
// be told that plainly instead of being shown an ACP lookup failure.
func TestDurableAgentCLIReconcileReportsNoSessionRecovery(t *testing.T) {
	t.Parallel()
	dispatcher := &acpJobDispatcher{children: []preparedAgentTool{durableCLIChild(&captureModel{text: "x"})}}

	_, err := dispatcher.Reconcile(context.Background(), durableCLIJob())
	if err == nil || !strings.Contains(err.Error(), "session recovery is not supported for job") {
		t.Fatalf("err = %v, want the explicit agent_cli recovery refusal", err)
	}
}

// An ACP job must never be served by a CLI child, and the reverse.
func TestDurableDispatcherKeepsFamiliesApart(t *testing.T) {
	t.Parallel()
	dispatcher := &acpJobDispatcher{children: []preparedAgentTool{durableCLIChild(&captureModel{text: "x"})}}
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
	if started.Kind != domain.ACPEventProcessStarted || started.PID != 4321 {
		t.Fatalf("started = %+v, want the process event and its pid", started)
	}

	step := agentCLIProgressEvent(agentcli.Activity{Kind: agentcli.ActivityStep, Step: "command_execution"})
	if step.Kind != domain.ACPEventToolCall || step.Tool != nil {
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
	plain := acpFailureClass(errors.New("codex wrote /etc/passwd"))
	if plain != string(domain.ACPErrorProcessExit) {
		t.Fatalf("class = %q, want the default process-exit class", plain)
	}
	typed := acpFailureClass(&domain.ACPError{Code: domain.ACPErrorResultTooLarge, Err: errors.New("secret")})
	if typed != string(domain.ACPErrorResultTooLarge) {
		t.Fatalf("class = %q, want the typed code", typed)
	}
}
