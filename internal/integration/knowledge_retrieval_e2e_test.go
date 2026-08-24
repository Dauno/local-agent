package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/session"

	"github.com/Dauno/slack-local-agent/internal/adapter/adkagent"
	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
	"github.com/Dauno/slack-local-agent/internal/usecase/contextcompiler"
	knowledgeusecase "github.com/Dauno/slack-local-agent/internal/usecase/knowledge"
)

const (
	knowledgeE2EAppName = "local-agent"
	knowledgeE2EUserID  = "local_user"
)

type knowledgeE2ERetrievalBindings struct{}

func (knowledgeE2ERetrievalBindings) ResolveRetrievalBinding(_ context.Context, team, actor string, conversation domain.ConversationKey, exchangeTS string) (port.KnowledgeRetrievalBinding, error) {
	return port.KnowledgeRetrievalBinding{
		Binding:    domain.KnowledgeWriteBinding{Team: team, Actor: actor, Conversation: conversation},
		ExchangeTS: exchangeTS,
	}, nil
}

type knowledgeE2ELogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *knowledgeE2ELogger) log(level, message string, args ...any) {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s %s", level, message)
	for index := 0; index+1 < len(args); index += 2 {
		fmt.Fprintf(&builder, " %v=%v", args[index], args[index+1])
	}
	l.mu.Lock()
	l.lines = append(l.lines, builder.String())
	l.mu.Unlock()
}

func (l *knowledgeE2ELogger) Debug(message string, args ...any) { l.log("debug", message, args...) }
func (l *knowledgeE2ELogger) Info(message string, args ...any)  { l.log("info", message, args...) }
func (l *knowledgeE2ELogger) Warn(message string, args ...any)  { l.log("warn", message, args...) }
func (l *knowledgeE2ELogger) Error(message string, args ...any) { l.log("error", message, args...) }

func (l *knowledgeE2ELogger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// TestKnowledgeRetrievalEndToEndEphemeralBoundary proves the full pipeline:
// one retrieval on an authorized human turn selects a card that reaches the
// provider request through the compiler, while no durable ADK event,
// conversation message, summary projection, memory projection, or log line
// ever contains the injected block.
func TestKnowledgeRetrievalEndToEndEphemeralBoundary(t *testing.T) {
	ctx := t.Context()
	stateDir := t.TempDir()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(stateDir, "local-agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// One user-scoped active claim that the exact channel can match.
	claimID := "kclaim_0000000000000000000000e2"
	ownerKey := "slack:T12345678:user:U12345678"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, current_rev, created_at, updated_at)
		VALUES (?, 'shared subject', 'is', 'string', 'value', 'user', ?, 'human', ?, 'asserted', 1, 100, 100)`,
		claimID, ownerKey, claimID); err != nil {
		t.Fatal(err)
	}

	textLLM := newFakeLLMTextServer("answer with knowledge")
	t.Cleanup(textLLM.Close)
	llm, err := openaillm.New(
		openaillm.WithAPIKey("e2e-key"),
		openaillm.WithBaseURL(textLLM.URL+"/"),
		openaillm.WithModel("e2e-model"),
	)
	if err != nil {
		t.Fatal(err)
	}
	configureIntegrationGuard(t, llm)

	adkSession := adaptersqlite.NewAdkSessionService(store)
	compiler := contextcompiler.New(nil, integrationRequestCounter{})
	runtime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{
		AgentName: "Dev Agent", Instruction: "You are a test assistant.",
		Model: llm, SessionService: adkSession,
		ContextCompiler:       compiler,
		ContextBudget:         domain.RequestBudget{HardTokens: 1_000_000, TargetTokens: 1_000_000},
		EpochStore:            adaptersqlite.NewContextEpochStore(store),
		KnowledgeBudgetTokens: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}

	retriever, err := knowledgeusecase.NewRetriever(knowledgeusecase.RetrieverDependencies{
		Reader: adaptersqlite.NewKnowledgeCandidateReader(store),
		// This e2e test is lexical-focused: no fingerprint is bound, so
		// semantic search stays disabled and no provider is wired.
		Index:    adaptersqlite.NewKnowledgeLexicalIndexStore(store, ""),
		Resolver: knowledgeusecase.UnavailableDocumentResolver{},
		Queue:    adaptersqlite.NewKnowledgeLexicalQueueStore(store),
		Clock:    port.SystemClock{},
		Redact:   func(value string) string { return value },
	})
	if err != nil {
		t.Fatal(err)
	}

	publisher := &recordingPublisher{}
	logger := &knowledgeE2ELogger{}
	service, err := botusecase.New(botusecase.Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 30, MaxChars: 20000},
		RetainMessages: 100, MaxConcurrentCalls: 4,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
		KnowledgeRetrievalLimits: func() domain.KnowledgeRetrievalLimits {
			limits := domain.DefaultKnowledgeRetrievalLimits()
			limits.TimeoutSeconds = 2
			return limits
		}(),
	}, botusecase.Dependencies{
		Store: store, Runtime: runtime, Publisher: publisher,
		Logger: logger, Clock: port.SystemClock{},
		KnowledgeRetriever:         retriever,
		KnowledgeRetrievalBindings: knowledgeE2ERetrievalBindings{},
	})
	if err != nil {
		t.Fatal(err)
	}

	invocation := e2eDMInvocation("Ev-kn-e2e-01", "shared subject")
	outcome, err := service.Handle(ctx, invocation)
	if err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}

	// The provider request carries the selected block as the user role
	// inside the current user turn, never as an assistant or model message.
	requests := textLLM.requestsSnapshot()
	var providerText strings.Builder
	blockRole := ""
	for _, request := range requests {
		for _, message := range request.Messages {
			providerText.WriteString(string(message))
			if strings.Contains(string(message), "[KNOWLEDGE DATA]") {
				if strings.Contains(string(message), `"role":"assistant"`) || strings.Contains(string(message), `"role":"model"`) {
					blockRole = "model-or-assistant"
				} else if strings.Contains(string(message), `"role":"user"`) {
					blockRole = "user"
				}
			}
		}
	}
	if !strings.Contains(providerText.String(), "[KNOWLEDGE DATA]") || !strings.Contains(providerText.String(), "shared subject") {
		t.Fatalf("provider request lacks the knowledge block: %q", providerText.String())
	}
	if blockRole != "user" {
		t.Fatalf("knowledge block role = %q, want user inside the current user turn", blockRole)
	}

	// Durable ADK events never carry the block.
	loaded, err := adkSession.Get(ctx, &session.GetRequest{
		AppName: knowledgeE2EAppName, UserID: knowledgeE2EUserID, SessionID: "adk:slack:T12345678:dm:D12345678",
	})
	if err != nil || loaded == nil || loaded.Session == nil || loaded.Session.Events() == nil {
		t.Fatalf("load durable session: %v", err)
	}
	for index := 0; index < loaded.Session.Events().Len(); index++ {
		event := loaded.Session.Events().At(index)
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if strings.Contains(part.Text, "[KNOWLEDGE DATA]") {
				t.Fatalf("durable ADK event %d carries the injected knowledge block: %q", index, part.Text)
			}
		}
	}

	// Conversation messages never carry the block.
	rows, err := store.DB().QueryContext(ctx, `SELECT content FROM messages WHERE content LIKE '%[KNOWLEDGE DATA]%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("conversation message carries the knowledge block")
	}

	// The published response is only the model answer.
	published := publisher.Snapshot()
	if len(published) != 1 || published[0].text != "answer with knowledge" {
		t.Fatalf("published = %#v", published)
	}

	// No memory projection file carries the block.
	memoryDir := filepath.Join(stateDir, "memory")
	if entries, readErr := os.ReadDir(memoryDir); readErr == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			content, readFileErr := os.ReadFile(filepath.Join(memoryDir, entry.Name()))
			if readFileErr != nil {
				continue
			}
			if strings.Contains(string(content), "[KNOWLEDGE DATA]") {
				t.Fatalf("memory projection %s carries the knowledge block", entry.Name())
			}
		}
	}

	// Logs never carry the injected block or the retrieved card content.
	for _, line := range logger.snapshot() {
		if strings.Contains(line, "[KNOWLEDGE DATA]") || strings.Contains(line, "value [status") {
			t.Fatalf("log line carries knowledge content: %q", line)
		}
	}

	// The epoch persisted content-free facts for the same model step.
	epochs, err := adaptersqlite.NewContextEpochStore(store).Range(ctx, knowledgeE2EAppName, knowledgeE2EUserID, "adk:slack:T12345678:dm:D12345678", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(epochs) != 1 {
		t.Fatalf("epochs = %d, want 1", len(epochs))
	}
	if len(epochs[0].KnowledgeIdentities) != 1 || epochs[0].KnowledgeIdentities[0] != "claim:"+claimID || epochs[0].SelectedSourceCount != 1 {
		t.Fatalf("epoch facts = %#v", epochs[0])
	}
}
