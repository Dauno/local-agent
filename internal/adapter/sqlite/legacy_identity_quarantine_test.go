package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

// quarantineFixtureCutoff is the seeded durable rollout cutoff every FIND-110
// row must be measured against.
const quarantineFixtureCutoff = int64(1_000_000_000_000)

// seedQuarantineJob inserts one external-agent job with an explicit creation
// timestamp and an incomplete result identity shape.
func seedQuarantineJob(t *testing.T, db *sql.DB, id string, createdAt int64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO external_agent_jobs (
		job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
		task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
		conversation_key, status, timeout_at, created_at, updated_at)
		VALUES (?, 'detached', 'opencode', 'build', 'workspace', '[]', 'r1',
		'task', 'request', ? || '-wrapper', ? || '-original', 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', 'completed', 2, ?, ?)`,
		id, id, id, createdAt, createdAt); err != nil {
		t.Fatal(err)
	}
}

// seedQuarantineActivation inserts one completed activation with zero content
// bytes and the given error code under an existing job row.
func seedQuarantineActivation(t *testing.T, db *sql.DB, jobID string, createdAt int64, errorCode string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO external_agent_job_activations (
		job_id, status_revision, kind, activation_id, terminal_status, notification_sha256,
		actor, team_id, conversation_key, original_call_id, delivery_mode, content_bytes,
		slack_message_ts, published_at, state, next_attempt_at,
		last_error_code, created_at, updated_at)
		VALUES (?, 0, 'terminal', ? || '-act', 'completed', ?,
		'U12345678', 'T12345678', 'slack:T12345678:dm:D12345678', ? || '-original', 'markdown', 0,
		'1710000000.000002', ?, 'pending', 0,
		?, ?, ?)`,
		jobID, jobID, strings.Repeat("a", 64), jobID, createdAt, errorCode, createdAt, createdAt); err != nil {
		t.Fatal(err)
	}
}

// seedHealthyIdentityJob inserts one completed job whose result identity is
// complete: it can own activations without joining the jobs match predicate.
func seedHealthyIdentityJob(t *testing.T, db *sql.DB, id string, createdAt int64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO external_agent_jobs (
		job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
		task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
		conversation_key, status, result_sha256, result_bytes, timeout_at, created_at, updated_at)
		VALUES (?, 'detached', 'opencode', 'build', 'workspace', '[]', 'r1',
		'task', 'request', ? || '-wrapper', ? || '-original', 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', 'completed', ?, 5, 2, ?, ?)`,
		id, id, id, strings.Repeat("b", 64), createdAt, createdAt); err != nil {
		t.Fatal(err)
	}
}

// seedFind110Fixture builds the checkpoint-5 entry shape: two legacy jobs and
// three legacy activations before the cutoff, plus one of each after it with
// the same incomplete identity shape (the post-cutoff exclusions).
func seedFind110Fixture(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, id := range []string{"legacy-job-1", "legacy-job-2"} {
		seedQuarantineJob(t, db, id, quarantineFixtureCutoff-10)
	}
	for _, id := range []string{"legacy-act-job-1", "legacy-act-job-2", "legacy-act-job-3"} {
		seedHealthyIdentityJob(t, db, id, quarantineFixtureCutoff-10)
		seedQuarantineActivation(t, db, id, quarantineFixtureCutoff-10, "")
	}
	// Same incomplete shape, created after the cutoff: the predicates must
	// never match these rows, no matter how often the command runs.
	seedQuarantineJob(t, db, "fresh-defect-job", quarantineFixtureCutoff+10)
	seedQuarantineActivation(t, db, "fresh-defect-job", quarantineFixtureCutoff+10, "")
}

// seedQuarantineRuntimeState writes the given runtime_state keys.
func seedQuarantineRuntimeState(t *testing.T, db *sql.DB, values map[string]string) {
	t.Helper()
	for key, value := range values {
		if _, err := db.ExecContext(context.Background(),
			`INSERT INTO runtime_state (state_key, state_value, updated_at) VALUES (?, ?, 1)`, key, value); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
}

// newQuarantineFixtureStore creates a fully migrated database whose
// adoption-at-creation rollout keys are stripped, so every test controls its
// own cutoff, postflight, and completion-marker state.
func newQuarantineFixtureStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quarantine.db")
	store, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(context.Background(), `DELETE FROM runtime_state`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return path
}

// TestIdentityHealthExcludesLegacyActivationContentAndCountsIt pins the
// IdentityHealth change: a quarantined activation leaves the defect count and
// surfaces only in the informational field. A real pre-cutoff failure keeps
// failing doctor exactly as before.
func TestIdentityHealthExcludesLegacyActivationContentAndCountsIt(t *testing.T) {
	path := newQuarantineFixtureStore(t)
	store, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	db := store.DB()
	seedQuarantineJob(t, db, "act-current-defect", quarantineFixtureCutoff+10)
	seedQuarantineActivation(t, db, "act-current-defect", quarantineFixtureCutoff+10, "")
	seedQuarantineJob(t, db, "act-legacy", quarantineFixtureCutoff-10)
	seedQuarantineActivation(t, db, "act-legacy", quarantineFixtureCutoff-10, domain.ActivationLegacyContentCode)
	seedQuarantineJob(t, db, "act-real-failure", quarantineFixtureCutoff-10)
	seedQuarantineActivation(t, db, "act-real-failure", quarantineFixtureCutoff-10, "activation_retry_exhausted")

	health, err := NewExternalAgentJobStore(store).IdentityHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Two current defects: the post-cutoff empty activation and the real
	// pre-cutoff failure. Only the quarantined row left the defect count.
	if health.ActivationsWithoutContent != 2 {
		t.Fatalf("activations without content = %d, want 2 (the current defect and the real failure)", health.ActivationsWithoutContent)
	}
	if health.ActivationsWithoutContentLegacy != 1 {
		t.Fatalf("activations without content legacy = %d, want 1 (only the quarantined row)", health.ActivationsWithoutContentLegacy)
	}
}

// TestQuarantinePreviewCountsRespectCutoffAndGuard proves both frozen
// predicates count exactly the pre-cutoff FIND-110 rows and never the
// identical post-cutoff defects, and that a zero Adoption cutoff matches
// nothing at all.
func TestQuarantinePreviewCountsRespectCutoffAndGuard(t *testing.T) {
	path := newQuarantineFixtureStore(t)
	store, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	seedFind110Fixture(t, store.DB())
	_ = store.Close()

	quarantine := FileLegacyIdentityQuarantine{}
	ctx := context.Background()
	jobs, activations, err := quarantine.CountMatches(ctx, path, time.Unix(0, quarantineFixtureCutoff).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if jobs != 2 || activations != 3 {
		t.Fatalf("matches = (%d, %d), want (2, 3)", jobs, activations)
	}
	zeroJobs, zeroActs, err := quarantine.CountMatches(ctx, path, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if zeroJobs != 0 || zeroActs != 0 {
		t.Fatalf("adoption cutoff matched (%d, %d), want (0, 0)", zeroJobs, zeroActs)
	}
}

// TestQuarantineApplyMarksExactRowsPlusCompletionOnce is gate 2: apply writes
// exactly N events, M activation stamps, and one completion row; a second
// preview reads already_applied without re-counting, and a second apply hits
// the completion CAS instead of double-marking.
func TestQuarantineApplyMarksExactRowsPlusCompletionOnce(t *testing.T) {
	path := newQuarantineFixtureStore(t)
	store, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	seedFind110Fixture(t, store.DB())
	seedQuarantineRuntimeState(t, store.DB(), map[string]string{
		rollout.KeyCutoff:           strconv.FormatInt(quarantineFixtureCutoff, 10),
		rollout.KeyPostflightStatus: string(rollout.PostflightPassed),
	})
	_ = store.Close()

	ctx := context.Background()
	quarantine := FileLegacyIdentityQuarantine{}
	report, err := quarantine.Apply(ctx, path, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if report.AlreadyApplied || report.JobsMarked != 2 || report.ActivationsMarked != 3 {
		t.Fatalf("report = %+v, want 2/3 fresh marks", report)
	}
	if report.AppliedAt.IsZero() {
		t.Fatal("report lacks completion timestamp")
	}

	reopened, err := Initialize(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	assertQuarantineWrites(t, reopened.DB(), 2, 3)

	var appliedRaw string
	if err := reopened.DB().QueryRow(`SELECT state_value FROM runtime_state WHERE state_key = ?`,
		rollout.KeyLegacyQuarantineAt).Scan(&appliedRaw); err != nil {
		t.Fatal(err)
	}
	appliedAt, ok := rollout.ParseRFC3339(appliedRaw)
	if !ok {
		t.Fatalf("completion timestamp %q is not RFC3339", appliedRaw)
	}

	gotAppliedAt, present, err := quarantine.ReadAppliedAt(ctx, path)
	if err != nil || !present {
		t.Fatalf("ReadAppliedAt = (%v, %v, %v), want present", gotAppliedAt, present, err)
	}
	if !gotAppliedAt.Equal(appliedAt) {
		t.Fatalf("second preview appliedAt = %v, want %v", gotAppliedAt, appliedAt)
	}

	// A second apply carrying the original expectations answers already_applied
	// from the durable marker before any recount (FIND-193): zero marks and
	// the original completion timestamp.
	replay, err := quarantine.Apply(ctx, path, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.AlreadyApplied || replay.JobsMarked != 0 || replay.ActivationsMarked != 0 {
		t.Fatalf("replay report = %+v, want already_applied with zero marks", replay)
	}
	if !replay.AppliedAt.Equal(appliedAt) {
		t.Fatalf("replay timestamp = %v, want the original %v", replay.AppliedAt, appliedAt)
	}
	// The replay left no additional trace.
	assertQuarantineWrites(t, reopened.DB(), 2, 3)
}

// TestQuarantineApplyMismatchWritesNothing is gate 3: a wrong --expect-* count
// refuses inside the transaction and leaves no event, stamp, or marker.
func TestQuarantineApplyMismatchWritesNothing(t *testing.T) {
	path := newQuarantineFixtureStore(t)
	store, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	seedFind110Fixture(t, store.DB())
	seedQuarantineRuntimeState(t, store.DB(), map[string]string{
		rollout.KeyCutoff:           strconv.FormatInt(quarantineFixtureCutoff, 10),
		rollout.KeyPostflightStatus: string(rollout.PostflightPassed),
	})
	_ = store.Close()

	quarantine := FileLegacyIdentityQuarantine{}
	_, applyErr := quarantine.Apply(context.Background(), path, 3, 3)
	var mismatch QuarantineCountMismatchError
	if !errors.As(applyErr, &mismatch) {
		t.Fatalf("err = %v, want QuarantineCountMismatchError", applyErr)
	}
	if mismatch.ExpectedJobs != 3 || mismatch.ActualJobs != 2 || mismatch.ExpectedActivation != 3 || mismatch.ActualActivation != 3 {
		t.Fatalf("mismatch = %+v, want expected 3/3 vs actual 2/3", mismatch)
	}
	if !strings.Contains(mismatch.Error(), "expected jobs=3 activations=3") ||
		!strings.Contains(mismatch.Error(), "found jobs=2 activations=3") {
		t.Fatalf("mismatch text names neither side: %q", mismatch.Error())
	}

	reopened, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	assertQuarantineWrites(t, reopened.DB(), 0, 0)
}

// TestQuarantineApplyNeverOverwritesExistingErrorCode pins the load-bearing
// guard: an activation that failed before the cutoff keeps its own error code
// and never joins either count or the stamped set.
func TestQuarantineApplyNeverOverwritesExistingErrorCode(t *testing.T) {
	path := newQuarantineFixtureStore(t)
	store, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	seedFind110Fixture(t, store.DB())
	seedHealthyIdentityJob(t, store.DB(), "real-failure-job", quarantineFixtureCutoff-10)
	seedQuarantineActivation(t, store.DB(), "real-failure-job", quarantineFixtureCutoff-10, "activation_retry_exhausted")
	seedQuarantineRuntimeState(t, store.DB(), map[string]string{
		rollout.KeyCutoff:           strconv.FormatInt(quarantineFixtureCutoff, 10),
		rollout.KeyPostflightStatus: string(rollout.PostflightPassed),
	})
	_ = store.Close()

	quarantine := FileLegacyIdentityQuarantine{}
	report, err := quarantine.Apply(context.Background(), path, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if report.ActivationsMarked != 3 {
		t.Fatalf("activations marked = %d, want 3 (the guarded row stays out)", report.ActivationsMarked)
	}

	reopened, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	var keptCode string
	if err := reopened.DB().QueryRow(`SELECT last_error_code FROM external_agent_job_activations
		WHERE job_id = 'real-failure-job'`).Scan(&keptCode); err != nil {
		t.Fatal(err)
	}
	if keptCode != "activation_retry_exhausted" {
		t.Fatalf("existing error code overwritten to %q", keptCode)
	}
	assertQuarantineWrites(t, reopened.DB(), 2, 3)
}

// TestQuarantineRefusesDatabaseWithoutCutoff covers the defensive gate: a v41
// database without a frozen cutoff refuses instead of matching everything,
// and the missing-cutoff sentinel carries the frozen operator text.
func TestQuarantineRefusesDatabaseWithoutCutoff(t *testing.T) {
	path := newQuarantineFixtureStore(t)
	store, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	seedFind110Fixture(t, store.DB())
	_ = store.Close()

	quarantine := FileLegacyIdentityQuarantine{}
	ctx := context.Background()
	if _, present, readErr := quarantine.ReadCutoff(ctx, path); present || readErr != nil {
		t.Fatalf("ReadCutoff = (%v, %v), want absent-clean", present, readErr)
	}
	if _, _, readErr := quarantine.ReadAppliedAt(ctx, path); readErr != nil {
		t.Fatalf("ReadAppliedAt absence errored: %v", readErr)
	}
	if _, applyErr := quarantine.Apply(ctx, path, 2, 3); !errors.Is(applyErr, rollout.ErrLegacyCutoffNotRecorded) {
		t.Fatalf("apply err = %v, want ErrLegacyCutoffNotRecorded", applyErr)
	} else if !strings.Contains(applyErr.Error(), "no cutoff recorded, run local-agent db upgrade") {
		t.Fatalf("cutoff failure text = %q", applyErr.Error())
	}
}

// assertQuarantineWrites counts the three durable traces of one apply run.
func assertQuarantineWrites(t *testing.T, db *sql.DB, wantEvents, wantStamped int) {
	t.Helper()
	var events, stamped, markers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM external_agent_job_events WHERE event_kind = ?`,
		legacyResultIdentityEvent).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM external_agent_job_activations
		WHERE last_error_code = ?`, domain.ActivationLegacyContentCode).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM runtime_state WHERE state_key = ?`,
		rollout.KeyLegacyQuarantineAt).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	wantMarkers := 0
	if wantEvents > 0 {
		wantMarkers = 1
	}
	if events != wantEvents || stamped != wantStamped || markers != wantMarkers {
		t.Fatalf("quarantine traces = events:%d stamped:%d markers:%d, want events:%d stamped:%d markers:%d",
			events, stamped, markers, wantEvents, wantStamped, wantMarkers)
	}
}
