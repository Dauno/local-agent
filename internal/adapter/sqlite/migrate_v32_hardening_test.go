package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// seedGrandfatheredV25DeliveryRows inserts the rows the historical v25 insert
// trigger admitted but every later delivery-shape update trigger rejects:
// file-mode delivery_v1 notifications whose upload_state stayed at the column
// default 'not_applicable'. The rows are seeded under the reconstructed
// historical v25 triggers, exactly as a deployed v25 binary left them. All
// values are synthetic; no secret, real job ID, or result content is used.
func seedGrandfatheredV25DeliveryRows(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	insertJob := func(id, mode string, resultSHA string, resultBytes int64) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO external_agent_jobs (
			job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
			task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
			conversation_key, status, result_sha256, result_bytes, status_revision, timeout_at, created_at, updated_at)
			VALUES (?, ?, 'opencode', 'build', 'workspace', '[]', 'r1',
			'task', 'request', 'wrapper', ?, 'U12345678', 'T12345678',
			'slack:T12345678:dm:D12345678', 'completed', ?, ?, 1, 2, 1, 1)`,
			id, mode, id+"-call", resultSHA, resultBytes); err != nil {
			t.Fatal(err)
		}
	}
	insertNotification := func(id, markdown, contentSHA, deliveryMode, policy, artifactRef string, resultBytes int64, publishState, uploadState, slackFileID, recoveredTS string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO external_agent_job_notifications (
			job_id, status_revision, kind, canonical_markdown, content_sha256,
			renderer_version, channel_id, thread_ts, publish_state, lease_owner,
			lease_expiry, attempts, next_attempt_at, recovered_slack_ts, last_error_code,
			created_at, updated_at, delivery_mode, policy_version, artifact_ref, result_bytes,
			max_markdown_parts, upload_state, slack_file_id)
			VALUES (?, 1, 'terminal', ?, ?, 'markdown_v1', 'D12345678', '', ?, '', 0, 0, 1, ?, '',
			1, 1, ?, ?, ?, ?, 6, ?, ?)`,
			id, markdown, contentSHA, publishState, recoveredTS, deliveryMode, policy, artifactRef, resultBytes, uploadState, slackFileID); err != nil {
			t.Fatal(err)
		}
	}

	fileContent := "file bytes"
	fileDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(fileContent)))
	fileMarkdown := "OpenCode job `gf-file-pending` completed. The complete result was attached."
	publishedMarkdown := "OpenCode job `gf-file-published` completed. The complete result was attached."
	markdownContent := "safe result"
	markdownDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(markdownContent)))
	markdown := "OpenCode job `gf-markdown` completed.\n\nsafe result"

	// File-mode deliveries stuck in pending or unknown with the historical
	// 'not_applicable' upload state: the CR1 grandfathered shape.
	insertJob("gf-file-pending", "detached", fileDigest, int64(len(fileContent)))
	insertNotification("gf-file-pending", fileMarkdown, fileDigest, "file", "delivery_v1", "gf-file-pending-delivery.result", int64(len(fileContent)), "pending", "not_applicable", "", "")

	insertJob("gf-file-unknown", "detached", fileDigest, int64(len(fileContent)))
	insertNotification("gf-file-unknown", fileMarkdown, fileDigest, "file", "delivery_v1", "gf-file-unknown-delivery.result", int64(len(fileContent)), "unknown", "not_applicable", "", "")

	// A published file row is a synthetic worst case: the historical evidence
	// triggers prevented this shape in every deployed era, so the migration
	// must leave it untouched (never block the upgrade, never lose the audit
	// row) rather than fabricate upload evidence.
	insertJob("gf-file-published", "detached", fileDigest, int64(len(fileContent)))
	insertNotification("gf-file-published", publishedMarkdown, fileDigest, "file", "delivery_v1", "gf-file-published-delivery.result", int64(len(fileContent)), "published", "not_applicable", "F999", "1710000000.000001")

	// Control rows with trigger-valid shapes: markdown delivery_v1, legacy
	// markdown, and a foreground delivery_v1 row.
	insertJob("gf-markdown", "detached", markdownDigest, int64(len(markdownContent)))
	insertNotification("gf-markdown", markdown, markdownDigest, "markdown", "delivery_v1", "", int64(len(markdownContent)), "pending", "not_applicable", "", "")

	insertJob("gf-legacy", "detached", markdownDigest, int64(len(markdownContent)))
	insertNotification("gf-legacy", markdown, markdownDigest, "markdown", "legacy_v1", "", 0, "pending", "not_applicable", "", "")

	insertJob("gf-foreground", "foreground", markdownDigest, int64(len(markdownContent)))
	insertNotification("gf-foreground", markdown, markdownDigest, "markdown", "delivery_v1", "", int64(len(markdownContent)), "pending", "not_applicable", "", "")
}

// createGrandfatheredFixture builds a database at version 25 or 30 whose
// notification rows were admitted by the historical v25 insert trigger and
// therefore carry the file+not_applicable shape that v26's update trigger
// rejects. For version 30 the real v26-v30 migrations are applied after the
// rows are seeded, exactly as a deployed v25 database passed through them.
func createGrandfatheredFixture(t *testing.T, version int) (string, *sql.DB) {
	t.Helper()
	path, raw := createSchemaAtVersion(t, 25)
	restoreHistoricalV25DeliveryTriggers(t, raw)
	seedGrandfatheredV25DeliveryRows(t, raw)
	if version <= 25 {
		return path, raw
	}
	ctx := context.Background()
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		_ = raw.Close()
		t.Fatalf("begin v26-v%d migrations: %v", version, err)
	}
	for current := 26; current <= version; current++ {
		if err := migrations[current](ctx, tx); err != nil {
			_ = tx.Rollback()
			_ = raw.Close()
			t.Fatalf("migration %d: %v", current, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		_ = tx.Rollback()
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	return path, raw
}

// assertGrandfatheredUpgradeState verifies the post-upgrade invariants of the
// CR1 fixture: the grandfathered file rows are normalized, the published
// worst-case row is preserved as unclassified audit, the control rows keep
// their complete identities, and no row content was lost.
func assertGrandfatheredUpgradeState(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	fileContent := "file bytes"
	fileDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(fileContent)))
	markdownContent := "safe result"
	markdownDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(markdownContent)))
	fileMarkdown := "OpenCode job `gf-file-pending` completed. The complete result was attached."
	markdown := "OpenCode job `gf-markdown` completed.\n\nsafe result"

	type row struct {
		uploadState, notificationSHA, resultSHA, markdown string
		notificationBytes, resultBytes                    int64
		root                                              int
	}
	read := func(jobID string) row {
		t.Helper()
		var got row
		if err := store.db.QueryRowContext(ctx, `SELECT upload_state, notification_sha256, notification_bytes,
			result_sha256, result_bytes, root_activation_required, canonical_markdown
			FROM external_agent_job_notifications WHERE job_id = ?`, jobID).
			Scan(&got.uploadState, &got.notificationSHA, &got.notificationBytes, &got.resultSHA, &got.resultBytes, &got.root, &got.markdown); err != nil {
			t.Fatalf("%s: %v", jobID, err)
		}
		return got
	}
	notificationIdentity := func(markdown string) (string, int64) {
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(markdown)))
		return digest, int64(len([]byte(markdown)))
	}

	// The grandfathered file rows are normalized to the honest 'unknown'
	// upload state and carry their own complete content identity. As
	// pre-v29 rows they have no terminal snapshot, so the completion route
	// stays 0 (never activation-eligible).
	for _, jobID := range []string{"gf-file-pending", "gf-file-unknown"} {
		got := read(jobID)
		if got.uploadState != "unknown" {
			t.Fatalf("%s upload_state = %q, want unknown", jobID, got.uploadState)
		}
		if got.resultSHA != fileDigest || got.resultBytes != int64(len(fileContent)) {
			t.Fatalf("%s result identity = %q/%d, want %q/%d", jobID, got.resultSHA, got.resultBytes, fileDigest, len(fileContent))
		}
		if got.root != 0 || got.notificationSHA == "" || got.notificationBytes <= 0 {
			t.Fatalf("%s route/identity = %d/%q/%d, want route 0 with notification identity", jobID, got.root, got.notificationSHA, got.notificationBytes)
		}
	}
	// The published worst-case row is preserved untouched: still
	// 'not_applicable', unclassified defaults, original content intact.
	got := read("gf-file-published")
	if got.uploadState != "not_applicable" || got.notificationSHA != "" || got.notificationBytes != 0 || got.resultSHA != "" {
		t.Fatalf("published grandfathered row = %q/%q/%d/%q, want untouched defaults", got.uploadState, got.notificationSHA, got.notificationBytes, got.resultSHA)
	}
	if got.markdown != "OpenCode job `gf-file-published` completed. The complete result was attached." {
		t.Fatalf("published grandfathered row content was changed: %q", got.markdown)
	}
	// Control rows keep their complete identities.
	markdownSHA, markdownBytes := notificationIdentity(markdown)
	got = read("gf-markdown")
	if got.uploadState != "not_applicable" || got.resultSHA != markdownDigest || got.resultBytes != int64(len(markdownContent)) ||
		got.notificationSHA != markdownSHA || got.notificationBytes != markdownBytes || got.root != 0 {
		t.Fatalf("gf-markdown = %+v, want complete markdown identity", got)
	}
	got = read("gf-legacy")
	if got.notificationSHA != markdownSHA || got.notificationBytes != markdownBytes || got.resultSHA != markdownDigest || got.resultBytes != int64(len(markdownContent)) {
		t.Fatalf("gf-legacy = %+v, want mirrored job identity", got)
	}
	got = read("gf-foreground")
	if got.resultSHA != markdownDigest || got.resultBytes != int64(len(markdownContent)) || got.root != 0 {
		t.Fatalf("gf-foreground = %+v, want identity with route 0", got)
	}
	// Audit preservation: every seeded row survived with its content.
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_agent_job_notifications`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 6 {
		t.Fatalf("notification count = %d, want 6", count)
	}
	if read("gf-file-pending").markdown != fileMarkdown {
		t.Fatal("grandfathered notification content was changed")
	}
}

// TestOpenExistingUpgradesV25WithGrandfatheredFileRows proves a v25 database
// holding the rows the historical v25 insert trigger admitted (file-mode with
// upload_state='not_applicable') upgrades through v26-v32 without aborting:
// CR1 before the fix permanently blocked every startup on the v26 update
// trigger.
func TestOpenExistingUpgradesV25WithGrandfatheredFileRows(t *testing.T) {
	ctx := context.Background()
	path, raw := createGrandfatheredFixture(t, 25)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("OpenExisting v25 with grandfathered rows: %v", err)
	}
	defer func() { _ = store.Close() }()
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("upgraded schema version = %d, want %d", version, SchemaVersion)
	}
	assertGrandfatheredUpgradeState(t, store)
}

// TestOpenExistingUpgradesV30WithGrandfatheredFileRows proves the same
// grandfathered rows survive the v26-v30 migrations and that the
// v30 -> v31 -> v32 upgrade completes with normalization before the backfill.
func TestOpenExistingUpgradesV30WithGrandfatheredFileRows(t *testing.T) {
	ctx := context.Background()
	path, raw := createGrandfatheredFixture(t, 30)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("OpenExisting v30 with grandfathered rows: %v", err)
	}
	defer func() { _ = store.Close() }()
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("upgraded schema version = %d, want %d", version, SchemaVersion)
	}
	assertGrandfatheredUpgradeState(t, store)
}

// TestMigrationV32RollsBackEntirelyWithGrandfatheredRows proves that any v32
// failure rolls back the normalization and backfill and leaves the database
// consistent at v30 with the grandfathered rows untouched.
func TestMigrationV32RollsBackEntirelyWithGrandfatheredRows(t *testing.T) {
	ctx := context.Background()
	path, raw := createGrandfatheredFixture(t, 30)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	original := migrations[32]
	migrations[32] = func(ctx context.Context, tx *sql.Tx) error {
		if err := migrateV32(ctx, tx); err != nil {
			return err
		}
		return errors.New("injected v32 failure")
	}
	defer func() { migrations[32] = original }()

	store, err := OpenExisting(ctx, path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting succeeded after injected v32 failure")
	}
	if err == nil || !strings.Contains(err.Error(), "injected v32 failure") {
		t.Fatalf("OpenExisting error = %v, want injected v32 failure", err)
	}

	raw, err = sql.Open("sqlite", mustDataSourceName(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var version int
	if err := raw.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 30 {
		t.Fatalf("schema version after failed migration = %d, want 30", version)
	}
	var uploadState, markdown string
	if err := raw.QueryRowContext(ctx, `SELECT upload_state, canonical_markdown
		FROM external_agent_job_notifications WHERE job_id = 'gf-file-pending'`).
		Scan(&uploadState, &markdown); err != nil {
		t.Fatal(err)
	}
	if uploadState != "not_applicable" || markdown != "OpenCode job `gf-file-pending` completed. The complete result was attached." {
		t.Fatalf("grandfathered row after rollback = %q/%q, want untouched", uploadState, markdown)
	}
	var columnCount int
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('external_agent_job_notifications')
		WHERE name IN ('root_activation_required', 'notification_sha256', 'notification_bytes', 'result_sha256')`).
		Scan(&columnCount); err != nil {
		t.Fatal(err)
	}
	if columnCount != 0 {
		t.Fatalf("v32 columns survived rollback: %d", columnCount)
	}
}

// TestMigrationV32RetriesAfterRollbackWithGrandfatheredRows proves a failed
// attempt does not wedge the upgrade: the next startup applies the full chain
// and completes (idempotent retry over grandfathered rows).
func TestMigrationV32RetriesAfterRollbackWithGrandfatheredRows(t *testing.T) {
	ctx := context.Background()
	path, raw := createGrandfatheredFixture(t, 30)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	original := migrations[32]
	migrations[32] = func(ctx context.Context, tx *sql.Tx) error {
		if err := migrateV32(ctx, tx); err != nil {
			return err
		}
		return errors.New("injected v32 failure")
	}

	store, err := OpenExisting(ctx, path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting succeeded after injected v32 failure")
	}
	if err == nil || !strings.Contains(err.Error(), "injected v32 failure") {
		t.Fatalf("OpenExisting error = %v, want injected v32 failure", err)
	}
	migrations[32] = original

	store, err = OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("OpenExisting retry: %v", err)
	}
	defer func() { _ = store.Close() }()
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version after retry = %d, want %d", version, SchemaVersion)
	}
	assertGrandfatheredUpgradeState(t, store)
}
