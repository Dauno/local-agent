package port

import (
	"context"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// --- Structured agent runtime (replaces Agent.Respond for tool-aware turns) ---

// AgentRequest bundles conversation history, recalled memory, and enriched
// context into one model call. Future facts stay out of the bot use case.
type AgentRequest struct {
	ConversationKey domain.ConversationKey
	Messages        []domain.Message
	Memory          []domain.MemorySnippet
	Context         domain.AgentContext
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
	Probe(ctx context.Context, primaryPath string, additionalPaths []string, configOptions []domain.ACPConfigOption) error
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
