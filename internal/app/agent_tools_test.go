package app

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/agenttool"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
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
	calls int
}

func (*delegatingRootModel) Name() string { return "root" }

func (m *delegatingRootModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.calls++
	call := m.calls
	return func(yield func(*model.LLMResponse, error) bool) {
		if call == 1 {
			yield(&model.LLMResponse{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
					ID: "delegate-1", Name: "opencode_worker", Args: map[string]any{"request": "inspect the repository"},
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
		t.Fatal("AgentTool SSE mode reached text-only agent_cli model")
	}
}

func TestNewAgentToolAgentUsesDefinition(t *testing.T) {
	definition := agentdef.AgentDef{
		Name:            "opencode_worker",
		Description:     "Handles delegated coding tasks.",
		Instruction:     "Return a concise result.",
		IncludeContents: "none",
	}
	child, err := newAgentToolAgent(definition, "Global policy.", &streamRecordingModel{})
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
	}, "Global policy.", childModel)
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
