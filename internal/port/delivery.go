package port

import (
	"context"
	"errors"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

var ErrIncrementalTextTooLong = errors.New("incremental text exceeds delivery limit")

type ResponsePublisher interface {
	Publish(ctx context.Context, target domain.ReplyTarget, text string) (PublishedResponse, error)
}

type PublishedResponse struct {
	LastMessageTS string
}

type ProgressPublisher interface {
	PublishProgress(ctx context.Context, target domain.ReplyTarget, operation domain.ProgressOperation) (PublishedResponse, error)
	UpdateProgress(ctx context.Context, operation domain.ProgressOperation) error
	RecoverProgress(ctx context.Context, operation domain.ProgressOperation) (PublishedResponse, bool, error)
}

type SuggestedPromptPublisher interface {
	PublishSuggestedPrompts(ctx context.Context, target domain.ReplyTarget, deliveryID string, prompts []string) (PublishedResponse, error)
}

type IncrementalPublisher interface {
	ValidateIncrementalText(text string) error
	CreateIncremental(ctx context.Context, target domain.ReplyTarget, operation domain.IncrementalOperation, text string) (PublishedResponse, error)
	UpdateIncremental(ctx context.Context, operation domain.IncrementalOperation, text string) error
	FinalizeIncremental(ctx context.Context, operation domain.IncrementalOperation, text, assistantCorrelationID string) error
	InterruptIncremental(ctx context.Context, operation domain.IncrementalOperation, text string) error
	RecoverIncremental(ctx context.Context, operation domain.IncrementalOperation) (PublishedResponse, bool, error)
}

type StandardExperienceStore interface {
	CreateProgress(ctx context.Context, operation domain.ProgressOperation) error
	MarkProgressPublished(ctx context.Context, operationID, messageTS string) error
	SetProgressState(ctx context.Context, operationID string, state domain.ProgressState, updatedAt time.Time) error
	ListRecoverableProgress(ctx context.Context) ([]domain.ProgressOperation, error)
	FindWaitingProgress(ctx context.Context, key domain.ConversationKey) (*domain.ProgressOperation, error)
	ClaimSuggestedPrompts(ctx context.Context, teamID, userID string, key domain.ConversationKey, createdAt time.Time) (deliveryID string, claimed bool, err error)
	MarkSuggestedPromptsPublished(ctx context.Context, deliveryID, messageTS string, updatedAt time.Time) error
	PrepareIncremental(ctx context.Context, operation domain.IncrementalOperation) error
	MarkIncrementalCreated(ctx context.Context, operationID, messageTS string, updatedAt time.Time) error
	AdvanceIncremental(ctx context.Context, operationID string, status domain.IncrementalStatus, sequence int, prefixDigest string, updatedAt time.Time) error
	ListUnfinishedIncremental(ctx context.Context) ([]domain.IncrementalOperation, error)
}

type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type Clock interface {
	Now() time.Time
}

// StructuredPublisher renders provider-neutral structured response data
// (Presentation) using provider-specific rich presentation (e.g. Slack
// Block Kit context and table blocks). It is selected by the bot only when
// the turn contains a validated Presentation.
type StructuredPublisher interface {
	ValidateStructured(presentation domain.Presentation) error
	PublishStructured(ctx context.Context, target domain.ReplyTarget, presentation domain.Presentation) (PublishedResponse, error)
}
