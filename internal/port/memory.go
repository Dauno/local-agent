package port

import (
	"context"
	"errors"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// ErrModelCallLimitReached indicates that the process-wide model-call budget is
// exhausted. Callers can use it to apply their own backpressure behavior.
var ErrModelCallLimitReached = errors.New("maximum concurrent model calls reached")

// ErrCuratorResponseIncomplete marks a curator response that cannot be safely
// applied because the model stopped before producing a complete payload.
var ErrCuratorResponseIncomplete = errors.New("curator model response incomplete")

// MemoryRetriever provides synchronous recall of curated memory topics.
// It is called before each model invocation and must never block the normal
// response path.
type MemoryRetriever interface {
	Recall(ctx context.Context, query, ownerKey string) ([]domain.MemorySnippet, error)
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

// MemoryWorkerStore contains only persistence operations needed by the
// background curator runner. It intentionally excludes topic policy and CRUD.
type MemoryWorkerStore interface {
	ReconcileAssistantExchanges(ctx context.Context, finder AssistantExchangeFinder) error
	ClaimNextOutboxItem(ctx context.Context) (*domain.OutboxItem, error)
	LoadOutboxMessages(ctx context.Context, item *domain.OutboxItem) ([]domain.Message, error)
	CompleteOutboxItem(ctx context.Context, id int, leaseUntil time.Time) error
	FailOutboxItem(ctx context.Context, id int, leaseUntil time.Time, reason string) error
	RetryOutboxItem(ctx context.Context, id int, leaseUntil, nextAttempt time.Time) error
	RescheduleOutboxItem(ctx context.Context, id int, leaseUntil, nextAttempt time.Time) error
	CleanupOutbox(ctx context.Context, before time.Time) error
}

// ProjectionSnapshot holds a consistent point-in-time view of all memory state
// required to render an OKF bundle. It is read under a single transaction.
// Knowledge rows are included so legacy and knowledge promotions each
// represent one complete snapshot; tombstones are never part of a snapshot.
type ProjectionSnapshot struct {
	Topics    []domain.Topic
	Revisions map[domain.TopicID][]domain.TopicRevision
	Links     map[domain.TopicID][]domain.TopicLink
	Evidence  map[domain.TopicID][]domain.Evidence
	Knowledge KnowledgeSnapshot
}

// KnowledgeSnapshot carries all content-bearing knowledge rows, including
// archived, disputed, and superseded records. Expiry is computed from
// validity at render time and never written back to SQLite.
type KnowledgeSnapshot struct {
	Claims      []domain.KnowledgeClaim
	Preferences []domain.KnowledgePreference
	Documents   []domain.KnowledgeDocument
	Evidence    []KnowledgeEvidenceRef
}

// Present reports whether the snapshot carries any knowledge rows. When it
// does not, projection output for legacy state stays byte-identical to the
// pre-knowledge renderer.
func (k KnowledgeSnapshot) Present() bool {
	return len(k.Claims)+len(k.Preferences)+len(k.Documents)+len(k.Evidence) > 0
}

// KnowledgeEvidenceRef is the projection-safe episodic reference: claim
// linkage, evidence kind, and the safe temporal reference only. Conversation
// keys and author identities are never carried into projections, and no
// ledger content is ever copied.
type KnowledgeEvidenceRef struct {
	ClaimID        domain.KnowledgeClaimID
	RevisionNumber int
	Kind           domain.KnowledgeEvidenceKind
	ExchangeTS     string
}

// ProjectionReader returns a consistent snapshot of the memory store suitable
// for projecting an OKF bundle. It must be read under a single transaction.
type ProjectionReader interface {
	ReadProjectionSnapshot(ctx context.Context) (ProjectionSnapshot, error)
}

// ErrProjectionCleanup marks a projection whose live bundle state is
// correct but whose promotion residue (backup or staging) could not be
// removed. Residue can retain content that has since been forgotten, so
// callers must not treat it as a complete projection: the outbox row must
// stay pending until the residue is actually removed.
var ErrProjectionCleanup = errors.New("projection residue cleanup incomplete")

// OKFProjector materializes committed SQLite memory state into an Open
// Knowledge Format bundle on the filesystem. It is never a writable source of
// truth.
type OKFProjector interface {
	Project(ctx context.Context, reader ProjectionReader, outputDir string) error
	// Recover removes promotion residue for outputDir without rendering:
	// leftover staging and backup directories. When the live bundle is
	// missing, a recovered backup is restored in its place instead of being
	// discarded. Recovery must be safe to run at startup with no pending
	// knowledge mutation.
	Recover(outputDir string) error
}
