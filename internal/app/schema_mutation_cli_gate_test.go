//go:build unix

package app_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	sysunix "golang.org/x/sys/unix"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/app"
	"github.com/Dauno/slack-local-agent/internal/cli"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

const schemaBehindText = "database schema is behind this binary's v42; run local-agent db upgrade first"

const mutationHeldText = "another local-agent process is using the database; wait for it to finish"

// rewindToV33Delete rewinds a fully migrated database to header v33 with an
// on-disk journal_mode=delete, mirroring the known deployment, and returns
// the exact byte content. The adoption-at-creation keys Create writes since
// checkpoint 4 are stripped: the known pre-rollout deployment has none.
func rewindToV33Delete(t *testing.T, dbPath string) string {
	t.Helper()
	plain, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Exec("DELETE FROM runtime_state"); err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Exec("PRAGMA journal_mode = delete"); err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Exec("PRAGMA user_version = 33"); err != nil {
		t.Fatal(err)
	}
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func assertDigestUnchanged(t *testing.T, dbPath, before string) {
	t.Helper()
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != before {
		t.Fatalf("fixture bytes changed: before=%s after=%s", before, got)
	}
}

func assertJournalStillDelete(t *testing.T, dbPath string) {
	t.Helper()
	plain, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	var mode string
	if err := plain.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
		t.Fatalf("journal_mode=%q err=%v, want delete untouched", mode, err)
	}
}

// holdRealFlock pre-acquires the database's sibling lock file through an
// independently opened descriptor: exactly what cross-process contention
// looks like to the locker.
func holdRealFlock(t *testing.T, dbPath string) func() {
	t.Helper()
	fd, err := sysunix.Open(dbPath+".lock", sysunix.O_RDWR|sysunix.O_CREAT|sysunix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := sysunix.Flock(fd, sysunix.LOCK_EX|sysunix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return func() {
		_ = sysunix.Flock(fd, sysunix.LOCK_UN)
		_ = sysunix.Close(fd)
	}
}

func executeCLI(t *testing.T, application *app.Application, args []string, stdin string) (int, string) {
	t.Helper()
	var output, stderr strings.Builder
	root, err := cli.NewRoot(application, cli.Streams{In: strings.NewReader(stdin), Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	code := cli.Execute(context.Background(), root, args, &stderr)
	return code, stderr.String()
}

// TestRunGateBehindSchemaIsFatalWithoutTouchingBytes pins the run call
// site: lock first, OpenCurrent rejection, exit 1 with the shared text,
// byte identity, and journal_mode=delete untouched. Byte identity comes
// before the status assertions so a write-capable site cannot hide behind
// its own error text.
func TestRunGateBehindSchemaIsFatalWithoutTouchingBytes(t *testing.T) {
	application, paths := setupInitializedProject(t)
	before := rewindToV33Delete(t, paths.DatabaseFile)

	code, stderr := executeCLI(t, application, []string{"run"}, "")
	assertDigestUnchanged(t, paths.DatabaseFile, before)
	assertJournalStillDelete(t, paths.DatabaseFile)
	if code != 1 {
		t.Fatalf("run exit=%d, want 1", code)
	}
	if !strings.Contains(stderr, schemaBehindText) {
		t.Fatalf("stderr = %q, want %q", stderr, schemaBehindText)
	}
}

// TestInitNormalGateBehindSchemaNeverMigratesOrCreates pins the initializer
// call site: an existing outdated database reports the upgrade requirement;
// it never reaches Create or the migrating opener.
func TestInitNormalGateBehindSchemaNeverMigratesOrCreates(t *testing.T) {
	application, paths := setupInitializedProject(t)
	before := rewindToV33Delete(t, paths.DatabaseFile)

	const wizardStdin = "\n\n\nxoxb-token\nxapp-token\nU12345678\n\n\n\n\nmodel-key\ny\n"
	code, stderr := executeCLI(t, application, []string{"init"}, wizardStdin)
	assertDigestUnchanged(t, paths.DatabaseFile, before)
	assertJournalStillDelete(t, paths.DatabaseFile)
	if code != 1 {
		t.Fatalf("init exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, schemaBehindText) {
		t.Fatalf("stderr = %q, want %q", stderr, schemaBehindText)
	}
}

// TestReconcileGateBehindSchemaIsFatalWithoutTouchingBytes pins the jobs
// reconcile call site.
func TestReconcileGateBehindSchemaIsFatalWithoutTouchingBytes(t *testing.T) {
	application, paths := setupInitializedProject(t)
	before := rewindToV33Delete(t, paths.DatabaseFile)

	_, err := application.ReconcileJob(context.Background(), "missing-job", 0)
	assertDigestUnchanged(t, paths.DatabaseFile, before)
	assertJournalStillDelete(t, paths.DatabaseFile)
	if err == nil || !strings.Contains(err.Error(), schemaBehindText) {
		t.Fatalf("err = %v, want %q", err, schemaBehindText)
	}
}

// TestKnowledgeRebuildGateBehindSchemaIsFatalWithoutTouchingBytes pins the
// knowledge rebuild-index call site.
func TestKnowledgeRebuildGateBehindSchemaIsFatalWithoutTouchingBytes(t *testing.T) {
	application, paths := setupInitializedProject(t)
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Orchestration.Knowledge.Enabled = true
	if err := config.Save(paths.ConfigFile, cfg); err != nil {
		t.Fatal(err)
	}
	before := rewindToV33Delete(t, paths.DatabaseFile)

	code, stderr := executeCLI(t, application, []string{"knowledge", "rebuild-index"}, "")
	assertDigestUnchanged(t, paths.DatabaseFile, before)
	assertJournalStillDelete(t, paths.DatabaseFile)
	if code != 1 {
		t.Fatalf("rebuild-index exit=%d, want 1", code)
	}
	if !strings.Contains(stderr, schemaBehindText) {
		t.Fatalf("stderr = %q, want %q", stderr, schemaBehindText)
	}
}

// TestResetStateUnderContentionChangesNothing pins the destructive flow's
// lock-first guarantee: with the lock held elsewhere, reset fails fast and
// the database survives byte-for-byte.
func TestResetStateUnderContentionChangesNothing(t *testing.T) {
	application, paths := setupInitializedProject(t)
	releaseLock := holdRealFlock(t, paths.DatabaseFile)
	defer releaseLock()

	before := rewindToV33Delete(t, paths.DatabaseFile)
	code, stderr := executeCLI(t, application, []string{"init", "--reset-state"}, "y\n")
	assertDigestUnchanged(t, paths.DatabaseFile, before)
	assertJournalStillDelete(t, paths.DatabaseFile)
	if code != 1 {
		t.Fatalf("reset exit=%d, want 1 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, mutationHeldText) {
		t.Fatalf("stderr = %q, want %q", stderr, mutationHeldText)
	}
}

// TestResetStateConfirmedReplacesBehindDatabase proves the destructive path
// may replace an outdated database outright once the lock is held: the
// result is a current-schema store.
func TestResetStateConfirmedReplacesBehindDatabase(t *testing.T) {
	application, paths := setupInitializedProject(t)
	rewindToV33Delete(t, paths.DatabaseFile)

	code, stderr := executeCLI(t, application, []string{"init", "--reset-state"}, "y\n")
	if code != 0 {
		t.Fatalf("reset exit=%d, want 0 (stderr=%s)", code, stderr)
	}
	var version int
	plain, err := sql.Open("sqlite", paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	if err := plain.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 42 {
		t.Fatalf("post-reset user_version=%d err=%v, want 42", version, err)
	}
}

// TestRunOrderGateHeldBeatsBehind proves the lock is acquired before any
// database open: with the real flock held elsewhere and a v33 fixture, a
// correct implementation reports contention. An implementation that opened
// first would answer the schema text instead, because OpenCurrent's
// read-only probe rejects without touching disk.
func TestRunOrderGateHeldBeatsBehind(t *testing.T) {
	application, paths := setupInitializedProject(t)
	releaseLock := holdRealFlock(t, paths.DatabaseFile)
	defer releaseLock()
	before := rewindToV33Delete(t, paths.DatabaseFile)

	code, stderr := executeCLI(t, application, []string{"run"}, "")
	assertDigestUnchanged(t, paths.DatabaseFile, before)
	assertJournalStillDelete(t, paths.DatabaseFile)
	if code != 1 || !strings.Contains(stderr, mutationHeldText) {
		t.Fatalf("run exit=%d stderr=%q, want held-contended failure", code, stderr)
	}
	if strings.Contains(stderr, schemaBehindText) {
		t.Fatalf("run answered the schema probe before acquiring the lock: %q", stderr)
	}
}

func TestInitOrderGateHeldBeatsBehind(t *testing.T) {
	application, paths := setupInitializedProject(t)
	releaseLock := holdRealFlock(t, paths.DatabaseFile)
	defer releaseLock()
	before := rewindToV33Delete(t, paths.DatabaseFile)

	const wizardStdin = "\n\n\nxoxb-token\nxapp-token\nU12345678\n\n\n\n\nmodel-key\ny\n"
	code, stderr := executeCLI(t, application, []string{"init"}, wizardStdin)
	assertDigestUnchanged(t, paths.DatabaseFile, before)
	assertJournalStillDelete(t, paths.DatabaseFile)
	if code != 1 || !strings.Contains(stderr, mutationHeldText) {
		t.Fatalf("init exit=%d stderr=%q, want held-contended failure", code, stderr)
	}
	if strings.Contains(stderr, schemaBehindText) {
		t.Fatalf("init answered the schema probe before acquiring the lock: %q", stderr)
	}
}

// TestReconcileOrderGateHeldBeatsBehind is the same sequence proof for the
// jobs reconcile call site, asserted on the typed error chain.
func TestReconcileOrderGateHeldBeatsBehind(t *testing.T) {
	application, paths := setupInitializedProject(t)
	releaseLock := holdRealFlock(t, paths.DatabaseFile)
	defer releaseLock()
	before := rewindToV33Delete(t, paths.DatabaseFile)

	_, err := application.ReconcileJob(context.Background(), "missing-job", 0)
	assertDigestUnchanged(t, paths.DatabaseFile, before)
	assertJournalStillDelete(t, paths.DatabaseFile)
	if !errors.Is(err, rollout.ErrMutationLockHeld) {
		t.Fatalf("err = %v, want ErrMutationLockHeld", err)
	}
	if errors.Is(err, adaptersqlite.ErrSchemaUpgradeRequired) || strings.Contains(err.Error(), schemaBehindText) {
		t.Fatalf("reconcile answered the schema probe before acquiring the lock: %v", err)
	}
}

func TestKnowledgeRebuildOrderGateHeldBeatsBehind(t *testing.T) {
	application, paths := setupInitializedProject(t)
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Orchestration.Knowledge.Enabled = true
	if err := config.Save(paths.ConfigFile, cfg); err != nil {
		t.Fatal(err)
	}
	releaseLock := holdRealFlock(t, paths.DatabaseFile)
	defer releaseLock()
	before := rewindToV33Delete(t, paths.DatabaseFile)

	code, stderr := executeCLI(t, application, []string{"knowledge", "rebuild-index"}, "")
	assertDigestUnchanged(t, paths.DatabaseFile, before)
	assertJournalStillDelete(t, paths.DatabaseFile)
	if code != 1 || !strings.Contains(stderr, mutationHeldText) {
		t.Fatalf("rebuild-index exit=%d stderr=%q, want held-contended failure", code, stderr)
	}
	if strings.Contains(stderr, schemaBehindText) {
		t.Fatalf("rebuild-index answered the schema probe before acquiring the lock: %q", stderr)
	}
}
