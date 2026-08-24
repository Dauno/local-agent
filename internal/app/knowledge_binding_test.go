package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
)

type knowledgeBindingTestStore struct {
	active domain.Workstream
	err    error
}

func (s *knowledgeBindingTestStore) Create(context.Context, domain.Workstream, domain.WorkstreamTransitionSource, string) error {
	return errors.New("unexpected create")
}
func (s *knowledgeBindingTestStore) Get(context.Context, string) (domain.Workstream, error) {
	return domain.Workstream{}, port.ErrWorkstreamNotFound
}
func (s *knowledgeBindingTestStore) ActiveForConversation(context.Context, domain.ConversationKey) (domain.Workstream, error) {
	return s.active, s.err
}
func (s *knowledgeBindingTestStore) Apply(context.Context, domain.WorkstreamTransition, domain.WorkstreamLimits, time.Time) (domain.WorkstreamTransitionRecord, error) {
	return domain.WorkstreamTransitionRecord{}, errors.New("unexpected apply")
}
func (s *knowledgeBindingTestStore) Transitions(context.Context, string) ([]domain.WorkstreamTransitionRecord, error) {
	return nil, errors.New("unexpected transitions")
}

func knowledgeBindingConversation() domain.ConversationKey {
	return domain.ConversationKey("slack:T12345678:dm:D12345678")
}

func activeWorkstream() domain.Workstream {
	return domain.Workstream{
		ID: "ws-1", ConversationKey: knowledgeBindingConversation(), OwnerActor: "U12345678",
		Project: "workspace", Status: domain.WorkstreamActive, Objective: "ship",
	}
}

const registeredProjectSelector = `memory-human {"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","scope_kind":"project","scope_id":"workspace"}`

const unregisteredProjectSelector = `memory-human {"action":"remember","subject":"api","predicate":"is","value_kind":"string","value_text":"x","scope_kind":"project","scope_id":"unregistered"}`

func TestWorkstreamKnowledgeBindingResolver(t *testing.T) {
	ctx := t.Context()
	allowed := map[string]struct{}{"workspace": {}}
	conversation := knowledgeBindingConversation()
	tests := []struct {
		name  string
		store port.WorkstreamStore
		text  string
		want  domain.KnowledgeWriteBinding
	}{
		{
			name: "no store defaults to user scope",
			text: `memory-human {"action":"inspect"}`,
			want: domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U12345678", Conversation: conversation},
		},
		{
			name:  "no active workstream defaults to user scope",
			store: &knowledgeBindingTestStore{err: port.ErrWorkstreamNotFound},
			text:  `memory-human {"action":"inspect"}`,
			want:  domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U12345678", Conversation: conversation},
		},
		{
			name:  "non-owner actor defaults to user scope",
			store: &knowledgeBindingTestStore{active: activeWorkstream()},
			text:  `memory-human {"action":"inspect"}`,
			want:  domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U99999999", Conversation: conversation},
		},
		{
			name: "inactive workstream defaults to user scope",
			store: &knowledgeBindingTestStore{active: domain.Workstream{
				ID: "ws-1", ConversationKey: conversation, OwnerActor: "U12345678",
				Project: "workspace", Status: domain.WorkstreamProposed,
			}},
			text: `memory-human {"action":"inspect"}`,
			want: domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U12345678", Conversation: conversation},
		},
		{
			name: "unregistered project defaults to user scope",
			store: &knowledgeBindingTestStore{active: domain.Workstream{
				ID: "ws-1", ConversationKey: conversation, OwnerActor: "U12345678",
				Project: "retired", Status: domain.WorkstreamActive,
			}},
			text: `memory-human {"action":"inspect"}`,
			want: domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U12345678", Conversation: conversation},
		},
		{
			name:  "active actor-bound workstream binds project",
			store: &knowledgeBindingTestStore{active: activeWorkstream()},
			text:  `memory-human {"action":"inspect"}`,
			want: domain.KnowledgeWriteBinding{
				Team: "T12345678", Actor: "U12345678", Conversation: conversation,
				Project: "workspace", WorkstreamID: "ws-1",
			},
		},
		{
			name: "explicit registered project selector binds without workstream",
			text: registeredProjectSelector,
			want: domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U12345678", Conversation: conversation, Project: "workspace"},
		},
		{
			name:  "explicit registered project selector wins over the active workstream",
			store: &knowledgeBindingTestStore{active: activeWorkstream()},
			text:  registeredProjectSelector,
			want:  domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U12345678", Conversation: conversation, Project: "workspace"},
		},
		{
			name:  "explicit unregistered project selector is never copied into the binding",
			store: &knowledgeBindingTestStore{active: activeWorkstream()},
			text:  unregisteredProjectSelector,
			want:  domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U12345678", Conversation: conversation},
		},
		{
			name:  "ordinary text falls through to the workstream default",
			store: &knowledgeBindingTestStore{active: activeWorkstream()},
			text:  "hello",
			want: domain.KnowledgeWriteBinding{
				Team: "T12345678", Actor: "U12345678", Conversation: conversation,
				Project: "workspace", WorkstreamID: "ws-1",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := workstreamKnowledgeBindingResolver{store: tt.store, allowed: allowed}
			got, err := resolver.ResolveKnowledgeBinding(ctx, tt.want.Team, tt.want.Actor, conversation, tt.text)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("binding = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestWorkstreamKnowledgeBindingResolverPropagatesStoreFailure(t *testing.T) {
	store := &knowledgeBindingTestStore{err: errors.New("sqlite unavailable")}
	resolver := workstreamKnowledgeBindingResolver{store: store, allowed: map[string]struct{}{"workspace": {}}}
	if _, err := resolver.ResolveKnowledgeBinding(t.Context(), "T12345678", "U12345678", knowledgeBindingConversation(), `memory-human {"action":"inspect"}`); err == nil {
		t.Fatal("expected store failure to propagate")
	}
}

func TestComposeKnowledgeServiceSharesCoordinator(t *testing.T) {
	ctx := t.Context()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(t.TempDir(), "knowledge-compose.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	coordinator := botusecase.NewLimiter(2)
	service, err := composeKnowledgeService(true, adaptersqlite.NewKnowledgeStore(store), coordinator)
	if err != nil {
		t.Fatal(err)
	}
	binding := domain.KnowledgeWriteBinding{
		Team: "T12345678", Actor: "U12345678",
		Conversation: domain.ConversationKey("slack:T12345678:dm:D12345678"),
	}
	release, acquired := coordinator.TryAcquire(string(binding.Conversation))
	if !acquired {
		t.Fatal("coordinator could not be held")
	}
	defer release()
	_, _, err = service.Execute(ctx, binding, "evt-1", "memory-human {\"action\":\"inspect\"}")
	if !errors.Is(err, port.ErrKnowledgeBusy) {
		t.Fatalf("knowledge command under held shared coordinator = %v, want ErrKnowledgeBusy", err)
	}
}

type countingWorkstreamStore struct {
	knowledgeBindingTestStore
	reads int
}

func (s *countingWorkstreamStore) ActiveForConversation(ctx context.Context, conversation domain.ConversationKey) (domain.Workstream, error) {
	s.reads++
	return s.knowledgeBindingTestStore.ActiveForConversation(ctx, conversation)
}

func TestWorkstreamKnowledgeRetrievalBindingResolver(t *testing.T) {
	ctx := t.Context()
	allowed := map[string]struct{}{"workspace": {}}
	conversation := knowledgeBindingConversation()
	exchangeTS := "1710000000.000001"
	tests := []struct {
		name    string
		store   port.WorkstreamStore
		team    string
		actor   string
		want    port.KnowledgeRetrievalBinding
		wantErr bool
	}{
		{
			name:  "no active workstream grants team actor conversation only",
			store: &knowledgeBindingTestStore{err: port.ErrWorkstreamNotFound},
			team:  "T12345678", actor: "U12345678",
			want: port.KnowledgeRetrievalBinding{
				Binding:    domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U12345678", Conversation: conversation},
				ExchangeTS: exchangeTS,
			},
		},
		{
			name:  "valid actor-bound registered workstream grants scope and snapshot",
			store: &knowledgeBindingTestStore{active: activeWorkstream()},
			team:  "T12345678", actor: "U12345678",
			want: func() port.KnowledgeRetrievalBinding {
				snapshot := activeWorkstream().Snapshot()
				return port.KnowledgeRetrievalBinding{
					Binding:    domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U12345678", Conversation: conversation, Project: "workspace", WorkstreamID: "ws-1"},
					Workstream: &snapshot, ExchangeTS: exchangeTS,
				}
			}(),
		},
		{
			name:  "foreign actor grants no project scope",
			store: &knowledgeBindingTestStore{active: activeWorkstream()},
			team:  "T12345678", actor: "U99999999",
			want: port.KnowledgeRetrievalBinding{
				Binding:    domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U99999999", Conversation: conversation},
				ExchangeTS: exchangeTS,
			},
		},
		{
			name:  "inactive workstream grants no project scope",
			store: &knowledgeBindingTestStore{active: func() domain.Workstream { ws := activeWorkstream(); ws.Status = domain.WorkstreamCancelled; return ws }()},
			team:  "T12345678", actor: "U12345678",
			want: port.KnowledgeRetrievalBinding{
				Binding:    domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U12345678", Conversation: conversation},
				ExchangeTS: exchangeTS,
			},
		},
		{
			name:  "unregistered project grants no project scope",
			store: &knowledgeBindingTestStore{active: func() domain.Workstream { ws := activeWorkstream(); ws.Project = "unregistered"; return ws }()},
			team:  "T12345678", actor: "U12345678",
			want: port.KnowledgeRetrievalBinding{
				Binding:    domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U12345678", Conversation: conversation},
				ExchangeTS: exchangeTS,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolver := workstreamKnowledgeBindingResolver{store: tc.store, allowed: allowed}
			got, err := resolver.ResolveRetrievalBinding(ctx, tc.team, tc.actor, conversation, exchangeTS)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ResolveRetrievalBinding() error = %v, want error=%t", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got.Binding != tc.want.Binding || got.ExchangeTS != tc.want.ExchangeTS {
				t.Fatalf("binding = %#v, want %#v", got, tc.want)
			}
			switch {
			case tc.want.Workstream == nil && got.Workstream != nil:
				t.Fatalf("snapshot = %#v, want nil", got.Workstream)
			case tc.want.Workstream != nil && got.Workstream == nil:
				t.Fatalf("snapshot = nil, want %#v", tc.want.Workstream)
			case tc.want.Workstream != nil:
				if got.Workstream.ID != tc.want.Workstream.ID || got.Workstream.Revision != tc.want.Workstream.Revision || got.Workstream.Project != tc.want.Workstream.Project {
					t.Fatalf("snapshot = %#v, want %#v", got.Workstream, tc.want.Workstream)
				}
			}
		})
	}
}

func TestWorkstreamKnowledgeRetrievalBindingResolverPropagatesStoreFailure(t *testing.T) {
	ctx := t.Context()
	resolver := workstreamKnowledgeBindingResolver{
		store:   &knowledgeBindingTestStore{err: errors.New("store exploded")},
		allowed: map[string]struct{}{"workspace": {}},
	}
	if _, err := resolver.ResolveRetrievalBinding(ctx, "T12345678", "U12345678", knowledgeBindingConversation(), "1710000000.000001"); err == nil {
		t.Fatal("ResolveRetrievalBinding() with store failure succeeded")
	}
}

func TestWorkstreamKnowledgeRetrievalBindingResolverSingleRead(t *testing.T) {
	ctx := t.Context()
	store := &countingWorkstreamStore{active: activeWorkstream()}
	resolver := workstreamKnowledgeBindingResolver{store: store, allowed: map[string]struct{}{"workspace": {}}}
	binding, err := resolver.ResolveRetrievalBinding(ctx, "T12345678", "U12345678", knowledgeBindingConversation(), "1710000000.000001")
	if err != nil {
		t.Fatal(err)
	}
	if store.reads != 1 {
		t.Fatalf("store reads = %d, want exactly one authoritative read", store.reads)
	}
	if binding.Workstream == nil || binding.Workstream.Revision != activeWorkstream().Revision {
		t.Fatalf("binding snapshot = %#v, want the same-read snapshot", binding.Workstream)
	}
}
