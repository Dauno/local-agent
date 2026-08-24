package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestMigrationV36FreshAndUpgradePreserveSessions(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/fresh-epochs.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `SELECT 1 FROM context_epochs`); err != nil {
		t.Fatalf("fresh context epoch table unavailable: %v", err)
	}
	store.Close()

	path, raw := createSchemaAtVersion(t, 35)
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO adk_sessions (app_name, user_id, session_id, state, create_time, update_time)
		VALUES ('app', 'user', 'session', '{}', 1, 1)`); err != nil {
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
	var count int
	if err := upgraded.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM adk_sessions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("preserved ADK sessions = %d, %v", count, err)
	}
	var version int
	if err := upgraded.DB().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("upgraded version = %d, %v", version, err)
	}
}

func TestMigrationV36CrashRollsBackAndReopens(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 35)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	original := migrations[36]
	defer func() { migrations[36] = original }()
	migrations[36] = func(ctx context.Context, tx *sql.Tx) error {
		if err := migrateV36(ctx, tx); err != nil {
			return err
		}
		return errors.New("injected v36 crash")
	}
	store, err := OpenExisting(t.Context(), path)
	if store != nil {
		store.Close()
		t.Fatal("OpenExisting succeeded after injected v36 crash")
	}
	if err == nil || !strings.Contains(err.Error(), "injected v36 crash") {
		t.Fatalf("OpenExisting error = %v", err)
	}
	check, err := sql.Open("sqlite", mustDataSourceName(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var version, tables int
	if err := check.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'context_epochs'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if version != 35 || tables != 0 {
		t.Fatalf("rolled-back v36 state = version %d/table count %d", version, tables)
	}
	migrations[36] = original
	reopened, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen after v36 crash: %v", err)
	}
	defer reopened.Close()
	if err := reopened.DB().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("reopened version = %d, %v", version, err)
	}
}
