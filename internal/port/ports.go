package port

import (
	"context"
	"errors"
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

// AgentRequest bundles conversation history, recalled memory, and enriched
// context into one model call. Future facts stay out of the bot use case.
type AgentRequest struct {
	Messages []domain.Message
	Memory   []domain.MemorySnippet
	Context  domain.AgentContext
}

// ContextEnricher resolves a bounded, structured view of the invoking Slack
// user and conversation before a primary model call. Slack API failures and
// missing scopes must never prevent a normal response.
type ContextEnricher interface {
	Enrich(ctx context.Context, invocation domain.Invocation) (domain.AgentContext, error)
}

type Agent interface {
	Respond(ctx context.Context, req AgentRequest) (string, error)
}

// ErrModelCallLimitReached indicates that the process-wide model-call budget is
// exhausted. Callers can use it to apply their own backpressure behavior.
var ErrModelCallLimitReached = errors.New("maximum concurrent model calls reached")

// ModelCallLimiter bounds all model calls made by one running agent process.
// The composition root supplies one instance to both foreground and background
// model consumers.
type ModelCallLimiter interface {
	TryAcquire() (release func(), acquired bool)
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

// PreparedAssistantExchange is returned before publication. CorrelationID is
// attached to every Slack chunk and is required for crash recovery.
type PreparedAssistantExchange struct {
	ID            string
	CorrelationID string
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

// MemoryRetriever provides synchronous recall of curated memory topics.
// It is called before each model invocation and must never block the normal
// response path.
type MemoryRetriever interface {
	Recall(ctx context.Context, query, ownerKey string) ([]domain.MemorySnippet, error)
}

// AssistantExchangeWriter durably stages an assistant exchange before it is
// published, then finalizes it and its curation work item after publishing.
// A staged exchange can be reconciled if the post-publish database write fails.
type AssistantExchangeWriter interface {
	PrepareAssistantExchange(ctx context.Context, metadata domain.ConversationMetadata, message domain.Message, retain int) (PreparedAssistantExchange, error)
	MarkAssistantExchangePublished(ctx context.Context, intentID, assistantTS string) error
	FinalizeAssistantExchange(ctx context.Context, intentID string) error
	DiscardAssistantExchange(ctx context.Context, intentID string) error
	ReconcileAssistantExchanges(ctx context.Context, finder AssistantExchangeFinder) error
}

// AssistantExchangeIntent is the bounded data required to prove that a
// prepared reply was accepted by Slack after a process crash.
type AssistantExchangeIntent struct {
	ID            string
	ChannelID     string
	ChannelKind   domain.ChannelKind
	RootTS        string
	Content       string
	CorrelationID string
}

// AssistantExchangeFinder returns an actual Slack timestamp only when every
// Slack reply chunk exposes the exact durable CorrelationID. Content and time
// alone must never finalize a prepared exchange.
type AssistantExchangeFinder interface {
	FindPublishedAssistantExchange(ctx context.Context, intent AssistantExchangeIntent) (assistantTS string, found bool, err error)
}

// MemoryStore owns topic CRUD, revision history, outbox claims, retention, and
// provenance. It is a low-level data access interface for SQLite-backed memory.
type MemoryStore interface {
	CreateTopic(ctx context.Context, slug, title, description string, tags []string, content, changeReason string) (domain.Topic, error)
	GetTopic(ctx context.Context, slug string) (domain.Topic, error)
	GetTopicByID(ctx context.Context, id domain.TopicID) (domain.Topic, *domain.TopicRevision, error)
	ListTopics(ctx context.Context) ([]domain.Topic, error)
	DeleteTopic(ctx context.Context, id domain.TopicID) error

	AddRevision(ctx context.Context, topicID domain.TopicID, expectedRev int, content, changeReason string) (domain.TopicRevision, error)
	AddEvidence(ctx context.Context, revisionID int, sourceKey domain.ConversationKey, sourceTS, authorID string, evidenceType domain.EvidenceType) (int, error)
	AddEvidenceBatch(ctx context.Context, evidence []domain.Evidence) error
	GetEvidence(ctx context.Context, topicID domain.TopicID) ([]domain.Evidence, error)
	ListRevisions(ctx context.Context, topicID domain.TopicID) ([]domain.TopicRevision, error)

	SearchTopics(ctx context.Context, query string, maxTopics, maxChars int) ([]domain.MemorySnippet, error)
	SearchTopicsForOwner(ctx context.Context, query, ownerKey string, maxTopics, maxChars int) ([]domain.MemorySnippet, error)
	SearchPersonTopicsByOwner(ctx context.Context, ownerKey string, maxTopics, maxChars int) ([]domain.MemorySnippet, error)
	SearchTopicReferences(ctx context.Context, query string, maxTopics int) ([]domain.TopicReference, error)
	GetTopicReference(ctx context.Context, slug string) (*domain.TopicReference, error)
	FindSimilarTopic(ctx context.Context, title string) (*domain.Topic, error)
	TopicExistsBySlug(ctx context.Context, slug string) (bool, error)

	AddTopicLink(ctx context.Context, sourceID, targetID domain.TopicID, relation string, revisionID int) error
	RemoveTopicLink(ctx context.Context, sourceID, targetID domain.TopicID) error
	GetTopicLinks(ctx context.Context, topicID domain.TopicID) ([]domain.TopicLink, error)
	ApplyMemoryPatch(ctx context.Context, patch domain.MemoryPatch, limits domain.MemoryLimits) (bool, error)

	EnqueueOutboxItem(ctx context.Context, conversationKey domain.ConversationKey, exchangeTS string) error
	ClaimNextOutboxItem(ctx context.Context) (*domain.OutboxItem, error)
	LoadOutboxMessages(ctx context.Context, item *domain.OutboxItem) ([]domain.Message, error)
	CompleteOutboxItem(ctx context.Context, id int, leaseUntil time.Time) error
	FailOutboxItem(ctx context.Context, id int, leaseUntil time.Time, reason string) error
	RetryOutboxItem(ctx context.Context, id int, leaseUntil, nextAttempt time.Time) error
	RescheduleOutboxItem(ctx context.Context, id int, leaseUntil, nextAttempt time.Time) error
	CleanupOutbox(ctx context.Context, before time.Time) error
}

// MemoryCurator receives one completed exchange and returns a schema-validated
// patch proposal. It may use an LLM internally but cannot write storage
// directly or change memory policy.
type MemoryCurator interface {
	ProposePatch(ctx context.Context, conversationKey domain.ConversationKey, exchangeTS string, messages []domain.Message, topics []domain.TopicReference) (domain.MemoryPatch, error)
}

// ProjectionSnapshot holds a consistent point-in-time view of all memory state
// required to render an OKF bundle. It is read under a single transaction.
type ProjectionSnapshot struct {
	Topics    []domain.Topic
	Revisions map[domain.TopicID][]domain.TopicRevision
	Links     map[domain.TopicID][]domain.TopicLink
	Evidence  map[domain.TopicID][]domain.Evidence
}

// ProjectionReader returns a consistent snapshot of the memory store suitable
// for projecting an OKF bundle. It must be read under a single transaction.
type ProjectionReader interface {
	ReadProjectionSnapshot(ctx context.Context) (ProjectionSnapshot, error)
}

// OKFProjector materializes committed SQLite memory state into an Open
// Knowledge Format bundle on the filesystem. It is never a writable source of
// truth.
type OKFProjector interface {
	Project(ctx context.Context, reader ProjectionReader, outputDir string) error
}
