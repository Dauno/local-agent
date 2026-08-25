package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.ExternalAgentJobProgressStore = (*ExternalAgentJobStore)(nil)

// WriteJobProgress persists the content-free live projection with a strict
// lease/attempt compare-and-set. The projection identity must match the
// target job exactly, the caller's attempt and the projection attempt must
// both equal the current job attempt, and the caller must be the current
// lease owner — for terminal states as well, so a recovered or stale worker
// can never overwrite the projection of a newer or foreign attempt.
func (s *ExternalAgentJobStore) WriteJobProgress(ctx context.Context, jobID, owner string, attempt int, progress domain.ExternalAgentJobProgress) error {
	if s == nil || s.db == nil {
		return errors.New("external-agent progress store is not configured")
	}
	if err := progress.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin external-agent progress write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var leaseOwner string
	var jobAttempt int
	err = tx.QueryRowContext(ctx, `SELECT lease_owner, attempt FROM external_agent_jobs WHERE job_id = ?`, jobID).Scan(&leaseOwner, &jobAttempt)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("external-agent progress job does not exist")
	}
	if err != nil {
		return fmt.Errorf("load external-agent job for progress write: %w", err)
	}
	if progress.JobID != jobID {
		return errors.New("external-agent progress job ID does not match the target job")
	}
	if attempt != jobAttempt || progress.Attempt != jobAttempt {
		return errors.New("external-agent progress attempt does not match the job")
	}
	if leaseOwner == "" || leaseOwner != owner {
		return errors.New("external-agent progress lease owner does not match the job")
	}
	now := progress.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO external_agent_job_progress (
		job_id, attempt, phase, last_event_kind,
		last_transport_activity_at, last_session_update_at, last_meaningful_progress_at,
		prompt_started_at, active_tool_count, last_tool_call_id, last_tool_kind,
		last_tool_status, tool_overflow, pending_permission, stop_reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET
		attempt = excluded.attempt,
		phase = excluded.phase,
		last_event_kind = excluded.last_event_kind,
		last_transport_activity_at = CASE WHEN attempt != excluded.attempt THEN excluded.last_transport_activity_at ELSE MAX(last_transport_activity_at, excluded.last_transport_activity_at) END,
		last_session_update_at = CASE WHEN attempt != excluded.attempt THEN excluded.last_session_update_at ELSE MAX(last_session_update_at, excluded.last_session_update_at) END,
		last_meaningful_progress_at = CASE WHEN attempt != excluded.attempt THEN excluded.last_meaningful_progress_at ELSE MAX(last_meaningful_progress_at, excluded.last_meaningful_progress_at) END,
		prompt_started_at = CASE WHEN attempt != excluded.attempt THEN excluded.prompt_started_at WHEN prompt_started_at = 0 THEN excluded.prompt_started_at ELSE prompt_started_at END,
		active_tool_count = excluded.active_tool_count,
		last_tool_call_id = excluded.last_tool_call_id,
		last_tool_kind = excluded.last_tool_kind,
		last_tool_status = excluded.last_tool_status,
		tool_overflow = excluded.tool_overflow,
		pending_permission = excluded.pending_permission,
		stop_reason = CASE WHEN attempt != excluded.attempt THEN excluded.stop_reason WHEN excluded.stop_reason != '' THEN excluded.stop_reason ELSE stop_reason END,
		created_at = CASE WHEN attempt != excluded.attempt THEN excluded.created_at ELSE created_at END,
		updated_at = excluded.updated_at`,
		progress.JobID, progress.Attempt, progress.Phase, progress.LastEventKind,
		unix(progress.LastTransportActivityAt), unix(progress.LastSessionUpdateAt), unix(progress.LastMeaningfulProgressAt),
		unix(progress.PromptStartedAt), progress.ActiveToolCount, progress.LastToolCallID, progress.LastToolKind,
		progress.LastToolStatus, boolInt(progress.ToolOverflow), boolInt(progress.PendingPermission), progress.StopReason,
		unix(now), unix(now))
	if err != nil {
		return fmt.Errorf("persist external-agent progress: %w", err)
	}
	return tx.Commit()
}

// ReadJobProgress loads the durable projection. A missing row returns nil so
// status callers can render an empty projection without failing.
func (s *ExternalAgentJobStore) ReadJobProgress(ctx context.Context, jobID string) (*domain.ExternalAgentJobProgress, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("external-agent progress store is not configured")
	}
	var (
		progress                                           domain.ExternalAgentJobProgress
		phase, lastEventKind, lastToolKind, lastToolStatus string
		transport, session, meaningful, promptStarted      int64
		overflow, pending                                  int
		created, updated                                   int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT job_id, attempt, phase, last_event_kind,
		last_transport_activity_at, last_session_update_at, last_meaningful_progress_at,
		prompt_started_at, active_tool_count, last_tool_call_id, last_tool_kind,
		last_tool_status, tool_overflow, pending_permission, stop_reason, created_at, updated_at
		FROM external_agent_job_progress WHERE job_id = ?`, jobID).
		Scan(&progress.JobID, &progress.Attempt, &phase, &lastEventKind,
			&transport, &session, &meaningful, &promptStarted, &progress.ActiveToolCount,
			&progress.LastToolCallID, &lastToolKind, &lastToolStatus, &overflow, &pending,
			&progress.StopReason, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read external-agent progress: %w", err)
	}
	progress.Phase = domain.ExternalAgentProgressPhase(phase)
	progress.LastEventKind = domain.ExternalAgentEventKind(lastEventKind)
	progress.LastToolKind = domain.ExternalAgentToolKind(lastToolKind)
	progress.LastToolStatus = domain.ExternalAgentToolStatus(lastToolStatus)
	progress.ToolOverflow = overflow != 0
	progress.PendingPermission = pending != 0
	progress.LastTransportActivityAt = fromUnix(transport)
	progress.LastSessionUpdateAt = fromUnix(session)
	progress.LastMeaningfulProgressAt = fromUnix(meaningful)
	progress.PromptStartedAt = fromUnix(promptStarted)
	progress.CreatedAt = fromUnix(created)
	progress.UpdatedAt = fromUnix(updated)
	return &progress, nil
}
