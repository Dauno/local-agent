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

func TestEntityMemoryCuratesSpanishFactAndRecallsItAcrossThreads(t *testing.T) {
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
	topic, err := store.GetTopic(t.Context(), "person-dauno")
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
	if outcome, err := service.Handle(t.Context(), dmInvocation("entity-recall", "D87654321", "1700000000.000002", "¿Dauno?")); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("Handle() = %q, %v", outcome, err)
	}
	if len(agent.memory) != 1 || agent.memory[0].Slug != "person-dauno" || agent.memory[0].Content != "Dauno se identifica como creador de local-agent." {
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

type entityMemoryLLM struct{}

func (entityMemoryLLM) GenerateText(context.Context, string) (string, error) { return "[]", nil }

type memoryRecordingAgent struct{ memory []domain.MemorySnippet }

func (a *memoryRecordingAgent) Respond(_ context.Context, req port.AgentRequest) (string, error) {
	a.memory = append([]domain.MemorySnippet(nil), req.Memory...)
	return "ok", nil
}

var _ port.Agent = (*memoryRecordingAgent)(nil)
