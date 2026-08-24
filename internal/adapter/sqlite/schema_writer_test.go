package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

const writerSHA1 = "1111111111111111111111111111111111111111111111111111111111111111"
const writerSHA2 = "2222222222222222222222222222222222222222222222222222222222222222"

func backupIdentityFor(path, sha string) rollout.BackupIdentity {
	return rollout.BackupIdentity{
		Path:          path,
		Bytes:         4096,
		SHA256:        sha,
		SourceVersion: 33,
		VerifiedAt:    time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC),
	}
}

func runtimeStateValue(t *testing.T, db *sql.DB, key string) string {
	t.Helper()
	var value string
	if err := db.QueryRowContext(context.Background(),
		`SELECT state_value FROM runtime_state WHERE state_key = ?`, key).Scan(&value); err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return value
}

func TestRecordBaselineAndCutoffConflictSemantics(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 33)
	defer func() { _ = raw.Close() }()
	writer := FileSchemaWriter{}

	first := backupIdentityFor("/tmp/first.db", writerSHA1)
	if err := writer.RecordBaselineAndCutoff(ctx, path, rollout.IdentityBaseline{JobsCompletedWithoutResultIdentity: 5, ActivationsWithoutContent: 6}, 111, first); err != nil {
		t.Fatalf("first write: %v", err)
	}

	var cutoffUpdatedAt int64
	if err := raw.QueryRowContext(ctx,
		`SELECT updated_at FROM runtime_state WHERE state_key = ?`, rollout.KeyCutoff).Scan(&cutoffUpdatedAt); err != nil {
		t.Fatal(err)
	}

	second := backupIdentityFor("/tmp/second.db", writerSHA2)
	if err := writer.RecordBaselineAndCutoff(ctx, path, rollout.IdentityBaseline{JobsCompletedWithoutResultIdentity: 9, ActivationsWithoutContent: 9}, 222, second); err != nil {
		t.Fatalf("second write: %v", err)
	}

	if got := runtimeStateValue(t, raw, rollout.KeyBaseline); got != "jobs=5;activations=6" {
		t.Fatalf("baseline changed on rewrite: %q, want DO NOTHING semantics", got)
	}
	if got := runtimeStateValue(t, raw, rollout.KeyCutoff); got != "111" {
		t.Fatalf("cutoff changed on rewrite: %q, want immutable cutoff", got)
	}
	var afterUpdatedAt int64
	if err := raw.QueryRowContext(ctx,
		`SELECT updated_at FROM runtime_state WHERE state_key = ?`, rollout.KeyCutoff).Scan(&afterUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if afterUpdatedAt != cutoffUpdatedAt {
		t.Fatalf("cutoff updated_at moved from %d to %d on a no-op rewrite", cutoffUpdatedAt, afterUpdatedAt)
	}
	if got := runtimeStateValue(t, raw, rollout.KeyBackupPath); got != "/tmp/second.db" {
		t.Fatalf("backup path = %q, want DO UPDATE semantics", got)
	}
	if got := runtimeStateValue(t, raw, rollout.KeyBackupSHA256); got != writerSHA2 {
		t.Fatalf("backup sha = %q, want overwritten identity", got)
	}
}

func TestFileSchemaWriterMigrateRunsChainToCurrent(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 33)
	defer func() { _ = raw.Close() }()
	migrateErr := (FileSchemaWriter{}).Migrate(ctx, path)
	if migrateErr != nil {
		t.Fatalf("Migrate: %v", migrateErr)
	}
	var version int
	if err := raw.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("user_version=%d err=%v, want %d", version, err, SchemaVersion)
	}
}

func installPostflightFaultTrigger(t *testing.T, db interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}) func() {
	t.Helper()
	trigger := `CREATE TRIGGER injected_postflight_fault BEFORE INSERT ON runtime_state
		WHEN NEW.state_key = '` + rollout.KeyPostflightDetail + `'
		BEGIN SELECT RAISE(ABORT, 'injected postflight failure'); END`
	if _, err := db.ExecContext(context.Background(), trigger); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := db.ExecContext(context.Background(), `DROP TRIGGER injected_postflight_fault`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecordPostflightAtomicityUnderInjectedFailure(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, rollout.TargetVersion)
	defer func() { _ = raw.Close() }()
	writer := FileSchemaWriter{}
	baseline := rollout.IdentityBaseline{JobsCompletedWithoutResultIdentity: 2, ActivationsWithoutContent: 3}
	if err := writer.RecordBaselineAndCutoff(ctx, path, baseline, 7, backupIdentityFor("/tmp/resume.db", writerSHA1)); err != nil {
		t.Fatalf("seed rollout keys: %v", err)
	}
	if err := writer.RecordPostflight(ctx, path, rollout.PostflightPassed, "prior detail"); err != nil {
		t.Fatalf("prior postflight: %v", err)
	}

	restore := installPostflightFaultTrigger(t, raw)
	faultErr := writer.RecordPostflight(ctx, path, rollout.PostflightFailed, "regression detail")
	if faultErr == nil || !strings.Contains(faultErr.Error(), "injected postflight failure") {
		t.Fatalf("err = %v, want the injected second-upsert failure", faultErr)
	}
	restore()

	// Both keys must have rolled back to their previous values: never one
	// new and one old.
	if got := runtimeStateValue(t, raw, rollout.KeyPostflightStatus); got != "passed" {
		t.Fatalf("status = %q, want previous value preserved by the rollback", got)
	}
	if got := runtimeStateValue(t, raw, rollout.KeyPostflightDetail); got != "prior detail" {
		t.Fatalf("detail = %q, want previous value preserved by the rollback", got)
	}

	// A retry with the fault disabled writes both keys and classifies row 5.
	if err := writer.RecordPostflight(ctx, path, rollout.PostflightPassed, "recovered"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	state, err := FileSchemaProbe{}.ReadRolloutState(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	row, classifyErr := rollout.ClassifyRollout(rollout.TargetVersion, state)
	if classifyErr != nil || row != rollout.RolloutRowAlreadyComplete {
		t.Fatalf("row=%d err=%v, want AlreadyComplete after recovery", row, classifyErr)
	}
}

func TestRecordPostflightAtomicityFromAbsentKeys(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, rollout.TargetVersion)
	defer func() { _ = raw.Close() }()
	writer := FileSchemaWriter{}

	restore := installPostflightFaultTrigger(t, raw)
	err := writer.RecordPostflight(ctx, path, rollout.PostflightPassed, "detail")
	if err == nil {
		t.Fatal("injected failure must abort the whole postflight write")
	}
	restore()

	for _, key := range []string{rollout.KeyPostflightStatus, rollout.KeyPostflightDetail} {
		var count int
		scanErr := raw.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM runtime_state WHERE state_key = ?`, key).Scan(&count)
		if scanErr != nil {
			t.Fatal(scanErr)
		}
		if count != 0 {
			t.Fatalf("%s survived an aborted transaction (%d rows), want neither key durable", key, count)
		}
	}
	if retryErr := writer.RecordPostflight(ctx, path, rollout.PostflightPassed, "detail"); retryErr != nil {
		t.Fatalf("retry without fault: %v", retryErr)
	}
}
