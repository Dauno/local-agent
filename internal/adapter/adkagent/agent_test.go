package adkagent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestBaseInstructionMatchesMVPContract(t *testing.T) {
	t.Parallel()

	want := "You are Dev Agent, a Slack conversational assistant. Answer concisely by default. You currently have no access to shell commands, local files, repositories, secrets, external tools, or autonomous background tasks. If users ask for unsupported actions, explain the limitation instead of pretending to perform the action. If users paste secrets or sensitive values, avoid repeating them unnecessarily."
	if got := BaseInstruction("Dev Agent"); got != want {
		t.Fatalf("BaseInstruction()\n got: %q\nwant: %q", got, want)
	}
}

func TestRespondPreloadsHistoryAndUsesCurrentUserTurn(t *testing.T) {
	t.Parallel()

	llm := &fakeLLM{response: func(*model.LLMRequest) string { return "assistant response" }}
	agent, err := New("Dev {Agent}", llm)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	messages := []domain.Message{
		{Role: domain.RoleUser, Content: "old question"},
		{Role: domain.RoleAssistant, Content: "old answer"},
		{Role: domain.RoleUser, Content: "new question"},
	}

	got, err := agent.Respond(context.Background(), messages)
	if err != nil {
		t.Fatalf("Respond() error = %v", err)
	}
	if got != "assistant response" {
		t.Fatalf("Respond() = %q", got)
	}

	requests := llm.recorded()
	if len(requests) != 1 {
		t.Fatalf("model request count = %d, want 1", len(requests))
	}
	request := requests[0]
	if request.stream {
		t.Fatal("ADK requested streaming")
	}
	if request.model != "fake-model" {
		t.Fatalf("request model = %q", request.model)
	}
	if len(request.tools) != 0 {
		t.Fatalf("ADK agent installed unexpected tools: %#v", request.tools)
	}
	wantContents := []contentView{
		{role: genai.RoleUser, text: "old question"},
		{role: genai.RoleModel, text: "old answer"},
		{role: genai.RoleUser, text: "new question"},
	}
	if fmt.Sprint(request.contents) != fmt.Sprint(wantContents) {
		t.Fatalf("request contents = %#v, want %#v", request.contents, wantContents)
	}
	if !strings.Contains(request.systemInstruction, BaseInstruction("Dev {Agent}")) {
		t.Fatalf("system instruction does not include base instruction:\n%s", request.systemInstruction)
	}
}

func TestRespondUsesAnIndependentEphemeralSessionPerCall(t *testing.T) {
	t.Parallel()

	llm := &fakeLLM{response: func(request *model.LLMRequest) string {
		last := request.Contents[len(request.Contents)-1]
		return "reply:" + last.Parts[0].Text
	}}
	agent, err := New("Dev Agent", llm)
	if err != nil {
		t.Fatal(err)
	}

	const calls = 8
	var wait sync.WaitGroup
	errorsFound := make(chan error, calls)
	for index := 0; index < calls; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			prompt := fmt.Sprintf("question-%d", index)
			got, respondErr := agent.Respond(context.Background(), []domain.Message{{Role: domain.RoleUser, Content: prompt}})
			if respondErr != nil {
				errorsFound <- respondErr
				return
			}
			if got != "reply:"+prompt {
				errorsFound <- fmt.Errorf("Respond() = %q for %q", got, prompt)
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}

	requests := llm.recorded()
	if len(requests) != calls {
		t.Fatalf("model request count = %d, want %d", len(requests), calls)
	}
	for _, request := range requests {
		if len(request.contents) != 1 {
			t.Fatalf("ephemeral request leaked history: %#v", request.contents)
		}
	}
}

func TestRespondPropagatesModelErrorsAndRejectsEmptyResponses(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("model failed")
	failing, err := New("Dev Agent", &fakeLLM{err: sentinel})
	if err != nil {
		t.Fatal(err)
	}
	_, err = failing.Respond(context.Background(), []domain.Message{{Role: domain.RoleUser, Content: "hello"}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Respond() error = %v, want wrapped sentinel", err)
	}

	empty, err := New("Dev Agent", &fakeLLM{response: func(*model.LLMRequest) string { return "  " }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = empty.Respond(context.Background(), []domain.Message{{Role: domain.RoleUser, Content: "hello"}})
	if !errors.Is(err, ErrNoResponse) {
		t.Fatalf("Respond() error = %v, want ErrNoResponse", err)
	}
}

func TestNewAndRespondValidateInputsBeforeModelCall(t *testing.T) {
	t.Parallel()

	if _, err := New("", &fakeLLM{}); err == nil {
		t.Fatal("New() accepted an empty name")
	}
	if _, err := New("Dev\nAgent", &fakeLLM{}); err == nil {
		t.Fatal("New() accepted a multiline name")
	}
	if _, err := New("Dev Agent", nil); err == nil {
		t.Fatal("New() accepted a nil model")
	}

	llm := &fakeLLM{}
	agent, err := New("Dev Agent", llm)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		messages []domain.Message
	}{
		{name: "empty"},
		{name: "blank content", messages: []domain.Message{{Role: domain.RoleUser, Content: " "}}},
		{name: "unsupported role", messages: []domain.Message{{Role: domain.Role("system"), Content: "instruction"}, {Role: domain.RoleUser, Content: "hello"}}},
		{name: "assistant final", messages: []domain.Message{{Role: domain.RoleAssistant, Content: "answer"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := agent.Respond(context.Background(), tt.messages); !errors.Is(err, ErrInvalidHistory) {
				t.Fatalf("Respond() error = %v, want ErrInvalidHistory", err)
			}
		})
	}
	if len(llm.recorded()) != 0 {
		t.Fatal("invalid history invoked the model")
	}
}

type fakeLLM struct {
	mu       sync.Mutex
	requests []requestView
	response func(*model.LLMRequest) string
	err      error
}

func (*fakeLLM) Name() string { return "fake-model" }

func (f *fakeLLM) GenerateContent(_ context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.mu.Lock()
		f.requests = append(f.requests, viewRequest(request, stream))
		f.mu.Unlock()
		if f.err != nil {
			yield(nil, f.err)
			return
		}
		text := "response"
		if f.response != nil {
			text = f.response(request)
		}
		yield(&model.LLMResponse{
			Content:      genai.NewContentFromText(text, genai.RoleModel),
			TurnComplete: true,
		}, nil)
	}
}

func (f *fakeLLM) recorded() []requestView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]requestView(nil), f.requests...)
}

type requestView struct {
	model             string
	stream            bool
	contents          []contentView
	systemInstruction string
	tools             map[string]any
}

type contentView struct {
	role string
	text string
}

func viewRequest(request *model.LLMRequest, stream bool) requestView {
	view := requestView{model: request.Model, stream: stream, tools: request.Tools}
	for _, content := range request.Contents {
		view.contents = append(view.contents, contentView{role: content.Role, text: partsText(content)})
	}
	if request.Config != nil {
		view.systemInstruction = partsText(request.Config.SystemInstruction)
	}
	return view
}

func partsText(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var result strings.Builder
	for _, part := range content.Parts {
		if part != nil {
			result.WriteString(part.Text)
		}
	}
	return result.String()
}
