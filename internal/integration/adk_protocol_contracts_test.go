package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"iter"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/adkagent"
	"github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/contextcompiler"
)

var updateADKProtocolFixtures = flag.Bool("update-adk-protocol-fixtures", false, "regenerate testdata/adk-protocol fixtures from the pinned ADK")

const (
	protocolAppName = "local-agent"
	protocolUserID  = "local_user"
	protocolActor   = "U12345678"
	protocolSchema  = "adk-protocol-ledger/v1"
	protocolADK     = "google.golang.org/adk/v2@v2.0.0"
)

type scriptedModelStep struct {
	responses []*model.LLMResponse
}

type scriptedProtocolRequest struct {
	contentCount    int
	texts           []string
	functionCallIDs []string
}

type scriptedProtocolLLM struct {
	mu       sync.Mutex
	steps    []scriptedModelStep
	step     int
	calls    int
	partials int
	requests []scriptedProtocolRequest
}

func (m *scriptedProtocolLLM) Name() string { return "adk-protocol-scripted-model" }

func (m *scriptedProtocolLLM) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		index := m.step
		m.step++
		m.calls++
		m.requests = append(m.requests, snapshotProtocolRequest(request))
		m.mu.Unlock()
		if index >= len(m.steps) {
			yield(nil, fmt.Errorf("scripted model exhausted at call %d", index+1))
			return
		}
		for _, response := range m.steps[index].responses {
			if response != nil && response.Partial {
				m.mu.Lock()
				m.partials++
				m.mu.Unlock()
			}
			if !yield(response, nil) {
				return
			}
		}
	}
}

func (m *scriptedProtocolLLM) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *scriptedProtocolLLM) partialCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.partials
}

func (m *scriptedProtocolLLM) requestSnapshots() []scriptedProtocolRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]scriptedProtocolRequest, len(m.requests))
	for index, request := range m.requests {
		result[index] = scriptedProtocolRequest{
			contentCount:    request.contentCount,
			texts:           append([]string(nil), request.texts...),
			functionCallIDs: append([]string(nil), request.functionCallIDs...),
		}
	}
	return result
}

func snapshotProtocolRequest(request *model.LLMRequest) scriptedProtocolRequest {
	if request == nil {
		return scriptedProtocolRequest{}
	}
	snapshot := scriptedProtocolRequest{contentCount: len(request.Contents)}
	for _, content := range request.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			if part.Text != "" {
				snapshot.texts = append(snapshot.texts, part.Text)
			}
			if part.FunctionCall != nil {
				snapshot.functionCallIDs = append(snapshot.functionCallIDs, part.FunctionCall.ID)
			}
		}
	}
	return snapshot
}

type protocolToolArgs struct {
	Value string `json:"value"`
}

type protocolToolResult struct {
	Status string `json:"status"`
	Value  string `json:"value"`
}

func newProtocolTool(t *testing.T, name string, requireConfirmation bool, executions *atomic.Int64) tool.Tool {
	t.Helper()
	created, err := functiontool.New(functiontool.Config{
		Name:                name,
		Description:         "Hermetic ADK protocol contract tool.",
		RequireConfirmation: requireConfirmation,
	}, func(_ agent.Context, args protocolToolArgs) (protocolToolResult, error) {
		if executions != nil {
			executions.Add(1)
		}
		return protocolToolResult{Status: "executed", Value: args.Value}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func protocolFunctionCallResponse(id, name, value string) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID:   id,
				Name: name,
				Args: map[string]any{"value": value},
			}}},
		},
		TurnComplete: true,
	}
}

func protocolTextResponse(text string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      genai.NewContentFromText(text, genai.RoleModel),
		TurnComplete: true,
	}
}

func newProtocolRuntime(t *testing.T, service session.Service, llm model.LLM, tools ...tool.Tool) *adkagent.Runtime {
	t.Helper()
	runtime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{
		AgentName:      "Protocol Contract Agent",
		Instruction:    "Use the registered contract tools when requested.",
		Model:          llm,
		SessionService: service,
		StaticTools:    tools,
		ProviderFamily: domain.ProviderFamilyOpenAICompatible,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func protocolRequest(key domain.ConversationKey, text string) port.AgentRequest {
	return port.AgentRequest{
		ConversationKey: key,
		Messages:        []domain.Message{{Role: domain.RoleUser, UserID: protocolActor, Content: text}},
	}
}

func protocolSessionID(key domain.ConversationKey) string { return "adk:" + string(key) }

func readProtocolEvents(t *testing.T, service session.Service, key domain.ConversationKey) []*session.Event {
	t.Helper()
	response, err := service.Get(t.Context(), &session.GetRequest{
		AppName: protocolAppName, UserID: protocolUserID, SessionID: protocolSessionID(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []*session.Event
	for event := range response.Session.Events().All() {
		events = append(events, event)
	}
	return events
}

// TestADKCrashBoundaryReproducesMissingResponse documents the Phase 0 crash
// boundary and the TRD validation split. The first runner persists the model
// call, then the test service simulates a process stop before the tool response
// append. The orphaned call is intentionally accepted when it remains in the
// active suffix. Under FR-11 and section 7.3, the provider-readiness/preflight
// boundary added in Wave 2 / Phase 2 must stop that incomplete request before
// it reaches the model; the compiler's active-suffix acceptance is intentional.
func TestADKCrashBoundaryReproducesMissingResponse(t *testing.T) {
	t.Run("orphaned call in ACTIVE suffix", func(t *testing.T) {
		database := filepath.Join(t.TempDir(), "crash-boundary.db")
		store, err := sqlite.Initialize(t.Context(), database)
		if err != nil {
			t.Fatal(err)
		}

		key := domain.ConversationKey("slack:T12345678:dm:DCRASH001")
		callID := "call_crash_boundary_001"
		scripted := &scriptedProtocolLLM{steps: []scriptedModelStep{{responses: []*model.LLMResponse{
			protocolFunctionCallResponse(callID, "inspect_project", "crash-boundary"),
		}}}}
		crashingService := &crashBeforeToolResponseService{Service: sqlite.NewAdkSessionService(store)}
		crashRuntime := newProtocolRuntime(t, crashingService, scripted, newProtocolTool(t, "inspect_project", false, nil))
		if _, err := crashRuntime.Run(t.Context(), protocolRequest(key, "inspect the project")); err == nil {
			t.Fatal("crash simulation unexpectedly completed")
		}

		beforeRestart := readProtocolEvents(t, crashingService, key)
		if len(beforeRestart) != 2 {
			t.Fatalf("events before simulated restart = %d, want user plus model call", len(beforeRestart))
		}
		if !eventHasFunctionCall(beforeRestart[1], callID) || eventHasFunctionResponse(beforeRestart[1], callID) {
			t.Fatalf("crash boundary ledger = %#v, want unmatched model function call", beforeRestart[1])
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}

		reopened, err := sqlite.OpenExisting(t.Context(), database)
		if err != nil {
			t.Fatal(err)
		}
		reopenedService := sqlite.NewAdkSessionService(reopened)
		if got := len(readProtocolEvents(t, reopenedService, key)); got != len(beforeRestart) {
			_ = reopened.Close()
			t.Fatalf("events after reopen = %d, want %d", got, len(beforeRestart))
		}

		// The retry makes the orphaned call part of the retained active suffix.
		// FR-11 and section 7.3 require structural acceptance here; completeness
		// belongs to the later provider-readiness/preflight boundary.
		compiler := contextcompiler.New(nil, protocolTokenCounter{})
		recoveryModel := &scriptedProtocolLLM{steps: []scriptedModelStep{{responses: []*model.LLMResponse{
			protocolTextResponse("recovery response"),
		}}}}
		recoveryRuntime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{
			AgentName:       "Protocol Contract Agent",
			Instruction:     "Use the registered contract tools when requested.",
			Model:           recoveryModel,
			SessionService:  reopenedService,
			ContextCompiler: compiler,
			ContextBudget:   domain.RequestBudget{HardTokens: 1_000_000, TargetTokens: 1_000_000},
			StaticTools:     []tool.Tool{newProtocolTool(t, "inspect_project", false, nil)},
			ProviderFamily:  domain.ProviderFamilyOpenAICompatible,
		})
		if err != nil {
			_ = reopened.Close()
			t.Fatal(err)
		}
		turn, err := recoveryRuntime.Run(t.Context(), protocolRequest(key, "retry after restart"))
		if err != nil {
			_ = reopened.Close()
			t.Fatalf("recovery turn failed: %v", err)
		}
		if turn.Text != "recovery response" {
			_ = reopened.Close()
			t.Fatalf("recovery turn text = %q, want %q", turn.Text, "recovery response")
		}
		if recoveryModel.callCount() != 1 {
			_ = reopened.Close()
			t.Fatalf("recovery model calls = %d, want one real model call", recoveryModel.callCount())
		}
		requests := recoveryModel.requestSnapshots()
		if len(requests) != 1 || requests[0].contentCount != 3 {
			_ = reopened.Close()
			t.Fatalf("recovery model request snapshots = %#v, want one request with orphaned history plus retry", requests)
		}
		if !containsString(requests[0].functionCallIDs, callID) || !containsString(requests[0].texts, "retry after restart") {
			_ = reopened.Close()
			t.Fatalf("recovery model request = %#v, want orphaned call and new retry", requests[0])
		}
		afterRestart := readProtocolEvents(t, reopenedService, key)
		if len(afterRestart) != len(beforeRestart)+2 {
			_ = reopened.Close()
			t.Fatalf("events after real recovery turn = %d, want %d", len(afterRestart), len(beforeRestart)+2)
		}
		if !eventHasText(afterRestart[len(afterRestart)-2], "retry after restart") || !eventHasText(afterRestart[len(afterRestart)-1], "recovery response") {
			_ = reopened.Close()
			t.Fatalf("recovery events = %#v, want new user event followed by model response", afterRestart[len(afterRestart)-2:])
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("orphaned call in COMPLETED turn", func(t *testing.T) {
		callID := "call_completed_turn_001"
		completedTurn := []domain.Content{
			{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "inspect the project"}}},
			{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{FunctionCall: &domain.FunctionCall{
				ID: callID, Name: "inspect_project", Args: map[string]any{"value": "completed-turn"},
			}}}},
		}

		// This is the exported validator used by the compiler for its closed
		// portion (flattenTurns(turns[:activeIdx])). A closed portion still
		// requires every call to have a response.
		err := domain.ValidateContentProtocol(completedTurn, domain.ProtocolValidationOptions{
			RequireComplete:            true,
			AllowConfirmationLifecycle: true,
		})
		if err == nil {
			t.Fatal("completed turn with orphaned call unexpectedly passed validation")
		}
		var validationErr *domain.ProtocolValidationError
		if !errors.As(err, &validationErr) {
			t.Fatalf("completed turn error = %v, want ProtocolValidationError", err)
		}
		if validationErr.Rule != domain.ProtocolRuleIncompleteCall {
			t.Fatalf("completed turn validation rule = %q, want %q", validationErr.Rule, domain.ProtocolRuleIncompleteCall)
		}
		wantReason := fmt.Sprintf("function call %q has no response in completed turn", callID)
		if validationErr.Error() != wantReason {
			t.Fatalf("completed turn error = %q, want exact reason %q", validationErr.Error(), wantReason)
		}
	})
}

// TestADKCallbackWarningDocumentsCurrentBehavior captures the pinned ADK
// callback wrapper warning caused by CompilerBeforeModelCallback calling
// ctx.Session(). Phase 2 must remove the warning and this assertion must be
// inverted to require its absence.
func TestADKCallbackWarningDocumentsCurrentBehavior(t *testing.T) {
	var output bytes.Buffer
	logMu.Lock()
	defer logMu.Unlock()
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&output)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	}()

	store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "callback-warning.db"))
	if err != nil {
		t.Fatal(err)
	}
	service := sqlite.NewAdkSessionService(store)
	model := &scriptedProtocolLLM{steps: []scriptedModelStep{{responses: []*model.LLMResponse{
		protocolTextResponse("callback warning captured"),
	}}}}
	runtime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{
		AgentName:       "Protocol Contract Agent",
		Instruction:     "Answer the test request.",
		Model:           model,
		SessionService:  service,
		ContextCompiler: noOpProtocolCompiler{},
		ContextBudget:   domain.RequestBudget{HardTokens: 1_000_000, TargetTokens: 1_000_000},
		ProviderFamily:  domain.ProviderFamilyOpenAICompatible,
	})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), protocolRequest("slack:T12345678:dm:DCALLBACK1", "capture callback warning")); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Session() is not supported for callback context") {
		t.Fatalf("callback log = %q, want pinned ADK unsupported Session warning", output.String())
	}
}

var logMu sync.Mutex

type noOpProtocolCompiler struct{}

func (noOpProtocolCompiler) Compile(_ context.Context, request domain.CompileRequest) (domain.CompileResult, error) {
	return domain.CompileResult{Contents: request.Contents}, nil
}

type protocolTokenCounter struct{}

func (protocolTokenCounter) CountRequest(_ context.Context, _ port.ModelRequestEnvelope) (port.TokenCount, error) {
	return port.TokenCount{Strategy: "contract"}, nil
}

type crashBeforeToolResponseService struct {
	session.Service
}

func (s *crashBeforeToolResponseService) AppendEvent(ctx context.Context, current session.Session, event *session.Event) error {
	if event != nil && event.Content != nil {
		for _, part := range event.Content.Parts {
			if part != nil && part.FunctionResponse != nil && part.FunctionResponse.Name != toolconfirmation.FunctionCallName {
				return errors.New("simulated process stop before tool response commit")
			}
		}
	}
	return s.Service.AppendEvent(ctx, current, event)
}

type recordedAppend struct {
	event *session.Event
}

type recordingProtocolSessionService struct {
	session.Service
	mu      sync.Mutex
	appends []recordedAppend
}

func (s *recordingProtocolSessionService) AppendEvent(ctx context.Context, current session.Session, event *session.Event) error {
	if err := s.Service.AppendEvent(ctx, current, event); err != nil {
		return err
	}
	s.mu.Lock()
	s.appends = append(s.appends, recordedAppend{event: event})
	s.mu.Unlock()
	return nil
}

func (s *recordingProtocolSessionService) appendsSnapshot() []recordedAppend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedAppend(nil), s.appends...)
}

// TestADKOrderingAndPersistenceContract is the Phase 0 ordering drift gate.
// It exercises the real runner through the SQLite adapter in SSE mode so the
// scripted model emits a partial response before its function call. The gate
// observes only non-partial persisted events and requires the model call append
// to precede its matching tool response append.
func TestADKOrderingAndPersistenceContract(t *testing.T) {
	store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "ordering.db"))
	if err != nil {
		t.Fatal(err)
	}
	baseService := sqlite.NewAdkSessionService(store)
	recordingService := &recordingProtocolSessionService{Service: baseService}
	model := &scriptedProtocolLLM{steps: []scriptedModelStep{
		{responses: []*model.LLMResponse{
			{Content: genai.NewContentFromText("partial preamble", genai.RoleModel), Partial: true},
			protocolFunctionCallResponse("call_ordering_001", "ordering_tool", "ordering"),
		}},
		{responses: []*model.LLMResponse{protocolTextResponse("ordering complete")}},
	}}
	runtime := newProtocolRuntime(t, recordingService, model, newProtocolTool(t, "ordering_tool", false, nil))
	var streamEvents []port.AgentStreamEvent
	runtime.Stream(t.Context(), protocolRequest("slack:T12345678:dm:DORDER001", "run ordering tool"), func(event port.AgentStreamEvent) bool {
		streamEvents = append(streamEvents, event)
		return true
	})
	for _, event := range streamEvents {
		if event.Kind == port.AgentStreamError {
			t.Fatalf("stream error = %v", event.Err)
		}
	}
	if model.partialCount() != 1 {
		t.Fatalf("scripted partial responses = %d, want one", model.partialCount())
	}

	appends := recordingService.appendsSnapshot()
	callIndex, responseIndex := -1, -1
	for index, append := range appends {
		if append.event == nil {
			continue
		}
		if append.event.Partial {
			t.Fatalf("partial event reached session persistence at append %d", index)
		}
		if eventHasFunctionCall(append.event, "call_ordering_001") {
			callIndex = index
		}
		if eventHasFunctionResponse(append.event, "call_ordering_001") {
			responseIndex = index
		}
	}
	if callIndex < 0 || responseIndex < 0 || callIndex >= responseIndex {
		t.Fatalf("persisted append order = call %d, response %d, all %#v; want call before response", callIndex, responseIndex, appends)
	}

	events := readProtocolEvents(t, baseService, "slack:T12345678:dm:DORDER001")
	for index, event := range events {
		if event.Partial {
			t.Fatalf("partial event persisted in SQLite ledger at ordinal %d", index)
		}
	}
	if len(events) < 4 {
		t.Fatalf("persisted ordering ledger has %d events, want input, partial-free model call, tool response, final model response", len(events))
	}
	if model.callCount() != 2 {
		t.Fatalf("model calls = %d, want tool phase plus final phase", model.callCount())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

type protocolFixture struct {
	Schema   string          `json:"schema"`
	ADK      string          `json:"adk"`
	Scenario string          `json:"scenario"`
	Events   []protocolEvent `json:"events"`
}

type protocolEvent struct {
	Ordinal            int            `json:"ordinal"`
	Role               string         `json:"role"`
	Author             string         `json:"author,omitempty"`
	Partial            bool           `json:"partial"`
	TurnComplete       bool           `json:"turn_complete"`
	SkipSummarization  bool           `json:"skip_summarization,omitempty"`
	LongRunningToolIDs []string       `json:"long_running_tool_ids,omitempty"`
	Parts              []protocolPart `json:"parts"`
}

type protocolPart struct {
	Kind         string         `json:"kind"`
	Text         string         `json:"text,omitempty"`
	ID           string         `json:"id,omitempty"`
	Name         string         `json:"name,omitempty"`
	Args         map[string]any `json:"args,omitempty"`
	Response     map[string]any `json:"response,omitempty"`
	WillContinue *bool          `json:"will_continue,omitempty"`
}

type protocolFixtureScenario string

const (
	fixtureNormal           protocolFixtureScenario = "normal-call-response"
	fixtureParallel         protocolFixtureScenario = "parallel-calls-responses"
	fixtureConfirmationOkay protocolFixtureScenario = "confirmation-approve"
	fixtureConfirmationNo   protocolFixtureScenario = "confirmation-reject"
	fixtureConfirmationExp  protocolFixtureScenario = "confirmation-expire"
)

func TestADKProtocolFixtureContract(t *testing.T) {
	for _, scenario := range []protocolFixtureScenario{
		fixtureNormal,
		fixtureParallel,
		fixtureConfirmationOkay,
		fixtureConfirmationNo,
		fixtureConfirmationExp,
	} {
		t.Run(string(scenario), func(t *testing.T) {
			fixture := captureProtocolFixture(t, scenario)
			assertProtocolFixture(t, fixture)
			fixtureDigest := assertProtocolFixtureProtocol(t, fixture)
			path := filepath.Join("testdata", "adk-protocol", string(scenario)+".json")
			actual, err := json.MarshalIndent(fixture, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			actual = append(actual, '\n')
			if *updateADKProtocolFixtures {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, actual, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture %s: %v; run with -update-adk-protocol-fixtures", path, err)
			}
			if !bytes.Equal(actual, want) {
				t.Fatalf("fixture drift in %s; regenerate with -update-adk-protocol-fixtures", path)
			}
			var stored protocolFixture
			if err := json.Unmarshal(want, &stored); err != nil {
				t.Fatalf("decode fixture %s: %v", path, err)
			}
			assertProtocolFixture(t, stored)
			storedDigest := assertProtocolFixtureProtocol(t, stored)
			if storedDigest != fixtureDigest {
				t.Fatalf("fixture protocol digest changed across serialization: generated %q, stored %q", fixtureDigest, storedDigest)
			}
		})
	}
}

func captureProtocolFixture(t *testing.T, scenario protocolFixtureScenario) protocolFixture {
	t.Helper()
	database := filepath.Join(t.TempDir(), string(scenario)+".db")
	store, err := sqlite.Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	service := sqlite.NewAdkSessionService(store)
	key := domain.ConversationKey("slack:T12345678:dm:D" + strings.ToUpper(strings.ReplaceAll(string(scenario), "-", ""))[:8])
	var executions atomic.Int64
	var scripted *scriptedProtocolLLM
	var tools []tool.Tool
	switch scenario {
	case fixtureNormal:
		scripted = &scriptedProtocolLLM{steps: []scriptedModelStep{
			{responses: []*model.LLMResponse{protocolFunctionCallResponse("call_normal_001", "normal_tool", "normal")}},
			{responses: []*model.LLMResponse{protocolTextResponse("normal complete")}},
		}}
		tools = []tool.Tool{newProtocolTool(t, "normal_tool", false, &executions)}
	case fixtureParallel:
		scripted = &scriptedProtocolLLM{steps: []scriptedModelStep{
			{responses: []*model.LLMResponse{{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{ID: "call_parallel_a", Name: "parallel_a", Args: map[string]any{"value": "a"}}},
					{FunctionCall: &genai.FunctionCall{ID: "call_parallel_b", Name: "parallel_b", Args: map[string]any{"value": "b"}}},
				}},
				TurnComplete: true,
			}}},
			{responses: []*model.LLMResponse{protocolTextResponse("parallel complete")}},
		}}
		tools = []tool.Tool{
			newProtocolTool(t, "parallel_a", false, &executions),
			newProtocolTool(t, "parallel_b", false, &executions),
		}
	case fixtureConfirmationOkay, fixtureConfirmationNo, fixtureConfirmationExp:
		scripted = &scriptedProtocolLLM{steps: []scriptedModelStep{
			{responses: []*model.LLMResponse{protocolFunctionCallResponse("call_confirmation_001", "confirmation_tool", string(scenario))}},
			{responses: []*model.LLMResponse{protocolTextResponse("confirmation complete: " + string(scenario))}},
		}}
		tools = []tool.Tool{newProtocolTool(t, "confirmation_tool", true, &executions)}
	default:
		t.Fatalf("unknown fixture scenario %q", scenario)
	}
	runtime := newProtocolRuntime(t, service, scripted, tools...)
	first, err := runtime.Run(t.Context(), protocolRequest(key, "run "+string(scenario)))
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if scenario != fixtureNormal && scenario != fixtureParallel {
		if first.PendingConfirmation == nil {
			_ = store.Close()
			t.Fatalf("confirmation scenario returned no pending confirmation: %#v", first)
		}
		decision := domain.ConfirmationDecision{
			ConversationKey: key,
			WrapperCallID:   first.PendingConfirmation.WrapperCallID,
			OriginalCallID:  first.PendingConfirmation.OriginalCallID,
			Actor:           protocolActor,
			Approved:        scenario == fixtureConfirmationOkay,
		}
		if scenario == fixtureConfirmationExp {
			decision.Payload = map[string]any{"expired": true}
		}
		if _, err := runtime.Resume(t.Context(), decision); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	events := readProtocolEvents(t, service, key)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return canonicalProtocolFixture(string(scenario), events)
}

func canonicalProtocolFixture(scenario string, events []*session.Event) protocolFixture {
	idMap := make(map[string]string)
	nextWrapper := 0
	normalizeID := func(id, name string) string {
		if name == toolconfirmation.FunctionCallName {
			if normalized, ok := idMap[id]; ok {
				return normalized
			}
			nextWrapper++
			normalized := fmt.Sprintf("wrapper_confirmation_%03d", nextWrapper)
			idMap[id] = normalized
			return normalized
		}
		return id
	}
	result := protocolFixture{Schema: protocolSchema, ADK: protocolADK, Scenario: scenario}
	for ordinal, event := range events {
		if event == nil {
			continue
		}
		canonical := protocolEvent{
			Ordinal:           ordinal,
			Role:              contentRole(event.Content),
			Author:            event.Author,
			Partial:           event.Partial,
			TurnComplete:      event.TurnComplete,
			SkipSummarization: event.Actions.SkipSummarization,
		}
		for _, id := range event.LongRunningToolIDs {
			canonical.LongRunningToolIDs = append(canonical.LongRunningToolIDs, normalizeID(id, toolconfirmation.FunctionCallName))
		}
		if event.Content != nil {
			for _, part := range event.Content.Parts {
				if part == nil {
					canonical.Parts = append(canonical.Parts, protocolPart{Kind: "empty"})
					continue
				}
				switch {
				case part.FunctionCall != nil:
					call := part.FunctionCall
					canonical.Parts = append(canonical.Parts, protocolPart{
						Kind: "function_call", ID: normalizeID(call.ID, call.Name), Name: call.Name,
						Args: canonicalMap(call.Args),
					})
				case part.FunctionResponse != nil:
					response := part.FunctionResponse
					part := protocolPart{
						Kind: "function_response", ID: normalizeID(response.ID, response.Name), Name: response.Name,
						Response: canonicalMap(response.Response),
					}
					if response.WillContinue != nil {
						value := *response.WillContinue
						part.WillContinue = &value
					}
					canonical.Parts = append(canonical.Parts, part)
				default:
					canonical.Parts = append(canonical.Parts, protocolPart{Kind: "text", Text: part.Text})
				}
			}
		}
		result.Events = append(result.Events, canonical)
	}
	return result
}

func canonicalMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return map[string]any{"_encoding_error": err.Error()}
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return map[string]any{"_decoding_error": err.Error()}
	}
	return result
}

func contentRole(content *genai.Content) string {
	if content == nil {
		return ""
	}
	return content.Role
}

func assertProtocolFixture(t *testing.T, fixture protocolFixture) {
	t.Helper()
	if fixture.Schema != protocolSchema || fixture.ADK != protocolADK {
		t.Fatalf("fixture metadata = %#v", fixture)
	}
	if len(fixture.Events) == 0 {
		t.Fatal("fixture contains no events")
	}
	for ordinal, event := range fixture.Events {
		if event.Ordinal != ordinal {
			t.Fatalf("event ordinal at index %d = %d", ordinal, event.Ordinal)
		}
		if event.Partial {
			t.Fatalf("fixture contains partial event at ordinal %d", ordinal)
		}
		for _, part := range event.Parts {
			if part.Kind == "function_response" && part.Name != toolconfirmation.FunctionCallName && part.WillContinue != nil {
				t.Fatalf("ordinary tool response %q at ordinal %d has WillContinue=%v", part.Name, ordinal, *part.WillContinue)
			}
		}
	}
	callIndex := make(map[string]int)
	responseIndex := make(map[string]int)
	for eventIndex, event := range fixture.Events {
		for _, part := range event.Parts {
			switch part.Kind {
			case "function_call":
				callIndex[part.ID] = eventIndex
			case "function_response":
				responseIndex[part.ID] = eventIndex
			}
		}
	}
	for id, responseAt := range responseIndex {
		if callAt, ok := callIndex[id]; !ok || callAt >= responseAt {
			t.Fatalf("response %q at event %d has no earlier call", id, responseAt)
		}
	}
	switch protocolFixtureScenario(fixture.Scenario) {
	case fixtureNormal:
		assertCallResponsePair(t, fixture, "call_normal_001", "normal_tool")
	case fixtureParallel:
		assertCallResponsePair(t, fixture, "call_parallel_a", "parallel_a")
		assertCallResponsePair(t, fixture, "call_parallel_b", "parallel_b")
	case fixtureConfirmationOkay, fixtureConfirmationNo, fixtureConfirmationExp:
		assertCallResponsePair(t, fixture, "call_confirmation_001", "confirmation_tool")
		wrapper := findFixturePart(t, fixture, "function_call", toolconfirmation.FunctionCallName)
		terminalAt := -1
		for index := wrapper.index + 1; index < len(fixture.Events); index++ {
			for _, part := range fixture.Events[index].Parts {
				if part.Kind == "function_response" && part.ID == "call_confirmation_001" {
					terminalAt = index
					break
				}
			}
			if terminalAt >= 0 {
				break
			}
		}
		if terminalAt <= wrapper.index {
			t.Fatalf("confirmation terminal response at %d precedes wrapper at %d", terminalAt, wrapper.index)
		}
		if !hasFixturePart(fixture, "function_response", wrapper.part.ID, toolconfirmation.FunctionCallName) {
			t.Fatalf("confirmation decision for wrapper %q missing", wrapper.part.ID)
		}
	}
}

func assertProtocolFixtureProtocol(t *testing.T, fixture protocolFixture) string {
	t.Helper()
	contents := protocolFixtureContents(t, fixture)
	options := domain.ProtocolValidationOptions{
		RequireComplete:            true,
		AllowConfirmationLifecycle: true,
		RequireProviderReadyOrder:  true,
	}
	frontier, err := domain.ScanProtocolFrontier(contents, options)
	if err != nil {
		t.Fatalf("fixture protocol scan: %v", err)
	}
	if frontier.Status != domain.ProtocolReady || frontier.OpenCallCount != 0 {
		t.Fatalf("fixture protocol frontier = %#v, want ready with no open calls", frontier)
	}
	if err := domain.ValidateContentProtocol(contents, options); err != nil {
		t.Fatalf("fixture protocol validation: %v", err)
	}
	digest := domain.ProtocolDigest(contents)
	cloned := make([]domain.Content, len(contents))
	for index, content := range contents {
		cloned[index] = content.Clone()
	}
	if digest == "" || digest != domain.ContentProtocolDigest(contents) || digest != domain.ProtocolDigest(cloned) {
		t.Fatalf("fixture protocol digest = %q, want stable content digest", digest)
	}
	return digest
}

func protocolFixtureContents(t *testing.T, fixture protocolFixture) []domain.Content {
	t.Helper()
	contents := make([]domain.Content, len(fixture.Events))
	for eventIndex, event := range fixture.Events {
		contents[eventIndex] = domain.Content{Role: domain.ContentRole(event.Role), Parts: make([]domain.ContentPart, len(event.Parts))}
		for partIndex, part := range event.Parts {
			switch part.Kind {
			case "text":
				contents[eventIndex].Parts[partIndex].Text = part.Text
			case "function_call":
				contents[eventIndex].Parts[partIndex].FunctionCall = &domain.FunctionCall{ID: part.ID, Name: part.Name, Args: part.Args}
			case "function_response":
				response := &domain.FunctionResponse{ID: part.ID, Name: part.Name, Response: part.Response}
				if part.WillContinue != nil {
					value := *part.WillContinue
					response.WillContinue = &value
				}
				contents[eventIndex].Parts[partIndex].FunctionResponse = response
			case "empty":
			default:
				t.Fatalf("fixture event %d part %d has unsupported kind %q", eventIndex, partIndex, part.Kind)
			}
		}
	}
	return contents
}

func assertCallResponsePair(t *testing.T, fixture protocolFixture, id, name string) {
	t.Helper()
	call := findFixturePart(t, fixture, "function_call", id)
	response := findFixturePart(t, fixture, "function_response", id)
	if call.part.Name != name || response.part.Name != name || response.index <= call.index {
		t.Fatalf("call/response pair = %#v / %#v", call.part, response.part)
	}
}

type fixturePartLocation struct {
	index int
	part  protocolPart
}

func findFixturePart(t *testing.T, fixture protocolFixture, kind, id string) fixturePartLocation {
	t.Helper()
	for index, event := range fixture.Events {
		for _, part := range event.Parts {
			if part.Kind == kind && (id == "" || part.ID == id || part.Name == id) {
				return fixturePartLocation{index: index, part: part}
			}
		}
	}
	t.Fatalf("fixture part kind=%q id=%q not found", kind, id)
	return fixturePartLocation{}
}

func hasFixturePart(fixture protocolFixture, kind, id, name string) bool {
	for _, event := range fixture.Events {
		for _, part := range event.Parts {
			if part.Kind == kind && part.ID == id && part.Name == name {
				return true
			}
		}
	}
	return false
}

func eventHasFunctionCall(event *session.Event, id string) bool {
	if event == nil || event.Content == nil {
		return false
	}
	for _, part := range event.Content.Parts {
		if part != nil && part.FunctionCall != nil && part.FunctionCall.ID == id {
			return true
		}
	}
	return false
}

func eventHasFunctionResponse(event *session.Event, id string) bool {
	if event == nil || event.Content == nil {
		return false
	}
	for _, part := range event.Content.Parts {
		if part != nil && part.FunctionResponse != nil && part.FunctionResponse.ID == id {
			return true
		}
	}
	return false
}

func eventHasText(event *session.Event, text string) bool {
	if event == nil || event.Content == nil {
		return false
	}
	for _, part := range event.Content.Parts {
		if part != nil && part.Text == text {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
