package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// migrateV42 removes the retired memory V1 storage. Durable assistant
// exchange intents remain because they are part of the conversation ledger.
func migrateV42(ctx context.Context, tx *sql.Tx) error {
	if err := execMigration(ctx, tx, 42, []string{
		`INSERT INTO knowledge_projection_outbox (status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
			SELECT 'pending', 0, strftime('%s', 'now'), 0, '', strftime('%s', 'now'), strftime('%s', 'now')
			WHERE EXISTS (SELECT 1 FROM knowledge_documents WHERE provenance = 'legacy_curated_document')`,
		`DELETE FROM knowledge_retrieval_fts
			WHERE item_kind = 'document'
			AND item_id IN (SELECT id FROM knowledge_documents WHERE provenance = 'legacy_curated_document')`,
		`DELETE FROM knowledge_embeddings
			WHERE item_kind = 'document'
			AND item_id IN (SELECT id FROM knowledge_documents WHERE provenance = 'legacy_curated_document')`,
		`DELETE FROM knowledge_document_revisions
			WHERE document_id IN (SELECT id FROM knowledge_documents WHERE provenance = 'legacy_curated_document')`,
		`DELETE FROM knowledge_document_receipts
			WHERE document_id IN (SELECT id FROM knowledge_documents WHERE provenance = 'legacy_curated_document')`,
		`DELETE FROM knowledge_documents WHERE provenance = 'legacy_curated_document'`,
		`DELETE FROM knowledge_lexical_queue
			WHERE item_kind = 'document'
			AND item_id NOT IN (SELECT id FROM knowledge_documents)`,
		`DELETE FROM knowledge_embedding_queue
			WHERE item_kind = 'document'
			AND item_id NOT IN (SELECT id FROM knowledge_documents)`,
		`DROP TRIGGER IF EXISTS knowledge_documents_enqueue_after_insert`,
		`DROP TRIGGER IF EXISTS knowledge_documents_enqueue_after_update`,
		`DROP TRIGGER IF EXISTS knowledge_documents_enqueue_after_delete`,
		`DROP TRIGGER IF EXISTS knowledge_documents_identity_immutable`,
		`DROP TRIGGER IF EXISTS knowledge_document_receipts_immutable_update`,
		`DROP TRIGGER IF EXISTS knowledge_document_revisions_immutable_update`,
		`DROP INDEX IF EXISTS knowledge_documents_by_scope_status`,
		`DROP INDEX IF EXISTS knowledge_document_revisions_by_document`,
		`ALTER TABLE knowledge_documents RENAME TO knowledge_documents_v41`,
		`ALTER TABLE knowledge_document_revisions RENAME TO knowledge_document_revisions_v41`,
		`ALTER TABLE knowledge_document_receipts RENAME TO knowledge_document_receipts_v41`,
		`CREATE TABLE knowledge_documents (
			id TEXT NOT NULL,
			subject TEXT NOT NULL,
			scope_kind TEXT NOT NULL CHECK (scope_kind IN ('global', 'team', 'user', 'project', 'conversation', 'workstream')),
			scope_id TEXT NOT NULL DEFAULT '',
			content_digest TEXT NOT NULL CHECK (length(content_digest) = 64 AND content_digest NOT GLOB '*[^0-9a-f]*'),
			content_handle TEXT NOT NULL,
			source_id TEXT NOT NULL DEFAULT '',
			source_rev INTEGER NOT NULL DEFAULT 0,
			provenance TEXT NOT NULL CHECK (provenance IN ('curated')),
			status TEXT NOT NULL CHECK (status IN ('active', 'archived')),
			current_rev INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (id),
			UNIQUE (subject, scope_kind, scope_id),
			CHECK ((scope_kind = 'global') = (scope_id = '')),
			CHECK (source_id = '' AND source_rev = 0),
			CHECK (length(subject) > 0 AND length(content_handle) > 0 AND current_rev >= 1 AND created_at > 0 AND updated_at > 0)
		)`,
		`INSERT INTO knowledge_documents
			(id, subject, scope_kind, scope_id, content_digest, content_handle, source_id, source_rev, provenance, status, current_rev, created_at, updated_at)
			SELECT id, subject, scope_kind, scope_id, content_digest, content_handle, source_id, source_rev, provenance, status, current_rev, created_at, updated_at
			FROM knowledge_documents_v41`,
		`CREATE TABLE knowledge_document_revisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			document_id TEXT NOT NULL,
			revision_number INTEGER NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('active', 'archived')),
			source_ref TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			FOREIGN KEY (document_id) REFERENCES knowledge_documents(id) ON DELETE CASCADE,
			UNIQUE (document_id, revision_number),
			UNIQUE (document_id, source_ref),
			CHECK (revision_number >= 1 AND length(source_ref) > 0 AND created_at > 0)
		)`,
		`INSERT INTO knowledge_document_revisions (id, document_id, revision_number, status, source_ref, created_at)
			SELECT id, document_id, revision_number, status, source_ref, created_at
			FROM knowledge_document_revisions_v41`,
		`CREATE TABLE knowledge_document_receipts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			document_id TEXT NOT NULL,
			subject TEXT NOT NULL,
			scope_kind TEXT NOT NULL CHECK (scope_kind IN ('global', 'team', 'user', 'project', 'conversation', 'workstream')),
			scope_id TEXT NOT NULL DEFAULT '',
			content_digest TEXT NOT NULL CHECK (length(content_digest) = 64 AND content_digest NOT GLOB '*[^0-9a-f]*'),
			content_handle TEXT NOT NULL,
			source_id TEXT NOT NULL DEFAULT '',
			source_rev INTEGER NOT NULL DEFAULT 0,
			provenance TEXT NOT NULL CHECK (provenance IN ('curated')),
			status TEXT NOT NULL CHECK (status IN ('active', 'archived')),
			created_at INTEGER NOT NULL,
			FOREIGN KEY (document_id) REFERENCES knowledge_documents(id) ON DELETE CASCADE,
			UNIQUE (document_id),
			CHECK ((scope_kind = 'global') = (scope_id = '')),
			CHECK (source_id = '' AND source_rev = 0),
			CHECK (length(subject) > 0 AND length(content_handle) > 0 AND created_at > 0)
		)`,
		`INSERT INTO knowledge_document_receipts
			(id, document_id, subject, scope_kind, scope_id, content_digest, content_handle, source_id, source_rev, provenance, status, created_at)
			SELECT id, document_id, subject, scope_kind, scope_id, content_digest, content_handle, source_id, source_rev, provenance, status, created_at
			FROM knowledge_document_receipts_v41`,
		`DROP TABLE knowledge_document_revisions_v41`,
		`DROP TABLE knowledge_document_receipts_v41`,
		`DROP TABLE knowledge_documents_v41`,
		`CREATE INDEX knowledge_documents_by_scope_status
			ON knowledge_documents (scope_kind, scope_id, status)`,
		`CREATE INDEX knowledge_document_revisions_by_document
			ON knowledge_document_revisions (document_id, revision_number)`,
		`CREATE TRIGGER knowledge_document_receipts_immutable_update
			BEFORE UPDATE ON knowledge_document_receipts
			BEGIN SELECT RAISE(ABORT, 'knowledge document receipt is immutable'); END`,
		`CREATE TRIGGER knowledge_document_revisions_immutable_update
			BEFORE UPDATE ON knowledge_document_revisions
			BEGIN SELECT RAISE(ABORT, 'knowledge document revision is immutable'); END`,
		`CREATE TRIGGER knowledge_documents_identity_immutable
			BEFORE UPDATE ON knowledge_documents
			WHEN NEW.id != OLD.id OR NEW.subject != OLD.subject OR NEW.scope_kind != OLD.scope_kind
				OR NEW.scope_id != OLD.scope_id OR NEW.content_digest != OLD.content_digest
				OR NEW.content_handle != OLD.content_handle OR NEW.source_id != OLD.source_id
				OR NEW.source_rev != OLD.source_rev OR NEW.provenance != OLD.provenance
				OR NEW.created_at != OLD.created_at
			BEGIN SELECT RAISE(ABORT, 'knowledge document identity is immutable'); END`,
		`CREATE TRIGGER knowledge_documents_enqueue_after_insert
			AFTER INSERT ON knowledge_documents
			BEGIN
				INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('document', NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET generation = knowledge_lexical_queue.generation + 1, status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0, last_error = '', updated_at = strftime('%s', 'now');
				INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('document', NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET generation = knowledge_embedding_queue.generation + 1, status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0, last_error = '', updated_at = strftime('%s', 'now');
			END`,
		`CREATE TRIGGER knowledge_documents_enqueue_after_update
			AFTER UPDATE ON knowledge_documents
			WHEN NEW.current_rev != OLD.current_rev OR NEW.status != OLD.status
			BEGIN
				INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('document', NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET generation = knowledge_lexical_queue.generation + 1, status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0, last_error = '', updated_at = strftime('%s', 'now');
				INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('document', NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET generation = knowledge_embedding_queue.generation + 1, status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0, last_error = '', updated_at = strftime('%s', 'now');
			END`,
		`CREATE TRIGGER knowledge_documents_enqueue_after_delete
			AFTER DELETE ON knowledge_documents
			BEGIN
				INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('document', OLD.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET generation = knowledge_lexical_queue.generation + 1, status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0, last_error = '', updated_at = strftime('%s', 'now');
				INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('document', OLD.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET generation = knowledge_embedding_queue.generation + 1, status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0, last_error = '', updated_at = strftime('%s', 'now');
			END`,
		`DROP TRIGGER IF EXISTS memory_topic_revisions_guard_referenced_update`,
		`DROP TRIGGER IF EXISTS memory_topic_revisions_guard_referenced_delete`,
		`DROP TABLE IF EXISTS memory_evidence`,
		`DROP TABLE IF EXISTS memory_topic_links`,
		`DROP TABLE IF EXISTS memory_topic_revisions`,
		`DROP TABLE IF EXISTS memory_patch_receipts`,
		`DROP TABLE IF EXISTS memory_outbox_items`,
		`DROP TABLE IF EXISTS memory_outbox`,
		`DROP TABLE IF EXISTS memory_topics_fts`,
		`DROP TABLE IF EXISTS memory_links`,
		`DROP TABLE IF EXISTS memory_topics`,
	}); err != nil {
		return err
	}
	return v42ProtectOrRemoveCuratedDocuments(ctx, tx)
}

// v42ProtectOrRemoveCuratedDocuments closes a gap left by the schema rebuild
// above: it copies every surviving 'curated' document across unchanged, but
// none of them ever held a result_references row, so a retention sweep
// could delete a result an active document still depends on. Each surviving
// document is re-validated with the same identity, digest, storage, and
// scope rule CreateDocument now enforces; a document that still passes gets
// the live reference it was always supposed to have, and an active document
// that no longer resolves (unknown result, digest drift, or a scope with no
// safe authorization rule such as global or workstream) is removed rather
// than left as content nothing can ever read back. An archived document that
// fails the check is removed too, since it is equally unresolvable and
// carries no live reference to release; one that still passes is left alone,
// consistent with ArchiveDocument's own contract that an archived document
// holds no live reference.
func v42ProtectOrRemoveCuratedDocuments(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, scope_kind, scope_id, content_digest, content_handle, status
		FROM knowledge_documents WHERE provenance = 'curated'`)
	if err != nil {
		return fmt.Errorf("v42: list curated documents: %w", err)
	}
	type curatedDocumentRow struct {
		id, scopeKind, scopeID, digest, handle, status string
	}
	var candidates []curatedDocumentRow
	for rows.Next() {
		var row curatedDocumentRow
		if scanErr := rows.Scan(&row.id, &row.scopeKind, &row.scopeID, &row.digest, &row.handle, &row.status); scanErr != nil {
			_ = rows.Close()
			return fmt.Errorf("v42: scan curated document: %w", scanErr)
		}
		candidates = append(candidates, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("v42: list curated documents: %w", err)
	}
	_ = rows.Close()

	now := time.Now().UTC()
	removedAny := false
	for _, row := range candidates {
		document := domain.KnowledgeDocument{
			ScopeKind: domain.KnowledgeScopeKind(row.scopeKind), ScopeID: row.scopeID,
			ContentDigest: row.digest, ContentHandle: row.handle,
			Provenance: domain.KnowledgeProvenanceCurated,
		}
		resultID, verifyErr := verifyCuratedDocumentResult(ctx, tx, document)
		if verifyErr != nil {
			if !errors.Is(verifyErr, port.ErrKnowledgeValidation) {
				return fmt.Errorf("v42: verify curated document %q: %w", row.id, verifyErr)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM knowledge_retrieval_fts WHERE item_kind = 'document' AND item_id = ?`, row.id); err != nil {
				return fmt.Errorf("v42: remove unprotectable document fts %q: %w", row.id, err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM knowledge_embeddings WHERE item_kind = 'document' AND item_id = ?`, row.id); err != nil {
				return fmt.Errorf("v42: remove unprotectable document embeddings %q: %w", row.id, err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = ?`, row.id); err != nil {
				return fmt.Errorf("v42: remove unprotectable document %q: %w", row.id, err)
			}
			removedAny = true
			continue
		}
		if row.status != string(domain.KnowledgeDocumentActive) {
			continue
		}
		if err := retainCuratedDocumentResult(ctx, tx, row.id, resultID, now); err != nil {
			return fmt.Errorf("v42: retain result reference for document %q: %w", row.id, err)
		}
	}
	if removedAny {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO knowledge_projection_outbox (status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
			VALUES ('pending', 0, ?, 0, '', ?, ?)`, now.UnixNano(), now.UnixNano(), now.UnixNano()); err != nil {
			return fmt.Errorf("v42: enqueue projection for removed documents: %w", err)
		}
	}
	return nil
}
