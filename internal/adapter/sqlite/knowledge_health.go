package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// CheckKnowledgeRetrievalState is the offline content-free doctor check for
// the reconstructible v39 retrieval state. It verifies required objects,
// closed queue states/leases/timestamps, orphan and revision metadata,
// vector storage types/lengths/dimensions, fingerprint consistency, and
// structural digest validity without reading source text, FTS bodies, or
// vector bytes. Stale leases and metadata mismatches whose identities have
// non-terminal queue work able to repair them are reported as bounded
// state; missing objects, impossible metadata, invalid queue rows, and
// unrepairable orphans fail with bounded remediation.
func (s *Store) CheckKnowledgeRetrievalState(ctx context.Context) (domain.KnowledgeRetrievalHealth, error) {
	if s == nil || s.db == nil {
		return domain.KnowledgeRetrievalHealth{}, errors.New("SQLite store is not configured")
	}
	health := domain.KnowledgeRetrievalHealth{}
	if err := checkKnowledgeRequiredObjects(ctx, s.db); err != nil {
		return health, err
	}
	var err error
	health.LexicalQueuePending, err = knowledgeQueueStateCount(ctx, s.db, "knowledge_lexical_queue", "pending")
	if err != nil {
		return health, fmt.Errorf("count pending lexical queue rows: %w", err)
	}
	health.LexicalQueueProcessing, err = knowledgeQueueStateCount(ctx, s.db, "knowledge_lexical_queue", "processing")
	if err != nil {
		return health, fmt.Errorf("count processing lexical queue rows: %w", err)
	}
	health.EmbeddingQueuePending, err = knowledgeQueueStateCount(ctx, s.db, "knowledge_embedding_queue", "pending")
	if err != nil {
		return health, fmt.Errorf("count pending embedding queue rows: %w", err)
	}
	health.EmbeddingQueueProcessing, err = knowledgeQueueStateCount(ctx, s.db, "knowledge_embedding_queue", "processing")
	if err != nil {
		return health, fmt.Errorf("count processing embedding queue rows: %w", err)
	}
	nowUnix := time.Now().UTC().Unix()
	health.LexicalQueueStaleProcessing, err = knowledgeQueueStaleCount(ctx, s.db, "knowledge_lexical_queue", nowUnix)
	if err != nil {
		return health, fmt.Errorf("count stale lexical leases: %w", err)
	}
	health.EmbeddingQueueStaleProcessing, err = knowledgeQueueStaleCount(ctx, s.db, "knowledge_embedding_queue", nowUnix)
	if err != nil {
		return health, fmt.Errorf("count stale embedding leases: %w", err)
	}
	for _, table := range []string{"knowledge_lexical_queue", "knowledge_embedding_queue"} {
		invalid, err := knowledgeQueueInvalidCount(ctx, s.db, table)
		if err != nil {
			return health, fmt.Errorf("inspect %s coherence: %w", table, err)
		}
		if invalid > 0 {
			return health, fmt.Errorf("%s has %d rows with invalid state, attempts, timestamps, or leases", table, invalid)
		}
	}
	if err := checkKnowledgeFTSStructure(ctx, s.db); err != nil {
		return health, err
	}
	if err := checkKnowledgeEmbeddingStructure(ctx, s.db); err != nil {
		return health, err
	}
	lexicalRepairable, lexicalOrphans, err := checkKnowledgeFTSTruthCorrespondence(ctx, s.db)
	if err != nil {
		return health, err
	}
	health.LexicalRepairableMismatch = lexicalRepairable
	if lexicalOrphans > 0 {
		return health, fmt.Errorf("knowledge_retrieval_fts has %d orphan or revision-mismatched rows without non-terminal queue work able to repair them", lexicalOrphans)
	}
	embeddingRepairable, embeddingOrphans, err := checkKnowledgeEmbeddingTruthCorrespondence(ctx, s.db)
	if err != nil {
		return health, err
	}
	health.EmbeddingRepairableMismatch = embeddingRepairable
	if embeddingOrphans > 0 {
		return health, fmt.Errorf("knowledge_embeddings has %d orphan or revision-mismatched rows without non-terminal queue work able to repair them", embeddingOrphans)
	}
	return health, nil
}

func checkKnowledgeRequiredObjects(ctx context.Context, db *sql.DB) error {
	objects := []struct {
		kind, name string
	}{
		// FTS5 virtual tables are recorded as ordinary tables in
		// sqlite_master; only their SQL is CREATE VIRTUAL TABLE.
		{"table", "knowledge_retrieval_fts"},
		{"table", "knowledge_embeddings"},
		{"table", "knowledge_lexical_queue"},
		{"table", "knowledge_embedding_queue"},
		{"index", "knowledge_claims_by_reference_scope_status"},
		{"index", "knowledge_lexical_queue_by_pending_next"},
		{"index", "knowledge_lexical_queue_by_processing_lease"},
		{"index", "knowledge_embedding_queue_by_pending_next"},
		{"index", "knowledge_embedding_queue_by_processing_lease"},
		{"trigger", "knowledge_claims_enqueue_after_insert"},
		{"trigger", "knowledge_claims_enqueue_after_update"},
		{"trigger", "knowledge_claims_enqueue_after_delete"},
		{"trigger", "knowledge_preferences_enqueue_after_insert"},
		{"trigger", "knowledge_preferences_enqueue_after_update"},
		{"trigger", "knowledge_preferences_enqueue_after_delete"},
		{"trigger", "knowledge_documents_enqueue_after_insert"},
		{"trigger", "knowledge_documents_enqueue_after_update"},
		{"trigger", "knowledge_documents_enqueue_after_delete"},
	}
	for _, object := range objects {
		var name string
		err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = ? AND name = ?`, object.kind, object.name).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("knowledge retrieval %s %q is missing", object.kind, object.name)
		}
		if err != nil {
			return fmt.Errorf("inspect knowledge retrieval %s %q: %w", object.kind, object.name, err)
		}
	}
	return nil
}

func knowledgeQueueStateCount(ctx context.Context, db *sql.DB, table, status string) (int, error) {
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE status = ?", status).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func knowledgeQueueStaleCount(ctx context.Context, db *sql.DB, table string, nowUnix int64) (int, error) {
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE status = 'processing' AND lease_until <= ?", nowUnix).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// knowledgeQueueInvalidCount detects impossible queue rows: leases on
// non-processing states, processing rows without a lease or attempts, and
// last_error values contradicting the closed state.
func knowledgeQueueInvalidCount(ctx context.Context, db *sql.DB, table string) (int, error) {
	var count int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+` WHERE
		status NOT IN ('pending','processing','done','failed')
		OR (status = 'pending' AND lease_until != 0)
		OR (status = 'pending' AND last_error != '')
		OR (status = 'processing' AND lease_until = 0)
		OR (status = 'processing' AND attempts < 1)
		OR (status = 'processing' AND last_error != '')
		OR (status = 'done' AND (lease_until != 0 OR last_error != '' OR next_attempt != 0))
		OR (status = 'failed' AND (lease_until != 0 OR last_error = '' OR next_attempt != 0))
		OR last_error NOT IN ('', 'source_invalid', 'provider_invalid', 'attempts_exhausted')
		OR generation < 0 OR attempts < 0 OR next_attempt < 0 OR lease_until < 0
		OR created_at <= 0 OR updated_at <= 0 OR updated_at < created_at`).Scan(&count)
	return count, err
}

func checkKnowledgeFTSStructure(ctx context.Context, db *sql.DB) error {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_retrieval_fts WHERE
		item_kind NOT IN ('claim','preference','document')
		OR item_revision < 1
		OR length(source_digest) != 64 OR source_digest GLOB '*[^0-9a-f]*'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("inspect lexical index structure: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("knowledge_retrieval_fts has %d rows with invalid kind, revision, or source digest metadata", count)
	}
	return nil
}

func checkKnowledgeEmbeddingStructure(ctx context.Context, db *sql.DB) error {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_embeddings WHERE
		item_kind NOT IN ('claim','preference','document')
		OR item_revision < 1
		OR length(source_digest) != 64 OR source_digest GLOB '*[^0-9a-f]*'
		OR length(model_fingerprint) = 0
		OR dimensions < 1 OR dimensions > 4096
		OR typeof(vector) != 'blob'
		OR length(vector) != dimensions * 4`).Scan(&count)
	if err != nil {
		return fmt.Errorf("inspect embedding metadata: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("knowledge_embeddings has %d rows with invalid kind, revision, digest, fingerprint, dimensions, vector type, or vector length", count)
	}
	var inconsistent int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		SELECT model_fingerprint FROM knowledge_embeddings
		GROUP BY model_fingerprint HAVING COUNT(DISTINCT dimensions) > 1)`).Scan(&inconsistent)
	if err != nil {
		return fmt.Errorf("inspect embedding dimension consistency: %w", err)
	}
	if inconsistent > 0 {
		return fmt.Errorf("knowledge_embeddings has %d model fingerprints with inconsistent dimensions", inconsistent)
	}
	return nil
}

// checkKnowledgeFTSTruthCorrespondence verifies every lexical index row
// against its authoritative truth identity, revision, and eligibility using
// metadata columns only. A row whose identity has non-terminal queue work
// is a repairable mismatch; anything else is an unrepairable orphan.
func checkKnowledgeFTSTruthCorrespondence(ctx context.Context, db *sql.DB) (repairable, orphans int, err error) {
	correspondences := []struct {
		kind, truth, joinExpr, eligibleExpr, queue string
	}{
		{"claim", "knowledge_claims", "t.id = f.item_id", "t.status IN ('asserted','verified','disputed')", "knowledge_lexical_queue"},
		{"preference", "knowledge_preferences", "'preference:' || t.id = f.item_id", "t.status = 'active'", "knowledge_lexical_queue"},
		{"document", "knowledge_documents", "t.id = f.item_id", "t.status = 'active'", "knowledge_lexical_queue"},
	}
	for _, spec := range correspondences {
		orphanQuery := fmt.Sprintf(`SELECT COUNT(*) FROM knowledge_retrieval_fts f
			WHERE f.item_kind = '%s' AND NOT EXISTS (SELECT 1 FROM %s t WHERE %s)`, spec.kind, spec.truth, spec.joinExpr)
		mismatchQuery := fmt.Sprintf(`SELECT COUNT(*) FROM knowledge_retrieval_fts f JOIN %s t ON %s
			WHERE f.item_kind = '%s' AND (f.item_revision != t.current_rev OR NOT (%s))`, spec.truth, spec.joinExpr, spec.kind, spec.eligibleExpr)
		repairableQuery := fmt.Sprintf(`SELECT COUNT(*) FROM knowledge_retrieval_fts f
			WHERE f.item_kind = '%s' AND (
				NOT EXISTS (SELECT 1 FROM %s t WHERE %s)
				OR EXISTS (SELECT 1 FROM %s t WHERE %s AND (f.item_revision != t.current_rev OR NOT (%s)))
			) AND EXISTS (SELECT 1 FROM %s q WHERE q.item_kind = '%s' AND q.item_id = f.item_id AND q.status IN ('pending','processing'))`,
			spec.kind, spec.truth, spec.joinExpr, spec.truth, spec.joinExpr, spec.eligibleExpr, spec.queue, spec.kind)
		var orphanCount, mismatchCount, repairableCount int
		if err := db.QueryRowContext(ctx, orphanQuery).Scan(&orphanCount); err != nil {
			return 0, 0, err
		}
		if err := db.QueryRowContext(ctx, mismatchQuery).Scan(&mismatchCount); err != nil {
			return 0, 0, err
		}
		if err := db.QueryRowContext(ctx, repairableQuery).Scan(&repairableCount); err != nil {
			return 0, 0, err
		}
		repairable += repairableCount
		orphans += orphanCount + mismatchCount - repairableCount
	}
	return repairable, orphans, nil
}

func checkKnowledgeEmbeddingTruthCorrespondence(ctx context.Context, db *sql.DB) (repairable, orphans int, err error) {
	correspondences := []struct {
		kind, truth, joinExpr, eligibleExpr string
	}{
		{"claim", "knowledge_claims", "t.id = e.item_id", "t.status IN ('asserted','verified','disputed')"},
		{"preference", "knowledge_preferences", "'preference:' || t.id = e.item_id", "t.status = 'active'"},
		{"document", "knowledge_documents", "t.id = e.item_id", "t.status = 'active'"},
	}
	for _, spec := range correspondences {
		orphanQuery := fmt.Sprintf(`SELECT COUNT(*) FROM knowledge_embeddings e
			WHERE e.item_kind = '%s' AND NOT EXISTS (SELECT 1 FROM %s t WHERE %s)`, spec.kind, spec.truth, spec.joinExpr)
		mismatchQuery := fmt.Sprintf(`SELECT COUNT(*) FROM knowledge_embeddings e JOIN %s t ON %s
			WHERE e.item_kind = '%s' AND (e.item_revision != t.current_rev OR NOT (%s))`, spec.truth, spec.joinExpr, spec.kind, spec.eligibleExpr)
		repairableQuery := fmt.Sprintf(`SELECT COUNT(*) FROM knowledge_embeddings e
			WHERE e.item_kind = '%s' AND (
				NOT EXISTS (SELECT 1 FROM %s t WHERE %s)
				OR EXISTS (SELECT 1 FROM %s t WHERE %s AND (e.item_revision != t.current_rev OR NOT (%s)))
			) AND EXISTS (SELECT 1 FROM knowledge_embedding_queue q WHERE q.item_kind = '%s' AND q.item_id = e.item_id AND q.status IN ('pending','processing'))`,
			spec.kind, spec.truth, spec.joinExpr, spec.truth, spec.joinExpr, spec.eligibleExpr, spec.kind)
		var orphanCount, mismatchCount, repairableCount int
		if err := db.QueryRowContext(ctx, orphanQuery).Scan(&orphanCount); err != nil {
			return 0, 0, err
		}
		if err := db.QueryRowContext(ctx, mismatchQuery).Scan(&mismatchCount); err != nil {
			return 0, 0, err
		}
		if err := db.QueryRowContext(ctx, repairableQuery).Scan(&repairableCount); err != nil {
			return 0, 0, err
		}
		repairable += repairableCount
		orphans += orphanCount + mismatchCount - repairableCount
	}
	return repairable, orphans, nil
}
