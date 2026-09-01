package port

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

var (
	ErrWorkstreamNotFound             = errors.New("workstream not found")
	ErrWorkstreamCASConflict          = errors.New("workstream CAS conflict")
	ErrWorkstreamConversationConflict = errors.New("workstream conversation already has an active workstream")
	ErrWorkstreamValidation           = errors.New("workstream storage validation failure")
	ErrWorkstreamUnavailable          = errors.New("workstream storage unavailable")
)

// WorkstreamStore persists current workstream state and its append-only
// transition journal. Apply performs validation, current-state CAS, child-row
// persistence, and journal insertion in one transaction.
type WorkstreamStore interface {
	Create(ctx context.Context, workstream domain.Workstream, source domain.WorkstreamTransitionSource, sourceID string) error
	Get(ctx context.Context, workstreamID string) (domain.Workstream, error)
	ActiveForConversation(ctx context.Context, conversationKey domain.ConversationKey) (domain.Workstream, error)
	Apply(ctx context.Context, transition domain.WorkstreamTransition, limits domain.WorkstreamLimits, now time.Time) (domain.WorkstreamTransitionRecord, error)
	Transitions(ctx context.Context, workstreamID string) ([]domain.WorkstreamTransitionRecord, error)
}

// WorkstreamMutator is the host-facing path for explicit human corrections.
// Implementations bind actor and conversation from the trusted invocation.
type WorkstreamMutator interface {
	ApplyHuman(ctx context.Context, binding WorkstreamBinding, transition domain.WorkstreamTransition) (domain.WorkstreamTransitionRecord, domain.WorkstreamSnapshot, error)
}

// WorkstreamSnapshotReader exposes the trusted current snapshot needed to
// build an isolated host activation frame. It derives project binding from the
// durable workstream rather than accepting it from the activation payload.
type WorkstreamSnapshotReader interface {
	SnapshotForActivation(ctx context.Context, workstreamID, actor string, conversationKey domain.ConversationKey) (domain.WorkstreamSnapshot, error)
}

// ExternalAgentJobCompletionBindingResolver resolves a legacy individual-job
// task selector from the active, actor-bound workstream. The job store still
// owns the new atomic association.
type ExternalAgentJobCompletionBindingResolver interface {
	CompletionBindingForTask(ctx context.Context, actor string, conversationKey domain.ConversationKey, project, task string) (domain.ExternalAgentJobCompletionBinding, bool, error)
}

// WorkstreamService is the actor-bound surface used by Slack commands. Creation
// remains explicit and has a trusted event identity distinct from model calls.
type WorkstreamService interface {
	WorkstreamMutator
	WorkstreamSnapshotReader
	ExternalAgentJobCompletionBindingResolver
	CreateHuman(ctx context.Context, binding WorkstreamBinding, id, objective, sourceID string) (domain.WorkstreamSnapshot, error)
}

type WorkstreamBinding struct {
	Actor           string
	ConversationKey domain.ConversationKey
	Project         string
}

func (b WorkstreamBinding) Validate() error {
	if strings.TrimSpace(b.Actor) == "" || strings.TrimSpace(string(b.ConversationKey)) == "" || strings.TrimSpace(b.Project) == "" {
		return errors.New("workstream binding requires actor, conversation, and project")
	}
	return nil
}
