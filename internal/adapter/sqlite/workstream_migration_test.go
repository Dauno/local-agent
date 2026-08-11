package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestMigrationV33RollbackLeavesV32DatabaseRetryable(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 32)
	if _, err := raw.ExecContext(ctx, `INSERT INTO conversations (
		conversation_key, team_id, channel_id, channel_kind, root_ts, last_ts, created_at, updated_at)
		VALUES ('slack:T12345678:dm:D12345678', 'T12345678', 'D12345678', 'dm', '', '1', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	original := migrations[33]
	migrations[33] = func(ctx context.Context, tx *sql.Tx) error {
		if err := migrateV33(ctx, tx); err != nil {
			return err
		}
		return errors.New("injected v33 failure")
	}
	defer func() { migrations[33] = original }()

	store, err := OpenExisting(ctx, path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting succeeded after injected v33 failure")
	}
	if err == nil || !strings.Contains(err.Error(), "injected v33 failure") {
		t.Fatalf("OpenExisting error = %v, want injected v33 failure", err)
	}

	raw, err = sql.Open("sqlite", mustDataSourceName(t, path))
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := raw.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if version != 32 {
		_ = raw.Close()
		t.Fatalf("schema version after rollback = %d, want 32", version)
	}
	var tableCount int
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'workstreams'`).Scan(&tableCount); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if tableCount != 0 {
		_ = raw.Close()
		t.Fatal("workstream tables survived failed migration")
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrations[33] = original
	store, err = OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("OpenExisting retry: %v", err)
	}
	defer store.Close()
	if err := store.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version after retry = %d, want %d", version, SchemaVersion)
	}
	var conversationCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE conversation_key = 'slack:T12345678:dm:D12345678'`).Scan(&conversationCount); err != nil {
		t.Fatal(err)
	}
	if conversationCount != 1 {
		t.Fatalf("pre-v33 conversation count = %d, want 1", conversationCount)
	}

}
