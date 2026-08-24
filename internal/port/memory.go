package port

import (
	"context"
	"errors"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// ErrModelCallLimitReached indicates that the process-wide model-call budget is
// exhausted. Callers can use it to apply their own backpressure behavior.
var ErrModelCallLimitReached = errors.New("maximum concurrent model calls reached")

// ErrProjectionCleanup reports a staging, backup, or stale-file removal
// failure during OKF projection. Attempt-neutral callers stay pending until
// the residue is actually removed.
var ErrProjectionCleanup = errors.New("projection cleanup failed")

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
// does not, projection output stays a minimal knowledge-only bundle.
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

// ProjectionSnapshot holds a consistent point-in-time view of the knowledge
// state required to render an OKF bundle. It is read under a single
// transaction; tombstones are never part of a snapshot.
type ProjectionSnapshot struct {
	Knowledge KnowledgeSnapshot
}

// ProjectionReader returns a consistent snapshot of the knowledge store
// suitable for rendering one complete OKF bundle.
type ProjectionReader interface {
	ReadProjectionSnapshot(ctx context.Context) (ProjectionSnapshot, error)
}

// OKFProjector renders one snapshot into a complete OKF bundle through safe
// staging, promotion, and recovery semantics.
type OKFProjector interface {
	Project(ctx context.Context, reader ProjectionReader, outputDir string) error
	Recover(outputDir string) error
}
