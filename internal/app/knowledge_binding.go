package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	knowledgeusecase "github.com/Dauno/slack-local-agent/internal/usecase/knowledge"
)

// composeKnowledgeService builds the knowledge executor behind the
// orchestration.knowledge.enabled gate. The caller must pass the same
// coordinator instance it hands to the bot service so knowledge commands and
// root turns share per-conversation serialization. While the gate is disabled
// the executor answers memory-human commands with a deterministic disabled
// response instead of mutating state or reaching the model.
func composeKnowledgeService(enabled bool, store port.KnowledgeStore, coordinator port.ConversationCoordinator, wakes ...func()) (*knowledgeusecase.Service, error) {
	return knowledgeusecase.New(
		knowledgeusecase.Config{Enabled: enabled},
		knowledgeusecase.Dependencies{Store: store, Coordinator: coordinator, Clock: port.SystemClock{}, Wakes: wakes},
	)
}

// workstreamKnowledgeBindingResolver resolves the trusted knowledge write
// binding for one invocation. An explicit payload project selector becomes
// the binding's project only after validation against the registered project
// set; unregistered selectors never fill the binding and are rejected by the
// knowledge use case. Otherwise the registered project and the active
// actor-bound workstream come only from durable host state: a conversation
// without an active actor-bound workstream, a workstream whose project is no
// longer registered, or a non-owner actor all resolve an empty project
// binding, which defaults knowledge writes to the user scope.
type workstreamKnowledgeBindingResolver struct {
	store   port.WorkstreamStore
	allowed map[string]struct{}
}

var _ port.KnowledgeBindingResolver = workstreamKnowledgeBindingResolver{}
var _ port.KnowledgeRetrievalBindingResolver = workstreamKnowledgeBindingResolver{}

func (r workstreamKnowledgeBindingResolver) ResolveKnowledgeBinding(ctx context.Context, team, actor string, conversationKey domain.ConversationKey, text string) (domain.KnowledgeWriteBinding, error) {
	binding := domain.KnowledgeWriteBinding{Team: team, Actor: actor, Conversation: conversationKey}
	if command, matched, err := knowledgeusecase.ParseHumanCommand(text); matched && err == nil && domain.KnowledgeScopeKind(command.ScopeKind) == domain.KnowledgeScopeProject {
		if _, ok := r.allowed[command.ScopeID]; ok {
			binding.Project = command.ScopeID
		}
		return binding, nil
	}
	if r.store == nil {
		return binding, nil
	}
	workstream, err := r.store.ActiveForConversation(ctx, conversationKey)
	if errors.Is(err, port.ErrWorkstreamNotFound) {
		return binding, nil
	}
	if err != nil {
		return domain.KnowledgeWriteBinding{}, fmt.Errorf("resolve active workstream for knowledge binding: %w", err)
	}
	if err := workstream.ValidateBinding(actor, conversationKey, workstream.Project); err != nil {
		return binding, nil
	}
	if workstream.Status != domain.WorkstreamActive {
		return binding, nil
	}
	if _, ok := r.allowed[workstream.Project]; !ok {
		return binding, nil
	}
	binding.Project = workstream.Project
	binding.WorkstreamID = workstream.ID
	return binding, nil
}

// ResolveRetrievalBinding resolves the trusted retrieval binding for one
// user-origin turn from a single authoritative ActiveForConversation read.
// Payload text never selects project scope: a conversation without an
// active, registered, actor-bound workstream yields the team/actor/
// conversation binding plus a nil snapshot and grants no project or
// workstream scope. The snapshot, when valid, is derived from that same
// read so a concurrent mutation can never mix binding and snapshot.
func (r workstreamKnowledgeBindingResolver) ResolveRetrievalBinding(ctx context.Context, team, actor string, conversation domain.ConversationKey, exchangeTS string) (port.KnowledgeRetrievalBinding, error) {
	binding := domain.KnowledgeWriteBinding{Team: team, Actor: actor, Conversation: conversation}
	if r.store == nil {
		return port.KnowledgeRetrievalBinding{Binding: binding, ExchangeTS: exchangeTS}, nil
	}
	workstream, err := r.store.ActiveForConversation(ctx, conversation)
	if errors.Is(err, port.ErrWorkstreamNotFound) {
		return port.KnowledgeRetrievalBinding{Binding: binding, ExchangeTS: exchangeTS}, nil
	}
	if err != nil {
		return port.KnowledgeRetrievalBinding{}, fmt.Errorf("resolve active workstream for retrieval binding: %w", err)
	}
	if err := workstream.ValidateBinding(actor, conversation, workstream.Project); err != nil {
		return port.KnowledgeRetrievalBinding{Binding: binding, ExchangeTS: exchangeTS}, nil
	}
	if workstream.Status != domain.WorkstreamActive {
		return port.KnowledgeRetrievalBinding{Binding: binding, ExchangeTS: exchangeTS}, nil
	}
	if _, ok := r.allowed[workstream.Project]; !ok {
		return port.KnowledgeRetrievalBinding{Binding: binding, ExchangeTS: exchangeTS}, nil
	}
	binding.Project = workstream.Project
	binding.WorkstreamID = workstream.ID
	snapshot := workstream.Snapshot()
	return port.KnowledgeRetrievalBinding{Binding: binding, Workstream: &snapshot, ExchangeTS: exchangeTS}, nil
}
