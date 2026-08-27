package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

const rolloutIncompleteMessage = "the schema rollout has not completed (missing cutoff or postflight); run local-agent db upgrade first"

// versionRangeError carries the observed schema version plus either the
// future-schema sentinel or the terminal no-upgrade-advice sentinel. It is
// the translation boundary between adapter errors and the rollout sentinels
// the CLI maps to exit codes without importing this package's internals.
type versionRangeError struct {
	found    int
	sentinel error
	message  string
}

func (e *versionRangeError) Error() string { return e.message }
func (e *versionRangeError) Unwrap() error { return e.sentinel }

func newFutureSchemaError(found int) *versionRangeError {
	return &versionRangeError{
		found:    found,
		sentinel: rollout.ErrFutureSchema,
		message:  fmt.Sprintf("%v: found v%d", rollout.ErrFutureSchema, found),
	}
}

func newTerminalSchemaError(sentinel error, found int) *versionRangeError {
	typed := fmt.Errorf("%w: found v%d", sentinel, found)
	if errors.Is(sentinel, rollout.ErrUnsupportedSourceSchema) {
		typed = rollout.UnsupportedSourceSchemaError{Found: found, MinSupported: rollout.MinSourceVersion, MaxSupported: rollout.MaxSourceVersion}
	}
	return &versionRangeError{
		found:    found,
		sentinel: typed,
		message: fmt.Sprintf(
			"database schema v%d is outside the range local-agent db upgrade accepts ([%d, %d]); this file cannot be upgraded or opened by this binary",
			found, rollout.MinSourceVersion, rollout.TargetVersion),
	}
}

func versionRangeFound(err error) (int, bool) {
	if typed, ok := errors.AsType[*versionRangeError](err); ok {
		return typed.found, true
	}
	return 0, false
}

// rolloutPreflightFailure maps requireRolloutComplete outcomes onto the
// operator-facing texts every ordinary command shares.
func rolloutPreflightFailure(err error) error {
	switch {
	case errors.Is(err, rollout.ErrSchemaUpgradeRequired):
		return errors.New(schemaBehindMessage)
	case errors.Is(err, rollout.ErrUnsupportedSourceSchema), errors.Is(err, rollout.ErrFutureSchema):
		if found, ok := versionRangeFound(err); ok {
			if errors.Is(err, rollout.ErrFutureSchema) {
				return newFutureSchemaError(found)
			}
			return newTerminalSchemaError(rollout.ErrUnsupportedSourceSchema, found)
		}
		return err
	case errors.Is(err, rollout.ErrPostflightNotPassed):
		return errors.New(rolloutIncompleteMessage)
	case errors.Is(err, rollout.ErrLegacyIdentityDispositionIncomplete):
		return dispositionIncompleteError{}
	default:
		// A Corrupt reading keeps its own message: it names the observed
		// keys and reasons instead of conflating untrustworthy state with
		// merely incomplete rollout.
		return err
	}
}

func (a *Application) rolloutProbe() rollout.SchemaProbe {
	if a.schemaProbe != nil {
		return a.schemaProbe
	}
	return adaptersqlite.FileSchemaProbe{}
}

func (a *Application) rolloutBackupper() rollout.DatabaseBackupper {
	if a.schemaBackupper != nil {
		return a.schemaBackupper
	}
	return adaptersqlite.FileDatabaseBackupper{}
}

func (a *Application) rolloutWriter() rollout.SchemaWriter {
	if a.schemaWriter != nil {
		return a.schemaWriter
	}
	return adaptersqlite.FileSchemaWriter{}
}

func (a *Application) upgradeDatabasePath() (string, error) {
	configPath, err := config.ConfigPath(a.root)
	if err != nil {
		return "", err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("configuration not found")
		}
		return "", err
	}
	paths, err := cfg.ResolvePaths(a.root)
	if err != nil {
		return "", err
	}
	return paths.DatabaseFile, nil
}

// PreviewDatabaseUpgrade classifies before validating --backup-dir (FIND-164)
// and refuses out-of-range schemas before resolving any backup directory,
// before any prompt, and before the lock. It opens read-only connections
// only.
func (a *Application) PreviewDatabaseUpgrade(ctx context.Context, opts rollout.UpgradeOptions) (rollout.UpgradePreview, error) {
	databasePath, err := a.upgradeDatabasePath()
	if err != nil {
		return rollout.UpgradePreview{}, err
	}
	probe := a.rolloutProbe()
	current, err := probe.CurrentVersion(ctx, databasePath)
	if err != nil {
		return rollout.UpgradePreview{}, err
	}
	if current > rollout.TargetVersion {
		return rollout.UpgradePreview{}, newFutureSchemaError(current)
	}
	if current < rollout.MinSourceVersion {
		return rollout.UpgradePreview{}, newTerminalSchemaError(rollout.ErrUnsupportedSourceSchema, current)
	}
	state, err := probe.ReadRolloutState(ctx, databasePath)
	if err != nil {
		return rollout.UpgradePreview{}, err
	}
	row, err := rollout.ClassifyRollout(current, state)
	if err != nil {
		return rollout.UpgradePreview{}, err
	}
	preview := rollout.UpgradePreview{
		Kind:         rollout.UpgradeKindForRow(row),
		FromVersion:  current,
		ToVersion:    rollout.TargetVersion,
		DatabasePath: databasePath,
	}
	if preview.Kind == rollout.UpgradeFreshUpgrade || preview.Kind == rollout.UpgradeAdoption {
		resolved, err := resolveBackupDir(opts.BackupDir, databasePath)
		if err != nil {
			return rollout.UpgradePreview{}, err
		}
		preview.ResolvedBackupDir = resolved
	}
	return preview, nil
}

// resolveBackupDir canonicalizes and validates the backup destination
// directory. The command never creates a directory: a mistyped --backup-dir
// fails closed here. The fd-based ownership re-check runs later under the
// lock inside BackupInto itself.
func resolveBackupDir(requested, databasePath string) (string, error) {
	dir := requested
	if strings.TrimSpace(dir) == "" {
		absDatabase, err := filepath.Abs(databasePath)
		if err != nil {
			return "", fmt.Errorf("resolve database path: %w", err)
		}
		dir = filepath.Dir(absDatabase)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve backup directory %q: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		return "", fmt.Errorf("backup directory %q does not exist: %w", dir, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat backup directory %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("backup path %q is not a directory", resolved)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("backup directory %q carries group or other write bits", resolved)
	}
	return resolved, nil
}

// ApplyDatabaseUpgrade performs the confirmed upgrade under the exclusive
// lock: re-read everything, then follow the Recovery Table row the re-read
// selects. Prompts belong to the CLI before this call; this method always
// performs the confirmed action.
func (a *Application) ApplyDatabaseUpgrade(ctx context.Context, opts rollout.UpgradeOptions, expected rollout.UpgradePreview) (rollout.UpgradeReport, error) {
	databasePath, err := a.upgradeDatabasePath()
	if err != nil {
		return rollout.UpgradeReport{}, err
	}
	probe := a.rolloutProbe()
	backupper := a.rolloutBackupper()
	writer := a.rolloutWriter()

	lock, err := a.schemaLock(databasePath)
	if err != nil {
		return rollout.UpgradeReport{}, schemaLockFailure(err)
	}
	defer func() { _ = lock.Release() }()

	current2, err := probe.CurrentVersion(ctx, databasePath)
	if err != nil {
		return rollout.UpgradeReport{}, err
	}
	if current2 > rollout.TargetVersion {
		return rollout.UpgradeReport{}, newFutureSchemaError(current2)
	}
	if current2 < rollout.MinSourceVersion {
		return rollout.UpgradeReport{}, newTerminalSchemaError(rollout.ErrUnsupportedSourceSchema, current2)
	}
	state2, err := probe.ReadRolloutState(ctx, databasePath)
	if err != nil {
		return rollout.UpgradeReport{}, err
	}
	row2, err := rollout.ClassifyRollout(current2, state2)
	if err != nil {
		return rollout.UpgradeReport{}, err
	}
	kind2 := rollout.UpgradeKindForRow(row2)
	if kind2 != expected.Kind {
		return rollout.UpgradeReport{Kind: kind2, RolloutAdvanced: false, FromVersion: expected.FromVersion, ToVersion: rollout.TargetVersion}, nil
	}

	backupIdentity := rollout.BackupIdentity{}
	switch row2 {
	case rollout.RolloutRowAdoption:
		if err := refuseAdoptionWithIncompleteRows(probe, ctx, databasePath); err != nil {
			return rollout.UpgradeReport{}, err
		}
		fresh, err := a.createVerifiedBackup(ctx, backupper, opts.BackupDir, databasePath, current2)
		if err != nil {
			return rollout.UpgradeReport{}, err
		}
		backupIdentity = fresh
		// Fixed-zero baseline and epoch cutoff sentinel (FIND-163): the
		// historical boundary of an adopted database is unknowable, so no
		// legacy exception is granted at all. The live counts are never
		// measured into the baseline.
		if err := writer.RecordBaselineAndCutoff(ctx, databasePath, rollout.IdentityBaseline{}, 0, fresh); err != nil {
			return rollout.UpgradeReport{}, err
		}
	case rollout.RolloutRowFreshCapture:
		baseline, err := probe.CaptureIdentityBaseline(ctx, databasePath)
		if err != nil {
			return rollout.UpgradeReport{}, err
		}
		fresh, err := a.createVerifiedBackup(ctx, backupper, opts.BackupDir, databasePath, current2)
		if err != nil {
			return rollout.UpgradeReport{}, err
		}
		backupIdentity = fresh
		cutoff := time.Now().UTC()
		if err := writer.RecordBaselineAndCutoff(ctx, databasePath, baseline, cutoff.UnixNano(), fresh); err != nil {
			return rollout.UpgradeReport{}, err
		}
	case rollout.RolloutRowFreshResume:
		recorded := durableBackupIdentity(state2)
		revalidated, verifyErr := backupper.VerifyBackup(ctx, recorded.Path, current2)
		if verifyErr == nil &&
			revalidated.Path == recorded.Path &&
			revalidated.Bytes == recorded.Bytes &&
			revalidated.SHA256 == recorded.SHA256 {
			// VerifiedAt never participates in the comparison: VerifyBackup
			// measures a fresh timestamp every call. On a pass the durable
			// record stays untouched and step 9 is skipped entirely.
			backupIdentity = recorded
		} else {
			fresh, backupErr := a.createVerifiedBackup(ctx, backupper, opts.BackupDir, databasePath, current2)
			if backupErr != nil {
				return rollout.UpgradeReport{}, backupErr
			}
			backupIdentity = fresh
			// Baseline+cutoff keep DO NOTHING semantics (existing values
			// are re-inserted harmlessly); only the five identity keys
			// update.
			if err := writer.RecordBaselineAndCutoff(ctx, databasePath, state2.Baseline, state2.CutoffUnixNanos, fresh); err != nil {
				return rollout.UpgradeReport{}, err
			}
		}
	case rollout.RolloutRowResumeNeeded:
		// Schema already sits at target and nothing needs protecting: no
		// backup, no migration, no backup revalidation.
	}

	if row2 == rollout.RolloutRowFreshCapture || row2 == rollout.RolloutRowFreshResume {
		if err := writer.Migrate(ctx, databasePath); err != nil {
			return rollout.UpgradeReport{}, err
		}
	}

	return a.finishUpgradePostflight(probe, writer, ctx, databasePath, expected, backupIdentity)
}

func (a *Application) finishUpgradePostflight(
	probe rollout.SchemaProbe,
	writer rollout.SchemaWriter,
	ctx context.Context,
	databasePath string,
	expected rollout.UpgradePreview,
	backupIdentity rollout.BackupIdentity,
) (rollout.UpgradeReport, error) {
	durable, err := probe.ReadRolloutState(ctx, databasePath)
	if err != nil {
		return rollout.UpgradeReport{}, err
	}
	if !durable.BaselinePresent || !durable.BaselineValid {
		return rollout.UpgradeReport{}, fmt.Errorf("%w: %s is absent or unparseable when postflight needs the durable baseline", rollout.ErrRolloutStateCorrupt, rollout.KeyBaseline)
	}
	health, err := probe.IdentityHealth(ctx, databasePath)
	if err != nil {
		return rollout.UpgradeReport{}, err
	}
	live := rollout.IdentityBaseline{
		JobsCompletedWithoutResultIdentity: health.JobsCompletedWithoutResultIdentity,
		ActivationsWithoutContent:          health.ActivationsWithoutContent,
	}
	ok, field, delta := rollout.ComparePostflight(durable.Baseline, live)
	status := rollout.PostflightPassed
	detail := fmt.Sprintf("postflight passed; %s=%d; %s=%d",
		"jobs_completed_without_result_identity", live.JobsCompletedWithoutResultIdentity,
		"activations_without_content", live.ActivationsWithoutContent)
	if !ok {
		status = rollout.PostflightFailed
		detail = fmt.Sprintf("postflight regression on %s by %d", field, delta)
	}
	if err := writer.RecordPostflight(ctx, databasePath, status, detail); err != nil {
		return rollout.UpgradeReport{}, err
	}
	report := rollout.UpgradeReport{
		Kind:                              expected.Kind,
		RolloutAdvanced:                   true,
		FromVersion:                       expected.FromVersion,
		ToVersion:                         rollout.TargetVersion,
		Backup:                            backupIdentity,
		PostflightOK:                      ok,
		PostflightDetail:                  detail,
		BaselineJobsWithoutIdentity:       durable.Baseline.JobsCompletedWithoutResultIdentity,
		BaselineActivationsWithoutContent: durable.Baseline.ActivationsWithoutContent,
		PostJobsWithoutIdentity:           live.JobsCompletedWithoutResultIdentity,
		PostActivationsWithoutContent:     live.ActivationsWithoutContent,
	}
	if !ok {
		return rollout.UpgradeReport{}, fmt.Errorf("%w: %s exceeded its baseline by %d", rollout.ErrPostflightRegression, field, delta)
	}
	return report, nil
}

// refuseAdoptionWithIncompleteRows reads all five postflight-fatal fields
// read-only under the lock, before any backup or write. Any nonzero refuses
// naming every offending field and count.
func refuseAdoptionWithIncompleteRows(probe rollout.SchemaProbe, ctx context.Context, databasePath string) error {
	health, err := probe.IdentityHealth(ctx, databasePath)
	if err != nil {
		return err
	}
	fields := []struct {
		name  string
		count int
	}{
		{"JobsCompletedWithoutResultIdentity", health.JobsCompletedWithoutResultIdentity},
		{"ActivationsWithoutContent", health.ActivationsWithoutContent},
		{"NotificationsWithoutIdentity", health.NotificationsWithoutIdentity},
		{"ActivationsWithoutIdentity", health.ActivationsWithoutIdentity},
		{"ForegroundActivationsActive", health.ForegroundActivationsActive},
	}
	var nonzero []string
	for _, field := range fields {
		if field.count != 0 {
			nonzero = append(nonzero, fmt.Sprintf("%s=%d", field.name, field.count))
		}
	}
	if len(nonzero) > 0 {
		return fmt.Errorf("%w: %s", rollout.ErrAdoptionUnsupportedIncompleteRows, strings.Join(nonzero, ", "))
	}
	return nil
}

func durableBackupIdentity(state rollout.RolloutState) rollout.BackupIdentity {
	return rollout.BackupIdentity{
		Path:          state.BackupPath,
		Bytes:         state.BackupBytes,
		SHA256:        state.BackupSHA256,
		SourceVersion: state.BackupSourceVersion,
		VerifiedAt:    state.BackupVerifiedAt,
	}
}

func (a *Application) createVerifiedBackup(ctx context.Context, backupper rollout.DatabaseBackupper, requestedBackupDir, databasePath string, sourceVersion int) (rollout.BackupIdentity, error) {
	resolvedDir, err := resolveBackupDir(requestedBackupDir, databasePath)
	if err != nil {
		return rollout.BackupIdentity{}, err
	}
	name := fmt.Sprintf("local-agent.pre-v%d.v%d.%s.db", rollout.TargetVersion, sourceVersion, time.Now().UTC().Format("20060102T150405Z"))
	destination := filepath.Join(resolvedDir, name)
	identity, err := backupper.BackupInto(ctx, databasePath, destination)
	if err != nil {
		// The real backupper removes its own partial output; this defensive
		// removal keeps the no-partial-artifact guarantee regardless of the
		// implementation behind the port, against exactly the path this
		// invocation chose.
		_ = os.Remove(destination)
		return rollout.BackupIdentity{}, err
	}
	return identity, nil
}

// CheckRollbackDrain runs the TRD 08 handoff registry's drain query,
// content-free: a count plus, on nonzero, the distinct pending session
// identities.
func (a *Application) CheckRollbackDrain(ctx context.Context) (rollout.SummaryDiscoveryDrainStatus, error) {
	databasePath, err := a.upgradeDatabasePath()
	if err != nil {
		return rollout.SummaryDiscoveryDrainStatus{}, err
	}
	store, err := adaptersqlite.OpenReadOnly(ctx, databasePath)
	if err != nil {
		return rollout.SummaryDiscoveryDrainStatus{}, err
	}
	defer func() { _ = store.Close() }()
	const pendingStates = `status IN ('pending', 'failed', 'running')`
	var count int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM adk_context_summary_jobs WHERE `+pendingStates+` AND target_ordinal >= ?`,
		domain.SummaryDiscoveryTargetFloor).Scan(&count); err != nil {
		return rollout.SummaryDiscoveryDrainStatus{}, fmt.Errorf("count pending discovery markers: %w", err)
	}
	status := rollout.SummaryDiscoveryDrainStatus{Clear: count == 0}
	if count == 0 {
		return status, nil
	}
	rows, err := store.DB().QueryContext(ctx,
		`SELECT DISTINCT session_identity FROM adk_context_summary_jobs WHERE `+pendingStates+` AND target_ordinal >= ? ORDER BY session_identity`,
		domain.SummaryDiscoveryTargetFloor)
	if err != nil {
		return rollout.SummaryDiscoveryDrainStatus{}, fmt.Errorf("list pending discovery sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var identity string
		if err := rows.Scan(&identity); err != nil {
			return rollout.SummaryDiscoveryDrainStatus{}, fmt.Errorf("scan pending discovery session: %w", err)
		}
		status.PendingSessionIdentities = append(status.PendingSessionIdentities, identity)
	}
	if err := rows.Err(); err != nil {
		return rollout.SummaryDiscoveryDrainStatus{}, fmt.Errorf("finish pending discovery sessions scan: %w", err)
	}
	return status, nil
}

// requireRolloutComplete reads the four durable fact groups over read-only
// connections and refuses unless the classification lands on row 5. Callers
// hold their own lock across the read pass; this method never opens mode=rw.
func (a *Application) requireRolloutComplete(ctx context.Context, databasePath string) error {
	probe := a.rolloutProbe()
	current, err := probe.CurrentVersion(ctx, databasePath)
	if err != nil {
		return err
	}
	if current > rollout.TargetVersion {
		return newFutureSchemaError(current)
	}
	if current < rollout.MinSourceVersion {
		return newTerminalSchemaError(rollout.ErrUnsupportedSourceSchema, current)
	}
	state, err := probe.ReadRolloutState(ctx, databasePath)
	if err != nil {
		return err
	}
	row, err := rollout.ClassifyRollout(current, state)
	if err != nil {
		return err
	}
	switch {
	case row == rollout.RolloutRowAlreadyComplete:
		return nil
	case current >= rollout.MinSourceVersion && current < rollout.TargetVersion:
		return fmt.Errorf("%w: found v%d", rollout.ErrSchemaUpgradeRequired, current)
	default:
		return fmt.Errorf("%w", rollout.ErrPostflightNotPassed)
	}
}
