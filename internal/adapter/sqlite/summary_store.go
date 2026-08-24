package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const MaxPersistedSummaryChars = domain.MaxPersistedSummaryChars

const (
	maxPersistedSummaryChars = MaxPersistedSummaryChars
	maxSummaryAttempts       = 5
)

var _ port.SummaryStore = (*Store)(nil)

func (s *Store) LatestSummary(ctx context.Context, sessionIdentity string) (port.SummaryRecord, error) {
	var record port.SummaryRecord
	var createdMicro, updatedMicro int64
	err := s.db.QueryRowContext(ctx, `
		SELECT session_identity, covered_ordinal_end, source_digest, sanitized_text,
			prompt_version, created_at, updated_at
		FROM adk_context_summaries WHERE session_identity = ?`, sessionIdentity).
		Scan(&record.SessionIdentity, &record.CoveredThroughOrdinal, &record.SourceDigest, &record.SanitizedText, &record.PromptVersion, &createdMicro, &updatedMicro)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return port.SummaryRecord{}, port.ErrSummaryNotFound
		}
		return port.SummaryRecord{}, fmt.Errorf("read ADK context summary: %w", err)
	}
	record.CreatedAt = time.UnixMicro(createdMicro).UTC()
	record.UpdatedAt = time.UnixMicro(updatedMicro).UTC()
	clean, sanitizeErr := domain.SanitizeConversationSummary(record.SanitizedText, MaxPersistedSummaryChars)
	if sanitizeErr != nil || record.CoveredThroughOrdinal <= 0 || record.SourceDigest == "" || record.PromptVersion == "" {
		// Treat persisted invalid data as absent. The projector must fall back to
		// raw turns rather than consuming an untrusted historical summary.
		return port.SummaryRecord{}, port.ErrSummaryNotFound
	}
	record.SanitizedText = clean
	return record, nil
}

func (s *Store) CommitSummary(ctx context.Context, commit port.SummaryCommit) (bool, error) {
	if commit.SessionIdentity == "" {
		return false, errors.New("summary session identity is required")
	}
	if commit.Summary.CoveredThroughOrdinal <= 0 || commit.Summary.SourceDigest == "" || commit.Summary.PromptVersion == "" {
		return false, errors.New("summary ordinal, source digest, and prompt version are required")
	}
	clean, err := domain.SanitizeConversationSummary(commit.Summary.Text, maxPersistedSummaryChars)
	if err != nil {
		return false, err
	}
	if err := validatePersistedSummaryText(clean); err != nil {
		return false, err
	}
	now := time.Now().UTC().UnixMicro()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin ADK context summary commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var currentOrdinal int64
	var currentDigest string
	err = tx.QueryRowContext(ctx, `SELECT covered_ordinal_end, source_digest FROM adk_context_summaries WHERE session_identity = ?`, commit.SessionIdentity).Scan(&currentOrdinal, &currentDigest)
	if errors.Is(err, sql.ErrNoRows) {
		if commit.ExpectedOrdinal != 0 || commit.ExpectedSourceDigest != "" {
			return false, nil
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO adk_context_summaries (
				session_identity, covered_ordinal_start, covered_ordinal_end, source_digest,
				sanitized_text, prompt_version, created_at, updated_at)
			VALUES (?, 1, ?, ?, ?, ?, ?, ?)`, commit.SessionIdentity, commit.Summary.CoveredThroughOrdinal,
			commit.Summary.SourceDigest, clean, commit.Summary.PromptVersion, now, now)
	} else if err == nil {
		if currentOrdinal != commit.ExpectedOrdinal || currentDigest != commit.ExpectedSourceDigest || commit.Summary.CoveredThroughOrdinal <= currentOrdinal {
			return false, nil
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE adk_context_summaries SET covered_ordinal_end = ?, source_digest = ?,
				sanitized_text = ?, prompt_version = ?, updated_at = ?
			WHERE session_identity = ? AND covered_ordinal_end = ? AND source_digest = ?`,
			commit.Summary.CoveredThroughOrdinal, commit.Summary.SourceDigest, clean,
			commit.Summary.PromptVersion, now, commit.SessionIdentity, commit.ExpectedOrdinal, commit.ExpectedSourceDigest)
	} else {
		return false, fmt.Errorf("read current ADK context summary: %w", err)
	}
	if err != nil {
		return false, fmt.Errorf("write ADK context summary: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit ADK context summary: %w", err)
	}
	return true, nil
}

func (s *Store) ScheduleSummaryJob(ctx context.Context, sessionIdentity string, targetOrdinal int64, nextAttempt time.Time) (bool, error) {
	if sessionIdentity == "" || targetOrdinal <= 0 {
		return false, errors.New("summary job identity and target ordinal are required")
	}
	if nextAttempt.IsZero() {
		nextAttempt = time.Now().UTC()
	}
	now := time.Now().UTC().UnixMicro()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin ADK context summary schedule: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existingTarget int64
	err = tx.QueryRowContext(ctx, `SELECT target_ordinal FROM adk_context_summary_jobs
		WHERE session_identity = ? AND status IN ('pending', 'failed') AND attempts < ?
		ORDER BY target_ordinal DESC LIMIT 1`, sessionIdentity, maxSummaryAttempts).Scan(&existingTarget)
	if err == nil && existingTarget >= targetOrdinal {
		return false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("inspect ADK context summary schedule: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM adk_context_summary_jobs
		WHERE session_identity = ? AND status IN ('pending', 'failed')`, sessionIdentity); err != nil {
		return false, fmt.Errorf("coalesce ADK context summary jobs: %w", err)
	}
	insertTarget := targetOrdinal
	result, err := tx.ExecContext(ctx, `
		INSERT INTO adk_context_summary_jobs (session_identity, target_ordinal, status, attempts, next_attempt, created_at, updated_at)
		VALUES (?, ?, 'pending', 0, ?, ?, ?)`, sessionIdentity, insertTarget, nextAttempt.UnixMicro(), now, now)
	if err != nil && domain.IsSummaryDiscoveryTarget(targetOrdinal) && isUniqueConstraint(err) {
		// The fixed discovery marker collided with a row this session's
		// jobs table already retains at that exact identity: a still-running
		// discovery job, or a completed one the coalescing delete above never
		// touches (FIND-134). Claim the next marker above every target this
		// session has ever used instead of failing or dropping the request:
		// that marker cannot collide with anything on record, and it is
		// still a discovery-range value, so the worker still resolves it as
		// a discovery job.
		var maxUsed int64
		if scanErr := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(target_ordinal), 0)
			FROM adk_context_summary_jobs WHERE session_identity = ?`, sessionIdentity).Scan(&maxUsed); scanErr != nil {
			return false, fmt.Errorf("inspect ADK context summary discovery markers: %w", scanErr)
		}
		insertTarget, markerErr := domain.NextSummaryDiscoveryMarker(maxUsed)
		if markerErr != nil {
			// maxUsed already sits at math.MaxInt64 (corrupt or manually
			// seeded data): fail explicitly instead of letting insertTarget
			// wrap to a negative value, which would either violate the
			// target_ordinal > 0 CHECK constraint or, worse, collide with a
			// low, concrete ordinal (FIND-135). tx.Rollback() (deferred)
			// undoes the coalescing delete above, so this leaves every prior
			// row exactly as it was.
			return false, fmt.Errorf("schedule ADK context summary discovery: %w", markerErr)
		}
		result, err = tx.ExecContext(ctx, `
			INSERT INTO adk_context_summary_jobs (session_identity, target_ordinal, status, attempts, next_attempt, created_at, updated_at)
			VALUES (?, ?, 'pending', 0, ?, ?, ?)`, sessionIdentity, insertTarget, nextAttempt.UnixMicro(), now, now)
	}
	if err != nil {
		return false, fmt.Errorf("schedule ADK context summary: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect ADK context summary schedule: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit ADK context summary schedule: %w", err)
	}
	return rows == 1, nil
}

func (s *Store) ClaimSummaryJob(ctx context.Context, now time.Time) (port.SummaryJob, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return port.SummaryJob{}, fmt.Errorf("begin ADK context summary claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var job port.SummaryJob
	var nextMicro, createdMicro, updatedMicro int64
	staleBefore := now.Add(-5 * time.Minute).UnixMicro()
	err = tx.QueryRowContext(ctx, `
		SELECT session_identity, target_ordinal, status, attempts, next_attempt, created_at, updated_at
		FROM adk_context_summary_jobs
		WHERE (((status IN ('pending', 'failed') AND attempts < ? AND next_attempt <= ?)) OR
			(status = 'running' AND attempts < ? AND updated_at <= ?))
		ORDER BY next_attempt, target_ordinal LIMIT 1`, maxSummaryAttempts, now.UnixMicro(), maxSummaryAttempts, staleBefore).
		Scan(&job.SessionIdentity, &job.TargetOrdinal, &job.Status, &job.Attempts, &nextMicro, &createdMicro, &updatedMicro)
	if errors.Is(err, sql.ErrNoRows) {
		return port.SummaryJob{}, port.ErrSummaryNotFound
	}
	if err != nil {
		return port.SummaryJob{}, err
	}
	updated, err := tx.ExecContext(ctx, `
		UPDATE adk_context_summary_jobs SET status = 'running', attempts = attempts + 1, updated_at = ?
		WHERE session_identity = ? AND target_ordinal = ? AND status = ?`, now.UnixMicro(), job.SessionIdentity, job.TargetOrdinal, job.Status)
	if err != nil {
		return port.SummaryJob{}, fmt.Errorf("claim ADK context summary: %w", err)
	}
	if count, _ := updated.RowsAffected(); count != 1 {
		return port.SummaryJob{}, errors.New("ADK context summary claim lost race")
	}
	if err := tx.Commit(); err != nil {
		return port.SummaryJob{}, fmt.Errorf("commit ADK context summary claim: %w", err)
	}
	job.Status = "running"
	job.Attempts++
	job.NextAttempt = time.UnixMicro(nextMicro).UTC()
	job.CreatedAt = time.UnixMicro(createdMicro).UTC()
	job.UpdatedAt = time.UnixMicro(updatedMicro).UTC()
	return job, nil
}

func (s *Store) CompleteSummaryJob(ctx context.Context, job port.SummaryJob) error {
	return s.updateSummaryJobStatus(ctx, job, "done", time.Time{})
}

func (s *Store) FailSummaryJob(ctx context.Context, job port.SummaryJob, nextAttempt time.Time) error {
	if nextAttempt.IsZero() {
		nextAttempt = time.Now().UTC().Add(time.Minute)
	}
	return s.updateSummaryJobStatus(ctx, job, "failed", nextAttempt)
}

func (s *Store) updateSummaryJobStatus(ctx context.Context, job port.SummaryJob, status string, nextAttempt time.Time) error {
	if status != "done" && status != "failed" {
		return errors.New("invalid ADK context summary job status")
	}
	next := nextAttempt
	if next.IsZero() {
		// A zero retry time is the permanent terminal state. Keep status=failed
		// for operational inspection, while the claim predicate excludes it.
		next = time.Unix(1<<62, 0).UTC()
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE adk_context_summary_jobs SET status = ?, next_attempt = ?, updated_at = ?
		WHERE session_identity = ? AND target_ordinal = ? AND status = 'running' AND attempts = ?`,
		status, next.UnixMicro(), time.Now().UTC().UnixMicro(), job.SessionIdentity, job.TargetOrdinal, job.Attempts)
	if err != nil {
		return fmt.Errorf("update ADK context summary job: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect ADK context summary job update: %w", err)
	}
	if rows != 1 {
		return errors.New("stale ADK context summary job update")
	}
	return nil
}

func validatePersistedSummaryText(text string) error {
	if utf8.RuneCountInString(text) > maxPersistedSummaryChars {
		return fmt.Errorf("summary exceeds %d Unicode code points", maxPersistedSummaryChars)
	}
	return nil
}
