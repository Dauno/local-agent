package sqlite

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const knowledgeImportTopicInsert = `INSERT INTO memory_topics (id, slug, title, description, status, tags, bundle_path, owner_key, content, current_rev, created_at, updated_at)
	VALUES (?, ?, ?, '', ?, '[]', ?, ?, ?, ?, 1, 1)`

func seedKnowledgeImportTopic(t *testing.T, store *Store, id, slug, title, status, bundlePath, ownerKey, content string, rev int) {
	t.Helper()
	if _, err := store.DB().ExecContext(t.Context(), knowledgeImportTopicInsert, id, slug, title, status, bundlePath, ownerKey, content, rev); err != nil {
		t.Fatal(err)
	}
	// Legacy revisions are append-only rows; the current revision must
	// exist and hold the current content, mirroring the legacy store.
	if rev >= 1 {
		if _, err := store.DB().ExecContext(t.Context(), `
			INSERT INTO memory_topic_revisions (topic_id, revision_number, content, change_reason, created_at)
			VALUES (?, ?, ?, '', 1)`, id, rev, content); err != nil {
			t.Fatal(err)
		}
	}
}

type knowledgeImportLegacyRow struct {
	id, slug, title, description, status, tags, bundlePath, ownerKey, content string
	currentRev, createdAt, updatedAt                                          int64
}

func knowledgeImportLegacyRows(t *testing.T, store *Store) []knowledgeImportLegacyRow {
	t.Helper()
	rows, err := store.DB().QueryContext(t.Context(), `SELECT id, slug, title, description, status, tags, bundle_path, owner_key, content, current_rev, created_at, updated_at FROM memory_topics ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []knowledgeImportLegacyRow
	for rows.Next() {
		var row knowledgeImportLegacyRow
		if err := rows.Scan(&row.id, &row.slug, &row.title, &row.description, &row.status, &row.tags, &row.bundlePath, &row.ownerKey, &row.content, &row.currentRev, &row.createdAt, &row.updatedAt); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func knowledgeImportDocument(t *testing.T, store *Store, sourceID string) domain.KnowledgeDocument {
	t.Helper()
	doc, err := scanKnowledgeDocument(store.DB().QueryRowContext(t.Context(), `
		SELECT `+knowledgeDocumentColumns+` FROM knowledge_documents WHERE source_id = ?`, sourceID))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func knowledgeImportCount(t *testing.T, store *Store, query string) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRowContext(t.Context(), query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestKnowledgeLegacyImportCreatesOneDocumentPerTopic(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_person", "person-dauno-slack-t12345678-user-u12345678", "Dauno", "active", "people", "slack:T12345678:user:U12345678", "dauno is the creator", 3)
	seedKnowledgeImportTopic(t, store, "mem_global", "durable-fact", "Durable fact", "active", "topics", "", "the fact is durable", 2)

	result, err := knowledge.ImportLegacyTopics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 2 || result.Skipped != 0 || result.Archived != 0 {
		t.Fatalf("import result = %+v, want 2 imported", result)
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_documents`) != 2 {
		t.Fatal("one document per topic was not created")
	}

	person := knowledgeImportDocument(t, store, "mem_person")
	if person.ScopeKind != domain.KnowledgeScopeUser || person.ScopeID != "slack:T12345678:user:U12345678" {
		t.Fatalf("person document scope = %s:%q, want user bound to the owner", person.ScopeKind, person.ScopeID)
	}
	if person.ContentDigest != sha256Hex("dauno is the creator") {
		t.Fatalf("person digest = %q, want sha256 of the current content", person.ContentDigest)
	}
	if person.SourceRev != 3 || person.Provenance != domain.KnowledgeProvenanceLegacyCurated || person.Status != domain.KnowledgeDocumentActive {
		t.Fatalf("person identity = rev %d provenance %q status %q", person.SourceRev, person.Provenance, person.Status)
	}
	if !domain.ValidKnowledgeDocumentID(person.ID) || person.ID != domain.LegacyTopicDocumentID(domain.TopicID("mem_person")) {
		t.Fatalf("person document id %q is not the deterministic legacy identity", person.ID)
	}

	global := knowledgeImportDocument(t, store, "mem_global")
	if global.ScopeKind != domain.KnowledgeScopeGlobal || global.ScopeID != "" {
		t.Fatalf("non-person document scope = %s:%q, want global with empty identity", global.ScopeKind, global.ScopeID)
	}
	if global.ContentDigest != sha256Hex("the fact is durable") || global.SourceRev != 2 {
		t.Fatalf("global document digest/rev = %q/%d", global.ContentDigest, global.SourceRev)
	}
	// The content handle references the immutable revision row the digest
	// was computed from, never the mutable topic row.
	var revisionID int64
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT id FROM memory_topic_revisions WHERE topic_id = 'mem_global' AND revision_number = 2`).Scan(&revisionID); err != nil {
		t.Fatal(err)
	}
	if global.ContentHandle != domain.LegacyTopicRevisionHandle(domain.TopicID("mem_global"), revisionID) {
		t.Fatalf("content handle %q does not reference the immutable revision %d", global.ContentHandle, revisionID)
	}

	for _, sourceID := range []string{"mem_person", "mem_global"} {
		if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_document_receipts WHERE document_id = (SELECT id FROM knowledge_documents WHERE source_id = '`+sourceID+`')`) != 1 {
			t.Fatalf("document receipt missing for %s", sourceID)
		}
	}
}

func TestKnowledgeLegacyImportReplayIsIdempotent(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_global", "durable-fact", "Durable fact", "active", "topics", "", "the fact is durable", 2)

	first, err := knowledge.ImportLegacyTopics(t.Context())
	if err != nil || first.Imported != 1 {
		t.Fatalf("first import = %+v, %v", first, err)
	}
	second, err := knowledge.ImportLegacyTopics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second.Imported != 0 || second.Skipped != 1 {
		t.Fatalf("replay result = %+v, want 0 imported and 1 skipped", second)
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_documents`) != 1 {
		t.Fatal("replay created duplicate documents")
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_document_receipts`) != 1 {
		t.Fatal("replay created duplicate receipts")
	}
}

func TestKnowledgeLegacyImportIgnoresPostImportRevisionAndImportsNewTopics(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_global", "durable-fact", "Durable fact", "active", "topics", "", "the fact is durable", 2)
	if _, err := knowledge.ImportLegacyTopics(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := knowledgeImportDocument(t, store, "mem_global")

	// A normal legacy curator revision updates content and current_rev and
	// appends an immutable revision row.
	if _, err := store.DB().ExecContext(t.Context(), `
		UPDATE memory_topics SET content = 'the fact is durable and updated', current_rev = 3, updated_at = 2 WHERE id = 'mem_global'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO memory_topic_revisions (topic_id, revision_number, content, change_reason, created_at)
		VALUES ('mem_global', 3, 'the fact is durable and updated', '', 2)`); err != nil {
		t.Fatal(err)
	}
	// And a new topic appears before the next startup.
	seedKnowledgeImportTopic(t, store, "mem_new", "new-fact", "New fact", "active", "topics", "", "a new fact", 1)

	result, err := knowledge.ImportLegacyTopics(t.Context())
	if err != nil {
		t.Fatalf("post-import legacy revision broke replay: %v", err)
	}
	if result.Imported != 1 || result.Skipped != 1 {
		t.Fatalf("replay result = %+v, want the revised topic skipped and the new topic imported", result)
	}
	after := knowledgeImportDocument(t, store, "mem_global")
	if after.ContentDigest != before.ContentDigest || after.SourceRev != before.SourceRev || after.ID != before.ID || after.ContentHandle != before.ContentHandle {
		t.Fatalf("imported document changed on replay:\nbefore: %+v\nafter:  %+v", before, after)
	}
	// The handle still references the original immutable revision, whose
	// bytes still match the imported digest.
	var revisionContent string
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT content FROM memory_topic_revisions WHERE id = ?`,
		strings.TrimPrefix(after.ContentHandle, "memory_topics:"+after.SourceID+":revision:")).Scan(&revisionContent); err != nil {
		t.Fatal(err)
	}
	if sha256Hex(revisionContent) != after.ContentDigest {
		t.Fatal("referenced revision bytes no longer match the imported digest")
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_documents`) != 2 {
		t.Fatal("new topic was not imported alongside the revised one")
	}
}

func TestKnowledgeLegacyImportMirrorsPostImportLegacyArchive(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_global", "durable-fact", "Durable fact", "active", "topics", "", "the fact is durable", 2)
	if _, err := knowledge.ImportLegacyTopics(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Drain the initial import trigger so the mirror is observed in
	// isolation.
	batch, err := knowledge.ClaimProjectionBatch(t.Context())
	if err != nil || len(batch) != 1 {
		t.Fatalf("claimed batch = %d rows, %v", len(batch), err)
	}
	if err := knowledge.CompleteProjectionBatch(t.Context(), []int{batch[0].ID}, batch[0].LeaseUntil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE memory_topics SET status = 'archived', updated_at = 2 WHERE id = 'mem_global'`); err != nil {
		t.Fatal(err)
	}
	result, err := knowledge.ImportLegacyTopics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Archived != 1 || result.Imported != 0 {
		t.Fatalf("mirror result = %+v, want 1 archived", result)
	}
	doc := knowledgeImportDocument(t, store, "mem_global")
	if doc.Status != domain.KnowledgeDocumentArchived || doc.Revision != 2 {
		t.Fatalf("mirrored document = status %q rev %d, want archived at revision 2", doc.Status, doc.Revision)
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_document_revisions WHERE document_id = '`+string(doc.ID)+`' AND source_ref = 'legacy_import:mem_global'`) != 1 {
		t.Fatal("mirror archive left no deterministic revision row")
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_projection_outbox WHERE status = 'pending'`) != 1 {
		t.Fatal("mirror archive enqueued no projection trigger")
	}
	// Replay of the mirror is idempotent.
	result, err = knowledge.ImportLegacyTopics(t.Context())
	if err != nil || result.Archived != 0 || result.Skipped != 1 {
		t.Fatalf("mirror replay = %+v, %v; want 0 archived and 1 skipped", result, err)
	}
}

func TestKnowledgeLegacyImportImportsArchivedTopicsAsArchived(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_active", "active", "Active", "active", "topics", "", "content", 1)
	seedKnowledgeImportTopic(t, store, "mem_archived", "archived", "Archived", "archived", "topics", "", "content", 1)

	result, err := knowledge.ImportLegacyTopics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 2 {
		t.Fatalf("import result = %+v, want every existing topic imported", result)
	}
	active := knowledgeImportDocument(t, store, "mem_active")
	if active.Status != domain.KnowledgeDocumentActive {
		t.Fatalf("active legacy topic imported as %q", active.Status)
	}
	archived := knowledgeImportDocument(t, store, "mem_archived")
	if archived.Status != domain.KnowledgeDocumentArchived {
		t.Fatalf("archived legacy topic imported as %q, want archived", archived.Status)
	}
}

func TestKnowledgeLegacyImportDisambiguatesDuplicateTitles(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_dup_a", "dup-a", "Shared title", "active", "topics", "", "content a", 1)
	seedKnowledgeImportTopic(t, store, "mem_dup_b", "dup-b", "Shared title", "active", "topics", "", "content b", 1)

	result, err := knowledge.ImportLegacyTopics(t.Context())
	if err != nil {
		t.Fatalf("duplicate valid titles must not fail the import: %v", err)
	}
	if result.Imported != 2 {
		t.Fatalf("import result = %+v, want both topics imported", result)
	}
	first := knowledgeImportDocument(t, store, "mem_dup_a")
	second := knowledgeImportDocument(t, store, "mem_dup_b")
	if first.Subject == second.Subject {
		t.Fatalf("duplicate titles were not disambiguated: %q vs %q", first.Subject, second.Subject)
	}
	// The subject is a pure function of the topic: readable title plus the
	// opaque topic-derived suffix, identical whether the topics arrive
	// together or separately.
	for _, doc := range []domain.KnowledgeDocument{first, second} {
		if doc.Subject != "Shared title "+domain.LegacyTopicDocumentSubjectSuffix(domain.TopicID(doc.SourceID)) {
			t.Fatalf("subject %q is not the pure per-topic disambiguation", doc.Subject)
		}
	}

	// Replay is stable: nothing changes.
	replay, err := knowledge.ImportLegacyTopics(t.Context())
	if err != nil || replay.Imported != 0 || replay.Skipped != 2 {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
}

func TestKnowledgeLegacyImportSubjectIsIndependentOfArrivalHistory(t *testing.T) {
	// Importing the same two duplicate-title topics separately must
	// produce the same subjects as importing them together: the subject is
	// a pure function of the topic.
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_dup_a", "dup-a", "Shared title", "active", "topics", "", "content a", 1)
	if _, err := knowledge.ImportLegacyTopics(t.Context()); err != nil {
		t.Fatal(err)
	}
	firstAlone := knowledgeImportDocument(t, store, "mem_dup_a")
	seedKnowledgeImportTopic(t, store, "mem_dup_b", "dup-b", "Shared title", "active", "topics", "", "content b", 1)
	if _, err := knowledge.ImportLegacyTopics(t.Context()); err != nil {
		t.Fatal(err)
	}
	secondLater := knowledgeImportDocument(t, store, "mem_dup_b")
	if firstAlone.Subject != "Shared title "+domain.LegacyTopicDocumentSubjectSuffix(domain.TopicID("mem_dup_a")) {
		t.Fatalf("first-imported subject %q is not the pure per-topic value", firstAlone.Subject)
	}
	if secondLater.Subject != "Shared title "+domain.LegacyTopicDocumentSubjectSuffix(domain.TopicID("mem_dup_b")) {
		t.Fatalf("later-imported subject %q is not the pure per-topic value", secondLater.Subject)
	}
}

func TestKnowledgeLegacyImportDisambiguatesAgainstCuratedDocuments(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_shared", "shared", "Shared subject", "active", "topics", "", "content", 1)
	if _, err := knowledge.CreateDocument(t.Context(), domain.KnowledgeDocument{
		Subject: "Shared subject", ScopeKind: domain.KnowledgeScopeGlobal,
		ContentDigest: sha256Hex("curated content"), ContentHandle: "memory_topics:mem_curated",
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}

	result, err := knowledge.ImportLegacyTopics(t.Context())
	if err != nil {
		t.Fatalf("a curated document sharing the title must not fail the import: %v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("import result = %+v, want the legacy topic imported", result)
	}
	legacy := knowledgeImportDocument(t, store, "mem_shared")
	if legacy.Subject != "Shared subject "+domain.LegacyTopicDocumentSubjectSuffix(domain.TopicID("mem_shared")) {
		t.Fatalf("legacy subject %q is not the pure per-topic value", legacy.Subject)
	}
}

func TestKnowledgeLegacyImportDisambiguationCollisionFailsClosed(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_shared", "shared", "Shared subject", "active", "topics", "", "content", 1)
	suffix := domain.LegacyTopicDocumentSubjectSuffix(domain.TopicID("mem_shared"))
	if _, err := knowledge.CreateDocument(t.Context(), domain.KnowledgeDocument{
		Subject: "Shared subject " + suffix, ScopeKind: domain.KnowledgeScopeGlobal,
		ContentDigest: sha256Hex("curated content"), ContentHandle: "memory_topics:mem_curated",
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatal(err)
	}

	_, err := knowledge.ImportLegacyTopics(t.Context())
	if err == nil {
		t.Fatal("suffixed identity occupied by another document must fail closed")
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_documents WHERE provenance = 'legacy_curated_document'`) != 0 {
		t.Fatal("failed import left partial legacy documents")
	}
}

func TestKnowledgeLegacyImportLeavesLegacyRowsIntact(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_global", "durable-fact", "Durable fact", "active", "topics", "", "the fact is durable", 2)
	seedKnowledgeImportTopic(t, store, "mem_person", "person-dauno-slack-t12345678-user-u12345678", "Dauno", "active", "people", "slack:T12345678:user:U12345678", "dauno is the creator", 3)
	before := knowledgeImportLegacyRows(t, store)

	if _, err := knowledge.ImportLegacyTopics(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := knowledgeImportLegacyRows(t, store)
	if len(before) != len(after) {
		t.Fatalf("legacy row count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("legacy row %d changed:\nbefore: %+v\nafter:  %+v", i, before[i], after[i])
		}
	}
}

func TestKnowledgeLegacyImportFailsClosedWithoutPartialImport(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_a_valid", "valid", "Valid", "active", "topics", "", "content", 1)
	seedKnowledgeImportTopic(t, store, "mem_b_malformed", "person-malformed", "Malformed owner", "active", "people", "garbage-owner", "content", 1)
	// Simulate pre-v9 legacy data: a people topic that never received an
	// owner (the v9 insert trigger forbids creating one directly, but old
	// rows could predate it).
	seedKnowledgeImportTopic(t, store, "mem_c_orphan", "person-orphan", "Orphan", "active", "topics", "", "content", 1)
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE memory_topics SET bundle_path = 'people' WHERE id = 'mem_c_orphan'`); err != nil {
		t.Fatal(err)
	}

	_, err := knowledge.ImportLegacyTopics(t.Context())
	if err == nil {
		t.Fatal("malformed person topics must fail the import closed")
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_documents`) != 0 {
		t.Fatal("failed import left partial documents")
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_projection_outbox`) != 0 {
		t.Fatal("failed import left outbox rows")
	}
}

func TestKnowledgeLegacyImportInvalidRevisionFailsClosed(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_zerorev", "zero-rev", "Zero rev", "active", "topics", "", "content", 0)

	_, err := knowledge.ImportLegacyTopics(t.Context())
	if err == nil {
		t.Fatal("a topic without an original revision must fail the import closed")
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_documents`) != 0 {
		t.Fatal("failed import left partial documents")
	}
}

func TestKnowledgeLegacyImportFailureSurvivesReopen(t *testing.T) {
	database := t.TempDir() + "/knowledge.db"
	store, err := Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	knowledge := NewKnowledgeStore(store)
	seedKnowledgeImportTopic(t, store, "mem_a_valid", "valid", "Valid", "active", "topics", "", "content", 1)
	seedKnowledgeImportTopic(t, store, "mem_b_orphan", "person-orphan", "Orphan", "active", "topics", "", "content", 1)
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE memory_topics SET bundle_path = 'people' WHERE id = 'mem_b_orphan'`); err != nil {
		t.Fatal(err)
	}
	if _, err := knowledge.ImportLegacyTopics(t.Context()); err == nil {
		t.Fatal("invalid legacy data must fail the import")
	}
	store.Close()

	reopened, err := Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if knowledgeImportCount(t, reopened, `SELECT COUNT(*) FROM knowledge_documents`) != 0 {
		t.Fatal("failed import persisted partial state across reopen")
	}
	// Once the legacy data is repaired, the next startup imports.
	if _, err := reopened.DB().ExecContext(t.Context(), `UPDATE memory_topics SET bundle_path = 'topics' WHERE id = 'mem_b_orphan'`); err != nil {
		t.Fatal(err)
	}
	result, err := NewKnowledgeStore(reopened).ImportLegacyTopics(t.Context())
	if err != nil || result.Imported != 2 {
		t.Fatalf("recovered import = %+v, %v", result, err)
	}
}

func TestKnowledgeLegacyImportEnqueuesProjectionOnce(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_global", "durable-fact", "Durable fact", "active", "topics", "", "the fact is durable", 2)

	if _, err := knowledge.ImportLegacyTopics(t.Context()); err != nil {
		t.Fatal(err)
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_projection_outbox WHERE status = 'pending'`) != 1 {
		t.Fatal("import must enqueue exactly one durable projection trigger")
	}
	batch, err := knowledge.ClaimProjectionBatch(t.Context())
	if err != nil || len(batch) != 1 {
		t.Fatalf("claimed batch = %d rows, %v; want 1", len(batch), err)
	}
	if err := knowledge.CompleteProjectionBatch(t.Context(), []int{batch[0].ID}, batch[0].LeaseUntil); err != nil {
		t.Fatal(err)
	}
	// A replay that imports nothing must not enqueue again.
	if _, err := knowledge.ImportLegacyTopics(t.Context()); err != nil {
		t.Fatal(err)
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_projection_outbox WHERE status = 'pending'`) != 0 {
		t.Fatal("idempotent replay enqueued a projection trigger")
	}
}

func TestKnowledgeLegacyImportNoTopics(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	result, err := knowledge.ImportLegacyTopics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 0 || result.Skipped != 0 || result.Archived != 0 {
		t.Fatalf("empty import result = %+v", result)
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_projection_outbox`) != 0 {
		t.Fatal("empty import enqueued a projection trigger")
	}
}

func TestKnowledgeLegacyImportSkipsTombstonedSubjects(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_forgotten", "forgotten", "Forgotten subject", "active", "topics", "", "content", 1)
	subject := "Forgotten subject " + domain.LegacyTopicDocumentSubjectSuffix(domain.TopicID("mem_forgotten"))
	if _, err := knowledge.ForgetSubject(t.Context(), subject, domain.KnowledgeScopeGlobal, "", "test-source"); err != nil {
		t.Fatal(err)
	}
	result, err := knowledge.ImportLegacyTopics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 0 || result.Skipped != 1 {
		t.Fatalf("import result = %+v, want the tombstoned subject skipped", result)
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_documents`) != 0 {
		t.Fatal("forgotten subject was resurrected")
	}
}

func TestKnowledgeLegacyImportRejectsForeignOccupant(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_global", "durable-fact", "Durable fact", "active", "topics", "", "the fact is durable", 2)
	// A foreign document occupies the deterministic identity with curated
	// provenance: replay must fail closed, never skip or mutate it.
	document := domain.KnowledgeDocument{
		ID:            domain.LegacyTopicDocumentID(domain.TopicID("mem_global")),
		Subject:       "Durable fact " + domain.LegacyTopicDocumentSubjectSuffix(domain.TopicID("mem_global")),
		ScopeKind:     domain.KnowledgeScopeGlobal,
		ContentDigest: sha256Hex("foreign content"),
		ContentHandle: "memory_topics:mem_foreign",
		Provenance:    domain.KnowledgeProvenanceCurated,
		Status:        domain.KnowledgeDocumentActive,
	}
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_documents (`+knowledgeDocumentColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		string(document.ID), document.Subject, string(document.ScopeKind), document.ScopeID,
		document.ContentDigest, document.ContentHandle, document.SourceID, document.SourceRev,
		string(document.Provenance), string(document.Status), 1, 1); err != nil {
		t.Fatal(err)
	}

	_, err := knowledge.ImportLegacyTopics(t.Context())
	if !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("foreign occupant error = %v, want ErrKnowledgeCASConflict", err)
	}
	// The foreign occupant was neither skipped silently nor archived.
	var status string
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT status FROM knowledge_documents WHERE id = ?`, string(document.ID)).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.KnowledgeDocumentActive) {
		t.Fatalf("foreign occupant mutated to %q", status)
	}
}

func TestKnowledgeLegacyImportRejectsOccupantWithoutReceipt(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_global", "durable-fact", "Durable fact", "active", "topics", "", "the fact is durable", 2)
	if _, err := knowledge.ImportLegacyTopics(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Corrupt persisted state: the document lost its immutable receipt.
	if _, err := store.DB().ExecContext(t.Context(), `DELETE FROM knowledge_document_receipts`); err != nil {
		t.Fatal(err)
	}
	_, err := knowledge.ImportLegacyTopics(t.Context())
	if !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("missing receipt error = %v, want ErrKnowledgeCASConflict", err)
	}
}

func TestKnowledgeLegacyImportNeverResurrectsArchivedDocuments(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	seedKnowledgeImportTopic(t, store, "mem_global", "durable-fact", "Durable fact", "active", "topics", "", "the fact is durable", 2)
	if _, err := knowledge.ImportLegacyTopics(t.Context()); err != nil {
		t.Fatal(err)
	}
	doc := knowledgeImportDocument(t, store, "mem_global")
	if _, err := knowledge.ArchiveDocument(t.Context(), doc.ID, 1, "test-source"); err != nil {
		t.Fatal(err)
	}

	result, err := knowledge.ImportLegacyTopics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 0 || result.Skipped != 1 {
		t.Fatalf("replay result = %+v, want the archived document skipped", result)
	}
	after := knowledgeImportDocument(t, store, "mem_global")
	if after.Status != domain.KnowledgeDocumentArchived {
		t.Fatalf("archived document status = %q, want archived preserved", after.Status)
	}
}

func TestKnowledgeLegacyImportNotBlockedByDocumentBudget(t *testing.T) {
	knowledge, store := newKnowledgeTestStore(t)
	// Well beyond the curated-document budget (default 256): the backfill
	// imports authoritative legacy state as-is and is never budget-blocked.
	const topicCount = 260
	for i := 0; i < topicCount; i++ {
		id := fmt.Sprintf("mem_%04d", i)
		seedKnowledgeImportTopic(t, store, id, "slug-"+id, "Topic "+id, "active", "topics", "", "content", 1)
	}

	result, err := knowledge.ImportLegacyTopics(t.Context())
	if err != nil {
		t.Fatalf("backfill blocked by the curated-document budget: %v", err)
	}
	if result.Imported != topicCount {
		t.Fatalf("import result = %+v, want all %d topics imported", result, topicCount)
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_documents`) != topicCount {
		t.Fatal("backfill did not import every topic")
	}
}

func TestKnowledgeLegacyImportConcurrentCallsNeverDuplicate(t *testing.T) {
	// Two independent store handles on the same database file exercise
	// real cross-connection locking, not a serialized single connection
	// pool.
	path := filepath.Join(t.TempDir(), "knowledge.db")
	store, err := Initialize(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	second, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for i := 0; i < 5; i++ {
		seedKnowledgeImportTopic(t, store, "mem_"+string(rune('a'+i)), "slug-"+string(rune('a'+i)), "Topic "+string(rune('a'+i)), "active", "topics", "", "content", 1)
	}
	var wg sync.WaitGroup
	results := make([]domain.KnowledgeLegacyImportResult, 2)
	errs := make([]error, 2)
	stores := []*KnowledgeStore{NewKnowledgeStore(store), NewKnowledgeStore(second)}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = stores[idx].ImportLegacyTopics(t.Context())
		}(i)
	}
	wg.Wait()
	// Concurrent runs never create duplicates or partial states: either
	// both converge (the loser skips everything) or the loser fails
	// closed with a conflict or lock error. The final state must always
	// be complete.
	for i, err := range errs {
		if err != nil && !errors.Is(err, port.ErrKnowledgeCASConflict) {
			t.Logf("concurrent import %d failed closed: %v", i, err)
		}
	}
	if errs[0] != nil && errs[1] != nil {
		t.Fatalf("both concurrent imports failed: %v / %v", errs[0], errs[1])
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_documents`) != 5 {
		t.Fatal("concurrent imports created duplicate or partial documents")
	}
	if knowledgeImportCount(t, store, `SELECT COUNT(*) FROM knowledge_document_receipts`) != 5 {
		t.Fatal("concurrent imports created partial receipts")
	}
	replay, err := NewKnowledgeStore(store).ImportLegacyTopics(t.Context())
	if err != nil || replay.Skipped != 5 || replay.Imported != 0 {
		t.Fatalf("replay after concurrency = %+v, %v", replay, err)
	}
}

// TestHelperImportCrashChild simulates a process crash mid-import: it opens
// a raw connection, writes partial import-style state inside a transaction,
// and dies via os.Exit without commit or close, exactly as a crashed
// process would drop the transaction.
func TestHelperImportCrashChild(t *testing.T) {
	if os.Getenv("KNOWLEDGE_IMPORT_CRASH_CHILD") != "1" {
		t.Skip("helper process")
	}
	path := os.Getenv("KNOWLEDGE_IMPORT_CRASH_DB")
	dsn, err := dataSourceName(path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	document := domain.KnowledgeDocument{
		ID:            domain.LegacyTopicDocumentID(domain.TopicID("mem_a")),
		Subject:       "A " + domain.LegacyTopicDocumentSubjectSuffix(domain.TopicID("mem_a")),
		ScopeKind:     domain.KnowledgeScopeGlobal,
		ContentDigest: sha256Hex("content"),
		ContentHandle: "memory_topics:mem_a:revision:1",
		SourceID:      "mem_a", SourceRev: 1,
		Provenance: domain.KnowledgeProvenanceLegacyCurated,
		Status:     domain.KnowledgeDocumentActive,
	}
	if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO knowledge_documents (`+knowledgeDocumentColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 1, 1)`,
		string(document.ID), document.Subject, string(document.ScopeKind), document.ScopeID,
		document.ContentDigest, document.ContentHandle, document.SourceID, document.SourceRev,
		string(document.Provenance), string(document.Status)); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestKnowledgeLegacyImportCrashMidTransactionLeavesNoPartialState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knowledge.db")
	store, err := Initialize(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	seedKnowledgeImportTopic(t, store, "mem_a", "a", "A", "active", "topics", "", "content", 1)
	seedKnowledgeImportTopic(t, store, "mem_b", "b", "B", "active", "topics", "", "content", 1)
	store.Close()

	// A real child process dies mid-import with an uncommitted partial
	// document, leaving the journal behind like a crashed process would.
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperImportCrashChild")
	cmd.Env = append(os.Environ(), "KNOWLEDGE_IMPORT_CRASH_CHILD=1", "KNOWLEDGE_IMPORT_CRASH_DB="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash child failed: %v\n%s", err, out)
	}

	// The next process restart opens the database: the journal rollback
	// restores atomicity, no partial state survives the crash, and the
	// real import then succeeds in full.
	reopened, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if knowledgeImportCount(t, reopened, `SELECT COUNT(*) FROM knowledge_documents`) != 0 {
		t.Fatal("crashed import persisted a partial document")
	}
	if knowledgeImportCount(t, reopened, `SELECT COUNT(*) FROM knowledge_document_receipts`) != 0 {
		t.Fatal("crashed import persisted a partial receipt")
	}
	result, err := NewKnowledgeStore(reopened).ImportLegacyTopics(t.Context())
	if err != nil || result.Imported != 2 {
		t.Fatalf("import after crash = %+v, %v", result, err)
	}
}

func TestKnowledgeLegacyImportAfterUpgrade(t *testing.T) {
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

	result, err := NewKnowledgeStore(upgraded).ImportLegacyTopics(t.Context())
	if err != nil || result.Imported != 1 {
		t.Fatalf("import after upgrade = %+v, %v", result, err)
	}
	doc := knowledgeImportDocument(t, upgraded, "mem_legacy1")
	if doc.Subject != "Legacy "+domain.LegacyTopicDocumentSubjectSuffix(domain.TopicID("mem_legacy1")) || doc.SourceRev != 2 || doc.Provenance != domain.KnowledgeProvenanceLegacyCurated {
		t.Fatalf("upgraded import document = %+v", doc)
	}
}

func TestKnowledgeLegacyImportRejectsUnavailableStore(t *testing.T) {
	knowledge := NewKnowledgeStore(nil)
	_, err := knowledge.ImportLegacyTopics(t.Context())
	if !errors.Is(err, port.ErrKnowledgeUnavailable) {
		t.Fatalf("unavailable store error = %v, want ErrKnowledgeUnavailable", err)
	}
}
