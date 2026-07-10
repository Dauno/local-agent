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

type Agent interface {
	Respond(ctx context.Context, messages []domain.Message) (string, error)
}

type History struct {
	Messages        []domain.Message
	BotParticipated bool
}

type HistoryReader interface {
	RecentHistory(ctx context.Context, invocation domain.Invocation, limits domain.ContextLimits) (History, error)
}

type ResponsePublisher interface {
	Publish(ctx context.Context, target domain.ReplyTarget, text string) (PublishedResponse, error)
}

type PublishedResponse struct {
	LastMessageTS string
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
