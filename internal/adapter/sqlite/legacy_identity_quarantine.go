package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

// FileLegacyIdentityQuarantine implements rollout.LegacyIdentityQuarantineStore.
// Every read opens its own mode=ro connection keyed only by the database path;
// Apply opens mode=rw itself and runs the recount, both marks, and the
// completion marker inside one transaction. It never migrates and never reads
// or writes result/notification bodies or digest values: it only counts rows
// against the two frozen predicates and stamps two fixed constants into
// existing columns.
type FileLegacyIdentityQuarantine struct{}

// legacyIdentityJobsPredicate is the frozen jobs match predicate shared by
// the preview counts and Apply's in-transaction recount. It mirrors
// migrate_v32's grandfathering shape plus the durable cutoff bound.
var legacyIdentityJobsPredicate = `status = 'completed'
	AND NOT EXISTS (
		SELECT 1 FROM external_agent_job_events e
		WHERE e.job_id = external_agent_jobs.job_id
			AND e.status_revision = external_agent_jobs.status_revision
			AND e.event_kind = ?
	)
	AND (result_bytes <= 0 OR ` + invalidDigestPredicate("result_sha256") + `)
	AND created_at <= ?`

// legacyIdentityActivationsPredicate is the frozen activations match
// predicate. The last_error_code = ” guard is load-bearing: the command
// never overwrites an existing error code.
const legacyIdentityActivationsPredicate = `terminal_status = 'completed'
	AND content_bytes <= 0
	AND last_error_code = ''
	AND created_at <= ?`

// quarantineQuery abstracts *sql.DB and *sql.Tx so every statement runs
// unchanged inside either connection scope.
type quarantineQuery interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func countLegacyIdentityJobs(ctx context.Context, q quarantineQuery, cutoffUnixNanos int64) (int, error) {
	var count int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM external_agent_jobs WHERE `+legacyIdentityJobsPredicate,
		legacyResultIdentityEvent, cutoffUnixNanos).Scan(&count); err != nil {
		return 0, fmt.Errorf("count legacy identity jobs: %w", err)
	}
	return count, nil
}

func countLegacyIdentityActivations(ctx context.Context, q quarantineQuery, cutoffUnixNanos int64) (int, error) {
	var count int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM external_agent_job_activations WHERE `+legacyIdentityActivationsPredicate,
		cutoffUnixNanos).Scan(&count); err != nil {
		return 0, fmt.Errorf("count legacy identity activations: %w", err)
	}
	return count, nil
}

// legacyIdentityCutoff returns the durable rollout cutoff in Unix nanos.
// Absence stays distinguishable from zero: cutoff 0 is the legitimate
// Adoption sentinel that makes every match predicate unsatisfiable.
func legacyIdentityCutoff(ctx context.Context, q quarantineQuery) (int64, bool, error) {
	raw, present, err := quarantineStateValue(ctx, q, rollout.KeyCutoff)
	if err != nil || !present {
		return 0, false, err
	}
	nanos, ok := rollout.ParseNonNegativeDecimal(raw)
	if !ok {
		return 0, false, fmt.Errorf("%w: %s is unparseable", rollout.ErrRolloutStateCorrupt, rollout.KeyCutoff)
	}
	return nanos, true, nil
}

func legacyIdentityAppliedAt(ctx context.Context, q quarantineQuery) (time.Time, bool, error) {
	raw, present, err := quarantineStateValue(ctx, q, rollout.KeyLegacyQuarantineAt)
	if err != nil || !present {
		return time.Time{}, false, err
	}
	appliedAt, ok := rollout.ParseRFC3339(raw)
	if !ok {
		return time.Time{}, false, fmt.Errorf("%w: %s is unparseable", rollout.ErrRolloutStateCorrupt, rollout.KeyLegacyQuarantineAt)
	}
	return appliedAt, true, nil
}

func quarantineStateValue(ctx context.Context, q quarantineQuery, key string) (value string, present bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT state_value FROM runtime_state WHERE state_key = ?`, key).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("read runtime_state key %s: %w", key, err)
	default:
		return value, true, nil
	}
}

func (FileLegacyIdentityQuarantine) ReadCutoff(ctx context.Context, path string) (time.Time, bool, error) {
	store, err := OpenReadOnly(ctx, path)
	if err != nil {
		return time.Time{}, false, err
	}
	defer func() { _ = store.Close() }()
	nanos, present, err := legacyIdentityCutoff(ctx, store.DB())
	if err != nil || !present {
		return time.Time{}, false, err
	}
	return time.Unix(0, nanos).UTC(), true, nil
}

func (FileLegacyIdentityQuarantine) ReadAppliedAt(ctx context.Context, path string) (time.Time, bool, error) {
	store, err := OpenReadOnly(ctx, path)
	if err != nil {
		return time.Time{}, false, err
	}
	defer func() { _ = store.Close() }()
	return legacyIdentityAppliedAt(ctx, store.DB())
}

func (FileLegacyIdentityQuarantine) CountMatches(ctx context.Context, path string, cutoff time.Time) (int, int, error) {
	store, err := OpenReadOnly(ctx, path)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = store.Close() }()
	jobs, err := countLegacyIdentityJobs(ctx, store.DB(), cutoff.UnixNano())
	if err != nil {
		return 0, 0, err
	}
	activations, err := countLegacyIdentityActivations(ctx, store.DB(), cutoff.UnixNano())
	if err != nil {
		return 0, 0, err
	}
	return jobs, activations, nil
}

// QuarantineCountMismatchError carries the CAS divergence between the
// previewed --expect-* counts and the in-transaction recount so the operator
// re-previews instead of guessing which side moved.
type QuarantineCountMismatchError struct {
	ExpectedJobs       int
	ActualJobs         int
	ExpectedActivation int
	ActualActivation   int
}

func (e QuarantineCountMismatchError) Error() string {
	return fmt.Sprintf("%v: expected jobs=%d activations=%d, found jobs=%d activations=%d; run local-agent jobs quarantine-legacy-identity to re-preview",
		rollout.ErrLegacyIdentityQuarantineMismatch, e.ExpectedJobs, e.ExpectedActivation, e.ActualJobs, e.ActualActivation)
}

func (e QuarantineCountMismatchError) Unwrap() error {
	return rollout.ErrLegacyIdentityQuarantineMismatch
}

// Apply performs the whole marking inside ONE write transaction. The durable
// completion marker is checked first (FIND-193): a completed disposition is
// final, so any replay reports already_applied with the original timestamp no
// matter which expectations it carries, and the deferred rollback discards a
// read-only transaction harmlessly. Without a marker the transaction re-reads
// the cutoff and both COUNT(*) predicates, refuses on any divergence from the
// expected counts, marks matching rows with the two fixed constants, then
// inserts the completion marker with ON CONFLICT DO NOTHING. Zero affected
// rows on that insert mean another process completed the disposition first:
// roll back and report already_applied instead of double-marking.
func (FileLegacyIdentityQuarantine) Apply(ctx context.Context, path string, expectJobs, expectActivations int) (rollout.LegacyIdentityQuarantineReport, error) {
	// OpenCurrent refuses any schema other than v41 before opening mode=rw,
	// so a seeded or residual postflight row below target can never reach a
	// write-capable open through this command (FIND-192).
	store, err := OpenCurrent(ctx, path)
	if err != nil {
		return rollout.LegacyIdentityQuarantineReport{}, fmt.Errorf("open database for legacy identity quarantine: %w", err)
	}
	defer func() { _ = store.Close() }()
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		return rollout.LegacyIdentityQuarantineReport{}, fmt.Errorf("begin legacy identity quarantine transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if appliedAt, applied, appliedErr := legacyIdentityAppliedAt(ctx, tx); appliedErr != nil {
		return rollout.LegacyIdentityQuarantineReport{}, appliedErr
	} else if applied {
		return rollout.LegacyIdentityQuarantineReport{AlreadyApplied: true, AppliedAt: appliedAt}, nil
	}

	cutoffNanos, present, err := legacyIdentityCutoff(ctx, tx)
	if err != nil {
		return rollout.LegacyIdentityQuarantineReport{}, err
	}
	if !present {
		return rollout.LegacyIdentityQuarantineReport{}, rollout.ErrLegacyCutoffNotRecorded
	}

	actualJobs, err := countLegacyIdentityJobs(ctx, tx, cutoffNanos)
	if err != nil {
		return rollout.LegacyIdentityQuarantineReport{}, err
	}
	actualActivations, err := countLegacyIdentityActivations(ctx, tx, cutoffNanos)
	if err != nil {
		return rollout.LegacyIdentityQuarantineReport{}, err
	}
	if actualJobs != expectJobs || actualActivations != expectActivations {
		return rollout.LegacyIdentityQuarantineReport{}, QuarantineCountMismatchError{
			ExpectedJobs: expectJobs, ActualJobs: actualJobs,
			ExpectedActivation: expectActivations, ActualActivation: actualActivations,
		}
	}

	jobsMarked, err := markLegacyIdentityJobs(ctx, tx, cutoffNanos)
	if err != nil {
		return rollout.LegacyIdentityQuarantineReport{}, err
	}
	activationsMarked, err := markLegacyIdentityActivations(ctx, tx, cutoffNanos)
	if err != nil {
		return rollout.LegacyIdentityQuarantineReport{}, err
	}

	now := time.Now().UTC()
	completed, err := recordLegacyIdentityCompletion(ctx, tx, now)
	if err != nil {
		return rollout.LegacyIdentityQuarantineReport{}, err
	}
	if completed == 0 {
		appliedAt, _, appliedErr := legacyIdentityAppliedAt(ctx, tx)
		if appliedErr != nil {
			return rollout.LegacyIdentityQuarantineReport{}, appliedErr
		}
		return rollout.LegacyIdentityQuarantineReport{AlreadyApplied: true, AppliedAt: appliedAt}, nil
	}
	if err := tx.Commit(); err != nil {
		return rollout.LegacyIdentityQuarantineReport{}, fmt.Errorf("commit legacy identity quarantine transaction: %w", err)
	}
	return rollout.LegacyIdentityQuarantineReport{
		JobsMarked:        jobsMarked,
		ActivationsMarked: activationsMarked,
		AppliedAt:         now,
	}, nil
}

func markLegacyIdentityJobs(ctx context.Context, tx *sql.Tx, cutoffUnixNanos int64) (int, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO external_agent_job_events
		(job_id, status_revision, event_kind, created_at)
		SELECT job_id, status_revision, ?, updated_at
		FROM external_agent_jobs
		WHERE `+legacyIdentityJobsPredicate+`
		ON CONFLICT DO NOTHING`, legacyResultIdentityEvent, legacyResultIdentityEvent, cutoffUnixNanos)
	if err != nil {
		return 0, fmt.Errorf("mark legacy identity jobs: %w", err)
	}
	marked, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect legacy identity job marks: %w", err)
	}
	return int(marked), nil
}

func markLegacyIdentityActivations(ctx context.Context, tx *sql.Tx, cutoffUnixNanos int64) (int, error) {
	result, err := tx.ExecContext(ctx, `UPDATE external_agent_job_activations
		SET last_error_code = ?, updated_at = ?
		WHERE `+legacyIdentityActivationsPredicate,
		domain.ActivationLegacyContentCode, time.Now().UTC().UnixNano(), cutoffUnixNanos)
	if err != nil {
		return 0, fmt.Errorf("mark legacy identity activations: %w", err)
	}
	marked, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect legacy identity activation marks: %w", err)
	}
	return int(marked), nil
}

// recordLegacyIdentityCompletion inserts the completion row as the final CAS
// layer and reports how many rows the insert affected.
func recordLegacyIdentityCompletion(ctx context.Context, tx *sql.Tx, now time.Time) (int, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO runtime_state (state_key, state_value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (state_key) DO NOTHING`,
		rollout.KeyLegacyQuarantineAt, now.Format(time.RFC3339), now.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("record legacy identity quarantine completion: %w", err)
	}
	completed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect legacy identity quarantine completion: %w", err)
	}
	return int(completed), nil
}
