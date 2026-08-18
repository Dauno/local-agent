package port

import (
	"context"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// KnowledgeIndexSource re-reads one complete authoritative item by stable
// identity for index construction. Unlike the candidate reader it applies no
// scope or eligibility predicates: the worker owns the private identity and
// must be able to see archived or superseded rows so it can remove their
// index rows. Missing items report ErrKnowledgeNotFound so the worker
// completes with index cleanup instead of failing.
type KnowledgeIndexSource interface {
	ReadIndexSource(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, id string) (KnowledgeAuthoritativeItem, error)
}

// KnowledgeLexicalIndexRow is one lexical index identity returned by
// pagination. It carries only kind, identity, revision, and source-digest
// metadata, never index text.
type KnowledgeLexicalIndexRow struct {
	Kind         domain.KnowledgeRetrievalItemKind
	ID           string
	Revision     int
	SourceDigest string
}

// KnowledgeLexicalIndex is the worker-owned lexical index mutation surface.
// Replace atomically swaps every FTS row of one identity in a single
// transaction; Delete removes every row of one identity; List pages index
// identities in stable order for reconciliation; Clear removes every FTS
// row for rebuild. No operation touches authoritative knowledge rows.
type KnowledgeLexicalIndex interface {
	ReplaceLexical(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, id string, revision int, sourceDigest, subject, body string) error
	DeleteLexical(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, id string) error
	ListLexical(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, afterID string, limit int) ([]KnowledgeLexicalIndexRow, error)
	ClearLexical(ctx context.Context) error
}

// KnowledgeTruthIdentity is one authoritative truth identity with its
// current revision, used by reconciliation to detect queue staleness
// without reading content.
type KnowledgeTruthIdentity struct {
	ID       string
	Revision int
}

// KnowledgeIdentityLister pages authoritative truth identities per kind in
// stable identity order for reconciliation and rebuild.
type KnowledgeIdentityLister interface {
	ListTruthIdentities(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, afterID string, limit int) ([]KnowledgeTruthIdentity, error)
}

// KnowledgeVectorIndexRow is one vector index identity returned by
// pagination. It carries only kind, identity, revision, source-digest, and
// fingerprint metadata, never vector bytes.
type KnowledgeVectorIndexRow struct {
	Kind         domain.KnowledgeRetrievalItemKind
	ID           string
	Revision     int
	SourceDigest string
	Fingerprint  string
}

// KnowledgeVectorIndex is the worker-owned vector index mutation surface
// over knowledge_embeddings. Replace atomically swaps only the row for one
// (kind, identity, fingerprint); rows under other fingerprints for the same
// identity coexist by design through the compound primary key and are never
// touched. Delete removes every fingerprint of one identity; List pages
// identity metadata scoped to one fingerprint in stable identity order for
// reconciliation; Clear removes every vector row for rebuild. No operation
// touches authoritative knowledge rows.
type KnowledgeVectorIndex interface {
	ReplaceVector(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, id string, revision int, sourceDigest, fingerprint string, vector []byte) error
	DeleteVector(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, id string) error
	ListVector(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, fingerprint, afterID string, limit int) ([]KnowledgeVectorIndexRow, error)
	ClearVector(ctx context.Context) error
}
