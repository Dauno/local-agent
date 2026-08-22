//go:build unix

package app

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

var adoptionFatalFields = []string{
	"JobsCompletedWithoutResultIdentity",
	"ActivationsWithoutContent",
	"NotificationsWithoutIdentity",
	"ActivationsWithoutIdentity",
	"ForegroundActivationsActive",
}

// TestAdoptionRefusalsCoverAllFiveFatalFields pins FIND-172/FIND-174: each
// nonzero postflight-fatal field refuses before any backup or write, leaves
// zero durable trace, and refuses identically on a second run.
func TestAdoptionRefusalsCoverAllFiveFatalFields(t *testing.T) {
	for _, field := range adoptionFatalFields {
		t.Run(field, func(t *testing.T) {
			h := newUpgradeHarness(t)
			adoptionFixture(t, h)
			dbPath := h.paths.DatabaseFile
			seedFatalJobField(t, dbPath, field)
			beforeState := dumpRuntimeState(t, dbPath)
			backupDir := t.TempDir()

			run := func() error {
				preview, err := h.application.PreviewDatabaseUpgrade(ctx(), rollout.UpgradeOptions{BackupDir: backupDir})
				if err != nil {
					return err
				}
				if preview.Kind != rollout.UpgradeAdoption {
					t.Fatalf("preview kind = %d, want Adoption", preview.Kind)
				}
				_, err = h.application.ApplyDatabaseUpgrade(ctx(), rollout.UpgradeOptions{BackupDir: backupDir}, preview)
				return err
			}

			firstErr := run()
			if firstErr == nil || !errors.Is(firstErr, rollout.ErrAdoptionUnsupportedIncompleteRows) {
				t.Fatalf("err = %v, want ErrAdoptionUnsupportedIncompleteRows", firstErr)
			}
			if !strings.Contains(firstErr.Error(), field+"=1") {
				t.Fatalf("err = %v, want it to name %s=1", firstErr, field)
			}
			for _, prefix := range []string{"backupper.into", "writer.record-baseline", "writer.postflight", "probe.capture"} {
				if h.logCounts(prefix) != 0 {
					t.Fatalf("%s ran on a refused adoption target", prefix)
				}
			}
			entries, readErr := os.ReadDir(backupDir)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("backup dir entries=%d err=%v, want none", len(entries), readErr)
			}
			if after := dumpRuntimeState(t, dbPath); after != beforeState {
				t.Fatalf("runtime_state changed on refusal:\n%s\n%s", beforeState, after)
			}
			h.resetLogs()
			secondErr := run()
			if secondErr == nil || firstErr.Error() != secondErr.Error() {
				t.Fatalf("second refusal = %v, want identical %v", secondErr, firstErr)
			}
		})
	}
}

func (h *upgradeHarness) resetLogs() {
	h.lockerLog.events = nil
	h.probeLog.events = nil
	h.backupLog.events = nil
	h.writerLog.events = nil
	h.writer.recordCalls = 0
	h.writer.migrateCalls = 0
	h.writer.postflights = nil
}

// corruptFixtureCases enumerates the exhaustive Corrupt table: every partial,
// malformed, or misplaced durable reading must fail closed everywhere, naming
// the keys involved.
func corruptFixtureCases() []struct {
	name    string
	version int
	seed    map[string]string
	wantKey string
} {
	const validTime = "2026-08-21T14:30:00Z"
	rowTwoBase := map[string]string{
		keyBaselineStr:    "jobs=1;activations=2",
		keyCutoffStr:      "12345",
		keyBackupPath:     "/tmp/valid-backup.db",
		keyBackupBytes:    "4096",
		keyBackupSHA:      testSHAValid,
		keyBackupSource:   "33",
		keyBackupVerified: validTime,
	}
	partialIdentity := map[string]string{
		keyBaselineStr:  "jobs=1;activations=2",
		keyCutoffStr:    "12345",
		keyBackupPath:   "/tmp/valid-backup.db",
		keyBackupBytes:  "4096",
		keyBackupSHA:    testSHAValid,
		keyBackupSource: "33",
	}
	return []struct {
		name    string
		version int
		seed    map[string]string
		wantKey string
	}{
		{"baseline-without-cutoff", 41, map[string]string{keyBaselineStr: "jobs=0;activations=0"}, keyBaselineStr},
		{"cutoff-without-baseline", 41, map[string]string{keyCutoffStr: "12345"}, keyCutoffStr},
		{"malformed-baseline", 41, map[string]string{keyBaselineStr: "bogus"}, keyBaselineStr},
		{"malformed-cutoff", 41, map[string]string{keyCutoffStr: "soon"}, keyCutoffStr},
		{"partial-backup-identity", 41, partialIdentity, keyBackupVerified},
		{"marker-beside-identity", 41, appendSeed(rowTwoBase, keyNotRequired, validTime), keyNotRequired},
		{"relative-backup-path", 41, merge(rowTwoBase, map[string]string{keyBackupPath: "relative.db"}), keyBackupPath},
		{"malformed-bytes", 41, merge(rowTwoBase, map[string]string{keyBackupBytes: "big"}), keyBackupBytes},
		{"short-sha", 41, merge(rowTwoBase, map[string]string{keyBackupSHA: "abc"}), keyBackupSHA},
		{"source-version-zero", 41, merge(rowTwoBase, map[string]string{keyBackupSource: "0"}), keyBackupSource},
		{"bad-verified-at", 41, merge(rowTwoBase, map[string]string{keyBackupVerified: "not-a-time"}), keyBackupVerified},
		{"bad-not-required-at", 41, map[string]string{keyNotRequired: "not-a-time"}, keyNotRequired},
		{"unknown-postflight", 41, map[string]string{keyPostStatus: "weird"}, keyPostStatus},
		{"status-without-detail", 41, map[string]string{keyPostStatus: "passed"}, keyPostStatus},
		{"detail-without-status", 41, map[string]string{keyPostDetail: "orphan detail"}, keyPostDetail},
		{"identity-without-baseline", 41, map[string]string{
			keyBackupPath: "/tmp/x.db", keyBackupBytes: "10", keyBackupSHA: testSHAValid,
			keyBackupSource: "41", keyBackupVerified: validTime,
		}, keyBaselineStr},
		{"misplaced-valid-status-on-row-two", 33, appendSeed(
			appendSeed(rowTwoBase, keyPostStatus, "passed"), keyPostDetail, "detail"), keyPostStatus},
		{"not-required-below-target", 33, map[string]string{keyNotRequired: validTime}, keyNotRequired},
	}
}

func merge(base, overlay map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

func appendSeed(base map[string]string, key, value string) map[string]string {
	return merge(base, map[string]string{key: value})
}

func TestCorruptShapesRefusedByPreviewAndRunPreflight(t *testing.T) {
	for _, testCase := range corruptFixtureCases() {
		t.Run(testCase.name, func(t *testing.T) {
			h := newUpgradeHarness(t)
			replaceFixture(t, h.paths.DatabaseFile, testCase.version, testCase.seed)
			dbPath := h.paths.DatabaseFile

			_, previewErr := h.application.PreviewDatabaseUpgrade(ctx(), rollout.UpgradeOptions{})
			if !errors.Is(previewErr, rollout.ErrRolloutStateCorrupt) {
				t.Fatalf("preview err = %v, want ErrRolloutStateCorrupt", previewErr)
			}
			preflightErr := h.application.requireRolloutComplete(ctx(), dbPath)
			if !errors.Is(preflightErr, rollout.ErrRolloutStateCorrupt) {
				t.Fatalf("preflight err = %v, want ErrRolloutStateCorrupt", preflightErr)
			}
			for _, err := range []error{previewErr, preflightErr} {
				if !strings.Contains(err.Error(), testCase.wantKey) {
					t.Fatalf("err = %v, want it to name the failing key %s", err, testCase.wantKey)
				}
			}
		})
	}
}

func TestOutOfRangeSchemasRefuseBeforeLockOrBackup(t *testing.T) {
	for _, version := range []int{20, 14, 0} {
		t.Run("v"+strconv.Itoa(version), func(t *testing.T) {
			h := newUpgradeHarness(t)
			replaceFixture(t, h.paths.DatabaseFile, version, nil)

			_, previewErr := h.application.PreviewDatabaseUpgrade(ctx(), rollout.UpgradeOptions{})
			var unsupported rollout.UnsupportedSourceSchemaError
			if !errors.As(previewErr, &unsupported) || unsupported.Found != version ||
				unsupported.MinSupported != 33 || unsupported.MaxSupported != 40 {
				t.Fatalf("preview err = %v (%T), want UnsupportedSourceSchemaError{%d,[33,40]}", previewErr, previewErr, version)
			}
			preflightErr := h.application.requireRolloutComplete(ctx(), h.paths.DatabaseFile)
			if !errors.As(preflightErr, &unsupported) || unsupported.Found != version {
				t.Fatalf("preflight err = %v, want the same typed refusal", preflightErr)
			}
			if h.lockerLog.count("lock:") != 0 || h.backupLog.count("backupper.into") != 0 {
				t.Fatalf("refusal reached lock or backup: %q", h.lockerLog.joined())
			}
		})
	}
}

func TestConcurrentReplacementMutatesNothing(t *testing.T) {
	for _, replacement := range []struct {
		version int
		wantErr error
	}{
		{32, rollout.ErrUnsupportedSourceSchema},
		{42, rollout.ErrFutureSchema},
	} {
		t.Run("replaced-with-v"+strconv.Itoa(replacement.version), func(t *testing.T) {
			h := newUpgradeHarness(t)
			dbPath := h.paths.DatabaseFile
			replaceFixture(t, dbPath, 33, nil)
			preview, err := h.application.PreviewDatabaseUpgrade(ctx(), rollout.UpgradeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			replaceFixture(t, dbPath, replacement.version, nil)

			_, applyErr := h.application.ApplyDatabaseUpgrade(ctx(), rollout.UpgradeOptions{}, preview)
			if !errors.Is(applyErr, replacement.wantErr) {
				t.Fatalf("apply err = %v, want %v after replacement", applyErr, replacement.wantErr)
			}
			for _, prefix := range []string{"backupper.into", "backupper.verify"} {
				if h.backupLog.count(prefix) != 0 {
					t.Fatalf("%s ran after replacement", prefix)
				}
			}
			for _, prefix := range []string{"writer.record-baseline", "writer.migrate", "writer.postflight"} {
				if h.writerLog.count(prefix) != 0 {
					t.Fatalf("%s ran after replacement", prefix)
				}
			}
			if got := queryUserVersion(t, dbPath); got != replacement.version {
				t.Fatalf("user_version = %d, want untouched %d", got, replacement.version)
			}
		})
	}
}

func TestConcurrentCompletionReportedWithoutMutation(t *testing.T) {
	h := newUpgradeHarness(t)
	dbPath := h.paths.DatabaseFile
	replaceFixture(t, dbPath, 33, nil)
	preview, err := h.application.PreviewDatabaseUpgrade(ctx(), rollout.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Another process finished the rollout between preview and apply.
	rowFourFixtureWithPassingPostflight(t, h)

	report, applyErr := h.application.ApplyDatabaseUpgrade(ctx(), rollout.UpgradeOptions{}, preview)
	if applyErr != nil {
		t.Fatalf("apply err = %v, want the concurrent-outcome report", applyErr)
	}
	if report.RolloutAdvanced || report.Kind != rollout.UpgradeAlreadyComplete {
		t.Fatalf("report = %+v, want AlreadyComplete with RolloutAdvanced=false", report)
	}
	if h.backupLog.count("backupper.into") != 0 || h.writerLog.count("writer.migrate") != 0 {
		t.Fatal("concurrent outcome mutated the database")
	}
}

func rowFourFixtureWithPassingPostflight(t *testing.T, h *upgradeHarness) {
	t.Helper()
	replaceFixture(t, h.paths.DatabaseFile, 41, map[string]string{
		keyBaselineStr: "jobs=0;activations=0",
		keyCutoffStr:   "5",
		keyNotRequired: "2026-08-21T14:30:00Z",
		keyPostStatus:  "passed",
		keyPostDetail:  "concurrent completion",
	})
}

// TestCreateOriginatedFilePassesPreflightImmediately closes FIND-161's
// acceptance guarantee at composition level: no db upgrade step may sit
// between init and run's own preflight.
func TestCreateOriginatedFilePassesPreflightImmediately(t *testing.T) {
	h := newUpgradeHarness(t)
	dbPath := h.paths.DatabaseFile
	if err := h.application.requireRolloutComplete(context.Background(), dbPath); err != nil {
		t.Fatalf("preflight on a fresh Create file: %v", err)
	}
	preview, err := h.application.PreviewDatabaseUpgrade(context.Background(), rollout.UpgradeOptions{})
	if err != nil || preview.Kind != rollout.UpgradeAlreadyComplete {
		t.Fatalf("preview = %+v err=%v, want AlreadyComplete", preview, err)
	}
	state, stateErr := adaptersqlite.FileSchemaProbe{}.ReadRolloutState(context.Background(), dbPath)
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	shape, shapeErr := rollout.ClassifyBackupIdentity(state)
	if shapeErr != nil || shape != rollout.BackupIdentityNotRequired {
		t.Fatalf("shape=%d err=%v, want NotRequired", shape, shapeErr)
	}
}

// TestRunPreflightRejectsFailedPostflightBeforeOpenCurrent pins FIND-162's
// corrected ordering at the composition entry: the schema/rollout half of
// run's preflight runs under the lock and refuses before any mode=rw open,
// leaving journal_mode untouched.
func TestRunPreflightRejectsFailedPostflightBeforeOpenCurrent(t *testing.T) {
	h := newUpgradeHarness(t)
	dbPath := h.paths.DatabaseFile
	replaceFixture(t, dbPath, 41, map[string]string{
		keyBaselineStr: "jobs=0;activations=0",
		keyCutoffStr:   "5",
		keyNotRequired: "2026-08-21T14:30:00Z",
		keyPostStatus:  "failed",
		keyPostDetail:  "regression",
	})
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-seam-token")
	t.Setenv("SLACK_APP_TOKEN", "xapp-seam-token")
	t.Setenv("DEEPSEEK_API_KEY", "seam-model-key")
	h.application.schemaTrace = h.lockerLog.add

	runErr := h.application.Run(context.Background())
	if runErr == nil || !strings.Contains(runErr.Error(), rolloutIncompleteMessage) {
		t.Fatalf("run err = %v, want %q", runErr, rolloutIncompleteMessage)
	}
	events := h.lockerLog.joined()
	if strings.Contains(events, "open-current") {
		t.Fatalf("OpenCurrent ran despite the preflight refusal: %q", events)
	}
	if !strings.Contains(events, "lock:") || !strings.Contains(events, ",preflight,") {
		t.Fatalf("events = %q, want lock then preflight", events)
	}
	if got := queryUserVersion(t, dbPath); got != 41 {
		t.Fatalf("user_version = %d", got)
	}
	plain, plainErr := sqlOpenPlain(dbPath)
	if plainErr != nil {
		t.Fatal(plainErr)
	}
	defer plain.Close()
	var mode string
	if err := plain.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
		t.Fatalf("journal_mode=%q err=%v, want delete untouched by the refusal", mode, err)
	}
}
