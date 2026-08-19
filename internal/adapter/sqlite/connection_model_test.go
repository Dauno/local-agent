package sqlite

// TRD 08 checkpoint 1 (docs/root-orchestrator-v2/08-sqlite-runtime-scaling-and-indexing-trd.md,
// DEC-08-1 and DEC-08-2). Every gate here asserts a value or a count, never a
// duration, per DEC-08-5.

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestConnectionModelPragmasAndPoolSize covers gate 1 and gate 2: a freshly
// created database opens in WAL, synchronous stays FULL, and the pool is
// sized to DEC-08-2's four connections.
func TestConnectionModelPragmasAndPoolSize(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var journalMode string
	if err := store.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	var synchronous int
	if err := store.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	const synchronousFull = 2
	if synchronous != synchronousFull {
		t.Fatalf("synchronous = %d, want %d (FULL)", synchronous, synchronousFull)
	}

	stats := store.db.Stats()
	if stats.MaxOpenConnections != 4 {
		t.Fatalf("MaxOpenConnections = %d, want 4", stats.MaxOpenConnections)
	}
}

// TestOpenExistingUpgradesLegacyRollbackJournalDatabaseToWAL covers gate 1's
// second required case: a database created under the rollback journal, the
// deployed configuration before this checkpoint, must move to WAL the next
// time it is opened through the production path.
func TestOpenExistingUpgradesLegacyRollbackJournalDatabaseToWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacyDSN := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(delete)",
		filepath.ToSlash(mustAbs(t, path)),
	)
	raw, err := sql.Open("sqlite", legacyDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	var legacyJournal string
	if err := raw.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&legacyJournal); err != nil {
		t.Fatal(err)
	}
	if legacyJournal != "delete" {
		t.Fatalf("seed journal_mode = %q, want delete (fixture did not reproduce the deployed configuration)", legacyJournal)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("OpenExisting on legacy rollback-journal database: %v", err)
	}
	defer store.Close()

	var journalMode string
	if err := store.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode after OpenExisting = %q, want wal", journalMode)
	}
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// legacyRollbackJournalDB builds a database in rollback-journal mode by hand,
// never through Initialize or OpenExisting. Every database created before
// checkpoint 1 is in this mode, including the deployment. A fixture built with
// Initialize would already be in WAL, which would hide FIND-111: setting
// journal_mode(wal) in a read-only DSN is a no-op on a database that is
// already WAL, and only fails on one that is not.
func legacyRollbackJournalDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rwc&_pragma=journal_mode(delete)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "delete" {
		t.Fatalf("fixture journal_mode = %q, want delete", mode)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestOpenReadOnlyOnRollbackJournalDatabaseOpensAndDoesNotChangeJournalMode
// covers FIND-111 and acceptance criteria 1 and 3 of the repair round:
// OpenReadOnly must open and read a database still in rollback-journal mode
// (the deployed configuration, and every database not yet opened for write
// under checkpoint 1), and a read-only open must not change that database's
// journal_mode. journal_mode is a database-level property; a read-only
// connection cannot write it, so it must not be requested on that DSN.
func TestOpenReadOnlyOnRollbackJournalDatabaseOpensAndDoesNotChangeJournalMode(t *testing.T) {
	ctx := context.Background()
	path := legacyRollbackJournalDB(t)

	store, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly on a rollback-journal database: %v", err)
	}
	defer store.Close()

	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&count); err != nil {
		t.Fatalf("read through OpenReadOnly: %v", err)
	}

	var journalMode string
	if err := store.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "delete" {
		t.Fatalf("journal_mode after OpenReadOnly = %q, want delete (a read-only open must not change it)", journalMode)
	}
}

// TestReadDuringOpenWriteTransactionSeesPreWriteSnapshot covers gate 3: under
// this checkpoint's production WAL configuration, a read issued while a write
// transaction is open, uncommitted, returns the value from before that write,
// not the pending one. The assertion is on the value read, never on how long
// the read took. This is a WAL snapshot-isolation check, not a comparison
// against rollback-journal behavior: an uncommitted write does not block a
// concurrent reader under the rollback journal either, so no value-based
// assertion here separates the two journal modes. WAL adoption itself is
// covered by the journal_mode gates above.
func TestReadDuringOpenWriteTransactionSeesPreWriteSnapshot(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.db.ExecContext(ctx, `CREATE TABLE checkpoint1_probe (id INTEGER PRIMARY KEY, value INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO checkpoint1_probe (id, value) VALUES (1, 1)`); err != nil {
		t.Fatal(err)
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE checkpoint1_probe SET value = 2 WHERE id = 1`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	// The write above is uncommitted. A read on a different pooled connection
	// must still see the pre-write value.
	var duringWrite int
	if err := store.db.QueryRowContext(ctx, `SELECT value FROM checkpoint1_probe WHERE id = 1`).Scan(&duringWrite); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if duringWrite != 1 {
		_ = tx.Rollback()
		t.Fatalf("read during open write transaction = %d, want 1 (the pre-write snapshot)", duringWrite)
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var afterCommit int
	if err := store.db.QueryRowContext(ctx, `SELECT value FROM checkpoint1_probe WHERE id = 1`).Scan(&afterCommit); err != nil {
		t.Fatal(err)
	}
	if afterCommit != 2 {
		t.Fatalf("read after commit = %d, want 2", afterCommit)
	}
}

// TestConcurrentReadThenWriteTransactionsAllCompleteUnderTxlockImmediate
// covers gate 4: with a pool larger than one connection, concurrent
// transactions that read a value and then write it back must all complete,
// with no lost update and no SQLITE_BUSY. This is exactly the AppendEvent
// shape (DEC-08-2 addendum) and is the case _txlock=immediate exists for.
//
// This test was run with _txlock=immediate removed from dataSourceName as a
// sabotage check: with 40 concurrent writers it failed with SQLITE_BUSY (or a
// silently lost update) on every run. Restoring _txlock=immediate is required
// for this test to pass, and the fix was restored before this file was
// delivered.
func TestConcurrentReadThenWriteTransactionsAllCompleteUnderTxlockImmediate(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.db.ExecContext(ctx, `CREATE TABLE checkpoint1_counter (id INTEGER PRIMARY KEY, value INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO checkpoint1_counter (id, value) VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}

	const writers = 40
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = readThenWriteIncrement(ctx, store.db)
		}(i)
	}
	wg.Wait()

	var failures []error
	for _, err := range errs {
		if err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		t.Fatalf("%d of %d read-then-write transactions failed, want 0: first error: %v", len(failures), writers, failures[0])
	}

	var final int
	if err := store.db.QueryRowContext(ctx, `SELECT value FROM checkpoint1_counter WHERE id = 1`).Scan(&final); err != nil {
		t.Fatal(err)
	}
	if final != writers {
		t.Fatalf("final counter = %d, want %d (a lost update means a read-then-write transaction ran on a stale snapshot)", final, writers)
	}
}

// readThenWriteIncrement has the AppendEvent shape: begin a transaction, read
// the current value, then write a value derived from that read.
func readThenWriteIncrement(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var current int
	if err := tx.QueryRowContext(ctx, `SELECT value FROM checkpoint1_counter WHERE id = 1`).Scan(&current); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE checkpoint1_counter SET value = ? WHERE id = 1`, current+1); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
