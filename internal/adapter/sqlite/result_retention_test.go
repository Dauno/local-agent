package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func insertRetentionTestResult(t *testing.T, ctx context.Context, store *Store, resultID, producerID, class string, createdAt time.Time) {
	t.Helper()
	digest := strings.Repeat("c", 64)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO result_records (
		result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key,
		sha256, bytes, media_type, actor, team_id, conversation_key, project,
		retention_class, created_at, state)
		VALUES (?, 'acp_job', ?, 1, 'artifact', ?, ?, 7,
		'text/plain; charset=utf-8', 'U1', 'T1', 'slack:T1:dm:D1', 'app', ?, ?, 'available')`,
		resultID, producerID, resultID+".result", digest, class, createdAt.UnixNano()); err != nil {
		t.Fatalf("insert result record %s: %v", resultID, err)
	}
}

func insertLiveReference(t *testing.T, ctx context.Context, store *Store, resultID, referenceID string) {
	t.Helper()
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO result_references
		(reference_id, result_id, owner_kind, owner_id, state, created_at) VALUES (?, ?, 'test_owner', ?, 'live', ?)`,
		referenceID, resultID, referenceID, time.Now().UTC().UnixNano()); err != nil {
		t.Fatalf("insert live reference for %s: %v", resultID, err)
	}
}

func insertPendingMaterialization(t *testing.T, ctx context.Context, store *Store, resultID, producerID string) {
	t.Helper()
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO result_materializations (
		producer_kind, producer_id, producer_revision, result_id, state, created_at, updated_at)
		VALUES ('acp_job', ?, 1, ?, 'reserved', 1, 1)`, producerID, resultID); err != nil {
		t.Fatalf("insert pending materialization for %s: %v", resultID, err)
	}
}

// TestCheckResultRetentionReportsBoundedCandidateCounts pins hallazgo 6's
// observable eligibility check: it never deletes anything, reports counts
// only, correctly distinguishes reference-protected and
// materialization-pending candidates from unprotected ones, skips a class
// with a non-positive age, and never scans "exported" (its age anchor does
// not exist yet).
func TestCheckResultRetentionReportsBoundedCandidateCounts(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now().UTC()
	old := now.Add(-10 * 24 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	unprotected := strings.Repeat("1", 64)
	insertRetentionTestResult(t, ctx, store, unprotected, "job-unprotected", "context", old)

	referenced := strings.Repeat("2", 64)
	insertRetentionTestResult(t, ctx, store, referenced, "job-referenced", "context", old)
	insertLiveReference(t, ctx, store, referenced, strings.Repeat("a", 64))

	pending := strings.Repeat("3", 64)
	insertRetentionTestResult(t, ctx, store, pending, "job-pending", "context", old)
	insertPendingMaterialization(t, ctx, store, pending, "job-pending")

	tooRecent := strings.Repeat("4", 64)
	insertRetentionTestResult(t, ctx, store, tooRecent, "job-recent", "context", recent)

	exportedOld := strings.Repeat("5", 64)
	insertRetentionTestResult(t, ctx, store, exportedOld, "job-exported", "exported", old)

	ages := domain.ResultRetentionAges{Context: 7 * 24 * time.Hour, Conversation: 30 * 24 * time.Hour, Workstream: 0}
	health, err := store.CheckResultRetention(ctx, ages, now)
	if err != nil {
		t.Fatal(err)
	}
	if !health.ExportedNotImplemented {
		t.Fatal("exported class must always be reported as not implemented")
	}
	var contextCounts, conversationCounts *domain.ResultRetentionClassCounts
	for i := range health.Classes {
		switch health.Classes[i].Class {
		case domain.ResultRetentionContext:
			contextCounts = &health.Classes[i]
		case domain.ResultRetentionConversation:
			conversationCounts = &health.Classes[i]
		case domain.ResultRetentionWorkstream:
			t.Fatal("workstream class must be skipped when its age is zero")
		case domain.ResultRetentionExported:
			t.Fatal("exported class must never be scanned")
		}
	}
	if contextCounts == nil {
		t.Fatal("context class summary is missing")
	}
	// Three results are old enough to qualify (unprotected, referenced,
	// pending); the too-recent one never enters the candidate count.
	if contextCounts.Candidates != 3 {
		t.Fatalf("context candidates = %d, want 3", contextCounts.Candidates)
	}
	if contextCounts.ReferenceProtected != 1 {
		t.Fatalf("context reference_protected = %d, want 1", contextCounts.ReferenceProtected)
	}
	if contextCounts.MaterializationPending != 1 {
		t.Fatalf("context materialization_pending = %d, want 1", contextCounts.MaterializationPending)
	}
	if conversationCounts == nil || conversationCounts.Candidates != 0 {
		t.Fatalf("conversation class summary = %#v, want zero candidates (no conversation-class rows seeded)", conversationCounts)
	}
}

func TestCheckResultRetentionRequiresConfiguredStore(t *testing.T) {
	var store *Store
	if _, err := store.CheckResultRetention(context.Background(), domain.ResultRetentionAges{Context: time.Hour}, time.Now()); err == nil {
		t.Fatal("nil store accepted a retention check")
	}
}
