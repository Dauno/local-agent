package app

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func batchTestChild(name, revision, result string) preparedAgentTool {
	return preparedAgentTool{
		definition: agentdef.AgentDef{
			Name: name, Model: "cli/" + name, Instruction: "Complete the delegated task.",
			ExecutionMode: agentdef.ExecutionModeDurableJob,
		},
		model:                &captureModel{text: result},
		cliResolved:          &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "cli", Type: agentdef.ProviderTypeAgentCLI}},
		projectRoots:         map[string]string{"workspace": os.TempDir()},
		executionMode:        agentdef.ExecutionModeDurableJob,
		registryRevision:     revision,
		externalAgentTimeout: time.Minute,
	}
}

type timeoutBatchModel struct{ runs int }

func (*timeoutBatchModel) Name() string { return "timeout-batch" }

func (m *timeoutBatchModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.runs++
	return func(yield func(*model.LLMResponse, error) bool) {
		<-ctx.Done()
		yield(nil, ctx.Err())
	}
}

func TestDurableAgentBatchCombinesResultsInTaskOrder(t *testing.T) {
	t.Parallel()
	children := []preparedAgentTool{
		batchTestChild("luna_worker", "sha256:luna", "luna result"),
		batchTestChild("sonnet_worker", "sha256:sonnet", "sonnet result"),
	}
	spec := externalAgentBatchSpec{
		Version: externalAgentBatchVersion, Mode: "parallel", Project: "workspace",
		Tasks: []externalAgentBatchItem{
			{Agent: "sonnet_worker", Task: "second agent first"},
			{Agent: "luna_worker", Task: "first agent second"},
		},
		FinalInstruction: "Present one combined summary.",
	}
	eligible := map[string]int{"luna_worker": 0, "sonnet_worker": 1}
	revision, err := externalAgentBatchRegistryRevision(spec, children, eligible, 5)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeBatchSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &externalAgentJobDispatcher{
		children: children, batchMaxTasks: 5,
		policy: domain.ResultDeliveryPolicy{MaxInlineResultBytes: 4096, MaxResultArtifactBytes: 4096, MaxMarkdownParts: 1, MaxFileBytes: 4096},
	}
	result, err := dispatcher.Run(context.Background(), domain.ExternalAgentJob{
		ID: "job-batch", Provider: externalAgentBatchProvider, Profile: externalAgentBatchProfile,
		PrimaryProject: "workspace", Task: encoded, RegistryRevision: revision, Mode: domain.JobForeground,
	})
	if err != nil {
		t.Fatal(err)
	}
	sonnet := strings.Index(result.Text, "## Task 1: sonnet_worker")
	luna := strings.Index(result.Text, "## Task 2: luna_worker")
	if sonnet < 0 || luna <= sonnet || !strings.Contains(result.Text, "sonnet result") || !strings.Contains(result.Text, "luna result") {
		t.Fatalf("combined result is not ordered: %q", result.Text)
	}
}

func TestDurableAgentBatchSequentialStopsBeforeLaterTask(t *testing.T) {
	t.Parallel()
	firstModel := &captureModel{text: "first result"}
	secondModel := &captureModel{err: errors.New("second failed")}
	thirdModel := &captureModel{text: "must not run"}
	children := []preparedAgentTool{
		batchTestChild("first_worker", "sha256:first", ""),
		batchTestChild("second_worker", "sha256:second", ""),
		batchTestChild("third_worker", "sha256:third", ""),
	}
	children[0].model, children[1].model, children[2].model = firstModel, secondModel, thirdModel
	spec := externalAgentBatchSpec{
		Version: externalAgentBatchVersion, Mode: "sequential", Project: "workspace",
		Tasks: []externalAgentBatchItem{
			{Agent: "first_worker", Task: "first"},
			{Agent: "second_worker", Task: "second"},
			{Agent: "third_worker", Task: "third"},
		},
		FinalInstruction: "Present one combined summary.",
	}
	eligible := map[string]int{"first_worker": 0, "second_worker": 1, "third_worker": 2}
	revision, err := externalAgentBatchRegistryRevision(spec, children, eligible, 5)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeBatchSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &externalAgentJobDispatcher{children: children, batchMaxTasks: 5}
	_, err = dispatcher.Run(t.Context(), domain.ExternalAgentJob{
		ID: "job-sequential", Provider: externalAgentBatchProvider, Profile: externalAgentBatchProfile,
		PrimaryProject: "workspace", Task: encoded, RegistryRevision: revision, Mode: domain.JobForeground,
	})
	if err == nil || !strings.Contains(err.Error(), "batch task 2 failed") {
		t.Fatalf("sequential error = %v", err)
	}
	if firstModel.runs != 1 || secondModel.runs != 1 || thirdModel.runs != 0 {
		t.Fatalf("sequential runs = first:%d second:%d third:%d", firstModel.runs, secondModel.runs, thirdModel.runs)
	}
}

func TestDurableAgentBatchTaskTimeoutStopsSequentialRemainder(t *testing.T) {
	t.Parallel()
	timedOutModel := &timeoutBatchModel{}
	laterModel := &captureModel{text: "must not run"}
	children := []preparedAgentTool{
		batchTestChild("slow_worker", "sha256:slow", ""),
		batchTestChild("later_worker", "sha256:later", ""),
	}
	children[0].model = timedOutModel
	children[0].externalAgentTimeout = 10 * time.Millisecond
	children[1].model = laterModel
	spec := externalAgentBatchSpec{
		Version: externalAgentBatchVersion, Mode: "sequential", Project: "workspace",
		Tasks: []externalAgentBatchItem{
			{Agent: "slow_worker", Task: "wait"},
			{Agent: "later_worker", Task: "must not start"},
		},
		FinalInstruction: "Present one combined summary.",
	}
	eligible := map[string]int{"slow_worker": 0, "later_worker": 1}
	revision, err := externalAgentBatchRegistryRevision(spec, children, eligible, 5)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeBatchSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &externalAgentJobDispatcher{children: children, batchMaxTasks: 5}
	_, err = dispatcher.Run(t.Context(), domain.ExternalAgentJob{
		ID: "job-task-timeout", Provider: externalAgentBatchProvider, Profile: externalAgentBatchProfile,
		PrimaryProject: "workspace", Task: encoded, RegistryRevision: revision, Mode: domain.JobForeground,
	})
	var externalErr *domain.ExternalAgentError
	if !errors.As(err, &externalErr) || externalErr.Code != domain.ExternalAgentErrorJobTimeout {
		t.Fatalf("task timeout error = %v", err)
	}
	if timedOutModel.runs != 1 || laterModel.runs != 0 {
		t.Fatalf("task timeout runs = slow:%d later:%d", timedOutModel.runs, laterModel.runs)
	}
}

func TestDurableAgentBatchRejectsConfiguredTaskOverflow(t *testing.T) {
	t.Parallel()
	child := batchTestChild("luna_worker", "sha256:luna", "result")
	tasks := make([]externalAgentBatchItem, 6)
	for index := range tasks {
		tasks[index] = externalAgentBatchItem{Agent: "luna_worker", Task: "task"}
	}
	spec := externalAgentBatchSpec{
		Version: externalAgentBatchVersion, Mode: "parallel", Project: "workspace",
		Tasks: tasks, FinalInstruction: "Present one combined summary.",
	}
	encoded, err := encodeBatchSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &externalAgentJobDispatcher{children: []preparedAgentTool{child}, batchMaxTasks: 5}
	_, err = dispatcher.Run(t.Context(), domain.ExternalAgentJob{
		ID: "job-overflow", Provider: externalAgentBatchProvider, Profile: externalAgentBatchProfile,
		PrimaryProject: "workspace", Task: encoded, RegistryRevision: "unused", Mode: domain.JobForeground,
	})
	if err == nil || !strings.Contains(err.Error(), "specification is invalid") {
		t.Fatalf("overflow error = %v", err)
	}
	if child.model.(*captureModel).runs != 0 {
		t.Fatal("overflow batch started a child model")
	}
}

func encodeBatchSpec(spec externalAgentBatchSpec) (string, error) {
	encoded, err := json.Marshal(spec)
	return string(encoded), err
}
