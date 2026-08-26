//go:build unix

package app

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

// Quarantine timestamp anchors: every legacy row predates the frozen cutoff,
// and one job plus one activation with the identical incomplete shape postdate
// it so the cutoff exclusion is exercised on both predicates.
const (
	quarantineLegacyAt   = int64(1_700_000_000_000_000_000)
	quarantineCutoffAt   = int64(1_750_000_000_000_000_000)
	quarantineFreshAt    = int64(1_800_000_000_000_000_000)
	quarantineWantJobs   = 2
	quarantineWantActive = 3
)

// assertFixtureUnchanged pins byte identity of a fixture across a refused or
// read-only call.
func assertFixtureUnchanged(t *testing.T, dbPath, before string) {
	t.Helper()
	if got := sha256File(t, dbPath); got != before {
		t.Fatalf("fixture bytes changed: before=%s after=%s", before, got)
	}
}

func openPlainDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	plain, err := sqlOpenPlain(path)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

// seedQuarantineJobRow inserts one completed job without result identity.
func seedQuarantineJobRow(t *testing.T, plain *sql.DB, id string, createdAt int64) {
	t.Helper()
	if _, err := plain.Exec(`INSERT INTO external_agent_jobs (
		job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
		task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
		conversation_key, status, timeout_at, created_at, updated_at)
		VALUES (?, 'detached', 'opencode', 'build', 'workspace', '[]', 'r1',
		'task', 'request', ? || '-wrapper', ? || '-original', 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', 'completed', 2, ?, ?)`,
		id, id, id, createdAt, createdAt); err != nil {
		t.Fatalf("seed job %s: %v", id, err)
	}
}

// seedQuarantineActivationRow inserts one completed activation with zero
// content bytes under a parent job whose own result identity is complete.
func seedQuarantineActivationRow(t *testing.T, plain *sql.DB, id string, createdAt int64) {
	t.Helper()
	if _, err := plain.Exec(`INSERT INTO external_agent_jobs (
		job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
		task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
		conversation_key, status, result_sha256, result_bytes, timeout_at, created_at, updated_at)
		VALUES (?, 'detached', 'opencode', 'build', 'workspace', '[]', 'r1',
		'task', 'request', ? || '-wrapper', ? || '-original', 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', 'completed', ?, 5, 2, ?, ?)`,
		id+"-parent", id+"-parent", id+"-parent", strings.Repeat("b", 64), createdAt, createdAt); err != nil {
		t.Fatalf("seed activation parent %s: %v", id, err)
	}
	if _, err := plain.Exec(`INSERT INTO external_agent_job_activations (
		job_id, status_revision, kind, activation_id, terminal_status, notification_sha256,
		actor, team_id, conversation_key, original_call_id, delivery_mode, content_bytes,
		slack_message_ts, published_at, state, next_attempt_at,
		last_error_code, created_at, updated_at)
		VALUES (?, 0, 'terminal', ?, 'completed', ?,
		'U12345678', 'T12345678', 'slack:T12345678:dm:D12345678', ? || '-original', 'markdown', 0,
		'1710000000.000002', ?, 'pending', 0,
		'', ?, ?)`,
		id+"-parent", id+"-act", strings.Repeat("a", 64), id+"-parent", createdAt, createdAt, createdAt); err != nil {
		t.Fatalf("seed activation %s: %v", id, err)
	}
}

// seedFind110Rows writes the checkpoint-5 entry shape against an existing v41
// database: two legacy jobs, three legacy activations before the cutoff, plus
// optionally one job and one activation after it with the identical shape.
func seedFind110Rows(t *testing.T, dbPath string, includeFreshDefects bool) {
	t.Helper()
	plain := openPlainDB(t, dbPath)
	defer func() { _ = plain.Close() }()
	seedQuarantineJobRow(t, plain, "legacy-job-1", quarantineLegacyAt)
	seedQuarantineJobRow(t, plain, "legacy-job-2", quarantineLegacyAt)
	for _, id := range []string{"legacy-act-1", "legacy-act-2", "legacy-act-3"} {
		seedQuarantineActivationRow(t, plain, id, quarantineLegacyAt)
	}
	if !includeFreshDefects {
		return
	}
	seedQuarantineJobRow(t, plain, "fresh-defect-job", quarantineFreshAt)
	seedQuarantineActivationRow(t, plain, "fresh-defect-act", quarantineFreshAt)
}

// completeQuarantineStateSeeds is the durable row-5 reading minus the
// completion marker: a fully rolled-out database that still owes its
// disposition.
func completeQuarantineStateSeeds() map[string]string {
	return map[string]string{
		keyBaselineStr: "jobs=0;activations=0",
		keyCutoffStr:   strconv.FormatInt(quarantineCutoffAt, 10),
		keyNotRequired: time.Unix(0, quarantineCutoffAt).UTC().Format(time.RFC3339),
		keyPostStatus:  string(rollout.PostflightPassed),
		keyPostDetail:  "postflight passed; jobs_completed_without_result_identity=0; activations_without_content=0",
	}
}

// buildQuarantineFixture replaces the configured database with the given
// rollout-state seed plus the FIND-110 rows, and returns the exact bytes.
func buildQuarantineFixture(t *testing.T, h *upgradeHarness, state map[string]string) string {
	t.Helper()
	return buildQuarantineFixtureWithFreshDefects(t, h, state, true)
}

func buildQuarantineFixtureWithFreshDefects(t *testing.T, h *upgradeHarness, state map[string]string, includeFreshDefects bool) string {
	t.Helper()
	dbPath := h.paths.DatabaseFile
	replaceFixture(t, dbPath, rollout.TargetVersion, nil)
	seedRolloutKeys(t, dbPath, state)
	seedFind110Rows(t, dbPath, includeFreshDefects)
	return sha256File(t, dbPath)
}

// countingQuarantineStore records which disposition operations ran.
type countingQuarantineStore struct {
	inner   rollout.LegacyIdentityQuarantineStore
	log     *upgradeLog
	applies int
}

func (s *countingQuarantineStore) ReadCutoff(ctx context.Context, path string) (time.Time, bool, error) {
	s.log.add("quarantine.read-cutoff")
	return s.inner.ReadCutoff(ctx, path)
}

func (s *countingQuarantineStore) ReadAppliedAt(ctx context.Context, path string) (time.Time, bool, error) {
	s.log.add("quarantine.read-applied")
	return s.inner.ReadAppliedAt(ctx, path)
}

func (s *countingQuarantineStore) CountMatches(ctx context.Context, path string, cutoff time.Time) (int, int, error) {
	s.log.add("quarantine.count")
	return s.inner.CountMatches(ctx, path, cutoff)
}

func (s *countingQuarantineStore) Apply(ctx context.Context, path string, expectJobs, expectActivations int) (rollout.LegacyIdentityQuarantineReport, error) {
	s.applies++
	s.log.add("quarantine.apply")
	return s.inner.Apply(ctx, path, expectJobs, expectActivations)
}

// TestPreviewQuarantineCountsExactFind110Shape is gate 1: the preview reports
// the two pre-cutoff jobs and three pre-cutoff activations, never N+1/M+1,
// and writes nothing.
func TestPreviewQuarantineCountsExactFind110Shape(t *testing.T) {
	h := newUpgradeHarness(t)
	before := buildQuarantineFixture(t, h, completeQuarantineStateSeeds())

	preview, err := h.application.PreviewLegacyIdentityQuarantine(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if preview.AlreadyApplied {
		t.Fatal("preview reported already_applied before any apply ran")
	}
	if preview.JobsMatched != quarantineWantJobs || preview.ActivationsMatched != quarantineWantActive {
		t.Fatalf("preview = (%d, %d), want (%d, %d)", preview.JobsMatched, preview.ActivationsMatched, quarantineWantJobs, quarantineWantActive)
	}
	if got, want := preview.Cutoff.UnixNano(), quarantineCutoffAt; got != want {
		t.Fatalf("cutoff = %d, want %d", got, want)
	}
	assertFixtureUnchanged(t, h.paths.DatabaseFile, before)
}

// TestPreviewQuarantineFailsClosedWithoutCutoff is gate 6: a v41 database
// that reached target without db upgrade's cutoff refuses with the frozen
// defensive text.
func TestPreviewQuarantineFailsClosedWithoutCutoff(t *testing.T) {
	h := newUpgradeHarness(t)
	state := completeQuarantineStateSeeds()
	delete(state, keyCutoffStr)
	buildQuarantineFixture(t, h, state)

	_, err := h.application.PreviewLegacyIdentityQuarantine(ctx())
	if err == nil {
		t.Fatal("preview without cutoff succeeded")
	}
	if !strings.Contains(err.Error(), "no cutoff recorded, run local-agent db upgrade") {
		t.Fatalf("err = %v, want the frozen no-cutoff text", err)
	}
}

// TestQuarantineAppliesRefuseOutOfRangeSchemasWithoutAdvice is gate 5: below
// range refuses with the terminal message that never recommends db upgrade.
func TestQuarantineAppliesRefuseOutOfRangeSchemasWithoutAdvice(t *testing.T) {
	h := newUpgradeHarness(t)
	replaceFixture(t, h.paths.DatabaseFile, 32, nil)

	preview, previewErr := h.application.PreviewLegacyIdentityQuarantine(ctx())
	if previewErr == nil || !errors.Is(previewErr, rollout.ErrUnsupportedSourceSchema) {
		t.Fatalf("preview err = %v, want ErrUnsupportedSourceSchema", previewErr)
	}
	apply, applyErr := h.application.ApplyLegacyIdentityQuarantine(ctx(), rollout.LegacyIdentityQuarantinePreview{JobsMatched: 0, ActivationsMatched: 0})
	if applyErr == nil || !errors.Is(applyErr, rollout.ErrUnsupportedSourceSchema) {
		t.Fatalf("apply err = %v, want ErrUnsupportedSourceSchema", applyErr)
	}
	for _, err := range []error{previewErr, applyErr} {
		if !strings.Contains(err.Error(), terminalRangeText) {
			t.Fatalf("err = %v, want the terminal range text", err)
		}
		if strings.Contains(err.Error(), "run local-agent db upgrade") {
			t.Fatalf("err recommends db upgrade for a schema db upgrade refuses: %v", err)
		}
	}
	_ = preview
	_ = apply
}

// TestApplyQuarantineMarksRowsAndCompletesOnce is gate 2: apply with the
// previewed counts writes exactly N events and M stamps plus the completion
// row; a second preview reports already_applied and a replaying apply reports
// already_applied through the completion CAS.
func TestApplyQuarantineMarksRowsAndCompletesOnce(t *testing.T) {
	h := newUpgradeHarness(t)
	buildQuarantineFixture(t, h, completeQuarantineStateSeeds())

	report, err := h.application.ApplyLegacyIdentityQuarantine(ctx(), rollout.LegacyIdentityQuarantinePreview{
		JobsMatched: quarantineWantJobs, ActivationsMatched: quarantineWantActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.AlreadyApplied || report.JobsMarked != quarantineWantJobs || report.ActivationsMarked != quarantineWantActive {
		t.Fatalf("report = %+v, want %d/%d fresh marks", report, quarantineWantJobs, quarantineWantActive)
	}
	if report.AppliedAt.IsZero() {
		t.Fatal("report lacks completion timestamp")
	}

	reopen := openPlainDB(t, h.paths.DatabaseFile)
	var events int
	if err := reopen.QueryRow(`SELECT COUNT(*) FROM external_agent_job_events WHERE event_kind = 'legacy_result_identity'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	var stamped int
	if err := reopen.QueryRow(`SELECT COUNT(*) FROM external_agent_job_activations WHERE last_error_code = 'legacy_activation_content'`).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	_ = reopen.Close()
	if events != quarantineWantJobs || stamped != quarantineWantActive {
		t.Fatalf("marks = events:%d stamped:%d, want %d/%d", events, stamped, quarantineWantJobs, quarantineWantActive)
	}

	second, err := h.application.PreviewLegacyIdentityQuarantine(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if !second.AlreadyApplied || second.JobsMatched != 0 || second.ActivationsMatched != 0 || second.AppliedAt.IsZero() {
		t.Fatalf("second preview = %+v, want already_applied with zero counts", second)
	}

	// A second apply carrying the original previewed counts answers
	// already_applied from the durable marker before any recount (FIND-193):
	// an exact retry of the confirmed command exits clean instead of
	// surfacing a CAS mismatch for a finished operation.
	replayStale, err := h.application.ApplyLegacyIdentityQuarantine(ctx(), rollout.LegacyIdentityQuarantinePreview{
		JobsMatched: quarantineWantJobs, ActivationsMatched: quarantineWantActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replayStale.AlreadyApplied || replayStale.JobsMarked != 0 || replayStale.ActivationsMarked != 0 {
		t.Fatalf("stale replay = %+v, want already_applied with zero marks", replayStale)
	}
	// The replay reports the original durable completion timestamp, which the
	// marker stores at RFC3339 second precision.
	durableAt, present, err := adaptersqlite.FileLegacyIdentityQuarantine{}.ReadAppliedAt(ctx(), h.paths.DatabaseFile)
	if err != nil || !present {
		t.Fatalf("durable marker = (%v, %v, %v), want present", durableAt, present, err)
	}
	if !replayStale.AppliedAt.Equal(durableAt) {
		t.Fatalf("stale replay timestamp = %v, want the durable %v", replayStale.AppliedAt, durableAt)
	}
}

// TestApplyQuarantineMismatchFailsClosedWritingNothing is gate 3: a wrong
// expected count refuses inside the transaction and leaves no durable row of
// any kind behind. The mode=rw open itself legitimately flips the journal
// property (the driver's DSN sets it), so the assertion is over row data.
func TestApplyQuarantineMismatchFailsClosedWritingNothing(t *testing.T) {
	h := newUpgradeHarness(t)
	buildQuarantineFixture(t, h, completeQuarantineStateSeeds())
	beforeState := dumpRuntimeState(t, h.paths.DatabaseFile)

	_, err := h.application.ApplyLegacyIdentityQuarantine(ctx(), rollout.LegacyIdentityQuarantinePreview{
		JobsMatched: quarantineWantJobs + 1, ActivationsMatched: quarantineWantActive,
	})
	if err == nil || !errors.Is(err, rollout.ErrLegacyIdentityQuarantineMismatch) {
		t.Fatalf("err = %v, want ErrLegacyIdentityQuarantineMismatch", err)
	}
	if !strings.Contains(err.Error(), "expected jobs=3 activations=3") || !strings.Contains(err.Error(), "found jobs=2 activations=3") {
		t.Fatalf("mismatch text names neither side: %v", err)
	}
	assertZeroQuarantineRowWrites(t, h.paths.DatabaseFile)
	if after := dumpRuntimeState(t, h.paths.DatabaseFile); after != beforeState {
		t.Fatalf("runtime_state changed on refusal:\n%s\n%s", beforeState, after)
	}
}

// assertZeroQuarantineRowWrites counts every durable trace of a quarantine
// write and refuses any of them.
func assertZeroQuarantineRowWrites(t *testing.T, dbPath string) {
	t.Helper()
	plain := openPlainDB(t, dbPath)
	defer func() { _ = plain.Close() }()
	var events, stamped, markers int
	if err := plain.QueryRow(`SELECT COUNT(*) FROM external_agent_job_events WHERE event_kind = 'legacy_result_identity'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := plain.QueryRow(`SELECT COUNT(*) FROM external_agent_job_activations WHERE last_error_code = 'legacy_activation_content'`).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if err := plain.QueryRow(`SELECT COUNT(*) FROM runtime_state WHERE state_key = 'legacy_identity_quarantine_applied_at'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if events != 0 || stamped != 0 || markers != 0 {
		t.Fatalf("refused apply wrote rows: events=%d stamped=%d markers=%d", events, stamped, markers)
	}
}

// TestQuarantineRefusesBelowTargetSchemaEvenWithSeededPostflight is the
// FIND-192 gate: a v33 database carrying a seeded passing postflight and a
// cutoff must still refuse preview and apply with ErrPostflightNotPassed, and
// apply must never reach a write-capable open (journal and bytes intact).
func TestQuarantineRefusesBelowTargetSchemaEvenWithSeededPostflight(t *testing.T) {
	h := newUpgradeHarness(t)
	dbPath := h.paths.DatabaseFile
	replaceFixture(t, dbPath, 33, nil)
	seedRolloutKeys(t, dbPath, completeQuarantineStateSeeds())
	seedFind110Rows(t, dbPath, true)
	before := sha256File(t, dbPath)
	recorder := &countingQuarantineStore{inner: adaptersqlite.FileLegacyIdentityQuarantine{}, log: h.lockerLog}
	h.application.quarantineStore = recorder

	_, previewErr := h.application.PreviewLegacyIdentityQuarantine(ctx())
	if previewErr == nil || !errors.Is(previewErr, rollout.ErrPostflightNotPassed) {
		t.Fatalf("preview err = %v, want ErrPostflightNotPassed", previewErr)
	}
	if !strings.Contains(previewErr.Error(), postflightNotPassedQuarantineMessage) {
		t.Fatalf("preview err = %v, want the frozen quarantine postflight text", previewErr)
	}
	if strings.Contains(previewErr.Error(), terminalRangeText) {
		t.Fatalf("preview answered the terminal range text for an upgradable schema: %v", previewErr)
	}

	_, applyErr := h.application.ApplyLegacyIdentityQuarantine(ctx(), rollout.LegacyIdentityQuarantinePreview{
		JobsMatched: quarantineWantJobs, ActivationsMatched: quarantineWantActive,
	})
	if applyErr == nil || !errors.Is(applyErr, rollout.ErrPostflightNotPassed) {
		t.Fatalf("apply err = %v, want ErrPostflightNotPassed", applyErr)
	}
	if recorder.applies != 0 {
		t.Fatalf("quarantine apply reached the write-capable open %d times below target schema", recorder.applies)
	}
	assertFixtureUnchanged(t, dbPath, before)
	assertJournalMode(t, dbPath, "delete")
}

// TestApplyQuarantineRefusesFailedPostflightBeforeAnyWrite is gate 4: the
// read-only postflight check rejects under the lock, before any mode=rw work.
func TestApplyQuarantineRefusesFailedPostflightBeforeAnyWrite(t *testing.T) {
	h := newUpgradeHarness(t)
	state := completeQuarantineStateSeeds()
	state[keyPostStatus] = string(rollout.PostflightFailed)
	before := buildQuarantineFixture(t, h, state)
	recorder := &countingQuarantineStore{inner: adaptersqlite.FileLegacyIdentityQuarantine{}, log: h.lockerLog}
	h.application.quarantineStore = recorder

	_, err := h.application.ApplyLegacyIdentityQuarantine(ctx(), rollout.LegacyIdentityQuarantinePreview{
		JobsMatched: quarantineWantJobs, ActivationsMatched: quarantineWantActive,
	})
	if err == nil || !errors.Is(err, rollout.ErrPostflightNotPassed) {
		t.Fatalf("err = %v, want ErrPostflightNotPassed", err)
	}
	if !strings.Contains(err.Error(), postflightNotPassedQuarantineMessage) {
		t.Fatalf("err = %v, want the frozen quarantine postflight text", err)
	}
	if recorder.applies != 0 {
		t.Fatalf("quarantine apply ran %d times on a failed-postflight fixture", recorder.applies)
	}
	assertFixtureUnchanged(t, h.paths.DatabaseFile, before)
}

// TestDoctorPostApplyReportsIdentityInformational is gate 7: after apply, the
// identity checks pass with informational counts and nothing fails.
func TestDoctorPostApplyReportsIdentityInformational(t *testing.T) {
	h := newUpgradeHarness(t)
	// The doctor gate uses the fixture without the post-cutoff defects: those
	// are current defects that must keep failing doctor after the disposition.
	buildQuarantineFixtureWithFreshDefects(t, h, completeQuarantineStateSeeds(), false)

	if _, err := h.application.ApplyLegacyIdentityQuarantine(ctx(), rollout.LegacyIdentityQuarantinePreview{
		JobsMatched: quarantineWantJobs, ActivationsMatched: quarantineWantActive,
	}); err != nil {
		t.Fatal(err)
	}

	report, err := h.application.Doctor(ctx(), false)
	if err != nil {
		t.Fatal(err)
	}
	foundIdentity := false
	for _, result := range report.Results {
		switch result.Name {
		case "external-agent result identity":
			foundIdentity = true
			if result.Status != "pass" {
				t.Fatalf("result identity status = %q detail=%q", result.Status, result.Detail)
			}
			if !strings.Contains(result.Detail, "2 historical completed jobs without result identity") ||
				!strings.Contains(result.Detail, "3 historical activations without content") {
				t.Fatalf("informational counts missing: %q", result.Detail)
			}
		case "external-agent jobs":
			if result.Status == "fail" {
				t.Fatalf("external-agent jobs failed: %q", result.Detail)
			}
		}
	}
	if !foundIdentity {
		t.Fatalf("result identity check missing from report: %+v", report.Results)
	}
}

// TestOrdinaryCommandsRequireCompleteRollout is gate 10: reconcile,
// rebuild-index, and init each refuse a failed-postflight fixture with the
// shared rollout-incomplete message and never open mode=rw.
func TestOrdinaryCommandsRequireCompleteRollout(t *testing.T) {
	newFailedFixture := func(t *testing.T) (*upgradeHarness, string) {
		h := newUpgradeHarness(t)
		state := completeQuarantineStateSeeds()
		state[keyPostStatus] = string(rollout.PostflightFailed)
		before := buildQuarantineFixture(t, h, state)
		h.application.schemaTrace = h.lockerLog.add
		return h, before
	}
	assertNoWriteOpen := func(t *testing.T, h *upgradeHarness, before string) {
		t.Helper()
		if got := h.lockerLog.joined(); strings.Contains(got, "open-current") {
			t.Fatalf("recorded events = %q, want none after the refused preflight", got)
		}
		assertFixtureUnchanged(t, h.paths.DatabaseFile, before)
	}

	t.Run("jobs reconcile", func(t *testing.T) {
		h, before := newFailedFixture(t)
		_, err := h.application.ReconcileJob(ctx(), "missing-job", 0)
		if err == nil || !strings.Contains(err.Error(), rolloutIncompleteMessage) {
			t.Fatalf("err = %v, want %q", err, rolloutIncompleteMessage)
		}
		assertNoWriteOpen(t, h, before)
	})

	t.Run("knowledge rebuild-index", func(t *testing.T) {
		h, before := newFailedFixture(t)
		cfg, err := config.Load(h.paths.ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Orchestration.Knowledge.Enabled = true
		if err := config.Save(h.paths.ConfigFile, cfg); err != nil {
			t.Fatal(err)
		}

		_, err = h.application.RebuildKnowledgeIndexes(ctx())
		if err == nil || !strings.Contains(err.Error(), rolloutIncompleteMessage) {
			t.Fatalf("err = %v, want %q", err, rolloutIncompleteMessage)
		}
		assertNoWriteOpen(t, h, before)
	})

	t.Run("init existing database", func(t *testing.T) {
		h, before := newFailedFixture(t)
		if _, _, err := h.application.PrepareSetup(ctx()); err == nil {
			t.Fatal("init succeeded on a failed-postflight fixture")
		} else if !strings.Contains(err.Error(), rolloutIncompleteMessage) {
			t.Fatalf("err = %v, want %q", err, rolloutIncompleteMessage)
		}
		assertNoWriteOpen(t, h, before)
	})
}

// TestQuarantineCLICommands pin the command surface end to end: frozen preview
// output, confirmation flow, required expectation flags, and exit codes.
func TestQuarantineCLICommands(t *testing.T) {
	t.Run("preview prints frozen output and writes nothing", func(t *testing.T) {
		h := newUpgradeHarness(t)
		before := buildQuarantineFixture(t, h, completeQuarantineStateSeeds())

		code, out, stderr := executeUpgradeCLI(t, h, []string{"jobs", "quarantine-legacy-identity"}, "")
		if code != 0 {
			t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, stderr)
		}
		wantCutoff := time.Unix(0, quarantineCutoffAt).UTC().Format(time.RFC3339Nano)
		for _, want := range []string{
			"cutoff: " + wantCutoff,
			"jobs_matched: 2",
			"activations_matched: 3",
			"jobs predicate: status = 'completed'",
			"created_at <= cutoff",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("stdout = %q, want it to contain %q", out, want)
			}
		}
		assertFixtureUnchanged(t, h.paths.DatabaseFile, before)
	})

	t.Run("apply declines without writing", func(t *testing.T) {
		h := newUpgradeHarness(t)
		before := buildQuarantineFixture(t, h, completeQuarantineStateSeeds())

		code, out, _ := executeUpgradeCLI(t, h, []string{
			"jobs", "quarantine-legacy-identity",
			"--apply", "--expect-jobs-matched", "2", "--expect-activations-matched", "3",
		}, "n\n")
		if code != 0 {
			t.Fatalf("exit=%d stdout=%q", code, out)
		}
		if !strings.Contains(out, "Marcar 2 jobs y 3 activations legacy sin identidad de resultado como excepcion informativa.") ||
			!strings.Contains(out, "Operacion cancelada.") {
			t.Fatalf("stdout = %q, want prompt and cancellation", out)
		}
		assertFixtureUnchanged(t, h.paths.DatabaseFile, before)
	})

	t.Run("apply demands both expectation flags", func(t *testing.T) {
		h := newUpgradeHarness(t)
		buildQuarantineFixture(t, h, completeQuarantineStateSeeds())

		code, _, stderr := executeUpgradeCLI(t, h, []string{"jobs", "quarantine-legacy-identity", "--apply", "--yes"}, "")
		if code != 1 || !strings.Contains(stderr, "--expect-jobs-matched is required with --apply") {
			t.Fatalf("exit=%d stderr=%q, want the missing-flag failure", code, stderr)
		}
		code, _, stderr = executeUpgradeCLI(t, h, []string{
			"jobs", "quarantine-legacy-identity",
			"--apply", "--yes", "--expect-jobs-matched", "2",
		}, "")
		if code != 1 || !strings.Contains(stderr, "--expect-activations-matched is required with --apply") {
			t.Fatalf("exit=%d stderr=%q, want the second missing-flag failure", code, stderr)
		}
	})

	t.Run("apply yes completes and replays already_applied", func(t *testing.T) {
		h := newUpgradeHarness(t)
		buildQuarantineFixture(t, h, completeQuarantineStateSeeds())

		args := []string{
			"jobs", "quarantine-legacy-identity",
			"--apply", "--yes", "--expect-jobs-matched", "2", "--expect-activations-matched", "3",
		}
		code, out, stderr := executeUpgradeCLI(t, h, args, "")
		if code != 0 {
			t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, stderr)
		}
		for _, want := range []string{"jobs_marked: 2", "activations_marked: 3", "applied_at: "} {
			if !strings.Contains(out, want) {
				t.Fatalf("stdout = %q, want it to contain %q", out, want)
			}
		}

		// The exact retry with the same confirmed flags is idempotent
		// (FIND-193): exit 0 reporting the durable completion instead of a
		// CAS mismatch for an already-finished operation.
		code, out, stderr = executeUpgradeCLI(t, h, args, "")
		if code != 0 {
			t.Fatalf("replay exit=%d stdout=%q stderr=%q, want 0", code, out, stderr)
		}
		if !strings.Contains(out, "already_applied: true") {
			t.Fatalf("replay stdout = %q, want already_applied: true", out)
		}

		code, out, _ = executeUpgradeCLI(t, h, []string{"jobs", "quarantine-legacy-identity"}, "")
		if code != 0 || !strings.Contains(out, "already_applied: true") {
			t.Fatalf("second preview exit=%d stdout=%q, want already_applied", code, out)
		}
	})

	t.Run("apply mismatch exits one naming both sides", func(t *testing.T) {
		h := newUpgradeHarness(t)
		buildQuarantineFixture(t, h, completeQuarantineStateSeeds())

		code, _, stderr := executeUpgradeCLI(t, h, []string{
			"jobs", "quarantine-legacy-identity",
			"--apply", "--yes", "--expect-jobs-matched", "5", "--expect-activations-matched", "3",
		}, "")
		if code != 1 {
			t.Fatalf("exit=%d, want 1", code)
		}
		if !strings.Contains(stderr, "expected jobs=5 activations=3") || !strings.Contains(stderr, "found jobs=2 activations=3") {
			t.Fatalf("stderr = %q, want both count sides", stderr)
		}
	})
}

// buildDispositionRunFixture replaces dbPath with a fully rolled-out v41
// database carrying the FIND-110 rows and, optionally, the completion marker,
// then returns the exact bytes. journal_mode is delete so the gate can prove
// a refused run never flipped it.
func buildDispositionRunFixture(t *testing.T, dbPath string, marker bool) string {
	t.Helper()
	store, err := adaptersqlite.Create(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	plain := openPlainDB(t, dbPath)
	defer func() { _ = plain.Close() }()
	if _, err := plain.Exec(`DELETE FROM runtime_state`); err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Exec(`PRAGMA journal_mode = delete`); err != nil {
		t.Fatal(err)
	}
	state := completeQuarantineStateSeeds()
	for key, value := range state {
		if _, err := plain.Exec(
			`INSERT INTO runtime_state (state_key, state_value, updated_at) VALUES (?, ?, 1)`, key, value); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	if marker {
		if _, err := plain.Exec(
			`INSERT INTO runtime_state (state_key, state_value, updated_at) VALUES (?, ?, 1)`,
			rollout.KeyLegacyQuarantineAt, time.Unix(0, quarantineCutoffAt).UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	seedFind110Rows(t, dbPath, true)
	return sha256File(t, dbPath)
}

func assertJournalMode(t *testing.T, dbPath, want string) {
	t.Helper()
	plain := openPlainDB(t, dbPath)
	defer func() { _ = plain.Close() }()
	var mode string
	if err := plain.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != want {
		t.Fatalf("journal_mode=%q err=%v, want %s untouched", mode, err, want)
	}
}

// TestSeamRunPreflightRequiresDispositionMarkerBeforeOpenCurrent is gate 8:
// with the rollout complete but the disposition missing, run exits 1 with the
// frozen text and never opens the database: the trace records exactly
// lock,preflight,disposition,unlock (FIND-162/FIND-168 ordering).
func TestSeamRunPreflightRequiresDispositionMarkerBeforeOpenCurrent(t *testing.T) {
	application, log, dbPath := newSeamApplication(t)
	writeMinimalDefinitions(t, filepath.Join(application.root, ".local-agent"))
	before := buildDispositionRunFixture(t, dbPath, false)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-seam-token")
	t.Setenv("SLACK_APP_TOKEN", "xapp-seam-token")
	t.Setenv("SEAM_MODEL_KEY", "seam-model-key")

	err := application.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), dispositionIncompleteMessage) {
		t.Fatalf("err = %v, want %q", err, dispositionIncompleteMessage)
	}
	assertOrder(t, log, "preflight,disposition")
	assertFixtureUnchanged(t, dbPath, before)
	assertJournalMode(t, dbPath, "delete")
}

// TestSeamRunPostDispositionProceedsPastFullPreflight is gate 9: after the
// disposition completes, run passes both preflight halves and opens the store.
func TestSeamRunPostDispositionProceedsPastFullPreflight(t *testing.T) {
	application, log, dbPath := newSeamApplication(t)
	writeMinimalDefinitions(t, filepath.Join(application.root, ".local-agent"))
	buildDispositionRunFixture(t, dbPath, true)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-seam-token")
	t.Setenv("SLACK_APP_TOKEN", "xapp-seam-token")
	t.Setenv("SEAM_MODEL_KEY", "seam-model-key")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	realOpen := adaptersqlite.OpenCurrent
	application.openCurrent = func(openCtx context.Context, path string) (*adaptersqlite.Store, error) {
		opened, openErr := realOpen(openCtx, path)
		if openErr == nil {
			cancel()
		}
		return opened, openErr
	}

	runErr := application.Run(ctx)
	assertOrder(t, log, "preflight,disposition,open-current")
	if runErr != nil {
		t.Logf("run returned %v after deterministic cancellation", runErr)
	}
}

// TestCreateOriginatedDatabasePassesDispositionPreflight is gate 11: Create
// writes the marker at creation, so an init-born database never owes a
// disposition.
func TestCreateOriginatedDatabasePassesDispositionPreflight(t *testing.T) {
	application, log, dbPath := newSeamApplication(t)
	writeMinimalDefinitions(t, filepath.Join(application.root, ".local-agent"))
	store, err := adaptersqlite.Create(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
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
			cancel()
		}
		return opened, openErr
	}

	runErr := application.Run(ctx)
	assertOrder(t, log, "preflight,disposition,open-current")
	if runErr != nil {
		t.Logf("run returned %v after deterministic cancellation", runErr)
	}
}
