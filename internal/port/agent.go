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
	Kind         AgentTurnOriginKind
	Actor        string
	ActivationID string
}

func (o AgentTurnOrigin) Validate() error {
	switch o.Kind {
	case AgentTurnOriginUser:
		if o.ActivationID != "" {
			return errors.New("user turn origin cannot carry an activation ID")
		}
	case AgentTurnOriginJobCompletion:
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

// AgentRequest bundles conversation history, recalled memory, and enriched
// context into one model call. Future facts stay out of the bot use case.
type AgentRequest struct {
	ConversationKey domain.ConversationKey
	Origin          AgentTurnOrigin
	Messages        []domain.Message
	Memory          []domain.MemorySnippet
	Context         domain.AgentContext
	// Activation is host-owned durable identity used only to build the
	// activation tool scope. It is not rendered into the model prompt.
	Activation *domain.ExternalAgentJobActivation
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

// ExternalAgentRuntime invokes an ACP-compatible external agent.
type ExternalAgentRuntime interface {
	Run(ctx context.Context, request domain.AcpInvocationRequest) (domain.AcpInvocationResult, error)
	Probe(ctx context.Context, primaryPath string, configOptions []domain.ACPConfigOption) error
	Describe(ctx context.Context) (domain.ACPInitResult, error)
}

// OpenCodeManager handles OpenCode lifecycle operations.
type OpenCodeManager interface {
	Status(ctx context.Context) (domain.OpenCodeManagementResult, error)
	Probe(ctx context.Context) error
	Upgrade(ctx context.Context) (domain.OpenCodeManagementResult, error)
	Rollback(ctx context.Context) (domain.OpenCodeManagementResult, error)
}

type OpenCodeCoordinator interface {
	TryInvocation() (release func(), acquired bool)
	TryMaintenance() (release func(), acquired bool)
}

// ModelCallLimiter bounds all model calls made by one running agent process.
// The composition root supplies one instance to both foreground and background
// model consumers.
type ModelCallLimiter interface {
	TryAcquire() (release func(), acquired bool)
}
