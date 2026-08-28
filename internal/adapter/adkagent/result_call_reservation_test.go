package adkagent

import (
	"context"
	"iter"
	"sync"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type reservationTestTool struct{ name string }

func (t reservationTestTool) Name() string      { return t.name }
func (reservationTestTool) Description() string { return "test tool" }
func (reservationTestTool) IsLongRunning() bool { return false }

var _ tool.Tool = reservationTestTool{}

func TestResultCallReservationLimitsOnlyProducerToolsPerModelStep(t *testing.T) {
	producer := reservationTestTool{name: "agentcli"}
	reader := reservationTestTool{name: "workstream_get"}
	reservation := newResultCallReservation([]tool.Tool{producer, reader}, map[string]struct{}{"agentcli": {}}, 1)
	if reservation == nil {
		t.Fatal("reservation was not created")
	}
	if _, err := reservation.BeforeTool(nil, reader, nil); err != nil {
		t.Fatalf("read-only tool = %v", err)
	}
	if _, err := reservation.BeforeTool(nil, producer, nil); err != nil {
		t.Fatalf("first producer = %v", err)
	}
	if _, err := reservation.BeforeTool(nil, producer, nil); err == nil {
		t.Fatal("second producer was accepted in one model step")
	}
	reservation.Reset()
	if _, err := reservation.BeforeTool(nil, producer, nil); err != nil {
		t.Fatalf("producer after next model step = %v", err)
	}
}

func TestReserveResultCallTokensReducesEveryAdmissionLimit(t *testing.T) {
	reserved, err := reserveResultCallTokens(domain.RequestBudget{WindowTokens: 8_000, HardTokens: 6_000, TriggerTokens: 5_000, TargetTokens: 4_000}, 2_048)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.HardTokens != 3_952 || reserved.TriggerTokens != 2_952 || reserved.TargetTokens != 1_952 {
		t.Fatalf("reserved budget = %#v", reserved)
	}
	if _, err := reserveResultCallTokens(domain.RequestBudget{HardTokens: 2_048}, 2_048); err == nil {
		t.Fatal("reserve equal to hard limit was accepted")
	}
	if _, err := reserveResultCallTokens(domain.RequestBudget{HardTokens: 6_000, TriggerTokens: 2_048, TargetTokens: 1_000}, 2_048); err == nil {
		t.Fatal("reserve exhausting an explicit admission limit was accepted")
	}
}

type reservationToolContext struct {
	*agent.ContextMock
	callID string
}

func (c reservationToolContext) FunctionCallID() string { return c.callID }

func TestResultCallReservationSelectsFirstProducerBeforeConcurrentCallbacks(t *testing.T) {
	first := reservationTestTool{name: "producer_first"}
	second := reservationTestTool{name: "producer_second"}
	reservation := newResultCallReservation([]tool.Tool{first, second}, map[string]struct{}{
		first.Name(): {}, second.Name(): {},
	}, 1)
	response := &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
		{FunctionCall: &genai.FunctionCall{ID: "first-call", Name: first.Name()}},
		{FunctionCall: &genai.FunctionCall{ID: "second-call", Name: second.Name()}},
	}}}
	if _, err := reservation.AfterModel(nil, response, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := reservation.BeforeTool(reservationToolContext{ContextMock: &agent.ContextMock{}, callID: "second-call"}, second, nil); err == nil {
		t.Fatal("later producer won by reaching BeforeTool first")
	}
	if _, err := reservation.BeforeTool(reservationToolContext{ContextMock: &agent.ContextMock{}, callID: "first-call"}, first, nil); err != nil {
		t.Fatalf("first producer = %v", err)
	}
}

type reservationPassCompiler struct {
	mu      sync.Mutex
	budgets []domain.RequestBudget
}

func (c *reservationPassCompiler) Compile(_ context.Context, req domain.CompileRequest) (domain.CompileResult, error) {
	c.mu.Lock()
	c.budgets = append(c.budgets, req.ModelBudget)
	c.mu.Unlock()
	return domain.CompileResult{Contents: req.Contents}, nil
}

type parallelProducerModel struct {
	mu    sync.Mutex
	calls int
}

func (*parallelProducerModel) Name() string { return "parallel-producer-model" }

func (m *parallelProducerModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		m.calls++
		call := m.calls
		m.mu.Unlock()
		if call == 1 {
			yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "first-call", Name: "producer_first", Args: map[string]any{}}},
				{FunctionCall: &genai.FunctionCall{ID: "second-call", Name: "producer_second", Args: map[string]any{}}},
			}}, TurnComplete: true}, nil)
			return
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText("complete", genai.RoleModel), TurnComplete: true}, nil)
	}
}

func TestRuntimeReservesEveryLimitAndExecutesFirstParallelProducer(t *testing.T) {
	var (
		runsMu sync.Mutex
		runs   []string
	)
	newProducer := func(name string) tool.Tool {
		created, err := functiontool.New(functiontool.Config{Name: name, Description: name}, func(agent.Context, struct{}) (map[string]any, error) {
			runsMu.Lock()
			runs = append(runs, name)
			runsMu.Unlock()
			return map[string]any{"ok": true}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	compiler := &reservationPassCompiler{}
	runtime, err := NewRuntime(RuntimeConfig{
		AgentName: "reservation-test", Model: &parallelProducerModel{}, SessionService: session.InMemoryService(),
		StaticTools:              []tool.Tool{newProducer("producer_first"), newProducer("producer_second")},
		ContextCompiler:          compiler,
		ContextBudget:            domain.RequestBudget{WindowTokens: 8_000, HardTokens: 6_000, TriggerTokens: 5_000, TargetTokens: 4_000},
		ResultProducingToolNames: []string{"producer_first", "producer_second"}, ResultProducingCallsPerStep: 1, ResultProducingCallReserveTokens: 2_048,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey: "slack:T12345678:dm:D12345678",
		Messages:        []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: "run one producer"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Text != "complete" {
		t.Fatalf("turn = %#v", turn)
	}
	runsMu.Lock()
	defer runsMu.Unlock()
	if len(runs) != 1 || runs[0] != "producer_first" {
		t.Fatalf("producer runs = %v, want first only", runs)
	}
	compiler.mu.Lock()
	defer compiler.mu.Unlock()
	if len(compiler.budgets) != 2 {
		t.Fatalf("compiled budgets = %v, want one per model step", compiler.budgets)
	}
	for _, budget := range compiler.budgets {
		if budget.HardTokens != 3_952 || budget.TriggerTokens != 2_952 || budget.TargetTokens != 1_952 {
			t.Fatalf("reserved budget = %#v", budget)
		}
	}
}
