//go:build unix

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/cli"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

const testSHAValid = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testSHAOther = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type upgradeLog struct{ events []string }

func (l *upgradeLog) add(event string) { l.events = append(l.events, event) }
func (l *upgradeLog) joined() string   { return strings.Join(l.events, ",") }
func (l *upgradeLog) count(prefix string) int {
	n := 0
	for _, event := range l.events {
		if strings.HasPrefix(event, prefix) {
			n++
		}
	}
	return n
}

// upgradeHarness bundles an initialized project whose database the caller
// replaces afterwards, plus recording seams around the real SQLite
// implementations. The migrate step is faked at the header level because a
// rewound fixture's tables already sit at v41; the real migration chain is
// covered by adapter tests against createSchemaAtVersion fixtures.
type upgradeHarness struct {
	application *Application
	paths       config.Paths

	lockerLog *upgradeLog
	probeLog  *upgradeLog
	backupLog *upgradeLog
	writerLog *upgradeLog
	writer    *recordingWriter
	backupper *recordingBackupper
}

func newUpgradeHarness(t *testing.T) *upgradeHarness {
	t.Helper()
	for _, key := range []string{"DEEPSEEK_API_KEY", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN"} {
		t.Setenv(key, "")
	}
	rootDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	application, err := New(rootDir, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.NewReader("\n\n\nxoxb-token\nxapp-token\nU12345678\n\n\n\n\nmodel-key\ny\n")
	var output, stderr bytes.Buffer
	root, err := cli.NewRoot(application, cli.Streams{In: input, Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := cli.Execute(context.Background(), root, []string{"init"}, &stderr); code != 0 {
		t.Fatalf("init exit=%d stderr=%s", code, stderr.String())
	}
	paths, err := config.Default().ResolvePaths(rootDir)
	if err != nil {
		t.Fatal(err)
	}

	// One shared ordered log across every recording seam: cross-seam call
	// order (lock vs capture vs backup vs write) is exactly what the gates
	// pin, so separate logs would erase the interleaving.
	shared := &upgradeLog{}
	harness := &upgradeHarness{
		application: application,
		paths:       paths,
		lockerLog:   shared,
		probeLog:    shared,
		backupLog:   shared,
		writerLog:   shared,
	}
	application.schemaLocker = recordingLocker{log: harness.lockerLog}
	application.schemaProbe = recordingProbe{inner: adaptersqlite.FileSchemaProbe{}, log: harness.probeLog}
	harness.backupper = &recordingBackupper{inner: adaptersqlite.FileDatabaseBackupper{}, log: harness.backupLog}
	application.schemaBackupper = harness.backupper
	harness.writer = &recordingWriter{inner: adaptersqlite.FileSchemaWriter{}, log: harness.writerLog, fakeMigrate: true}
	application.schemaWriter = harness.writer
	return harness
}

type recordingLocker struct{ log *upgradeLog }

func (l recordingLocker) AcquireExclusive(path string) (rollout.Lock, error) {
	l.log.add("lock:" + filepath.Base(path))
	return releaseFuncLock(func() { l.log.add("unlock") }), nil
}

type releaseFuncLock func()

func (f releaseFuncLock) Release() error { f(); return nil }

type recordingProbe struct {
	inner rollout.SchemaProbe
	log   *upgradeLog
}

func (p recordingProbe) CurrentVersion(ctx context.Context, path string) (int, error) {
	p.log.add("probe.version")
	return p.inner.CurrentVersion(ctx, path)
}

func (p recordingProbe) CaptureIdentityBaseline(ctx context.Context, path string) (rollout.IdentityBaseline, error) {
	p.log.add("probe.capture")
	return p.inner.CaptureIdentityBaseline(ctx, path)
}

func (p recordingProbe) ReadRolloutState(ctx context.Context, path string) (rollout.RolloutState, error) {
	p.log.add("probe.state")
	return p.inner.ReadRolloutState(ctx, path)
}

func (p recordingProbe) IdentityHealth(ctx context.Context, path string) (domain.ExternalAgentJobIdentityHealth, error) {
	p.log.add("probe.health")
	return p.inner.IdentityHealth(ctx, path)
}

type recordingBackupper struct {
	inner rollout.DatabaseBackupper
	log   *upgradeLog
}

func (b *recordingBackupper) BackupInto(ctx context.Context, srcPath, destPath string) (rollout.BackupIdentity, error) {
	b.log.add("backupper.into")
	identity, err := b.inner.BackupInto(ctx, srcPath, destPath)
	// FIND-155 ordering evidence: snapshot the source journal immediately
	// after the verified backup exists; the flip may only happen later.
	if err == nil {
		b.log.add("journal.after-verify=" + journalModeOf(srcPath))
	}
	return identity, err
}

func journalModeOf(path string) string {
	plain, err := sql.Open("sqlite", path)
	if err != nil {
		return ""
	}
	defer plain.Close()
	var mode string
	if err := plain.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return ""
	}
	return mode
}

func (b *recordingBackupper) VerifyBackup(ctx context.Context, backupPath string, wantSourceVersion int) (rollout.BackupIdentity, error) {
	b.log.add("backupper.verify")
	return b.inner.VerifyBackup(ctx, backupPath, wantSourceVersion)
}

type recordingWriter struct {
	inner       rollout.SchemaWriter
	log         *upgradeLog
	fakeMigrate bool

	lastBaseline rollout.IdentityBaseline
	lastCutoff   int64
	recordCalls  int
	migrateCalls int
	postflights  []string
}

func (w *recordingWriter) RecordBaselineAndCutoff(ctx context.Context, path string, baseline rollout.IdentityBaseline, cutoffUnixNanos int64, backup rollout.BackupIdentity) error {
	w.recordCalls++
	w.lastBaseline = baseline
	w.lastCutoff = cutoffUnixNanos
	w.log.add("writer.record-baseline")
	return w.inner.RecordBaselineAndCutoff(ctx, path, baseline, cutoffUnixNanos, backup)
}

func (w *recordingWriter) Migrate(ctx context.Context, path string) error {
	w.migrateCalls++
	w.log.add("writer.migrate")
	if !w.fakeMigrate {
		return w.inner.Migrate(ctx, path)
	}
	plain, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer plain.Close()
	_, err = plain.ExecContext(ctx, "PRAGMA user_version = 41")
	return err
}

func (w *recordingWriter) RecordPostflight(ctx context.Context, path string, status rollout.PostflightStatus, detail string) error {
	w.postflights = append(w.postflights, string(status))
	w.log.add("writer.postflight:" + string(status))
	return w.inner.RecordPostflight(ctx, path, status, detail)
}

// replaceFixture rewrites the configured database path with a fixture built
// from a real Create() database: adoption keys stripped, journal reset to
// delete, optional header version, optional seeded runtime_state values.
func replaceFixture(t *testing.T, dbPath string, version int, seed map[string]string) string {
	t.Helper()
	ctx := context.Background()
	// The configured file already exists (init created it); a fixture
	// replacement removes it first because Create refuses existing paths.
	if err := os.Remove(dbPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	store, err := adaptersqlite.Create(ctx, dbPath)
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
	defer plain.Close()
	if _, err := plain.Exec("DELETE FROM runtime_state"); err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Exec("PRAGMA journal_mode = delete"); err != nil {
		t.Fatal(err)
	}
	if version != 41 {
		if _, err := plain.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range seed {
		if _, err := plain.Exec(
			`INSERT INTO runtime_state (state_key, state_value, updated_at) VALUES (?, ?, 1)`, key, value); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func dumpRuntimeState(t *testing.T, dbPath string) string {
	t.Helper()
	plain, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	rows, err := plain.Query(`SELECT state_key || '=' || state_value FROM runtime_state ORDER BY state_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, ";")
}

func queryUserVersion(t *testing.T, dbPath string) int {
	t.Helper()
	plain, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	var version int
	if err := plain.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func sqlOpenPlain(dbPath string) (*sql.DB, error) { return sql.Open("sqlite", dbPath) }

func seedFatalJobField(t *testing.T, dbPath, field string) {
	t.Helper()
	plain, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	ctx := context.Background()
	jobInsert := func(jobID, mode, status string) {
		query := `INSERT INTO external_agent_jobs (
			job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
			task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
			conversation_key, status, timeout_at, created_at, updated_at)
			VALUES (?, ?, 'opencode', 'build', 'workspace', '[]', 'r1',
			'task', 'request', ? || '-wrapper', ? || '-original', 'U12345678', 'T12345678',
			'slack:T12345678:dm:D12345678', ?, 2, 1, 1)`
		if _, err := plain.ExecContext(ctx, query, jobID, mode, jobID, jobID, status); err != nil {
			t.Fatalf("seed %s: %v", field, err)
		}
	}
	switch field {
	case "JobsCompletedWithoutResultIdentity":
		jobInsert("fatal-jobs-"+field, "detached", "completed")
	case "ActivationsWithoutContent":
		jobInsert("fatal-act-content", "detached", "running")
		seedActivationRow(t, plain, "fatal-act-content", "completed", testSHAValid, 0, "pending", false)
	case "NotificationsWithoutIdentity":
		jobInsert("fatal-notifs", "detached", "running")
		if _, err := plain.ExecContext(ctx, `INSERT INTO external_agent_job_notifications (
			job_id, status_revision, kind, canonical_markdown, content_sha256, renderer_version,
			channel_id, next_attempt_at, created_at, updated_at)
			VALUES ('fatal-notifs', 0, 'terminal', 'm', 'short-digest', 'markdown_v1', 'C1', 1, 1, 1)`); err != nil {
			t.Fatalf("seed notification: %v", err)
		}
	case "ActivationsWithoutIdentity":
		jobInsert("fatal-act-id", "detached", "running")
		// The table CHECK only pins length 64; 64 non-hex characters trip
		// the identity-health digest predicate.
		seedActivationRow(t, plain, "fatal-act-id", "failed", strings.Repeat("z", 64), 10, "pending", false)
	case "ForegroundActivationsActive":
		jobInsert("fatal-fg", "foreground", "running")
		seedActivationRow(t, plain, "fatal-fg", "failed", testSHAValid, 10, "pending", true)
	default:
		t.Fatalf("unknown fatal field %q", field)
	}
}

func seedActivationRow(t *testing.T, plain *sql.DB, jobID, terminalStatus, notificationSHA string, contentBytes int64, state string, foreground bool) {
	t.Helper()
	if _, err := plain.ExecContext(context.Background(), `INSERT INTO external_agent_job_activations (
		job_id, status_revision, kind, activation_id, terminal_status, notification_sha256,
		actor, team_id, conversation_key, original_call_id, delivery_mode, content_bytes,
		slack_message_ts, published_at, next_attempt_at, state, created_at, updated_at)
		VALUES (?, 0, 'terminal', ? || '-act', ?, ?, 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', ? || '-original', 'markdown', ?,
		'msg-1', 1, 1, ?, 1, 1)`,
		jobID, jobID, terminalStatus, notificationSHA, jobID, contentBytes, state); err != nil {
		t.Fatalf("seed activation for %s: %v", jobID, err)
	}
	if !foreground {
		return
	}
	if _, err := plain.ExecContext(context.Background(),
		`UPDATE external_agent_jobs SET mode = 'foreground' WHERE job_id = ?`, jobID); err != nil {
		t.Fatal(err)
	}
}

func (l *upgradeLog) lastWithPrefix(prefix string) string {
	for i := len(l.events) - 1; i >= 0; i-- {
		if strings.HasPrefix(l.events[i], prefix) {
			return l.events[i]
		}
	}
	return ""
}
