package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	"github.com/Dauno/slack-local-agent/internal/tooldef"
	contextcompilerusecase "github.com/Dauno/slack-local-agent/internal/usecase/contextcompiler"
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
	withCanvas bool
}

var _ port.AgentToolFactory = (*fakeBaseFactory)(nil)
var _ port.ActivationAgentToolFactory = (*fakeBaseFactory)(nil)

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
	result := []any{listRepos, readFile}
	if f.withCanvas {
		createCanvas, err := functiontool.New(functiontool.Config{
			Name: "create_canvas", RequireConfirmation: true,
		}, func(agent.Context, struct{}) (map[string]any, error) {
			return map[string]any{"canvas_id": "F123"}, nil
		})
		if err != nil {
			return nil, err
		}
		result = append(result, createCanvas)
	}
	return result, nil
}

func (*fakeBaseFactory) ToolsForActivation(string, domain.ConversationKey, domain.ExternalAgentJobActivation) ([]any, error) {
	status, err := functiontool.New(functiontool.Config{Name: "job_status"}, func(agent.Context, struct{}) (map[string]any, error) {
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	chunk, err := functiontool.New(functiontool.Config{Name: "read_job_result_chunk"}, func(agent.Context, struct{}) (map[string]any, error) {
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return []any{status, chunk}, nil
}

// exploringChildModel drives a two-step child trajectory: call read_file, then
// consume the FunctionResponse and return final evidence.
type exploringChildModel struct {
	calls     int
	stream    bool
	sawResult bool
	system    string
	sawCanvas bool
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
	if request != nil && request.Config != nil {
		for _, configuredTool := range request.Config.Tools {
			for _, declaration := range configuredTool.FunctionDeclarations {
				if declaration.Name == "create_canvas" {
					m.sawCanvas = true
				}
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
		ToolScope:       agentdef.ToolScope{"invocation_scoped"},
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

func TestCompositeActivationScopeExcludesCollidingChildAgent(t *testing.T) {
	factory := newCompositeAgentToolFactory(&fakeBaseFactory{}, []preparedAgentTool{
		{definition: agentdef.AgentDef{Name: "read_job_result_chunk"}, model: &exploringChildModel{}},
	}, nil, "Global policy.")
	activation := domain.ExternalAgentJobActivation{
		ActivationID: "activation-1", JobID: "job-1", StatusRevision: 1,
		TerminalStatus: domain.JobCompleted, Actor: "U777", ConversationKey: "slack:T1:dm:D1",
	}
	raw, err := factory.ToolsForActivation(activation.Actor, activation.ConversationKey, activation)
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(t, raw)
	if strings.Join(names, ",") != "job_status,read_job_result_chunk" {
		t.Fatalf("activation tools = %v", names)
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
	base := &fakeBaseFactory{readOutput: "package main", withCanvas: true}
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
	if childModel.sawCanvas {
		t.Fatal("scoped child received mutable create_canvas tool")
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

func TestExploreChildContextCompilerRejectsRepeatedOversizedToolResponsesWithoutWrites(t *testing.T) {
	const maxRequestBytes = 30_000
	run := func(t *testing.T, compiler port.ContextCompiler) *largeExploringChildModel {
		t.Helper()
		base := &fakeBaseFactory{readOutput: strings.Repeat("x", 18_000)}
		childModel := &largeExploringChildModel{maxRequestBytes: maxRequestBytes, reads: 6}
		factory := newCompositeAgentToolFactory(base, []preparedAgentTool{{
			definition:      exploreDefinition(),
			model:           childModel,
			contextCompiler: compiler,
			contextBudget:   domain.RequestBudget{HardTokens: 30_000, TriggerTokens: 22_000, TargetTokens: 20_000},
		}}, nil, "Global policy.")
		raw, err := factory.ToolsForInvocation("U777", domain.ConversationKey("slack:T1:dm:D1"))
		if err != nil {
			t.Fatal(err)
		}
		root, err := llmagent.New(llmagent.Config{
			Name:        "root_agent",
			Model:       &delegatingRootModel{target: "explore"},
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
		return childModel
	}

	withoutCompiler := run(t, nil)
	if !withoutCompiler.oversized {
		t.Fatal("unmanaged child request never exceeded the test bound")
	}

	store := &childContextResultStore{}
	withCompiler := run(t, contextcompilerusecase.New(store, childByteCounter{}))
	if withCompiler.oversized {
		t.Fatalf("managed child request exceeded %d bytes at sizes %v", maxRequestBytes, withCompiler.requestSizes)
	}
	if withCompiler.calls >= 7 {
		t.Fatalf("managed child continued after irreducible context: calls=%d", withCompiler.calls)
	}
	if store.puts != 0 {
		t.Fatalf("context compiler wrote %d late projections", store.puts)
	}
}

type largeExploringChildModel struct {
	calls           int
	reads           int
	maxRequestBytes int
	requestSizes    []int
	oversized       bool
}

func (*largeExploringChildModel) Name() string { return "large-explore-model" }

func (m *largeExploringChildModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.calls++
	encoded, _ := json.Marshal(request.Contents)
	m.requestSizes = append(m.requestSizes, len(encoded))
	if len(encoded) > m.maxRequestBytes {
		m.oversized = true
	}
	call := m.calls
	return func(yield func(*model.LLMResponse, error) bool) {
		if call <= m.reads {
			yield(&model.LLMResponse{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
					ID: fmt.Sprintf("large-child-read-%d", call), Name: "read_file", Args: map[string]any{"project": "workspace", "path": fmt.Sprintf("file-%d.go", call)},
				}}}},
				FinishReason: genai.FinishReasonStop,
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content:      genai.NewContentFromText("bounded exploration evidence", genai.RoleModel),
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

type childByteCounter struct{}

func (childByteCounter) CountRequest(_ context.Context, envelope port.ModelRequestEnvelope) (port.TokenCount, error) {
	return port.TokenCount{Tokens: len(envelope.Serialized), Strategy: "byte_bound"}, nil
}

type childContextResultStore struct {
	puts int
}

func (s *childContextResultStore) Put(_ context.Context, request port.PutResultRequest) (domain.RecoverableResult, error) {
	s.puts++
	return domain.RecoverableResult{Ref: fmt.Sprintf("child-result-%d", s.puts), Kind: request.Kind, SizeBytes: int64(len(request.Content)), CodePoints: utf8.RuneCountInString(request.Content)}, nil
}

func (*childContextResultStore) ReadChunk(context.Context, domain.ResultChunkRequest) (domain.ResultChunk, error) {
	return domain.ResultChunk{}, errors.New("child test result is not readable")
}

func (*childContextResultStore) Stat(context.Context, port.StatResultRequest) (domain.RecoverableResult, error) {
	return domain.RecoverableResult{}, errors.New("child test result is unavailable")
}

func (*childContextResultStore) DeleteExpired(context.Context, time.Time, int) (int, error) {
	return 0, nil
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

func TestResolveACPProjectRejectsUnknownName(t *testing.T) {
	root := t.TempDir()
	projects := map[string]string{"workspace": root}
	if _, err := resolveACPProject(projects, "missing"); err == nil {
		t.Fatal("expected unknown project rejection")
	}
}

// declarativeTestBase is a minimal base factory that exposes declared tools by
// name, mirroring the toolfactory contract used in production.
type declarativeTestBase struct {
	raw   []any
	tools map[string]tooldef.ToolDef
}

func (b *declarativeTestBase) ToolsForInvocation(string, domain.ConversationKey) ([]any, error) {
	return b.raw, nil
}

func (b *declarativeTestBase) DeclarativeToolByName(name string) (tool.Tool, error) {
	if _, ok := b.tools[name]; !ok {
		return nil, fmt.Errorf("declarative tool %q is not registered", name)
	}
	return namedTool{name: name}, nil
}

type namedTool struct{ name string }

func (t namedTool) Name() string        { return t.name }
func (t namedTool) Description() string { return "declared read-only tool" }
func (t namedTool) IsLongRunning() bool { return false }

func TestCompositeChildReceivesDeclaredToolScope(t *testing.T) {
	base := &declarativeTestBase{tools: map[string]tooldef.ToolDef{
		"ripgrep": {Name: "ripgrep"},
	}}
	definition := exploreDefinition()
	definition.ToolScope = agentdef.ToolScope{"invocation_scoped", "ripgrep"}
	factory := newCompositeAgentToolFactory(base, nil, nil, "Global policy.")
	factory.setDeclarativeTools(base.tools)

	fixed := []tool.Tool{namedTool{name: "read_file"}}
	tools, err := factory.childDeclarativeTools(base, definition, fixed)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(tools))
	for _, candidate := range tools {
		names = append(names, candidate.Name())
	}
	want := "read_file,ripgrep"
	if strings.Join(names, ",") != want {
		t.Fatalf("child tools = %v, want %v", names, want)
	}
}

func TestCompositeChildRejectsUndeclaredScopeTool(t *testing.T) {
	base := &declarativeTestBase{tools: map[string]tooldef.ToolDef{
		"ripgrep": {Name: "ripgrep"},
	}}
	definition := exploreDefinition()
	definition.ToolScope = agentdef.ToolScope{"invocation_scoped", "ripgrep"}
	factory := newCompositeAgentToolFactory(base, nil, nil, "Global policy.")
	// The deployment has no active declarative tools.
	factory.setDeclarativeTools(nil)

	if _, err := factory.childDeclarativeTools(base, definition, nil); err == nil || !strings.Contains(err.Error(), "undeclared tool") {
		t.Fatalf("error = %v, want undeclared tool", err)
	}
}

func TestCompositeToolsForInvocationIncludesDeclarativeTools(t *testing.T) {
	// In production the base factory already appends the active declarative
	// tools to its own invocation list; the composite passes them through and
	// additionally indexes them for children and workflow steps.
	base := &declarativeTestBase{
		raw: []any{namedTool{name: "list_messages"}, namedTool{name: "ripgrep"}},
		tools: map[string]tooldef.ToolDef{
			"ripgrep": {Name: "ripgrep"},
		},
	}
	factory := newCompositeAgentToolFactory(base, nil, nil, "Global policy.")
	factory.setDeclarativeTools(base.tools)

	raw, err := factory.ToolsForInvocation("U777", domain.ConversationKey("slack:T1:dm:D1"))
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(t, raw)
	want := []string{"list_messages", "ripgrep"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tool names = %v, want %v", names, want)
	}
}

func TestChildAllowlistIncludesReadOnlyRangeTool(t *testing.T) {
	for _, name := range []string{"list_messages", "list_repos", "list_directory", "read_file", "list_worktrees", "read_file_range"} {
		if !isReadOnlyChildTool(name) {
			t.Fatalf("child allowlist must include %q", name)
		}
	}
	for _, name := range []string{"read_result_chunk", "create_worktree", "ripgrep"} {
		if isReadOnlyChildTool(name) {
			t.Fatalf("child allowlist must not include %q", name)
		}
	}
}
