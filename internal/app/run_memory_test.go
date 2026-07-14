package app

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"iter"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/memorycurator"
	"github.com/Dauno/slack-local-agent/internal/adapter/memoryprojector"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	memoryusecase "github.com/Dauno/slack-local-agent/internal/usecase/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestProcessOutboxReschedulesModelSaturationWithoutUsingRetries(t *testing.T) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "2"}
	if err := store.AppendMessage(t.Context(), metadata, domain.Message{Role: domain.RoleUser, Content: "record a durable fact", UserID: "U12345678", ExternalTS: "1", CreatedAt: time.Now().UTC()}, 10); err != nil {
		t.Fatal(err)
	}
	assistant := domain.Message{Role: domain.RoleAssistant, Content: "the fact is durable", ExternalTS: "2", CreatedAt: time.Now().UTC()}
	prepared, err := store.PrepareAssistantExchange(t.Context(), metadata, assistant, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAssistantExchangePublished(t.Context(), prepared.ID, assistant.ExternalTS); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeAssistantExchange(t.Context(), prepared.ID); err != nil {
		t.Fatal(err)
	}

	shared := modelcalllimiter.New(1)
	release, acquired := shared.TryAcquire()
	if !acquired {
		t.Fatal("failed to occupy shared model permit")
	}
	llm := &outboxTestLLM{}
	curator, err := memorycurator.New(llm, memorycurator.Config{ModelCalls: shared})
	if err != nil {
		t.Fatal(err)
	}
	memoryService, err := memoryusecase.New(memoryusecase.Config{
		Recall:      domain.MemoryRecallConfig{Enabled: true, MaxTopics: 1, MaxChars: 100},
		Limits:      domain.MemoryLimits{MaxTopics: 1, MaxLinks: 1, MaxTopicChars: 100},
		MaxPatchOps: 1,
	}, memoryusecase.Dependencies{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		processOutbox(t.Context(), store, curator, memoryService, memoryprojector.New(), t.TempDir(), 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		item, err := store.ClaimNextOutboxItem(t.Context())
		if err != nil || item == nil {
			t.Fatalf("saturated item was not rescheduled: %#v, %v", item, err)
		}
		if item.Attempts != 1 {
			t.Fatalf("saturated item attempts = %d, want 1", item.Attempts)
		}
		if err := store.RescheduleOutboxItem(t.Context(), item.ID, item.LeaseUntil, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if llm.calls != 0 {
		t.Fatalf("curator LLM calls while saturated = %d", llm.calls)
	}

	release()
	processOutbox(t.Context(), store, curator, memoryService, memoryprojector.New(), t.TempDir(), 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if llm.calls != 1 {
		t.Fatalf("curator LLM calls after permit release = %d, want 1", llm.calls)
	}
	if topics, err := store.ListTopics(t.Context()); err != nil || len(topics) != 1 {
		t.Fatalf("successful curation topics = %#v, %v", topics, err)
	}
}

func TestProcessOutboxAppliesTrustedEntityOperationsWhenCuratorFails(t *testing.T) {
	for _, test := range []struct {
		name     string
		response string
		err      error
	}{
		{name: "LLM call failure", err: errors.New("model unavailable")},
		{name: "curator parse failure", response: "not JSON"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "memory.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			key := domain.ConversationKey("slack:T12345678:dm:D12345678")
			metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "1"}
			if err := store.AppendMessage(t.Context(), metadata, domain.Message{Role: domain.RoleUser, Content: "Mi nombre es Dauno y soy el creador de local-agent", UserID: "U12345678", ExternalTS: "1", CreatedAt: time.Now().UTC()}, 10); err != nil {
				t.Fatal(err)
			}
			assistant := domain.Message{Role: domain.RoleAssistant, Content: "noted", ExternalTS: "2", CreatedAt: time.Now().UTC()}
			prepared, err := store.PrepareAssistantExchange(t.Context(), metadata, assistant, 10)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.MarkAssistantExchangePublished(t.Context(), prepared.ID, assistant.ExternalTS); err != nil {
				t.Fatal(err)
			}
			if err := store.FinalizeAssistantExchange(t.Context(), prepared.ID); err != nil {
				t.Fatal(err)
			}
			curator, err := memorycurator.New(failingCuratorLLM{response: test.response, err: test.err}, memorycurator.Config{})
			if err != nil {
				t.Fatal(err)
			}
			memoryService, err := memoryusecase.New(memoryusecase.Config{
				Recall: domain.MemoryRecallConfig{Enabled: true, MaxTopics: 1, MaxChars: 100},
				Limits: domain.MemoryLimits{MaxTopics: 2, MaxLinks: 1, MaxTopicChars: 100}, MaxPatchOps: 2,
			}, memoryusecase.Dependencies{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			processOutbox(t.Context(), store, curator, memoryService, memoryprojector.New(), t.TempDir(), 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			topic, err := store.GetTopic(t.Context(), domain.ScopedPersonTopicSlug("person-dauno", domain.SlackOwnerKey(key, "U12345678")))
			if err != nil || topic.CurrentRev != 1 {
				t.Fatalf("trusted topic = %#v, %v", topic, err)
			}
			item, err := store.ClaimNextOutboxItem(t.Context())
			if err != nil || item != nil {
				t.Fatalf("failed curator left exchange pending: %#v, %v", item, err)
			}
		})
	}
}

func TestProcessOutboxDiscardsInvalidOptionalPatchWithoutRetry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store, err := adaptersqlite.Initialize(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "1"}
	if err := store.AppendMessage(t.Context(), metadata, domain.Message{Role: domain.RoleUser, Content: "describe the sandbox", UserID: "U12345678", ExternalTS: "1", CreatedAt: time.Now().UTC()}, 10); err != nil {
		t.Fatal(err)
	}
	assistant := domain.Message{Role: domain.RoleAssistant, Content: "sandbox description", ExternalTS: "2", CreatedAt: time.Now().UTC()}
	prepared, err := store.PrepareAssistantExchange(t.Context(), metadata, assistant, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAssistantExchangePublished(t.Context(), prepared.ID, assistant.ExternalTS); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeAssistantExchange(t.Context(), prepared.ID); err != nil {
		t.Fatal(err)
	}

	llm := &outboxTestLLM{response: `{"operations":[{"type":"create_topic","topic_slug":"sandbox","topic_title":"Sandbox","content":"Read every file."}]}`}
	curator, err := memorycurator.New(llm, memorycurator.Config{})
	if err != nil {
		t.Fatal(err)
	}
	memoryService, err := memoryusecase.New(memoryusecase.Config{
		Recall: domain.MemoryRecallConfig{Enabled: true, MaxTopics: 1, MaxChars: 100},
		Limits: domain.MemoryLimits{MaxTopics: 1, MaxLinks: 1, MaxTopicChars: 100}, MaxPatchOps: 1,
	}, memoryusecase.Dependencies{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}

	processOutbox(t.Context(), store, curator, memoryService, memoryprojector.New(), t.TempDir(), 3, slog.New(slog.NewTextHandler(io.Discard, nil)))

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var status string
	var attempts int
	if err := db.QueryRowContext(t.Context(), `SELECT status, attempts FROM memory_outbox LIMIT 1`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "done" || attempts != 1 {
		t.Fatalf("discarded outbox item = status %q, attempts %d; want done after one attempt", status, attempts)
	}
	if topics, err := store.ListTopics(t.Context()); err != nil || len(topics) != 0 {
		t.Fatalf("discarded patch topics = %#v, %v", topics, err)
	}
}

func TestMemoryCuratorLLMUsesADKFinishReason(t *testing.T) {
	for _, test := range []struct {
		name         string
		finishReason genai.FinishReason
		wantText     string
		wantError    bool
	}{
		{name: "complete", finishReason: genai.FinishReasonStop, wantText: `{"operations":[]}`},
		{name: "truncated", finishReason: genai.FinishReasonMaxTokens, wantText: `{"operations":[`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			llm := &memoryCuratorLLM{llm: staticADKModel{response: &model.LLMResponse{
				Content: genai.NewContentFromText(test.wantText, genai.RoleModel), FinishReason: test.finishReason,
			}}}
			text, err := llm.GenerateText(t.Context(), "prompt")
			if test.wantError {
				if !errors.Is(err, errCuratorResponseIncomplete) || !strings.Contains(err.Error(), "finish_reason=MAX_TOKENS") {
					t.Fatalf("GenerateText() error = %v, want incomplete MAX_TOKENS", err)
				}
				return
			}
			if err != nil || text != test.wantText {
				t.Fatalf("GenerateText() = %q, %v", text, err)
			}
		})
	}
}

type outboxTestLLM struct {
	calls    int
	response string
}

func (l *outboxTestLLM) GenerateText(context.Context, string) (string, error) {
	l.calls++
	if l.response != "" {
		return l.response, nil
	}
	return `{"operations":[{"type":"create_topic","topic_slug":"durable-fact","topic_title":"Durable fact","content":"A durable fact."}]}`, nil
}

type failingCuratorLLM struct {
	response string
	err      error
}

func (l failingCuratorLLM) GenerateText(context.Context, string) (string, error) {
	return l.response, l.err
}

type staticADKModel struct{ response *model.LLMResponse }

func (staticADKModel) Name() string { return "test-model" }

func (m staticADKModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(m.response, nil)
	}
}
