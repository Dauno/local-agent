package port

import (
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
	Name       string
	Model      string
	AgentClass string
}

// AgentBuilderService compiles an AgentDraft into a validated agent definition.
// The current definitions snapshot is implementation-owned so the port does
// not depend on the declarative definition package.
type AgentBuilderService interface {
	Preview(draft domain.AgentDraft, current any) (*PreviewResult, error)
}
