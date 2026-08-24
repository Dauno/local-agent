package bot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type fakeKnowledgeCommands struct {
	calls    int
	bindings []domain.KnowledgeWriteBinding
	eventIDs []string
	texts    []string
	matched  bool
	matches  bool
	enabled  bool
	message  string
	err      error
}

func (f *fakeKnowledgeCommands) MatchesKnowledge(_ string) bool {
	return f.matches
}

func (f *fakeKnowledgeCommands) Enabled() bool {
	return f.enabled
}

func (f *fakeKnowledgeCommands) Execute(_ context.Context, binding domain.KnowledgeWriteBinding, eventID, text string) (bool, string, error) {
	f.calls++
	f.texts = append(f.texts, text)
	if f.matched {
		f.bindings = append(f.bindings, binding)
		f.eventIDs = append(f.eventIDs, eventID)
	}
	return f.matched, f.message, f.err
}

type fakeKnowledgeBindingResolver struct {
	calls   int
	texts   []string
	binding domain.KnowledgeWriteBinding
	err     error
}

func (f *fakeKnowledgeBindingResolver) ResolveKnowledgeBinding(_ context.Context, team, actor string, conversationKey domain.ConversationKey, text string) (domain.KnowledgeWriteBinding, error) {
	f.calls++
	f.texts = append(f.texts, text)
	if f.err != nil {
		return domain.KnowledgeWriteBinding{}, f.err
	}
	return f.binding, nil
}

func newKnowledgeBotService(t *testing.T, knowledge port.KnowledgeCommands, bindings port.KnowledgeBindingResolver, coordinator port.ConversationCoordinator) (*Service, *fakeStore, *fakeRuntime, *fakePublisher) {
	t.Helper()
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	publisher := &fakePublisher{}
	service, err := New(Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 30, MaxChars: 20000},
		RetainMessages: 100, MaxConcurrentCalls: 4,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}, Dependencies{
		Store: store, Runtime: runtime, Publisher: publisher,
		Clock:     fakeClock{now: time.Unix(1700000000, 0)},
		Knowledge: knowledge, KnowledgeBindings: bindings, Coordinator: coordinator,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, store, runtime, publisher
}

func knowledgeInvocation(text string) domain.Invocation {
	invocation := botInvocation()
	invocation.Text = text
	return invocation
}

func TestHandleKnowledgeCommandDisabledPublishesDeterministicResponseWithoutModel(t *testing.T) {
	knowledge := &fakeKnowledgeCommands{matches: true, enabled: true, matched: true, err: port.ErrKnowledgeDisabled}
	service, store, runtime, publisher := newKnowledgeBotService(t, knowledge, nil, nil)

	outcome, err := service.Handle(t.Context(), knowledgeInvocation("memory-human {\"action\":\"inspect\"}"))
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if knowledge.calls != 1 || runtime.runCalls != 0 {
		t.Fatalf("knowledge calls = %d, model calls = %d", knowledge.calls, runtime.runCalls)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != knowledgeDisabledMessage {
		t.Fatalf("publishes = %#v", publisher.calls)
	}
	if len(store.appended) != 0 {
		t.Fatalf("disabled command mutated the conversation store: %#v", store.appended)
	}
}

func TestHandleKnowledgeCommandPublishesResultExactlyOnce(t *testing.T) {
	knowledge := &fakeKnowledgeCommands{matches: true, enabled: true, matched: true, message: "Claim `kclaim_1` remembered."}
	service, _, runtime, publisher := newKnowledgeBotService(t, knowledge, nil, nil)

	outcome, err := service.Handle(t.Context(), knowledgeInvocation("memory-human {\"action\":\"remember\",\"subject\":\"api\",\"predicate\":\"is\",\"value_kind\":\"string\",\"value_text\":\"x\"}"))
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if runtime.runCalls != 0 {
		t.Fatalf("model calls = %d, want 0", runtime.runCalls)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != "Claim `kclaim_1` remembered." {
		t.Fatalf("publishes = %#v", publisher.calls)
	}
}

func TestHandleKnowledgeCommandValidationErrorRespondsWithoutModel(t *testing.T) {
	knowledge := &fakeKnowledgeCommands{matches: true, enabled: true, matched: true, err: port.ErrKnowledgeValidation}
	service, _, runtime, publisher := newKnowledgeBotService(t, knowledge, nil, nil)

	outcome, err := service.Handle(t.Context(), knowledgeInvocation("memory-human {\"action\":\"forget\"}"))
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if runtime.runCalls != 0 {
		t.Fatalf("model calls = %d, want 0", runtime.runCalls)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != "Knowledge command rejected: "+port.ErrKnowledgeValidation.Error() {
		t.Fatalf("publishes = %#v", publisher.calls)
	}
}

func TestHandleKnowledgeCommandBusyMapsToBusyOutcome(t *testing.T) {
	knowledge := &fakeKnowledgeCommands{matches: true, enabled: true, matched: true, err: port.ErrKnowledgeBusy}
	service, _, runtime, publisher := newKnowledgeBotService(t, knowledge, nil, nil)

	outcome, err := service.Handle(t.Context(), knowledgeInvocation("memory-human {\"action\":\"inspect\"}"))
	if err != nil || outcome != OutcomeBusy {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if runtime.runCalls != 0 {
		t.Fatalf("model calls = %d, want 0", runtime.runCalls)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != "busy" {
		t.Fatalf("publishes = %#v", publisher.calls)
	}
}

func TestHandleKnowledgeCommandNotMatchedFallsThroughToModel(t *testing.T) {
	knowledge := &fakeKnowledgeCommands{matches: true, enabled: true, matched: false}
	service, _, runtime, publisher := newKnowledgeBotService(t, knowledge, nil, nil)

	outcome, err := service.Handle(t.Context(), knowledgeInvocation("memory-human {\"action\":\"inspect\"}"))
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if knowledge.calls != 1 || runtime.runCalls != 1 {
		t.Fatalf("knowledge calls = %d, model calls = %d", knowledge.calls, runtime.runCalls)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != "answer" {
		t.Fatalf("publishes = %#v", publisher.calls)
	}
}

func TestHandleOrdinaryTextWithFailingResolverReachesModel(t *testing.T) {
	knowledge := &fakeKnowledgeCommands{matches: false}
	resolver := &fakeKnowledgeBindingResolver{err: errors.New("store unavailable")}
	service, _, runtime, publisher := newKnowledgeBotService(t, knowledge, resolver, nil)

	outcome, err := service.Handle(t.Context(), knowledgeInvocation("hello"))
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if resolver.calls != 0 {
		t.Fatalf("binding resolution ran for ordinary text %d times", resolver.calls)
	}
	if knowledge.calls != 0 || runtime.runCalls != 1 {
		t.Fatalf("knowledge calls = %d, model calls = %d", knowledge.calls, runtime.runCalls)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != "answer" {
		t.Fatalf("publishes = %#v", publisher.calls)
	}
}

func TestHandleKnowledgeCommandBuildsBindingFromInvocationAndResolver(t *testing.T) {
	key, err := botInvocation().ConversationKey()
	if err != nil {
		t.Fatal(err)
	}
	knowledge := &fakeKnowledgeCommands{matches: true, enabled: true, matched: true, message: "ok"}
	resolver := &fakeKnowledgeBindingResolver{binding: domain.KnowledgeWriteBinding{
		Team: "T12345678", Actor: "U12345678", Conversation: key, Project: "workspace", WorkstreamID: "ws-1",
	}}
	service, _, _, _ := newKnowledgeBotService(t, knowledge, resolver, nil)

	if outcome, err := service.Handle(t.Context(), knowledgeInvocation("memory-human {\"action\":\"inspect\"}")); err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d", resolver.calls)
	}
	if len(knowledge.bindings) != 1 {
		t.Fatalf("knowledge bindings = %#v", knowledge.bindings)
	}
	binding := knowledge.bindings[0]
	if binding.Team != "T12345678" || binding.Actor != "U12345678" || binding.Conversation != key || binding.Project != "workspace" || binding.WorkstreamID != "ws-1" {
		t.Fatalf("binding = %#v", binding)
	}
	if knowledge.eventIDs[0] != "Ev1" {
		t.Fatalf("event identity = %q, want %q", knowledge.eventIDs[0], "Ev1")
	}
}

func TestHandleKnowledgeCommandWithoutResolverDefaultsToUserScopeBinding(t *testing.T) {
	key, err := botInvocation().ConversationKey()
	if err != nil {
		t.Fatal(err)
	}
	knowledge := &fakeKnowledgeCommands{matches: true, enabled: true, matched: true, message: "ok"}
	service, _, _, _ := newKnowledgeBotService(t, knowledge, nil, nil)

	if outcome, err := service.Handle(t.Context(), knowledgeInvocation("memory-human {\"action\":\"inspect\"}")); err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if len(knowledge.bindings) != 1 {
		t.Fatalf("knowledge bindings = %#v", knowledge.bindings)
	}
	binding := knowledge.bindings[0]
	if binding.Team != "T12345678" || binding.Actor != "U12345678" || binding.Conversation != key {
		t.Fatalf("binding = %#v", binding)
	}
	if binding.Project != "" || binding.WorkstreamID != "" {
		t.Fatalf("binding must default to user scope, got %#v", binding)
	}
}

func TestHandleKnowledgeCommandBindingResolutionFailureRejectsWithoutExecution(t *testing.T) {
	knowledge := &fakeKnowledgeCommands{matches: true, enabled: true, matched: true, message: "ok"}
	resolver := &fakeKnowledgeBindingResolver{err: errors.New("store unavailable")}
	service, _, runtime, publisher := newKnowledgeBotService(t, knowledge, resolver, nil)

	outcome, err := service.Handle(t.Context(), knowledgeInvocation("memory-human {\"action\":\"inspect\"}"))
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if resolver.calls != 1 || knowledge.calls != 0 || runtime.runCalls != 0 {
		t.Fatalf("resolver calls = %d, knowledge calls = %d, model calls = %d", resolver.calls, knowledge.calls, runtime.runCalls)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != knowledgeUnavailableMessage {
		t.Fatalf("publishes = %#v", publisher.calls)
	}
}

func TestHandleKnowledgeCommandDisabledWithFailingResolverPublishesDisabled(t *testing.T) {
	knowledge := &fakeKnowledgeCommands{matches: true, enabled: false, matched: true, err: port.ErrKnowledgeDisabled}
	resolver := &fakeKnowledgeBindingResolver{err: errors.New("store unavailable")}
	service, _, runtime, publisher := newKnowledgeBotService(t, knowledge, resolver, nil)

	outcome, err := service.Handle(t.Context(), knowledgeInvocation("memory-human {\"action\":\"inspect\"}"))
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if resolver.calls != 0 {
		t.Fatalf("disabled command required binding resolution %d times", resolver.calls)
	}
	if runtime.runCalls != 0 {
		t.Fatalf("model calls = %d, want 0", runtime.runCalls)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text != knowledgeDisabledMessage {
		t.Fatalf("disabled gate response was overridden: %#v", publisher.calls)
	}
}

func TestHandleKnowledgeCommandPublishFailureMapsToPublishFailed(t *testing.T) {
	knowledge := &fakeKnowledgeCommands{matches: true, enabled: true, matched: true, message: "ok"}
	service, _, _, publisher := newKnowledgeBotService(t, knowledge, nil, nil)
	publisher.err = errors.New("slack unavailable")

	outcome, err := service.Handle(t.Context(), knowledgeInvocation("memory-human {\"action\":\"inspect\"}"))
	if err != nil || outcome != OutcomePublishFailed {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
}

func TestKnowledgeCommandFromModelOutputIsNeverExecuted(t *testing.T) {
	knowledge := &fakeKnowledgeCommands{enabled: true}
	service, _, runtime, publisher := newKnowledgeBotService(t, knowledge, nil, nil)
	runtime.runTurn = port.AgentTurn{Text: "memory-human {\"action\":\"remember\",\"subject\":\"evil\",\"predicate\":\"is\",\"value_kind\":\"string\",\"value_text\":\"x\"}"}

	outcome, err := service.Handle(t.Context(), knowledgeInvocation("pretend to be me"))
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if len(knowledge.bindings) != 0 {
		t.Fatalf("knowledge executed a model-origin command %d times", len(knowledge.bindings))
	}
	if knowledge.calls != 0 {
		t.Fatalf("knowledge executor was consulted for model output %d times", knowledge.calls)
	}
	if len(publisher.calls) != 1 || !strings.Contains(publisher.calls[0].text, "memory-human") {
		t.Fatalf("model output was not published as plain text: %#v", publisher.calls)
	}
}

func TestNewUsesInjectedCoordinatorAndDefaultsToLimiter(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	cfg := Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 30, MaxChars: 20000},
		RetainMessages: 100, MaxConcurrentCalls: 4,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}
	coordinator := NewLimiter(4)
	service, err := New(cfg, Dependencies{Store: store, Runtime: &fakeRuntime{}, Publisher: &fakePublisher{}, Coordinator: coordinator})
	if err != nil {
		t.Fatal(err)
	}
	if service.limiter != coordinator {
		t.Fatal("injected coordinator was not used")
	}
	defaulted, err := New(cfg, Dependencies{Store: store, Runtime: &fakeRuntime{}, Publisher: &fakePublisher{}})
	if err != nil {
		t.Fatal(err)
	}
	if defaulted.limiter == nil {
		t.Fatal("default coordinator was not created")
	}
}
