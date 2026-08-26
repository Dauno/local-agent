package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMigrationV39FreshObjectsAndConstraints(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/fresh-v39.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	db := store.DB()

	for _, object := range []string{
		"knowledge_retrieval_fts",
		"knowledge_retrieval_fts_config", "knowledge_retrieval_fts_content",
		"knowledge_retrieval_fts_data", "knowledge_retrieval_fts_docsize",
		"knowledge_retrieval_fts_idx",
		"knowledge_embeddings",
		"knowledge_lexical_queue", "knowledge_embedding_queue",
		"knowledge_claims_by_reference_scope_status",
		"knowledge_lexical_queue_by_pending_next",
		"knowledge_lexical_queue_by_processing_lease",
		"knowledge_embedding_queue_by_pending_next",
		"knowledge_embedding_queue_by_processing_lease",
	} {
		var present int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE name = ?`, object).Scan(&present); err != nil || present != 1 {
			t.Fatalf("v39 object %q present = %d, %v", object, present, err)
		}
	}
	for _, trigger := range []string{
		"knowledge_claims_enqueue_after_insert",
		"knowledge_claims_enqueue_after_update",
		"knowledge_claims_enqueue_after_delete",
		"knowledge_preferences_enqueue_after_insert",
		"knowledge_preferences_enqueue_after_update",
		"knowledge_preferences_enqueue_after_delete",
		"knowledge_documents_enqueue_after_insert",
		"knowledge_documents_enqueue_after_update",
		"knowledge_documents_enqueue_after_delete",
	} {
		var present int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'trigger' AND name = ?`, trigger).Scan(&present); err != nil || present != 1 {
			t.Fatalf("v39 trigger %q present = %d, %v", trigger, present, err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO knowledge_retrieval_fts (item_kind, item_id, item_revision, source_digest, subject, body)
		VALUES ('claim', 'c1', 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'subject', 'body')`); err != nil {
		t.Fatalf("FTS5 insert failed: %v", err)
	}
	var ftsRows int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_retrieval_fts WHERE knowledge_retrieval_fts MATCH 'subject'`).Scan(&ftsRows); err != nil || ftsRows != 1 {
		t.Fatalf("FTS5 match rows = %d, %v", ftsRows, err)
	}

	now := time.Now().UTC().Unix()
	insert := func(name, statement string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), statement, args...); err == nil {
			t.Errorf("%s: statement unexpectedly succeeded", name)
		}
	}
	insert("queue unknown kind", `INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('topic', 'x', 1, 'pending', 0, 0, 0, '', ?, ?)`, now, now)
	insert("queue unknown status", `INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', 'x', 1, 'running', 0, 0, 0, '', ?, ?)`, now, now)
	insert("queue negative generation", `INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', 'x', -1, 'pending', 0, 0, 0, '', ?, ?)`, now, now)
	insert("queue negative attempts", `INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', 'x', 1, 'pending', -1, 0, 0, '', ?, ?)`, now, now)
	insert("queue unknown failure code", `INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', 'x', 1, 'failed', 0, 0, 0, 'invented', ?, ?)`, now, now)
	insert("queue empty identity", `INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', '', 1, 'pending', 0, 0, 0, '', ?, ?)`, now, now)
	insert("queue oversized identity", `INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', ?, 1, 'pending', 0, 0, 0, '', ?, ?)`, strings.Repeat("a", 257), now, now)
	insert("queue negative next attempt", `INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', 'x', 1, 'pending', 0, -1, 0, '', ?, ?)`, now, now)
	insert("queue negative lease", `INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', 'x', 1, 'processing', 0, 0, -1, '', ?, ?)`, now, now)
	insert("queue incoherent timestamps", `INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', 'x', 1, 'pending', 0, 0, 0, '', ?, ?)`, now, now-10)

	insert("embedding zero dimensions", `INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES ('claim', 'c1', 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'fp', 0, X'0000', ?)`, now)
	insert("embedding excessive dimensions", `INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES ('claim', 'c1', 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'fp', 4097, X'0000', ?)`, now)
	insert("embedding vector length mismatch", `INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES ('claim', 'c1', 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'fp', 2, X'00000000', ?)`, now)
	insert("embedding text vector", `INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES ('claim', 'c1', 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'fp', 1, 'abcd', ?)`, now)
	insert("embedding numeric vector", `INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES ('claim', 'c1', 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'fp', 1, 1234, ?)`, now)
	insert("embedding null vector", `INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES ('claim', 'c1', 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'fp', 1, NULL, ?)`, now)
	insert("embedding uppercase digest", `INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES ('claim', 'c1', 1, 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', 'fp', 1, X'00000000', ?)`, now)
	insert("embedding short digest", `INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES ('claim', 'c1', 1, 'abcd', 'fp', 1, X'00000000', ?)`, now)
	insert("embedding empty fingerprint", `INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES ('claim', 'c1', 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', '', 1, X'00000000', ?)`, now)
	insert("embedding zero revision", `INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES ('claim', 'c1', 0, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'fp', 1, X'00000000', ?)`, now)
	insert("embedding unknown kind", `INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES ('topic', 'c1', 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'fp', 1, X'00000000', ?)`, now)

	if _, err := db.ExecContext(t.Context(), `INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', 'valid', 1, 'pending', 0, 0, 0, '', ?, ?)`, now, now); err != nil {
		t.Fatalf("valid queue insert failed: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES ('claim', 'c1', 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'fp', 2, X'0000000000000000', ?)`, now); err != nil {
		t.Fatalf("valid embedding insert failed: %v", err)
	}
}

func TestMigrationV39UpgradePreservesStateAndSeedsQueues(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 38)
	now := time.Now().UTC().UnixNano()
	insert := func(statement string, args ...any) {
		t.Helper()
		if _, err := raw.ExecContext(t.Context(), statement, args...); err != nil {
			_ = raw.Close()
			t.Fatalf("seed v38 row: %v", err)
		}
	}
	insert(`INSERT INTO memory_topics (id, slug, title, description, status, tags, bundle_path, owner_key, content, current_rev, created_at, updated_at)
		VALUES ('mem_legacy1', 'legacy', 'Legacy', '', 'active', '[]', 'topics', '', 'legacy content', 1, 1, 1)`)
	insert(`INSERT INTO memory_topic_revisions (topic_id, revision_number, content, change_reason, created_at)
		VALUES ('mem_legacy1', 1, 'legacy content', 'init', 1)`)
	insert(
		`INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, valid_from, valid_until, current_rev, created_at, updated_at)
		VALUES ('claim-a', 'api', 'is', 'string', 'x', 'project', 'p', 'human', 'r1', 'asserted', 0, 0, 1, ?, ?)`,
		now,
		now,
	)
	insert(
		`INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, valid_from, valid_until, current_rev, created_at, updated_at)
		VALUES ('claim-b', 'db', 'uses', 'string', 'y', 'project', 'p', 'human', 'r2', 'archived', 0, 0, 3, ?, ?)`,
		now,
		now,
	)
	insert(`INSERT INTO knowledge_preferences (owner_key, key, value_kind, value_text, status, source_ref, current_rev, created_at, updated_at)
		VALUES ('slack:T1:user:U1', 'theme', 'string', 'dark', 'active', 'r3', 1, ?, ?)`, now, now)
	insert(`INSERT INTO knowledge_preferences (owner_key, key, value_kind, value_number, status, source_ref, current_rev, created_at, updated_at)
		VALUES ('slack:T1:user:U1', 'budget', 'number', 7, 'archived', 'r4', 2, ?, ?)`, now, now)
	insert(`INSERT INTO knowledge_documents (id, subject, scope_kind, scope_id, content_digest, content_handle, source_id, source_rev, provenance, status, current_rev, created_at, updated_at)
		VALUES ('doc-a', 'legacy', 'global', '', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'memory_topics:mem_legacy1:revision:1', 'mem_legacy1', 1, 'legacy_curated_document', 'active', 1, ?, ?)`, now, now)
	insert(`INSERT INTO knowledge_documents (id, subject, scope_kind, scope_id, content_digest, content_handle, source_id, source_rev, provenance, status, current_rev, created_at, updated_at)
		VALUES ('doc-b', 'other', 'global', '', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 'memory_topics:mem_legacy1:revision:1', 'mem_legacy1', 1, 'legacy_curated_document', 'archived', 4, ?, ?)`, now, now)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upgraded.Close() }()
	db := upgraded.DB()

	var version int
	if err := db.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("upgraded version = %d, %v", version, err)
	}
	for table, want := range map[string]int{
		"knowledge_claims":      2,
		"knowledge_preferences": 2,
		"knowledge_documents":   0,
	} {
		var count int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != want {
			t.Fatalf("preserved %s rows = %d, %v", table, count, err)
		}
	}

	// V39 seeds current claim and preference identities. V42 removes the
	// document rows and their queue work with the retired legacy documents.
	wantRows := []struct {
		kind       string
		itemID     string
		generation int
	}{
		{"claim", "claim-a", 1},
		{"claim", "claim-b", 3},
		{"preference", "preference:1", 1},
		{"preference", "preference:2", 2},
	}
	for _, queue := range []string{"knowledge_lexical_queue", "knowledge_embedding_queue"} {
		var total int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+queue).Scan(&total); err != nil || total != len(wantRows) {
			t.Fatalf("%s seeded rows = %d, %v", queue, total, err)
		}
		for _, want := range wantRows {
			var status, lastError string
			var generation, attempts, nextAttempt, leaseUntil int
			err := db.QueryRowContext(t.Context(), `
				SELECT generation, status, attempts, next_attempt, lease_until, last_error
				FROM `+queue+` WHERE item_kind = ? AND item_id = ?`, want.kind, want.itemID).
				Scan(&generation, &status, &attempts, &nextAttempt, &leaseUntil, &lastError)
			if err != nil {
				t.Fatalf("%s row %s:%s missing: %v", queue, want.kind, want.itemID, err)
			}
			if generation != want.generation || status != "pending" || attempts != 0 || nextAttempt != 0 || leaseUntil != 0 || lastError != "" {
				t.Fatalf("%s row %s:%s = gen %d status %q attempts %d next %d lease %d error %q",
					queue, want.kind, want.itemID, generation, status, attempts, nextAttempt, leaseUntil, lastError)
			}
		}
	}
	var ftsRows, embeddingRows int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_retrieval_fts`).Scan(&ftsRows); err != nil || ftsRows != 0 {
		t.Fatalf("FTS rows after migration = %d, %v", ftsRows, err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_embeddings`).Scan(&embeddingRows); err != nil || embeddingRows != 0 {
		t.Fatalf("embedding rows after migration = %d, %v", embeddingRows, err)
	}
}

func TestKnowledgeRetrievalQueueEnqueueOnMutations(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/enqueue.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	db := store.DB()
	now := time.Now().UTC().UnixNano()

	queueState := func(kind, itemID string) (generation, attempts, nextAttempt, leaseUntil int, status, lastError string) {
		t.Helper()
		err := db.QueryRowContext(t.Context(), `
			SELECT generation, status, attempts, next_attempt, lease_until, last_error
			FROM knowledge_lexical_queue WHERE item_kind = ? AND item_id = ?`, kind, itemID).
			Scan(&generation, &status, &attempts, &nextAttempt, &leaseUntil, &lastError)
		if err != nil {
			t.Fatalf("lexical queue %s:%s missing: %v", kind, itemID, err)
		}
		return generation, attempts, nextAttempt, leaseUntil, status, lastError
	}
	assertBothQueues := func(kind, itemID string, generation int) {
		t.Helper()
		for _, queue := range []string{"knowledge_lexical_queue", "knowledge_embedding_queue"} {
			var gen, attempts, nextAttempt, leaseUntil int
			var status, lastError string
			if err := db.QueryRowContext(t.Context(), `
				SELECT generation, status, attempts, next_attempt, lease_until, last_error
				FROM `+queue+` WHERE item_kind = ? AND item_id = ?`, kind, itemID).
				Scan(&gen, &status, &attempts, &nextAttempt, &leaseUntil, &lastError); err != nil {
				t.Fatalf("%s row %s:%s missing: %v", queue, kind, itemID, err)
			}
			if gen != generation || status != "pending" || attempts != 0 || nextAttempt != 0 || leaseUntil != 0 || lastError != "" {
				t.Fatalf("%s row %s:%s = gen %d status %q attempts %d next %d lease %d error %q",
					queue, kind, itemID, gen, status, attempts, nextAttempt, leaseUntil, lastError)
			}
		}
	}

	// Insert enqueues both identities with generation 1 pending.
	if _, err := db.ExecContext(
		t.Context(),
		`INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, created_at, updated_at)
		VALUES ('c1', 'api', 'is', 'string', 'x', 'project', 'p', 'human', 'r1', 'asserted', ?, ?)`,
		now,
		now,
	); err != nil {
		t.Fatal(err)
	}
	assertBothQueues("claim", "c1", 1)

	// A mutation on a settled queue row increments generation and resets
	// attempts/lease/error state.
	if _, err := db.ExecContext(
		t.Context(),
		`UPDATE knowledge_lexical_queue SET status = 'done', attempts = 5, next_attempt = 99, lease_until = 88, last_error = 'attempts_exhausted' WHERE item_kind = 'claim' AND item_id = 'c1'`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE knowledge_claims SET status = 'verified', current_rev = current_rev + 1, updated_at = ? WHERE id = 'c1'`, now); err != nil {
		t.Fatal(err)
	}
	assertBothQueues("claim", "c1", 2)

	// A replay without recoverable mutation does not increment generation.
	if _, err := db.ExecContext(t.Context(), `UPDATE knowledge_claims SET status = 'verified' WHERE id = 'c1'`); err != nil {
		t.Fatal(err)
	}
	assertBothQueues("claim", "c1", 2)
	if _, err := db.ExecContext(t.Context(), `UPDATE knowledge_claims SET updated_at = updated_at WHERE id = 'c1'`); err != nil {
		t.Fatal(err)
	}
	assertBothQueues("claim", "c1", 2)

	// Deletion preserves identity in both queues for later cleanup.
	if _, err := db.ExecContext(t.Context(), `DELETE FROM knowledge_claims WHERE id = 'c1'`); err != nil {
		t.Fatal(err)
	}
	assertBothQueues("claim", "c1", 3)

	// Preference identity is canonical preference:<row-id>.
	if _, err := db.ExecContext(t.Context(), `INSERT INTO knowledge_preferences (owner_key, key, value_kind, value_text, status, source_ref, created_at, updated_at)
		VALUES ('slack:T1:user:U1', 'theme', 'string', 'dark', 'active', 'r2', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	assertBothQueues("preference", "preference:1", 1)
	if _, err := db.ExecContext(t.Context(), `UPDATE knowledge_preferences SET value_text = 'light', current_rev = current_rev + 1, updated_at = ? WHERE id = 1`, now); err != nil {
		t.Fatal(err)
	}
	assertBothQueues("preference", "preference:1", 2)
	if _, err := db.ExecContext(t.Context(), `DELETE FROM knowledge_preferences WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	assertBothQueues("preference", "preference:1", 3)

	// Documents follow the same contract.
	if _, err := db.ExecContext(t.Context(), `INSERT INTO knowledge_documents (id, subject, scope_kind, scope_id, content_digest, content_handle, provenance, status, created_at, updated_at)
		VALUES ('d1', 'doc', 'project', 'p', 'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc', 'h', 'curated', 'active', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	assertBothQueues("document", "d1", 1)
	if _, err := db.ExecContext(t.Context(), `UPDATE knowledge_documents SET status = 'archived', current_rev = current_rev + 1, updated_at = ? WHERE id = 'd1'`, now); err != nil {
		t.Fatal(err)
	}
	assertBothQueues("document", "d1", 2)
	if _, err := db.ExecContext(t.Context(), `DELETE FROM knowledge_documents WHERE id = 'd1'`); err != nil {
		t.Fatal(err)
	}
	assertBothQueues("document", "d1", 3)

	// Queue state helper sanity for the deleted claim.
	gen, _, _, _, status, lastErr := queueState("claim", "c1")
	if gen != 3 || status != "pending" || lastErr != "" {
		t.Fatalf("deleted claim queue state = gen %d status %q error %q", gen, status, lastErr)
	}
}

func TestKnowledgeRetrievalQueueEnqueueRollsBackWithTruth(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/rollback.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	db := store.DB()
	now := time.Now().UTC().UnixNano()

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(
		t.Context(),
		`INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, created_at, updated_at)
		VALUES ('c1', 'api', 'is', 'string', 'x', 'project', 'p', 'human', 'r1', 'asserted', ?, ?)`,
		now,
		now,
	); err != nil {
		t.Fatal(err)
	}
	var queueRows int
	if err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_lexical_queue WHERE item_kind = 'claim' AND item_id = 'c1'`).Scan(&queueRows); err != nil || queueRows != 1 {
		t.Fatalf("in-transaction queue rows = %d, %v", queueRows, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var claims, rows int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_claims WHERE id = 'c1'`).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(
		t.Context(),
		`SELECT (SELECT COUNT(*) FROM knowledge_lexical_queue WHERE item_kind = 'claim' AND item_id = 'c1') + (SELECT COUNT(*) FROM knowledge_embedding_queue WHERE item_kind = 'claim' AND item_id = 'c1')`,
	).Scan(
		&rows,
	); err != nil {
		t.Fatal(err)
	}
	if claims != 0 || rows != 0 {
		t.Fatalf("rolled-back state = %d claims, %d queue rows", claims, rows)
	}
}

func TestMigrationV39CrashRollsBackAndReopens(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 38)
	now := time.Now().UTC().UnixNano()
	if _, err := raw.ExecContext(
		t.Context(),
		`INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, created_at, updated_at)
		VALUES ('c1', 'api', 'is', 'string', 'x', 'project', 'p', 'human', 'r1', 'asserted', ?, ?)`,
		now,
		now,
	); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	original := migrations[39]
	defer func() { migrations[39] = original }()
	migrations[39] = func(ctx context.Context, tx *sql.Tx) error {
		if err := migrateV39(ctx, tx); err != nil {
			return err
		}
		return errors.New("injected v39 crash")
	}
	store, err := OpenExisting(t.Context(), path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting succeeded after injected v39 crash")
	}
	if err == nil || !strings.Contains(err.Error(), "injected v39 crash") {
		t.Fatalf("OpenExisting error = %v", err)
	}
	check, err := sql.Open("sqlite", mustDataSourceName(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = check.Close() }()
	var version, objects int
	if err := check.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE name = 'knowledge_retrieval_fts' OR name = 'knowledge_lexical_queue'`).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if version != 38 || objects != 0 {
		t.Fatalf("rolled-back v39 state = version %d/objects %d", version, objects)
	}

	migrations[39] = original
	reopened, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.DB().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("reopened version = %d, %v", version, err)
	}
	var queueRows int
	if err := reopened.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_lexical_queue`).Scan(&queueRows); err != nil || queueRows != 1 {
		t.Fatalf("reopened seeded lexical queue rows = %d, %v", queueRows, err)
	}
}

func TestMigrationV39ReopenPreservesRows(t *testing.T) {
	path := t.TempDir() + "/reopen.db"
	store, err := Initialize(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().UnixNano()
	if _, err := store.DB().ExecContext(
		t.Context(),
		`INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, created_at, updated_at)
		VALUES ('c1', 'api', 'is', 'string', 'x', 'project', 'p', 'human', 'r1', 'asserted', ?, ?)`,
		now,
		now,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	var claims, queueRows int
	if err := reopened.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_claims`).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("reopened claims = %d, %v", claims, err)
	}
	if err := reopened.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_lexical_queue`).Scan(&queueRows); err != nil || queueRows != 1 {
		t.Fatalf("reopened queue rows = %d, %v", queueRows, err)
	}
}
