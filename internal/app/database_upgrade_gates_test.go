//go:build unix

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

const (
	keyBaselineStr    = rollout.KeyBaseline
	keyCutoffStr      = rollout.KeyCutoff
	keyPostStatus     = rollout.KeyPostflightStatus
	keyPostDetail     = rollout.KeyPostflightDetail
	keyNotRequired    = rollout.KeyBackupNotRequiredAt
	keyBackupPath     = rollout.KeyBackupPath
	keyBackupBytes    = rollout.KeyBackupBytes
	keyBackupSHA      = rollout.KeyBackupSHA256
	keyBackupSource   = rollout.KeyBackupSourceVersion
	keyBackupVerified = rollout.KeyBackupVerifiedAt
)

func ctx() context.Context { return context.Background() }

func rowOneFixture(t *testing.T, h *upgradeHarness) {
	t.Helper()
	replaceFixture(t, h.paths.DatabaseFile, 33, nil)
}

func rowTwoFixture(t *testing.T, h *upgradeHarness, overrides map[string]string) rollout.BackupIdentity {
	t.Helper()
	dbPath := h.paths.DatabaseFile
	replaceFixture(t, dbPath, 33, nil)
	identity, err := adaptersqlite.FileDatabaseBackupper{}.BackupInto(ctx(), dbPath, filepath.Join(t.TempDir(), "rowtwo-backup.db"))
	if err != nil {
		t.Fatalf("seed row-2 artifact: %v", err)
	}
	seed := map[string]string{
		keyBaselineStr:    "jobs=0;activations=0",
		keyCutoffStr:      "12345",
		keyBackupPath:     identity.Path,
		keyBackupBytes:    strconv.FormatInt(identity.Bytes, 10),
		keyBackupSHA:      identity.SHA256,
		keyBackupSource:   "33",
		keyBackupVerified: identity.VerifiedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	maps.Copy(seed, overrides)
	seedRolloutKeys(t, dbPath, seed)
	return identity
}

func rowFourFixture(t *testing.T, h *upgradeHarness, postflight map[string]string) {
	t.Helper()
	seed := map[string]string{
		keyBaselineStr: "jobs=0;activations=0",
		keyCutoffStr:   "999",
		keyNotRequired: "2026-08-21T14:30:00Z",
	}
	maps.Copy(seed, postflight)
	replaceFixture(t, h.paths.DatabaseFile, rollout.TargetVersion, seed)
}

func adoptionFixture(t *testing.T, h *upgradeHarness) {
	t.Helper()
	replaceFixture(t, h.paths.DatabaseFile, rollout.TargetVersion, nil)
}

func seedRolloutKeys(t *testing.T, dbPath string, seed map[string]string) {
	t.Helper()
	plain, err := sqlOpenPlain(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	for key, value := range seed {
		if _, err := plain.Exec(
			`INSERT INTO runtime_state (state_key, state_value, updated_at) VALUES (?, ?, 1)
			ON CONFLICT (state_key) DO UPDATE SET state_value = excluded.state_value`, key, value); err != nil {
			t.Fatalf("seed %s=%s: %v", key, value, err)
		}
	}
}

// TestApplyRowOneFullRunPinsFrozenOrdering covers the confirmed row-1 run:
// v41 reached, 0600 backup passing its own integrity check, five backup keys
// matching size and hand-computed SHA-256, capture-once-before-backup under
// the lock, postflight passed, and journal_mode flipping only after the
// backup step (FIND-155/FIND-180).
func TestApplyRowOneFullRunPinsFrozenOrdering(t *testing.T) {
	h := newUpgradeHarness(t)
	rowOneFixture(t, h)
	dbPath := h.paths.DatabaseFile

	preview, err := h.application.PreviewDatabaseUpgrade(ctx(), rollout.UpgradeOptions{})
	if err != nil || preview.Kind != rollout.UpgradeFreshUpgrade || preview.FromVersion != 33 {
		t.Fatalf("preview = %+v err=%v", preview, err)
	}
	if h.lockerLog.count("lock:") != 0 {
		t.Fatalf("preview took a lock: %q", h.lockerLog.joined())
	}

	report, err := h.application.ApplyDatabaseUpgrade(ctx(), rollout.UpgradeOptions{}, preview)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !report.RolloutAdvanced || report.ToVersion != 45 || !report.PostflightOK {
		t.Fatalf("report = %+v", report)
	}
	if got := queryUserVersion(t, dbPath); got != 45 {
		t.Fatalf("user_version = %d, want 45", got)
	}

	// Call-order gate: capture once, under the lock, strictly before the
	// first backup call.
	events := h.lockerLog.joined()
	lockIdx := strings.Index(events, "lock:")
	captureIdx := strings.Index(events, "probe.capture")
	intoIdx := strings.Index(events, "backupper.into")
	if lockIdx < 0 || captureIdx < lockIdx || intoIdx < captureIdx {
		t.Fatalf("ordering broken: %s | %s | %s", h.lockerLog.joined(), h.probeLog.joined(), h.backupLog.joined())
	}
	if h.probeLog.count("probe.capture") != 1 {
		t.Fatalf("capture calls = %d, want exactly one", h.probeLog.count("probe.capture"))
	}
	if h.writer.recordCalls != 1 || h.writer.migrateCalls != 1 || len(h.writer.postflights) != 1 {
		t.Fatalf("writer calls: record=%d migrate=%d postflight=%v", h.writer.recordCalls, h.writer.migrateCalls, h.writer.postflights)
	}
	// FIND-155: the journal flip happens at the baseline write, after the
	// backup verification, never before; record-baseline precedes migrate.
	if got := h.backupLog.lastWithPrefix("journal.after-verify="); got != "journal.after-verify=delete" {
		t.Fatalf("%q, want the pre-run journal value right after verification", got)
	}
	recordIdx := strings.Index(h.writerLog.joined(), "writer.record-baseline")
	migrateIdx := strings.Index(h.writerLog.joined(), "writer.migrate")
	if recordIdx < 0 || migrateIdx < recordIdx {
		t.Fatalf("record-baseline must precede migrate: %q", h.writerLog.joined())
	}
	if final := journalModeOf(dbPath); final != "wal" {
		t.Fatalf("final journal_mode = %q, want wal after the rollout write", final)
	}
	if h.lockerLog.count("lock:") != 1 || h.lockerLog.count("unlock") != 1 {
		t.Fatalf("lock events = %q", h.lockerLog.joined())
	}

	// Backup artifact gate: 0600, integrity ok, keys match reality.
	state, err := adaptersqlite.FileSchemaProbe{}.ReadRolloutState(ctx(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(state.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %04o", info.Mode().Perm())
	}
	data, err := os.ReadFile(state.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if state.BackupSHA256 != hex.EncodeToString(sum[:]) || state.BackupBytes != int64(len(data)) {
		t.Fatalf("identity keys do not match the artifact bytes")
	}
	if state.BackupSourceVersion != 33 {
		t.Fatalf("source version = %d, want 33", state.BackupSourceVersion)
	}
	if state.PostflightStatus != rollout.PostflightPassed {
		t.Fatalf("postflight = %q", state.PostflightStatus)
	}
	var keyCount int
	plain, _ := sqlOpenPlain(dbPath)
	defer func() { _ = plain.Close() }()
	for _, key := range []string{keyBaselineStr, keyCutoffStr, keyBackupPath, keyBackupBytes, keyBackupSHA, keyBackupSource, keyBackupVerified, keyPostStatus, keyPostDetail} {
		if err := plain.QueryRow(`SELECT COUNT(*) FROM runtime_state WHERE state_key = ?`, key).Scan(&keyCount); err != nil || keyCount != 1 {
			t.Fatalf("key %s rows=%d err=%v, want exactly one durable row", key, keyCount, err)
		}
	}
}

// TestApplySecondRunAfterSuccessIsPreviewOnlyNoLock pins FIND-159's no-op
// path: AlreadyComplete previews without prompts, without the lock, and
// without touching the cutoff row.
func TestApplySecondRunAfterSuccessIsPreviewOnlyNoLock(t *testing.T) {
	h := newUpgradeHarness(t)
	rowOneFixture(t, h)
	opts := rollout.UpgradeOptions{}
	preview, err := h.application.PreviewDatabaseUpgrade(ctx(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.application.ApplyDatabaseUpgrade(ctx(), opts, preview); err != nil {
		t.Fatalf("first run: %v", err)
	}

	h.lockerLog.events = nil
	var cutoffUpdatedAt int64
	plain, _ := sqlOpenPlain(h.paths.DatabaseFile)
	if err := plain.QueryRow(`SELECT updated_at FROM runtime_state WHERE state_key = ?`, keyCutoffStr).Scan(&cutoffUpdatedAt); err != nil {
		t.Fatal(err)
	}
	_ = plain.Close()

	second, err := h.application.PreviewDatabaseUpgrade(ctx(), opts)
	if err != nil || second.Kind != rollout.UpgradeAlreadyComplete || second.ResolvedBackupDir != "" {
		t.Fatalf("second preview = %+v err=%v, want AlreadyComplete with unresolved backup dir", second, err)
	}
	if h.lockerLog.count("lock:") != 0 {
		t.Fatalf("no-op preview took the lock: %q", h.lockerLog.joined())
	}
	plain, _ = sqlOpenPlain(h.paths.DatabaseFile)
	defer func() { _ = plain.Close() }()
	var afterUpdatedAt int64
	if err := plain.QueryRow(`SELECT updated_at FROM runtime_state WHERE state_key = ?`, keyCutoffStr).Scan(&afterUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if afterUpdatedAt != cutoffUpdatedAt {
		t.Fatalf("cutoff updated_at moved %d -> %d on the no-op run", cutoffUpdatedAt, afterUpdatedAt)
	}
}

func TestRowTwoIntactResumeSkipsCaptureBackupAndRecord(t *testing.T) {
	h := newUpgradeHarness(t)
	identity := rowTwoFixture(t, h, nil)
	dbPath := h.paths.DatabaseFile

	var cutoffUpdatedAt int64
	plain, _ := sqlOpenPlain(dbPath)
	if err := plain.QueryRow(`SELECT updated_at FROM runtime_state WHERE state_key = ?`, keyCutoffStr).Scan(&cutoffUpdatedAt); err != nil {
		t.Fatal(err)
	}
	_ = plain.Close()

	preview, err := h.application.PreviewDatabaseUpgrade(ctx(), rollout.UpgradeOptions{})
	if err != nil || preview.Kind != rollout.UpgradeFreshUpgrade {
		t.Fatalf("preview = %+v err=%v, want FreshUpgrade (resumed)", preview, err)
	}
	report, err := h.application.ApplyDatabaseUpgrade(ctx(), rollout.UpgradeOptions{}, preview)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if h.backupLog.count("backupper.verify") != 1 || h.backupLog.count("backupper.into") != 0 {
		t.Fatalf("backup events = %q", h.backupLog.joined())
	}
	if h.probeLog.count("probe.capture") != 0 || h.writer.recordCalls != 0 {
		t.Fatalf("resume must reuse durable metadata: capture=%d record=%d", h.probeLog.count("probe.capture"), h.writer.recordCalls)
	}
	if h.writer.migrateCalls != 1 {
		t.Fatalf("migrate calls = %d", h.writer.migrateCalls)
	}
	if report.Backup.Path != identity.Path {
		t.Fatalf("report backup = %+v, want the revalidated record", report.Backup)
	}
	if got := queryUserVersion(t, dbPath); got != 45 {
		t.Fatalf("user_version = %d", got)
	}
	plain, _ = sqlOpenPlain(dbPath)
	defer func() { _ = plain.Close() }()
	var after int64
	if err := plain.QueryRow(`SELECT updated_at FROM runtime_state WHERE state_key = ?`, keyCutoffStr).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != cutoffUpdatedAt {
		t.Fatalf("intact resume rewrote the cutoff row")
	}
}

func TestRowTwoFallbackReplacesMissingOrTamperedArtifact(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, identity rollout.BackupIdentity, overrides map[string]string)
	}{
		{
			name: "deleted artifact",
			mutate: func(t *testing.T, identity rollout.BackupIdentity, _ map[string]string) {
				if err := os.Remove(identity.Path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "tampered sha",
			mutate: func(t *testing.T, _ rollout.BackupIdentity, overrides map[string]string) {
				overrides[keyBackupSHA] = testSHAOther
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newUpgradeHarness(t)
			overrides := map[string]string{}
			identity := rowTwoFixtureWithOverrides(t, h, overrides, testCase.mutate)
			_ = identity

			preview, err := h.application.PreviewDatabaseUpgrade(ctx(), rollout.UpgradeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.application.ApplyDatabaseUpgrade(ctx(), rollout.UpgradeOptions{}, preview); err != nil {
				t.Fatalf("fallback run: %v", err)
			}
			if h.backupLog.count("backupper.verify") != 1 || h.backupLog.count("backupper.into") != 1 {
				t.Fatalf("backup events = %q", h.backupLog.joined())
			}
			if h.writer.recordCalls != 1 {
				t.Fatalf("record-baseline calls = %d, want the identity overwrite", h.writer.recordCalls)
			}
			state, err := adaptersqlite.FileSchemaProbe{}.ReadRolloutState(ctx(), h.paths.DatabaseFile)
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := os.ReadFile(state.BackupPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			sum := sha256.Sum256(data)
			if state.BackupSHA256 != hex.EncodeToString(sum[:]) {
				t.Fatal("replacement identity does not describe the fresh artifact")
			}
		})
	}
}

func rowTwoFixtureWithOverrides(t *testing.T, h *upgradeHarness, overrides map[string]string, mutate func(*testing.T, rollout.BackupIdentity, map[string]string)) rollout.BackupIdentity {
	t.Helper()
	identity := rowTwoFixture(t, h, nil)
	if mutate != nil {
		mutate(t, identity, overrides)
		// A mutation may only touch the durable record AFTER seeding;
		// re-seed the overridden keys so the fixture really carries them.
		if len(overrides) > 0 {
			seedRolloutKeys(t, h.paths.DatabaseFile, overrides)
		}
	}
	return identity
}

// TestManipulatedSourceVersionFailsClosedEverywhere pins FIND-177: a fully
// valid five-key identity naming another source version is Corrupt from
// Preview and from Apply's step-7 re-read, and never reaches revalidation.
func TestManipulatedSourceVersionFailsClosedEverywhere(t *testing.T) {
	h := newUpgradeHarness(t)
	rowTwoFixture(t, h, map[string]string{keyBackupSource: "30"})
	dbPath := h.paths.DatabaseFile

	_, previewErr := h.application.PreviewDatabaseUpgrade(ctx(), rollout.UpgradeOptions{})
	if !errors.Is(previewErr, rollout.ErrRolloutStateCorrupt) || !strings.Contains(previewErr.Error(), "30") || !strings.Contains(previewErr.Error(), "33") {
		t.Fatalf("preview err = %v, want Corrupt naming recorded 30 and live 33", previewErr)
	}
	preview := rollout.UpgradePreview{Kind: rollout.UpgradeFreshUpgrade, FromVersion: 33, ToVersion: rollout.TargetVersion, DatabasePath: dbPath}
	_, applyErr := h.application.ApplyDatabaseUpgrade(ctx(), rollout.UpgradeOptions{}, preview)
	if !errors.Is(applyErr, rollout.ErrRolloutStateCorrupt) {
		t.Fatalf("apply err = %v, want Corrupt from the re-read", applyErr)
	}
	for _, prefix := range []string{"backupper.verify", "backupper.into", "writer.migrate", "writer.postflight"} {
		if h.logCounts(prefix) != 0 {
			t.Fatalf("%s called on the manipulated fixture", prefix)
		}
	}
}

func (h *upgradeHarness) logCounts(prefix string) int {
	return h.backupLog.count(prefix) + h.writerLog.count(prefix)
}

func TestResumeNeededRunsPostflightOnly(t *testing.T) {
	h := newUpgradeHarness(t)
	rowFourFixture(t, h, nil)

	preview, err := h.application.PreviewDatabaseUpgrade(ctx(), rollout.UpgradeOptions{BackupDir: "/definitely/not/here"})
	if err != nil || preview.Kind != rollout.UpgradeResumeNeeded || preview.ResolvedBackupDir != "" {
		t.Fatalf("preview = %+v err=%v, want ResumeNeeded with unresolved backup dir (FIND-164)", preview, err)
	}
	report, err := h.application.ApplyDatabaseUpgrade(ctx(), rollout.UpgradeOptions{}, preview)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if h.backupLog.count("backupper.into") != 0 || h.writer.migrateCalls != 0 || h.writer.recordCalls != 0 {
		t.Fatalf("row 4 must not back up or migrate: %q", h.writerLog.joined())
	}
	if len(h.writer.postflights) != 1 || h.writer.postflights[0] != "passed" {
		t.Fatalf("postflights = %v", h.writer.postflights)
	}
	if report.Kind != rollout.UpgradeResumeNeeded || !report.RolloutAdvanced {
		t.Fatalf("report = %+v", report)
	}
}

func TestAdoptionWritesFixedZeroWithoutMeasuringOrMigrating(t *testing.T) {
	h := newUpgradeHarness(t)
	adoptionFixture(t, h)
	backupDir := t.TempDir()

	preview, err := h.application.PreviewDatabaseUpgrade(ctx(), rollout.UpgradeOptions{BackupDir: backupDir})
	if err != nil || preview.Kind != rollout.UpgradeAdoption {
		t.Fatalf("preview = %+v err=%v", preview, err)
	}
	report, err := h.application.ApplyDatabaseUpgrade(ctx(), rollout.UpgradeOptions{BackupDir: backupDir}, preview)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if h.probeLog.count("probe.capture") != 0 {
		t.Fatalf("adoption measured a live baseline: %q", h.probeLog.joined())
	}
	if h.writer.recordCalls != 1 || h.writer.lastBaseline != (rollout.IdentityBaseline{}) || h.writer.lastCutoff != 0 {
		t.Fatalf("fixed-zero baseline violated: %+v cutoff=%d", h.writer.lastBaseline, h.writer.lastCutoff)
	}
	if h.writer.migrateCalls != 0 {
		t.Fatal("adoption migrated although the schema was already at target")
	}
	if report.Backup.Path == "" {
		t.Fatal("adoption must still produce a verified backup")
	}
	if got := queryUserVersion(t, h.paths.DatabaseFile); got != 45 {
		t.Fatalf("user_version moved to %d on adoption", got)
	}

	// Gate 7 (row 3): the five durable identity keys must describe the real
	// artifact: actual byte size and a hand-computed SHA-256.
	state, stateErr := adaptersqlite.FileSchemaProbe{}.ReadRolloutState(ctx(), h.paths.DatabaseFile)
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	data, readErr := os.ReadFile(state.BackupPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	sum := sha256.Sum256(data)
	if state.BackupBytes != int64(len(data)) || state.BackupSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("adoption identity keys do not match the backup bytes")
	}
	if state.BackupSourceVersion != 45 {
		t.Fatalf("adoption source version = %d, want 45", state.BackupSourceVersion)
	}
}

// truncatedBackupper simulates the injected verification failure: it leaves
// a partial artifact behind and reports ErrBackupVerificationFailed exactly
// like the real backupper would when its own verify pass fails.
type truncatedBackupper struct {
	inner rollout.DatabaseBackupper
	log   *upgradeLog
}

func (b *truncatedBackupper) BackupInto(ctx context.Context, srcPath, destPath string) (rollout.BackupIdentity, error) {
	b.log.add("backupper.into")
	if writeErr := os.WriteFile(destPath, []byte("truncated"), 0o600); writeErr != nil {
		return rollout.BackupIdentity{}, writeErr
	}
	return rollout.BackupIdentity{}, fmt.Errorf("%w: simulated truncated artifact", rollout.ErrBackupVerificationFailed)
}

func (b *truncatedBackupper) VerifyBackup(ctx context.Context, backupPath string, wantSourceVersion int) (rollout.BackupIdentity, error) {
	return b.inner.VerifyBackup(ctx, backupPath, wantSourceVersion)
}

// TestTruncatedFakeBackupperAbortsBeforeAnyWrite pins gate 4: the injected
// verification failure stops the run before RecordBaselineAndCutoff, keeps
// user_version untouched, and leaves no partial artifact behind.
func TestTruncatedFakeBackupperAbortsBeforeAnyWrite(t *testing.T) {
	h := newUpgradeHarness(t)
	rowOneFixture(t, h)
	backupDir := t.TempDir()
	h.application.schemaBackupper = &truncatedBackupper{inner: adaptersqlite.FileDatabaseBackupper{}, log: h.backupLog}

	preview, err := h.application.PreviewDatabaseUpgrade(ctx(), rollout.UpgradeOptions{BackupDir: backupDir})
	if err != nil {
		t.Fatal(err)
	}
	_, applyErr := h.application.ApplyDatabaseUpgrade(ctx(), rollout.UpgradeOptions{BackupDir: backupDir}, preview)
	if !errors.Is(applyErr, rollout.ErrBackupVerificationFailed) {
		t.Fatalf("apply err = %v, want ErrBackupVerificationFailed", applyErr)
	}
	if h.writer.recordCalls != 0 || h.writer.migrateCalls != 0 || len(h.writer.postflights) != 0 {
		t.Fatalf("writer ran despite the backup failure: record=%d migrate=%d postflight=%v",
			h.writer.recordCalls, h.writer.migrateCalls, h.writer.postflights)
	}
	if got := queryUserVersion(t, h.paths.DatabaseFile); got != 33 {
		t.Fatalf("user_version = %d, want untouched 33", got)
	}
	entries, readErr := os.ReadDir(backupDir)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("partial backup survived: entries=%d err=%v", len(entries), readErr)
	}
}
