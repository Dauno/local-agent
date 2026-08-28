package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestOpenExistingDoesNotCreateDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	store, err := OpenExisting(context.Background(), path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting returned a store for a missing database")
	}
	if !errors.Is(err, ErrDatabaseNotFound) {
		t.Fatalf("OpenExisting error = %v, want ErrDatabaseNotFound", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("OpenExisting created the database: stat error = %v", statErr)
	}
}

func TestInitializeMigratesVersionZeroAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "local-agent.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Initialize(ctx, path)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, SchemaVersion)
	}

	rows, err := store.db.QueryContext(ctx, `
		SELECT name FROM sqlite_schema
		WHERE type = 'table' AND name IN ('dedupe_records', 'conversations', 'messages')
		ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tables, []string{"conversations", "dedupe_records", "messages"}) {
		t.Fatalf("migrated tables = %v", tables)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Initialize(ctx, path)
	if err != nil {
		t.Fatalf("second Initialize: %v", err)
	}
	if err := store.ProbeReadWrite(ctx); err != nil {
		t.Fatalf("ProbeReadWrite: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationV21CreatesContextSummaryTables(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "summary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	for _, table := range []string{"adk_context_summaries", "adk_context_summary_jobs"} {
		var name string
		if err := store.db.QueryRowContext(context.Background(), "SELECT name FROM sqlite_schema WHERE type = 'table' AND name = ?", table).Scan(&name); err != nil || name != table {
			t.Fatalf("migration table %q: %v", table, err)
		}
	}
}

func TestMigrationV24CreatesRecoverableResultCleanupClaims(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "recoverable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	rows, err := store.db.QueryContext(context.Background(), `PRAGMA table_info(recoverable_results)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cleanup_claim", "cleanup_version", "cleanup_claimed_at"} {
		if !columns[name] {
			t.Fatalf("recoverable_results missing %s", name)
		}
	}
}

func TestOpenExistingUpgradesV21ToV26AndPreservesLegacyNotification(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 21)
	seedLegacyExternalAgentNotification(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var policy string
	if err := store.db.QueryRowContext(ctx, `SELECT policy_version FROM external_agent_job_notifications WHERE job_id = 'migration-job'`).Scan(&policy); err != nil {
		t.Fatal(err)
	}
	if policy != "legacy_v1" {
		t.Fatalf("legacy v21 notification policy = %q, want legacy_v1", policy)
	}
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("upgraded schema version = %d, want %d", version, SchemaVersion)
	}
}

func TestOpenExistingUpgradesV28WithoutBackfillingActivations(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 28)
	if _, err := raw.ExecContext(ctx, `INSERT INTO conversations (
		conversation_key, team_id, channel_id, channel_kind, root_ts, last_ts, created_at, updated_at)
		VALUES ('slack:T12345678:dm:D12345678', 'T12345678', 'D12345678', 'dm', '', '1', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO messages (
		conversation_key, role, content, user_id, external_ts, created_at)
		VALUES ('slack:T12345678:dm:D12345678', 'user', 'human', 'U12345678', '1', 1),
		('slack:T12345678:dm:D12345678', 'assistant', 'assistant', '', '2', 2)`); err != nil {
		t.Fatal(err)
	}
	seedLegacyExternalAgentNotification(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("upgraded schema version = %d, want %d", version, SchemaVersion)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT role, source FROM messages ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var sources []string
	for rows.Next() {
		var role, source string
		if err := rows.Scan(&role, &source); err != nil {
			t.Fatal(err)
		}
		sources = append(sources, role+":"+source)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sources, []string{"user:human", "assistant:assistant"}) {
		t.Fatalf("message sources = %v", sources)
	}
	var terminalStatus string
	var publishedAt int64
	if err := store.db.QueryRowContext(ctx, `SELECT terminal_status, published_at
		FROM external_agent_job_notifications WHERE job_id = 'migration-job'`).Scan(&terminalStatus, &publishedAt); err != nil {
		t.Fatal(err)
	}
	if terminalStatus != "" || publishedAt != 0 {
		t.Fatalf("historical notification snapshot = %q/%d", terminalStatus, publishedAt)
	}
	var activationCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_agent_job_activations`).Scan(&activationCount); err != nil {
		t.Fatal(err)
	}
	if activationCount != 0 {
		t.Fatalf("historical activation count = %d", activationCount)
	}
}

func TestOpenExistingUpgradesV25ToV26PreservesLegacyRowsAndAddsEvidenceTriggers(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 25)
	restoreHistoricalV25DeliveryTriggers(t, raw)
	seedLegacyExternalAgentNotification(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_agent_job_notifications WHERE job_id = 'migration-job' AND policy_version = 'legacy_v1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("legacy v25 notification count = %d, want 1", count)
	}
	var triggerCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'trigger' AND name LIKE '%_v26'`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if triggerCount != 4 {
		t.Fatalf("v26 trigger count = %d, want 4", triggerCount)
	}
	_, err = store.db.ExecContext(ctx, `INSERT INTO external_agent_job_notifications (
		job_id, status_revision, kind, canonical_markdown, content_sha256, renderer_version,
		channel_id, publish_state, next_attempt_at, delivery_mode, policy_version,
		result_bytes, max_markdown_parts, upload_state, recovered_slack_ts)
		VALUES ('migration-job', 1, 'invalid', 'result', ?, 'markdown_v1', 'D12345678',
		'published', 1, 'markdown', 'delivery_v1', 1, 1, 'not_applicable', '')`, strings.Repeat("a", 64))
	if err == nil {
		t.Fatal("v26 published delivery without Slack evidence was accepted")
	}
	_, err = store.db.ExecContext(ctx, `INSERT INTO external_agent_job_notifications (
		job_id, status_revision, kind, canonical_markdown, content_sha256, renderer_version,
		channel_id, publish_state, next_attempt_at, delivery_mode, policy_version,
		artifact_ref, result_bytes, max_markdown_parts, upload_state)
		VALUES ('migration-job', 1, 'invalid-file', 'result', ?, 'markdown_v1', 'D12345678',
		'pending', 1, 'file', 'delivery_v1', 'migration-job-delivery.result', 1, 1, 'not_applicable')`, strings.Repeat("a", 64))
	if err == nil {
		t.Fatal("v26 file delivery with inapplicable upload state was accepted")
	}
}

func restoreHistoricalV25DeliveryTriggers(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, name := range []string{
		"external_agent_job_notifications_delivery_insert",
		"external_agent_job_notifications_delivery_shape_update",
		"external_agent_job_notifications_delivery_evidence_insert",
		"external_agent_job_notifications_delivery_evidence_update",
	} {
		if _, err := db.ExecContext(ctx, "DROP TRIGGER "+name); err != nil {
			t.Fatal(err)
		}
	}
	_, err := db.ExecContext(ctx, `CREATE TRIGGER external_agent_job_notifications_delivery_insert
		BEFORE INSERT ON external_agent_job_notifications
		WHEN NEW.policy_version != 'legacy_v1' AND (
			NEW.delivery_mode NOT IN ('markdown', 'file') OR
			length(NEW.content_sha256) != 64 OR NEW.result_bytes <= 0 OR NEW.max_markdown_parts < 1 OR NEW.max_markdown_parts > 8 OR
			(NEW.delivery_mode = 'markdown' AND (length(NEW.artifact_ref) > 0 OR NEW.upload_state != 'not_applicable')) OR
			(NEW.delivery_mode = 'file' AND length(NEW.artifact_ref) = 0) OR
			NEW.upload_state NOT IN ('not_applicable', 'pending', 'url_requested', 'bytes_uploaded', 'completed', 'unknown')
		)
		BEGIN SELECT RAISE(ABORT, 'invalid external-agent result delivery'); END`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `CREATE TRIGGER external_agent_job_notifications_delivery_shape_update
		BEFORE UPDATE ON external_agent_job_notifications
		WHEN NEW.policy_version != 'legacy_v1' AND (
			NEW.delivery_mode NOT IN ('markdown', 'file') OR length(NEW.content_sha256) != 64 OR NEW.result_bytes <= 0 OR
			NEW.max_markdown_parts < 1 OR NEW.max_markdown_parts > 8 OR
			(NEW.delivery_mode = 'markdown' AND (length(NEW.artifact_ref) > 0 OR NEW.upload_state != 'not_applicable')) OR
			(NEW.delivery_mode = 'file' AND length(NEW.artifact_ref) = 0) OR
			NEW.upload_state NOT IN ('not_applicable', 'pending', 'url_requested', 'bytes_uploaded', 'completed', 'unknown')
		)
		BEGIN SELECT RAISE(ABORT, 'invalid external-agent result delivery'); END`)
	if err != nil {
		t.Fatal(err)
	}
}

func createSchemaAtVersion(t *testing.T, version int) (string, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "migration.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dsn, err := dataSourceName(path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	for current := 1; current <= version; current++ {
		if err := migrations[current](ctx, tx); err != nil {
			_ = tx.Rollback()
			_ = raw.Close()
			t.Fatalf("migration %d: %v", current, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(version)); err != nil {
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

func seedLegacyExternalAgentNotification(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO external_agent_jobs (
		job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
		task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
		conversation_key, status, timeout_at, created_at, updated_at)
		VALUES ('migration-job', 'detached', 'agentcli', 'build', 'workspace', '[]', 'r1',
		'task', 'request', 'wrapper', 'original', 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', 'completed', 2, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO external_agent_job_notifications (
		job_id, status_revision, kind, canonical_markdown, content_sha256, renderer_version,
		channel_id, next_attempt_at, created_at, updated_at)
		VALUES ('migration-job', 0, 'terminal', 'legacy result', 'legacy', 'markdown_v1', 'D12345678', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
}

func TestCreateUsesRestrictivePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-agent.db")
	store, err := Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf("database permissions = %04o, want no group/other access", got)
	}
	if _, err := Create(context.Background(), path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second Create error = %v, want os.ErrExist", err)
	}
}

func TestOpenExistingRejectsFutureSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "future.db")
	store, err := Initialize(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	dsn, err := dataSourceName(path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
		_ = rawDB.Close()
		t.Fatal(err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenExisting(ctx, path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting returned a store for a future schema")
	}
	if !errors.Is(err, ErrFutureSchema) {
		t.Fatalf("OpenExisting error = %v, want ErrFutureSchema", err)
	}
	var versionError *FutureSchemaError
	if !errors.As(err, &versionError) {
		t.Fatalf("OpenExisting error %T does not expose FutureSchemaError", err)
	}
	if versionError.Found != 99 || versionError.Supported != SchemaVersion {
		t.Fatalf("FutureSchemaError = %#v", versionError)
	}

	rawDB, err = sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rawDB.Close() }()
	var version int
	if err := rawDB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 99 {
		t.Fatalf("failed open mutated future schema version to %d", version)
	}
}

func TestOpenExistingUpgradesV14WithoutReset(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v14.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dsn, err := dataSourceName(path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 14; version++ {
		if err := migrations[version](ctx, tx); err != nil {
			t.Fatalf("migration %d: %v", version, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO conversations (
			conversation_key, team_id, channel_id, channel_kind, root_ts,
			last_ts, created_at, updated_at
		) VALUES ('slack:T12345678:dm:D12345678', 'T12345678', 'D12345678', 'dm', '', '1.1', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 14"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("OpenExisting() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.EnsureDMIdentityMode(ctx, false); err != nil {
		t.Fatalf("EnsureDMIdentityMode(false) error = %v", err)
	}
	var conversationCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations`).Scan(&conversationCount); err != nil {
		t.Fatal(err)
	}
	if conversationCount != 1 {
		t.Fatalf("conversation count = %d, want 1", conversationCount)
	}
}

func TestOpenExistingUpgradesV3OutboxWithSourceSnapshot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v3.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dsn, err := dataSourceName(path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV1(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := migrateV2(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := migrateV3(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting unexpectedly accepted a pre-tool schema")
	}
	if !errors.Is(err, ErrStateResetNeeded) {
		t.Fatalf("OpenExisting error = %v, want ErrStateResetNeeded", err)
	}
	if store == nil {
		return
	}
	var defaultValue string
	if err := store.db.QueryRowContext(ctx, `SELECT dflt_value FROM pragma_table_info('memory_outbox') WHERE name = 'source_messages'`).Scan(&defaultValue); err != nil {
		t.Fatal(err)
	}
	if defaultValue != "'[]'" {
		t.Fatalf("source_messages default = %q, want '[]'", defaultValue)
	}
}

func TestOpenExistingUpgradesV5ExchangeIntentsAsPrepared(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v5.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dsn, err := dataSourceName(path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range []func(context.Context, *sql.Tx) error{migrateV1, migrateV2, migrateV3, migrateV4, migrateV5} {
		if err := migration(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_exchange_intents (
			id, conversation_key, team_id, channel_id, channel_kind, root_ts, last_ts,
			assistant_content, assistant_external_ts, assistant_created_at, retain, source_messages, created_at
		) VALUES ('intent', 'slack:T:dm:D', 'T', 'D', 'dm', '', '1', 'reply', '1', 1, 10, '[]', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 5"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting unexpectedly accepted a pre-tool schema")
	}
	if !errors.Is(err, ErrStateResetNeeded) {
		t.Fatalf("OpenExisting error = %v, want ErrStateResetNeeded", err)
	}
	if store == nil {
		return
	}
	var status, correlationID string
	if err := store.db.QueryRowContext(ctx, `SELECT publish_status, correlation_id FROM memory_exchange_intents WHERE id = 'intent'`).Scan(&status, &correlationID); err != nil {
		t.Fatal(err)
	}
	if status != "prepared" {
		t.Fatalf("migrated intent status = %q, want prepared", status)
	}
	if correlationID != "" {
		t.Fatalf("legacy prepared intent correlation = %q, want empty", correlationID)
	}
	// A content-only finder must not be consulted for an intent that predates
	// durable correlation metadata.
	if err := store.ReconcileAssistantExchanges(ctx, exchangeFinder{content: "reply", timestamp: "2"}); err != nil {
		t.Fatal(err)
	}
	var localAssistantCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE role = 'assistant'`).Scan(&localAssistantCount); err != nil {
		t.Fatal(err)
	}
	if localAssistantCount != 0 {
		t.Fatalf("unverified v5 intent created %d local assistant messages", localAssistantCount)
	}
	var preparedCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_exchange_intents WHERE publish_status = 'prepared'`).Scan(&preparedCount); err != nil {
		t.Fatal(err)
	}
	if preparedCount != 1 {
		t.Fatalf("legacy prepared intent count = %d, want 1", preparedCount)
	}
}

func TestOpenExistingUpgradesV7TopicsWithDefaultBundlePath(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v7.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dsn, err := dataSourceName(path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range []func(context.Context, *sql.Tx) error{migrateV1, migrateV2, migrateV3, migrateV4, migrateV5, migrateV6, migrateV7} {
		if err := migration(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_topics (id, slug, title, description, status, tags, content, current_rev, created_at, updated_at)
		VALUES ('mem_existing', 'existing', 'Existing', '', 'active', '[]', 'content', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 7"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting unexpectedly accepted a pre-tool schema")
	}
	if !errors.Is(err, ErrStateResetNeeded) {
		t.Fatalf("OpenExisting error = %v, want ErrStateResetNeeded", err)
	}
	if store == nil {
		return
	}

	var defaultPath string
	if err := store.db.QueryRowContext(ctx,
		`SELECT dflt_value FROM pragma_table_info('memory_topics') WHERE name = 'bundle_path'`,
	).Scan(&defaultPath); err != nil {
		t.Fatal(err)
	}
	if defaultPath != "'topics'" {
		t.Fatalf("bundle_path default = %q, want 'topics'", defaultPath)
	}
}

func TestOpenExistingUpgradesV8PersonTopicWithUnambiguousOwner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v8.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dsn, err := dataSourceName(path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range []func(context.Context, *sql.Tx) error{migrateV1, migrateV2, migrateV3, migrateV4, migrateV5, migrateV6, migrateV7, migrateV8} {
		if err := migration(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_topics (id, slug, title, description, status, tags, bundle_path, content, current_rev, created_at, updated_at)
		VALUES ('mem_legacy', 'person-dauno', 'Dauno', '', 'active', '[]', 'people', 'legacy identity', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	revision, err := tx.ExecContext(ctx, `
		INSERT INTO memory_topic_revisions (topic_id, revision_number, content, change_reason, created_at)
		VALUES ('mem_legacy', 1, 'legacy identity', 'created', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	revisionID, err := revision.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_evidence (topic_revision, source_key, source_ts, author_id, type)
		VALUES (?, 'slack:T12345678:dm:D12345678', '1', 'U12345678', 'source')`, revisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 8"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting unexpectedly accepted a pre-tool schema")
	}
	if !errors.Is(err, ErrStateResetNeeded) {
		t.Fatalf("OpenExisting error = %v, want ErrStateResetNeeded", err)
	}
	if store == nil {
		return
	}
	var ownerKey string
	if err := store.db.QueryRowContext(ctx, `SELECT owner_key FROM memory_topics WHERE id = 'mem_legacy'`).Scan(&ownerKey); err != nil {
		t.Fatal(err)
	}
	if ownerKey != "slack:T12345678:user:U12345678" {
		t.Fatalf("legacy topic owner_key = %q, want inferred owner", ownerKey)
	}
}

// TestMigrationV31ChainAppliesOnFreshSchema proves the complete compilable
// migration chain v30 -> v31 -> v32 applies on a fresh database with no rows.
func TestMigrationV31ChainAppliesOnFreshSchema(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 0)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("OpenExisting on fresh schema: %v", err)
	}
	defer func() { _ = store.Close() }()
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("fresh schema version = %d, want %d", version, SchemaVersion)
	}
}

func insertV30JobRow(t *testing.T, db *sql.DB, id, mode, status, summary, artifact, sha string, bytes int64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO external_agent_jobs (
		job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
		task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
		conversation_key, status, result_summary, result_artifact, result_sha256, result_bytes,
		status_revision, timeout_at, created_at, updated_at)
		VALUES (?, ?, 'agentcli', 'build', 'workspace', '[]', 'r1',
		'task', 'request', 'wrapper', ?, 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', ?, ?, ?, ?, ?, 1, 2, 1, 1)`,
		id, mode, id+"-call", status, summary, artifact, sha, bytes); err != nil {
		t.Fatal(err)
	}
}

// insertV30ActivationRow seeds one activation with identity fields that satisfy
// the v29 table checks. Response fields are populated only for states that
// require them.
func insertV30ActivationRow(t *testing.T, db *sql.DB, jobID, state string) {
	t.Helper()
	responseBody, responseSHA, intentID, correlationID, slackTS := "", "", "", "", ""
	if state == "response_prepared" || state == "completed" {
		responseBody = "prepared response"
		responseSHA = fmt.Sprintf("%x", sha256.Sum256([]byte(responseBody)))
		intentID = "exchange_" + jobID
		correlationID = "correlation_" + jobID
	}
	if state == "completed" {
		slackTS = "1710000000.000001"
	}
	leaseOwner, leaseExpiry := "", int64(0)
	if state == "processing" || state == "model_started" || state == "response_prepared" {
		leaseOwner, leaseExpiry = "worker-1", 2
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO external_agent_job_activations (
		job_id, status_revision, kind, activation_id, terminal_status, notification_sha256,
		actor, team_id, conversation_key, original_call_id, delivery_mode, content_bytes,
		slack_message_ts, published_at, state, attempt, lease_owner, lease_expiry, next_attempt_at,
		last_error_code, response_body, response_sha256, exchange_intent_id, correlation_id,
		response_slack_ts, created_at, updated_at)
		VALUES (?, 1, 'terminal', ?, 'completed', ?, 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', ?, 'markdown', 12, '1710000000.000002', 1, ?, 1, ?, ?, 0,
		'', ?, ?, ?, ?, ?, 1, 1)`,
		jobID, "activation_"+jobID, strings.Repeat("a", 64), jobID+"-call", state, leaseOwner, leaseExpiry,
		responseBody, responseSHA, intentID, correlationID, slackTS); err != nil {
		t.Fatal(err)
	}
}

func insertV30LegacyNotification(t *testing.T, db *sql.DB, jobID, markdown string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO external_agent_job_notifications (
		job_id, status_revision, kind, terminal_status, canonical_markdown, content_sha256,
		renderer_version, channel_id, next_attempt_at, created_at, updated_at,
		delivery_mode, policy_version, upload_state)
		VALUES (?, 1, 'terminal', 'completed', ?, ?, 'markdown_v1', 'D12345678', 1, 1, 1,
		'markdown', 'legacy_v1', 'not_applicable')`,
		jobID, markdown, fmt.Sprintf("%x", sha256.Sum256([]byte(markdown)))); err != nil {
		t.Fatal(err)
	}
}

// TestMigrationV31RepairsForegroundInlineIdentityMatrix covers the v30 -> v31
// identity repair over ASCII, '<', control characters, multibyte UTF-8,
// invalid UTF-8 and whitespace-only summaries, plus non-candidates.
func TestMigrationV31RepairsForegroundInlineIdentityMatrix(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 30)
	digest := func(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }
	type seed struct {
		id, mode, status, summary, artifact, sha string
		bytes                                    int64
	}
	for _, s := range []seed{
		{"fg-ascii", "foreground", "completed", "plain summary", "", "", 0},
		{"fg-less", "foreground", "completed", "use a < b", "", "", 0},
		{"fg-controls", "foreground", "completed", "a\x07b\x01c", "", "", 0},
		{"fg-multibyte", "foreground", "completed", "héllo 世界", "", "", 0},
		{"fg-invalid-utf8", "foreground", "completed", "\xff\xfe", "", "", 0},
		{"fg-whitespace", "foreground", "completed", "  \t", "", "", 0},
		{"fg-empty", "foreground", "completed", "", "", "", 0},
		{"fg-nul-prefixed", "foreground", "completed", "\x00abc", "", "", 0},
		{"fg-nul-empty-after-sanitize", "foreground", "completed", "\x00\x00", "", digest("x"), 5},
		{"fg-consistent", "foreground", "completed", "ok", "", digest("ok"), 2},
		{"fg-unsanitized-identity", "foreground", "completed", "raw <x>", "", digest("raw <x>"), int64(len("raw <x>"))},
		{"detached-inline", "detached", "completed", "d", "", "", 0},
		{"fg-artifact", "foreground", "completed", "a", "ref", "", 0},
		{"fg-failed", "foreground", "failed", "f", "", "", 0},
	} {
		insertV30JobRow(t, raw, s.id, s.mode, s.status, s.summary, s.artifact, s.sha, s.bytes)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("OpenExisting v30: %v", err)
	}
	defer func() { _ = store.Close() }()
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("upgraded schema version = %d, want %d", version, SchemaVersion)
	}

	type want struct {
		summary, sha string
		bytes        int64
	}
	expected := map[string]want{
		"fg-ascii":                    {"plain summary", digest("plain summary"), int64(len("plain summary"))},
		"fg-less":                     {"use a &lt; b", digest("use a &lt; b"), int64(len("use a &lt; b"))},
		"fg-controls":                 {"abc", digest("abc"), int64(len("abc"))},
		"fg-multibyte":                {"héllo 世界", digest("héllo 世界"), int64(len([]byte("héllo 世界")))},
		"fg-invalid-utf8":             {"\xff\xfe", "", 0},
		"fg-whitespace":               {"  \t", "", 0},
		"fg-empty":                    {"", "", 0},
		"fg-nul-prefixed":             {"abc", digest("abc"), int64(len("abc"))},
		"fg-nul-empty-after-sanitize": {"\x00\x00", "", 0},
		"fg-consistent":               {"ok", digest("ok"), 2},
		"fg-unsanitized-identity":     {"raw &lt;x>", digest("raw &lt;x>"), int64(len("raw &lt;x>"))},
		"detached-inline":             {"d", "", 0},
		"fg-artifact":                 {"a", "", 0},
		"fg-failed":                   {"f", "", 0},
	}
	for jobID, w := range expected {
		var got want
		if err := store.db.QueryRowContext(ctx, `SELECT result_summary, result_sha256, result_bytes
			FROM external_agent_jobs WHERE job_id = ?`, jobID).
			Scan(&got.summary, &got.sha, &got.bytes); err != nil {
			t.Fatalf("%s: %v", jobID, err)
		}
		if got != w {
			t.Fatalf("%s identity = %+v, want %+v", jobID, got, w)
		}
	}
}

// TestMigrationV31RetiresForegroundActivationsByState covers every claimable
// activation state through its legal transition, preserves terminal rows,
// clears leases, and proves v32 applies cleanly after v31.
func TestMigrationV31RetiresForegroundActivationsByState(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 30)
	if _, err := raw.ExecContext(ctx, `INSERT INTO conversations (
		conversation_key, team_id, channel_id, channel_kind, root_ts, last_ts, created_at, updated_at)
		VALUES ('slack:T12345678:dm:D12345678', 'T12345678', 'D12345678', 'dm', '', '1', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO messages (
		conversation_key, role, source, content, user_id, external_ts, created_at)
		VALUES ('slack:T12345678:dm:D12345678', 'user', 'human', 'human', 'U12345678', '1', 1),
		('slack:T12345678:dm:D12345678', 'assistant', 'assistant', 'assistant', '', '2', 2)`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pending", "processing", "model_started", "response_prepared", "completed", "failed", "completion_unknown"} {
		insertV30JobRow(t, raw, "fg-"+state, "foreground", "completed", "ok", "", "", 0)
		insertV30ActivationRow(t, raw, "fg-"+state, state)
	}
	insertV30JobRow(t, raw, "det-pending", "detached", "completed", "ok", "", "", 0)
	insertV30ActivationRow(t, raw, "det-pending", "pending")
	insertV30LegacyNotification(t, raw, "fg-pending", "External-agent job `fg-pending` completed.\n\nok")
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("OpenExisting v30: %v", err)
	}
	defer func() { _ = store.Close() }()
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("upgraded schema version = %d, want %d", version, SchemaVersion)
	}

	type activationState struct {
		state, code, leaseOwner, responseBody, intentID, slackTS string
		leaseExpiry                                              int64
	}
	expected := map[string]activationState{
		"fg-pending":            {"failed", "foreground_activation_retired", "", "", "", "", 0},
		"fg-processing":         {"failed", "foreground_activation_retired", "", "", "", "", 0},
		"fg-model_started":      {"completion_unknown", "foreground_activation_retired", "", "", "", "", 0},
		"fg-response_prepared":  {"failed", "foreground_activation_retired", "", "prepared response", "exchange_fg-response_prepared", "", 0},
		"fg-completed":          {"completed", "", "", "prepared response", "exchange_fg-completed", "1710000000.000001", 0},
		"fg-failed":             {"failed", "", "", "", "", "", 0},
		"fg-completion_unknown": {"completion_unknown", "", "", "", "", "", 0},
		"det-pending":           {"pending", "", "", "", "", "", 0},
	}
	for jobID, w := range expected {
		var got activationState
		if err := store.db.QueryRowContext(ctx, `SELECT state, last_error_code, lease_owner, response_body,
			exchange_intent_id, response_slack_ts, lease_expiry
			FROM external_agent_job_activations WHERE job_id = ?`, jobID).
			Scan(&got.state, &got.code, &got.leaseOwner, &got.responseBody, &got.intentID, &got.slackTS, &got.leaseExpiry); err != nil {
			t.Fatalf("%s: %v", jobID, err)
		}
		if got != w {
			t.Fatalf("%s activation = %+v, want %+v", jobID, got, w)
		}
	}
	var foregroundClaimable int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_agent_job_activations a
		JOIN external_agent_jobs j ON j.job_id = a.job_id
		WHERE j.mode = 'foreground' AND a.state IN ('pending', 'processing', 'model_started', 'response_prepared')`).
		Scan(&foregroundClaimable); err != nil {
		t.Fatal(err)
	}
	if foregroundClaimable != 0 {
		t.Fatalf("foreground claimable activations = %d, want 0", foregroundClaimable)
	}
	var detachedClaimable int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_agent_job_activations a
		JOIN external_agent_jobs j ON j.job_id = a.job_id
		WHERE j.mode = 'detached' AND a.state IN ('pending', 'processing', 'model_started', 'response_prepared')`).
		Scan(&detachedClaimable); err != nil {
		t.Fatal(err)
	}
	if detachedClaimable != 1 {
		t.Fatalf("detached claimable activations = %d, want 1", detachedClaimable)
	}

	// Notification rows, Slack evidence and the conversation transcript survive.
	var notificationCount, messageCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_agent_job_notifications`).Scan(&notificationCount); err != nil {
		t.Fatal(err)
	}
	if notificationCount != 1 {
		t.Fatalf("notification count = %d, want 1", notificationCount)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&messageCount); err != nil {
		t.Fatal(err)
	}
	if messageCount != 2 {
		t.Fatalf("message count = %d, want 2", messageCount)
	}
	// v32 applies cleanly after v31 and mirrors the repaired job identity.
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("ok")))
	var markdown string
	var rootRequired int
	var resultSHA string
	var resultBytes int64
	if err := store.db.QueryRowContext(ctx, `SELECT canonical_markdown, root_activation_required, result_sha256, result_bytes
		FROM external_agent_job_notifications WHERE job_id = 'fg-pending'`).
		Scan(&markdown, &rootRequired, &resultSHA, &resultBytes); err != nil {
		t.Fatal(err)
	}
	if markdown != "External-agent job `fg-pending` completed.\n\nok" || rootRequired != 0 || resultSHA != digest || resultBytes != 2 {
		t.Fatalf("v32 backfill after v31 = %q/%d/%s/%d, want preserved markdown, route 0, identity %s/2", markdown, rootRequired, resultSHA, resultBytes, digest)
	}
}

// TestMigrationV31RollsBackEntirelyOnError proves that any migration failure
// rolls back every v31 change and leaves PRAGMA user_version untouched.
func TestMigrationV31RollsBackEntirelyOnError(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 30)
	insertV30JobRow(t, raw, "rollback-job", "foreground", "completed", "rollback text", "", "", 0)
	insertV30ActivationRow(t, raw, "rollback-job", "pending")
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	original := migrations[31]
	migrations[31] = func(ctx context.Context, tx *sql.Tx) error {
		if err := migrateV31(ctx, tx); err != nil {
			return err
		}
		return errors.New("injected v31 failure")
	}
	defer func() { migrations[31] = original }()

	store, err := OpenExisting(ctx, path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting succeeded after injected v31 failure")
	}
	if err == nil || !strings.Contains(err.Error(), "injected v31 failure") {
		t.Fatalf("OpenExisting error = %v, want injected v31 failure", err)
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
	var sha string
	var bytes int64
	if err := raw.QueryRowContext(ctx, `SELECT result_sha256, result_bytes FROM external_agent_jobs WHERE job_id = 'rollback-job'`).
		Scan(&sha, &bytes); err != nil {
		t.Fatal(err)
	}
	if sha != "" || bytes != 0 {
		t.Fatalf("rolled-back job identity = %q/%d, want empty/zero", sha, bytes)
	}
	var state string
	if err := raw.QueryRowContext(ctx, `SELECT state FROM external_agent_job_activations WHERE job_id = 'rollback-job'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.ActivationPending) {
		t.Fatalf("rolled-back activation state = %q, want pending", state)
	}

	migrations[31] = original
	store, err = OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("OpenExisting after restore: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version after retry = %d, want %d", version, SchemaVersion)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("rollback text")))
	if err := store.db.QueryRowContext(ctx, `SELECT result_sha256, result_bytes FROM external_agent_jobs WHERE job_id = 'rollback-job'`).
		Scan(&sha, &bytes); err != nil {
		t.Fatal(err)
	}
	if sha != digest || bytes != int64(len("rollback text")) {
		t.Fatalf("repaired job identity = %q/%d, want %s/%d", sha, bytes, digest, len("rollback text"))
	}
	if err := store.db.QueryRowContext(ctx, `SELECT state FROM external_agent_job_activations WHERE job_id = 'rollback-job'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.ActivationFailed) {
		t.Fatalf("retired activation state = %q, want failed", state)
	}
}

func mustDataSourceName(t *testing.T, path string) string {
	t.Helper()
	dsn, err := dataSourceName(path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

func TestMigrationV32AddsExplicitRouteAndIdentityColumnsOnFreshSchema(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "v32.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	rows, err := store.db.QueryContext(context.Background(), `PRAGMA table_info(external_agent_job_notifications)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"root_activation_required", "notification_sha256", "notification_bytes", "result_sha256", "content_sha256", "result_bytes"} {
		if !columns[name] {
			t.Fatalf("fresh schema missing %s", name)
		}
	}
	var triggerCount int
	if err := store.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'trigger' AND name LIKE 'external_agent_job_notifications_%' AND (name LIKE '%route%' OR name LIKE '%identity%')`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if triggerCount != 3 {
		t.Fatalf("v32 trigger count = %d, want 3", triggerCount)
	}
}

func TestOpenExistingUpgradesV30ToV32WithExplicitRoutesAndIdentities(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 30)

	insertV30Job := func(id, mode, status, resultSHA string, resultBytes int64) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, `INSERT INTO external_agent_jobs (
			job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
			task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
			conversation_key, status, result_sha256, result_bytes, status_revision, timeout_at, created_at, updated_at)
			VALUES (?, ?, 'agentcli', 'build', 'workspace', '[]', 'r1',
			'task', 'request', 'wrapper', ?, 'U12345678', 'T12345678',
			'slack:T12345678:dm:D12345678', ?, ?, ?, 1, 2, 1, 1)`,
			id, mode, id+"-call", status, resultSHA, resultBytes); err != nil {
			t.Fatal(err)
		}
	}
	insertV30Notification := func(id, terminalStatus, markdown, contentSHA string, policy, deliveryMode, artifactRef string, resultBytes int64) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, `INSERT INTO external_agent_job_notifications (
			job_id, status_revision, kind, terminal_status, canonical_markdown, content_sha256,
			renderer_version, channel_id, next_attempt_at, created_at, updated_at,
			delivery_mode, policy_version, artifact_ref, result_bytes, max_markdown_parts, upload_state, published_at)
			VALUES (?, 1, 'terminal', ?, ?, ?, 'markdown_v1', 'D12345678', 1, 1, 1, ?, ?, ?, ?, 6, ?, 0)`,
			id, terminalStatus, markdown, contentSHA, deliveryMode, policy, artifactRef, resultBytes, uploadStateForPolicy(policy, deliveryMode)); err != nil {
			t.Fatal(err)
		}
	}

	markdownText := "External-agent job `matrix-detached` completed.\n\nsafe result"
	markdownDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(markdownText)))
	resultDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("safe result")))
	legacyMarkdown := "legacy summary"
	legacyDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(legacyMarkdown)))

	insertV30Job("matrix-detached-markdown", "detached", "completed", resultDigest, int64(len("safe result")))
	insertV30Notification("matrix-detached-markdown", "completed", markdownText, resultDigest, "delivery_v1", "markdown", "", int64(len("safe result")))

	fileMarkdown := "External-agent job `matrix-detached-file` completed. The complete result was attached."
	fileDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("file bytes")))
	insertV30Job("matrix-detached-file", "detached", "completed", fileDigest, int64(len("file bytes")))
	insertV30Notification("matrix-detached-file", "completed", fileMarkdown, fileDigest, "delivery_v1", "file", "matrix-detached-file-delivery.result", int64(len("file bytes")))

	failedMarkdown := "External-agent job `matrix-detached-failed` failed."
	insertV30Job("matrix-detached-failed", "detached", "failed", "", 0)
	insertV30Notification("matrix-detached-failed", "failed", failedMarkdown, fmt.Sprintf("%x", sha256.Sum256([]byte(failedMarkdown))), "legacy_v1", "markdown", "", 0)

	insertV30Job("matrix-foreground", "foreground", "completed", resultDigest, int64(len("safe result")))
	insertV30Notification("matrix-foreground", "completed", markdownText, resultDigest, "delivery_v1", "markdown", "", int64(len("safe result")))

	insertV30Job("matrix-legacy", "detached", "completed", resultDigest, int64(len("safe result")))
	insertV30Notification("matrix-legacy", "", legacyMarkdown, legacyDigest, "legacy_v1", "markdown", "", 0)

	insertV30Job("matrix-invalid-result", "detached", "completed", "not-a-digest", int64(len("safe result")))
	insertV30Notification("matrix-invalid-result", "completed", markdownText, resultDigest, "delivery_v1", "markdown", "", int64(len("safe result")))

	insertV30Job("matrix-invalid-legacy", "detached", "completed", "not-a-digest", int64(len("safe result")))
	insertV30Notification("matrix-invalid-legacy", "completed", legacyMarkdown, legacyDigest, "legacy_v1", "markdown", "", 0)

	insertV30Job("matrix-empty-markdown", "detached", "completed", resultDigest, int64(len("safe result")))
	insertV30Notification("matrix-empty-markdown", "completed", "   ", fmt.Sprintf("%x", sha256.Sum256([]byte("   "))), "legacy_v1", "markdown", "", 0)

	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("upgraded schema version = %d, want %d", version, SchemaVersion)
	}

	type expected struct {
		root            int
		notificationSHA string
		notificationB   int64
		resultSHA       string
		resultBytes     int64
	}
	cases := map[string]expected{
		"matrix-detached-markdown": {1, markdownDigest, int64(len([]byte(markdownText))), resultDigest, int64(len("safe result"))},
		"matrix-detached-file":     {1, fmt.Sprintf("%x", sha256.Sum256([]byte(fileMarkdown))), int64(len([]byte(fileMarkdown))), fileDigest, int64(len("file bytes"))},
		"matrix-detached-failed":   {1, fmt.Sprintf("%x", sha256.Sum256([]byte(failedMarkdown))), int64(len([]byte(failedMarkdown))), "", 0},
		"matrix-foreground":        {0, markdownDigest, int64(len([]byte(markdownText))), resultDigest, int64(len("safe result"))},
		"matrix-legacy":            {0, legacyDigest, int64(len([]byte(legacyMarkdown))), resultDigest, int64(len("safe result"))},
		// A delivery_v1 row always keeps its own complete content identity: a
		// digest over the bytes it actually delivered, never the job's broken
		// sha paired with stale bytes (CR2 atomic-pair fix).
		"matrix-invalid-result": {1, markdownDigest, int64(len([]byte(markdownText))), resultDigest, int64(len("safe result"))},
		// A legacy row with an unclassifiable job identity fails closed to the
		// empty pair; bytes are never retained without a digest.
		"matrix-invalid-legacy": {1, legacyDigest, int64(len([]byte(legacyMarkdown))), "", 0},
		"matrix-empty-markdown": {0, "", 0, resultDigest, int64(len("safe result"))},
	}
	for jobID, want := range cases {
		var got expected
		if err := store.db.QueryRowContext(ctx, `SELECT root_activation_required, notification_sha256, notification_bytes, result_sha256, result_bytes
			FROM external_agent_job_notifications WHERE job_id = ?`, jobID).
			Scan(&got.root, &got.notificationSHA, &got.notificationB, &got.resultSHA, &got.resultBytes); err != nil {
			t.Fatalf("%s: %v", jobID, err)
		}
		if got != want {
			t.Fatalf("%s backfill = %+v, want %+v", jobID, got, want)
		}
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE external_agent_job_notifications
		SET root_activation_required = 0 WHERE job_id = 'matrix-detached-markdown'`); err == nil {
		t.Fatal("completion route was mutable")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE external_agent_job_notifications
		SET notification_sha256 = ? WHERE job_id = 'matrix-detached-markdown'`, strings.Repeat("0", 64)); err == nil {
		t.Fatal("notification identity was mutable")
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO external_agent_job_notifications (
		job_id, status_revision, kind, canonical_markdown, renderer_version, channel_id, next_attempt_at, created_at, updated_at, root_activation_required)
		VALUES ('matrix-detached-markdown', 2, 'terminal', 'x', 'markdown_v1', 'D12345678', 1, 1, 1, 2)`); err == nil {
		t.Fatal("non-boolean completion route was accepted")
	}
}

func uploadStateForPolicy(policy, deliveryMode string) string {
	if policy == "delivery_v1" && deliveryMode == "file" {
		return "completed"
	}
	return "not_applicable"
}

// TestMigrationV32ScopesIdentityBackfillToCompatibleSnapshots covers the CR2
// multi-revision matrix: after a completion_unknown -> completed
// reconciliation the job row carries only the newer revision's result
// identity, so an older terminal notification must never receive it. A
// notification that snapshots the job's current terminal event keeps its
// identity, and the result identity pair is always written atomically.
func TestMigrationV32ScopesIdentityBackfillToCompatibleSnapshots(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 30)

	insertMultiRevJob := func(id, mode, status string, revision int, resultSHA string, resultBytes int64) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, `INSERT INTO external_agent_jobs (
			job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
			task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
			conversation_key, status, result_sha256, result_bytes, status_revision, timeout_at, created_at, updated_at)
			VALUES (?, ?, 'agentcli', 'build', 'workspace', '[]', 'r1',
			'task', 'request', 'wrapper', ?, 'U12345678', 'T12345678',
			'slack:T12345678:dm:D12345678', ?, ?, ?, ?, 2, 1, 1)`,
			id, mode, id+"-call", status, resultSHA, resultBytes, revision); err != nil {
			t.Fatal(err)
		}
	}
	insertMultiRevNotification := func(id string, revision int, terminalStatus, markdown, contentSHA string, policy, deliveryMode string, resultBytes int64) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, `INSERT INTO external_agent_job_notifications (
			job_id, status_revision, kind, terminal_status, canonical_markdown, content_sha256,
			renderer_version, channel_id, next_attempt_at, created_at, updated_at,
			delivery_mode, policy_version, artifact_ref, result_bytes, max_markdown_parts, upload_state, published_at,
			publish_state, recovered_slack_ts)
			VALUES (?, ?, 'terminal', ?, ?, ?, 'markdown_v1', 'D12345678', 1, 1, 1, ?, ?, '', ?, 6, ?, 0,
			'published', '1710000000.000003')`,
			id, revision, terminalStatus, markdown, contentSHA, deliveryMode, policy, resultBytes, uploadStateForPolicy(policy, deliveryMode)); err != nil {
			t.Fatal(err)
		}
	}

	newContent := "current result"
	newDigest := contentSHA256Hex(newContent)
	newMarkdown := "External-agent job `legacy-old` completed.\n\ncurrent result"

	// legacy-old: the job was reconciled to revision 2, so the row carries
	// only the completed result identity of the newer event. The R1
	// completion_unknown notification predates the reconciliation and must
	// not receive the R2 identity. Its legacy row carries stale positive
	// delivery bytes that the backfill must clear together with the empty
	// digest (atomic pair).
	insertMultiRevJob("legacy-old", "detached", "completed", 2, newDigest, int64(len(newContent)))
	oldMarkdown := "External-agent job `legacy-old` was interrupted after external actions may have occurred. It was not retried; reconciliation is required."
	insertMultiRevNotification("legacy-old", 1, "completion_unknown", oldMarkdown, contentSHA256Hex(oldMarkdown), "legacy_v1", "markdown", 17)

	// legacy-current: a legacy terminal notification that snapshots the
	// job's current event does receive the mirrored job identity.
	insertMultiRevJob("legacy-current", "detached", "completed", 2, newDigest, int64(len(newContent)))
	insertMultiRevNotification("legacy-current", 2, "completed", newMarkdown, contentSHA256Hex(newMarkdown), "legacy_v1", "markdown", 0)

	// delivery-current: a delivery_v1 row keeps its own complete content
	// identity, never the job's later identity.
	insertMultiRevJob("delivery-current", "detached", "completed", 2, newDigest, int64(len(newContent)))
	insertMultiRevNotification("delivery-current", 2, "completed", newMarkdown, newDigest, "delivery_v1", "markdown", int64(len(newContent)))

	// single-current: a job that never reconciled keeps its identity on the
	// legacy terminal notification at the same revision.
	singleContent := "safe result"
	singleDigest := contentSHA256Hex(singleContent)
	singleMarkdown := "External-agent job `single-current` completed.\n\nsafe result"
	insertMultiRevJob("single-current", "detached", "completed", 1, singleDigest, int64(len(singleContent)))
	insertMultiRevNotification("single-current", 1, "completed", singleMarkdown, contentSHA256Hex(singleMarkdown), "legacy_v1", "markdown", 0)

	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("OpenExisting v30: %v", err)
	}
	defer func() { _ = store.Close() }()
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("upgraded schema version = %d, want %d", version, SchemaVersion)
	}

	type expected struct {
		root            int
		notificationSHA string
		resultSHA       string
		resultBytes     int64
	}
	cases := map[string]expected{
		// The R1 completion_unknown notification stays historical: empty
		// identity pair, no R2 digest leak, and its stale positive bytes are
		// cleared.
		"legacy-old":       {1, contentSHA256Hex(oldMarkdown), "", 0},
		"legacy-current":   {1, contentSHA256Hex(newMarkdown), newDigest, int64(len(newContent))},
		"delivery-current": {1, contentSHA256Hex(newMarkdown), newDigest, int64(len(newContent))},
		"single-current":   {1, contentSHA256Hex(singleMarkdown), singleDigest, int64(len(singleContent))},
	}
	for jobID, want := range cases {
		var got expected
		if err := store.db.QueryRowContext(ctx, `SELECT root_activation_required, notification_sha256, result_sha256, result_bytes
			FROM external_agent_job_notifications WHERE job_id = ? AND kind = 'terminal'`, jobID).
			Scan(&got.root, &got.notificationSHA, &got.resultSHA, &got.resultBytes); err != nil {
			t.Fatalf("%s: %v", jobID, err)
		}
		if got != want {
			t.Fatalf("%s backfill = %+v, want %+v", jobID, got, want)
		}
	}
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "local-agent.db")
	store, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store, path
}
