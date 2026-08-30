package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

// eventLog is the ordered sink shared by the fake locker and the
// Application's own schema-trace seam (FIND-190). It is per-test state, never
// a global.
type eventLog struct{ events []string }

func (l *eventLog) record(event string) { l.events = append(l.events, event) }

func (l *eventLog) joined() string { return strings.Join(l.events, ",") }

type seamLocker struct{ log *eventLog }

func (l seamLocker) AcquireExclusive(databasePath string) (rollout.Lock, error) {
	l.log.record("lock:" + filepath.Base(databasePath))
	return seamLock(l), nil
}

type seamLock struct{ log *eventLog }

func (l seamLock) Release() error {
	l.log.record("unlock")
	return nil
}

// newSeamApplication builds an application over a minimal valid project with
// the recording locker and the trace sink wired together.
func newSeamApplication(t *testing.T) (*Application, *eventLog, string) {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Orchestration.Knowledge.Enabled = true
	if err := config.Save(filepath.Join(stateDir, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(stateDir, "local-agent.db")
	log := &eventLog{}
	application := &Application{
		root:          root,
		logOutput:     &bytes.Buffer{},
		forceShutdown: make(chan struct{}),
		schemaLocker:  seamLocker{log: log},
		schemaTrace:   log.record,
	}
	return application, log, dbPath
}

// writeMinimalDefinitions seeds a valid openai_compatible provider plus the
// root agent so loadRuntimeSetup resolves definitions.
func writeMinimalDefinitions(t *testing.T, stateDir string) {
	t.Helper()
	for _, dir := range []string{"agents", "providers"} {
		if err := os.MkdirAll(filepath.Join(stateDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	provider := `
name: seam-openai
type: openai_compatible
base_url: http://127.0.0.1:9/v1
api_key_env: SEAM_MODEL_KEY
profiles:
  root:
    model: seam-model
    context_window_tokens: 100000
    max_output_tokens: 1024
    token_counter:
      strategy: byte_bound
`
	agent := `
agent_class: LlmAgent
name: root_agent
model: seam-openai/root
global_instruction: policy
instruction: root
`
	if err := os.WriteFile(filepath.Join(stateDir, "providers", "seam-openai.yaml"), []byte(provider), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "agents", "root_agent.yaml"), []byte(agent), 0o600); err != nil {
		t.Fatal(err)
	}
}

// buildBehindFixture replaces dbPath with a real database whose header says
// v33 and whose on-disk journal mode is delete, mirroring the known
// deployment. It returns the exact byte content for identity assertions.
func buildBehindFixture(t *testing.T, dbPath string) string {
	t.Helper()
	store, err := adaptersqlite.Create(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	plain, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Create now records adoption-at-creation rollout keys; the known
	// pre-rollout deployment this fixture simulates carries none, so strip
	// them to keep the behind-schema classification honest.
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

func assertFileDigest(t *testing.T, dbPath, want string) {
	t.Helper()
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("fixture bytes changed: before=%s after=%s", want, got)
	}
}

// assertOrder pins the frozen call-order contract: exactly one lock event,
// then the given database events (open-current and/or create) in order,
// then exactly one unlock.
func assertOrder(t *testing.T, log *eventLog, openEvents string) {
	t.Helper()
	want := "lock:local-agent.db," + openEvents + ",unlock"
	if got := log.joined(); got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// TestSeamRecordsOpenBetweenLockAndUnlock proves the recorded sequence for
// every direct call site: the open or create happens strictly between the
// lock acquisition and its release (FIND-190).
func TestSeamRecordsOpenBetweenLockAndUnlock(t *testing.T) {
	ctx := context.Background()

	application, log, _ := newSeamApplication(t)
	writeMinimalDefinitions(t, filepath.Join(application.root, ".local-agent"))
	if _, _, err := application.PrepareSetup(ctx); err != nil {
		t.Fatalf("PrepareSetup fresh: %v", err)
	}
	// Fresh init runs the preflight (not found), then creates under the same
	// lock.
	assertOrder(t, log, "preflight,open-current,create")

	log.events = nil
	writeMinimalDefinitions(t, filepath.Join(application.root, ".local-agent")) // idempotent rewrite
	if _, err := application.RebuildKnowledgeIndexes(ctx); err != nil {
		t.Fatalf("RebuildKnowledgeIndexes: %v", err)
	}
	assertOrder(t, log, "preflight,open-current")
}

// TestSeamReconcileRecordsOpenBetweenLockAndUnlock covers the jobs reconcile
// site: the preflight and the store open run strictly inside the locked window
// even though the job lookup itself fails.
func TestSeamReconcileRecordsOpenBetweenLockAndUnlock(t *testing.T) {
	application, log, dbPath := newSeamApplication(t)
	writeMinimalDefinitions(t, filepath.Join(application.root, ".local-agent"))
	store, err := adaptersqlite.Create(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = application.ReconcileJob(context.Background(), "missing-job", 0)
	if err == nil || !strings.Contains(err.Error(), "external-agent job was not found") {
		t.Fatalf("err = %v, want missing-job failure after a clean open", err)
	}
	assertOrder(t, log, "preflight,open-current")
}

// TestSeamBehindSchemaRefusesBeforeOpen proves the rejection path never opens
// the database at all: the v33 fixture fails the read-only preflight under the
// lock, so no open-current marker is recorded and the bytes stay untouched.
func TestSeamBehindSchemaRefusesBeforeOpen(t *testing.T) {
	application, log, dbPath := newSeamApplication(t)
	writeMinimalDefinitions(t, filepath.Join(application.root, ".local-agent"))
	before := buildBehindFixture(t, dbPath)

	_, err := application.ReconcileJob(context.Background(), "missing-job", 0)
	if err == nil || !strings.Contains(err.Error(), schemaBehindMessage) {
		t.Fatalf("err = %v, want %q", err, schemaBehindMessage)
	}
	assertOrder(t, log, "preflight")
	assertFileDigest(t, dbPath, before)
}

// TestSeamContentionRecordsNoDatabaseEvents is the direct-event order gate:
// with the locker refusing, no open-current or create marker may appear at
// all, so any implementation that opened before locking fails this
// assertion instead of merely changing the final error text.
func TestSeamContentionRecordsNoDatabaseEvents(t *testing.T) {
	application, log, dbPath := newSeamApplication(t)
	application.schemaLocker = refusingLocker{}
	before := buildBehindFixture(t, dbPath)

	if err := application.ResetState(context.Background()); !errors.Is(err, rollout.ErrMutationLockHeld) {
		t.Fatalf("reset err = %v, want ErrMutationLockHeld", err)
	}
	if _, err := application.RebuildKnowledgeIndexes(context.Background()); !errors.Is(err, rollout.ErrMutationLockHeld) {
		t.Fatalf("rebuild err = %v, want ErrMutationLockHeld", err)
	}
	if _, _, err := application.PrepareSetup(context.Background()); !errors.Is(err, rollout.ErrMutationLockHeld) {
		t.Fatalf("init err = %v, want ErrMutationLockHeld", err)
	}
	if got := log.joined(); got != "" {
		t.Fatalf("database events recorded while the lock was never acquired: %q", got)
	}
	assertFileDigest(t, dbPath, before)
	var mode string
	plain, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	if err := plain.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
		t.Fatalf("journal_mode=%q err=%v, want delete untouched on every refused path", mode, err)
	}
}

// TestSeamResetStateRecordsCreateUnderLock covers the destructive flow's two
// paths: confirmed reset records create between lock and unlock, and the
// nothing-to-reset failure records neither.
func TestSeamResetStateRecordsCreateUnderLock(t *testing.T) {
	application, log, dbPath := newSeamApplication(t)
	buildBehindFixture(t, dbPath)

	if err := application.ResetState(context.Background()); err != nil {
		t.Fatalf("ResetState: %v", err)
	}
	assertOrder(t, log, "create")
	if version := probeUserVersion(t, dbPath); version != 45 {
		t.Fatalf("post-reset user_version = %d, want 45", version)
	}

	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	log.events = nil
	err := application.ResetState(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nothing to reset") {
		t.Fatalf("err = %v, want nothing-to-reset", err)
	}
	if got := log.joined(); got != "lock:local-agent.db,unlock" {
		t.Fatalf("failed-reset events = %q", got)
	}
}

// TestResetStateDeletesConfiguredMemoryDir covers a project whose state.dir
// is not the default ".local-agent": ResetState must delete the memory
// projection at the resolved paths.MemoryDir, and must not create or touch
// <root>/.local-agent/memory.
func TestResetStateDeletesConfiguredMemoryDir(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, ".local-agent")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Orchestration.Knowledge.Enabled = true
	cfg.State.Dir = "var/state"
	cfg.State.DB = "var/state/local-agent.db"
	if err := config.Save(filepath.Join(configDir, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "var", "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(stateDir, "local-agent.db")
	buildBehindFixture(t, dbPath)

	configuredMemoryDir := filepath.Join(stateDir, "memory")
	if err := os.MkdirAll(configuredMemoryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configuredMemoryDir, "projection.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sentinel at the default .local-agent/memory path: ResetState must
	// leave it byte-for-byte alone. Without this file, "the path does not
	// exist" would trivially hold whether or not ResetState ever touched it.
	defaultMemoryDir := filepath.Join(configDir, "memory")
	if err := os.MkdirAll(defaultMemoryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinelPath := filepath.Join(defaultMemoryDir, "sentinel.json")
	sentinelContent := []byte(`{"untouched":true}`)
	if err := os.WriteFile(sentinelPath, sentinelContent, 0o600); err != nil {
		t.Fatal(err)
	}

	log := &eventLog{}
	application := &Application{
		root:          root,
		logOutput:     &bytes.Buffer{},
		forceShutdown: make(chan struct{}),
		schemaLocker:  seamLocker{log: log},
		schemaTrace:   log.record,
	}

	if err := application.ResetState(context.Background()); err != nil {
		t.Fatalf("ResetState: %v", err)
	}
	if _, err := os.Stat(configuredMemoryDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configured memory dir stat = %v, want not-exist", err)
	}
	got, err := os.ReadFile(sentinelPath)
	if err != nil {
		t.Fatalf("default .local-agent/memory sentinel: %v, want untouched", err)
	}
	if string(got) != string(sentinelContent) {
		t.Fatalf("default .local-agent/memory sentinel content = %q, want %q", got, sentinelContent)
	}
}

// TestSeamRunRecordsPreflightBetweenLockAndOpen covers the composition entry
// (run): the lock acquisition precedes the rollout-completeness preflight,
// which precedes the infrastructure open; the release follows both on the
// rejection path (checkpoint 4 inserts requireRolloutComplete between the
// checkpoint-3 lock and open).
func TestSeamRunRecordsPreflightBetweenLockAndOpen(t *testing.T) {
	application, log, dbPath := newSeamApplication(t)
	writeMinimalDefinitions(t, filepath.Join(application.root, ".local-agent"))
	before := buildBehindFixture(t, dbPath)
	// Slack tokens are validated during model preparation, before the
	// database opens; provide them plus the provider key so Run reaches the
	// schema gate.
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-seam-token")
	t.Setenv("SLACK_APP_TOKEN", "xapp-seam-token")
	t.Setenv("SEAM_MODEL_KEY", "seam-model-key")

	err := application.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), schemaBehindMessage) {
		t.Fatalf("err = %v, want %q", err, schemaBehindMessage)
	}
	assertOrder(t, log, "preflight")
	assertFileDigest(t, dbPath, before)
}

type refusingLocker struct{}

func (refusingLocker) AcquireExclusive(string) (rollout.Lock, error) {
	return nil, rollout.ErrMutationLockHeld
}

func probeUserVersion(t *testing.T, dbPath string) int {
	t.Helper()
	plain, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	var version int
	if err := plain.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

// TestSeamRunSuccessPathRecordsFullSequence closes FIND-191: the success
// path of run needs direct sequence evidence too. The fixture is exactly
// what Create just wrote (AlreadyComplete), so the preflight passes and the
// real store opens; the context cancels right after a successful open, so
// Run unwinds deterministically with no network or Slack dependency, and
// unlock follows both earlier events.
func TestSeamRunSuccessPathRecordsFullSequence(t *testing.T) {
	application, log, dbPath := newSeamApplication(t)
	writeMinimalDefinitions(t, filepath.Join(application.root, ".local-agent"))
	store, createErr := adaptersqlite.Create(context.Background(), dbPath)
	if createErr != nil {
		t.Fatal(createErr)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-seam-token")
	t.Setenv("SLACK_APP_TOKEN", "xapp-seam-token")
	t.Setenv("SEAM_MODEL_KEY", "seam-model-key")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	realOpen := adaptersqlite.OpenCurrent
	application.openCurrent = func(openCtx context.Context, path string) (*adaptersqlite.Store, error) {
		opened, openErr := realOpen(openCtx, path)
		if openErr == nil {
			// Every success-path event is recorded once the store opens;
			// cancelling here makes Run return deterministically without
			// ever reaching Slack or the network.
			cancel()
		}
		return opened, openErr
	}

	runErr := application.Run(ctx)
	assertOrder(t, log, "preflight,disposition,open-current")
	if runErr == nil {
		t.Log("run returned cleanly after the cancelled context")
	}
}
