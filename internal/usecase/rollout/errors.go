package rollout

import (
	"errors"
	"fmt"
)

var (
	// ErrFutureSchema reports a schema newer than this binary's target.
	ErrFutureSchema = errors.New("database schema is newer than this binary supports")

	// ErrSchemaUpgradeRequired reports an accepted schema below TargetVersion.
	// The db upgrade command must process it before ordinary commands open it.
	ErrSchemaUpgradeRequired = fmt.Errorf("database schema is behind this binary's v%d", TargetVersion)

	// ErrBackupPrimitiveUnsupported reports platforms without the backup
	// primitive. Distinct from ErrMutationLockUnsupported so a caller can
	// tell "no lock primitive" from "no backup primitive".
	ErrBackupPrimitiveUnsupported = errors.New("database backup primitive is not supported on this platform")

	// ErrRolloutStateCorrupt reports a durable rollout reading that matches
	// neither a Recovery Table row nor one of the three backup-identity
	// shapes. Every return names the observed keys and reasons.
	ErrRolloutStateCorrupt = errors.New("rollout state cannot be trusted")

	// ErrAdoptionUnsupportedIncompleteRows reports an Adoption target that
	// already carries nonzero postflight-fatal counts. Returned before any
	// backup or write, so its presence proves no mutation happened.
	ErrAdoptionUnsupportedIncompleteRows = errors.New("adoption target already carries incomplete rows that postflight would fail on")

	// ErrUnsupportedSourceSchema reports a source schema below 33, including
	// a never-migrated schema-0 file.
	ErrUnsupportedSourceSchema = errors.New("source schema version is outside the range this binary can upgrade")

	// ErrBackupVerificationFailed reports a backup artifact that failed its
	// integrity, foreign-key, size, digest, or source-version revalidation.
	ErrBackupVerificationFailed = errors.New("backup verification failed")

	// ErrPostflightRegression reports a live identity count that exceeds the
	// durable baseline after the rollout advanced.
	ErrPostflightRegression = errors.New("postflight identity regression")

	// ErrPostflightNotPassed reports a database whose rollout has not yet
	// recorded a passing postflight.
	ErrPostflightNotPassed = errors.New("the schema rollout has not completed (missing cutoff or postflight)")

	// ErrLegacyIdentityDispositionIncomplete reports a database whose rollout
	// is complete but whose legacy identity quarantine has not run yet.
	ErrLegacyIdentityDispositionIncomplete = errors.New("legacy identity disposition has not completed")

	// ErrLegacyIdentityQuarantineMismatch reports a quarantine apply whose
	// re-read match counts diverge from the previewed --expect-* counts.
	ErrLegacyIdentityQuarantineMismatch = errors.New("legacy identity quarantine match counts diverged from the expected counts")

	// ErrLegacyCutoffNotRecorded reports a database that reached the target
	// without db upgrade freezing a rollout cutoff. Its message is the operator
	// text the preview renders verbatim.
	ErrLegacyCutoffNotRecorded = errors.New("no cutoff recorded, run local-agent db upgrade")
)

// UnsupportedSourceSchemaError carries the observed schema and the closed
// supported range, mirroring the adapter-level StateResetNeededError shape.
type UnsupportedSourceSchemaError struct {
	Found        int
	MinSupported int
	MaxSupported int
}

func (e UnsupportedSourceSchemaError) Error() string {
	return fmt.Sprintf("%v: found version %d, supported source range [%d, %d]", ErrUnsupportedSourceSchema, e.Found, e.MinSupported, e.MaxSupported)
}

func (e UnsupportedSourceSchemaError) Unwrap() error { return ErrUnsupportedSourceSchema }
