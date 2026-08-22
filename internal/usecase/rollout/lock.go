// Package rollout owns the operator-facing contracts of the TRD 09 v2
// migration rollout. This checkpoint ships only the cross-process exclusion
// port: every mutable command takes an exclusive schema mutation lock before
// it opens the database, and holds it for its whole declared scope.
package rollout

import "errors"

var (
	// ErrMutationLockHeld reports non-blocking contention with another
	// local-agent process already holding the schema mutation lock.
	ErrMutationLockHeld = errors.New("schema mutation lock is held by another process")

	// ErrMutationLockUnsupported reports platforms where the kernel-held
	// lock primitive is unavailable. Callers fail closed; there is no no-op
	// lock.
	ErrMutationLockUnsupported = errors.New("schema mutation locking is not supported on this platform")
)

// Lock is one held exclusive schema mutation lock. Release frees the
// underlying OS resource exactly once: releasing a lock that is no longer
// held reports an error, and the lock file itself stays on disk carrying no
// authority.
type Lock interface {
	Release() error
}

// SchemaLocker acquires the exclusive lock for one database path. The
// implementation must never block: contention returns ErrMutationLockHeld
// immediately.
type SchemaLocker interface {
	AcquireExclusive(databasePath string) (Lock, error)
}
