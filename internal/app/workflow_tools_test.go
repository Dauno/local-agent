package app

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type exitLoopCallingModel struct {
	calls int
}

type stubRunnableWorkflowTool struct {
	result map[string]any
}

func (*stubRunnableWorkflowTool) Name() string                            { return "EmptyWorkflow" }
func (*stubRunnableWorkflowTool) Description() string                     { return "test" }
func (*stubRunnableWorkflowTool) IsLongRunning() bool                     { return false }
func (*stubRunnableWorkflowTool) Declaration() *genai.FunctionDeclaration { return nil }
func (t *stubRunnableWorkflowTool) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	if req.Tools == nil {
		req.Tools = make(map[string]any)
	}
	req.Tools[t.Name()] = t
	return nil
}
func (t *stubRunnableWorkflowTool) Run(agent.Context, any) (map[string]any, error) {
	return t.result, nil
}

func (*exitLoopCallingModel) Name() string { return "exit-loop-caller" }

func (m *exitLoopCallingModel) GenerateContent(_ context.Context, _ *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	m.calls++
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "exit-1", Name: "exit_loop", Args: map[string]any{},
			}}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

func TestCompositeFactoryExecutesWorkflowWithOnlyRequestedScopedTools(t *testing.T) {
	childModel := &exploringChildModel{}
	rootPath := "/workflow/root_agent.yaml"
	rootDoc := agentdef.AgentDocument{
		Path:        rootPath,
		AgentClass:  agentdef.AgentClassLLM,
		Name:        "ReaderWorkflow",
		Description: "Reads one delegated file.",
		LLM: &agentdef.LLMAgentDocument{
			Model:           "deepseek/test",
			Instruction:     "Inspect the delegated request.",
			IncludeContents: "none",
			Tools:           []agentdef.ToolRef{{Name: "read_file"}},
		},
	}
	bp := &agentdef.WorkflowBlueprint{
		ID:          "reader",
		Description: rootDoc.Description,
		Root:        rootDoc,
		Documents:   map[string]agentdef.AgentDocument{rootPath: rootDoc},
	}
	base := &fakeBaseFactory{readOutput: "package main"}
	factory := newCompositeAgentToolFactory(base, nil, []preparedWorkflowTool{{
		blueprint: bp,
		models:    map[string]model.LLM{"deepseek/test": childModel},
	}}, "Global policy.")

	raw, err := factory.ToolsForInvocation("U777", domain.ConversationKey("slack:T1:dm:D1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(toolNames(t, raw), ","), "ReaderWorkflow,list_repos,read_file"; got != want {
		t.Fatalf("tool order = %q, want %q", got, want)
	}

	rootModel := &delegatingRootModel{target: "ReaderWorkflow"}
	root, err := llmagent.New(llmagent.Config{
		Name:        "root_agent",
		Model:       rootModel,
		Instruction: "Delegate reads.",
		Mode:        llmagent.ModeChat,
		Tools:       rawAsTools(t, raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	if final := runDelegatingTurn(t, root); final != "root used delegated result" {
		t.Fatalf("final response = %q", final)
	}
	if childModel.calls != 2 || childModel.stream || !childModel.sawResult {
		t.Fatalf("workflow model calls=%d stream=%v sawResult=%v", childModel.calls, childModel.stream, childModel.sawResult)
	}
	if !strings.Contains(childModel.system, "Global policy.") {
		t.Fatalf("workflow child did not receive global instruction: %q", childModel.system)
	}
}

func TestWorkflowConstructionFailsWhenScopedToolIsUnavailable(t *testing.T) {
	doc := agentdef.AgentDocument{
		AgentClass: agentdef.AgentClassLLM,
		Name:       "ReaderWorkflow",
		LLM: &agentdef.LLMAgentDocument{
			Model:       "deepseek/test",
			Instruction: "Read.",
			Tools:       []agentdef.ToolRef{{Name: "read_file"}},
		},
	}
	bp := &agentdef.WorkflowBlueprint{Root: doc}
	_, err := buildWorkflowAgent(bp, map[string]model.LLM{"deepseek/test": &streamRecordingModel{}}, invocationScope{})
	if err == nil || !strings.Contains(err.Error(), "not registered or not available") {
		t.Fatalf("error = %v", err)
	}
}

func TestWorkflowToolRejectsEmptyFinalText(t *testing.T) {
	guard := &nonEmptyWorkflowTool{delegate: &stubRunnableWorkflowTool{result: map[string]any{}}}
	if _, err := guard.Run(nil, nil); err == nil || !strings.Contains(err.Error(), "produced no final text") {
		t.Fatalf("error = %v", err)
	}

	req := &model.LLMRequest{}
	if err := guard.ProcessRequest(nil, req); err != nil {
		t.Fatal(err)
	}
	if req.Tools[guard.Name()] != guard {
		t.Fatalf("request runnable tool was not replaced by output guard: %#v", req.Tools)
	}
}

func TestSequentialWorkflowUsesADKOutputKeyState(t *testing.T) {
	writerPath := "/workflow/writer.yaml"
	presenterPath := "/workflow/presenter.yaml"
	rootDoc := agentdef.AgentDocument{
		Path:        "/workflow/root_agent.yaml",
		AgentClass:  agentdef.AgentClassSequential,
		Name:        "StatePipeline",
		Description: "Writes and presents a result.",
		SubAgents: []agentdef.AgentRef{
			{ConfigPath: "writer.yaml", Path: writerPath},
			{ConfigPath: "presenter.yaml", Path: presenterPath},
		},
	}
	writerDoc := agentdef.AgentDocument{
		Path:       writerPath,
		AgentClass: agentdef.AgentClassLLM,
		Name:       "Writer",
		LLM: &agentdef.LLMAgentDocument{
			Model:       "deepseek/writer",
			Instruction: "Write the delegated request.",
			OutputKey:   "draft",
		},
	}
	presenterDoc := agentdef.AgentDocument{
		Path:       presenterPath,
		AgentClass: agentdef.AgentClassLLM,
		Name:       "Presenter",
		LLM: &agentdef.LLMAgentDocument{
			Model:           "deepseek/presenter",
			Instruction:     "Present this draft: {draft}",
			IncludeContents: "none",
		},
	}
	bp := &agentdef.WorkflowBlueprint{
		ID:          "state_pipeline",
		Description: rootDoc.Description,
		Root:        rootDoc,
		Documents: map[string]agentdef.AgentDocument{
			rootDoc.Path:  rootDoc,
			writerPath:    writerDoc,
			presenterPath: presenterDoc,
		},
	}
	writerModel := &streamRecordingModel{}
	presenterModel := &streamRecordingModel{}
	prepared := preparedWorkflowTool{
		blueprint: bp,
		models: map[string]model.LLM{
			"deepseek/writer":    writerModel,
			"deepseek/presenter": presenterModel,
		},
	}
	workflowTool, err := prepared.buildAgentTool(invocationScope{globalInstruction: "Global policy."})
	if err != nil {
		t.Fatal(err)
	}
	root, err := llmagent.New(llmagent.Config{
		Name:        "root_agent",
		Model:       &delegatingRootModel{target: "StatePipeline"},
		Instruction: "Delegate writing.",
		Mode:        llmagent.ModeChat,
		Tools:       []tool.Tool{workflowTool},
	})
	if err != nil {
		t.Fatal(err)
	}
	if final := runDelegatingTurn(t, root); final != "root used delegated result" {
		t.Fatalf("final response = %q", final)
	}
	if writerModel.calls != 1 || presenterModel.calls != 1 {
		t.Fatalf("model calls writer=%d presenter=%d", writerModel.calls, presenterModel.calls)
	}
	if !strings.Contains(presenterModel.system, "Present this draft: delegated") {
		t.Fatalf("presenter instruction did not receive ADK state: %q", presenterModel.system)
	}
}

func TestWorkflowLoopExitsThroughADKExitLoopTool(t *testing.T) {
	paths := map[string]string{
		"root":      "/workflow/root_agent.yaml",
		"writer":    "/workflow/writer.yaml",
		"loop":      "/workflow/loop.yaml",
		"critic":    "/workflow/critic.yaml",
		"refiner":   "/workflow/refiner.yaml",
		"presenter": "/workflow/presenter.yaml",
	}
	rootDoc := agentdef.AgentDocument{
		Path: paths["root"], AgentClass: agentdef.AgentClassSequential,
		Name: "LoopPipeline", Description: "Runs a bounded refinement loop.",
		SubAgents: []agentdef.AgentRef{
			{ConfigPath: "writer.yaml", Path: paths["writer"]},
			{ConfigPath: "loop.yaml", Path: paths["loop"]},
			{ConfigPath: "presenter.yaml", Path: paths["presenter"]},
		},
	}
	writerDoc := agentdef.AgentDocument{
		Path: paths["writer"], AgentClass: agentdef.AgentClassLLM, Name: "Writer",
		LLM: &agentdef.LLMAgentDocument{Model: "test/writer", Instruction: "Write.", OutputKey: "draft"},
	}
	loopDoc := agentdef.AgentDocument{
		Path: paths["loop"], AgentClass: agentdef.AgentClassLoop, Name: "RefinementLoop",
		SubAgents: []agentdef.AgentRef{
			{ConfigPath: "critic.yaml", Path: paths["critic"]},
			{ConfigPath: "refiner.yaml", Path: paths["refiner"]},
		},
		Loop: &agentdef.LoopAgentDocument{MaxIterations: 5},
	}
	criticDoc := agentdef.AgentDocument{
		Path: paths["critic"], AgentClass: agentdef.AgentClassLLM, Name: "Critic",
		LLM: &agentdef.LLMAgentDocument{Model: "test/critic", Instruction: "Review {draft}.", IncludeContents: "none", OutputKey: "critique"},
	}
	refinerDoc := agentdef.AgentDocument{
		Path: paths["refiner"], AgentClass: agentdef.AgentClassLLM, Name: "Refiner",
		LLM: &agentdef.LLMAgentDocument{
			Model: "test/refiner", Instruction: "Exit after {critique}.", IncludeContents: "none",
			Tools: []agentdef.ToolRef{{Name: "exit_loop"}},
		},
	}
	presenterDoc := agentdef.AgentDocument{
		Path: paths["presenter"], AgentClass: agentdef.AgentClassLLM, Name: "Presenter",
		LLM: &agentdef.LLMAgentDocument{Model: "test/presenter", Instruction: "Present {draft}.", IncludeContents: "none"},
	}
	documents := map[string]agentdef.AgentDocument{
		paths["root"]: rootDoc, paths["writer"]: writerDoc, paths["loop"]: loopDoc,
		paths["critic"]: criticDoc, paths["refiner"]: refinerDoc, paths["presenter"]: presenterDoc,
	}
	bp := &agentdef.WorkflowBlueprint{ID: "loop", Description: rootDoc.Description, Root: rootDoc, Documents: documents}
	exitModel := &exitLoopCallingModel{}
	presenterModel := &streamRecordingModel{}
	prepared := preparedWorkflowTool{blueprint: bp, models: map[string]model.LLM{
		"test/writer": &streamRecordingModel{}, "test/critic": &streamRecordingModel{},
		"test/refiner": exitModel, "test/presenter": presenterModel,
	}}
	workflowTool, err := prepared.buildAgentTool(invocationScope{globalInstruction: "Global policy."})
	if err != nil {
		t.Fatal(err)
	}
	root, err := llmagent.New(llmagent.Config{
		Name: "root_agent", Model: &delegatingRootModel{target: "LoopPipeline"},
		Instruction: "Delegate refinement.", Mode: llmagent.ModeChat, Tools: []tool.Tool{workflowTool},
	})
	if err != nil {
		t.Fatal(err)
	}
	if final := runDelegatingTurn(t, root); final != "root used delegated result" {
		t.Fatalf("final response = %q", final)
	}
	if exitModel.calls != 1 {
		t.Fatalf("refiner calls = %d, want loop exit after 1", exitModel.calls)
	}
	if !strings.Contains(presenterModel.system, "Present delegated.") {
		t.Fatalf("presenter did not run after loop exit: %q", presenterModel.system)
	}
}

func TestACPWorkflowNodeValidatesGitResultAndWritesOutput(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	managedBase := filepath.Join(t.TempDir(), "worktrees")
	managedInvocation := filepath.Join(managedBase, "project", "call")
	createdWorktree := filepath.Join(managedInvocation, "feature")
	for _, path := range []string{project, createdWorktree} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	jsonResult := `{"status":"success","repository":"workspace","pr_url":"https://example.test/pr/1","branch":"trd/x","base_branch":"main","remote":"origin","commit":"abc","title":"Title","file_path":"docs/TRD-X.md","worktree":"` + createdWorktree + `","error":""}`
	runtime := &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: jsonResult}}
	doc := agentdef.AgentDocument{
		AgentClass: agentdef.AgentClassAcp,
		Name:       "TRDGitOperator",
		ACP: &agentdef.AcpAgentDocument{
			Runtime: "opencode/build", Instruction: "Deliver {trd_content}.", Project: "{target_project}",
			AdditionalDirectories: []string{"{worktree_root}"}, OutputKey: "delivery_result", OutputSchema: "git_delivery_result",
		},
	}
	bp := &agentdef.WorkflowBlueprint{Root: doc}
	root, err := buildWorkflowAgent(bp, nil, invocationScope{
		globalInstruction: "Global.", acpRuntimes: map[string]port.ExternalAgentRuntime{"opencode/build": runtime},
		acpResolved: map[string]*agentdef.ResolvedModel{"opencode/build": {
			Provider: agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeACP}, PermissionOptionKind: domain.ACPPermissionAllowOnce,
		}},
		projectRoots: map[string]string{"workspace": project}, worktreeRoot: managedBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := runWorkflow(t.Context(), root, map[string]any{
		"target_project": "workspace", "worktree_root": managedInvocation, "trd_content": "TRD",
	}, "deliver")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, `"status":"success"`) || runtime.request.PrimaryPath != project || len(runtime.request.AdditionalPaths) != 1 {
		t.Fatalf("text = %s, request = %+v", text, runtime.request)
	}
}
