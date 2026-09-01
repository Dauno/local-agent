package port

import (
	"context"
	"errors"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// --- Structured agent runtime (replaces Agent.Respond for tool-aware turns) ---

// AgentTurnOriginKind identifies who or what started a root turn. The ADK
// technical user role is deliberately not used as this provenance field.
type AgentTurnOriginKind string

const (
	AgentTurnOriginUser          AgentTurnOriginKind = "user"
	AgentTurnOriginJobCompletion AgentTurnOriginKind = "job_completion"

	// Event metadata keys are host-owned and must not be taken from model output.
	AgentTurnOriginMetadataKey       = "local_agent_turn_origin"
	AgentTurnActivationIDMetadataKey = "local_agent_activation_id"
)

// AgentTurnOrigin carries trusted host provenance for one root turn. Actor is
// the original job actor for job-completion turns, never a Slack event actor
// supplied later in the pipeline.
type AgentTurnOrigin struct {
	Kind            AgentTurnOriginKind
	Actor           string
	ActivationID    string
	ActivationScope domain.ExternalAgentActivationScope
}

func (o AgentTurnOrigin) Validate() error {
	switch o.Kind {
	case AgentTurnOriginUser:
		if o.ActivationID != "" || o.ActivationScope != "" {
			return errors.New("user turn origin cannot carry activation metadata")
		}
	case AgentTurnOriginJobCompletion:
		if o.ActivationScope == "" {
			// Pre-v45 callers did not carry a scope. Keep those in the
			// workstream instruction path; durable v45 activations always set it.
			o.ActivationScope = domain.ExternalAgentActivationWorkstream
		}
		if !o.ActivationScope.Valid() || o.ActivationScope == domain.ExternalAgentActivationLegacy {
			return errors.New("job-completion turn origin has an invalid activation scope")
		}
		if strings.TrimSpace(o.Actor) == "" {
			return errors.New("job-completion turn origin requires an actor")
		}
		if strings.TrimSpace(o.ActivationID) == "" {
			return errors.New("job-completion turn origin requires an activation ID")
		}
	default:
		return errors.New("agent turn origin kind is required")
	}
	if strings.ContainsAny(o.Actor, "\x00\r\n") || strings.ContainsAny(o.ActivationID, "\x00\r\n") {
		return errors.New("agent turn origin contains control characters")
	}
	return nil
}

// AgentTurnContext is exposed through context.Context to ADK callbacks and
// tools. It is not serialized into the model prompt; durable event metadata is
// added separately by the runtime's session-service wrapper.
type AgentTurnContext struct {
	ConversationKey domain.ConversationKey
	Origin          AgentTurnOrigin
}

type agentTurnContextKey struct{}

func WithAgentTurnContext(ctx context.Context, turn AgentTurnContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, agentTurnContextKey{}, turn)
}

func AgentTurnContextFromContext(ctx context.Context) (AgentTurnContext, bool) {
	if ctx == nil {
		return AgentTurnContext{}, false
	}
	turn, ok := ctx.Value(agentTurnContextKey{}).(AgentTurnContext)
	return turn, ok
}

// AgentRequest bundles conversation history and ephemeral context into one
// model call. Future facts stay out of the bot use case.
type AgentRequest struct {
	ConversationKey domain.ConversationKey
	Origin          AgentTurnOrigin
	Messages        []domain.Message
	Context         domain.AgentContext
	// InternalEvent carries one host-originated event separately from user
	// messages. The runtime exposes it as ephemeral model-role evidence, not
	// as a user utterance or a durable session event.
	InternalEvent string
	// Activation is host-owned durable identity used only to build the
	// activation tool scope. It is not rendered into the model prompt.
	Activation *domain.ExternalAgentJobActivation
	// BeforeModel runs after host context admission and immediately before the
	// runtime makes its first model call. It is used to durably cross the
	// no-replay boundary for host-originated activation turns.
	BeforeModel func(context.Context) error
	// Knowledge carries the ordered complete retrieval frame cards for one
	// authorized human turn. It is ephemeral optional context owned by the
	// before-model path: it is never appended to durable session events and
	// only the compiler may select it into the model-facing request.
	Knowledge []domain.KnowledgeFrameCard
	// WorkstreamRevision is the host-trusted active workstream revision for
	// the turn. Zero means no active actor-bound workstream exists.
	WorkstreamRevision int64
	// WorkstreamSnapshot is the bounded snapshot of the active actor-bound
	// workstream, when one exists. It is ephemeral turn data owned by the
	// before-model path, never appended to durable session events, and
	// untrusted, non-authoritative model input: it grants no tool scope and
	// authorizes no mutation.
	WorkstreamSnapshot *domain.WorkstreamSnapshot
}

// AgentTurn is the structured result of one agent invocation. It carries
// assistant text, a provider-neutral Presentation, or a PendingConfirmation.
type AgentTurn struct {
	Text                string
	PendingConfirmation *domain.PendingConfirmation
	Presentation        *domain.Presentation
}

// AgentRuntime runs one request or resumes a pending confirmation.
type AgentRuntime interface {
	Run(ctx context.Context, request AgentRequest) (AgentTurn, error)
	Resume(ctx context.Context, decision domain.ConfirmationDecision) (AgentTurn, error)
}

// AgentActivationRecovery proves whether a durable ADK session already has a
// final response for one activation. It must never invoke the model.
type AgentActivationRecovery interface {
	RecoverActivation(ctx context.Context, conversationKey domain.ConversationKey, activationID string) (AgentTurn, bool, error)
}

type AgentStreamEventKind string

const (
	AgentStreamTextDelta           AgentStreamEventKind = "text_delta"
	AgentStreamPendingConfirmation AgentStreamEventKind = "pending_confirmation"
	AgentStreamCompleted           AgentStreamEventKind = "completed"
	AgentStreamError               AgentStreamEventKind = "error"
)

type AgentStreamEvent struct {
	Kind      AgentStreamEventKind
	TextDelta string
	Turn      *AgentTurn
	Err       error
}

type StreamingAgentRuntime interface {
	Stream(ctx context.Context, request AgentRequest, yield func(AgentStreamEvent) bool)
}

// ModelCallLimiter bounds all model calls made by one running agent process.
// The composition root supplies one instance to both foreground and background
// model consumers.
type ModelCallLimiter interface {
	TryAcquire() (release func(), acquired bool)
}
