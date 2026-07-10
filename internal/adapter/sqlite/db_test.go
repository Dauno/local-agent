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
