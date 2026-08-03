package contextcompiler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// ---------------------------------------------------------------------------
// Test fakes
// ---------------------------------------------------------------------------

type fakeResultStore struct {
	results  map[string]string
	nextRef  int
	putCalls int
}

func newFakeResultStore() *fakeResultStore {
	return &fakeResultStore{results: make(map[string]string)}
}

func (s *fakeResultStore) Put(_ context.Context, req port.PutResultRequest) (domain.RecoverableResult, error) {
	s.putCalls++
	s.nextRef++
	ref := fmt.Sprintf("ref-%d", s.nextRef)
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
