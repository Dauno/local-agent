package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// migrateV39 adds reconstructible retrieval state over verified v38: the
// lexical FTS5 index, the optional embeddings store, the one-per-identity
// lexical/embedding queues, the reference hot-path index, and atomic
// enqueue triggers. It seeds both queues for every existing claim,
// preference, and document without copying authoritative content into FTS
// or contacting a provider. No truth table gains a foreign key to an index
// table. Queue timestamps are unix seconds.
func migrateV39(ctx context.Context, tx *sql.Tx) error {
	if err := execMigration(ctx, tx, 39, []string{
		`CREATE VIRTUAL TABLE knowledge_retrieval_fts USING fts5(
			item_kind UNINDEXED,
			item_id UNINDEXED,
			item_revision UNINDEXED,
			source_digest UNINDEXED,
			subject,
			body,
			tokenize='unicode61'
		)`,
		`CREATE TABLE knowledge_embeddings (
			item_kind TEXT NOT NULL,
			item_id TEXT NOT NULL,
			item_revision INTEGER NOT NULL,
			source_digest TEXT NOT NULL,
			model_fingerprint TEXT NOT NULL,
			dimensions INTEGER NOT NULL,
			vector BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (item_kind, item_id, model_fingerprint),
			CHECK (item_kind IN ('claim', 'preference', 'document')),
			CHECK (length(item_id) > 0 AND length(item_id) <= 256),
			CHECK (item_revision >= 1),
			CHECK (length(source_digest) = 64 AND source_digest NOT GLOB '*[^0-9a-f]*'),
			CHECK (length(model_fingerprint) > 0),
			CHECK (dimensions BETWEEN 1 AND 4096),
			CHECK (typeof(vector) = 'blob'),
			CHECK (length(vector) = dimensions * 4),
			CHECK (created_at > 0)
		)`,
		`CREATE TABLE knowledge_lexical_queue (
			item_kind TEXT NOT NULL,
			item_id TEXT NOT NULL,
			generation INTEGER NOT NULL,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL,
			next_attempt INTEGER NOT NULL,
			lease_until INTEGER NOT NULL,
			last_error TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (item_kind, item_id),
			CHECK (item_kind IN ('claim', 'preference', 'document')),
			CHECK (length(item_id) > 0 AND length(item_id) <= 256),
			CHECK (generation >= 0 AND attempts >= 0),
			CHECK (status IN ('pending', 'processing', 'done', 'failed')),
			CHECK (next_attempt >= 0 AND lease_until >= 0),
			CHECK (last_error IN ('', 'source_invalid', 'provider_invalid', 'attempts_exhausted')),
			CHECK (created_at > 0 AND updated_at > 0 AND updated_at >= created_at)
		)`,
		`CREATE TABLE knowledge_embedding_queue (
			item_kind TEXT NOT NULL,
			item_id TEXT NOT NULL,
			generation INTEGER NOT NULL,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL,
			next_attempt INTEGER NOT NULL,
			lease_until INTEGER NOT NULL,
			last_error TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (item_kind, item_id),
			CHECK (item_kind IN ('claim', 'preference', 'document')),
			CHECK (length(item_id) > 0 AND length(item_id) <= 256),
			CHECK (generation >= 0 AND attempts >= 0),
			CHECK (status IN ('pending', 'processing', 'done', 'failed')),
			CHECK (next_attempt >= 0 AND lease_until >= 0),
			CHECK (last_error IN ('', 'source_invalid', 'provider_invalid', 'attempts_exhausted')),
			CHECK (created_at > 0 AND updated_at > 0 AND updated_at >= created_at)
		)`,
		`CREATE INDEX knowledge_claims_by_reference_scope_status
			ON knowledge_claims (value_reference, scope_kind, scope_id, status)
			WHERE value_reference != ''`,
		`CREATE INDEX knowledge_lexical_queue_by_pending_next
			ON knowledge_lexical_queue (status, next_attempt)`,
		`CREATE INDEX knowledge_lexical_queue_by_processing_lease
			ON knowledge_lexical_queue (status, lease_until)`,
		`CREATE INDEX knowledge_embedding_queue_by_pending_next
			ON knowledge_embedding_queue (status, next_attempt)`,
		`CREATE INDEX knowledge_embedding_queue_by_processing_lease
			ON knowledge_embedding_queue (status, lease_until)`,
		`CREATE TRIGGER knowledge_claims_enqueue_after_insert
			AFTER INSERT ON knowledge_claims
			BEGIN
				INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('claim', NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_lexical_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
				INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('claim', NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_embedding_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
			END`,
		`CREATE TRIGGER knowledge_claims_enqueue_after_update
			AFTER UPDATE ON knowledge_claims
			WHEN NEW.current_rev != OLD.current_rev OR NEW.status != OLD.status
			BEGIN
				INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('claim', NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_lexical_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
				INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('claim', NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_embedding_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
			END`,
		`CREATE TRIGGER knowledge_claims_enqueue_after_delete
			AFTER DELETE ON knowledge_claims
			BEGIN
				INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('claim', OLD.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_lexical_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
				INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('claim', OLD.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_embedding_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
			END`,
		`CREATE TRIGGER knowledge_preferences_enqueue_after_insert
			AFTER INSERT ON knowledge_preferences
			BEGIN
				INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('preference', 'preference:' || NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_lexical_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
				INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('preference', 'preference:' || NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_embedding_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
			END`,
		`CREATE TRIGGER knowledge_preferences_enqueue_after_update
			AFTER UPDATE ON knowledge_preferences
			WHEN NEW.current_rev != OLD.current_rev OR NEW.status != OLD.status
				OR NEW.value_kind != OLD.value_kind OR NEW.value_text != OLD.value_text
				OR NEW.value_number != OLD.value_number OR NEW.value_boolean != OLD.value_boolean
			BEGIN
				INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('preference', 'preference:' || NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_lexical_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
				INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('preference', 'preference:' || NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_embedding_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
			END`,
		`CREATE TRIGGER knowledge_preferences_enqueue_after_delete
			AFTER DELETE ON knowledge_preferences
			BEGIN
				INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('preference', 'preference:' || OLD.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_lexical_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
				INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('preference', 'preference:' || OLD.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_embedding_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
			END`,
		`CREATE TRIGGER knowledge_documents_enqueue_after_insert
			AFTER INSERT ON knowledge_documents
			BEGIN
				INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('document', NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_lexical_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
				INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('document', NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_embedding_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
			END`,
		`CREATE TRIGGER knowledge_documents_enqueue_after_update
			AFTER UPDATE ON knowledge_documents
			WHEN NEW.current_rev != OLD.current_rev OR NEW.status != OLD.status
			BEGIN
				INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('document', NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_lexical_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
				INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('document', NEW.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_embedding_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
			END`,
		`CREATE TRIGGER knowledge_documents_enqueue_after_delete
			AFTER DELETE ON knowledge_documents
			BEGIN
				INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('document', OLD.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_lexical_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
				INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
				VALUES ('document', OLD.id, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
				ON CONFLICT(item_kind, item_id) DO UPDATE SET
					generation = knowledge_embedding_queue.generation + 1,
					status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
					last_error = '', updated_at = strftime('%s', 'now');
			END`,
	}); err != nil {
		return err
	}

	now := time.Now().UTC().Unix()
	seeds := []string{
		`INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
			SELECT 'claim', id, current_rev, 'pending', 0, 0, 0, '', ?, ? FROM knowledge_claims`,
		`INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
			SELECT 'claim', id, current_rev, 'pending', 0, 0, 0, '', ?, ? FROM knowledge_claims`,
		`INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
			SELECT 'preference', 'preference:' || id, current_rev, 'pending', 0, 0, 0, '', ?, ? FROM knowledge_preferences`,
		`INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
			SELECT 'preference', 'preference:' || id, current_rev, 'pending', 0, 0, 0, '', ?, ? FROM knowledge_preferences`,
		`INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
			SELECT 'document', id, current_rev, 'pending', 0, 0, 0, '', ?, ? FROM knowledge_documents`,
		`INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
			SELECT 'document', id, current_rev, 'pending', 0, 0, 0, '', ?, ? FROM knowledge_documents`,
	}
	for i, seed := range seeds {
		if _, err := tx.ExecContext(ctx, seed, now, now); err != nil {
			return fmt.Errorf("seed SQLite schema v39 queue statement %d: %w", i+1, err)
		}
	}
	return nil
}
