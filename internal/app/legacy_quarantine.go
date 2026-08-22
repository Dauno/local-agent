package app

import (
	"context"
	"fmt"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

const (
	// postflightNotPassedQuarantineMessage is the frozen apply/preview text for
	// an unresolved postflight regression. It differs from the ordinary
	// rollout-incomplete message because this command only demands the
	// postflight half, not the full row-5 classification.
	postflightNotPassedQuarantineMessage = "the schema rollout's postflight has not passed; run local-agent db upgrade first"

	// dispositionIncompleteMessage is the frozen run-preflight text for a
	// missing legacy identity quarantine completion marker.
	dispositionIncompleteMessage = "legacy identity disposition has not completed; run local-agent jobs quarantine-legacy-identity first"
)

// postflightNotPassedQuarantineError carries the quarantine command's frozen
// text while keeping the typed sentinel chain intact.
type postflightNotPassedQuarantineError struct{}

func (postflightNotPassedQuarantineError) Error() string { return postflightNotPassedQuarantineMessage }
func (postflightNotPassedQuarantineError) Unwrap() error { return rollout.ErrPostflightNotPassed }

// dispositionIncompleteError carries run's frozen preflight text while keeping
// the typed sentinel chain intact.
type dispositionIncompleteError struct{}

func (dispositionIncompleteError) Error() string { return dispositionIncompleteMessage }
func (dispositionIncompleteError) Unwrap() error {
	return rollout.ErrLegacyIdentityDispositionIncomplete
}

func (a *Application) legacyQuarantineStore() rollout.LegacyIdentityQuarantineStore {
	if a.quarantineStore != nil {
		return a.quarantineStore
	}
	return adaptersqlite.FileLegacyIdentityQuarantine{}
}

// requirePostflightPassed is the quarantine command's own narrow precondition:
// the version guard first, then the read-only demand that the durable
// postflight status is exactly "passed". Unlike requireRolloutComplete it
// never demands the disposition marker, which this command itself completes.
// Callers hold their own lock across the read pass on the apply path; this
// method never opens mode=rw.
func (a *Application) requirePostflightPassed(ctx context.Context, databasePath string) error {
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
	if current < rollout.TargetVersion {
		// Only db upgrade records a passing postflight, and it does so at v41
		// (FIND-192): a passed row seen below target is residual, corrupt, or
		// seeded, and this command must never trust it.
		return postflightNotPassedQuarantineError{}
	}
	state, err := probe.ReadRolloutState(ctx, databasePath)
	if err != nil {
		return err
	}
	if !state.PostflightPresent || !state.PostflightValid || state.PostflightStatus != rollout.PostflightPassed {
		return postflightNotPassedQuarantineError{}
	}
	return nil
}

// requireLegacyIdentityDispositionComplete is run's remaining preflight half:
// one read-only marker read in the same lock-held pass as
// requireRolloutComplete, strictly before OpenCurrent.
func (a *Application) requireLegacyIdentityDispositionComplete(ctx context.Context, databasePath string) error {
	_, present, err := a.legacyQuarantineStore().ReadAppliedAt(ctx, databasePath)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%w", rollout.ErrLegacyIdentityDispositionIncomplete)
	}
	return nil
}

// PreviewLegacyIdentityQuarantine reports the two frozen match counts over
// read-only connections without taking any lock and without ever opening
// mode=rw. A completed disposition is final: it reports already_applied with
// zero counts instead of re-running the match queries.
func (a *Application) PreviewLegacyIdentityQuarantine(ctx context.Context) (rollout.LegacyIdentityQuarantinePreview, error) {
	databasePath, err := a.upgradeDatabasePath()
	if err != nil {
		return rollout.LegacyIdentityQuarantinePreview{}, err
	}
	store := a.legacyQuarantineStore()
	if err := a.requirePostflightPassed(ctx, databasePath); err != nil {
		return rollout.LegacyIdentityQuarantinePreview{}, err
	}
	cutoff, present, err := store.ReadCutoff(ctx, databasePath)
	if err != nil {
		return rollout.LegacyIdentityQuarantinePreview{}, err
	}
	if !present {
		return rollout.LegacyIdentityQuarantinePreview{}, fmt.Errorf("%w", rollout.ErrLegacyCutoffNotRecorded)
	}
	appliedAt, applied, err := store.ReadAppliedAt(ctx, databasePath)
	if err != nil {
		return rollout.LegacyIdentityQuarantinePreview{}, err
	}
	if applied {
		return rollout.LegacyIdentityQuarantinePreview{AlreadyApplied: true, AppliedAt: appliedAt}, nil
	}
	jobs, activations, err := store.CountMatches(ctx, databasePath, cutoff)
	if err != nil {
		return rollout.LegacyIdentityQuarantinePreview{}, err
	}
	return rollout.LegacyIdentityQuarantinePreview{
		Cutoff: cutoff, JobsMatched: jobs, ActivationsMatched: activations,
	}, nil
}

// ApplyLegacyIdentityQuarantine performs the confirmed marking under the
// exclusive lock: version guard and postflight re-check read-only first, then
// the single CAS-guarded write transaction. Prompts belong to the CLI before
// this call; this method always performs the confirmed action.
func (a *Application) ApplyLegacyIdentityQuarantine(ctx context.Context, expected rollout.LegacyIdentityQuarantinePreview) (rollout.LegacyIdentityQuarantineReport, error) {
	databasePath, err := a.upgradeDatabasePath()
	if err != nil {
		return rollout.LegacyIdentityQuarantineReport{}, err
	}
	lock, err := a.schemaLock(databasePath)
	if err != nil {
		return rollout.LegacyIdentityQuarantineReport{}, schemaLockFailure(err)
	}
	defer func() { _ = lock.Release() }()
	if err := a.requirePostflightPassed(ctx, databasePath); err != nil {
		return rollout.LegacyIdentityQuarantineReport{}, err
	}
	report, err := a.legacyQuarantineStore().Apply(ctx, databasePath, expected.JobsMatched, expected.ActivationsMatched)
	if err != nil {
		return rollout.LegacyIdentityQuarantineReport{}, err
	}
	return report, nil
}

var _ interface {
	PreviewLegacyIdentityQuarantine(context.Context) (rollout.LegacyIdentityQuarantinePreview, error)
	ApplyLegacyIdentityQuarantine(context.Context, rollout.LegacyIdentityQuarantinePreview) (rollout.LegacyIdentityQuarantineReport, error)
} = (*Application)(nil)

// ensure the adapter satisfies the port at compile time.
var _ rollout.LegacyIdentityQuarantineStore = adaptersqlite.FileLegacyIdentityQuarantine{}
