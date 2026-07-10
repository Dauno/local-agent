package bot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type fakeClock struct{ now time.Time }

func (c fakeClock) Now() time.Time { return c.now }

type fakeStore struct {
	mu           sync.Mutex
	claimed      bool
	claimAll     bool
	claimCalls   int
	hasAssistant bool
	hasCalls     int
	recent       map[domain.ConversationKey][]domain.Message
	appended     []domain.Message
	appendedMeta []domain.ConversationMetadata
}

func (s *fakeStore) ClaimDedupe(context.Context, []string, time.Time, time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	if s.claimAll {
		return true, nil
	}
	if s.claimed {
		return false, nil
	}
	s.claimed = true
	return true, nil
}
func (s *fakeStore) HasAssistantMessage(context.Context, domain.ConversationKey) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hasCalls++
	return s.hasAssistant, nil
}
func (s *fakeStore) RecentMessages(_ context.Context, key domain.ConversationKey, _ int) ([]domain.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Message(nil), s.recent[key]...), nil
}
func (s *fakeStore) AppendMessage(_ context.Context, metadata domain.ConversationMetadata, message domain.Message, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appended = append(s.appended, message)
	s.appendedMeta = append(s.appendedMeta, metadata)
	return nil
}
func (*fakeStore) CleanupDedupe(context.Context, time.Time) error { return nil }

type fakeAgent struct {
	response string
	err      error
	calls    int
	context  []domain.Message
	block    <-chan struct{}
	started  chan<- struct{}
}

func (a *fakeAgent) Respond(ctx context.Context, messages []domain.Message) (string, error) {
	a.calls++
	a.context = append([]domain.Message(nil), messages...)
	if a.started != nil {
		a.started <- struct{}{}
	}
	if a.block != nil {
		select {
		case <-a.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return a.response, a.err
}

type fakeHistory struct {
	history port.History
	calls   int
}

func (h *fakeHistory) RecentHistory(context.Context, domain.Invocation, domain.ContextLimits) (port.History, error) {
	h.calls++
	return h.history, nil
}

type publishedCall struct {
	target domain.ReplyTarget
	text   string
}

type fakePublisher struct {
	mu    sync.Mutex
	calls []publishedCall
	err   error
}

func (p *fakePublisher) Publish(_ context.Context, target domain.ReplyTarget, text string) (port.PublishedResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, publishedCall{target, text})
	return port.PublishedResponse{LastMessageTS: "1700000002.000003"}, p.err
}

func botInvocation() domain.Invocation {
	return domain.Invocation{
		EventID: "Ev1", EventType: "message.im", TeamID: "T12345678",
		ChannelID: "D12345678", ChannelKind: domain.ChannelDM, UserID: "U12345678",
		EventTS: "1700000000.000001", Text: "hello", Trigger: domain.TriggerDirectMessage,
	}
}

func newTestService(t *testing.T, store *fakeStore, agent *fakeAgent, history *fakeHistory, publisher *fakePublisher, mutate func(*Config)) *Service {
	t.Helper()
	cfg := Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 30, MaxChars: 20000},
		RetainMessages: 100, MaxConcurrentCalls: 4,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	service, err := New(cfg, Dependencies{
		Store: store, Agent: agent, History: history, Publisher: publisher,
		Clock: fakeClock{now: time.Unix(1700000000, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestHandleAuthorizedDM(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	agent := &fakeAgent{response: "answer"}
	history := &fakeHistory{}
	publisher := &fakePublisher{}
	service := newTestService(t, store, agent, history, publisher, nil)

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if agent.calls != 1 || len(agent.context) != 1 || agent.context[0].Content != "hello" {
		t.Fatalf("unexpected model calls/context: %d %#v", agent.calls, agent.context)
	}
	if len(store.appended) != 2 || store.appended[0].Role != domain.RoleUser || store.appended[1].Role != domain.RoleAssistant {
		t.Fatalf("unexpected persisted messages: %#v", store.appended)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].target.ThreadTS != "" || publisher.calls[0].text != "answer" {
		t.Fatalf("unexpected publishes: %#v", publisher.calls)
	}
}

func TestHandleAuthorizedMentionRepliesInItsThread(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	agent := &fakeAgent{response: "channel answer"}
	publisher := &fakePublisher{}
	service := newTestService(t, store, agent, &fakeHistory{}, publisher, nil)
	invocation := botInvocation()
	invocation.EventType = "app_mention"
	invocation.ChannelID = "C12345678"
	invocation.ChannelKind = domain.ChannelPublic
	invocation.Trigger = domain.TriggerMention

	outcome, err := service.Handle(t.Context(), invocation)
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.calls) != 1 || publisher.calls[0].target.ChannelID != invocation.ChannelID || publisher.calls[0].target.ThreadTS != invocation.EventTS {
		t.Fatalf("mention response target=%#v", publisher.calls)
	}
}

func TestUnauthorizedClaimsDedupeButTouchesNoConversation(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	agent := &fakeAgent{response: "must not run"}
	history := &fakeHistory{}
	publisher := &fakePublisher{}
	service := newTestService(t, store, agent, history, publisher, func(cfg *Config) {
		cfg.AccessPolicy.AllowedUserIDs = []string{"U99999999"}
	})

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeDenied {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if store.claimCalls != 1 || store.hasCalls != 0 || len(store.appended) != 0 || agent.calls != 0 || history.calls != 0 {
		t.Fatalf("unauthorized side effects: store=%#v agent=%d history=%d", store, agent.calls, history.calls)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != "denied" {
		t.Fatalf("unexpected denial publish: %#v", publisher.calls)
	}
	outcome, err = service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeDuplicate || len(publisher.calls) != 1 {
		t.Fatalf("duplicate denial outcome=%q err=%v publishes=%d", outcome, err, len(publisher.calls))
	}
}

func TestDuplicateHasNoVisibleOrModelEffect(t *testing.T) {
	store := &fakeStore{claimed: true, recent: make(map[domain.ConversationKey][]domain.Message)}
	agent := &fakeAgent{response: "answer"}
	publisher := &fakePublisher{}
	service := newTestService(t, store, agent, &fakeHistory{}, publisher, nil)
	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeDuplicate || agent.calls != 0 || len(publisher.calls) != 0 {
		t.Fatalf("duplicate processing: outcome=%q err=%v calls=%d publishes=%d", outcome, err, agent.calls, len(publisher.calls))
	}
}

func TestThreadFollowupCanRecoverParticipation(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	agent := &fakeAgent{response: "answer"}
	history := &fakeHistory{history: port.History{
		BotParticipated: true,
		Messages:        []domain.Message{{Role: domain.RoleAssistant, Content: "previous", ExternalTS: "1699999999.000001"}},
	}}
	publisher := &fakePublisher{}
	service := newTestService(t, store, agent, history, publisher, nil)
	i := botInvocation()
	i.EventID, i.EventType, i.ChannelID, i.ChannelKind = "Ev2", "message.channels", "C12345678", domain.ChannelPublic
	i.EventTS, i.ThreadTS, i.Trigger = "1700000001.000002", "1700000000.000001", domain.TriggerThreadReply

	outcome, err := service.Handle(t.Context(), i)
	if err != nil || outcome != OutcomeResponded || history.calls != 1 {
		t.Fatalf("outcome=%q err=%v history=%d", outcome, err, history.calls)
	}
	if len(agent.context) != 2 || agent.context[0].Content != "previous" {
		t.Fatalf("recovered context not passed to model: %#v", agent.context)
	}
}

func TestModelFailureKeepsOnlyUserMessage(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	agent := &fakeAgent{err: errors.New("upstream failed")}
	publisher := &fakePublisher{}
	service := newTestService(t, store, agent, &fakeHistory{}, publisher, nil)
	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeModelFailed {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if len(store.appended) != 1 || store.appended[0].Role != domain.RoleUser {
		t.Fatalf("unexpected persistence: %#v", store.appended)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != "model error" {
		t.Fatalf("unexpected model error response: %#v", publisher.calls)
	}
}

func TestPublishFailureDoesNotPersistAssistant(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	publisher := &fakePublisher{err: errors.New("Slack unavailable")}
	service := newTestService(t, store, &fakeAgent{response: "answer"}, &fakeHistory{}, publisher, nil)
	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomePublishFailed {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if len(store.appended) != 1 || store.appended[0].Role != domain.RoleUser {
		t.Fatalf("assistant was persisted after publish failure: %#v", store.appended)
	}
}

func TestSecretsAreSanitizedOnlyForPersistence(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	agent := &fakeAgent{response: "answer contains xoxb-sensitive-token"}
	publisher := &fakePublisher{}
	service := newTestService(t, store, agent, &fakeHistory{}, publisher, nil)
	service.sanitize = func(value string) string {
		return strings.ReplaceAll(value, "xoxb-sensitive-token", "xoxb-****oken")
	}
	invocation := botInvocation()
	invocation.Text = "inspect xoxb-sensitive-token"
	outcome, err := service.Handle(t.Context(), invocation)
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if agent.context[0].Content != invocation.Text {
		t.Fatalf("model did not receive the authorized current message: %#v", agent.context)
	}
	for _, message := range store.appended {
		if strings.Contains(message.Content, "xoxb-sensitive-token") {
			t.Fatalf("raw secret persisted: %#v", store.appended)
		}
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.calls) != 1 || strings.Contains(publisher.calls[0].text, "xoxb-sensitive-token") {
		t.Fatalf("raw secret posted to Slack: %#v", publisher.calls)
	}
}

func TestHandleReturnsBusyWithoutPersistingOrQueueing(t *testing.T) {
	tests := []struct {
		name          string
		secondChannel string
		maxCalls      int
	}{
		{name: "global limit", secondChannel: "D87654321", maxCalls: 1},
		{name: "per conversation limit", secondChannel: "D12345678", maxCalls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{claimAll: true, recent: make(map[domain.ConversationKey][]domain.Message)}
			block := make(chan struct{})
			started := make(chan struct{}, 1)
			agent := &fakeAgent{response: "answer", block: block, started: started}
			publisher := &fakePublisher{}
			service := newTestService(t, store, agent, &fakeHistory{}, publisher, func(cfg *Config) {
				cfg.MaxConcurrentCalls = tt.maxCalls
			})

			firstDone := make(chan error, 1)
			go func() {
				outcome, err := service.Handle(t.Context(), botInvocation())
				if err == nil && outcome != OutcomeResponded {
					err = errors.New("first invocation did not respond")
				}
				firstDone <- err
			}()
			<-started

			second := botInvocation()
			second.EventID = "Ev2"
			second.EventTS = "1700000001.000002"
			second.ChannelID = tt.secondChannel
			outcome, err := service.Handle(t.Context(), second)
			if err != nil || outcome != OutcomeBusy {
				t.Fatalf("second outcome=%q err=%v", outcome, err)
			}
			store.mu.Lock()
			persistedWhileBusy := len(store.appended)
			store.mu.Unlock()
			if persistedWhileBusy != 1 || agent.calls != 1 {
				t.Fatalf("busy invocation persisted or invoked model: messages=%d model_calls=%d", persistedWhileBusy, agent.calls)
			}
			publisher.mu.Lock()
			busyPublished := len(publisher.calls) == 1 && publisher.calls[0].text == "busy"
			publisher.mu.Unlock()
			if !busyPublished {
				t.Fatalf("configured busy response was not published")
			}

			close(block)
			if err := <-firstDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}
