package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMigrationV38FreshAndUpgradeRetireLegacyMemoryState(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/fresh-knowledge.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, table := range []string{
		"knowledge_claims", "knowledge_claim_revisions", "knowledge_evidence",
		"knowledge_preferences", "knowledge_preference_revisions",
		"knowledge_documents", "knowledge_document_receipts",
		"knowledge_tombstones", "knowledge_projection_outbox",
	} {
		if _, err := store.DB().ExecContext(t.Context(), `SELECT 1 FROM `+table); err != nil {
			t.Fatalf("fresh %s unavailable: %v", table, err)
		}
	}

	path, raw := createSchemaAtVersion(t, 37)
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO memory_topics (id, slug, title, description, status, tags, bundle_path, owner_key, content, current_rev, created_at, updated_at)
		VALUES ('mem_legacy1', 'legacy', 'Legacy', '', 'active', '[]', 'topics', '', 'content', 2, 1, 1)`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO memory_topic_revisions (topic_id, revision_number, content, change_reason, created_at)
		VALUES ('mem_legacy1', 2, 'content', 'change', 1)`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var legacyTopics int
	if err := upgraded.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE name = 'memory_topics'`).Scan(&legacyTopics); err != nil || legacyTopics != 0 {
		t.Fatalf("legacy memory topics table present = %d, %v", legacyTopics, err)
	}
	var version int
	if err := upgraded.DB().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("upgraded version = %d, %v", version, err)
	}
	// Explicit object verification replaces the historical fragile
	// knowledge_% table count: FTS5 shadow tables and v39 retrieval objects
	// share the prefix, so each object is checked by name instead.
	knowledgeTables := []string{
		"knowledge_claims", "knowledge_claim_revisions", "knowledge_evidence",
		"knowledge_preferences", "knowledge_preference_revisions",
		"knowledge_documents", "knowledge_document_revisions",
		"knowledge_document_receipts", "knowledge_tombstones",
		"knowledge_projection_outbox", "knowledge_command_receipts",
		"knowledge_retrieval_fts", "knowledge_embeddings",
		"knowledge_lexical_queue", "knowledge_embedding_queue",
	}
	for _, table := range knowledgeTables {
		var present int
		if err := upgraded.DB().QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM sqlite_schema WHERE name = ? AND type IN ('table', 'index')`,
			table).Scan(&present); err != nil || present != 1 {
			t.Fatalf("knowledge table %q present = %d, %v", table, present, err)
		}
	}
	for _, shadow := range []string{
		"knowledge_retrieval_fts_config", "knowledge_retrieval_fts_content",
		"knowledge_retrieval_fts_data", "knowledge_retrieval_fts_docsize",
		"knowledge_retrieval_fts_idx",
	} {
		var present int
		if err := upgraded.DB().QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM sqlite_schema WHERE name = ?`,
			shadow).Scan(&present); err != nil || present != 1 {
			t.Fatalf("FTS5 shadow object %q present = %d, %v", shadow, present, err)
		}
	}
}

func TestMigrationV38CrashRollsBackAndReopens(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 37)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	original := migrations[38]
	defer func() { migrations[38] = original }()
	migrations[38] = func(ctx context.Context, tx *sql.Tx) error {
		if err := migrateV38(ctx, tx); err != nil {
			return err
		}
		return errors.New("injected v38 crash")
	}
	store, err := OpenExisting(t.Context(), path)
	if store != nil {
		store.Close()
		t.Fatal("OpenExisting succeeded after injected v38 crash")
	}
	if err == nil || !strings.Contains(err.Error(), "injected v38 crash") {
		t.Fatalf("OpenExisting error = %v", err)
	}
	check, err := sql.Open("sqlite", mustDataSourceName(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var version, tables int
	if err := check.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'knowledge_claims'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if version != 37 || tables != 0 {
		t.Fatalf("rolled-back v38 state = version %d/table count %d", version, tables)
	}
}

func TestMigrationV38ConstraintNegatives(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/constraints.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := store.DB()
	now := time.Now().UTC().UnixNano()

	insert := func(name, statement string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), statement, args...); err == nil {
			t.Errorf("%s: statement unexpectedly succeeded", name)
		}
	}
	insert("unknown predicate", `INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, created_at, updated_at)
		VALUES ('c1', 'api', 'invented', 'string', 'x', 'project', 'p', 'human', 'r1', 'asserted', ?, ?)`, now, now)
	insert("removed curator claim source", `INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, created_at, updated_at)
		VALUES ('c1', 'api', 'is', 'string', 'x', 'project', 'p', 'curator', 'r1', 'asserted', ?, ?)`, now, now)
	insert("expired status", `INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, created_at, updated_at)
		VALUES ('c2', 'api', 'is', 'string', 'x', 'project', 'p', 'human', 'r2', 'expired', ?, ?)`, now, now)
	insert("global with identity", `INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, created_at, updated_at)
		VALUES ('c3', 'api', 'is', 'string', 'x', 'global', 'p', 'human', 'r3', 'asserted', ?, ?)`, now, now)
	insert("reference predicate with scalar", `INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, created_at, updated_at)
		VALUES ('c4', 'api', 'owns', 'string', 'x', 'project', 'p', 'human', 'r4', 'asserted', ?, ?)`, now, now)
	insert("ambiguous union value", `INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, value_number, scope_kind, scope_id, source_class, source_ref, status, created_at, updated_at)
		VALUES ('c5', 'api', 'is', 'string', 'x', 5, 'project', 'p', 'human', 'r5', 'asserted', ?, ?)`, now, now)
	insert("legacy document without source", `INSERT INTO knowledge_documents (id, subject, scope_kind, scope_id, content_digest, content_handle, provenance, status, created_at, updated_at)
		VALUES ('d1', 'doc', 'global', '', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'h', 'legacy_curated_document', 'active', ?, ?)`, now, now)
	insert("tombstone short digest", `INSERT INTO knowledge_tombstones (subject_digest, scope_kind, scope_id, forgotten_at, source_ref)
		VALUES ('abcd', 'global', '', ?, 'r')`, now)
	insert("preference reference value", `INSERT INTO knowledge_preferences (owner_key, key, value_kind, value_text, status, source_ref, created_at, updated_at)
		VALUES ('o', 'k', 'reference', 'x', 'active', 'r', ?, ?)`, now, now)

	valid, err := db.ExecContext(t.Context(), `INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, created_at, updated_at)
		VALUES ('c6', 'api', 'is', 'string', 'x', 'project', 'p', 'human', 'r6', 'asserted', ?, ?)`, now, now)
	if err != nil || valid == nil {
		t.Fatalf("valid claim insert failed: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO knowledge_claim_revisions (claim_id, revision_number, subject, predicate, value_kind, value_text, value_number, value_boolean, value_reference, status, source_class, operation, change_reason, source_ref, created_at)
		VALUES ('c6', 1, 'api', 'is', 'string', 'x', 0, 0, '', 'asserted', 'human', 'create', 'created', 'r6', ?)`, now); err != nil {
		t.Fatal(err)
	}
	insert("removed curator claim revision source", `INSERT INTO knowledge_claim_revisions (claim_id, revision_number, subject, predicate, value_kind, value_text, value_number, value_boolean, value_reference, status, source_class, operation, change_reason, source_ref, created_at)
		VALUES ('c6', 4, 'api', 'is', 'string', 'x', 0, 0, '', 'asserted', 'curator', 'create', 'created', 'r8', ?)`, now)
	insert("duplicate claim revision source", `INSERT INTO knowledge_claim_revisions (claim_id, revision_number, subject, predicate, value_kind, value_text, value_number, value_boolean, value_reference, status, source_class, operation, change_reason, source_ref, created_at)
		VALUES ('c6', 2, 'api', 'is', 'string', 'x', 0, 0, '', 'verified', 'human', 'transition', 'transition', 'r6', ?)`, now)
	insert("unknown revision operation", `INSERT INTO knowledge_claim_revisions (claim_id, revision_number, subject, predicate, value_kind, value_text, value_number, value_boolean, value_reference, status, source_class, operation, change_reason, source_ref, created_at)
		VALUES ('c6', 3, 'api', 'is', 'string', 'x', 0, 0, '', 'verified', 'human', 'invented', 'transition', 'r7', ?)`, now)
	insert("preference revision without status", `INSERT INTO knowledge_preference_revisions (preference_id, revision_number, value_kind, value_text, source_ref, created_at)
		VALUES (1, 1, 'string', 'x', 'r7', ?)`, now)
	var revisionID int
	if err := db.QueryRowContext(t.Context(), `SELECT id FROM knowledge_claim_revisions WHERE claim_id = 'c6'`).Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO knowledge_evidence (claim_revision, conversation_key, exchange_ts, author_id, kind)
		VALUES (?, 'slack:T1:dm:D1', '1723543200.123456', 'U1', 'source')`, revisionID); err != nil {
		t.Fatal(err)
	}
	insert("conflicting evidence provenance for exchange", `INSERT INTO knowledge_evidence (claim_revision, conversation_key, exchange_ts, author_id, kind)
		VALUES (?, 'slack:T1:dm:D1', '1723543200.123456', 'U2', 'source')`, revisionID)
	if _, err := db.ExecContext(t.Context(), `UPDATE knowledge_claims SET subject = 'other' WHERE id = 'c6'`); err == nil {
		t.Error("claim identity mutation unexpectedly succeeded")
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE knowledge_claim_revisions SET change_reason = 'mutated' WHERE claim_id = 'c6'`); err == nil {
		t.Error("claim revision mutation unexpectedly succeeded")
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO knowledge_documents (id, subject, scope_kind, scope_id, content_digest, content_handle, provenance, status, created_at, updated_at)
		VALUES ('d2', 'doc', 'project', 'p', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 'h', 'curated', 'active', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO knowledge_document_receipts (document_id, subject, scope_kind, scope_id, content_digest, content_handle, provenance, status, created_at)
		VALUES ('d2', 'doc', 'project', 'p', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 'h', 'curated', 'active', ?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE knowledge_document_receipts SET status = 'archived' WHERE document_id = 'd2'`); err == nil {
		t.Error("document receipt mutation unexpectedly succeeded")
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO knowledge_document_revisions (document_id, revision_number, status, source_ref, created_at)
		VALUES ('d2', 2, 'archived', 'slack-human:evt-a', ?)`, now); err != nil {
		t.Fatal(err)
	}
	insert("duplicate document revision number", `INSERT INTO knowledge_document_revisions (document_id, revision_number, status, source_ref, created_at)
		VALUES ('d2', 2, 'archived', 'slack-human:evt-b', ?)`, now)
	insert("document revision without source", `INSERT INTO knowledge_document_revisions (document_id, revision_number, status, source_ref, created_at)
		VALUES ('d2', 3, 'archived', '', ?)`, now)
	insert("unknown document revision status", `INSERT INTO knowledge_document_revisions (document_id, revision_number, status, source_ref, created_at)
		VALUES ('d2', 4, 'deleted', 'slack-human:evt-c', ?)`, now)
	if _, err := db.ExecContext(t.Context(), `UPDATE knowledge_document_revisions SET status = 'active' WHERE document_id = 'd2'`); err == nil {
		t.Error("document revision mutation unexpectedly succeeded")
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO knowledge_command_receipts (source_ref, action, payload_digest, target, created_at)
		VALUES ('slack-human:evt-r', 'remember', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 'claim:api', ?)`, now); err != nil {
		t.Fatal(err)
	}
	insert("duplicate command receipt source", `INSERT INTO knowledge_command_receipts (source_ref, action, payload_digest, target, created_at)
		VALUES ('slack-human:evt-r', 'remember', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 'claim:db', ?)`, now)
	insert("unknown command receipt action", `INSERT INTO knowledge_command_receipts (source_ref, action, payload_digest, target, created_at)
		VALUES ('slack-human:evt-s', 'inspect', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 'claim:api', ?)`, now)
	insert("uppercase command receipt digest", `INSERT INTO knowledge_command_receipts (source_ref, action, payload_digest, target, created_at)
		VALUES ('slack-human:evt-t', 'forget', 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB', 'subject:api', ?)`, now)
	insert("empty command receipt target", `INSERT INTO knowledge_command_receipts (source_ref, action, payload_digest, target, created_at)
		VALUES ('slack-human:evt-u', 'forget', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', '', ?)`, now)
	if _, err := db.ExecContext(t.Context(), `UPDATE knowledge_command_receipts SET target = 'mutated'`); err == nil {
		t.Error("command receipt mutation unexpectedly succeeded")
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM knowledge_command_receipts WHERE source_ref = 'slack-human:evt-r'`); err == nil {
		t.Error("command receipt deletion unexpectedly succeeded")
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE knowledge_documents SET current_rev = 0 WHERE id = 'd2'`); err == nil {
		t.Error("zero document revision unexpectedly accepted")
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM knowledge_documents WHERE id = 'd2'`); err != nil {
		t.Fatalf("forget-style document delete must cascade receipts: %v", err)
	}
	var receiptRows int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_document_receipts WHERE document_id = 'd2'`).Scan(&receiptRows); err != nil || receiptRows != 0 {
		t.Fatalf("receipt rows after cascade delete = %d, %v", receiptRows, err)
	}
	var revisionRows int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_document_revisions WHERE document_id = 'd2'`).Scan(&revisionRows); err != nil || revisionRows != 0 {
		t.Fatalf("document revision rows after cascade delete = %d, %v", revisionRows, err)
	}
}

func TestMigrationV38TombstonesAndRevisionsAreImmutable(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/immutable.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := store.DB()
	now := time.Now().UTC().UnixNano()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO knowledge_tombstones (subject_digest, scope_kind, scope_id, forgotten_at, source_ref)
		VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'global', '', ?, 'r')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE knowledge_tombstones SET source_ref = 'mutated' WHERE id = 1`); err == nil {
		t.Error("tombstone update unexpectedly succeeded")
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM knowledge_tombstones WHERE id = 1`); err == nil {
		t.Error("tombstone delete unexpectedly succeeded")
	}
}
