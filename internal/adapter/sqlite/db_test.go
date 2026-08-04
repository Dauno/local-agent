package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
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
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, name)
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
	defer store.Close()
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
	defer store.Close()

	rows, err := store.db.QueryContext(context.Background(), `PRAGMA table_info(recoverable_results)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
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
	defer store.Close()
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
	defer store.Close()
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 30 {
		t.Fatalf("upgraded schema version = %d, want 30", version)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT role, source FROM messages ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
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
	defer store.Close()
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
		raw.Close()
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
		VALUES ('migration-job', 'detached', 'opencode', 'build', 'workspace', '[]', 'r1',
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
	defer rawDB.Close()
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
	defer store.Close()
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

	topic, err := store.GetTopic(ctx, "existing")
	if err != nil {
		t.Fatal(err)
	}
	if topic.BundlePath != "topics" {
		t.Fatalf("migrated topic bundle_path = %q, want topics", topic.BundlePath)
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
