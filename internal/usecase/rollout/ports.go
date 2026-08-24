package rollout

import (
	"context"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// DatabaseBackupper creates and revalidates backup artifacts. BackupInto
// never opens the source mode=rw and never migrates; it owns the exclusive
// creation of destPath and removes its own partial output on verification
// failure. VerifyBackup is read-only over a file that may not be this
// process's own backup, so it never removes anything; callers pass their
// own live lock-held schema read as wantSourceVersion.
type DatabaseBackupper interface {
	BackupInto(ctx context.Context, srcPath, destPath string) (BackupIdentity, error)
	VerifyBackup(ctx context.Context, backupPath string, wantSourceVersion int) (BackupIdentity, error)
}

// SchemaProbe groups the read-only reads. No method takes or returns a
// connection handle: every method takes only path and opens its own
// mode=ro connection. The lock, not connection reuse, makes a caller's
// sequence of these reads trustworthy by excluding lock-participating
// writers.
type SchemaProbe interface {
	CurrentVersion(ctx context.Context, path string) (int, error)
	// CaptureIdentityBaseline measures the two carve-out counters. Apply
	// calls it exactly once per invocation, only for row 1, under the lock,
	// strictly before any backup work.
	CaptureIdentityBaseline(ctx context.Context, path string) (IdentityBaseline, error)
	ReadRolloutState(ctx context.Context, path string) (RolloutState, error)
	// IdentityHealth returns the full identity-health aggregate so Apply's
	// Adoption unsupported-input check can read all five postflight-fatal
	// fields through the same port seam.
	IdentityHealth(ctx context.Context, path string) (domain.ExternalAgentJobIdentityHealth, error)
}

// SchemaWriter owns every source-modifying step. The ordering the methods
// must be called in (baseline+cutoff+backup identity, then migrate, then
// postflight) stays visible at the interface level.
type SchemaWriter interface {
	RecordBaselineAndCutoff(ctx context.Context, path string, baseline IdentityBaseline, cutoffUnixNanos int64, backup BackupIdentity) error
	Migrate(ctx context.Context, path string) error
	RecordPostflight(ctx context.Context, path string, status PostflightStatus, detail string) error
}

// LegacyIdentityQuarantineStore owns the durable reads and the single marking
// transaction of the legacy identity disposition. Reads open their own
// mode=ro connections keyed only by path; Apply performs the whole
// CAS-guarded write in one transaction and never migrates.
type LegacyIdentityQuarantineStore interface {
	ReadCutoff(ctx context.Context, path string) (time.Time, bool, error)
	ReadAppliedAt(ctx context.Context, path string) (time.Time, bool, error)
	// CountMatches runs the two frozen COUNT(*) predicates read-only against
	// the given cutoff. It never reads row content.
	CountMatches(ctx context.Context, path string, cutoff time.Time) (jobs int, activations int, err error)
	// Apply re-reads the cutoff and both counts inside one write transaction,
	// refuses on any divergence from the expected counts, marks matching rows,
	// and inserts the completion marker with ON CONFLICT DO NOTHING.
	Apply(ctx context.Context, path string, expectJobs, expectActivations int) (LegacyIdentityQuarantineReport, error)
}
