package port

import (
	"context"
	"errors"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

var (
	ErrKnowledgeNotFound    = errors.New("knowledge item not found")
	ErrKnowledgeCASConflict = errors.New("knowledge CAS conflict")
	ErrKnowledgeValidation  = errors.New("knowledge storage validation failure")
	ErrKnowledgeUnavailable = errors.New("knowledge storage unavailable")
	ErrKnowledgeDisabled    = errors.New("knowledge commands are disabled")
	ErrKnowledgeBusy        = errors.New("conversation coordinator is busy")
)

// KnowledgeStore persists scoped knowledge with revision CAS, supersession,
// content-free tombstones, global command receipts, and the projection
// outbox. Reads filter by the closed set of readable scopes inside SQL so an
// unreadable item is indistinguishable from a missing one. Admission and
// trusted binding policy live in the use case; the store re-validates
// persisted invariants and enforces replay and tombstone protection at the
// persistence boundary.
type KnowledgeStore interface {
	CreateClaim(ctx context.Context, claim domain.KnowledgeClaim, limits domain.KnowledgeLimits) (domain.KnowledgeClaim, error)
	GetClaim(ctx context.Context, id domain.KnowledgeClaimID, readable []domain.KnowledgeScopeRef) (domain.KnowledgeClaim, error)
	CorrectClaim(ctx context.Context, replacement domain.KnowledgeClaim, source domain.KnowledgeSourceClass, limits domain.KnowledgeLimits) (domain.KnowledgeClaim, error)
	TransitionClaimStatus(
		ctx context.Context,
		id domain.KnowledgeClaimID,
		expectedRev int,
		next domain.KnowledgeClaimStatus,
		source domain.KnowledgeSourceClass,
		sourceRef string,
	) (domain.KnowledgeClaim, error)
	ForgetSubject(ctx context.Context, subject string, scopeKind domain.KnowledgeScopeKind, scopeID, sourceRef string) (bool, error)
	AddEvidence(ctx context.Context, claimID domain.KnowledgeClaimID, revisionNumber int, evidence domain.KnowledgeEvidence) error
	CreatePreference(ctx context.Context, preference domain.KnowledgePreference, limits domain.KnowledgeLimits) (domain.KnowledgePreference, error)
	UpdatePreference(ctx context.Context, preference domain.KnowledgePreference, expectedRev int, limits domain.KnowledgeLimits) (domain.KnowledgePreference, error)
	GetPreference(ctx context.Context, ownerKey, key string) (domain.KnowledgePreference, error)
	ListPreferencesForOwner(ctx context.Context, ownerKey string, limits domain.KnowledgeLimits) ([]domain.KnowledgePreference, error)
	ArchivePreference(ctx context.Context, ownerKey, key string, expectedRev int, sourceRef string) (domain.KnowledgePreference, error)
	CreateDocument(ctx context.Context, document domain.KnowledgeDocument, limits domain.KnowledgeLimits) (domain.KnowledgeDocument, error)
	GetDocument(ctx context.Context, id domain.KnowledgeDocumentID, readable []domain.KnowledgeScopeRef) (domain.KnowledgeDocument, error)
	ListClaimsInScopes(ctx context.Context, scopes []domain.KnowledgeScopeRef, subject string, limits domain.KnowledgeLimits) ([]domain.KnowledgeClaim, error)
	ListDocumentsInScopes(ctx context.Context, scopes []domain.KnowledgeScopeRef, limits domain.KnowledgeLimits) ([]domain.KnowledgeDocument, error)
	ArchiveDocument(ctx context.Context, id domain.KnowledgeDocumentID, expectedRev int, sourceRef string) (domain.KnowledgeDocument, error)
	CommitCommandReceipt(ctx context.Context, receipt domain.KnowledgeCommandReceipt) error
	EnqueueProjection(ctx context.Context) error
}

// KnowledgeProjectionExhaustedCode is the stable bounded terminal code
// persisted in the projection outbox when a batch exceeds its retry budget.
// Detailed errors are logged sanitized and never persisted.
const KnowledgeProjectionExhaustedCode = "attempts_exhausted"

// KnowledgeProjectionStore is the minimal batch/coalescing CAS surface the
// knowledge projection worker depends on. A claim atomically takes every due
// row as one batch, so several pending mutations collapse into a single
// read/render. Completion, retry, and failure apply to exactly the claimed
// batch and never mark other pending triggers done. Retry preserves the
// attempt counter; failure stores only the bounded terminal code.
type KnowledgeProjectionStore interface {
	ClaimProjectionBatch(ctx context.Context) ([]domain.KnowledgeProjectionItem, error)
	CompleteProjectionBatch(ctx context.Context, ids []int, leaseUntil time.Time) error
	RetryProjectionBatch(ctx context.Context, ids []int, leaseUntil, nextAttempt time.Time) error
	// DeferProjectionCleanupBatch schedules the claimed batch for a
	// cleanup-only retry without consuming the projection attempt budget:
	// the bundle state is correct but promotion residue (backup or
	// staging) removal failed, and cleanup retries must never exhaust
	// through the normal retry limit. The attempt counter is rolled back
	// to its pre-claim value so a later real projection failure still
	// keeps its own budget.
	DeferProjectionCleanupBatch(ctx context.Context, ids []int, leaseUntil, nextAttempt time.Time) error
	FailProjectionBatch(ctx context.Context, ids []int, leaseUntil time.Time, code string) error
	CleanupProjection(ctx context.Context, before time.Time) error
}

// ConversationCoordinator serializes knowledge commands within one canonical
// conversation, mirroring the workstream-human coordinator. At most one
// knowledge command may execute per conversation key at a time; a busy
// conversation rejects the command instead of queueing it.
type ConversationCoordinator interface {
	TryAcquire(conversationKey string) (release func(), acquired bool)
}

// KnowledgeCommands is the consumer-owned surface the bot use case depends on
// for the memory-human dispatch. Implementations parse, serialize, and execute
// one trusted human command. matched=false lets callers fall through to the
// normal agent flow. Binding and event identity are host-trusted values
// resolved from verified Slack event metadata, never model output.
type KnowledgeCommands interface {
	// MatchesKnowledge reports whether text is a memory-human command,
	// including payloads that will later fail parsing. Callers must use it to
	// decide whether a message is knowledge traffic before resolving any
	// binding, so ordinary messages never touch binding resolution.
	MatchesKnowledge(text string) bool
	// Enabled reports the authoritative gate state. Enabled commands must
	// not execute when binding resolution fails; disabled commands answer
	// with the deterministic disabled response without requiring successful
	// resolution.
	Enabled() bool
	Execute(ctx context.Context, binding domain.KnowledgeWriteBinding, eventID, text string) (matched bool, message string, err error)
}

// KnowledgeBindingResolver resolves the trusted knowledge write binding for
// one invocation. The registered project and the active actor-bound
// workstream come only from host-trusted state. An explicit payload project
// selector becomes the binding's project only after validation against the
// registered project set; unregistered or model selectors never fill these
// fields. A conversation without an active actor-bound workstream and without
// a validated explicit project resolves an empty project binding and defaults
// to the user scope.
type KnowledgeBindingResolver interface {
	ResolveKnowledgeBinding(ctx context.Context, team, actor string, conversationKey domain.ConversationKey, text string) (domain.KnowledgeWriteBinding, error)
}
