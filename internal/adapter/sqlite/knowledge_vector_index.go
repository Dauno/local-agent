package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.KnowledgeVectorIndex = (*KnowledgeVectorIndexStore)(nil)

// KnowledgeVectorIndexStore implements the worker-owned vector index
// mutation surface over knowledge_embeddings. Rows are never read back as
// vector content here: pagination returns only identity metadata, and no
// operation touches authoritative knowledge rows.
type KnowledgeVectorIndexStore struct {
	db *sql.DB
}

func NewKnowledgeVectorIndexStore(store *Store) *KnowledgeVectorIndexStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &KnowledgeVectorIndexStore{db: store.db}
}

// validateVectorMutation enforces the structural invariants before any
// write: closed kind, bounded identity, positive revision, lowercase
// 64-character hex digest, bounded fingerprint, and a vector whose byte
// length is exactly dimensions * 4 with dimensions inside the closed
// 1..4096 bound.
func validateVectorMutation(kind domain.KnowledgeRetrievalItemKind, id string, revision int, sourceDigest, fingerprint string, vector []byte) error {
	if !domain.ValidKnowledgeRetrievalItemKind(kind) {
		return fmt.Errorf("%w: unknown vector index kind", port.ErrKnowledgeValidation)
	}
	if id == "" || utf8.RuneCountInString(id) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return fmt.Errorf("%w: vector index identity is not bounded", port.ErrKnowledgeValidation)
	}
	if revision < 1 {
		return fmt.Errorf("%w: vector index revision must be positive", port.ErrKnowledgeValidation)
	}
	if !domain.ValidKnowledgeIndexSourceDigest(sourceDigest) || sourceDigest == "" {
		return fmt.Errorf("%w: vector index digest must be a lowercase 64-character hex string", port.ErrKnowledgeValidation)
	}
	if fingerprint == "" || utf8.RuneCountInString(fingerprint) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return fmt.Errorf("%w: vector index fingerprint must be a bounded non-empty string", port.ErrKnowledgeValidation)
	}
	if len(vector) == 0 || len(vector)%4 != 0 {
		return fmt.Errorf("%w: vector index bytes must encode a whole number of float32 values", port.ErrKnowledgeValidation)
	}
	dimensions := len(vector) / 4
	if dimensions < 1 || dimensions > domain.HardMaxKnowledgeEmbeddingDimensions {
		return fmt.Errorf("%w: vector index dimensions %d are outside the closed 1..%d bound", port.ErrKnowledgeValidation, dimensions, domain.HardMaxKnowledgeEmbeddingDimensions)
	}
	return nil
}

// ReplaceVector atomically deletes and reinserts only the row for (kind,
// id, fingerprint). Rows under other fingerprints for the same identity
// coexist through the compound primary key and are never touched.
func (s *KnowledgeVectorIndexStore) ReplaceVector(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, id string, revision int, sourceDigest, fingerprint string, vector []byte) error {
	if err := validateVectorMutation(kind, id, revision, sourceDigest, fingerprint, vector); err != nil {
		return err
	}
	dimensions := len(vector) / 4
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: begin vector replacement: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM knowledge_embeddings WHERE item_kind = ? AND item_id = ? AND model_fingerprint = ?`,
		kind, id, fingerprint); err != nil {
		return fmt.Errorf("%w: delete vector row: %v", port.ErrKnowledgeUnavailable, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, strftime('%s', 'now'))`,
		kind, id, revision, sourceDigest, fingerprint, dimensions, vector); err != nil {
		return fmt.Errorf("%w: insert vector row: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit vector replacement: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}

// DeleteVector removes every fingerprint row of one identity. It is used on
// missing, deleted, ineligible, or redacted-empty items.
func (s *KnowledgeVectorIndexStore) DeleteVector(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, id string) error {
	if !domain.ValidKnowledgeRetrievalItemKind(kind) {
		return fmt.Errorf("%w: unknown vector index kind", port.ErrKnowledgeValidation)
	}
	if id == "" || utf8.RuneCountInString(id) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return fmt.Errorf("%w: vector index identity is not bounded", port.ErrKnowledgeValidation)
	}
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM knowledge_embeddings WHERE item_kind = ? AND item_id = ?`, kind, id); err != nil {
		return fmt.Errorf("%w: delete vector rows: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}

// ListVector pages identity metadata scoped to one fingerprint in stable
// identity order for reconciliation. Rows carry only metadata, never vector
// bytes.
func (s *KnowledgeVectorIndexStore) ListVector(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, fingerprint, afterID string, limit int) ([]port.KnowledgeVectorIndexRow, error) {
	if !domain.ValidKnowledgeRetrievalItemKind(kind) {
		return nil, fmt.Errorf("%w: unknown vector index kind", port.ErrKnowledgeValidation)
	}
	if fingerprint == "" || utf8.RuneCountInString(fingerprint) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return nil, fmt.Errorf("%w: vector index fingerprint must be a bounded non-empty string", port.ErrKnowledgeValidation)
	}
	if !domain.ValidKnowledgeQueueListLimit(limit) {
		return nil, fmt.Errorf("%w: vector index list limit is not bounded", port.ErrKnowledgeValidation)
	}
	if afterID != "" && utf8.RuneCountInString(afterID) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return nil, fmt.Errorf("%w: vector index page cursor is not bounded", port.ErrKnowledgeValidation)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT item_id, item_revision, source_digest, model_fingerprint FROM knowledge_embeddings
		WHERE item_kind = ? AND model_fingerprint = ? AND item_id > ?
		ORDER BY item_id LIMIT ?`, kind, fingerprint, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: list vector index rows: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer rows.Close()
	result := make([]port.KnowledgeVectorIndexRow, 0, limit)
	for rows.Next() {
		var row port.KnowledgeVectorIndexRow
		row.Kind = kind
		if err := rows.Scan(&row.ID, &row.Revision, &row.SourceDigest, &row.Fingerprint); err != nil {
			return nil, fmt.Errorf("%w: vector index row scan: %v", port.ErrKnowledgeUnavailable, err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: vector index row scan: %v", port.ErrKnowledgeUnavailable, err)
	}
	return result, nil
}

// ClearVector removes every reconstructible vector row for rebuild. It
// never touches authoritative knowledge tables.
func (s *KnowledgeVectorIndexStore) ClearVector(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM knowledge_embeddings`); err != nil {
		return fmt.Errorf("%w: clear vector index: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}
