package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMigrationV41FreshObjectsAndConstraints(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/fresh-v41.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := store.DB()

	for _, object := range []string{"recoverable_result_refs", "recoverable_result_refs_by_ref"} {
		var present int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE name = ?`, object).Scan(&present); err != nil || present != 1 {
			t.Fatalf("v41 object %q present = %d, %v", object, present, err)
		}
	}

	now := time.Now().UTC().Unix()
	ref := strings.Repeat("a", 64)
	insert := func(name, statement string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), statement, args...); err == nil {
			t.Errorf("%s: statement unexpectedly succeeded", name)
		}
	}
	insert("short ref", `INSERT INTO recoverable_result_refs (ref, owner_kind, owner_id, created_at) VALUES (?, 'adk_event', 'owner', ?)`, ref[:63], now)
	insert("uppercase ref", `INSERT INTO recoverable_result_refs (ref, owner_kind, owner_id, created_at) VALUES (?, 'adk_event', 'owner', ?)`, strings.ToUpper(ref), now)
	insert("unknown owner kind", `INSERT INTO recoverable_result_refs (ref, owner_kind, owner_id, created_at) VALUES (?, 'workstream', 'owner', ?)`, ref, now)
	insert("empty owner id", `INSERT INTO recoverable_result_refs (ref, owner_kind, owner_id, created_at) VALUES (?, 'adk_event', '', ?)`, ref, now)
	insert("zero created_at", `INSERT INTO recoverable_result_refs (ref, owner_kind, owner_id, created_at) VALUES (?, 'adk_event', 'owner', 0)`, ref)

	if _, err := db.ExecContext(t.Context(), `INSERT INTO recoverable_result_refs (ref, owner_kind, owner_id, created_at) VALUES (?, 'adk_event', 'owner', ?)`, ref, now); err != nil {
		t.Fatalf("valid insert failed: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO recoverable_result_refs (ref, owner_kind, owner_id, created_at) VALUES (?, 'continuity_capsule', 'owner2', ?)`, ref, now); err != nil {
		t.Fatalf("valid capsule-owner insert failed: %v", err)
	}
}

func TestVerifyBackfillCoverage(t *testing.T) {
	if err := verifyBackfillCoverage("adk_events", 10, 10); err != nil {
		t.Fatalf("matching counts unexpectedly failed: %v", err)
	}
	err := verifyBackfillCoverage("adk_events", 9, 10)
	if err == nil || !strings.Contains(err.Error(), "refusing to migrate without proven coverage") {
		t.Fatalf("mismatched counts error = %v, want a fail-closed coverage error", err)
	}
}

// seedV40BackfillFixture builds a hand-constructed v40 database (not one
// created by Initialize, per the repo's FIND-111 lesson: a fixture the code
// under test builds can hide a migration defect) with adk_events and
// continuity_capsules rows that predate recoverable_result_refs, so the v41
// backfill has real work to prove coverage over.
func seedV40BackfillFixture(t *testing.T) (path string, refs struct{ embedded, capsule, notAResult string }) {
	t.Helper()
	path, raw := createSchemaAtVersion(t, 40)
	ctx := t.Context()
	now := time.Now().UTC().Unix()

	refs.embedded = strings.Repeat("b2", 32)
	refs.capsule = strings.Repeat("c3", 32)
	refs.notAResult = strings.Repeat("d4", 32) // a 64-hex string that is never a recoverable_results row

	insert := func(statement string, args ...any) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, statement, args...); err != nil {
			raw.Close()
			t.Fatalf("seed v40 row: %v", err)
		}
	}

	insert(`INSERT INTO recoverable_results (ref, actor, conversation_key, kind, storage_locator, size_bytes, code_points, sha256, created_at, expires_at)
		VALUES (?, 'actor', 'slack:T1:dm:D1', 'test', 'loc-a', 1, 1, ?, ?, ?)`, refs.embedded, strings.Repeat("0", 64), now, now+3600)
	insert(`INSERT INTO recoverable_results (ref, actor, conversation_key, kind, storage_locator, size_bytes, code_points, sha256, created_at, expires_at)
		VALUES (?, 'actor', 'slack:T1:dm:D1', 'test', 'loc-b', 1, 1, ?, ?, ?)`, refs.capsule, strings.Repeat("0", 64), now, now+3600)

	insert(`INSERT INTO adk_sessions (app_name, user_id, session_id, state, revision, create_time, update_time)
		VALUES ('app', 'user', 'sess', '{}', 1, ?, ?)`, now, now)
	// refs.embedded appears only inside a longer hex run; refs.notAResult
	// appears as an exact 64-hex token that is not a recoverable_results
	// row, so it must not be indexed even though instr would find it as a
	// substring were it ever queried.
	content := `{"role":"model","parts":[{"text":"ff` + refs.embedded + `ee and also ` + refs.notAResult + `"}]}`
	insert(`INSERT INTO adk_events (id, app_name, user_id, session_id, ordinal, invocation_id, author, actions, timestamp, content, partial, turn_complete, interrupted)
		VALUES ('evt-1', 'app', 'user', 'sess', 0, 'inv', 'model', '{}', ?, ?, 0, 1, 0)`, now, content)
	// A second event with NULL content proves the backfill still counts it
	// as processed without indexing anything for it.
	insert(`INSERT INTO adk_events (id, app_name, user_id, session_id, ordinal, invocation_id, author, actions, timestamp, partial, turn_complete, interrupted)
		VALUES ('evt-2', 'app', 'user', 'sess', 1, 'inv', 'model', '{}', ?, 0, 1, 0)`, now)

	capsuleJSON := `{"Revision":1,"ActiveResults":[{"Kind":"text","ResultRef":"` + refs.capsule + `","SHA256":"` + strings.Repeat("0", 64) + `"}]}`
	insert(`INSERT INTO continuity_capsules (session_id, revision, capsule_json, source_digest, covered_through, created_at, updated_at)
		VALUES ('sess', 1, ?, '', 0, ?, ?)`, capsuleJSON, now, now)

	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return path, refs
}

func TestMigrationV41UpgradeBackfillsExistingRefs(t *testing.T) {
	path, refs := seedV40BackfillFixture(t)

	upgraded, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	db := upgraded.DB()

	var version int
	if err := db.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("upgraded version = %d, %v", version, err)
	}

	referenced := func(ref string) bool {
		t.Helper()
		var got bool
		if err := db.QueryRowContext(t.Context(), recoverableReferenceCheckQuery, ref).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if !referenced(refs.embedded) {
		t.Fatal("backfill did not index a ref embedded inside a longer hex run")
	}
	if !referenced(refs.capsule) {
		t.Fatal("backfill did not index the capsule ref")
	}
	if referenced(refs.notAResult) {
		t.Fatal("backfill indexed a hex-64 token that is not a recoverable_results ref")
	}

	var eventOwnerRows int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM recoverable_result_refs WHERE owner_kind = 'adk_event'`).Scan(&eventOwnerRows); err != nil {
		t.Fatal(err)
	}
	if eventOwnerRows != 1 {
		t.Fatalf("adk_event-owned index rows = %d, want 1", eventOwnerRows)
	}
}

// TestMigrationV41UpgradeAbortsOnInjectedCoverageMismatch proves the
// fail-closed path end to end: a backfill that under-counts one table must
// abort the whole migration (schema change included), leaving the database
// at v40, exactly like TestMigrationV39CrashRollsBackAndReopens proves for a
// crash. Restoring migrations[41] afterward and reopening proves the
// original migration still succeeds, so the injected variant is the only
// thing that failed.
func TestMigrationV41UpgradeAbortsOnInjectedCoverageMismatch(t *testing.T) {
	path, _ := seedV40BackfillFixture(t)

	original := migrations[41]
	defer func() { migrations[41] = original }()
	migrations[41] = func(ctx context.Context, tx *sql.Tx) error {
		if err := execMigration(ctx, tx, 41, []string{
			`CREATE TABLE recoverable_result_refs (
				ref TEXT NOT NULL,
				owner_kind TEXT NOT NULL,
				owner_id TEXT NOT NULL,
				created_at INTEGER NOT NULL,
				PRIMARY KEY (owner_kind, owner_id, ref),
				CHECK (length(ref) = 64 AND ref NOT GLOB '*[^0-9a-f]*'),
				CHECK (owner_kind IN ('adk_event', 'continuity_capsule')),
				CHECK (length(owner_id) > 0),
				CHECK (created_at > 0)
			) WITHOUT ROWID`,
			`CREATE INDEX recoverable_result_refs_by_ref ON recoverable_result_refs (ref)`,
		}); err != nil {
			return err
		}
		// Injected defect: claim one fewer adk_events row was processed
		// than the table actually has, simulating a backfill that silently
		// dropped a row.
		return verifyBackfillCoverage("adk_events", 0, 2)
	}

	store, err := OpenExisting(t.Context(), path)
	if store != nil {
		store.Close()
		t.Fatal("OpenExisting succeeded despite an injected coverage mismatch")
	}
	if err == nil || !strings.Contains(err.Error(), "refusing to migrate without proven coverage") {
		t.Fatalf("OpenExisting error = %v, want a fail-closed coverage error", err)
	}

	check, err := sql.Open("sqlite", mustDataSourceName(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var version, objects int
	if err := check.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE name = 'recoverable_result_refs'`).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if version != 40 || objects != 0 {
		t.Fatalf("rolled-back v41 state = version %d/objects %d, want version 40/objects 0", version, objects)
	}

	migrations[41] = original
	reopened, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen with the real migration after the injected failure: %v", err)
	}
	defer reopened.Close()
	if err := reopened.DB().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("reopened version = %d, %v", version, err)
	}
}

// TestOpenExistingUpgradesV40ToV41WithoutStateReset covers criterion 9:
// migrate.go's upgrade whitelist must accept a v40 database in place
// instead of returning StateResetNeededError, which would otherwise force a
// destructive reset on every real v40 deployment.
func TestOpenExistingUpgradesV40ToV41WithoutStateReset(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 40)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(t.Context(), path)
	if err != nil {
		if errors.Is(err, ErrStateResetNeeded) {
			t.Fatalf("OpenExisting returned ErrStateResetNeeded for a v40 database: %v", err)
		}
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err := store.DB().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("upgraded version = %d, %v", version, err)
	}
}
