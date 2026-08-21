package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestCheckSQLiteRuntimeReadOnlyDoesNotChangeJournalMode builds a database
// manually at journal_mode=delete (not the release default) and confirms
// the inspection checker reports the mode it actually observes, opened
// read-only, without changing it (TRD 08 checkpoint 6, item 2).
func TestCheckSQLiteRuntimeReadOnlyDoesNotChangeJournalMode(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "delete-mode.db")

	rw, err := Create(ctx, path)
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if _, err := rw.DB().ExecContext(ctx, `PRAGMA journal_mode=delete`); err != nil {
		t.Fatalf("set journal_mode=delete: %v", err)
	}
	var confirmedMode string
	if err := rw.DB().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&confirmedMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if confirmedMode != "delete" {
		t.Fatalf("fixture journal_mode = %q, want delete", confirmedMode)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close fixture writer: %v", err)
	}

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly() = %v", err)
	}
	defer ro.Close()

	health, err := ro.CheckSQLiteRuntime(ctx)
	if err != nil {
		t.Fatalf("CheckSQLiteRuntime() = %v", err)
	}
	if health.JournalMode != "delete" {
		t.Fatalf("CheckSQLiteRuntime().JournalMode = %q, want delete", health.JournalMode)
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("close read-only inspection: %v", err)
	}

	verify, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatalf("open verification connection: %v", err)
	}
	defer verify.Close()
	var finalMode string
	if err := verify.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&finalMode); err != nil {
		t.Fatalf("read final journal_mode: %v", err)
	}
	if finalMode != "delete" {
		t.Fatalf("journal_mode after inspection = %q, want delete (inspection must not change it)", finalMode)
	}
}

// TestCheckSQLiteRuntimeReportsPoolAndPragmas confirms the checker reports
// the release's actual v41 contract values on a normally created database.
func TestCheckSQLiteRuntimeReportsPoolAndPragmas(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Create(ctx, path)
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	defer store.Close()

	health, err := store.CheckSQLiteRuntime(ctx)
	if err != nil {
		t.Fatalf("CheckSQLiteRuntime() = %v", err)
	}
	if health.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", health.SchemaVersion, SchemaVersion)
	}
	if health.JournalMode != "wal" {
		t.Fatalf("JournalMode = %q, want wal", health.JournalMode)
	}
	if health.Synchronous != 2 {
		t.Fatalf("Synchronous = %d, want 2 (FULL)", health.Synchronous)
	}
	if health.BusyTimeoutMillis != 5000 {
		t.Fatalf("BusyTimeoutMillis = %d, want 5000", health.BusyTimeoutMillis)
	}
	if !health.ForeignKeys {
		t.Fatal("ForeignKeys = false, want true")
	}
	if health.MaxOpenConnections != 4 {
		t.Fatalf("MaxOpenConnections = %d, want 4", health.MaxOpenConnections)
	}
}
