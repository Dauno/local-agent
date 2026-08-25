package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

const probeSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func seedRolloutKey(t *testing.T, db execer, key, value string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO runtime_state (state_key, state_value, updated_at) VALUES (?, ?, 1)`, key, value); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
}

func seedCompletedJobWithoutIdentity(t *testing.T, db execer, jobID string) {
	t.Helper()
	query := `INSERT INTO external_agent_jobs (
		job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
		task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
		conversation_key, status, timeout_at, created_at, updated_at)
		VALUES (?, 'detached', 'opencode', 'build', 'workspace', '[]', 'r1',
		'task', 'request', ? || '-wrapper', ? || '-original', 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', 'completed', 2, 1, 1)`
	if _, err := db.ExecContext(context.Background(), query, jobID, jobID, jobID); err != nil {
		t.Fatalf("seed completed job %s: %v", jobID, err)
	}
}

func seedActivation(t *testing.T, db execer, jobID, activationID, terminalStatus, notificationSHA256 string, contentBytes int64, state string) {
	t.Helper()
	mode := "detached"
	if state == "pending" && terminalStatus == "failed" {
		mode = "foreground"
	}
	query := `INSERT INTO external_agent_job_activations (
		job_id, status_revision, kind, activation_id, terminal_status, notification_sha256,
		actor, team_id, conversation_key, original_call_id, delivery_mode, content_bytes,
		slack_message_ts, published_at, next_attempt_at, state, created_at, updated_at)
		VALUES (?, 0, 'terminal', ?, ?, ?, 'U12345678', 'T12345678', 'slack:T12345678:dm:D12345678',
		? || '-original-act', 'markdown', ?, 'msg-1', 1, 1, ?, 1, 1)`
	if _, err := db.ExecContext(context.Background(), query,
		jobID, activationID, terminalStatus, notificationSHA256, activationID, contentBytes, state); err != nil {
		t.Fatalf("seed activation %s: %v", activationID, err)
	}
	if mode == "foreground" {
		if _, err := db.ExecContext(context.Background(),
			`UPDATE external_agent_jobs SET mode = 'foreground' WHERE job_id = ?`, jobID); err != nil {
			t.Fatalf("seed foreground job: %v", err)
		}
	}
}

func TestSchemaProbeCurrentVersionReadsHeader(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 33)
	defer func() { _ = raw.Close() }()
	current, err := FileSchemaProbe{}.CurrentVersion(context.Background(), path)
	if err != nil || current != 33 {
		t.Fatalf("current=%d err=%v, want 33", current, err)
	}
}

func TestSchemaProbeReadRolloutStateRoundTrip(t *testing.T) {
	path, raw := createSchemaAtVersion(t, rollout.TargetVersion)
	defer func() { _ = raw.Close() }()
	seedRolloutKey(t, raw, rollout.KeyBaseline, "jobs=3;activations=5")
	seedRolloutKey(t, raw, rollout.KeyCutoff, "12345")
	seedRolloutKey(t, raw, rollout.KeyBackupPath, "/tmp/backup.db")
	seedRolloutKey(t, raw, rollout.KeyBackupBytes, "4096")
	seedRolloutKey(t, raw, rollout.KeyBackupSHA256, probeSHA256)
	seedRolloutKey(t, raw, rollout.KeyBackupSourceVersion, "33")
	verifiedAt := time.Date(2026, 8, 21, 14, 30, 0, 0, time.UTC).Format(time.RFC3339)
	seedRolloutKey(t, raw, rollout.KeyBackupVerifiedAt, verifiedAt)
	seedRolloutKey(t, raw, rollout.KeyPostflightStatus, "passed")
	seedRolloutKey(t, raw, rollout.KeyPostflightDetail, "detail text")

	state, err := FileSchemaProbe{}.ReadRolloutState(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadRolloutState: %v", err)
	}
	if !state.BaselinePresent || !state.BaselineValid || state.Baseline.JobsCompletedWithoutResultIdentity != 3 || state.Baseline.ActivationsWithoutContent != 5 {
		t.Fatalf("baseline = %+v present=%v valid=%v", state.Baseline, state.BaselinePresent, state.BaselineValid)
	}
	if !state.CutoffPresent || !state.CutoffValid || state.CutoffUnixNanos != 12345 {
		t.Fatalf("cutoff = %d present=%v valid=%v", state.CutoffUnixNanos, state.CutoffPresent, state.CutoffValid)
	}
	if !state.BackupPathPresent || !state.BackupPathValid || state.BackupPath != "/tmp/backup.db" {
		t.Fatalf("backup path = %q valid=%v", state.BackupPath, state.BackupPathValid)
	}
	if !state.BackupBytesPresent || !state.BackupBytesValid || state.BackupBytes != 4096 {
		t.Fatalf("backup bytes = %d valid=%v", state.BackupBytes, state.BackupBytesValid)
	}
	if !state.BackupSHA256Present || !state.BackupSHA256Valid || state.BackupSHA256 != probeSHA256 {
		t.Fatalf("backup sha valid=%v", state.BackupSHA256Valid)
	}
	if !state.BackupSourceVersionPresent || !state.BackupSourceVersionValid || state.BackupSourceVersion != 33 {
		t.Fatalf("source version = %d valid=%v", state.BackupSourceVersion, state.BackupSourceVersionValid)
	}
	if !state.BackupVerifiedAtPresent || !state.BackupVerifiedAtValid {
		t.Fatalf("verified at present=%v valid=%v", state.BackupVerifiedAtPresent, state.BackupVerifiedAtValid)
	}
	if !state.PostflightPresent || !state.PostflightValid || state.PostflightStatus != rollout.PostflightPassed {
		t.Fatalf("postflight = %q valid=%v", state.PostflightStatus, state.PostflightValid)
	}
	if !state.PostflightDetailPresent || state.PostflightDetail != "detail text" {
		t.Fatalf("detail = %q", state.PostflightDetail)
	}
	if state.BackupNotRequiredAtPresent {
		t.Fatal("absent marker must not be reported present")
	}
	row, classifyErr := rollout.ClassifyRollout(rollout.TargetVersion, state)
	if classifyErr != nil || row != rollout.RolloutRowAlreadyComplete {
		t.Fatalf("row=%d err=%v, want AlreadyComplete", row, classifyErr)
	}
}

func TestSchemaProbeReadRolloutStateMalformedValuesStayInvalid(t *testing.T) {
	cases := []struct {
		key     string
		value   string
		checkFn func(rollout.RolloutState) bool
	}{
		{rollout.KeyBaseline, "jobs=x", func(s rollout.RolloutState) bool { return s.BaselinePresent && !s.BaselineValid }},
		{rollout.KeyCutoff, "soon", func(s rollout.RolloutState) bool { return s.CutoffPresent && !s.CutoffValid }},
		{rollout.KeyBackupPath, "relative/path.db", func(s rollout.RolloutState) bool { return s.BackupPathPresent && !s.BackupPathValid }},
		{rollout.KeyBackupBytes, "big", func(s rollout.RolloutState) bool { return s.BackupBytesPresent && !s.BackupBytesValid }},
		{rollout.KeyBackupSHA256, strings.ToUpper(probeSHA256), func(s rollout.RolloutState) bool { return s.BackupSHA256Present && !s.BackupSHA256Valid }},
		{rollout.KeyBackupSourceVersion, "44", func(s rollout.RolloutState) bool { return s.BackupSourceVersionPresent && !s.BackupSourceVersionValid }},
		{rollout.KeyBackupVerifiedAt, "yesterday", func(s rollout.RolloutState) bool { return s.BackupVerifiedAtPresent && !s.BackupVerifiedAtValid }},
		{rollout.KeyPostflightStatus, "weird", func(s rollout.RolloutState) bool { return s.PostflightPresent && !s.PostflightValid }},
	}
	for _, testCase := range cases {
		path, raw := createSchemaAtVersion(t, 41)
		seedRolloutKey(t, raw, testCase.key, testCase.value)
		_ = raw.Close()
		state, err := FileSchemaProbe{}.ReadRolloutState(context.Background(), path)
		if err != nil {
			t.Fatalf("%s: %v", testCase.key, err)
		}
		if !testCase.checkFn(state) {
			t.Fatalf("key %s value %q: presence/validity reading wrong", testCase.key, testCase.value)
		}
	}
}

func TestSchemaProbeCaptureIdentityBaselineCountsCarveOutFields(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 41)
	defer func() { _ = raw.Close() }()

	baseline, err := FileSchemaProbe{}.CaptureIdentityBaseline(ctx, path)
	if err != nil || baseline.JobsCompletedWithoutResultIdentity != 0 || baseline.ActivationsWithoutContent != 0 {
		t.Fatalf("empty baseline = %+v err=%v, want zeros", baseline, err)
	}

	seedCompletedJobWithoutIdentity(t, raw, "probe-jobs")
	baseline, err = FileSchemaProbe{}.CaptureIdentityBaseline(ctx, path)
	if err != nil || baseline.JobsCompletedWithoutResultIdentity != 1 {
		t.Fatalf("jobs baseline = %+v err=%v, want jobs=1", baseline, err)
	}

	seedActivation(t, raw, "probe-jobs", "probe-act", "completed", probeSHA256, 0, "pending")
	baseline, err = FileSchemaProbe{}.CaptureIdentityBaseline(ctx, path)
	if err != nil || baseline.ActivationsWithoutContent != 1 {
		t.Fatalf("activation baseline = %+v err=%v, want activations=1", baseline, err)
	}
}

func TestSchemaProbeIdentityHealthSurfacesAllFatalFields(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 41)
	defer func() { _ = raw.Close() }()

	health, err := FileSchemaProbe{}.IdentityHealth(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	zeroFields := []struct {
		name  string
		count int
	}{
		{"JobsCompletedWithoutResultIdentity", health.JobsCompletedWithoutResultIdentity},
		{"ActivationsWithoutContent", health.ActivationsWithoutContent},
		{"NotificationsWithoutIdentity", health.NotificationsWithoutIdentity},
		{"ActivationsWithoutIdentity", health.ActivationsWithoutIdentity},
		{"ForegroundActivationsActive", health.ForegroundActivationsActive},
	}
	for _, field := range zeroFields {
		if field.count != 0 {
			t.Fatalf("%s = %d on a fresh fixture, want 0", field.name, field.count)
		}
	}
}
