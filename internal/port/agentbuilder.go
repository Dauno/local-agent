package port

import (
	"context"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

// PreviewResult holds the result of previewing an agent draft.
type PreviewResult struct {
	AgentDef AgentDefPreview
	YAML     string
	SHA256   string
}

// AgentDefPreview contains the definition identity needed by delivery ports.
type AgentDefPreview struct {
	Name          string
	Model         string
	AgentClass    string
	ExecutionMode string
	TimeoutSec    int
}

// AgentBuilderService compiles an AgentDraft into a validated agent definition.
// The current definitions snapshot comes from the declarative definition package.
type AgentBuilderService interface {
	Preview(draft domain.AgentDraft, defs *agentdef.Definitions) (*PreviewResult, error)
}

// BuilderLauncherRequest contains the context for publishing a builder launcher.
type BuilderLauncherRequest struct {
	Actor           string
	ConversationKey domain.ConversationKey
	IdempotencyKey  string
}

// BuilderLauncherPublisher publishes a message with a button to open the builder modal.
type BuilderLauncherPublisher interface {
	PublishBuilderLauncher(ctx context.Context, req BuilderLauncherRequest) error
}
