package integration

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
	"github.com/Dauno/slack-local-agent/internal/usecase/knowledge"
	"github.com/Dauno/slack-local-agent/internal/usecase/workstream"
)

type knowledgeBotRuntime struct {
	turn    port.AgentTurn
	err     error
	calls   int
	started chan struct{}
	block   <-chan struct{}
}

func (r *knowledgeBotRuntime) Run(_ context.Context, _ port.AgentRequest) (port.AgentTurn, error) {
	r.calls++
	if r.started != nil {
		r.started <- struct{}{}
	}
	if r.block != nil {
		<-r.block
	}
	return r.turn, r.err
}

func (r *knowledgeBotRuntime) Resume(_ context.Context, _ domain.ConfirmationDecision) (port.AgentTurn, error) {
	return port.AgentTurn{}, errors.New("unexpected resume")
}

type knowledgeBotPublishCall struct {
	target domain.ReplyTarget
	text   string
}

type knowledgeBotPublisher struct {
	mu    sync.Mutex
	calls []knowledgeBotPublishCall
}

func (p *knowledgeBotPublisher) Publish(_ context.Context, target domain.ReplyTarget, text string) (port.PublishedResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, knowledgeBotPublishCall{target: target, text: text})
	return port.PublishedResponse{LastMessageTS: "1700000002.000003"}, nil
}

func (p *knowledgeBotPublisher) snapshot() []knowledgeBotPublishCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]knowledgeBotPublishCall(nil), p.calls...)
}

var knowledgeBotEventSequence atomic.Int64

func knowledgeBotDMInvocation(eventID, text string) domain.Invocation {
	sequence := knowledgeBotEventSequence.Add(1)
	return domain.Invocation{
		EventID: eventID, EventType: "message.im", TeamID: "T12345678",
		ChannelID: "D12345678", ChannelKind: domain.ChannelDM, UserID: "U12345678",
		EventTS: fmt.Sprintf("1700000000.%06d", sequence), Text: text, Trigger: domain.TriggerDirectMessage,
	}
}

type knowledgeBotBindingResolver struct {
	store   port.WorkstreamStore
	allowed map[string]struct{}
	err     error
}

func (r knowledgeBotBindingResolver) ResolveKnowledgeBinding(ctx context.Context, team, actor string, conversationKey domain.ConversationKey, text string) (domain.KnowledgeWriteBinding, error) {
	if r.err != nil {
		return domain.KnowledgeWriteBinding{}, r.err
	}
	binding := domain.KnowledgeWriteBinding{Team: team, Actor: actor, Conversation: conversationKey}
	if command, matched, err := knowledge.ParseHumanCommand(text); matched && err == nil && domain.KnowledgeScopeKind(command.ScopeKind) == domain.KnowledgeScopeProject {
		if _, ok := r.allowed[command.ScopeID]; ok {
			binding.Project = command.ScopeID
		}
		return binding, nil
	}
	if r.store == nil {
		return binding, nil
	}
	active, err := r.store.ActiveForConversation(ctx, conversationKey)
	if errors.Is(err, port.ErrWorkstreamNotFound) {
		return binding, nil
	}
	if err != nil {
		return domain.KnowledgeWriteBinding{}, err
	}
	if err := active.ValidateBinding(actor, conversationKey, active.Project); err != nil {
		return binding, nil //nolint:nilerr // fake mirrors production's graceful degradation on binding mismatch
	}
	if active.Status != domain.WorkstreamActive {
		return binding, nil
	}
	if _, ok := r.allowed[active.Project]; !ok {
		return binding, nil
	}
	binding.Project = active.Project
	binding.WorkstreamID = active.ID
	return binding, nil
}

func newKnowledgeBotService(
	t *testing.T,
	store *adaptersqlite.Store,
	enabled bool,
	bindings port.KnowledgeBindingResolver,
	runtime *knowledgeBotRuntime,
) (*botusecase.Service, *knowledgeBotPublisher, *botusecase.Limiter) {
	t.Helper()
	coordinator := botusecase.NewLimiter(2)
	knowledgeService, err := knowledge.New(knowledge.Config{Enabled: enabled}, knowledge.Dependencies{
		Store: adaptersqlite.NewKnowledgeStore(store), Coordinator: coordinator,
	})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &knowledgeBotPublisher{}
	service, err := botusecase.New(botusecase.Config{
		AccessPolicy:   domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits:  domain.ContextLimits{MaxMessages: 30, MaxChars: 20000},
		RetainMessages: 100, MaxConcurrentCalls: 2,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}, botusecase.Dependencies{
		Store: store, Runtime: runtime, Publisher: publisher,
		Coordinator: coordinator, Knowledge: knowledgeService, KnowledgeBindings: bindings,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, publisher, coordinator
}

func knowledgeClaimScopeRow(t *testing.T, store *adaptersqlite.Store, subject string) (kind, id string) {
	t.Helper()
	row := store.DB().QueryRowContext(t.Context(), `SELECT scope_kind, scope_id FROM knowledge_claims WHERE subject = ?`, subject)
	if err := row.Scan(&kind, &id); err != nil {
		t.Fatalf("claim scope lookup: %v", err)
	}
	return kind, id
}

func knowledgeClaimCount(t *testing.T, store *adaptersqlite.Store) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_claims`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestKnowledgeBotRememberAndInspectViaSlackCommand(t *testing.T) {
	ctx := t.Context()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(t.TempDir(), "knowledge-bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	service, publisher, _ := newKnowledgeBotService(t, store, true, nil, &knowledgeBotRuntime{turn: port.AgentTurn{Text: "unused"}})

	outcome, err := service.Handle(ctx, knowledgeBotDMInvocation("EvK1", `memory-human {"action":"remember","subject":"database","predicate":"runs_on","value_kind":"string","value_text":"pg-01"}`))
	if err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("remember outcome = %q, err = %v", outcome, err)
	}
	calls := publisher.snapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "remembered") {
		t.Fatalf("remember publishes = %#v", calls)
	}
	kind, id := knowledgeClaimScopeRow(t, store, "database")
	if kind != string(domain.KnowledgeScopeUser) {
		t.Fatalf("claim scope kind = %q, want user", kind)
	}
	if id != domain.SlackOwnerKey("slack:T12345678:dm:D12345678", "U12345678") {
		t.Fatalf("claim scope id = %q", id)
	}

	outcome, err = service.Handle(ctx, knowledgeBotDMInvocation("EvK2", `memory-human {"action":"inspect","subject":"database"}`))
	if err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("inspect outcome = %q, err = %v", outcome, err)
	}
	calls = publisher.snapshot()
	if len(calls) != 2 || !strings.Contains(calls[1].text, "runs_on") || !strings.Contains(calls[1].text, "pg-01") {
		t.Fatalf("inspect publishes = %#v", calls)
	}
}

func TestKnowledgeBotRememberAndForgetViaSlackCommand(t *testing.T) {
	ctx := t.Context()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(t.TempDir(), "knowledge-bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	service, publisher, _ := newKnowledgeBotService(t, store, true, nil, &knowledgeBotRuntime{turn: port.AgentTurn{Text: "unused"}})

	if outcome, err := service.Handle(
		ctx,
		knowledgeBotDMInvocation("EvK1", `memory-human {"action":"remember","subject":"secret-plan","predicate":"is","value_kind":"string","value_text":"classified"}`),
	); err != nil ||
		outcome != botusecase.OutcomeResponded {
		t.Fatalf("remember outcome = %q, err = %v", outcome, err)
	}
	if outcome, err := service.Handle(ctx, knowledgeBotDMInvocation("EvK2", `memory-human {"action":"forget","subject":"secret-plan"}`)); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("forget outcome = %q, err = %v", outcome, err)
	}
	calls := publisher.snapshot()
	if len(calls) != 2 || !strings.Contains(calls[1].text, "forgotten") {
		t.Fatalf("forget publishes = %#v", calls)
	}
	var claims int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_claims WHERE subject = 'secret-plan'`).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("claims after forget = %d, %v", claims, err)
	}
	var tombstones int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_tombstones`).Scan(&tombstones); err != nil || tombstones != 1 {
		t.Fatalf("tombstones = %d, %v", tombstones, err)
	}
}

func TestKnowledgeBotDisabledGateMutatesNothingAndSkipsModel(t *testing.T) {
	ctx := t.Context()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(t.TempDir(), "knowledge-bot-disabled.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	runtime := &knowledgeBotRuntime{turn: port.AgentTurn{Text: "unused"}}
	service, publisher, _ := newKnowledgeBotService(t, store, false, nil, runtime)

	outcome, err := service.Handle(ctx, knowledgeBotDMInvocation("EvK1", `memory-human {"action":"remember","subject":"database","predicate":"is","value_kind":"string","value_text":"pg-01"}`))
	if err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if runtime.calls != 0 {
		t.Fatalf("model calls = %d, want 0", runtime.calls)
	}
	calls := publisher.snapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "disabled") {
		t.Fatalf("disabled publishes = %#v", calls)
	}
	if knowledgeClaimCount(t, store) != 0 {
		t.Fatal("disabled command mutated knowledge state")
	}
}

func TestKnowledgeBotProjectBindingAndUnregisteredSelectorRejection(t *testing.T) {
	ctx := t.Context()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(t.TempDir(), "knowledge-bot-project.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	workstreamStore := adaptersqlite.NewWorkstreamStore(store)
	workstreams, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: workstreamStore})
	if err != nil {
		t.Fatal(err)
	}
	binding := workstream.Binding{Actor: "U12345678", ConversationKey: key, Project: "workspace"}
	if _, err := workstreams.CreateHuman(ctx, binding, "ws-1", "knowledge-bound objective", "slack-human:create-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workstreams.ApplyHuman(ctx, binding, domain.WorkstreamTransition{
		WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "slack-human:activate-1", Action: domain.WorkstreamActionActivateWorkstream,
	}); err != nil {
		t.Fatal(err)
	}
	resolver := knowledgeBotBindingResolver{store: workstreamStore, allowed: map[string]struct{}{"workspace": {}}}
	service, publisher, _ := newKnowledgeBotService(t, store, true, resolver, &knowledgeBotRuntime{turn: port.AgentTurn{Text: "unused"}})

	if outcome, err := service.Handle(
		ctx,
		knowledgeBotDMInvocation("EvK1", `memory-human {"action":"remember","subject":"database","predicate":"runs_on","value_kind":"string","value_text":"pg-01"}`),
	); err != nil ||
		outcome != botusecase.OutcomeResponded {
		t.Fatalf("default-project remember outcome = %q, err = %v", outcome, err)
	}
	kind, id := knowledgeClaimScopeRow(t, store, "database")
	if kind != string(domain.KnowledgeScopeProject) || id != "workspace" {
		t.Fatalf("default claim scope = %q:%q, want project:workspace", kind, id)
	}

	before := knowledgeClaimCount(t, store)
	if outcome, err := service.Handle(
		ctx,
		knowledgeBotDMInvocation(
			"EvK2",
			`memory-human {"action":"remember","subject":"cache","predicate":"is","value_kind":"string","value_text":"redis","scope_kind":"project","scope_id":"unregistered"}`,
		),
	); err != nil ||
		outcome != botusecase.OutcomeResponded {
		t.Fatalf("unregistered selector outcome = %q, err = %v", outcome, err)
	}
	calls := publisher.snapshot()
	if len(calls) != 2 || !strings.Contains(calls[1].text, "rejected") {
		t.Fatalf("unregistered selector publishes = %#v", calls)
	}
	if knowledgeClaimCount(t, store) != before {
		t.Fatal("unregistered project selector mutated knowledge state")
	}

	if outcome, err := service.Handle(
		ctx,
		knowledgeBotDMInvocation(
			"EvK3",
			`memory-human {"action":"remember","subject":"cache","predicate":"is","value_kind":"string","value_text":"redis","scope_kind":"project","scope_id":"workspace"}`,
		),
	); err != nil ||
		outcome != botusecase.OutcomeResponded {
		t.Fatalf("registered selector outcome = %q, err = %v", outcome, err)
	}
	kind, id = knowledgeClaimScopeRow(t, store, "cache")
	if kind != string(domain.KnowledgeScopeProject) || id != "workspace" {
		t.Fatalf("explicit claim scope = %q:%q, want project:workspace", kind, id)
	}
}

func TestKnowledgeBotBusyWhileTurnHoldsConversationDoesNotMutate(t *testing.T) {
	ctx := t.Context()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(t.TempDir(), "knowledge-bot-busy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	runtime := &knowledgeBotRuntime{turn: port.AgentTurn{Text: "long answer"}, started: started, block: block}
	service, publisher, _ := newKnowledgeBotService(t, store, true, nil, runtime)

	var wg sync.WaitGroup
	wg.Go(func() {
		if outcome, err := service.Handle(ctx, knowledgeBotDMInvocation("EvK1", "hello")); err != nil || outcome != botusecase.OutcomeResponded {
			t.Errorf("turn outcome = %q, err = %v", outcome, err)
		}
	})
	<-started

	outcome, err := service.Handle(ctx, knowledgeBotDMInvocation("EvK2", `memory-human {"action":"remember","subject":"database","predicate":"is","value_kind":"string","value_text":"pg-01"}`))
	if err != nil || outcome != botusecase.OutcomeBusy {
		t.Fatalf("busy knowledge outcome = %q, err = %v", outcome, err)
	}
	if knowledgeClaimCount(t, store) != 0 {
		t.Fatal("busy knowledge command mutated knowledge state")
	}
	calls := publisher.snapshot()
	if len(calls) != 1 || calls[0].text != "busy" {
		t.Fatalf("busy publishes = %#v", calls)
	}

	close(block)
	wg.Wait()
	calls = publisher.snapshot()
	if len(calls) != 2 || calls[1].text != "long answer" {
		t.Fatalf("turn publishes = %#v", calls)
	}
}

func TestKnowledgeBotRegisteredProjectSelectorWithoutWorkstream(t *testing.T) {
	ctx := t.Context()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(t.TempDir(), "knowledge-bot-selector.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	resolver := knowledgeBotBindingResolver{allowed: map[string]struct{}{"workspace": {}}}
	service, publisher, _ := newKnowledgeBotService(t, store, true, resolver, &knowledgeBotRuntime{turn: port.AgentTurn{Text: "unused"}})

	command := `memory-human {"action":"remember","subject":"database","predicate":"runs_on","value_kind":"string","value_text":"pg-01","scope_kind":"project","scope_id":"workspace"}`
	if outcome, err := service.Handle(ctx, knowledgeBotDMInvocation("EvK1", command)); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("registered selector outcome = %q, err = %v", outcome, err)
	}
	kind, id := knowledgeClaimScopeRow(t, store, "database")
	if kind != string(domain.KnowledgeScopeProject) || id != "workspace" {
		t.Fatalf("claim scope = %q:%q, want project:workspace", kind, id)
	}
	calls := publisher.snapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "remembered") {
		t.Fatalf("publishes = %#v", calls)
	}

	unregistered := `memory-human {"action":"remember","subject":"cache","predicate":"is","value_kind":"string","value_text":"redis","scope_kind":"project","scope_id":"unregistered"}`
	if outcome, err := service.Handle(ctx, knowledgeBotDMInvocation("EvK2", unregistered)); err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("unregistered selector outcome = %q, err = %v", outcome, err)
	}
	calls = publisher.snapshot()
	if len(calls) != 2 || !strings.Contains(calls[1].text, "rejected") {
		t.Fatalf("unregistered selector publishes = %#v", calls)
	}
	var cacheClaims int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_claims WHERE subject = 'cache'`).Scan(&cacheClaims); err != nil || cacheClaims != 0 {
		t.Fatalf("unregistered selector claims = %d, %v", cacheClaims, err)
	}
}

func TestKnowledgeBotPersistedActiveWorkstreamBindsWithoutWorkstreamCommands(t *testing.T) {
	ctx := t.Context()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(t.TempDir(), "knowledge-bot-durable-ws.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	workstreamStore := adaptersqlite.NewWorkstreamStore(store)
	workstreams, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: workstreamStore})
	if err != nil {
		t.Fatal(err)
	}
	binding := workstream.Binding{Actor: "U12345678", ConversationKey: key, Project: "workspace"}
	if _, err := workstreams.CreateHuman(ctx, binding, "ws-durable", "durable knowledge objective", "slack-human:create-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workstreams.ApplyHuman(ctx, binding, domain.WorkstreamTransition{
		WorkstreamID: "ws-durable", ExpectedRevision: 0, SourceID: "slack-human:activate-1", Action: domain.WorkstreamActionActivateWorkstream,
	}); err != nil {
		t.Fatal(err)
	}

	// The workstream command surface is disabled (no Workstreams dependency),
	// but the durable active workstream must still drive the knowledge
	// default scope through read-only binding resolution.
	resolver := knowledgeBotBindingResolver{store: workstreamStore, allowed: map[string]struct{}{"workspace": {}}}
	service, _, _ := newKnowledgeBotService(t, store, true, resolver, &knowledgeBotRuntime{turn: port.AgentTurn{Text: "unused"}})

	if outcome, err := service.Handle(
		ctx,
		knowledgeBotDMInvocation("EvK1", `memory-human {"action":"remember","subject":"database","predicate":"is","value_kind":"string","value_text":"pg-01"}`),
	); err != nil ||
		outcome != botusecase.OutcomeResponded {
		t.Fatalf("remember outcome = %q, err = %v", outcome, err)
	}
	kind, id := knowledgeClaimScopeRow(t, store, "database")
	if kind != string(domain.KnowledgeScopeProject) || id != "workspace" {
		t.Fatalf("claim scope = %q:%q, want project:workspace", kind, id)
	}
}

func TestKnowledgeBotResolverFailureRejectsEnabledRememberWithoutMutation(t *testing.T) {
	ctx := t.Context()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(t.TempDir(), "knowledge-bot-resolver-fail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	resolver := knowledgeBotBindingResolver{allowed: map[string]struct{}{"workspace": {}}, err: errors.New("workstream store unavailable")}
	service, publisher, _ := newKnowledgeBotService(t, store, true, resolver, &knowledgeBotRuntime{turn: port.AgentTurn{Text: "unused"}})

	outcome, err := service.Handle(ctx, knowledgeBotDMInvocation("EvK1", `memory-human {"action":"remember","subject":"database","predicate":"is","value_kind":"string","value_text":"pg-01"}`))
	if err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	calls := publisher.snapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "temporarily unavailable") {
		t.Fatalf("publishes = %#v", calls)
	}
	if knowledgeClaimCount(t, store) != 0 {
		t.Fatal("resolver failure mutated knowledge state")
	}
}

func TestKnowledgeBotResolverFailureRejectsEnabledForgetWithoutMutation(t *testing.T) {
	ctx := t.Context()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(t.TempDir(), "knowledge-bot-resolver-fail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	direct, err := knowledge.New(knowledge.Config{Enabled: true}, knowledge.Dependencies{
		Store: adaptersqlite.NewKnowledgeStore(store), Coordinator: newKnowledgeTestCoordinator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := direct.Execute(ctx, knowledgeTestBinding("U12345678", ""), "evt-seed",
		knowledge.HumanCommandPrefix+`{"action":"remember","subject":"secret-plan","predicate":"is","value_kind":"string","value_text":"classified"}`); err != nil {
		t.Fatal(err)
	}

	resolver := knowledgeBotBindingResolver{allowed: map[string]struct{}{"workspace": {}}, err: errors.New("workstream store unavailable")}
	service, publisher, _ := newKnowledgeBotService(t, store, true, resolver, &knowledgeBotRuntime{turn: port.AgentTurn{Text: "unused"}})

	outcome, err := service.Handle(ctx, knowledgeBotDMInvocation("EvK1", `memory-human {"action":"forget","subject":"secret-plan"}`))
	if err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	calls := publisher.snapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "temporarily unavailable") {
		t.Fatalf("publishes = %#v", calls)
	}
	var claims, tombstones int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_claims WHERE subject = 'secret-plan'`).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("claims after rejected forget = %d, %v; want 1", claims, err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_tombstones`).Scan(&tombstones); err != nil || tombstones != 0 {
		t.Fatalf("tombstones after rejected forget = %d, %v; want 0", tombstones, err)
	}
}

func TestKnowledgeBotResolverFailureWithDisabledGatePublishesDisabled(t *testing.T) {
	ctx := t.Context()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(t.TempDir(), "knowledge-bot-resolver-fail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	resolver := knowledgeBotBindingResolver{allowed: map[string]struct{}{"workspace": {}}, err: errors.New("workstream store unavailable")}
	service, publisher, _ := newKnowledgeBotService(t, store, false, resolver, &knowledgeBotRuntime{turn: port.AgentTurn{Text: "unused"}})

	outcome, err := service.Handle(ctx, knowledgeBotDMInvocation("EvK1", `memory-human {"action":"remember","subject":"database","predicate":"is","value_kind":"string","value_text":"pg-01"}`))
	if err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	calls := publisher.snapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "disabled") {
		t.Fatalf("disabled publishes = %#v", calls)
	}
	if knowledgeClaimCount(t, store) != 0 {
		t.Fatal("disabled command mutated knowledge state")
	}
}
