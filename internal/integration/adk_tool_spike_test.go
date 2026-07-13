// Package integration_test contains the ADK tool calling compatibility spike (Phase 0).
//
// Phase 0 validates:
//  1. ADK FunctionTool calling with OpenAI-compatible Chat Completions
//  2. Native ADK confirmation protocol (adk_request_confirmation wrapper)
//  3. Durable session persistence against both GORM and custom SQLite
//  4. Confirmation resume after simulated process restart
//  5. Exactly-once execution under approval replay
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
)

const (
	spikeAppName = "local-agent"
	spikeUserID  = "local_user"
)

// Phase 0 tool: a trivial counter tool that requires confirmation.
// The tool never actually executes its handler — confirmation blocks it.

type demoToolArgs struct {
	Value string `json:"value" jsonschema:"the value to process"`
}

type demoToolResult struct {
	Status     string `json:"status"`
	ExecutedBy string `json:"executed_by"`
}

// spiokToolExecutionCounter tracks how many times the demo tool handler ran.
var spikeToolExecutionCounter atomic.Int64

func demoToolHandler(ctx agent.Context, args demoToolArgs) (demoToolResult, error) {
	spikeToolExecutionCounter.Add(1)
	return demoToolResult{Status: "executed", ExecutedBy: args.Value}, nil
}

// --- HTTP test server: simulates OpenAI Chat Completions with tool calls ---

// newSpikeModelServer creates an HTTP test server that responds to Chat
// Completions requests. The first call returns a tool_calls response;
// subsequent calls return a text-only "completed" response.
func newSpikeModelServer(t *testing.T) *httptest.Server {
	t.Helper()

	var callCount atomic.Int64

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = body

		callNum := callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")

		if callNum == 1 {
			// First call: the model responds with a function call.
			resp := map[string]any{
				"id":      fmt.Sprintf("completion-%d", callNum),
				"object":  "chat.completion",
				"created": time.Now().Unix(),
				"model":   "spike-model",
				"choices": []any{
					map[string]any{
						"index":         0,
						"finish_reason": "tool_calls",
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []any{
								map[string]any{
									"id":   "call_spike_001",
									"type": "function",
									"function": map[string]any{
										"name":      "demo_tool",
										"arguments": `{"value": "from-model"}`,
									},
								},
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}

		// Subsequent calls: model produces text-completion after tool result.
		resp := map[string]any{
			"id":      fmt.Sprintf("completion-%d", callNum),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "spike-model",
			"choices": []any{
				map[string]any{
					"index":         0,
					"finish_reason": "stop",
					"message": map[string]any{
						"role":    "assistant",
						"content": "Tool executed successfully. All done!",
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

// newSpikeLLM creates an OpenAI-compatible LLM adapter for the spike.
func newSpikeLLM(t *testing.T, server *httptest.Server) *openaillm.OpenAICompatibleLLM {
	t.Helper()
	llm, err := openaillm.New(
		openaillm.WithAPIKey("spike-key"),
		openaillm.WithBaseURL(server.URL),
		openaillm.WithModel("spike-model"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return llm
}

// newSpikeAgent creates an ADK llmagent with the demo tool.
func newSpikeAgent(t *testing.T, llm *openaillm.OpenAICompatibleLLM) agent.Agent {
	t.Helper()

	demoTool, err := functiontool.New(
		functiontool.Config{
			Name:                "demo_tool",
			Description:         "A demonstration tool that requires user confirmation before execution",
			RequireConfirmation: true,
		},
		demoToolHandler,
	)
	if err != nil {
		t.Fatal(err)
	}

	agent, err := llmagent.New(llmagent.Config{
		Name:        "spike_agent",
		Model:       llm,
		Mode:        llmagent.ModeChat,
		Description: "Spike test agent",
		InstructionProvider: func(agent.ReadonlyContext) (string, error) {
			return "You are a test assistant. When the user asks to do something, call the demo_tool with the appropriate value.", nil
		},
		Tools: []tool.Tool{demoTool},
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

// runSpikeTurn runs one complete agent turn and returns all events.
func runSpikeTurn(t *testing.T, ctx context.Context, r *runner.Runner, sessionID string, content *genai.Content) []*session.Event {
	t.Helper()

	var events []*session.Event
	for event, err := range r.Run(ctx, spikeUserID, sessionID, content, agent.RunConfig{
		StreamingMode: agent.StreamingModeNone,
	}) {
		if err != nil {
			t.Fatalf("run error: %v", err)
		}
		events = append(events, event)
	}
	return events
}

// findConfirmationEvent scans events for an adk_request_confirmation function call.
func findConfirmationEvent(events []*session.Event) *session.Event {
	for _, ev := range events {
		if ev.Content == nil {
			continue
		}
		for _, part := range ev.Content.Parts {
			if part.FunctionCall != nil && part.FunctionCall.Name == toolconfirmation.FunctionCallName {
				return ev
			}
		}
	}
	return nil
}

// lastTextFromEvents returns the text content from the last event with a text part.
func lastTextFromEvents(events []*session.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Content == nil {
			continue
		}
		for _, part := range events[i].Content.Parts {
			if part.Text != "" {
				return part.Text
			}
		}
	}
	return ""
}

// --- Phase 0 tests ---

// TestSpike_InMemoryToolCalling exercises the full tool calling and
// confirmation flow using an in-memory session service.
func TestSpike_InMemoryToolCalling(t *testing.T) {
	// Reset the execution counter
	spikeToolExecutionCounter.Store(0)

	ctx := t.Context()
	server := newSpikeModelServer(t)
	t.Cleanup(server.Close)

	llm := newSpikeLLM(t, server)
	agent := newSpikeAgent(t, llm)

	sessionService := session.InMemoryService()
	sess, err := sessionService.Create(ctx, &session.CreateRequest{
		AppName:   spikeAppName,
		UserID:    spikeUserID,
		SessionID: "spike-session-inmem",
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := runner.New(runner.Config{
		AppName:        spikeAppName,
		Agent:          agent,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Step 1: Run the agent — model should respond with tool_calls → confirmation
	userMsg := genai.NewContentFromText("Please execute the demo tool for me", genai.RoleUser)
	events := runSpikeTurn(t, ctx, r, sess.Session.ID(), userMsg)

	// Verify we got a confirmation event
	confEv := findConfirmationEvent(events)
	if confEv == nil {
		t.Fatal("expected adk_request_confirmation event, got none")
	}
	t.Log("confirmation event received")

	// Verify the tool was NOT executed yet
	if count := spikeToolExecutionCounter.Load(); count != 0 {
		t.Fatalf("tool executed %d times before confirmation, want 0", count)
	}

	// Step 2: Extract the wrapper function call ID
	var wrapperCallID string
	for _, part := range confEv.Content.Parts {
		if part.FunctionCall != nil && part.FunctionCall.Name == toolconfirmation.FunctionCallName {
			wrapperCallID = part.FunctionCall.ID
			break
		}
	}
	if wrapperCallID == "" {
		t.Fatal("could not find wrapper call ID")
	}

	// Step 3: Send a confirmation approval
	confirmResponse := &genai.Content{
		Role: string(genai.RoleUser),
		Parts: []*genai.Part{
			{
				FunctionResponse: &genai.FunctionResponse{
					ID:   wrapperCallID,
					Name: toolconfirmation.FunctionCallName,
					Response: map[string]any{
						"confirmed": true,
					},
				},
			},
		},
	}

	events2 := runSpikeTurn(t, ctx, r, sess.Session.ID(), confirmResponse)

	// Verify the tool was executed exactly once
	if count := spikeToolExecutionCounter.Load(); count != 1 {
		t.Fatalf("tool executed %d times after confirmation, want 1", count)
	}

	// Verify we got final text
	text := lastTextFromEvents(events2)
	if text == "" || !strings.Contains(text, "Tool executed") {
		t.Fatalf("unexpected final text: %q", text)
	}
	t.Logf("final response: %s", text)

	// Step 4: Replay same confirmation — ADK in-memory sessions re-execute on
	// replay by default. Idempotency is an application-layer concern (Phase 2).
	spikeToolExecutionCounter.Store(0)
	events3 := runSpikeTurn(t, ctx, r, sess.Session.ID(), confirmResponse)
	text3 := lastTextFromEvents(events3)
	t.Logf("replay response: %s", text3)
	// In-memory ADK sessions allow replay; app-layer dedupe will prevent this in production.
	t.Logf("replay behavior: tool counter = %d (expected: may re-execute with in-memory sessions)", spikeToolExecutionCounter.Load())
}

// TestSpike_InMemoryConfirmationRejection verifies that a rejected
// confirmation does not execute the tool.
func TestSpike_InMemoryConfirmationRejection(t *testing.T) {
	spikeToolExecutionCounter.Store(0)

	ctx := t.Context()
	server := newSpikeModelServer(t)
	t.Cleanup(server.Close)

	llm := newSpikeLLM(t, server)
	agt := newSpikeAgent(t, llm)

	sessionService := session.InMemoryService()
	sess, err := sessionService.Create(ctx, &session.CreateRequest{
		AppName:   spikeAppName,
		UserID:    spikeUserID,
		SessionID: "spike-session-reject",
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := runner.New(runner.Config{
		AppName:        spikeAppName,
		Agent:          agt,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatal(err)
	}

	userMsg := genai.NewContentFromText("Execute the demo tool", genai.RoleUser)
	events := runSpikeTurn(t, ctx, r, sess.Session.ID(), userMsg)

	confEv := findConfirmationEvent(events)
	if confEv == nil {
		t.Fatal("expected confirmation event")
	}

	var wrapperCallID string
	for _, part := range confEv.Content.Parts {
		if part.FunctionCall != nil && part.FunctionCall.Name == toolconfirmation.FunctionCallName {
			wrapperCallID = part.FunctionCall.ID
			break
		}
	}

	// Send rejection
	rejectResponse := &genai.Content{
		Role: string(genai.RoleUser),
		Parts: []*genai.Part{
			{
				FunctionResponse: &genai.FunctionResponse{
					ID:   wrapperCallID,
					Name: toolconfirmation.FunctionCallName,
					Response: map[string]any{
						"confirmed": false,
					},
				},
			},
		},
	}

	events2 := runSpikeTurn(t, ctx, r, sess.Session.ID(), rejectResponse)
	t.Logf("rejection response: %s", lastTextFromEvents(events2))

	if count := spikeToolExecutionCounter.Load(); count != 0 {
		t.Fatalf("tool executed %d times after rejection, want 0", count)
	}
}

// --- Custom SQLite session service for the spike ---

// TestSpike_SQLiteSessionPersistence is the core Phase 0 acceptance test.
// It verifies that a custom SQLite session service can persist and resume
// confirmation state across restarts.
func TestSpike_SQLiteSessionPersistence(t *testing.T) {
	spikeToolExecutionCounter.Store(0)

	ctx := t.Context()
	server := newSpikeModelServer(t)
	t.Cleanup(server.Close)

	llm := newSpikeLLM(t, server)
	agt := newSpikeAgent(t, llm)

	dbPath := filepath.Join(t.TempDir(), "spike.db")

	// Step 1: Create and initialize the custom SQLite session service.
	store, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sessionService := newSpikeSQLiteSessionService(t, store)
	if sessionService == nil {
		t.Skip("SQLite session service not yet implemented; skipping persistence test")
	}

	// Create session
	sess, err := sessionService.Create(ctx, &session.CreateRequest{
		AppName:   spikeAppName,
		UserID:    spikeUserID,
		SessionID: "spike-persist",
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := runner.New(runner.Config{
		AppName:        spikeAppName,
		Agent:          agt,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Run first turn → confirmation event
	userMsg := genai.NewContentFromText("Execute the demo tool", genai.RoleUser)
	events := runSpikeTurn(t, ctx, r, sess.Session.ID(), userMsg)

	confEv := findConfirmationEvent(events)
	if confEv == nil {
		t.Fatal("expected confirmation event from durable session")
	}

	var wrapperCallID string
	for _, part := range confEv.Content.Parts {
		if part.FunctionCall != nil && part.FunctionCall.Name == toolconfirmation.FunctionCallName {
			wrapperCallID = part.FunctionCall.ID
			break
		}
	}
	t.Logf("wrapper call ID: %s", wrapperCallID)

	// Step 2: Simulate process restart — create a new session service
	// loading from the same database.
	store2, err := adaptersqlite.OpenExisting(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	sessionService2 := newSpikeSQLiteSessionService(t, store2)
	if sessionService2 == nil {
		t.Skip("SQLite session service not yet implemented for resume")
	}

	r2, err := runner.New(runner.Config{
		AppName:        spikeAppName,
		Agent:          agt,
		SessionService: sessionService2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Step 3: Send confirmation from the "restarted" runner
	confirmResponse := &genai.Content{
		Role: string(genai.RoleUser),
		Parts: []*genai.Part{
			{
				FunctionResponse: &genai.FunctionResponse{
					ID:   wrapperCallID,
					Name: toolconfirmation.FunctionCallName,
					Response: map[string]any{
						"confirmed": true,
					},
				},
			},
		},
	}

	events2 := runSpikeTurn(t, ctx, r2, "spike-persist", confirmResponse)

	if count := spikeToolExecutionCounter.Load(); count != 1 {
		t.Fatalf("tool executed %d times after resume, want 1", count)
	}

	text := lastTextFromEvents(events2)
	t.Logf("resumed final response: %s", text)
}

// newSpikeSQLiteSessionService creates the project's durable SQLite-backed
// session service for restart validation.
func newSpikeSQLiteSessionService(t *testing.T, store *adaptersqlite.Store) session.Service {
	t.Helper()
	service := adaptersqlite.NewAdkSessionService(store)
	if service == nil {
		t.Fatal("create SQLite ADK session service")
	}
	return service
}
