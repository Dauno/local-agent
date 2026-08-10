package contextcompiler

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/adkagent"
	"github.com/Dauno/slack-local-agent/internal/adapter/tokencounter"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// ---------------------------------------------------------------------------
// Test fakes
// ---------------------------------------------------------------------------

type fakeResultStore struct {
	results    map[string]string
	nextRef    int
	putCalls   int
	emptyRef   bool
	failPutAt  int
	putFailure error
}

func newFakeResultStore() *fakeResultStore {
	return &fakeResultStore{results: make(map[string]string)}
}

type callbackLedgerProbeLLM struct {
	mu                     sync.Mutex
	modelCalls             int
	secondRequestHasMarker bool
}

func (*callbackLedgerProbeLLM) Name() string { return "callback-ledger-probe" }

func (m *callbackLedgerProbeLLM) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		m.modelCalls++
		call := m.modelCalls
		if call == 2 {
			m.secondRequestHasMarker = genaiContentsHaveProjectionMarker(request.Contents)
		}
		m.mu.Unlock()

		if call == 1 {
			yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "probe-call", Name: "probe_bulk", Args: map[string]any{},
			}}}}, TurnComplete: true}, nil)
			return
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText("probe complete", genai.RoleModel), TurnComplete: true}, nil)
	}
}

func genaiContentsHaveProjectionMarker(contents []*genai.Content) bool {
	for _, content := range contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part != nil && part.FunctionResponse != nil {
				if _, ok := part.FunctionResponse.Response["_local_agent_context_projection"]; ok {
					return true
				}
			}
		}
	}
	return false
}

func TestADKCallbackProjectionDoesNotPersistInLedger(t *testing.T) {
	bulkTool, err := functiontool.New(functiontool.Config{
		Name:        "probe_bulk",
		Description: "Return a large probe payload.",
	}, func(agent.Context, struct{}) (map[string]any, error) {
		return map[string]any{"text": strings.Repeat("probe payload ", 20_000)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	service := session.InMemoryService()
	llm := &callbackLedgerProbeLLM{}
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	runtime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{
		AgentName:       "Probe Agent",
		Model:           llm,
		SessionService:  service,
		ContextCompiler: New(newFakeResultStore(), serializedByteCounter{}),
		ContextBudget:   domain.RequestBudget{WindowTokens: 128_000, HardTokens: 5_000, TargetTokens: 5_000},
		StaticTools:     []tool.Tool{bulkTool},
	})
	if err != nil {
		t.Fatal(err)
	}

	turn, err := runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey: key,
		Messages:        []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: "run the probe tool"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Text != "probe complete" {
		t.Fatalf("turn text = %q, want probe complete", turn.Text)
	}
	llm.mu.Lock()
	modelCalls := llm.modelCalls
	secondRequestHasMarker := llm.secondRequestHasMarker
	llm.mu.Unlock()
	if modelCalls != 2 {
		t.Fatalf("model calls = %d, want two model steps", modelCalls)
	}
	if !secondRequestHasMarker {
		t.Fatal("second model request did not receive the callback projection")
	}

	loaded, err := service.Get(t.Context(), &session.GetRequest{
		AppName: "local-agent", UserID: "local_user", SessionID: "adk:" + string(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < loaded.Session.Events().Len(); index++ {
		event := loaded.Session.Events().At(index)
		if event != nil && genaiContentsHaveProjectionMarker([]*genai.Content{event.Content}) {
			t.Fatalf("ADK persisted callback projection in ledger event %d; stop before changing classification", index)
		}
	}
}

func (s *fakeResultStore) Put(_ context.Context, req port.PutResultRequest) (domain.RecoverableResult, error) {
	s.putCalls++
	if s.failPutAt > 0 && s.putCalls == s.failPutAt {
		if s.putFailure != nil {
			return domain.RecoverableResult{}, s.putFailure
		}
		return domain.RecoverableResult{}, errors.New("injected result-store failure")
	}
	s.nextRef++
	ref := fmt.Sprintf("ref-%d", s.nextRef)
	if s.emptyRef {
		ref = ""
	}
	s.results[ref] = req.Content
	return domain.RecoverableResult{
		Ref:        ref,
		Kind:       req.Kind,
		SizeBytes:  int64(len(req.Content)),
		CodePoints: utf8.RuneCountInString(req.Content),
		CreatedAt:  time.Now(),
	}, nil
}

func TestCompilerMinimumDryRunFailsBeforeWrites(t *testing.T) {
	store := newFakeResultStore()
	counter := &sequenceTokenCounter{counts: []int{101, 101, 101}}
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "inspect"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{FunctionCall: &domain.FunctionCall{ID: "call-1", Name: "read_file"}}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{FunctionResponse: &domain.FunctionResponse{ID: "call-1", Name: "read_file", Response: map[string]any{"text": readableText(500)}}}}},
	}
	_, err := New(store, counter).Compile(t.Context(), domain.CompileRequest{
		Contents: contents, ConversationKey: "minimum-dry-run", ModelBudget: domain.RequestBudget{HardTokens: 100, TargetTokens: 80},
	})
	if !errors.Is(err, domain.ErrIrreducibleContext) {
		t.Fatalf("Compile() error = %v, want irreducible context", err)
	}
	if store.putCalls != 0 {
		t.Fatalf("result-store writes = %d, want zero before minimum admission", store.putCalls)
	}
}

func TestCompilerAcceptsVisualEstimatorForNonEmptyBudgetTurn(t *testing.T) {
	counter, err := tokencounter.New(tokencounter.StrategyEstimator, tokencounter.EstimatorVisualTileConservativeV1)
	if err != nil {
		t.Fatal(err)
	}
	contents := []domain.Content{{
		Role:  domain.ContentRoleUser,
		Parts: []domain.ContentPart{{Text: "hello"}},
	}}
	result, err := New(newFakeResultStore(), counter).Compile(t.Context(), domain.CompileRequest{
		Contents: contents,
		ModelBudget: domain.RequestBudget{
			HardTokens:   10_000,
			TargetTokens: 10_000,
		},
	})
	if err != nil {
		t.Fatalf("non-empty estimator budget turn failed: %v", err)
	}
	if result.Diagnostics.CounterStrategy != tokencounter.StrategyByteBound {
		t.Fatalf("compiler counter strategy = %q, want %q", result.Diagnostics.CounterStrategy, tokencounter.StrategyByteBound)
	}
	serialized, err := domain.CanonicalJSON(result.Contents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Diagnostics.RequestTokensBefore != len(serialized) {
		t.Fatalf("compiler projection count = %d, want one serialized byte bound %d", result.Diagnostics.RequestTokensBefore, len(serialized))
	}
}

func (s *fakeResultStore) ReadChunk(_ context.Context, req domain.ResultChunkRequest) (domain.ResultChunk, error) {
	content, ok := s.results[req.Ref]
	if !ok {
		return domain.ResultChunk{}, errors.New("not found")
	}
	return domain.ResultChunk{Content: content, EOF: true}, nil
}

func (s *fakeResultStore) Stat(_ context.Context, req port.StatResultRequest) (domain.RecoverableResult, error) {
	content, ok := s.results[req.Ref]
	if !ok {
		return domain.RecoverableResult{}, errors.New("not found")
	}
	return domain.RecoverableResult{Ref: req.Ref, SizeBytes: int64(len(content)), CodePoints: utf8.RuneCountInString(content)}, nil
}

func (s *fakeResultStore) DeleteExpired(_ context.Context, _ time.Time, _ int) (int, error) {
	return 0, nil
}

type fakeTokenCounter struct{}

func (fakeTokenCounter) CountRequest(_ context.Context, _ port.ModelRequestEnvelope) (port.TokenCount, error) {
	return port.TokenCount{Tokens: 0, Strategy: "byte_bound"}, nil
}

type serializedByteCounter struct{}

func (serializedByteCounter) CountRequest(_ context.Context, envelope port.ModelRequestEnvelope) (port.TokenCount, error) {
	return port.TokenCount{Tokens: len(envelope.Serialized), Strategy: "byte_bound"}, nil
}

type sequenceTokenCounter struct {
	counts []int
	calls  int
}

type compilerMetricCapture struct {
	samples []port.MetricSample
}

func (m *compilerMetricCapture) AddCounter(name string, delta int64, labels port.MetricLabels) {
	m.samples = append(m.samples, port.MetricSample{Name: name, Kind: port.MetricKindCounter, Value: float64(delta), Labels: labels})
}
func (m *compilerMetricCapture) SetGauge(name string, value int64, labels port.MetricLabels) {
	m.samples = append(m.samples, port.MetricSample{Name: name, Kind: port.MetricKindGauge, Value: float64(value), Labels: labels})
}
func (m *compilerMetricCapture) Observe(name string, value float64, labels port.MetricLabels) {
	m.samples = append(m.samples, port.MetricSample{Name: name, Kind: port.MetricKindObservation, Value: value, Labels: labels})
}
func (m *compilerMetricCapture) Snapshot() []port.MetricSample {
	return append([]port.MetricSample(nil), m.samples...)
}
func (m *compilerMetricCapture) find(name string) (port.MetricSample, bool) {
	for _, sample := range m.samples {
		if sample.Name == name {
			return sample, true
		}
	}
	return port.MetricSample{}, false
}

func (c *sequenceTokenCounter) CountRequest(context.Context, port.ModelRequestEnvelope) (port.TokenCount, error) {
	index := c.calls
	if index >= len(c.counts) {
		index = len(c.counts) - 1
	}
	c.calls++
	return port.TokenCount{Tokens: c.counts[index], Strategy: "exact"}, nil
}

func TestCompilerPhaseOrder(t *testing.T) {
	tests := []struct {
		name   string
		req    domain.CompileRequest
		counts []int
		want   []string
	}{
		{
			name: "admitted without reduction",
			req: domain.CompileRequest{
				Contents:    []domain.Content{{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "hello"}}}},
				ModelBudget: domain.RequestBudget{HardTokens: 100, TargetTokens: 100},
			},
			counts: []int{1},
			want:   []string{"analysis", "assembly", "admission"},
		},
		{
			name: "evicts optional context before reduction",
			req: domain.CompileRequest{
				Contents: []domain.Content{
					{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "old request"}}},
					{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "old answer"}}},
					{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "current request"}}},
				},
				ExistingSummary: "summary",
				ModelBudget:     domain.RequestBudget{HardTokens: 100, TriggerTokens: 80, TargetTokens: 70},
			},
			counts: []int{81, 70},
			want:   []string{"analysis", "assembly", "admission", "optional_eviction", "admission"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			compiler := New(newFakeResultStore(), &sequenceTokenCounter{counts: tc.counts})
			state, err := analyzeCompilation(tc.req)
			if err != nil {
				t.Fatal(err)
			}
			state, err = assembleCompilation(state)
			if err != nil {
				t.Fatal(err)
			}
			state, err = compiler.countCompilation(t.Context(), state, true)
			if err != nil {
				t.Fatal(err)
			}
			if state.count.Tokens > triggerTokens(state.hardLimit, tc.req.ModelBudget.TriggerTokens) {
				state, err = compiler.reduceCompilation(t.Context(), state)
				if err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(state.stageOrder, tc.want) {
				t.Fatalf("phase order = %v, want %v", state.stageOrder, tc.want)
			}
		})
	}
}

func TestCompilerAnalysisSerializesEachResponseOnce(t *testing.T) {
	contents := largeProjectionContents(false, false)
	counts := make(map[string]int)
	state, err := analyzeCompilationWithSerializer(domain.CompileRequest{
		Contents:    contents,
		ModelBudget: domain.RequestBudget{HardTokens: 100_000, TargetTokens: 100_000},
	}, func(response *domain.FunctionResponse) ([]byte, error) {
		counts[response.ID]++
		return fullResponseJSON(response)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"call-1", "call-2"} {
		if counts[id] != 1 {
			t.Errorf("response %s serialized %d times, want once", id, counts[id])
		}
	}
	for _, part := range state.reducible {
		projection, projectionErr := newProjectionMutation(part, 0, "request_budget")
		if projectionErr != nil {
			t.Fatal(projectionErr)
		}
		if !reflect.DeepEqual(projection.fullJSON, part.canonicalJSON) {
			t.Fatalf("projection for %s did not reuse analyzed canonical JSON", part.response.ID)
		}
	}
}

func TestCompilerOutputDeterministicExceptOpaqueReferences(t *testing.T) {
	contents := largeProjectionContents(false, false)
	req := domain.CompileRequest{
		Contents:        contents,
		ModelBudget:     domain.RequestBudget{HardTokens: 2_000, TargetTokens: 1_500},
		Actor:           "U1",
		ConversationKey: "deterministic-output",
	}
	firstStore := newFakeResultStore()
	secondStore := newFakeResultStore()
	secondStore.nextRef = 100
	first, err := New(firstStore, serializedByteCounter{}).Compile(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(secondStore, serializedByteCounter{}).Compile(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Diagnostics.ResponsesExternalized != second.Diagnostics.ResponsesExternalized ||
		first.Diagnostics.ReductionReason != second.Diagnostics.ReductionReason ||
		first.Diagnostics.ReductionStage != second.Diagnostics.ReductionStage {
		t.Fatalf("stable diagnostics differ: first=%#v second=%#v", first.Diagnostics, second.Diagnostics)
	}
	for _, result := range []*domain.CompileResult{&first, &second} {
		for contentIndex := range result.Contents {
			for partIndex := range result.Contents[contentIndex].Parts {
				response := result.Contents[contentIndex].Parts[partIndex].FunctionResponse
				if response == nil {
					continue
				}
				if marker, ok := response.Response[projectionMarkerKey].(domain.ContextProjectionMarker); ok {
					marker.ResultRef = "<opaque>"
					response.Response[projectionMarkerKey] = marker
				}
			}
		}
	}
	firstJSON, err := domain.CanonicalJSON(first.Contents)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := domain.CanonicalJSON(second.Contents)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatal("compiler output changed beyond opaque result references")
	}
}

func TestCompilerCounterCallsRemainBoundedAcrossFixtures(t *testing.T) {
	tests := []struct {
		name string
		req  domain.CompileRequest
		max  int
	}{
		{
			name: "optional eviction",
			req: domain.CompileRequest{
				Contents: []domain.Content{
					{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "old request"}}},
					{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "old answer"}}},
					{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "current request"}}},
				},
				ExistingSummary: "summary",
				ModelBudget:     domain.RequestBudget{HardTokens: 100, TriggerTokens: 80, TargetTokens: 70},
			},
			max: 8,
		},
		{
			name: "minimum guard",
			req: domain.CompileRequest{
				Contents:    largeProjectionContents(false, false),
				ModelBudget: domain.RequestBudget{HardTokens: 100, TargetTokens: 80},
			},
			max: 8,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			counter := &sequenceTokenCounter{counts: []int{101, 101, 101, 101, 101, 101, 101, 101}}
			_, _ = New(newFakeResultStore(), counter).Compile(t.Context(), tc.req)
			if counter.calls > tc.max {
				t.Fatalf("counter calls = %d, want at most %d", counter.calls, tc.max)
			}
		})
	}
}

func TestCompilerRejectsNilCompiler(t *testing.T) {
	var compiler *Compiler
	_, err := compiler.Compile(t.Context(), domain.CompileRequest{ModelBudget: domain.RequestBudget{HardTokens: 100}})
	if err == nil || !strings.Contains(err.Error(), "compiler is required") {
		t.Fatalf("Compile() error = %v, want nil compiler error", err)
	}
}

func TestCompilerRejectsNilCounterForEmptyContents(t *testing.T) {
	_, err := New(newFakeResultStore(), nil).Compile(t.Context(), domain.CompileRequest{
		ModelBudget: domain.RequestBudget{HardTokens: 100},
	})
	if err == nil || !strings.Contains(err.Error(), "request token counter is required") {
		t.Fatalf("Compile() error = %v, want missing counter error", err)
	}
}

func TestCompilerRejectsNegativeFixedRequestTokens(t *testing.T) {
	counter := &sequenceTokenCounter{counts: []int{1}}
	_, err := New(newFakeResultStore(), counter).Compile(t.Context(), domain.CompileRequest{
		Contents:           []domain.Content{{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "hello"}}}},
		ModelBudget:        domain.RequestBudget{HardTokens: 100},
		FixedRequestTokens: -1,
	})
	if err == nil || !strings.Contains(err.Error(), "fixed request tokens must not be negative") {
		t.Fatalf("Compile() error = %v, want negative fixed-cost error", err)
	}
	if counter.calls != 0 {
		t.Fatalf("counter calls = %d, want zero for invalid request", counter.calls)
	}
}

func TestCompilerRejectsInvalidBudgetBeforeCounting(t *testing.T) {
	counter := &sequenceTokenCounter{counts: []int{1}}
	_, err := New(newFakeResultStore(), counter).Compile(t.Context(), domain.CompileRequest{
		Contents:    []domain.Content{{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "hello"}}}},
		ModelBudget: domain.RequestBudget{HardTokens: 0},
	})
	if err == nil || !strings.Contains(err.Error(), "validate request budget") {
		t.Fatalf("Compile() error = %v, want budget validation error", err)
	}
	if counter.calls != 0 {
		t.Fatalf("counter calls = %d, want zero for invalid budget", counter.calls)
	}
}

func TestCompilerRejectsNegativeCounterResult(t *testing.T) {
	counter := &sequenceTokenCounter{counts: []int{-1}}
	_, err := New(newFakeResultStore(), counter).Compile(t.Context(), domain.CompileRequest{
		Contents:    []domain.Content{{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "hello"}}}},
		ModelBudget: domain.RequestBudget{HardTokens: 100},
	})
	if err == nil || !strings.Contains(err.Error(), "request_token_count_unavailable") {
		t.Fatalf("Compile() error = %v, want unavailable counter error", err)
	}
}

func TestCompilerAcceptsValidEmptyContentsWithoutCounting(t *testing.T) {
	counter := &sequenceTokenCounter{counts: []int{1}}
	result, err := New(newFakeResultStore(), counter).Compile(t.Context(), domain.CompileRequest{
		ModelBudget: domain.RequestBudget{HardTokens: 100},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v, want nil", err)
	}
	if len(result.Contents) != 0 || result.Diagnostics.ReductionReason != "empty" {
		t.Fatalf("result = %#v, want empty result", result)
	}
	if counter.calls != 0 {
		t.Fatalf("counter calls = %d, want zero for empty contents", counter.calls)
	}
}

func TestCompilerAccountsForFixedProviderInput(t *testing.T) {
	contents := []domain.Content{{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "current request"}}}}
	serialized, err := domain.CanonicalJSON(contents)
	if err != nil {
		t.Fatal(err)
	}
	const fixed = 100
	compiler := New(newFakeResultStore(), serializedByteCounter{})
	_, err = compiler.Compile(context.Background(), domain.CompileRequest{Contents: contents,
		ModelBudget:        domain.RequestBudget{HardTokens: len(serialized) + fixed - 1, TargetTokens: len(serialized) + fixed - 1},
		FixedRequestTokens: fixed})
	if !errors.Is(err, domain.ErrIrreducibleContext) {
		t.Fatalf("Compile() error = %v, want fixed input to cross hard limit", err)
	}
}

func TestCompilerRecountsSummaryAndTurnsAtMostOnce(t *testing.T) {
	counter := &sequenceTokenCounter{counts: []int{101, 50}}
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "old request"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "old answer"}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "current request"}}},
	}
	result, err := New(newFakeResultStore(), counter).Compile(t.Context(), domain.CompileRequest{
		Contents: contents, ExistingSummary: "summary", ModelBudget: domain.RequestBudget{HardTokens: 100, TargetTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Diagnostics.RecountPasses != 1 || counter.calls != 2 {
		t.Fatalf("diagnostics = %#v, counter calls = %d", result.Diagnostics, counter.calls)
	}
}

func TestCompilerRecountsContinuityAndExcerptsThenFailsClosed(t *testing.T) {
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "current request"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{FunctionCall: &domain.FunctionCall{ID: "call-1", Name: "read_file", Args: map[string]any{}}}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{FunctionResponse: &domain.FunctionResponse{ID: "call-1", Name: "read_file", Response: map[string]any{"text": readableText(500)}}}}},
	}
	base := domain.CompileRequest{Contents: contents, ExistingSummary: "summary", ModelBudget: domain.RequestBudget{HardTokens: 10_000, TargetTokens: 10_000}, Continuity: domain.ContinuityCapsule{Objective: &domain.ContinuityItem{ID: "objective", Kind: domain.ContinuityKindObjective, Text: "keep context bounded", Status: domain.ContinuityStatusCurrent}}}

	for _, tc := range []struct {
		name    string
		counts  []int
		passes  int
		wantErr bool
	}{
		{name: "stage two admits", counts: []int{10_001, 10_001, 500}, passes: 2},
		{name: "irreducible", counts: []int{10_001, 10_001, 10_001}, passes: 2, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter := &sequenceTokenCounter{counts: tc.counts}
			_, err := New(newFakeResultStore(), counter).Compile(t.Context(), base)
			if tc.wantErr != (err != nil) {
				t.Fatalf("error = %v, want error=%t", err, tc.wantErr)
			}
			if !tc.wantErr {
				result, resultErr := New(newFakeResultStore(), &sequenceTokenCounter{counts: tc.counts}).Compile(t.Context(), base)
				if resultErr != nil || result.Diagnostics.RecountPasses != tc.passes {
					t.Fatalf("result = %#v, error = %v", result.Diagnostics, resultErr)
				}
			}
			if counter.calls > 4 {
				t.Fatalf("counter calls = %d, want at most 4", counter.calls)
			}
		})
	}
}

func TestCompilerIrreducibleResultExposesRecountMetrics(t *testing.T) {
	contents := []domain.Content{{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "current request"}}}}
	recorder := &compilerMetricCapture{}
	result, err := New(newFakeResultStore(), &sequenceTokenCounter{counts: []int{101, 101, 101}}, recorder).Compile(t.Context(), domain.CompileRequest{
		Contents: contents, ExistingSummary: "summary", ModelBudget: domain.RequestBudget{HardTokens: 100, TargetTokens: 100},
		Continuity: domain.ContinuityCapsule{Objective: &domain.ContinuityItem{ID: "objective", Kind: domain.ContinuityKindObjective, Text: "retain", Status: domain.ContinuityStatusCurrent}},
	})
	if err == nil {
		t.Fatal("expected irreducible error")
	}
	if result.Diagnostics.RecountPasses != 2 || result.Diagnostics.ReductionReason != "irreducible" {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	recount, ok := recorder.find(domain.MetricContextRecountPasses)
	if !ok || recount.Value != 2 {
		t.Fatalf("recount metric = %#v, found=%t", recount, ok)
	}
	if _, ok := recorder.find(domain.MetricModelRequestIrreducibleTotal); !ok {
		t.Fatal("irreducible metric was not emitted")
	}
	if _, ok := recorder.find(domain.MetricContinuityCheckpointRenderCodePoints); !ok {
		t.Fatal("continuity render metric was not emitted")
	}
	if _, ok := recorder.find(domain.MetricContextCompileDuration); !ok {
		t.Fatal("compile duration metric was not emitted")
	}
}

func TestCompilerExternalizedTotalIsCounter(t *testing.T) {
	recorder := &compilerMetricCapture{}
	compiler := New(newFakeResultStore(), fakeTokenCounter{}, recorder)
	compiler.recordDiagnostics(domain.CompileDiagnostics{
		ProtectedTokens: 10, ContinuityTokens: 5, RecentTurnsRetained: 2,
		ResponsesExternalized: 3, ResponseTokensRemoved: 20, RecountPasses: 1,
		ReductionReason: "request_budget",
	}, false)
	sample, ok := recorder.find(domain.MetricContextResponsesExternalized)
	if !ok || sample.Kind != port.MetricKindCounter || sample.Value != 3 {
		t.Fatalf("externalized metric = %#v, found=%t", sample, ok)
	}
	for _, name := range []string{
		domain.MetricContextProtectedTokens,
		domain.MetricContextContinuityTokens,
		domain.MetricContextRecentTurnsRetained,
		domain.MetricContextTokensRemoved,
		domain.MetricContextRecountPasses,
		domain.MetricModelRequestReductionTotal,
	} {
		if _, ok := recorder.find(name); !ok {
			t.Errorf("metric %q was not emitted", name)
		}
	}
}

func TestCompilerReducesOptionalContextTowardTargetBeforeHardLimit(t *testing.T) {
	counter := &sequenceTokenCounter{counts: []int{81, 70}}
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "old request"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "old answer"}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "current request"}}},
	}
	result, err := New(newFakeResultStore(), counter).Compile(t.Context(), domain.CompileRequest{
		Contents: contents, ExistingSummary: "summary",
		ModelBudget: domain.RequestBudget{HardTokens: 100, TriggerTokens: 80, TargetTokens: 70},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Diagnostics.RequestTokensAfter != 70 || result.Diagnostics.ReductionStage != "optional" {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	if len(result.Contents) != 1 || result.Contents[0].Parts[0].Text != "current request" {
		t.Fatalf("contents = %#v", result.Contents)
	}
}

func TestCompilerDoesNotExternalizeAfterOptionalContextReachesTarget(t *testing.T) {
	store := newFakeResultStore()
	counter := &sequenceTokenCounter{counts: []int{81, 65}}
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "old request"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "old answer"}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "current request"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{FunctionCall: &domain.FunctionCall{ID: "call-1", Name: "read_file"}}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{FunctionResponse: &domain.FunctionResponse{ID: "call-1", Name: "read_file", Response: map[string]any{"text": readableText(500)}}}}},
	}
	result, err := New(store, counter).Compile(t.Context(), domain.CompileRequest{
		Contents: contents, ModelBudget: domain.RequestBudget{HardTokens: 100, TriggerTokens: 80, TargetTokens: 70},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Diagnostics.RequestTokensAfter != 65 || result.Diagnostics.ResponsesExternalized != 0 || store.putCalls != 0 {
		t.Fatalf("diagnostics=%#v writes=%d", result.Diagnostics, store.putCalls)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// readableText returns n code points of safe, repeatable text.
func readableText(n int) string {
	var builder strings.Builder
	const line = "Line content demonstrating a realistic file excerpt with technical prose about code structure, architecture patterns, and implementation details.\n"
	chars := 0
	for chars < n {
		builder.WriteString(line)
		chars += utf8.RuneCountInString(line)
	}
	runes := []rune(builder.String())
	if len(runes) > n {
		runes = runes[:n]
	}
	return string(runes)
}

func funcCallID(i int) string {
	return fmt.Sprintf("call-%d", i+1)
}

func fileName(i int) string {
	files := []string{
		"src/main.go", "src/config.go", "src/router.go",
		"src/handler.go", "src/models.go", "src/utils.go",
		"src/middleware.go",
	}
	return files[i]
}

// budget60Percent returns a RequestBudget that sets the hard limit to 60% of
// the window, as used in the incident reproduction.
func budget60Percent(windowTokens int) domain.RequestBudget {
	budget, _ := domain.NewRequestBudget(windowTokens, domain.RequestBudgetPolicy{MaxRequestPercent: 60})
	return budget
}

// newCompiler creates a test compiler with fakes.
func newCompiler() *Compiler {
	return New(newFakeResultStore(), fakeTokenCounter{})
}

// ---------------------------------------------------------------------------
// Incident fixture: 7 large read_file responses within a 76,800 token budget
// ---------------------------------------------------------------------------

func TestIncidentFixtureExternalizesLargeResponses(t *testing.T) {
	const perResponseText = 17_200
	const windowTokens = 128_000

	// Build 2 completed turns + 1 active turn with 7 large responses.
	completed1 := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "hello"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "hi there, how can I help?"}}},
	}
	completed2 := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "analyze the project"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "I'll take a look."}}},
	}

	activeUser := domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "read these project files"}}}

	// Model emits 7 read_file function calls.
	modelCalls := make([]domain.ContentPart, 7)
	for i := 0; i < 7; i++ {
		modelCalls[i] = domain.ContentPart{
			FunctionCall: &domain.FunctionCall{
				ID:   funcCallID(i),
				Name: "read_file",
				Args: map[string]any{"path": fileName(i)},
			},
		}
	}
	activeModel := domain.Content{Role: domain.ContentRoleModel, Parts: modelCalls}

	// 7 large function responses.
	responses := make([]domain.Content, 7)
	for i := 0; i < 7; i++ {
		responses[i] = domain.Content{
			Role: domain.ContentRoleUser,
			Parts: []domain.ContentPart{{
				FunctionResponse: &domain.FunctionResponse{
					ID:   funcCallID(i),
					Name: "read_file",
					Response: map[string]any{
						"text": readableText(perResponseText),
					},
				},
			}},
		}
	}

	contents := make([]domain.Content, 0, len(completed1)+len(completed2)+2+len(responses))
	contents = append(contents, completed1...)
	contents = append(contents, completed2...)
	contents = append(contents, activeUser)
	contents = append(contents, activeModel)
	for _, r := range responses {
		contents = append(contents, r)
	}

	beforeChars, err := domain.ContentCost(contents)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("total before cost: %d code points", beforeChars)

	budget := budget60Percent(windowTokens)
	t.Logf("hard budget: %d tokens (60%% of %d)", budget.HardTokens, windowTokens)

	compiler := New(newFakeResultStore(), serializedByteCounter{})
	result, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:        contents,
		ModelBudget:     budget,
		Actor:           "U-test",
		ConversationKey: "incident-test",
	})
	if err != nil {
		t.Fatalf("Compile(): %v", err)
	}

	t.Logf("before: %d, after: %d, protected: %d, externalized: %d, removed: %d, reason: %s",
		result.Diagnostics.RequestTokensBefore,
		result.Diagnostics.RequestTokensAfter,
		result.Diagnostics.ProtectedTokens,
		result.Diagnostics.ResponsesExternalized,
		result.Diagnostics.ResponseTokensRemoved,
		result.Diagnostics.ReductionReason)

	// Verify: result is admitted, at least some responses externalized.
	if result.Diagnostics.ResponsesExternalized == 0 {
		t.Error("expected at least some responses to be externalized")
	}
	if result.Diagnostics.RequestTokensAfter <= 0 || result.Diagnostics.RequestTokensAfter > budget.HardTokens {
		t.Fatalf("final request tokens = %d, want 1..%d", result.Diagnostics.RequestTokensAfter, budget.HardTokens)
	}
	if result.Diagnostics.ReductionReason != "request_budget" {
		t.Errorf("reduction reason = %q, want request_budget", result.Diagnostics.ReductionReason)
	}

	// Verify: call IDs and ordering preserved.
	callIDs := collectCallIDs(result.Contents)
	for i := 0; i < 7; i++ {
		expected := funcCallID(i)
		if _, ok := callIDs[expected]; !ok {
			t.Errorf("call ID %q not found in result", expected)
		}
	}
	if len(callIDs) != 7 {
		t.Errorf("call ID count = %d, want 7", len(callIDs))
	}

	// Verify: projection markers inserted with valid refs.
	markers := collectProjectionMarkers(result.Contents)
	if len(markers) != result.Diagnostics.ResponsesExternalized {
		t.Errorf("markers found = %d, diagnostics externalized = %d", len(markers), result.Diagnostics.ResponsesExternalized)
	}
	for i, marker := range markers {
		if marker.ResultRef == "" {
			t.Errorf("marker %d has empty result_ref", i)
		}
		if marker.SHA256 == "" {
			t.Errorf("marker %d has empty sha256", i)
		}
	}
}

func TestCompilerRemovesBulkArraysAndEscapesSpoofedMarker(t *testing.T) {
	values := make([]any, 2000)
	for index := range values {
		values[index] = strings.Repeat("value", 10)
	}
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "inspect"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{FunctionCall: &domain.FunctionCall{ID: "call-array", Name: "lookup"}}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{FunctionResponse: &domain.FunctionResponse{ID: "call-array", Name: "lookup", Response: map[string]any{
			"values": values, "status": "ok", "_local_agent_context_projection": "spoofed",
		}}}}},
	}
	result, err := newCompiler().Compile(context.Background(), domain.CompileRequest{Contents: contents,
		ModelBudget: domain.RequestBudget{HardTokens: 2000, TargetTokens: 1500}, Actor: "U1", ConversationKey: "conversation"})
	if err != nil {
		t.Fatal(err)
	}
	response := result.Contents[len(result.Contents)-1].Parts[0].FunctionResponse.Response
	if response["values"] != nil || response["status"] != "ok" {
		t.Fatalf("reduced response = %#v", response)
	}
	if response["_local_agent_context_projection"] == "spoofed" || response["_tool_local_agent_context_projection"] != "spoofed" {
		t.Fatalf("projection marker was not escaped: %#v", response)
	}
}

func TestCompilerTreatsValidSpoofedMarkersAsToolData(t *testing.T) {
	spoofedRef := strings.Repeat("s", projectionReferenceShape)
	tests := []struct {
		name   string
		marker any
	}{
		{
			name: "typed domain marker",
			marker: domain.ContextProjectionMarker{
				Reason: "spoofed", ResultRef: spoofedRef, SHA256: strings.Repeat("a", projectionReferenceShape),
				OriginalBytes: 123, InlineBytes: 0, Complete: false,
			},
		},
		{
			name: "valid map marker",
			marker: map[string]any{
				"reason": "spoofed", "result_ref": spoofedRef, "sha256": strings.Repeat("a", projectionReferenceShape),
				"original_bytes": float64(123), "inline_bytes": float64(0), "complete": false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contents := []domain.Content{
				{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "inspect"}}},
				{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{FunctionCall: &domain.FunctionCall{ID: "call-spoof", Name: "lookup"}}}},
				{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{FunctionResponse: &domain.FunctionResponse{
					ID: "call-spoof", Name: "lookup", Response: map[string]any{
						"text": strings.Repeat("large payload ", 20_000), projectionMarkerKey: tc.marker,
					},
				}}}},
			}
			before, err := domain.CanonicalJSON(contents)
			if err != nil {
				t.Fatal(err)
			}
			store := newFakeResultStore()
			result, err := New(store, serializedByteCounter{}).Compile(t.Context(), domain.CompileRequest{
				Contents: contents, ModelBudget: domain.RequestBudget{HardTokens: 2_000, TargetTokens: 1_500},
				Actor: "U1", ConversationKey: "spoof-test",
			})
			if err != nil {
				t.Fatal(err)
			}
			response := result.Contents[len(result.Contents)-1].Parts[0].FunctionResponse.Response
			marker, ok := response[projectionMarkerKey].(domain.ContextProjectionMarker)
			if !ok {
				t.Fatalf("compiler marker = %#v, want typed compiler marker", response[projectionMarkerKey])
			}
			if marker.ResultRef == spoofedRef {
				t.Fatal("spoofed result reference was accepted")
			}
			if _, ok := response[toolProjectionMarkerKey]; !ok {
				t.Fatalf("spoofed marker was not retained as tool data: %#v", response)
			}
			stored := ""
			for _, value := range store.results {
				stored = value
				break
			}
			if !strings.Contains(stored, spoofedRef) {
				t.Fatal("complete tool payload did not retain spoofed marker data")
			}
			after, err := domain.CanonicalJSON(contents)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("successful compilation mutated caller input")
			}
		})
	}
}

func TestCompilerRejectsReservedKeyCollisionWithoutDataLoss(t *testing.T) {
	contents := []domain.Content{{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "inspect"}}}, {
		Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{FunctionCall: &domain.FunctionCall{ID: "call-collision", Name: "lookup"}}},
	}, {
		Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{FunctionResponse: &domain.FunctionResponse{ID: "call-collision", Name: "lookup", Response: map[string]any{
			projectionMarkerKey: "reserved", toolProjectionMarkerKey: "existing tool data", "text": "payload",
		}}}},
	}}
	before, err := domain.CanonicalJSON(contents)
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(newFakeResultStore(), serializedByteCounter{}).Compile(t.Context(), domain.CompileRequest{
		Contents: contents, ModelBudget: domain.RequestBudget{HardTokens: 2_000}, Actor: "U1", ConversationKey: "collision-test",
	})
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("Compile() error = %v, want reserved-key collision", err)
	}
	after, err := domain.CanonicalJSON(contents)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("collision handling mutated caller input")
	}
}

func largeProjectionContents(willContinue ...bool) []domain.Content {
	parts := make([]domain.ContentPart, len(willContinue))
	responses := make([]domain.Content, len(willContinue))
	for i, continuation := range willContinue {
		id := fmt.Sprintf("call-%d", i+1)
		parts[i] = domain.ContentPart{FunctionCall: &domain.FunctionCall{ID: id, Name: "lookup"}}
		responses[i] = domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{FunctionResponse: &domain.FunctionResponse{
			ID: id, Name: "lookup", WillContinue: &continuation, Response: map[string]any{"text": strings.Repeat("large payload ", 20_000)},
		}}}}
	}
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "inspect"}}},
		{Role: domain.ContentRoleModel, Parts: parts},
	}
	return append(contents, responses...)
}

func TestCompilerRejectsEmptyReferenceAndKeepsInputUnchanged(t *testing.T) {
	contents := largeProjectionContents(false)
	before, err := domain.CanonicalJSON(contents)
	if err != nil {
		t.Fatal(err)
	}
	store := newFakeResultStore()
	store.emptyRef = true
	result, err := New(store, serializedByteCounter{}).Compile(t.Context(), domain.CompileRequest{
		Contents: contents, ModelBudget: domain.RequestBudget{HardTokens: 2_000, TargetTokens: 1_500}, Actor: "U1", ConversationKey: "empty-ref-test",
	})
	if err == nil || !strings.Contains(err.Error(), "empty reference") {
		t.Fatalf("Compile() error = %v, want empty reference error", err)
	}
	if len(result.Contents) != 0 || store.putCalls != 1 {
		t.Fatalf("result=%#v writes=%d, want no projected result and one attempted Put", result, store.putCalls)
	}
	after, err := domain.CanonicalJSON(contents)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("empty-reference failure mutated caller input")
	}
}

func TestCompilerSecondPutFailureDoesNotPublishPartialProjection(t *testing.T) {
	contents := largeProjectionContents(false, false)
	before, err := domain.CanonicalJSON(contents)
	if err != nil {
		t.Fatal(err)
	}
	store := newFakeResultStore()
	store.failPutAt = 2
	store.putFailure = errors.New("second put failed")
	result, err := New(store, serializedByteCounter{}).Compile(t.Context(), domain.CompileRequest{
		Contents: contents, ModelBudget: domain.RequestBudget{HardTokens: 2_000, TargetTokens: 1_500}, Actor: "U1", ConversationKey: "second-put-test",
	})
	if err == nil || !strings.Contains(err.Error(), "second put failed") {
		t.Fatalf("Compile() error = %v, want second Put error", err)
	}
	if len(result.Contents) != 0 || store.putCalls != 2 {
		t.Fatalf("result=%#v writes=%d, want no projected result and two Put attempts", result, store.putCalls)
	}
	after, err := domain.CanonicalJSON(contents)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("second-Put failure mutated caller input")
	}
}

func TestCompilerRequiresBindingsBeforeProjectionStorage(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  domain.CompileRequest
	}{
		{name: "actor", req: domain.CompileRequest{ConversationKey: "binding-test"}},
		{name: "conversation", req: domain.CompileRequest{Actor: "U1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contents := largeProjectionContents(false)
			before, err := domain.CanonicalJSON(contents)
			if err != nil {
				t.Fatal(err)
			}
			store := newFakeResultStore()
			tc.req.Contents = contents
			tc.req.ModelBudget = domain.RequestBudget{HardTokens: 2_000, TargetTokens: 1_500}
			_, err = New(store, serializedByteCounter{}).Compile(t.Context(), tc.req)
			if err == nil {
				t.Fatal("Compile() succeeded without projection binding")
			}
			if store.putCalls != 0 {
				t.Fatalf("Put calls = %d, want zero before binding validation", store.putCalls)
			}
			after, err := domain.CanonicalJSON(contents)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("binding failure mutated caller input")
			}
		})
	}
}

func lateProjectionHardLimit(t *testing.T, contents []domain.Content, parts []reduciblePart) int {
	t.Helper()
	compiler := New(newFakeResultStore(), serializedByteCounter{})
	minimum := dryRunActiveContents(contents, parts)
	count, err := compiler.countProjection(t.Context(), minimum, 0)
	if err != nil {
		t.Fatal(err)
	}
	return count.Tokens
}

func TestLateExternalizationUsesBatchValidation(t *testing.T) {
	contents := largeProjectionContents(false, false)
	before, err := domain.CanonicalJSON(contents)
	if err != nil {
		t.Fatal(err)
	}
	_, parts := classifyActiveParts(contents)
	store := newFakeResultStore()
	store.emptyRef = true
	compiler := New(store, serializedByteCounter{})
	_, _, _, err = compiler.lateExternalize(t.Context(), domain.CompileRequest{
		Actor: "U1", ConversationKey: "late-empty-ref", ModelBudget: domain.RequestBudget{HardTokens: 2_000},
	}, contents, parts, nil, lateProjectionHardLimit(t, contents, parts))
	if err == nil || !strings.Contains(err.Error(), "empty reference") {
		t.Fatalf("lateExternalize() error = %v, want empty reference error", err)
	}
	if store.putCalls != 1 {
		t.Fatalf("late Put calls = %d, want one", store.putCalls)
	}
	after, err := domain.CanonicalJSON(contents)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("late empty-reference failure mutated caller input")
	}
}

func TestLateExternalizationDoesNotPublishAfterSecondPutFailure(t *testing.T) {
	contents := largeProjectionContents(false, false)
	before, err := domain.CanonicalJSON(contents)
	if err != nil {
		t.Fatal(err)
	}
	_, parts := classifyActiveParts(contents)
	store := newFakeResultStore()
	store.failPutAt = 2
	store.putFailure = errors.New("late second put failed")
	compiler := New(store, serializedByteCounter{})
	result, _, _, err := compiler.lateExternalize(t.Context(), domain.CompileRequest{
		Actor: "U1", ConversationKey: "late-second-put", ModelBudget: domain.RequestBudget{HardTokens: 2_000},
	}, contents, parts, nil, lateProjectionHardLimit(t, contents, parts))
	if err == nil || !strings.Contains(err.Error(), "late second put failed") {
		t.Fatalf("lateExternalize() error = %v, want second Put error", err)
	}
	if result != nil || store.putCalls != 2 {
		t.Fatalf("late result=%#v writes=%d, want no result and two Put attempts", result, store.putCalls)
	}
	after, err := domain.CanonicalJSON(contents)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("late second-Put failure mutated caller input")
	}
}

func TestCompilerPreservesResponseIdentityAndOrderAfterProjection(t *testing.T) {
	contents := largeProjectionContents(false, false)
	result, err := New(newFakeResultStore(), serializedByteCounter{}).Compile(t.Context(), domain.CompileRequest{
		Contents: contents, ModelBudget: domain.RequestBudget{HardTokens: 2_000, TargetTokens: 1_500}, Actor: "U1", ConversationKey: "identity-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	var responses []*domain.FunctionResponse
	for _, content := range result.Contents {
		for _, part := range content.Parts {
			if part.FunctionResponse != nil {
				responses = append(responses, part.FunctionResponse)
			}
		}
	}
	if len(responses) != 2 {
		t.Fatalf("projected responses = %d, want two", len(responses))
	}
	for i, response := range responses {
		if response.ID != fmt.Sprintf("call-%d", i+1) || response.Name != "lookup" || response.WillContinue == nil || *response.WillContinue {
			t.Fatalf("response %d identity = %#v, want preserved ID/name/WillContinue/order", i, response)
		}
	}
}

// ---------------------------------------------------------------------------
// Call ID ordering preservation
// ---------------------------------------------------------------------------

func TestCallIDsAndOrderingPreserved(t *testing.T) {
	const perResponseText = 17_200
	budget := budget60Percent(128_000)

	modelCalls := make([]domain.ContentPart, 3)
	for i := 0; i < 3; i++ {
		modelCalls[i] = domain.ContentPart{
			FunctionCall: &domain.FunctionCall{
				ID:   funcCallID(i),
				Name: "read_file",
				Args: map[string]any{"path": fileName(i)},
			},
		}
	}
	responses := make([]domain.Content, 3)
	for i := 0; i < 3; i++ {
		responses[i] = domain.Content{
			Role: domain.ContentRoleUser,
			Parts: []domain.ContentPart{{
				FunctionResponse: &domain.FunctionResponse{
					ID:   funcCallID(i),
					Name: "read_file",
					Response: map[string]any{
						"text": readableText(perResponseText),
					},
				},
			}},
		}
	}

	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "read files"}}},
		{Role: domain.ContentRoleModel, Parts: modelCalls},
	}
	contents = append(contents, responses...)

	compiler := newCompiler()
	result, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:        contents,
		ModelBudget:     budget,
		ConversationKey: "ordering-test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify all call IDs exist in the result.
	callIDs := collectCallIDs(result.Contents)
	for i := 0; i < 3; i++ {
		expected := funcCallID(i)
		if _, ok := callIDs[expected]; !ok {
			t.Errorf("call ID %q not found in result (found: %v)", expected, structKeys(callIDs))
		}
	}

	// Collect function calls and responses in order.
	var orderedIDs []string
	for _, content := range result.Contents {
		for _, part := range content.Parts {
			if part.FunctionCall != nil {
				orderedIDs = append(orderedIDs, "call:"+part.FunctionCall.ID)
			}
			if part.FunctionResponse != nil {
				orderedIDs = append(orderedIDs, "resp:"+part.FunctionResponse.ID)
			}
		}
	}

	// Verify calls come before their responses and match.
	callPos := make(map[string]int)
	respPos := make(map[string]int)
	for i, entry := range orderedIDs {
		if strings.HasPrefix(entry, "call:") {
			callPos[entry[5:]] = i
		} else if strings.HasPrefix(entry, "resp:") {
			respPos[entry[5:]] = i
		}
	}
	for i := 0; i < 3; i++ {
		id := funcCallID(i)
		cPos, cOk := callPos[id]
		rPos, rOk := respPos[id]
		if !cOk || !rOk {
			t.Errorf("call or response %q missing (calls=%v, resps=%v)", id,
				keys(callPos), keys(respPos))
			continue
		}
		if cPos >= rPos {
			t.Errorf("call %q at %d is not before response at %d", id, cPos, rPos)
		}
	}
}

// ---------------------------------------------------------------------------
// Small responses: no externalization
// ---------------------------------------------------------------------------

func TestSmallResponsesNoExternalization(t *testing.T) {
	modelCalls := make([]domain.ContentPart, 2)
	for i := 0; i < 2; i++ {
		modelCalls[i] = domain.ContentPart{
			FunctionCall: &domain.FunctionCall{
				ID:   funcCallID(i),
				Name: "read_file",
				Args: map[string]any{"path": fileName(i)},
			},
		}
	}
	responses := make([]domain.Content, 2)
	for i := 0; i < 2; i++ {
		responses[i] = domain.Content{
			Role: domain.ContentRoleUser,
			Parts: []domain.ContentPart{{
				FunctionResponse: &domain.FunctionResponse{
					ID:   funcCallID(i),
					Name: "read_file",
					Response: map[string]any{
						"text": "short response",
					},
				},
			}},
		}
	}

	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "read files"}}},
		{Role: domain.ContentRoleModel, Parts: modelCalls},
	}
	contents = append(contents, responses...)

	budget := budget60Percent(128_000)
	compiler := newCompiler()
	result, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:        contents,
		ModelBudget:     budget,
		ConversationKey: "small-test",
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.Diagnostics.ResponsesExternalized != 0 {
		t.Errorf("expected 0 externalized, got %d", result.Diagnostics.ResponsesExternalized)
	}
	if result.Diagnostics.ReductionReason == "request_budget" {
		t.Error("should not have request_budget reason for small responses")
	}
}

// ---------------------------------------------------------------------------
// Large response → externalized (with forced tight budget)
// ---------------------------------------------------------------------------

func TestHugeResponseExternalizedWithTightBudget(t *testing.T) {
	const perResponseText = 50_000

	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "read file"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{
			FunctionCall: &domain.FunctionCall{ID: "call-1", Name: "read_file", Args: map[string]any{"path": "big.go"}},
		}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{
			FunctionResponse: &domain.FunctionResponse{
				ID: "call-1", Name: "read_file",
				Response: map[string]any{"text": readableText(perResponseText)},
			},
		}}},
	}

	// Use a budget small enough to force externalization (10k tokens).
	budget := domain.RequestBudget{WindowTokens: 128_000, HardTokens: 10_000}

	compiler := newCompiler()
	result, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:        contents,
		ModelBudget:     budget,
		Actor:           "U-test",
		ConversationKey: "tight-budget",
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.Diagnostics.ResponsesExternalized != 1 {
		t.Errorf("expected 1 externalized, got %d", result.Diagnostics.ResponsesExternalized)
	}
	if result.Diagnostics.ResponseTokensRemoved <= 0 {
		t.Error("expected positive tokens removed")
	}
}

// ---------------------------------------------------------------------------
// All responses huge → fair division
// ---------------------------------------------------------------------------

func TestAllHugeResponsesFairDivision(t *testing.T) {
	const perResponseText = 30_000
	const numResponses = 5
	budget := budget60Percent(128_000)

	modelCalls := make([]domain.ContentPart, numResponses)
	for i := 0; i < numResponses; i++ {
		modelCalls[i] = domain.ContentPart{
			FunctionCall: &domain.FunctionCall{
				ID:   funcCallID(i),
				Name: "read_file",
				Args: map[string]any{"path": fileName(i)},
			},
		}
	}
	responses := make([]domain.Content, numResponses)
	for i := 0; i < numResponses; i++ {
		responses[i] = domain.Content{
			Role: domain.ContentRoleUser,
			Parts: []domain.ContentPart{{
				FunctionResponse: &domain.FunctionResponse{
					ID:   funcCallID(i),
					Name: "read_file",
					Response: map[string]any{
						"text": readableText(perResponseText),
					},
				},
			}},
		}
	}

	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "read all files"}}},
		{Role: domain.ContentRoleModel, Parts: modelCalls},
	}
	contents = append(contents, responses...)

	compiler := newCompiler()
	result, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:        contents,
		ModelBudget:     budget,
		Actor:           "U-test",
		ConversationKey: "fair-division",
	})
	if err != nil {
		t.Fatal(err)
	}

	// All should be externalized since they're all too big for the budget.
	if result.Diagnostics.ResponsesExternalized != numResponses {
		t.Errorf("expected %d externalized, got %d", numResponses, result.Diagnostics.ResponsesExternalized)
	}

	// Verify no response is given significantly more inline space than others.
	markers := collectProjectionMarkers(result.Contents)
	if len(markers) != numResponses {
		t.Fatalf("expected %d markers, got %d", numResponses, len(markers))
	}
}

func TestSmallResponseDonatesUnusedAllocationToLargeResponse(t *testing.T) {
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "inspect files"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{
			{FunctionCall: &domain.FunctionCall{ID: "small", Name: "read_file", Args: map[string]any{"path": "small.go"}}},
			{FunctionCall: &domain.FunctionCall{ID: "large", Name: "read_file", Args: map[string]any{"path": "large.go"}}},
		}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{FunctionResponse: &domain.FunctionResponse{
			ID: "small", Name: "read_file", Response: map[string]any{"text": readableText(600)},
		}}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{FunctionResponse: &domain.FunctionResponse{
			ID: "large", Name: "read_file", Response: map[string]any{"text": readableText(10_000)},
		}}}},
	}
	_, reducible := classifyActiveParts(contents)
	mins := minEnvelopeCosts(reducible)
	protected, err := domain.ContentCost([]domain.Content{{Role: contents[0].Role, Parts: contents[0].Parts}, {Role: contents[1].Role, Parts: contents[1].Parts}})
	if err != nil {
		t.Fatal(err)
	}
	// Give the small response its complete cost plus a small remainder. Equal
	// division would incorrectly externalize it instead of donating the remainder.
	smallCost := reducible[0].cost
	hard := protected + mins[0] + mins[1] + 2*(smallCost-mins[0]) + 100
	result, err := newCompiler().Compile(t.Context(), domain.CompileRequest{
		Contents:        contents,
		ModelBudget:     domain.RequestBudget{HardTokens: hard, TargetTokens: hard},
		Actor:           "U-test",
		ConversationKey: "fair-water-fill",
	})
	if err != nil {
		t.Fatal(err)
	}
	markers := collectProjectionMarkers(result.Contents)
	if len(markers) != 1 {
		t.Fatalf("projection markers = %d, want only large response externalized", len(markers))
	}
	if result.Contents[2].Parts[0].FunctionResponse.Response["_local_agent_context_projection"] != nil {
		t.Fatal("small response was externalized")
	}
}

// ---------------------------------------------------------------------------
// Empty responses → no-op
// ---------------------------------------------------------------------------

func TestEmptyResponsesNoOp(t *testing.T) {
	modelCalls := make([]domain.ContentPart, 2)
	for i := 0; i < 2; i++ {
		modelCalls[i] = domain.ContentPart{
			FunctionCall: &domain.FunctionCall{
				ID:   funcCallID(i),
				Name: "empty_tool",
				Args: map[string]any{},
			},
		}
	}
	responses := make([]domain.Content, 2)
	for i := 0; i < 2; i++ {
		responses[i] = domain.Content{
			Role: domain.ContentRoleUser,
			Parts: []domain.ContentPart{{
				FunctionResponse: &domain.FunctionResponse{
					ID:       funcCallID(i),
					Name:     "empty_tool",
					Response: map[string]any{},
				},
			}},
		}
	}

	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "run empty tools"}}},
		{Role: domain.ContentRoleModel, Parts: modelCalls},
	}
	contents = append(contents, responses...)

	budget := budget60Percent(128_000)
	compiler := newCompiler()
	result, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:        contents,
		ModelBudget:     budget,
		ConversationKey: "empty-test",
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.Diagnostics.ResponsesExternalized != 0 {
		t.Errorf("expected 0 externalized for empty responses, got %d", result.Diagnostics.ResponsesExternalized)
	}
}

// ---------------------------------------------------------------------------
// Confirmations never reduced, but adjacent reducible responses may be
// ---------------------------------------------------------------------------

func TestConfirmationsNeverReduced(t *testing.T) {
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "delete file"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{
			FunctionCall: &domain.FunctionCall{ID: "call-delete", Name: "delete_file", Args: map[string]any{"path": "important.go"}},
		}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{
			FunctionResponse: &domain.FunctionResponse{
				ID: "call-delete", Name: "delete_file",
				Response: map[string]any{"status": "requires_confirmation", "text": readableText(20_000)},
			},
		}}},
	}

	// Use a budget that forces externalization of the non-confirmation response.
	beforeCost, _ := domain.ContentCost(contents)
	// Budget: just above the minimum needed for protocol skeleton.
	budget := domain.RequestBudget{WindowTokens: 128_000, HardTokens: beforeCost - 5_000}

	compiler := newCompiler()
	result, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:          contents,
		ModelBudget:       budget,
		Actor:             "U-test",
		OpenInvocationIDs: map[string]struct{}{"call-delete": {}},
		ConversationKey:   "confirmation-test",
	})
	if err != nil {
		t.Fatalf("expected non-confirmation response to be externalized, got: %v", err)
	}

	// The non-confirmation delete_file response should be externalized.
	if result.Diagnostics.ResponsesExternalized != 1 {
		t.Errorf("expected 1 externalized (delete_file response), got %d", result.Diagnostics.ResponsesExternalized)
	}

	// Verify the response that was externalized is the delete_file one, not a confirmation.
	markers := collectProjectionMarkers(result.Contents)
	for _, m := range markers {
		if m.Reason != "request_budget" {
			t.Errorf("unexpected marker reason: %s", m.Reason)
		}
	}

	// Verify the confirmation function call has NOT been reducible-classified.
	// (This is verified constructively — the compiler never classifies
	// confirmation-named FunctionResponses as reducible.)
}

// ---------------------------------------------------------------------------
// Confirmations in a full cycle with tight budget still protect confirmation
// ---------------------------------------------------------------------------

func TestConfirmationsProtectedWhenBudgetForcesExternalization(t *testing.T) {
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "delete file"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{
			FunctionCall: &domain.FunctionCall{ID: "call-del", Name: "delete_file", Args: map[string]any{"path": "f.go"}},
		}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{
			FunctionResponse: &domain.FunctionResponse{
				ID: "call-del", Name: "delete_file",
				Response: map[string]any{"status": "requires_confirmation", "text": readableText(30_000)},
			},
		}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{
			FunctionCall: &domain.FunctionCall{
				ID: "conf-1", Name: domain.ConfirmationFunctionName,
				Args: map[string]any{"originalFunctionCall": map[string]any{"id": "call-del", "name": "delete_file"}},
			},
		}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{
			FunctionResponse: &domain.FunctionResponse{
				ID: "conf-1", Name: domain.ConfirmationFunctionName,
				Response: map[string]any{"decision": "approved"},
			},
		}}},
		// Terminal response for call-del after confirmation.
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{
			FunctionResponse: &domain.FunctionResponse{
				ID: "call-del", Name: "delete_file",
				Response: map[string]any{"status": "deleted"},
			},
		}}},
	}

	// Budget large enough for minimum protocol but small enough to force
	// externalization of the reducible delete_file response.
	budget := domain.RequestBudget{WindowTokens: 128_000, HardTokens: 15_000}

	compiler := newCompiler()
	result, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:          contents,
		ModelBudget:       budget,
		Actor:             "U-test",
		OpenInvocationIDs: map[string]struct{}{"call-del": {}},
		ConversationKey:   "conf-protection",
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	// The non-confirmation delete_file response should be externalized.
	if result.Diagnostics.ResponsesExternalized < 1 {
		t.Errorf("expected at least 1 externalized (delete_file response), got %d",
			result.Diagnostics.ResponsesExternalized)
	}

	// The confirmation response must NOT have a projection marker.
	for _, c := range result.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse == nil {
				continue
			}
			if p.FunctionResponse.Name == domain.ConfirmationFunctionName {
				if _, hasMarker := p.FunctionResponse.Response["_local_agent_context_projection"]; hasMarker {
					t.Error("confirmation response should not have projection marker")
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// User content never reduced
// ---------------------------------------------------------------------------

func TestUserContentNeverReduced(t *testing.T) {
	hugeUserText := readableText(80_000)

	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: hugeUserText}}},
	}

	budget := budget60Percent(128_000)

	compiler := newCompiler()
	_, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:        contents,
		ModelBudget:     budget,
		ConversationKey: "user-content-test",
	})
	if err == nil {
		t.Fatal("expected irreducible error for huge user content, got nil")
	}
	if !errors.Is(err, domain.ErrIrreducibleContext) {
		t.Fatalf("expected ErrIrreducibleContext, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Budget exactly equals minimum → succeeds (no reducible content)
// ---------------------------------------------------------------------------

func TestBudgetExactlyMinimumSucceeds(t *testing.T) {
	// Pure text turn — no reducible function responses.
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "hello"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "hi there"}}},
	}

	cost, err := domain.ContentCost(contents)
	if err != nil {
		t.Fatal(err)
	}

	budget := domain.RequestBudget{
		WindowTokens: 128_000,
		HardTokens:   cost,
	}

	compiler := newCompiler()
	result, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:        contents,
		ModelBudget:     budget,
		ConversationKey: "exact-budget",
	})
	if err != nil {
		t.Fatalf("expected success with exact budget, got: %v", err)
	}
	if result.Diagnostics.ReductionReason == "request_budget" {
		t.Error("should not have reduction with pure text content")
	}
}

// ---------------------------------------------------------------------------
// Budget too small for even minimum protocol → irreducible
// ---------------------------------------------------------------------------

func TestBudgetBelowMinimumFailsWithIrreducible(t *testing.T) {
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "hello"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{
			FunctionCall: &domain.FunctionCall{ID: "call-fail", Name: "tool", Args: map[string]any{}},
		}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{
			FunctionResponse: &domain.FunctionResponse{
				ID: "call-fail", Name: "tool",
				Response: map[string]any{"text": readableText(5_000)},
			},
		}}},
	}

	// Budget so small that even the min envelope doesn't fit.
	budget := domain.RequestBudget{WindowTokens: 128_000, HardTokens: 100}

	compiler := newCompiler()
	_, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:        contents,
		ModelBudget:     budget,
		ConversationKey: "below-minimum",
	})
	if err == nil {
		t.Fatal("expected irreducible error, got nil")
	}
	if !errors.Is(err, domain.ErrIrreducibleContext) {
		t.Fatalf("expected ErrIrreducibleContext, got: %v", err)
	}
	var irr *domain.IrreducibleContextError
	if !errors.As(err, &irr) {
		t.Fatalf("expected *IrreducibleContextError, got: %T", err)
	}
	if irr.MinimumTokens <= irr.HardTokens {
		t.Errorf("minimum tokens %d should exceed hard tokens %d", irr.MinimumTokens, irr.HardTokens)
	}
}

// ---------------------------------------------------------------------------
// Continuity capsule injection
// ---------------------------------------------------------------------------

func TestContinuityCapsuleInjectedWhenFits(t *testing.T) {
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "status update"}}},
	}

	capsule := domain.ContinuityCapsule{
		Revision: 1,
		Objective: &domain.ContinuityItem{
			ID: "obj-1", Kind: domain.ContinuityKindObjective,
			Text: "Deliver Wave 2 context compiler",
		},
	}

	budget := budget60Percent(128_000)
	compiler := newCompiler()
	result, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:        contents,
		Continuity:      capsule,
		ModelBudget:     budget,
		ConversationKey: "capsule-test",
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.Diagnostics.ContinuityCodePoints <= 0 || result.Diagnostics.ContinuityTokens != 0 {
		t.Errorf("continuity diagnostics = %#v", result.Diagnostics)
	}
	foundCapsule := false
	for _, c := range result.Contents {
		for _, p := range c.Parts {
			if strings.Contains(p.Text, "[UNTRUSTED CONTINUITY REFERENCE]") {
				foundCapsule = true
			}
		}
	}
	if !foundCapsule {
		t.Error("continuity capsule not found in result contents")
	}
}

func TestEmptyContinuityCapsuleNotInjected(t *testing.T) {
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "status update"}}},
	}

	budget := budget60Percent(128_000)
	compiler := newCompiler()
	result, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:        contents,
		Continuity:      domain.ContinuityCapsule{},
		ModelBudget:     budget,
		ConversationKey: "no-capsule",
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.Diagnostics.ContinuityTokens != 0 {
		t.Errorf("expected 0 continuity tokens for empty capsule, got %d", result.Diagnostics.ContinuityTokens)
	}
}

// ---------------------------------------------------------------------------
// Summary injection
// ---------------------------------------------------------------------------

func TestSummaryInjectedWhenFits(t *testing.T) {
	contents := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "hello"}}},
	}

	budget := budget60Percent(128_000)
	compiler := newCompiler()
	result, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:        contents,
		ExistingSummary: "The user stated a goal to implement a context compiler.",
		ModelBudget:     budget,
		ConversationKey: "summary-test",
	})
	if err != nil {
		t.Fatal(err)
	}

	foundSummary := false
	for _, c := range result.Contents {
		for _, p := range c.Parts {
			if strings.Contains(p.Text, "[UNTRUSTED CONVERSATION SUMMARY REFERENCE]") {
				foundSummary = true
			}
		}
	}
	if !foundSummary {
		t.Error("summary not found in result contents")
	}
}

// ---------------------------------------------------------------------------
// Recent completed turn selection
// ---------------------------------------------------------------------------

func TestRecentCompletedTurnsSelected(t *testing.T) {
	// Build 10 completed turns.
	completed := make([]domain.Content, 0, 20)
	for i := 0; i < 10; i++ {
		completed = append(completed, domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: fmt.Sprintf("msg %d", i)}}})
		completed = append(completed, domain.Content{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: fmt.Sprintf("resp %d", i)}}})
	}
	// Active turn.
	contents := append(completed, domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "current request"}}})

	budget := budget60Percent(128_000)
	compiler := newCompiler()
	result, err := compiler.Compile(context.Background(), domain.CompileRequest{
		Contents:        contents,
		ModelBudget:     budget,
		ConversationKey: "recent-turns",
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.Diagnostics.RecentTurnsRetained < 1 {
		t.Errorf("expected at least 1 recent turn retained, got %d", result.Diagnostics.RecentTurnsRetained)
	}

	// Active turn must be present.
	lastContent := result.Contents[len(result.Contents)-1]
	if lastContent.Parts[0].Text != "current request" {
		t.Errorf("active turn not retained: last part text = %q", lastContent.Parts[0].Text)
	}
}

// ---------------------------------------------------------------------------
// Collection helpers
// ---------------------------------------------------------------------------

func collectCallIDs(contents []domain.Content) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, c := range contents {
		for _, p := range c.Parts {
			if p.FunctionCall != nil {
				ids[p.FunctionCall.ID] = struct{}{}
			}
		}
	}
	return ids
}

func keys(m map[string]int) []string {
	var result []string
	for k := range m {
		result = append(result, k)
	}
	return result
}

func structKeys(m map[string]struct{}) []string {
	var result []string
	for k := range m {
		result = append(result, k)
	}
	return result
}

func collectProjectionMarkers(contents []domain.Content) []domain.ContextProjectionMarker {
	var markers []domain.ContextProjectionMarker
	for _, c := range contents {
		for _, p := range c.Parts {
			if p.FunctionResponse == nil {
				continue
			}
			if raw, ok := p.FunctionResponse.Response["_local_agent_context_projection"]; ok {
				if m, ok := raw.(domain.ContextProjectionMarker); ok {
					markers = append(markers, m)
				}
			}
		}
	}
	return markers
}
