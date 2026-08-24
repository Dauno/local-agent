package port

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// KnowledgeRetriever is the consumer-owned retrieval surface. It resolves
// eligibility, ranking, and card construction for one user-origin turn and
// returns complete untrusted frame cards. An empty card list is a normal
// successful outcome. Top-level request or domain rejection wraps
// ErrKnowledgeValidation; authoritative read or index failures wrap
// ErrKnowledgeUnavailable; queue generation/lease loss wraps
// ErrKnowledgeCASConflict. Optional channel failures are typed diagnostic
// reasons, never top-level errors.
type KnowledgeRetriever interface {
	Retrieve(ctx context.Context, request domain.KnowledgeRetrievalRequest) (domain.KnowledgeRetrievalResult, error)
}

// KnowledgeRetrievalBinding is the resolution result for one user-origin
// turn: the trusted binding, the optional active actor-bound workstream
// snapshot, and the originating Slack exchange timestamp that binds the
// result to its turn. It is read from one authoritative workstream read,
// preventing a binding/snapshot revision race.
type KnowledgeRetrievalBinding struct {
	Binding    domain.KnowledgeWriteBinding
	Workstream *domain.WorkstreamSnapshot
	ExchangeTS string
}

// KnowledgeRetrievalBindingResolver resolves the trusted retrieval binding
// for the current turn. The host-trusted team, actor, canonical
// conversation, and originating exchange timestamp are explicit arguments,
// never hidden context values or per-turn reconstruction. With no active
// actor-bound registered workstream it returns team/actor/conversation
// binding plus a nil snapshot and grants no project or workstream scope.
// Invalid or failed binding produces no retrieval request and no cards.
type KnowledgeRetrievalBindingResolver interface {
	ResolveRetrievalBinding(ctx context.Context, team, actor string, conversation domain.ConversationKey, exchangeTS string) (KnowledgeRetrievalBinding, error)
}

// KnowledgeEligibleCandidate is the bounded authoritative identity metadata
// the candidate reader returns after scope-or-owner, status, and validity
// filtering. It never carries content.
type KnowledgeEligibleCandidate struct {
	Kind      domain.KnowledgeRetrievalItemKind
	ID        string
	Subject   string
	ScopeKind domain.KnowledgeScopeKind
	ScopeID   string
	Revision  int
}

// KnowledgeAuthoritativeItem is the tagged union of one complete authorized
// item used to build a frame card. Exactly one payload is present. It is
// read under the trusted scopes only after an index or channel hit named
// this identity, never through a catalog scan.
type KnowledgeAuthoritativeItem struct {
	Kind       domain.KnowledgeRetrievalItemKind
	ID         string
	Claim      *domain.KnowledgeClaim
	Preference *domain.KnowledgePreference
	Document   *domain.KnowledgeDocument
}

// Validate enforces the union contract: exactly one payload matching Kind,
// an ID matching the payload identity, and full authoritative payload
// validation.
func (i KnowledgeAuthoritativeItem) Validate() error {
	if !domain.ValidKnowledgeRetrievalItemKind(i.Kind) {
		return fmt.Errorf("%w: unknown authoritative item kind %q", ErrKnowledgeValidation, i.Kind)
	}
	payloads := 0
	if i.Claim != nil {
		payloads++
	}
	if i.Preference != nil {
		payloads++
	}
	if i.Document != nil {
		payloads++
	}
	if payloads != 1 {
		return fmt.Errorf("%w: authoritative item must carry exactly one payload, got %d", ErrKnowledgeValidation, payloads)
	}
	switch i.Kind {
	case domain.KnowledgeRetrievalClaim:
		if i.Claim == nil || i.ID != string(i.Claim.ID) {
			return fmt.Errorf("%w: authoritative claim identity mismatch", ErrKnowledgeValidation)
		}
		if err := i.Claim.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrKnowledgeValidation, err)
		}
	case domain.KnowledgeRetrievalPreference:
		if i.Preference == nil || i.ID != "preference:"+strconv.Itoa(i.Preference.ID) {
			return fmt.Errorf("%w: authoritative preference identity mismatch", ErrKnowledgeValidation)
		}
		if err := i.Preference.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrKnowledgeValidation, err)
		}
	case domain.KnowledgeRetrievalDocument:
		if i.Document == nil || i.ID != string(i.Document.ID) {
			return fmt.Errorf("%w: authoritative document identity mismatch", ErrKnowledgeValidation)
		}
		if err := i.Document.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrKnowledgeValidation, err)
		}
	}
	return nil
}

// KnowledgeCandidateReader is the authoritative channel-shaped candidate
// surface. Every read receives the closed readable scopes and the trusted
// owner key derived from the binding; an unreadable row is indistinguishable
// from a missing row and filtering happens inside SQL before rows leave the
// adapter, so the adapter never scans the whole authorized catalog in Go.
type KnowledgeCandidateReader interface {
	// ReadExact matches the canonical query and the extracted technical
	// tokens against claim subject/value/reference, preference key/value,
	// and document subject with byte-exact comparison, bounded by
	// limits.MaxCandidatesPerChannel after authorization and stable
	// identity ordering.
	ReadExact(ctx context.Context, binding domain.KnowledgeWriteBinding, now time.Time, limits domain.KnowledgeRetrievalLimits, query string, tokens []string) ([]KnowledgeEligibleCandidate, error)
	// ReadRelated expands one hop from the authorized exact claim seeds:
	// claims whose subject or reference matches either (subject,
	// value_reference) endpoint of the seeds' owns/relates_to edges.
	ReadRelated(ctx context.Context, binding domain.KnowledgeWriteBinding, now time.Time, limits domain.KnowledgeRetrievalLimits, seeds []KnowledgeEligibleCandidate) ([]KnowledgeEligibleCandidate, error)
	// ReadItem re-reads the complete authorized row for card construction
	// after a channel or index hit. Documents are returned with their
	// original handle; content resolution happens separately through
	// KnowledgeDocumentResolver.
	ReadItem(ctx context.Context, binding domain.KnowledgeWriteBinding, now time.Time, limits domain.KnowledgeRetrievalLimits, kind domain.KnowledgeRetrievalItemKind, id string) (KnowledgeAuthoritativeItem, error)
}

// KnowledgeDocumentResolver resolves complete verified document content.
// The resolver returns exact bytes only after scope and status
// authorization, handle verification, size bounding, and digest
// verification. It never falls back to mutable legacy content or returns
// partial bytes.
type KnowledgeDocumentResolver interface {
	Resolve(ctx context.Context, document domain.KnowledgeDocument, limits domain.KnowledgeRetrievalLimits) ([]byte, error)
}

// KnowledgeIndexHit is one index result carrying only kind, identity, rank,
// revision, and source-digest metadata. The authoritative row is re-read
// before any card construction, and a hit whose revision or source digest
// no longer matches the authoritative row is excluded as stale — the digest
// catches redactor rotations and index-text changes that do not advance the
// item revision.
type KnowledgeIndexHit struct {
	Kind         domain.KnowledgeRetrievalItemKind
	ID           string
	Rank         int
	Revision     int
	SourceDigest string
}

// KnowledgeIndex is the reconstructible lexical and vector search surface.
// Both operations receive the closed readable scopes and the trusted owner
// key; an index row is never authoritative on its own.
type KnowledgeIndex interface {
	SearchLexical(ctx context.Context, scopes []domain.KnowledgeScopeRef, ownerKey, query string, limit int) ([]KnowledgeIndexHit, error)
	SearchSemantic(ctx context.Context, scopes []domain.KnowledgeScopeRef, ownerKey string, vector []float32, minSimilarityBasisPoints, limit int) ([]KnowledgeIndexHit, error)
}

// EmbeddingProvider embeds bounded redacted inputs. Output vectors must
// have exactly the configured dimensions and only finite values; host code
// rejects zero-norm and malformed output before storage.
type EmbeddingProvider interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// KnowledgeQueueStore is the generation-CAS queue surface the lexical and
// embedding workers and startup reconciliation depend on. A claim receives
// the bounded injected clock and lease duration, and returns a structural
// token carrying generation plus lease identity; every terminal and retry
// transition must present that exact token, so an expired claimant can
// never mutate a reclaimed claim. Enqueue upserts one identity with a fresh
// generation for truth mutations and reconciliation; List pages current
// queue rows in identity order for rebuild checks.
type KnowledgeQueueStore interface {
	ClaimNext(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, now time.Time, lease time.Duration) (domain.KnowledgeQueueItem, bool, error)
	Complete(ctx context.Context, claim domain.KnowledgeQueueClaim) error
	Retry(ctx context.Context, claim domain.KnowledgeQueueClaim, nextAttempt time.Time) error
	Fail(ctx context.Context, claim domain.KnowledgeQueueClaim, code domain.KnowledgeQueueFailureCode) error
	List(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, afterID string, limit int) ([]domain.KnowledgeQueueItem, error)
	Enqueue(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, itemID string) (domain.KnowledgeQueueItem, error)
}
