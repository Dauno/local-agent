package sqlite

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestMigrationV42RemovesLegacyProvenanceFromSchemaAndQueuesProjection(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 41)
	now := time.Now().UTC().UnixNano()
	digest := strings.Repeat("a", 64)
	protectedContent := "protected curated content"
	protectedDigestSum := sha256.Sum256([]byte(protectedContent))
	protectedDigest := hex.EncodeToString(protectedDigestSum[:])
	protectedResultID := strings.Repeat("b", 64)
	userContent := "protected curated content for a user scope"
	userDigestSum := sha256.Sum256([]byte(userContent))
	userDigest := hex.EncodeToString(userDigestSum[:])
	userResultID := strings.Repeat("c", 64)
	userMismatchResultID := strings.Repeat("d", 64)
	if _, err := raw.ExecContext(t.Context(), `
		INSERT INTO result_records
			(result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key, sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state)
		VALUES
			(?, 'tool_operation', 'producer', 1, 'artifact', 'result-key', ?, ?, 'text/markdown', 'U12345678', 'T12345678', 'slack:T12345678:dm:D12345678', 'project-a', 'conversation', ?, 'available'),
			(?, 'tool_operation', 'producer-user', 1, 'artifact', 'result-key-user', ?, ?, 'text/markdown', 'U12345678', 'T12345678', 'slack:T12345678:dm:D12345678', 'project-a', 'conversation', ?, 'available'),
			(?, 'tool_operation', 'producer-mismatch', 1, 'artifact', 'result-key-mismatch', ?, ?, 'text/markdown', 'U99999999', 'T12345678', 'slack:T12345678:dm:D99999999', 'project-a', 'conversation', ?, 'available')`,
		protectedResultID, protectedDigest, len(protectedContent), now,
		userResultID, userDigest, len(userContent), now,
		userMismatchResultID, userDigest, len(userContent), now); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), `
		INSERT INTO knowledge_documents
			(id, subject, scope_kind, scope_id, content_digest, content_handle, source_id, source_rev, provenance, status, current_rev, created_at, updated_at)
		VALUES ('legacy-doc', 'legacy', 'global', '', ?, 'memory_topics:old:revision:1', 'old', 1, 'legacy_curated_document', 'active', 1, ?, ?),
			('curated-doc', 'curated', 'global', '', ?, 'result:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', '', 0, 'curated', 'active', 1, ?, ?),
			('curated-protected', 'protected', 'team', 'T12345678', ?, 'result:' || ?, '', 0, 'curated', 'active', 1, ?, ?),
			('curated-user', 'user-scoped', 'user', 'slack:T12345678:user:U12345678', ?, 'result:' || ?, '', 0, 'curated', 'active', 1, ?, ?),
			('curated-user-mismatch', 'user-scoped-mismatch', 'user', 'U12345678', ?, 'result:' || ?, '', 0, 'curated', 'active', 1, ?, ?)`,
		digest, now, now,
		digest, now, now,
		protectedDigest, protectedResultID, now, now,
		userDigest, userResultID, now, now,
		userDigest, userMismatchResultID, now, now); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	db := store.DB()

	// The legacy-provenance document, the global-scope curated document with
	// no matching result_records row, and the curated document whose scope_id
	// is a bare Slack user ID (not the "slack:{team}:user:{user}" owner key
	// SlackOwnerKey produces) are all unresolvable: none of them survive.
	// Only documents whose result_records row genuinely backs them, under a
	// scope with a safe authorization rule, remain.
	var documents int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_documents`).Scan(&documents); err != nil || documents != 2 {
		t.Fatalf("document count = %d, %v; want two surviving curated documents", documents, err)
	}
	survivingIDs := map[string]bool{}
	rows, err := db.QueryContext(t.Context(), `SELECT id FROM knowledge_documents`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		survivingIDs[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !survivingIDs["curated-protected"] || !survivingIDs["curated-user"] {
		t.Fatalf("surviving documents = %v; want curated-protected and curated-user", survivingIDs)
	}

	// The surviving documents' results must now hold a live reference so a
	// retention sweep never reclaims them out from under an active document.
	var refState string
	if err := db.QueryRowContext(t.Context(), `
		SELECT state FROM result_references WHERE result_id = ? AND owner_kind = 'knowledge_document' AND owner_id = 'curated-protected'`,
		protectedResultID).Scan(&refState); err != nil || refState != "live" {
		t.Fatalf("backfilled reference state = %q, %v; want live", refState, err)
	}
	if err := db.QueryRowContext(t.Context(), `
		SELECT state FROM result_references WHERE result_id = ? AND owner_kind = 'knowledge_document' AND owner_id = 'curated-user'`,
		userResultID).Scan(&refState); err != nil || refState != "live" {
		t.Fatalf("backfilled user-scope reference state = %q, %v; want live", refState, err)
	}

	// Two invalidations are expected: one for the legacy-provenance rebuild,
	// and one for the helper removing the unresolvable global-scope curated
	// document. A projection worker that skips the second one would keep
	// serving deleted content from a stale OKF projection.
	var pending int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_projection_outbox WHERE status = 'pending'`).Scan(&pending); err != nil || pending != 2 {
		t.Fatalf("pending projection count = %d, %v; want two migration invalidations", pending, err)
	}
	var schemaSQL string
	if err := db.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = 'knowledge_documents'`).Scan(&schemaSQL); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(schemaSQL, "legacy_curated_document") {
		t.Fatalf("v42 knowledge_documents schema retains legacy provenance: %s", schemaSQL)
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO knowledge_documents
			(id, subject, scope_kind, scope_id, content_digest, content_handle, source_id, source_rev, provenance, status, current_rev, created_at, updated_at)
		VALUES ('rejected', 'rejected', 'global', '', ?, 'memory_topics:x:revision:1', 'x', 1, 'legacy_curated_document', 'active', 1, ?, ?)`, digest, now, now); err == nil {
		t.Fatal("v42 accepted legacy document provenance")
	}
}
