package sqlite

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// seedGuardedTopic creates one legacy topic with its revision-1 row and
// returns the immutable revision row ID referenced by imported document
// handles.
func seedGuardedTopic(t *testing.T, store *Store, slug, title string) (domain.Topic, int64) {
	t.Helper()
	topic, err := store.CreateTopic(t.Context(), slug, title, "desc", nil, "content", "init")
	if err != nil {
		t.Fatalf("CreateTopic() error = %v", err)
	}
	var revisionID int64
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT id FROM memory_topic_revisions WHERE topic_id = ? AND revision_number = 1`,
		string(topic.ID)).Scan(&revisionID); err != nil {
		t.Fatalf("revision lookup: %v", err)
	}
	return topic, revisionID
}

// seedReferencingDocument inserts one knowledge document whose content
// handle references the given immutable revision row, mirroring the
// deterministic legacy import shape.
func seedReferencingDocument(t *testing.T, store *Store, subject string, topicID domain.TopicID, revisionID int64) {
	t.Helper()
	digest := sha256.Sum256([]byte("content"))
	now := time.Now().UTC().UnixNano()
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_documents (id, subject, scope_kind, scope_id, content_digest, content_handle,
			source_id, source_rev, provenance, status, current_rev, created_at, updated_at)
		VALUES (?, ?, 'global', '', ?, ?, ?, 1, 'legacy_curated_document', 'active', 1, ?, ?)`,
		"kdoc_"+hex.EncodeToString(digest[:8]), subject, hex.EncodeToString(digest[:]),
		domain.LegacyTopicRevisionHandle(topicID, revisionID), string(topicID), now, now); err != nil {
		t.Fatalf("insert referencing document: %v", err)
	}
}

func TestKnowledgeHandleGuardRejectsReferencedRevisionMutationAndDelete(t *testing.T) {
	store, _ := newTestStore(t)
	topic, revisionID := seedGuardedTopic(t, store, "guarded", "Guarded")
	seedReferencingDocument(t, store, "Guarded document", topic.ID, revisionID)

	if _, err := store.DB().ExecContext(t.Context(), `
		UPDATE memory_topic_revisions SET content = 'mutated' WHERE id = ?`, revisionID); err == nil {
		t.Fatal("referenced revision update unexpectedly succeeded")
	}
	var content string
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT content FROM memory_topic_revisions WHERE id = ?`, revisionID).Scan(&content); err != nil || content != "content" {
		t.Fatalf("referenced revision content after rejected update = %q, %v", content, err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `
		UPDATE memory_topic_revisions SET change_reason = 'mutated' WHERE id = ?`, revisionID); err == nil {
		t.Fatal("referenced revision change_reason update unexpectedly succeeded")
	}
	if _, err := store.DB().ExecContext(t.Context(), `
		DELETE FROM memory_topic_revisions WHERE id = ?`, revisionID); err == nil {
		t.Fatal("direct delete of referenced revision unexpectedly succeeded")
	}
	var present int
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM memory_topic_revisions WHERE id = ?`, revisionID).Scan(&present); err != nil || present != 1 {
		t.Fatalf("referenced revision rows after rejected delete = %d, %v", present, err)
	}

	// An unreferenced revision keeps its prior mutable lifecycle.
	unreferenced, err := store.AddRevision(t.Context(), topic.ID, 1, "revised content", "update")
	if err != nil {
		t.Fatalf("AddRevision() error = %v", err)
	}
	var unreferencedID int64
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT id FROM memory_topic_revisions WHERE topic_id = ? AND revision_number = ?`,
		string(topic.ID), unreferenced.RevisionNumber).Scan(&unreferencedID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `
		UPDATE memory_topic_revisions SET change_reason = 'updated' WHERE id = ?`, unreferencedID); err != nil {
		t.Fatalf("unreferenced revision update rejected: %v", err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `
		DELETE FROM memory_topic_revisions WHERE id = ?`, unreferencedID); err != nil {
		t.Fatalf("unreferenced revision delete rejected: %v", err)
	}
}

func TestKnowledgeHandleGuardRejectsDeleteTopicCascadingReferencedRevision(t *testing.T) {
	store, _ := newTestStore(t)
	topic, revisionID := seedGuardedTopic(t, store, "guarded", "Guarded")
	seedReferencingDocument(t, store, "Guarded document", topic.ID, revisionID)

	var ftsRowid int64
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT rowid FROM memory_topics WHERE id = ?`, string(topic.ID)).Scan(&ftsRowid); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTopic(t.Context(), topic.ID); err == nil {
		t.Fatal("DeleteTopic with a referenced revision unexpectedly succeeded")
	}
	// The whole delete transaction must roll back: topic, revision, and FTS
	// rows all stay intact.
	var topics, revisions, fts int
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM memory_topics WHERE id = ?`, string(topic.ID)).Scan(&topics); err != nil || topics != 1 {
		t.Fatalf("topic rows after rejected DeleteTopic = %d, %v", topics, err)
	}
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM memory_topic_revisions WHERE id = ?`, revisionID).Scan(&revisions); err != nil || revisions != 1 {
		t.Fatalf("revision rows after rejected DeleteTopic = %d, %v", revisions, err)
	}
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM memory_topics_fts WHERE rowid = ?`, ftsRowid).Scan(&fts); err != nil || fts != 1 {
		t.Fatalf("FTS rows after rejected DeleteTopic = %d, %v", fts, err)
	}

	// A topic without any referenced revision still deletes cleanly.
	unreferenced, _ := seedGuardedTopic(t, store, "free", "Free")
	if err := store.DeleteTopic(t.Context(), unreferenced.ID); err != nil {
		t.Fatalf("DeleteTopic for unreferenced topic error = %v", err)
	}
}

func TestKnowledgeHandleGuardReleasesAfterAuthoritativeForget(t *testing.T) {
	store, _ := newTestStore(t)
	topic, revisionID := seedGuardedTopic(t, store, "guarded", "Guarded")
	seedReferencingDocument(t, store, "Guarded document", topic.ID, revisionID)

	forgotten, err := NewKnowledgeStore(store).ForgetSubject(t.Context(), "Guarded document", domain.KnowledgeScopeGlobal, "", "slack-human:evt-forget")
	if err != nil {
		t.Fatalf("ForgetSubject() error = %v", err)
	}
	if !forgotten {
		t.Fatal("forget reported replayed before any tombstone existed")
	}
	var documents int
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM knowledge_documents WHERE content_handle = ?`,
		domain.LegacyTopicRevisionHandle(topic.ID, revisionID)).Scan(&documents); err != nil || documents != 0 {
		t.Fatalf("referencing documents after forget = %d, %v", documents, err)
	}

	if _, err := store.DB().ExecContext(t.Context(), `
		UPDATE memory_topic_revisions SET change_reason = 'released' WHERE id = ?`, revisionID); err != nil {
		t.Fatalf("released revision update rejected: %v", err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `
		DELETE FROM memory_topic_revisions WHERE id = ?`, revisionID); err != nil {
		t.Fatalf("released revision delete rejected: %v", err)
	}
	if err := store.DeleteTopic(t.Context(), topic.ID); err != nil {
		t.Fatalf("DeleteTopic after forget error = %v", err)
	}
}

func TestKnowledgeHandleGuardExistsOnFreshAndUpgradedDatabases(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/fresh-guard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var freshTriggers int
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sqlite_schema WHERE type = 'trigger'
			AND name IN ('memory_topic_revisions_guard_referenced_update', 'memory_topic_revisions_guard_referenced_delete')`,
	).Scan(&freshTriggers); err != nil || freshTriggers != 2 {
		t.Fatalf("fresh guard triggers = %d, %v", freshTriggers, err)
	}

	path, raw := createSchemaAtVersion(t, 37)
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO memory_topics (id, slug, title, description, status, tags, bundle_path, owner_key, content, current_rev, created_at, updated_at)
		VALUES ('mem_legacy1', 'legacy', 'Legacy', '', 'active', '[]', 'topics', '', 'content', 1, 1, 1)`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO memory_topic_revisions (topic_id, revision_number, content, change_reason, created_at)
		VALUES ('mem_legacy1', 1, 'content', 'init', 1)`); err != nil {
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
	var upgradedTriggers int
	if err := upgraded.DB().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sqlite_schema WHERE type = 'trigger'
			AND name IN ('memory_topic_revisions_guard_referenced_update', 'memory_topic_revisions_guard_referenced_delete')`,
	).Scan(&upgradedTriggers); err != nil || upgradedTriggers != 2 {
		t.Fatalf("upgraded guard triggers = %d, %v", upgradedTriggers, err)
	}
	seedReferencingDocument(t, upgraded, "Legacy document", domain.TopicID("mem_legacy1"), 1)
	if _, err := upgraded.DB().ExecContext(t.Context(), `
		UPDATE memory_topic_revisions SET content = 'mutated' WHERE topic_id = 'mem_legacy1'`); err == nil {
		t.Fatal("upgraded referenced revision update unexpectedly succeeded")
	}
	if err := upgraded.DeleteTopic(t.Context(), domain.TopicID("mem_legacy1")); err == nil {
		t.Fatal("upgraded DeleteTopic with referenced revision unexpectedly succeeded")
	}
}
