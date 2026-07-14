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

func TestMemoryContextFitsExactRenderedUnicodeBudget(t *testing.T) {
	snippets := []domain.MemorySnippet{{Title: "Topic", Slug: "topic", RevisionNumber: 1, Content: "abcdef🚀"}}
	full := domain.RenderMemoryReference(snippets)
	result := domain.FitMemorySnippets(snippets, len([]rune(full))-1)
	if len(result) != 1 || result[0].Content != "abcdef" {
		t.Fatalf("FitMemorySnippets() = %#v", result)
	}
	if got := len([]rune(domain.RenderMemoryReference(result))); got > len([]rune(full))-1 {
		t.Fatalf("rendered memory has %d runes, exceeds budget", got)
	}
	if result := domain.FitMemorySnippets(snippets, 1); len(result) != 0 {
		t.Fatalf("FitMemorySnippets() with no room = %#v", result)
	}
}

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
	response  string
	err       error
	calls     int
	context   []domain.Message
	memory    []domain.MemorySnippet
	agentCtx  domain.AgentContext
	block     <-chan struct{}
	started   chan<- struct{}
	onRespond func()
}

type fakeRuntime struct {
	runTurn        port.AgentTurn
	resumeTurn     port.AgentTurn
	err            error
	runRequest     port.AgentRequest
	resumeDecision domain.ConfirmationDecision
	runCalls       int
	resumeCalls    int
}

func (r *fakeRuntime) Run(_ context.Context, request port.AgentRequest) (port.AgentTurn, error) {
	r.runCalls++
	r.runRequest = request
	return r.runTurn, r.err
}

func (r *fakeRuntime) Resume(_ context.Context, decision domain.ConfirmationDecision) (port.AgentTurn, error) {
	r.resumeCalls++
	r.resumeDecision = decision
	return r.resumeTurn, r.err
}

type fakeConfirmationStore struct {
	delivery *port.ConfirmationDelivery
	pending  []port.ConfirmationDelivery
}

func (*fakeConfirmationStore) CreateDelivery(context.Context, port.ConfirmationDelivery) error {
	return nil
}
func (s *fakeConfirmationStore) MarkPublished(_ context.Context, wrapperCallID, correlationID string) error {
	if s.delivery != nil && s.delivery.WrapperCallID == wrapperCallID {
		s.delivery.Status = port.ConfirmationPublished
		s.delivery.CorrelationID = correlationID
	}
	for index := range s.pending {
		if s.pending[index].WrapperCallID == wrapperCallID {
			s.pending[index].Status = port.ConfirmationPublished
			s.pending[index].CorrelationID = correlationID
		}
	}
	return nil
}
func (s *fakeConfirmationStore) MarkConsumed(_ context.Context, _ string) error {
	s.delivery.Status = port.ConfirmationConsumed
	return nil
}
func (s *fakeConfirmationStore) RejectDelivery(_ context.Context, _ string) error {
	s.delivery.Status = port.ConfirmationRejected
	return nil
}
func (s *fakeConfirmationStore) GetByWrapperCallID(context.Context, string) (*port.ConfirmationDelivery, error) {
	return s.delivery, nil
}
func (s *fakeConfirmationStore) ListPending(context.Context) ([]port.ConfirmationDelivery, error) {
	return append([]port.ConfirmationDelivery(nil), s.pending...), nil
}
func (*fakeConfirmationStore) ExpireDeliveries(context.Context, time.Time) error { return nil }

type fakeExchangeFinder struct {
	found bool
	seen  []port.AssistantExchangeIntent
}

func (f *fakeExchangeFinder) FindPublishedAssistantExchange(_ context.Context, intent port.AssistantExchangeIntent) (string, bool, error) {
	f.seen = append(f.seen, intent)
	return "1700000000.000001", f.found, nil
}

type fakeExchangeWriter struct {
	calls       int
	prepares    int
	published   int
	discards    int
	metadata    domain.ConversationMetadata
	message     domain.Message
	prepared    port.PreparedAssistantExchange
	err         error
	onAppend    func()
	publishedTS string
}

func (w *fakeExchangeWriter) PrepareAssistantExchange(_ context.Context, _ domain.ConversationMetadata, _ domain.Message, _ int) (port.PreparedAssistantExchange, error) {
	w.prepares++
	if w.prepared.ID == "" {
		w.prepared = port.PreparedAssistantExchange{ID: "intent", CorrelationID: "intent-correlation"}
	}
	return w.prepared, nil
}

func (w *fakeExchangeWriter) MarkAssistantExchangePublished(_ context.Context, _ string, assistantTS string) error {
	w.published++
	w.publishedTS = assistantTS
	return nil
}

func (w *fakeExchangeWriter) FinalizeAssistantExchange(_ context.Context, _ string) error {
	w.calls++
	if w.onAppend != nil {
		w.onAppend()
	}
	return w.err
}

func (w *fakeExchangeWriter) DiscardAssistantExchange(context.Context, string) error {
	w.discards++
	return nil
}
func (*fakeExchangeWriter) ReconcileAssistantExchanges(context.Context, port.AssistantExchangeFinder) error {
	return nil
}

func (a *fakeAgent) Respond(ctx context.Context, req port.AgentRequest) (string, error) {
	a.calls++
	a.context = append([]domain.Message(nil), req.Messages...)
	a.memory = append([]domain.MemorySnippet(nil), req.Memory...)
	a.agentCtx = req.Context
	if a.onRespond != nil {
		a.onRespond()
	}
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

type fakeRecall struct{ snippets []domain.MemorySnippet }

func (r fakeRecall) Recall(context.Context, string, string) ([]domain.MemorySnippet, error) {
	return append([]domain.MemorySnippet(nil), r.snippets...), nil
}

type fakeEnricher struct {
	context  domain.AgentContext
	err      error
	calls    int
	onEnrich func()
}

func (e *fakeEnricher) Enrich(context.Context, domain.Invocation) (domain.AgentContext, error) {
	e.calls++
	if e.onEnrich != nil {
		e.onEnrich()
	}
	return e.context, e.err
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
	mu        sync.Mutex
	calls     []publishedCall
	err       error
	onPublish func()
}

func (p *fakePublisher) Publish(_ context.Context, target domain.ReplyTarget, text string) (port.PublishedResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, publishedCall{target, text})
	if p.onPublish != nil {
		p.onPublish()
	}
	return port.PublishedResponse{LastMessageTS: "1700000002.000003"}, p.err
}

type trackingModelCallLimiter struct {
	mu   sync.Mutex
	held bool
}

func (l *trackingModelCallLimiter) TryAcquire() (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		return nil, false
	}
	l.held = true
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.held = false
		})
	}, true
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

func TestHandleRuntimeReceivesCanonicalConversationKey(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	service := newTestService(t, store, &fakeAgent{}, &fakeHistory{}, &fakePublisher{}, nil)
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "durable answer"}}
	service.AddRuntime(runtime, nil)

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if runtime.runCalls != 1 || runtime.runRequest.ConversationKey != "slack:T12345678:dm:D12345678" {
		t.Fatalf("runtime request = %#v", runtime.runRequest)
	}
}

func TestHandleConfirmationBindsActorAndConversation(t *testing.T) {
	invocation := botInvocation()
	key, err := invocation.ConversationKey()
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	service := newTestService(t, store, &fakeAgent{}, &fakeHistory{}, &fakePublisher{}, nil)
	runtime := &fakeRuntime{resumeTurn: port.AgentTurn{Text: "completed"}}
	confirmations := &fakeConfirmationStore{delivery: &port.ConfirmationDelivery{
		WrapperCallID: "wrapper", OriginalCallID: "original", SessionID: "adk:" + string(key),
		Actor: invocation.UserID, ConversationKey: key, Status: port.ConfirmationPublished,
		Expiry: time.Now().Add(time.Hour),
	}}
	service.AddRuntime(runtime, confirmations)

	if outcome := service.HandleConfirmation(t.Context(), invocation, "wrapper", true); outcome != OutcomeResponded {
		t.Fatalf("HandleConfirmation() = %q", outcome)
	}
	if runtime.resumeCalls != 1 || runtime.resumeDecision.Actor != invocation.UserID || runtime.resumeDecision.ConversationKey != key {
		t.Fatalf("resume decision = %#v", runtime.resumeDecision)
	}

	confirmations.delivery.Status = port.ConfirmationPublished
	confirmations.delivery.ConversationKey = "slack:T12345678:dm:D99999999"
	if outcome := service.HandleConfirmation(t.Context(), invocation, "wrapper", true); outcome != OutcomeIgnoredFollowup {
		t.Fatalf("cross-conversation HandleConfirmation() = %q", outcome)
	}
	if runtime.resumeCalls != 1 {
		t.Fatalf("cross-conversation confirmation resumed %d times", runtime.resumeCalls)
	}
}

func TestReconcileConfirmationsRepublishesOnlyUnprovenPendingDelivery(t *testing.T) {
	publisher := &fakePublisher{}
	service := newTestService(t, &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}, &fakeAgent{}, &fakeHistory{}, publisher, nil)
	confirmations := &fakeConfirmationStore{pending: []port.ConfirmationDelivery{{
		WrapperCallID: "wrapper", OriginalCallID: "original", ChannelID: "D12345678",
		Summary: "Delete worktree", Expiry: time.Now().Add(time.Hour), Status: port.ConfirmationPending,
	}}}
	service.AddRuntime(&fakeRuntime{}, confirmations)

	finder := &fakeExchangeFinder{}
	if err := service.ReconcileConfirmations(t.Context(), finder); err != nil {
		t.Fatal(err)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].target.CorrelationID != "confirmation:wrapper" {
		t.Fatalf("republished calls = %#v", publisher.calls)
	}
	if text := publisher.calls[0].text; !strings.Contains(text, "**Call ID**") || strings.Contains(text, "\n*Call ID*") {
		t.Fatalf("confirmation prompt is not standard Markdown: %q", text)
	}
	if confirmations.pending[0].Status != port.ConfirmationPublished {
		t.Fatalf("delivery status = %q", confirmations.pending[0].Status)
	}

	confirmations.pending[0].Status = port.ConfirmationPending
	finder.found = true
	if err := service.ReconcileConfirmations(t.Context(), finder); err != nil {
		t.Fatal(err)
	}
	if len(publisher.calls) != 1 {
		t.Fatalf("proven confirmation was republished: %#v", publisher.calls)
	}
}

func TestHandleUsesAtomicAssistantExchangeWriterWhenMemoryEnabled(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	writer := &fakeExchangeWriter{}
	publisher := &fakePublisher{}
	service := newTestService(t, store, &fakeAgent{response: "answer"}, &fakeHistory{}, publisher, nil)
	service.AddMemory(nil, writer)

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if writer.prepares != 1 || writer.published != 1 || writer.publishedTS != "1700000002.000003" || writer.calls != 1 {
		t.Fatalf("atomic writer = %#v", writer)
	}
	if got := publisher.calls[0].target.CorrelationID; got != "intent-correlation" {
		t.Fatalf("prepared response correlation = %q", got)
	}
	if len(store.appended) != 1 || store.appended[0].Role != domain.RoleUser {
		t.Fatalf("assistant bypassed atomic writer: %#v", store.appended)
	}
}

func TestHandlePublishesOnlyAfterExchangeIntentIsPrepared(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	writer := &fakeExchangeWriter{err: errors.New("database unavailable")}
	preparedAtPublish := false
	publisher := &fakePublisher{onPublish: func() { preparedAtPublish = writer.prepares == 1 }}
	service := newTestService(t, store, &fakeAgent{response: "answer"}, &fakeHistory{}, publisher, nil)
	service.AddMemory(nil, writer)

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err == nil || outcome != "" {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if writer.prepares != 1 || writer.published != 1 || writer.calls != 1 {
		t.Fatalf("exchange writer calls: prepares=%d published=%d finalizes=%d", writer.prepares, writer.published, writer.calls)
	}
	if !preparedAtPublish || len(publisher.calls) != 1 || publisher.calls[0].text != "answer" {
		t.Fatalf("Slack publish did not occur before injected finalization failure: %#v", publisher.calls)
	}
}

func TestHandleEnforcesCombinedMessageAndRenderedMemoryBudget(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	agent := &fakeAgent{response: "answer"}
	service := newTestService(t, store, agent, &fakeHistory{}, &fakePublisher{}, func(cfg *Config) {
		cfg.ContextLimits.MaxChars = 500
	})
	service.AddMemory(fakeRecall{snippets: []domain.MemorySnippet{{Title: "Topic", RevisionNumber: 1, Content: strings.Repeat("é", 200)}}}, nil)
	if outcome, err := service.Handle(t.Context(), botInvocation()); err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if len(agent.memory) != 1 {
		t.Fatalf("memory = %#v", agent.memory)
	}
	if got := len([]rune(agent.context[0].Content)) + len([]rune(domain.RenderMemoryReference(agent.memory))); got > 500 {
		t.Fatalf("combined model context has %d runes, exceeds 500", got)
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

func TestPublishErrorRetainsPreparedExchangeForRecovery(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	writer := &fakeExchangeWriter{}
	publisher := &fakePublisher{err: errors.New("connection closed after Slack accepted reply")}
	service := newTestService(t, store, &fakeAgent{response: "answer"}, &fakeHistory{}, publisher, nil)
	service.AddMemory(nil, writer)

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomePublishFailed {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if writer.prepares != 1 || writer.discards != 0 || writer.published != 0 || writer.calls != 0 {
		t.Fatalf("prepared exchange was not retained for recovery: %#v", writer)
	}
}

func TestSharedModelPermitOnlyCoversAgentRespond(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	limiter := &trackingModelCallLimiter{}
	var agentPermitAvailable, publishReleased, persistReleased bool
	agent := &fakeAgent{response: "answer", onRespond: func() {
		_, agentPermitAvailable = limiter.TryAcquire()
	}}
	publisher := &fakePublisher{onPublish: func() {
		release, acquired := limiter.TryAcquire()
		publishReleased = acquired
		if acquired {
			release()
		}
	}}
	writer := &fakeExchangeWriter{onAppend: func() {
		release, acquired := limiter.TryAcquire()
		persistReleased = acquired
		if acquired {
			release()
		}
	}}
	service := newTestService(t, store, agent, &fakeHistory{}, publisher, nil)
	service.modelCalls = limiter
	service.AddMemory(nil, writer)

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if agentPermitAvailable || !publishReleased || !persistReleased {
		t.Fatalf("shared permit states: agentPermitAvailable=%t publishReleased=%t persistReleased=%t", agentPermitAvailable, publishReleased, persistReleased)
	}
}

func TestEnrichmentRunsBeforeModelTimeoutAndSharedPermit(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	limiter := &trackingModelCallLimiter{}
	var enrichmentPermitAvailable, agentPermitAvailable bool
	enricher := &fakeEnricher{
		context: domain.AgentContext{MaxChars: 10, Facts: []domain.ContextFact{{Key: "k", Value: "v"}}},
	}
	agent := &fakeAgent{response: "answer", onRespond: func() {
		_, agentPermitAvailable = limiter.TryAcquire()
	}}
	service := newTestService(t, store, agent, &fakeHistory{}, &fakePublisher{}, func(cfg *Config) {
		cfg.ModelTimeout = 10 * time.Millisecond
	})
	service.modelCalls = limiter
	service.enricher = enricher
	enricher.onEnrich = func() {
		time.Sleep(20 * time.Millisecond)
		release, acquired := limiter.TryAcquire()
		enrichmentPermitAvailable = acquired
		if acquired {
			release()
		}
	}
	delayedRelease := make(chan struct{})
	go func() {
		time.Sleep(time.Millisecond)
		close(delayedRelease)
	}()
	agent.block = delayedRelease

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if !enrichmentPermitAvailable || agentPermitAvailable || enricher.calls != 1 || len(agent.agentCtx.Facts) != 1 {
		t.Fatalf("enrichment/model permit state: enrichment=%t agent=%t calls=%d context=%#v", enrichmentPermitAvailable, agentPermitAvailable, enricher.calls, agent.agentCtx)
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
