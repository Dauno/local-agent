package port

import (
	"context"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

type ConversationStore interface {
	ClaimDedupe(ctx context.Context, keys []string, createdAt, expiresAt time.Time) (bool, error)
	HasAssistantMessage(ctx context.Context, key domain.ConversationKey) (bool, error)
	RecentMessages(ctx context.Context, key domain.ConversationKey, limit int) ([]domain.Message, error)
	AppendMessage(ctx context.Context, metadata domain.ConversationMetadata, message domain.Message, retain int) error
	CleanupDedupe(ctx context.Context, now time.Time) error
}

// JobCompletionMessageWriter makes the host-originated input idempotent. The
// external activation ID is used as the durable message key, not Slack data.
type JobCompletionMessageWriter interface {
	AppendJobCompletionMessage(ctx context.Context, metadata domain.ConversationMetadata, message domain.Message, retain int) error
}

type History struct {
	Messages        []domain.Message
	BotParticipated bool
}

type HistoryReader interface {
	RecentHistory(ctx context.Context, invocation domain.Invocation, limits domain.ContextLimits) (History, error)
}

// PreparedAssistantExchange is returned before publication. CorrelationID is
// attached to every Slack chunk and is required for crash recovery.
type PreparedAssistantExchange struct {
	ID            string
	CorrelationID string
}

// AssistantExchangeWriter durably stages an assistant exchange before it is
// published, then finalizes it and its curation work item after publishing.
// A staged exchange can be reconciled if the post-publish database write fails.
type AssistantExchangeWriter interface {
	PrepareAssistantExchange(ctx context.Context, metadata domain.ConversationMetadata, message domain.Message, retain int) (PreparedAssistantExchange, error)
	PrepareStructuredAssistantExchange(ctx context.Context, metadata domain.ConversationMetadata, message domain.Message, presentationJSON string, retain int) (PreparedAssistantExchange, error)
	MarkAssistantExchangePublished(ctx context.Context, intentID, assistantTS string) error
	FinalizeAssistantExchange(ctx context.Context, intentID string) error
	DiscardAssistantExchange(ctx context.Context, intentID string) error
	ReconcileAssistantExchanges(ctx context.Context, finder AssistantExchangeFinder) error
}

// AssistantExchangeIntent is the bounded data required to prove that a
// prepared reply was accepted by Slack after a process crash.
type AssistantExchangeIntent struct {
	ID               string
	ChannelID        string
	ChannelKind      domain.ChannelKind
	RootTS           string
	Content          string
	CorrelationID    string
	PresentationJSON string
}

// AssistantExchangeFinder returns an actual Slack timestamp only when every
// Slack reply chunk exposes the exact durable CorrelationID. Content and time
// alone must never finalize a prepared exchange.
type AssistantExchangeFinder interface {
	FindPublishedAssistantExchange(ctx context.Context, intent AssistantExchangeIntent) (assistantTS string, found bool, err error)
}
