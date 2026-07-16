package integration_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/adapter/memorycurator"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
	memoryusecase "github.com/Dauno/slack-local-agent/internal/usecase/memory"
)

func TestEntityMemoryCuratesSpanishFactAndRecallsItForFirstPersonQueryAcrossThreads(t *testing.T) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	memoryService, err := memoryusecase.New(memoryusecase.Config{
		Recall: domain.MemoryRecallConfig{Enabled: true, MaxTopics: 3, MaxChars: 2_000},
		Limits: domain.MemoryLimits{MaxTopics: 10, MaxLinks: 10, MaxTopicChars: 1_000}, MaxPatchOps: 3,
	}, memoryusecase.Dependencies{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	curator, err := memorycurator.New(entityMemoryLLM{}, memorycurator.Config{})
	if err != nil {
		t.Fatal(err)
	}
	patch, err := curator.ProposePatch(t.Context(), "slack:T12345678:dm:D12345678", "1", []domain.Message{{Role: domain.RoleUser, Content: "Mi nombre es Dauno y soy el creador de local-agent", UserID: "U12345678"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	patch.SourceAuthorID = "U12345678"
	if outcome, err := memoryService.ValidateAndApply(t.Context(), patch); err != nil || outcome != memoryusecase.OutcomeApplyCreated {
		t.Fatalf("ValidateAndApply() = %q, %v", outcome, err)
	}
	ownerKey := domain.SlackOwnerKey("slack:T12345678:dm:D12345678", "U12345678")
	slug := domain.ScopedPersonTopicSlug("person-dauno", ownerKey)
	topic, err := store.GetTopic(t.Context(), slug)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := store.GetEvidence(t.Context(), topic.ID)
	if err != nil || len(evidence) != 1 || evidence[0].AuthorID != "U12345678" {
		t.Fatalf("evidence = %#v, %v", evidence, err)
	}

	agent := &memoryRecordingAgent{}
	service, err := botusecase.New(botusecase.Config{
		AccessPolicy:  domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits: domain.ContextLimits{MaxMessages: 30, MaxChars: 20_000}, RetainMessages: 100, MaxConcurrentCalls: 1,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}, botusecase.Dependencies{Store: store, Agent: agent, Publisher: integrationPublisher{}, Memory: memoryService})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := service.Handle(t.Context(), dmInvocation("entity-recall", "D87654321", "1700000000.000002", "que sabes de mi?")); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("Handle() = %q, %v", outcome, err)
	}
	if len(agent.memory) != 1 || agent.memory[0].Slug != slug || agent.memory[0].Content != "Dauno se identifica como creador de local-agent." {
		t.Fatalf("cross-thread recalled memory = %#v", agent.memory)
	}
}

func TestEntityMemoryDoesNotPersistSpanishDirective(t *testing.T) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	memoryService, err := memoryusecase.New(memoryusecase.Config{
		Recall: domain.MemoryRecallConfig{Enabled: true, MaxTopics: 3, MaxChars: 2_000},
		Limits: domain.MemoryLimits{MaxTopics: 10, MaxLinks: 10, MaxTopicChars: 1_000}, MaxPatchOps: 3,
	}, memoryusecase.Dependencies{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	curator, err := memorycurator.New(entityMemoryLLM{}, memorycurator.Config{})
	if err != nil {
		t.Fatal(err)
	}
	patch, err := curator.ProposePatch(t.Context(), "slack:T12345678:dm:D12345678", "1", []domain.Message{{
		Role: domain.RoleUser, Content: "Recuerda que el asistente debe contestar siempre en inglés", UserID: "U12345678",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	patch.SourceAuthorID = "U12345678"
	if outcome, err := memoryService.ValidateAndApply(t.Context(), patch); err != nil || outcome != memoryusecase.OutcomeApplyNoop {
		t.Fatalf("ValidateAndApply() = %q, %v", outcome, err)
	}
	if topics, err := store.ListTopics(t.Context()); err != nil || len(topics) != 0 {
		t.Fatalf("directive persisted topics = %#v, %v", topics, err)
	}
}

func TestEntityMemoryPreservesFTSForThirdPersonAndMixedQuestions(t *testing.T) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	ownerKey := domain.SlackOwnerKey(key, "U12345678")
	limits := domain.MemoryLimits{MaxTopics: 10, MaxLinks: 10, MaxTopicChars: 1_000}
	for _, patch := range []domain.MemoryPatch{
		{
			ConversationKey: key, ExchangeTS: "1", SourceAuthorID: "U12345678",
			Operations: []domain.MemoryOp{{Type: domain.MemoryOpCreateTopic, TopicSlug: "person-dauno", TopicTitle: "Dauno", BundlePath: "people", Content: "Dauno is the creator."}},
		},
		{
			ConversationKey: key, ExchangeTS: "2", SourceAuthorID: "U12345678",
			Operations: []domain.MemoryOp{{Type: domain.MemoryOpCreateTopic, TopicSlug: "project-atlas", TopicTitle: "Project Atlas", BundlePath: "projects", Content: "Dauno maintains Project Atlas."}},
		},
	} {
		if _, err := store.ApplyMemoryPatch(t.Context(), patch, limits); err != nil {
			t.Fatal(err)
		}
	}
	memoryService, err := memoryusecase.New(memoryusecase.Config{
		Recall: domain.MemoryRecallConfig{Enabled: true, MaxTopics: 3, MaxChars: 2_000},
		Limits: domain.MemoryLimits{MaxTopics: 10, MaxLinks: 10, MaxTopicChars: 1_000}, MaxPatchOps: 3,
	}, memoryusecase.Dependencies{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	agent := &memoryRecordingAgent{}
	service, err := botusecase.New(botusecase.Config{
		AccessPolicy:  domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits: domain.ContextLimits{MaxMessages: 30, MaxChars: 20_000}, RetainMessages: 100, MaxConcurrentCalls: 1,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}, botusecase.Dependencies{Store: store, Agent: agent, Publisher: integrationPublisher{}, Memory: memoryService})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		eventID   string
		timestamp string
		query     string
	}{
		{eventID: "third-person", timestamp: "1700000000.000002", query: "Can you tell me what you know about Dauno?"},
		{eventID: "mixed", timestamp: "1700000000.000003", query: "What do you know about me and Dauno?"},
	} {
		if outcome, err := service.Handle(t.Context(), dmInvocation(test.eventID, "D87654321", test.timestamp, test.query)); err != nil || outcome != botusecase.OutcomeResponded {
			t.Fatalf("Handle(%q) = %q, %v", test.query, outcome, err)
		}
		if !containsMemorySlug(agent.memory, "project-atlas") {
			t.Fatalf("FTS topic missing for %q: %#v", test.query, agent.memory)
		}
		if test.eventID == "mixed" && !containsMemorySlug(agent.memory, domain.ScopedPersonTopicSlug("person-dauno", ownerKey)) {
			t.Fatalf("personal topic missing from mixed recall: %#v", agent.memory)
		}
	}
}

func containsMemorySlug(memory []domain.MemorySnippet, slug string) bool {
	for _, snippet := range memory {
		if snippet.Slug == slug {
			return true
		}
	}
	return false
}

type entityMemoryLLM struct{}

func (entityMemoryLLM) GenerateText(context.Context, string) (string, error) {
	return `{"operations":[]}`, nil
}

type memoryRecordingAgent struct{ memory []domain.MemorySnippet }

func (a *memoryRecordingAgent) Respond(_ context.Context, req port.AgentRequest) (string, error) {
	a.memory = append([]domain.MemorySnippet(nil), req.Memory...)
	return "ok", nil
}

var _ port.Agent = (*memoryRecordingAgent)(nil)
