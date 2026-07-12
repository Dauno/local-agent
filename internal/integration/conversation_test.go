package integration_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
)

func TestConversationContextSurvivesRestartAndRemainsIsolated(t *testing.T) {
	database := filepath.Join(t.TempDir(), "local-agent.db")
	store, err := adaptersqlite.Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	firstAgent := &recordingAgent{responses: []string{"first answer"}}
	firstService := integrationService(t, store, firstAgent)
	if outcome, err := firstService.Handle(t.Context(), dmInvocation("Ev1", "D12345678", "1700000000.000001", "first question")); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("first outcome=%q err=%v", outcome, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := adaptersqlite.OpenExisting(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	secondAgent := &recordingAgent{responses: []string{"second answer", "isolated answer"}}
	secondService := integrationService(t, reopened, secondAgent)
	if outcome, err := secondService.Handle(t.Context(), dmInvocation("Ev2", "D12345678", "1700000001.000002", "second question")); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("second outcome=%q err=%v", outcome, err)
	}
	if outcome, err := secondService.Handle(t.Context(), dmInvocation("Ev3", "D87654321", "1700000002.000003", "other conversation")); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("isolated outcome=%q err=%v", outcome, err)
	}

	contexts := secondAgent.contextsSnapshot()
	if len(contexts) != 2 {
		t.Fatalf("model contexts=%d", len(contexts))
	}
	want := []struct {
		role    domain.Role
		content string
	}{{domain.RoleUser, "first question"}, {domain.RoleAssistant, "first answer"}, {domain.RoleUser, "second question"}}
	if len(contexts[0]) != len(want) {
		t.Fatalf("restored context=%#v", contexts[0])
	}
	for index := range want {
		if contexts[0][index].Role != want[index].role || contexts[0][index].Content != want[index].content {
			t.Fatalf("restored context[%d]=%#v want=%#v", index, contexts[0][index], want[index])
		}
	}
	if len(contexts[1]) != 1 || contexts[1][0].Content != "other conversation" {
		t.Fatalf("conversation context leaked across DMs: %#v", contexts[1])
	}
}

func integrationService(t *testing.T, store port.ConversationStore, agent port.Agent) *botusecase.Service {
	t.Helper()
	service, err := botusecase.New(botusecase.Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 30, MaxChars: 20_000},
		RetainMessages: 100, MaxConcurrentCalls: 4,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}, botusecase.Dependencies{Store: store, Agent: agent, Publisher: integrationPublisher{}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func dmInvocation(eventID, channelID, timestamp, text string) domain.Invocation {
	return domain.Invocation{
		EventID: eventID, EventType: "message.im", TeamID: "T12345678",
		ChannelID: channelID, ChannelKind: domain.ChannelDM, UserID: "U12345678",
		EventTS: timestamp, Text: text, Trigger: domain.TriggerDirectMessage,
	}
}

type recordingAgent struct {
	mu        sync.Mutex
	responses []string
	contexts  [][]domain.Message
}

func (a *recordingAgent) Respond(_ context.Context, req port.AgentRequest) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.contexts = append(a.contexts, append([]domain.Message(nil), req.Messages...))
	response := a.responses[0]
	a.responses = a.responses[1:]
	return response, nil
}

func (a *recordingAgent) contextsSnapshot() [][]domain.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([][]domain.Message, len(a.contexts))
	for index := range a.contexts {
		result[index] = append([]domain.Message(nil), a.contexts[index]...)
	}
	return result
}

type integrationPublisher struct{}

func (integrationPublisher) Publish(_ context.Context, _ domain.ReplyTarget, _ string) (port.PublishedResponse, error) {
	return port.PublishedResponse{LastMessageTS: "1700000010.000010"}, nil
}
