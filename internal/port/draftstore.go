package port

import (
	"context"
	"time"
)

// AgentDraftStatus represents the lifecycle state of a draft.
type AgentDraftStatus string

const (
	DraftStatusDraft            AgentDraftStatus = "draft"
	DraftStatusPreviewed        AgentDraftStatus = "previewed"
	DraftStatusInstallRequested AgentDraftStatus = "install_requested"
	DraftStatusInstalled        AgentDraftStatus = "installed"
	DraftStatusCancelled        AgentDraftStatus = "cancelled"
	DraftStatusExpired          AgentDraftStatus = "expired"
	DraftStatusFailed           AgentDraftStatus = "failed"
)

// AgentDraft is the persisted state of an agent creation flow.
type AgentDraft struct {
	DraftID         string           `json:"draft_id"`
	TeamID          string           `json:"team_id"`
	ActorID         string           `json:"actor_id"`
	ConversationKey string           `json:"conversation_key"`
	Name            string           `json:"name"`
	Description     string           `json:"description"`
	Instruction     string           `json:"instruction"`
	Model           string           `json:"model"`
	DefinitionHash  string           `json:"definition_hash"`
	CatalogRevision int              `json:"catalog_revision"`
	Kind            string           `json:"kind"`
	ExecutionMode   string           `json:"execution_mode"`
	TimeoutSeconds  int              `json:"timeout_seconds"`
	CanonicalYAML   string           `json:"canonical_yaml"`
	Status          AgentDraftStatus `json:"status"`
	CreatedAt       time.Time        `json:"created_at"`
	ExpiresAt       time.Time        `json:"expires_at"`
}

// AgentDraftStore persists agent drafts.
type AgentDraftStore interface {
	Create(ctx context.Context, draft *AgentDraft) error
	Get(ctx context.Context, draftID string) (*AgentDraft, error)
	FindByNameAndDefinitionHash(ctx context.Context, name, definitionHash string) (*AgentDraft, error)
	// MarkPreviewed atomically saves the preview result and transitions from Draft to Previewed.
	MarkPreviewed(ctx context.Context, draftID string, definitionHash string, catalogRevision int) error
	// UpdateStatus atomically transitions from fromStatus to toStatus. Returns error if current status doesn't match.
	UpdateStatus(ctx context.Context, draftID string, fromStatus, toStatus AgentDraftStatus) error
	ExpireDrafts(ctx context.Context, now time.Time) error
}
