package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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
