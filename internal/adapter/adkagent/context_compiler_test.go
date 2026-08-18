package adkagent

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type rejectingContextCompiler struct{}

func (rejectingContextCompiler) Compile(context.Context, domain.CompileRequest) (domain.CompileResult, error) {
	return domain.CompileResult{}, errors.New("frame rejected")
}

type recordingContextFrameCompiler struct {
	frameCalls  int
	legacyCalls int
	count       port.TokenCount
}

func (c *recordingContextFrameCompiler) Compile(_ context.Context, req domain.CompileRequest) (domain.CompileResult, error) {
	c.legacyCalls++
	return domain.CompileResult{Contents: req.Contents}, nil
}

func (c *recordingContextFrameCompiler) CompileFrame(ctx context.Context, req domain.CompileRequest, counter port.ContextFrameCounter) (domain.CompileResult, error) {
	c.frameCalls++
	count, err := counter.CountContextFrame(ctx, req.Contents)
	if err != nil {
		return domain.CompileResult{}, err
	}
	c.count = count
	return domain.CompileResult{Contents: req.Contents}, nil
}

type recordingFrameModel struct {
	stream   bool
	contents []*genai.Content
}

func (*recordingFrameModel) Name() string { return "recording-frame-model" }

func (*recordingFrameModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}

func (m *recordingFrameModel) CountLLMRequest(_ context.Context, request *model.LLMRequest, stream bool) (port.TokenCount, error) {
	m.stream = stream
	m.contents = append([]*genai.Content(nil), request.Contents...)
	return port.TokenCount{Tokens: 3, Strategy: "provider", Exact: true}, nil
}

func TestCompilerBeforeModelCallbackUsesProviderFrameCounter(t *testing.T) {
	compiler := &recordingContextFrameCompiler{}
	frameModel := &recordingFrameModel{}
	callback := CompilerBeforeModelCallback(CompilerCallbackConfig{
		Compiler: compiler, RequestModel: frameModel, Stream: true,
		Budget: domain.RequestBudget{HardTokens: 10}, Actor: "U12345678",
	})
	request := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("current input", genai.RoleUser)}}
	if _, err := callback(nil, request); err != nil {
		t.Fatal(err)
	}
	if compiler.frameCalls != 1 || compiler.legacyCalls != 0 {
		t.Fatalf("compiler calls = frame %d, legacy %d", compiler.frameCalls, compiler.legacyCalls)
	}
	if compiler.count != (port.TokenCount{Tokens: 3, Strategy: "provider", Exact: true}) || !frameModel.stream {
		t.Fatalf("frame count = %#v, stream = %v", compiler.count, frameModel.stream)
	}
	if !reflect.DeepEqual(frameModel.contents, request.Contents) {
		t.Fatalf("counted contents = %#v, request contents = %#v", frameModel.contents, request.Contents)
	}
}

func TestRuntimeRejectsActivationBeforeModelBoundaryWhenFrameCompilationFails(t *testing.T) {
	model := &originContextLLM{}
	runtime, err := NewRuntime(RuntimeConfig{
		AgentName: "Dev Agent", Model: model, SessionService: session.InMemoryService(), ContextCompiler: rejectingContextCompiler{},
	})
	if err != nil {
		t.Fatal(err)
	}
	started := 0
	_, err = runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey: "slack:T12345678:dm:D12345678",
		Origin:          port.AgentTurnOrigin{Kind: port.AgentTurnOriginJobCompletion, Actor: "U12345678", ActivationID: "activation-frame-reject"},
		Messages: []domain.Message{{
			Role: domain.RoleUser, Source: domain.MessageSourceJobCompletion, Content: "bounded frame", UserID: "U12345678",
		}},
		BeforeModel: func(context.Context) error {
			started++
			return nil
		},
	})
	if err == nil {
		t.Fatalf("frame compilation error = %v", err)
	}
	if started != 0 || model.calls != 0 {
		t.Fatalf("rejected frame crossed model boundary: started=%d calls=%d", started, model.calls)
	}
}
