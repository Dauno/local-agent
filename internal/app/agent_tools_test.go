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

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/agenttool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type streamRecordingModel struct {
	stream bool
	calls  int
	system string
	input  string
}

func (*streamRecordingModel) Name() string { return "recording" }

func (m *streamRecordingModel) GenerateContent(_ context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	m.stream = stream
	m.calls++
	if request != nil && request.Config != nil && request.Config.SystemInstruction != nil {
		for _, part := range request.Config.SystemInstruction.Parts {
			if part != nil {
				m.system += part.Text
			}
		}
	}
	if request != nil && len(request.Contents) > 0 {
		for _, part := range request.Contents[len(request.Contents)-1].Parts {
			if part != nil {
				m.input += part.Text
			}
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: genai.NewContentFromText("delegated", genai.RoleModel), FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

type delegatingRootModel struct {
	calls  int
	target string
}

type fakeExternalRuntime struct {
	request domain.AcpInvocationRequest
	result  domain.AcpInvocationResult
	err     error
	probes  int
	runs    int
}

func (f *fakeExternalRuntime) Run(_ context.Context, request domain.AcpInvocationRequest) (domain.AcpInvocationResult, error) {
	f.runs++
	f.request = request
	return f.result, f.err
}

type acpCallingRootModel struct{}

func (*acpCallingRootModel) Name() string { return "acp-caller" }

func (*acpCallingRootModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "acp-1", Name: "opencode_worker", Args: map[string]any{"project": "workspace", "task": "change code"},
			}}}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
		}, nil)
	}
}

func (f *fakeExternalRuntime) Probe(context.Context, string, []string, []domain.ACPConfigOption) error {
	f.probes++
	return f.err
}

func (f *fakeExternalRuntime) Describe(context.Context) (domain.ACPInitResult, error) {
	return domain.ACPInitResult{ProtocolVersion: "1", AgentInfo: domain.ACPAgentInfo{Name: "fake", Version: "1"}}, f.err
}

func (*delegatingRootModel) Name() string { return "root" }

func (m *delegatingRootModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.calls++
	call := m.calls
	target := m.target
	if target == "" {
		target = "opencode_worker"
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		if call == 1 {
			yield(&model.LLMResponse{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
					ID: "delegate-1", Name: target, Args: map[string]any{"request": "inspect the repository"},
				}}}},
				FinishReason: genai.FinishReasonStop,
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content:      genai.NewContentFromText("root used delegated result", genai.RoleModel),
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

func TestAgentToolModelForcesNonStreamingCall(t *testing.T) {
	delegate := &streamRecordingModel{}
	wrapped := &agentToolNonStreamingModel{delegate: delegate}
	for _, err := range wrapped.GenerateContent(t.Context(), &model.LLMRequest{}, true) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if delegate.stream {
		t.Fatal("AgentTool SSE mode reached the non-streaming delegated model")
	}
}

func TestNewAgentToolAgentUsesDefinition(t *testing.T) {
	definition := agentdef.AgentDef{
		Name:            "opencode_worker",
		Description:     "Handles delegated coding tasks.",
		Instruction:     "Return a concise result.",
		IncludeContents: "none",
	}
	child, err := newAgentToolAgent(definition, "Global policy.", &streamRecordingModel{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if child.Name() != definition.Name || child.Description() != definition.Description {
		t.Fatalf("child identity = %q / %q", child.Name(), child.Description())
	}
}

func TestADKAgentToolExecutesTextOnlyChild(t *testing.T) {
	childModel := &streamRecordingModel{}
	child, err := newAgentToolAgent(agentdef.AgentDef{
		Name:            "opencode_worker",
		Description:     "Handles delegated repository tasks.",
		Instruction:     "Complete the delegated task.",
		IncludeContents: "none",
	}, "Global policy.", childModel, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootModel := &delegatingRootModel{}
	root, err := llmagent.New(llmagent.Config{
		Name:        "root_agent",
		Model:       rootModel,
		Instruction: "Delegate repository tasks.",
		Mode:        llmagent.ModeChat,
		Tools:       []tool.Tool{agenttool.New(child, nil)},
	})
	if err != nil {
		t.Fatal(err)
	}
	final := runDelegatingTurn(t, root)
	if final != "root used delegated result" {
		t.Fatalf("final response = %q", final)
	}
	if childModel.calls != 1 || childModel.stream {
		t.Fatalf("child calls = %d, stream = %v", childModel.calls, childModel.stream)
	}
	if !strings.Contains(childModel.system, "Global policy.") || childModel.input != "inspect the repository" {
		t.Fatalf("child system/input = %q / %q", childModel.system, childModel.input)
	}
	if rootModel.calls != 2 {
		t.Fatalf("root calls = %d, want 2", rootModel.calls)
	}
}

// --- invocation-scoped composition ---

type fakeBaseFactory struct {
	err        error
	lastActor  string
	lastKey    domain.ConversationKey
	readCalls  []string
	readActor  []string
	readOutput string
}

var _ port.AgentToolFactory = (*fakeBaseFactory)(nil)

func (f *fakeBaseFactory) ToolsForInvocation(actor string, key domain.ConversationKey) ([]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.lastActor = actor
	f.lastKey = key
	trustedActor := actor
	type readFileArgs struct {
		Project string `json:"project"`
		Path    string `json:"path"`
	}
	readFile, err := functiontool.New(functiontool.Config{
		Name:        "read_file",
		Description: "Reads a file from a pre-registered project. Read-only.",
	}, func(ctx agent.Context, args readFileArgs) (map[string]any, error) {
		f.readCalls = append(f.readCalls, ctx.FunctionCallID())
		f.readActor = append(f.readActor, trustedActor)
		return map[string]any{"content": f.readOutput}, nil
	})
	if err != nil {
		return nil, err
	}
	listRepos, err := functiontool.New(functiontool.Config{
		Name:        "list_repos",
		Description: "Lists pre-registered projects. Read-only.",
	}, func(agent.Context, struct{}) (map[string]any, error) {
		return map[string]any{"repos": []string{"workspace"}}, nil
	})
	if err != nil {
		return nil, err
	}
	return []any{listRepos, readFile}, nil
}

// exploringChildModel drives a two-step child trajectory: call read_file, then
// consume the FunctionResponse and return final evidence.
type exploringChildModel struct {
	calls     int
	stream    bool
	sawResult bool
	system    string
}

func (*exploringChildModel) Name() string { return "explore-model" }

func (m *exploringChildModel) GenerateContent(_ context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	m.calls++
	m.stream = m.stream || stream
	call := m.calls
	if request != nil && request.Config != nil && request.Config.SystemInstruction != nil {
		for _, part := range request.Config.SystemInstruction.Parts {
			if part != nil {
				m.system += part.Text
			}
		}
	}
	if request != nil {
		for _, content := range request.Contents {
			if content == nil {
				continue
			}
			for _, part := range content.Parts {
				if part != nil && part.FunctionResponse != nil && part.FunctionResponse.Name == "read_file" {
					m.sawResult = true
				}
			}
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		if call == 1 {
			yield(&model.LLMResponse{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
					ID: "child-read-1", Name: "read_file", Args: map[string]any{"project": "workspace", "path": "main.go"},
				}}}},
				FinishReason: genai.FinishReasonStop,
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content:      genai.NewContentFromText("evidence: main.go defines main()", genai.RoleModel),
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

func exploreDefinition() agentdef.AgentDef {
	return agentdef.AgentDef{
		Name:            "explore",
		Description:     "Explores registered projects and returns read-only evidence.",
		Instruction:     "Investigate using registered read-only tools.",
		IncludeContents: "none",
		ToolScope:       "invocation_scoped",
	}
}

func toolNames(t *testing.T, raw []any) []string {
	t.Helper()
	names := make([]string, 0, len(raw))
	for index, candidate := range raw {
		named, ok := candidate.(interface{ Name() string })
		if !ok {
			t.Fatalf("tool %d does not expose a name: %T", index, candidate)
		}
		names = append(names, named.Name())
	}
	return names
}

func TestCompositeFactoryReturnsAgentToolsPlusDirectTools(t *testing.T) {
	base := &fakeBaseFactory{}
	factory := newCompositeAgentToolFactory(base, []preparedAgentTool{
		{definition: exploreDefinition(), model: &exploringChildModel{}},
	}, nil, "Global policy.")

	raw, err := factory.ToolsForInvocation("U777", domain.ConversationKey("slack:T1:dm:D1"))
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(t, raw)
	want := []string{"explore", "list_repos", "read_file"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tool names = %v, want %v", names, want)
	}
	if base.lastActor != "U777" || base.lastKey != domain.ConversationKey("slack:T1:dm:D1") {
		t.Fatalf("base factory received actor %q key %q", base.lastActor, base.lastKey)
	}
}

func TestCompositeFactoryPropagatesBaseFactoryError(t *testing.T) {
	baseErr := errors.New("base tool construction failed")
	factory := newCompositeAgentToolFactory(&fakeBaseFactory{err: baseErr}, []preparedAgentTool{
		{definition: exploreDefinition(), model: &exploringChildModel{}},
	}, nil, "Global policy.")

	raw, err := factory.ToolsForInvocation("U777", domain.ConversationKey("slack:T1:dm:D1"))
	if !errors.Is(err, baseErr) {
		t.Fatalf("error = %v, want %v", err, baseErr)
	}
	if raw != nil {
		t.Fatalf("partial tool list returned alongside error: %v", toolNames(t, raw))
	}
}

type nonToolBaseFactory struct{}

func (nonToolBaseFactory) ToolsForInvocation(string, domain.ConversationKey) ([]any, error) {
	return []any{"not-an-adk-tool"}, nil
}

func TestCompositeFactoryFailsInsteadOfDroppingConfiguredChild(t *testing.T) {
	factory := newCompositeAgentToolFactory(nonToolBaseFactory{}, []preparedAgentTool{
		{definition: exploreDefinition(), model: &exploringChildModel{}},
	}, nil, "Global policy.")

	raw, err := factory.ToolsForInvocation("U777", domain.ConversationKey("slack:T1:dm:D1"))
	if err == nil || !strings.Contains(err.Error(), "is not an ADK tool") {
		t.Fatalf("error = %v, want invalid dynamic composition failure", err)
	}
	if raw != nil {
		t.Fatalf("partial tool list returned alongside error: %v", toolNames(t, raw))
	}
}

func TestCompositeFactoryKeepsCLIChildrenToolLess(t *testing.T) {
	cliChildModel := &streamRecordingModel{}
	cliChild, err := newAgentToolAgent(agentdef.AgentDef{
		Name:            "opencode_worker",
		Description:     "Handles delegated coding tasks.",
		Instruction:     "Complete the delegated task.",
		IncludeContents: "none",
	}, "Global policy.", cliChildModel, nil)
	if err != nil {
		t.Fatal(err)
	}
	factory := newCompositeAgentToolFactory(&fakeBaseFactory{}, []preparedAgentTool{
		{definition: exploreDefinition(), model: &exploringChildModel{}},
		{
			definition: agentdef.AgentDef{Name: "opencode_worker"},
			model:      cliChildModel,
			cliTool:    agenttool.New(cliChild, &agenttool.Config{}),
		},
	}, nil, "Global policy.")

	raw, err := factory.ToolsForInvocation("U777", domain.ConversationKey("slack:T1:dm:D1"))
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(t, raw)
	want := []string{"explore", "opencode_worker", "list_repos", "read_file"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tool names = %v, want %v", names, want)
	}

	root, err := llmagent.New(llmagent.Config{
		Name:        "root_agent",
		Model:       &delegatingRootModel{target: "opencode_worker"},
		Instruction: "Delegate coding tasks.",
		Mode:        llmagent.ModeChat,
		Tools:       rawAsTools(t, raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	if final := runDelegatingTurn(t, root); final != "root used delegated result" {
		t.Fatalf("final response = %q", final)
	}
	if !strings.Contains(cliChildModel.system, "Global policy.") {
		t.Fatalf("CLI child system = %q", cliChildModel.system)
	}
	if strings.Contains(cliChildModel.system, "read_file") || strings.Contains(cliChildModel.system, "list_repos") {
		t.Fatalf("CLI child received ADK tool declarations:\n%s", cliChildModel.system)
	}
}

func TestExploreChildRunsScopedReadOnlyToolLoop(t *testing.T) {
	base := &fakeBaseFactory{readOutput: "package main"}
	childModel := &exploringChildModel{}
	factory := newCompositeAgentToolFactory(base, []preparedAgentTool{
		{definition: exploreDefinition(), model: childModel},
	}, nil, "Global policy.")

	raw, err := factory.ToolsForInvocation("U777", domain.ConversationKey("slack:T1:dm:D1"))
	if err != nil {
		t.Fatal(err)
	}
	rootModel := &delegatingRootModel{target: "explore"}
	root, err := llmagent.New(llmagent.Config{
		Name:        "root_agent",
		Model:       rootModel,
		Instruction: "Delegate exploration.",
		Mode:        llmagent.ModeChat,
		Tools:       rawAsTools(t, raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	if final := runDelegatingTurn(t, root); final != "root used delegated result" {
		t.Fatalf("final response = %q", final)
	}
	if childModel.calls != 2 {
		t.Fatalf("child model calls = %d, want 2", childModel.calls)
	}
	if childModel.stream {
		t.Fatal("child model received a streaming request despite AgentTool SSE mode")
	}
	if !childModel.sawResult {
		t.Fatal("child model never consumed the read_file FunctionResponse")
	}
	if !strings.Contains(childModel.system, "Global policy.") {
		t.Fatalf("child system instruction missing root global instruction:\n%s", childModel.system)
	}
	if len(base.readCalls) != 1 || base.readCalls[0] != "child-read-1" {
		t.Fatalf("read_file call IDs = %v", base.readCalls)
	}
	if len(base.readActor) != 1 || base.readActor[0] != "U777" {
		t.Fatalf("read_file actors = %v, want trusted Slack actor", base.readActor)
	}
	if rootModel.calls != 2 {
		t.Fatalf("root calls = %d, want 2", rootModel.calls)
	}
}

func rawAsTools(t *testing.T, raw []any) []tool.Tool {
	t.Helper()
	tools := make([]tool.Tool, 0, len(raw))
	for index, candidate := range raw {
		adkTool, ok := candidate.(tool.Tool)
		if !ok {
			t.Fatalf("tool %d is not an ADK tool: %T", index, candidate)
		}
		tools = append(tools, adkTool)
	}
	return tools
}

func runDelegatingTurn(t *testing.T, root agent.Agent) string {
	t.Helper()
	sessions := session.InMemoryService()
	created, err := sessions.Create(t.Context(), &session.CreateRequest{AppName: "agent-tool-test", UserID: "U123"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runner.New(runner.Config{AppName: "agent-tool-test", Agent: root, SessionService: sessions})
	if err != nil {
		t.Fatal(err)
	}
	var final string
	for event, runErr := range run.Run(t.Context(), "U123", created.Session.ID(), genai.NewContentFromText("inspect", genai.RoleUser), agent.RunConfig{StreamingMode: agent.StreamingModeNone}) {
		if runErr != nil {
			t.Fatal(runErr)
		}
		if event.IsFinalResponse() && event.Content != nil {
			for _, part := range event.Content.Parts {
				if part != nil {
					final += part.Text
				}
			}
		}
	}
	return final
}

func TestACPAgentToolResolvesRegisteredProjectsAndInvokesRuntime(t *testing.T) {
	primary := filepath.Join(t.TempDir(), "primary")
	additional := filepath.Join(t.TempDir(), "additional")
	for _, path := range []string{primary, additional} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runtime := &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: "completed"}}
	resolved := &agentdef.ResolvedModel{
		Provider:             agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeACP},
		ConfigOptions:        []agentdef.ACPConfigOption{{ID: "model", Value: "test/model"}},
		PermissionOptionKind: domain.ACPPermissionAllowOnce,
	}
	result, err := invokeACPAgent(t.Context(), agentdef.AgentDef{Name: "opencode_worker", Description: "Runs OpenCode.", Instruction: "Do work."}, "Global.", runtime, resolved, map[string]string{"primary": primary, "additional": additional}, time.Minute, acpAgentArgs{Project: "primary", Task: "change code", AdditionalProjects: []string{"additional"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != "completed" || runtime.request.PrimaryProject != "primary" || len(runtime.request.AdditionalPaths) != 1 {
		t.Fatalf("result = %v, request = %+v", result, runtime.request)
	}
	if runtime.request.PermissionOptionKind != domain.ACPPermissionAllowOnce || runtime.request.Task != "change code" {
		t.Fatalf("request = %+v", runtime.request)
	}
}

func TestResolveACPProjectsRejectsDuplicateAndUnknownNames(t *testing.T) {
	root := t.TempDir()
	projects := map[string]string{"workspace": root}
	if _, _, err := resolveACPProjects(projects, "workspace", []string{"workspace"}); err == nil {
		t.Fatal("expected duplicate rejection")
	}
	if _, _, err := resolveACPProjects(projects, "missing", nil); err == nil {
		t.Fatal("expected unknown project rejection")
	}
}

func TestACPAgentToolRequiresADKConfirmationBeforeRuntime(t *testing.T) {
	runtime := &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: "should not run"}}
	resolved := &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeACP}, PermissionOptionKind: domain.ACPPermissionAllowOnce}
	toolValue, err := newAcpAgentTool(agentdef.AgentDef{Name: "opencode_worker", Description: "Runs OpenCode.", Instruction: "Do work."}, "Global.", runtime, resolved, map[string]string{"workspace": t.TempDir()}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	root, err := llmagent.New(llmagent.Config{Name: "root_agent", Model: &acpCallingRootModel{}, Instruction: "Delegate.", Mode: llmagent.ModeChat, Tools: []tool.Tool{toolValue}})
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.InMemoryService()
	created, err := sessions.Create(t.Context(), &session.CreateRequest{AppName: "acp-confirmation-test", UserID: "U123"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runner.New(runner.Config{AppName: "acp-confirmation-test", Agent: root, SessionService: sessions})
	if err != nil {
		t.Fatal(err)
	}
	foundConfirmation := false
	for event, runErr := range run.Run(t.Context(), "U123", created.Session.ID(), genai.NewContentFromText("use OpenCode", genai.RoleUser), agent.RunConfig{StreamingMode: agent.StreamingModeNone}) {
		if runErr != nil {
			t.Fatal(runErr)
		}
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part != nil && part.FunctionCall != nil && part.FunctionCall.Name == "adk_request_confirmation" {
				foundConfirmation = true
			}
		}
	}
	if !foundConfirmation || runtime.runs != 0 {
		t.Fatalf("confirmation=%v runtime runs=%d", foundConfirmation, runtime.runs)
	}
}
