package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
)

const knowledgeImportLegacyTopicInsert = `INSERT INTO memory_topics (id, slug, title, description, status, tags, bundle_path, owner_key, content, current_rev, created_at, updated_at)
	VALUES (?, ?, ?, '', 'active', '[]', ?, ?, ?, ?, 1, 1)`

func seedKnowledgeImportTopic(t *testing.T, store *adaptersqlite.Store, id, slug, title, bundlePath, ownerKey, content string, rev int) {
	t.Helper()
	if _, err := store.DB().ExecContext(t.Context(), knowledgeImportLegacyTopicInsert, id, slug, title, bundlePath, ownerKey, content, rev); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO memory_topic_revisions (topic_id, revision_number, content, change_reason, created_at)
		VALUES (?, ?, ?, '', 1)`, id, rev, content); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeLegacyImportProjectsAndSurvivesRestart(t *testing.T) {
	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "knowledge.db")
	service, store := newKnowledgeTestService(t, database)
	seedKnowledgeImportTopic(t, store, "mem_person", "person-dauno-slack-t12345678-user-u12345678", "Dauno", "people", "slack:T12345678:user:U12345678", "dauno is the creator", 3)
	seedKnowledgeImportTopic(t, store, "mem_global", "durable-fact", "Durable fact", "topics", "", "the fact is durable", 2)

	knowledgeStore := adaptersqlite.NewKnowledgeStore(store)
	result, err := knowledgeStore.ImportLegacyTopics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 2 {
		t.Fatalf("import result = %+v, want 2 imported", result)
	}

	memoryDir := filepath.Join(t.TempDir(), "memory")
	stopWorker := startKnowledgeProjectionWorker(t, service, store, memoryDir)
	documents := waitForKnowledgeText(t, memoryDir, "knowledge/documents.md", "Dauno")
	for _, want := range []string{"Durable fact", "legacy_curated_document", "user", "global"} {
		if !strings.Contains(documents, want) {
			t.Fatalf("projected documents missing %q:\n%s", want, documents)
		}
	}
	if strings.Contains(documents, "U12345678") {
		t.Fatalf("projected documents leaked the user scope identity:\n%s", documents)
	}
	if strings.Contains(documents, "mem_person") || strings.Contains(documents, "mem_global") {
		t.Fatalf("projected documents leaked the legacy source identity:\n%s", documents)
	}

	// Replay across a store reopen simulates a restart: the worker is
	// stopped first so the store can be closed and reopened safely, and no
	// duplicates or new projection triggers are produced.
	stopWorker()
	store.Close()
	reopened, err := adaptersqlite.Initialize(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replay, err := adaptersqlite.NewKnowledgeStore(reopened).ImportLegacyTopics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Imported != 0 || replay.Skipped != 2 {
		t.Fatalf("replay after reopen = %+v, want 0 imported and 2 skipped", replay)
	}
	var count int
	if err := reopened.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_documents`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("documents after reopen = %d, %v; want 2", count, err)
	}
	content, err := os.ReadFile(filepath.Join(memoryDir, "knowledge", "documents.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "legacy_curated_document") {
		t.Fatalf("projected documents lost content after reopen:\n%s", content)
	}

	// A legacy archive after the import is mirrored into the projected
	// knowledge state by the next startup import.
	if _, err := reopened.DB().ExecContext(ctx, `UPDATE memory_topics SET status = 'archived', updated_at = 2 WHERE id = 'mem_global'`); err != nil {
		t.Fatal(err)
	}
	mirror, err := adaptersqlite.NewKnowledgeStore(reopened).ImportLegacyTopics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.Archived != 1 {
		t.Fatalf("mirror result = %+v, want 1 archived", mirror)
	}
	startKnowledgeProjectionWorker(t, service, reopened, memoryDir)
	archived := waitForKnowledgeText(t, memoryDir, "knowledge/documents.md", "`archived`")
	if !strings.Contains(archived, "Durable fact") {
		t.Fatalf("mirrored archive was not projected:\n%s", archived)
	}
}
