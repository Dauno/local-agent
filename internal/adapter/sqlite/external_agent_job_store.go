package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.ExternalAgentJobStore = (*ExternalAgentJobStore)(nil)
var _ port.ExpiredExternalAgentJobRecovery = (*ExternalAgentJobStore)(nil)
var _ port.ExternalAgentJobNotificationStore = (*ExternalAgentJobStore)(nil)
var _ port.ExternalAgentJobNotificationRetryStore = (*ExternalAgentJobStore)(nil)
var _ port.ExternalAgentJobNotificationHealthStore = (*ExternalAgentJobStore)(nil)
var _ port.ExternalAgentJobAdminStore = (*ExternalAgentJobStore)(nil)
var _ port.ExternalAgentJobDeliveryStore = (*ExternalAgentJobStore)(nil)
var _ port.ArtifactReferenceChecker = (*ExternalAgentJobStore)(nil)
var _ port.ExternalAgentJobReconciler = (*ExternalAgentJobStore)(nil)

var ErrNotificationStateConflict = port.ErrNotificationStateConflict

const (
	notificationRetryBaseDelay = time.Second
	notificationRetryMaxDelay  = 60 * time.Second
	notificationRetryJitter    = 0.2
)

type ExternalAgentJobStore struct{ db *sql.DB }

func NewExternalAgentJobStore(store *Store) *ExternalAgentJobStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &ExternalAgentJobStore{db: store.db}
}

func (s *ExternalAgentJobStore) CreateIfAbsent(ctx context.Context, job domain.ExternalAgentJob) (bool, *domain.ExternalAgentJob, error) {
	if s == nil || s.db == nil {
		return false, nil, errors.New("external-agent job store is not configured")
	}
	if err := job.Validate(); err != nil {
		return false, nil, err
	}
	if job.Status != domain.JobQueued {
		return false, nil, errors.New("new external-agent jobs must be queued")
	}
	projects, err := json.Marshal(job.AdditionalProjects)
	if err != nil {
		return false, nil, fmt.Errorf("encode external-agent projects: %w", err)
	}
	now := job.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
		job.CreatedAt = now
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = now
	}
	if job.TimeoutAt.IsZero() {
		return false, nil, errors.New("external-agent job timeout is required")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO external_agent_jobs (
		job_id, mode, provider, profile, primary_project, additional_projects,
		registry_revision, task, request_sha256, wrapper_call_id, original_call_id,
		actor, slack_team_id, conversation_key, status, attempt, acp_session_id,
		side_effects_possible, lease_owner, lease_expiry, heartbeat_at, timeout_at,
		result_summary, result_artifact, result_sha256, result_bytes, error_code,
		status_revision, created_at, started_at, finished_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		job.ID, job.Mode, job.Provider, job.Profile, job.PrimaryProject, string(projects),
		job.RegistryRevision, job.Task, job.RequestSHA256, job.WrapperCallID, job.OriginalCallID,
		job.Actor, job.TeamID, string(job.ConversationKey), job.Status, job.Attempt, job.ACPSessionID,
		boolInt(job.SideEffectsPossible), job.LeaseOwner, unix(job.LeaseExpiry), unix(job.HeartbeatAt), unix(job.TimeoutAt),
		job.ResultSummary, job.ResultArtifact, job.ResultSHA256, job.ResultBytes, job.ErrorCode,
		job.StatusRevision, unix(job.CreatedAt), unix(job.StartedAt), unix(job.FinishedAt), unix(job.UpdatedAt),
	)
	if err != nil {
		return false, nil, fmt.Errorf("insert external-agent job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, nil, fmt.Errorf("inspect external-agent job insert: %w", err)
	}
	if affected == 1 {
		return true, nil, nil
	}
	existing, err := s.findExisting(ctx, job.ID, job.OriginalCallID)
	return false, existing, err
}

func (s *ExternalAgentJobStore) findExisting(ctx context.Context, jobID, originalCallID string) (*domain.ExternalAgentJob, error) {
	job, err := s.load(ctx, s.db, `WHERE job_id = ? OR original_call_id = ?`, jobID, originalCallID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load existing external-agent job: %w", err)
	}
	return &job, nil
}

func (s *ExternalAgentJobStore) GetJob(ctx context.Context, jobID string) (*domain.ExternalAgentJob, error) {
	job, err := s.load(ctx, s.db, `WHERE job_id = ?`, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get external-agent job: %w", err)
	}
	return &job, nil
}

// InspectJob returns only fields approved for the local administrative view.
// Querying the projection directly avoids loading task text, result content,
// artifact references, or the actor/conversation binding into this boundary.
func (s *ExternalAgentJobStore) InspectJob(ctx context.Context, jobID string) (*domain.ExternalAgentJobInspection, error) {
	if s == nil || s.db == nil || strings.TrimSpace(jobID) == "" || strings.ContainsAny(jobID, "\x00\r\n") {
		return nil, nil
	}
	var status string
	var statusRevision int
	var finishedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT status, status_revision, finished_at
		FROM external_agent_jobs WHERE job_id = ?`, jobID).Scan(&status, &statusRevision, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect external-agent job: %w", err)
	}
	view := &domain.ExternalAgentJobInspection{
		JobID: jobID, Status: safeAdminJobStatus(status), StatusRevision: statusRevision,
		FinishedAt: fromUnix(finishedAt),
	}
	rows, err := s.db.QueryContext(ctx, `SELECT status_revision, kind, publish_state,
		attempts, last_error_code, next_attempt_at, recovered_slack_ts,
		delivery_mode, upload_state, length(slack_file_id) > 0
		FROM external_agent_job_notifications WHERE job_id = ?
		ORDER BY status_revision ASC, kind ASC`, jobID)
	if err != nil {
		return nil, fmt.Errorf("inspect external-agent job deliveries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var delivery domain.ExternalAgentJobDeliveryInspection
		var kind, publishState, errorCode, deliveryMode, uploadState, recoveredTS string
		var nextAttemptAt int64
		var filePresent int
		if err := rows.Scan(&delivery.StatusRevision, &kind, &publishState, &delivery.Attempts, &errorCode, &nextAttemptAt, &recoveredTS, &deliveryMode, &uploadState, &filePresent); err != nil {
			return nil, fmt.Errorf("scan external-agent job delivery inspection: %w", err)
		}
		delivery.NotificationKind = safeAdminNotificationKind(kind)
		delivery.PublishState = safeAdminPublishState(publishState)
		delivery.LastErrorCode = safeAdminErrorCode(errorCode)
		delivery.NextAttemptAt = fromUnix(nextAttemptAt)
		delivery.RecoveredSlackTS = safeAdminSlackTimestamp(recoveredTS)
		delivery.DeliveryMode = safeAdminDeliveryMode(deliveryMode)
		delivery.UploadState = safeAdminUploadState(uploadState)
		delivery.SlackFileIDPresent = filePresent != 0
		view.Deliveries = append(view.Deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read external-agent job delivery inspection: %w", err)
	}
	return view, nil
}

// NotificationHealth returns content-free counts for the durable notification
// outbox. Expired leases are immediately stuck; overdue retries are stuck only
// after the configured threshold.
func (s *ExternalAgentJobStore) NotificationHealth(ctx context.Context, now time.Time, stuckThreshold time.Duration) (domain.ExternalAgentJobNotificationHealth, error) {
	if s == nil || s.db == nil {
		return domain.ExternalAgentJobNotificationHealth{}, errors.New("external-agent notification health store is not configured")
	}
	if stuckThreshold < 0 {
		return domain.ExternalAgentJobNotificationHealth{}, errors.New("notification stuck threshold must not be negative")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	var health domain.ExternalAgentJobNotificationHealth
	rows, err := s.db.QueryContext(ctx, `SELECT publish_state, COUNT(*)
		FROM external_agent_job_notifications GROUP BY publish_state`)
	if err != nil {
		return health, fmt.Errorf("count external-agent notification states: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return health, fmt.Errorf("scan external-agent notification state count: %w", err)
		}
		switch domain.NotificationPublishState(state) {
		case domain.NotificationPending:
			health.Pending += count
		case domain.NotificationPublishing:
			health.Publishing += count
		case domain.NotificationUnknown:
			health.Unknown += count
		case domain.NotificationPublished:
			health.Published += count
		}
	}
	if err := rows.Err(); err != nil {
		return health, fmt.Errorf("read external-agent notification state counts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return health, fmt.Errorf("close external-agent notification state counts: %w", err)
	}
	permanentCodes := []string{"result_artifact_invalid", "result_delivery_failed", "result_destination_mismatch", "notification_delivery_invalid", "result_file_upload_unknown"}
	args := make([]any, 0, len(permanentCodes)+1)
	args = append(args, domain.NotificationPublished)
	for _, code := range permanentCodes {
		args = append(args, code)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(permanentCodes)), ",")
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_agent_job_notifications
		WHERE publish_state != ? AND last_error_code IN (`+placeholders+`)`, args...).Scan(&health.PermanentFailures); err != nil {
		return health, fmt.Errorf("count permanent external-agent notification failures: %w", err)
	}
	cutoff := now.Add(-stuckThreshold).UnixNano()
	stuckArgs := make([]any, 0, len(permanentCodes)+4)
	stuckArgs = append(stuckArgs, domain.NotificationPublished)
	for _, code := range permanentCodes {
		stuckArgs = append(stuckArgs, code)
	}
	stuckArgs = append(stuckArgs, domain.NotificationPublishing, now.UnixNano(), cutoff)
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_agent_job_notifications
		WHERE publish_state != ? AND last_error_code NOT IN (`+placeholders+`)
		AND ((publish_state = ? AND lease_expiry > 0 AND lease_expiry <= ?) OR
		(next_attempt_at > 0 AND next_attempt_at <= ?))`, stuckArgs...).Scan(&health.Stuck); err != nil {
		return health, fmt.Errorf("count stuck external-agent notifications: %w", err)
	}
	return health, nil
}

func (s *ExternalAgentJobStore) ClaimNext(ctx context.Context, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJob, error) {
	if strings.TrimSpace(owner) == "" || leaseTTL <= 0 {
		return nil, errors.New("job lease owner and positive TTL are required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("begin external-agent job claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var jobID string
	err = tx.QueryRowContext(ctx, `SELECT job_id FROM external_agent_jobs
		WHERE status = ? AND (lease_expiry = 0 OR lease_expiry <= ?)
		ORDER BY created_at ASC LIMIT 1`, domain.JobQueued, now.UnixNano()).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select claimable external-agent job: %w", err)
	}
	leaseExpiry := now.Add(leaseTTL)
	result, err := tx.ExecContext(ctx, `UPDATE external_agent_jobs SET
		status = ?, attempt = attempt + 1, lease_owner = ?, lease_expiry = ?,
		heartbeat_at = ?, started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END,
		status_revision = status_revision + 1, updated_at = ?
		WHERE job_id = ? AND status = ? AND (lease_expiry = 0 OR lease_expiry <= ?)`,
		domain.JobRunning, owner, leaseExpiry.UnixNano(), now.UnixNano(), now.UnixNano(), now.UnixNano(),
		jobID, domain.JobQueued, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("claim external-agent job: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return nil, nil
	}
	job, err := s.load(ctx, tx, `WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, fmt.Errorf("load claimed external-agent job: %w", err)
	}
	if err := insertJobEvent(ctx, tx, job, "lease"); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit external-agent job claim: %w", err)
	}
	return &job, nil
}

func (s *ExternalAgentJobStore) RenewLease(ctx context.Context, jobID, owner string, attempt int, now time.Time, leaseTTL time.Duration) error {
	if leaseTTL <= 0 || owner == "" || attempt <= 0 {
		return errors.New("invalid external-agent lease renewal")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_jobs SET lease_expiry = ?, heartbeat_at = ?, updated_at = ?
		WHERE job_id = ? AND lease_owner = ? AND attempt = ? AND status IN (?, ?) AND lease_expiry > ?`,
		now.Add(leaseTTL).UnixNano(), now.UnixNano(), now.UnixNano(), jobID, owner, attempt, domain.JobRunning, domain.JobCancelRequested, now.UnixNano())
	if err != nil {
		return fmt.Errorf("renew external-agent job lease: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return errors.New("external-agent job lease is lost")
	}
	return nil
}

func (s *ExternalAgentJobStore) AssignACPSession(ctx context.Context, jobID, owner string, attempt int, sessionID string) error {
	if sessionID == "" {
		return errors.New("ACP session ID is required")
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_jobs SET acp_session_id = ?, updated_at = ?
		WHERE job_id = ? AND lease_owner = ? AND attempt = ? AND status = ? AND acp_session_id = '' AND lease_expiry > ?`,
		sessionID, now.UnixNano(), jobID, owner, attempt, domain.JobRunning, now.UnixNano())
	if err != nil {
		return fmt.Errorf("assign ACP session: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return errors.New("ACP session cannot be assigned to this job attempt")
	}
	return nil
}

func (s *ExternalAgentJobStore) MarkSideEffectsPossible(ctx context.Context, jobID, owner string, attempt int) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_jobs SET side_effects_possible = 1, updated_at = ?
		WHERE job_id = ? AND lease_owner = ? AND attempt = ? AND status = ? AND lease_expiry > ?`,
		now.UnixNano(), jobID, owner, attempt, domain.JobRunning, now.UnixNano())
	if err != nil {
		return fmt.Errorf("mark external-agent side effects: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return errors.New("external-agent job lease is lost before side-effect boundary")
	}
	return nil
}

func (s *ExternalAgentJobStore) RequestCancellation(ctx context.Context, jobID, actor string) (*domain.ExternalAgentJob, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	job, err := s.load(ctx, tx, `WHERE job_id = ?`, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if job.Actor != actor {
		return nil, errors.New("external-agent cancellation actor is not authorized")
	}
	previous := job.Status
	expectedRevision := job.StatusRevision
	if job.Status == domain.JobQueued {
		if err := job.Transition(domain.JobCancelled); err != nil {
			return nil, err
		}
	} else if job.Status == domain.JobRunning {
		if err := job.Transition(domain.JobCancelRequested); err != nil {
			return nil, err
		}
	} else if job.Status != domain.JobCancelRequested && job.Status != domain.JobCancelled && job.Status != domain.JobCompletionUnknown {
		return nil, errors.New("external-agent job is not cancellable")
	} else {
		return &job, nil
	}
	job.StatusRevision++
	job.UpdatedAt = time.Now().UTC()
	if job.Status == domain.JobCancelled {
		job.FinishedAt = job.UpdatedAt
	}
	changed, err := tx.ExecContext(ctx, `UPDATE external_agent_jobs SET status = ?, status_revision = ?, finished_at = ?, updated_at = ? WHERE job_id = ? AND status = ? AND status_revision = ?`, job.Status, job.StatusRevision, unix(job.FinishedAt), unix(job.UpdatedAt), job.ID, previous, expectedRevision)
	if err != nil {
		return nil, err
	}
	if affected, _ := changed.RowsAffected(); affected != 1 {
		return nil, errors.New("external-agent cancellation lost its compare-and-set")
	}
	if err := insertJobEvent(ctx, tx, job, "cancellation"); err != nil {
		return nil, err
	}
	if isNotificationTerminal(job.Status) {
		if err := insertJobNotification(ctx, tx, job, nil); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *ExternalAgentJobStore) BeginReconciliation(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJob, error) {
	if strings.TrimSpace(owner) == "" || leaseTTL <= 0 {
		return nil, errors.New("reconciliation lease owner and positive TTL are required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	job, err := s.load(ctx, tx, `WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, err
	}
	if job.Actor != actor || job.ConversationKey != conversationKey {
		return nil, errors.New("external-agent reconciliation is not authorized")
	}
	if job.Status != domain.JobCompletionUnknown {
		return nil, errors.New("external-agent job is not awaiting reconciliation")
	}
	leaseExpiry := now.Add(leaseTTL)
	result, err := tx.ExecContext(ctx, `UPDATE external_agent_jobs SET status = ?, attempt = attempt + 1, lease_owner = ?, lease_expiry = ?, heartbeat_at = ?, status_revision = status_revision + 1, updated_at = ? WHERE job_id = ? AND status = ? AND status_revision = ?`, domain.JobReconciling, owner, leaseExpiry.UnixNano(), now.UnixNano(), now.UnixNano(), jobID, domain.JobCompletionUnknown, job.StatusRevision)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, errors.New("external-agent reconciliation compare-and-set failed")
	}
	job, err = s.load(ctx, tx, `WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, err
	}
	if err := insertJobEvent(ctx, tx, job, "reconciliation"); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *ExternalAgentJobStore) Transition(ctx context.Context, jobID, owner string, attempt int, next domain.ExternalAgentJobStatus, result *domain.AcpInvocationResult, errorCode string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	job, err := s.load(ctx, tx, `WHERE job_id = ?`, jobID)
	if err != nil {
		return err
	}
	if job.Attempt != attempt || job.LeaseOwner != owner || job.LeaseExpiry.IsZero() || !job.LeaseExpiry.After(now) {
		return errors.New("external-agent job lease is lost")
	}
	expectedRevision := job.StatusRevision
	previous := job.Status
	if err := job.Transition(next); err != nil {
		return err
	}
	job.StatusRevision++
	job.UpdatedAt = now.UTC()
	if next == domain.JobCompleted || next == domain.JobFailed || next == domain.JobCancelled || next == domain.JobAbandoned {
		job.FinishedAt = job.UpdatedAt
	}
	if result != nil {
		job.ResultSummary, job.ResultArtifact, job.ResultSHA256, job.ResultBytes = result.Text, result.ArtifactRef, result.ResultSHA256, result.ResultBytes
		if result.DeliveryMode == domain.JobResultDeliveryFile {
			job.ResultSummary = ""
		}
	}
	job.ErrorCode = errorCode
	leaseOwner, leaseExpiry, heartbeat := job.LeaseOwner, unix(job.LeaseExpiry), unix(job.HeartbeatAt)
	if next == domain.JobQueued {
		// A safe retry releases its old lease so the queued job is immediately claimable.
		leaseOwner, leaseExpiry, heartbeat = "", 0, 0
	}
	query := `UPDATE external_agent_jobs SET status = ?, result_summary = ?, result_artifact = ?, result_sha256 = ?, result_bytes = ?, error_code = ?, status_revision = ?, finished_at = ?, lease_owner = ?, lease_expiry = ?, heartbeat_at = ?, updated_at = ?
		WHERE job_id = ? AND status = ? AND lease_owner = ? AND attempt = ? AND status_revision = ? AND lease_expiry > ?`
	args := []any{job.Status, job.ResultSummary, job.ResultArtifact, job.ResultSHA256, job.ResultBytes, job.ErrorCode, job.StatusRevision, unix(job.FinishedAt), leaseOwner, leaseExpiry, heartbeat, unix(job.UpdatedAt), job.ID, previous, owner, attempt, expectedRevision, now.UnixNano()}
	if next == domain.JobQueued {
		query += ` AND timeout_at > ?`
		args = append(args, now.UnixNano())
	}
	changed, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	affected, _ := changed.RowsAffected()
	if affected != 1 {
		return errors.New("external-agent job transition lost its lease")
	}
	if err := insertJobEvent(ctx, tx, job, "transition"); err != nil {
		return err
	}
	if isNotificationTerminal(job.Status) {
		if err := insertJobNotification(ctx, tx, job, result); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *ExternalAgentJobStore) ListExpiredRunning(ctx context.Context, now time.Time) ([]domain.ExternalAgentJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM external_agent_jobs WHERE status IN (?, ?) AND lease_expiry > 0 AND lease_expiry <= ? ORDER BY updated_at ASC`, domain.JobRunning, domain.JobCancelRequested, now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []domain.ExternalAgentJob
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *ExternalAgentJobStore) RecoverExpired(ctx context.Context, jobID string, attempt, statusRevision int, now time.Time, next domain.ExternalAgentJobStatus, errorCode string) error {
	if next != domain.JobQueued && next != domain.JobFailed && next != domain.JobCompletionUnknown {
		return errors.New("invalid expired external-agent recovery status")
	}
	finished := int64(0)
	if next != domain.JobQueued {
		finished = now.UnixNano()
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin expired external-agent recovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	query := `UPDATE external_agent_jobs SET status = ?, error_code = ?, status_revision = status_revision + 1, finished_at = ?, lease_owner = '', lease_expiry = 0, heartbeat_at = 0, updated_at = ?
		WHERE job_id = ? AND attempt = ? AND status_revision = ? AND status IN (?, ?) AND lease_expiry > 0 AND lease_expiry <= ?`
	args := []any{next, errorCode, finished, now.UnixNano(), jobID, attempt, statusRevision, domain.JobRunning, domain.JobCancelRequested, now.UnixNano()}
	if next == domain.JobQueued {
		query += ` AND timeout_at > ?`
		args = append(args, now.UnixNano())
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("recover expired external-agent job: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return errors.New("expired external-agent recovery compare-and-set failed")
	}
	job, err := s.load(ctx, tx, `WHERE job_id = ?`, jobID)
	if err != nil {
		return err
	}
	if err := insertJobEvent(ctx, tx, job, "recovery"); err != nil {
		return err
	}
	if isNotificationTerminal(job.Status) {
		if err := insertJobNotification(ctx, tx, job, nil); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func isNotificationTerminal(status domain.ExternalAgentJobStatus) bool {
	switch status {
	case domain.JobCompleted, domain.JobFailed, domain.JobCancelled, domain.JobCompletionUnknown, domain.JobAbandoned:
		return true
	default:
		return false
	}
}

func insertJobNotification(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, job domain.ExternalAgentJob, result *domain.AcpInvocationResult) error {
	var notification domain.ExternalAgentJobNotification
	var err error
	if result != nil && job.Status == domain.JobCompleted && job.Mode == domain.JobDetached {
		notification, err = domain.NewExternalAgentJobDelivery(job, *result)
	} else {
		notification, err = domain.NewExternalAgentJobNotification(job)
	}
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, `INSERT INTO external_agent_job_notifications (
		job_id, status_revision, kind, canonical_markdown, content_sha256,
		renderer_version, channel_id, thread_ts, publish_state, lease_owner,
		lease_expiry, attempts, next_attempt_at, recovered_slack_ts, last_error_code,
		created_at, updated_at, delivery_mode, policy_version, artifact_ref, result_bytes,
		max_markdown_parts, upload_state, slack_file_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', 0, 0, ?, '', '', ?, ?, ?, ?, ?, ?, ?, ?, '')
		ON CONFLICT(job_id, status_revision, kind) DO NOTHING`,
		notification.JobID, notification.StatusRevision, notification.Kind,
		notification.CanonicalMarkdown, notification.ContentSHA256, notification.RendererVersion,
		notification.Target.ChannelID, notification.Target.ThreadTS, notification.PublishState,
		unix(job.UpdatedAt), unix(job.UpdatedAt), unix(job.UpdatedAt), notification.DeliveryMode, notification.PolicyVersion,
		notification.ArtifactRef, notification.ResultBytes, notification.MaxMarkdownParts, notification.UploadState,
	)
	return err
}

func (s *ExternalAgentJobStore) ClaimNextNotification(ctx context.Context, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJobNotification, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || leaseTTL <= 0 {
		return nil, errors.New("notification lease owner and positive TTL are required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("begin notification claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var jobID, kind, previousState string
	var revision int
	err = tx.QueryRowContext(ctx, `SELECT job_id, status_revision, kind
		FROM external_agent_job_notifications
		WHERE ((publish_state IN (?, ?) AND next_attempt_at <= ?) OR
			(publish_state = ? AND lease_expiry > 0 AND lease_expiry <= ?))
		AND last_error_code NOT IN ('result_artifact_invalid', 'result_delivery_failed', 'result_destination_mismatch', 'notification_delivery_invalid')
		AND NOT (last_error_code = 'result_file_upload_unknown' AND publish_state = 'unknown')
		ORDER BY next_attempt_at ASC, created_at ASC LIMIT 1`,
		domain.NotificationPending, domain.NotificationUnknown, now.UnixNano(), domain.NotificationPublishing, now.UnixNano()).Scan(&jobID, &revision, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select notification claimable: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT publish_state FROM external_agent_job_notifications WHERE job_id = ? AND status_revision = ? AND kind = ?`, jobID, revision, kind).Scan(&previousState); err != nil {
		return nil, fmt.Errorf("read notification state before claim: %w", err)
	}
	leaseExpiry := now.Add(leaseTTL)
	changed, err := tx.ExecContext(ctx, `UPDATE external_agent_job_notifications SET
		publish_state = ?, lease_owner = ?, lease_expiry = ?, attempts = attempts + 1, updated_at = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND
		((publish_state IN (?, ?) AND next_attempt_at <= ?) OR
		 (publish_state = ? AND lease_expiry > 0 AND lease_expiry <= ?))
		AND last_error_code NOT IN ('result_artifact_invalid', 'result_delivery_failed', 'result_destination_mismatch', 'notification_delivery_invalid')
		AND NOT (last_error_code = 'result_file_upload_unknown' AND publish_state = 'unknown')`,
		domain.NotificationPublishing, owner, leaseExpiry.UnixNano(), now.UnixNano(), jobID, revision, kind,
		domain.NotificationPending, domain.NotificationUnknown, now.UnixNano(), domain.NotificationPublishing, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("claim notification: %w", err)
	}
	if affected, _ := changed.RowsAffected(); affected != 1 {
		return nil, nil
	}
	notification, err := loadNotification(ctx, tx, jobID, revision, kind)
	if err != nil {
		return nil, err
	}
	notification.NeedsReconciliation = previousState == string(domain.NotificationUnknown) || previousState == string(domain.NotificationPublishing) || notification.LastErrorCode == "notification_publish_ambiguous" || notification.LastErrorCode == "result_file_upload_unknown"
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit notification claim: %w", err)
	}
	return &notification, nil
}

func (s *ExternalAgentJobStore) MarkNotificationPublished(ctx context.Context, notification *domain.ExternalAgentJobNotification, slackTS string, now time.Time) error {
	if notification == nil || notification.JobID == "" || notification.LeaseOwner == "" || slackTS == "" {
		return errors.New("notification publication identity is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_job_notifications SET
		publish_state = ?, recovered_slack_ts = ?, lease_owner = '', lease_expiry = 0,
		last_error_code = '', updated_at = ?, upload_state = CASE WHEN delivery_mode = 'file' THEN ? ELSE upload_state END
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND publish_state = ?
		AND lease_owner = ? AND attempts = ? AND (policy_version = 'legacy_v1' OR
			(delivery_mode = 'markdown' OR (delivery_mode = 'file' AND upload_state = 'completed' AND length(slack_file_id) > 0)))`, domain.NotificationPublished, slackTS, now.UnixNano(),
		domain.JobResultUploadCompleted, notification.JobID, notification.StatusRevision, notification.Kind, domain.NotificationPublishing, notification.LeaseOwner, notification.Attempts)
	if err != nil {
		return fmt.Errorf("mark notification published: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotificationStateConflict
	}
	return nil
}

func (s *ExternalAgentJobStore) MarkNotificationFileID(ctx context.Context, notification *domain.ExternalAgentJobNotification, fileID string, now time.Time) error {
	if notification == nil || notification.JobID == "" || notification.LeaseOwner == "" || fileID == "" || strings.ContainsAny(fileID, "\x00\r\n") {
		return errors.New("notification file identity is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_job_notifications SET
		slack_file_id = ?, upload_state = ?, updated_at = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND publish_state = ?
		AND lease_owner = ? AND attempts = ? AND (slack_file_id = '' OR slack_file_id = ?)`, fileID, domain.JobResultUploadURLRequested, now.UnixNano(), notification.JobID,
		notification.StatusRevision, notification.Kind, domain.NotificationPublishing, notification.LeaseOwner, notification.Attempts, fileID)
	if err != nil {
		return fmt.Errorf("persist notification Slack file ID: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotificationStateConflict
	}
	return nil
}

func (s *ExternalAgentJobStore) MarkNotificationRetry(ctx context.Context, notification *domain.ExternalAgentJobNotification, errorCode string, nextAttemptAt, now time.Time) error {
	if notification == nil || notification.JobID == "" || notification.LeaseOwner == "" || nextAttemptAt.IsZero() {
		return errors.New("notification retry identity is required")
	}
	code := safeNotificationError(errorCode)
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_job_notifications SET
		publish_state = ?, lease_owner = '', lease_expiry = 0, next_attempt_at = ?, last_error_code = ?, updated_at = ?,
		upload_state = CASE WHEN delivery_mode = 'file' AND ? = 'result_file_upload_unknown' THEN ? ELSE upload_state END
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND publish_state = ?
		AND lease_owner = ? AND attempts = ?`, domain.NotificationPending, nextAttemptAt.UnixNano(), code, now.UnixNano(),
		code, domain.JobResultUploadUnknown, notification.JobID, notification.StatusRevision, notification.Kind, domain.NotificationPublishing, notification.LeaseOwner, notification.Attempts)
	if err != nil {
		return fmt.Errorf("mark notification retry: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotificationStateConflict
	}
	return nil
}

func (s *ExternalAgentJobStore) MarkNotificationUploadState(ctx context.Context, notification *domain.ExternalAgentJobNotification, state domain.JobResultUploadState, now time.Time) error {
	if notification == nil || notification.JobID == "" || notification.LeaseOwner == "" {
		return errors.New("notification upload identity is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_job_notifications SET upload_state = ?, updated_at = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND publish_state = ?
		AND lease_owner = ? AND attempts = ?`, state, now.UnixNano(), notification.JobID,
		notification.StatusRevision, notification.Kind, domain.NotificationPublishing, notification.LeaseOwner, notification.Attempts)
	if err != nil {
		return fmt.Errorf("persist notification upload state: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotificationStateConflict
	}
	return nil
}

func (s *ExternalAgentJobStore) IsArtifactReferenced(ctx context.Context, reference string) (bool, error) {
	if s == nil || s.db == nil || reference == "" {
		return false, errors.New("artifact reference store is not configured")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_agent_job_notifications
		WHERE artifact_ref = ? AND publish_state != ?
		AND last_error_code NOT IN ('result_artifact_invalid', 'result_delivery_failed', 'result_destination_mismatch', 'notification_delivery_invalid')
		AND NOT (last_error_code = 'result_file_upload_unknown' AND publish_state = ?)`, reference, domain.NotificationPublished, domain.NotificationUnknown).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect active result artifact reference: %w", err)
	}
	return count > 0, nil
}

func (s *ExternalAgentJobStore) MarkNotificationUnknown(ctx context.Context, notification *domain.ExternalAgentJobNotification, errorCode string) error {
	if notification == nil || notification.JobID == "" || notification.LeaseOwner == "" {
		return errors.New("notification failure identity is required")
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin notification failure update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	code := safeNotificationError(errorCode)
	permanent := permanentNotificationError(code)
	nextAttemptAt := now
	if !permanent {
		nextAttemptAt = now.Add(notificationRetryDelay(notification.Attempts, rand.Float64()*2-1))
	}
	result, err := tx.ExecContext(ctx, `UPDATE external_agent_job_notifications SET
		publish_state = ?, lease_owner = '', lease_expiry = 0,
		next_attempt_at = CASE WHEN ? THEN next_attempt_at ELSE ? END, last_error_code = ?, updated_at = ?
		, upload_state = CASE WHEN delivery_mode = 'file' THEN ? ELSE upload_state END
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND publish_state = ?
		AND lease_owner = ? AND attempts = ?`, domain.NotificationUnknown, boolInt(permanent), nextAttemptAt.UnixNano(), code, now.UnixNano(),
		domain.JobResultUploadUnknown, notification.JobID, notification.StatusRevision, notification.Kind, domain.NotificationPublishing, notification.LeaseOwner, notification.Attempts)
	if err != nil {
		return fmt.Errorf("mark notification unknown: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotificationStateConflict
	}
	if permanent {
		markdown := fmt.Sprintf("OpenCode job `%s` completed, but its result could not be delivered.\nDelivery code: `%s`.", notification.JobID, code)
		digest := sha256.Sum256([]byte(markdown))
		_, err = tx.ExecContext(ctx, `INSERT INTO external_agent_job_notifications (
			job_id, status_revision, kind, canonical_markdown, content_sha256,
			renderer_version, channel_id, thread_ts, publish_state, lease_owner,
			lease_expiry, attempts, next_attempt_at, recovered_slack_ts, last_error_code,
			created_at, updated_at, delivery_mode, policy_version, artifact_ref, result_bytes,
			max_markdown_parts, upload_state, slack_file_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', 0, 0, ?, '', '', ?, ?, 'markdown', 'legacy_v1', '', 0, 1, 'not_applicable', '')
			ON CONFLICT(job_id, status_revision, kind) DO NOTHING`,
			notification.JobID, notification.StatusRevision, domain.JobNotificationFailure,
			markdown, fmt.Sprintf("%x", digest), domain.JobNotificationRenderer,
			notification.Target.ChannelID, notification.Target.ThreadTS,
			domain.NotificationPending, now.UnixNano(), now.UnixNano(), now.UnixNano())
		if err != nil {
			return fmt.Errorf("enqueue result delivery failure notification: %w", err)
		}
	}
	return tx.Commit()
}

func notificationRetryDelay(attempt int, jitter float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := notificationRetryBaseDelay
	for current := 1; current < attempt && delay < notificationRetryMaxDelay; current++ {
		if delay >= notificationRetryMaxDelay/2 {
			delay = notificationRetryMaxDelay
			break
		}
		delay *= 2
	}
	if delay >= notificationRetryMaxDelay {
		return notificationRetryMaxDelay
	}
	jitter = min(max(jitter, -1), 1)
	return delay + time.Duration(float64(delay)*notificationRetryJitter*jitter)
}

func permanentNotificationError(code string) bool {
	switch code {
	case "result_artifact_invalid", "result_delivery_failed", "result_destination_mismatch", "notification_delivery_invalid", "result_file_upload_unknown":
		return true
	default:
		return false
	}
}

func safeNotificationError(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return "notification_publish_ambiguous"
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return "notification_publish_ambiguous"
		}
	}
	return value
}

func safeAdminJobStatus(value string) domain.ExternalAgentJobStatus {
	switch domain.ExternalAgentJobStatus(value) {
	case domain.JobQueued, domain.JobRunning, domain.JobCancelRequested,
		domain.JobInterruptedSafe, domain.JobCompletionUnknown, domain.JobReconciling,
		domain.JobCompleted, domain.JobFailed, domain.JobCancelled, domain.JobAbandoned:
		return domain.ExternalAgentJobStatus(value)
	default:
		return domain.ExternalAgentJobStatus("unknown")
	}
}

func safeAdminNotificationKind(value string) string {
	switch value {
	case domain.JobNotificationTerminal, domain.JobNotificationFailure:
		return value
	default:
		return "unknown"
	}
}

func safeAdminPublishState(value string) domain.NotificationPublishState {
	switch domain.NotificationPublishState(value) {
	case domain.NotificationPending, domain.NotificationPublishing,
		domain.NotificationPublished, domain.NotificationUnknown:
		return domain.NotificationPublishState(value)
	default:
		return domain.NotificationUnknown
	}
}

func safeAdminErrorCode(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	code := safeNotificationError(value)
	switch code {
	case "result_artifact_invalid", "result_delivery_failed", "result_destination_mismatch",
		"notification_delivery_invalid", "notification_publish_ambiguous", "result_file_upload_failed",
		"result_file_upload_unknown", "result_file_completion_failed", "notification_state_conflict",
		"notification_state_persist_failed":
		return code
	default:
		return "notification_publish_ambiguous"
	}
}

func safeAdminSlackTimestamp(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 64 {
		return ""
	}
	dot := strings.IndexByte(value, '.')
	if dot <= 0 || dot == len(value)-1 || strings.IndexByte(value[dot+1:], '.') >= 0 {
		return ""
	}
	for _, r := range value {
		if r != '.' && (r < '0' || r > '9') {
			return ""
		}
	}
	return value
}

func safeAdminDeliveryMode(value string) domain.JobResultDeliveryMode {
	switch domain.JobResultDeliveryMode(value) {
	case domain.JobResultDeliveryMarkdown, domain.JobResultDeliveryFile:
		return domain.JobResultDeliveryMode(value)
	default:
		return ""
	}
}

func safeAdminUploadState(value string) domain.JobResultUploadState {
	switch domain.JobResultUploadState(value) {
	case domain.JobResultUploadNotApplicable, domain.JobResultUploadPending,
		domain.JobResultUploadURLRequested, domain.JobResultUploadBytesUploaded,
		domain.JobResultUploadCompleted, domain.JobResultUploadUnknown:
		return domain.JobResultUploadState(value)
	default:
		return domain.JobResultUploadUnknown
	}
}

const notificationColumns = `n.job_id, n.status_revision, n.kind, n.canonical_markdown, n.content_sha256, n.renderer_version, n.channel_id, n.thread_ts, n.publish_state, n.lease_owner, n.lease_expiry, n.attempts, n.next_attempt_at, n.recovered_slack_ts, n.last_error_code, n.created_at, n.updated_at, n.delivery_mode, n.policy_version, n.artifact_ref, n.result_bytes, n.max_markdown_parts, n.upload_state, n.slack_file_id`

func loadNotification(ctx context.Context, queryer queryRower, jobID string, revision int, kind string) (domain.ExternalAgentJobNotification, error) {
	var n domain.ExternalAgentJobNotification
	var state string
	var leaseExpiry, nextAttempt, created, updated int64
	var deliveryMode, policyVersion, uploadState string
	row := queryer.QueryRowContext(ctx, `SELECT `+notificationColumns+`, j.actor, j.conversation_key
		FROM external_agent_job_notifications n
		JOIN external_agent_jobs j ON j.job_id = n.job_id
		WHERE n.job_id = ? AND n.status_revision = ? AND n.kind = ?`, jobID, revision, kind)
	err := row.Scan(&n.JobID, &n.StatusRevision, &n.Kind, &n.CanonicalMarkdown, &n.ContentSHA256, &n.RendererVersion, &n.Target.ChannelID, &n.Target.ThreadTS, &state, &n.LeaseOwner, &leaseExpiry, &n.Attempts, &nextAttempt, &n.RecoveredSlackTS, &n.LastErrorCode, &created, &updated, &deliveryMode, &policyVersion, &n.ArtifactRef, &n.ResultBytes, &n.MaxMarkdownParts, &uploadState, &n.SlackFileID, &n.Actor, &n.ConversationKey)
	if err != nil {
		return domain.ExternalAgentJobNotification{}, fmt.Errorf("load notification: %w", err)
	}
	n.Target.CorrelationID = fmt.Sprintf("job:%s:%d:%s", n.JobID, n.StatusRevision, n.Kind)
	n.PublishState = domain.NotificationPublishState(state)
	n.DeliveryMode = domain.JobResultDeliveryMode(deliveryMode)
	n.PolicyVersion = policyVersion
	n.UploadState = domain.JobResultUploadState(uploadState)
	n.ContentBytes = n.ResultBytes
	n.LeaseExpiry, n.NextAttemptAt = fromUnix(leaseExpiry), fromUnix(nextAttempt)
	return n, nil
}

const jobColumns = `job_id, mode, provider, profile, primary_project, additional_projects, registry_revision, task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id, conversation_key, status, attempt, acp_session_id, side_effects_possible, lease_owner, lease_expiry, heartbeat_at, timeout_at, result_summary, result_artifact, result_sha256, result_bytes, error_code, status_revision, created_at, started_at, finished_at, updated_at`

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type rowScanner interface{ Scan(...any) error }

func (s *ExternalAgentJobStore) load(ctx context.Context, queryer queryRower, where string, args ...any) (domain.ExternalAgentJob, error) {
	return scanJob(queryer.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM external_agent_jobs `+where, args...))
}

func scanJob(row rowScanner) (domain.ExternalAgentJob, error) {
	var (
		job                                                                  domain.ExternalAgentJob
		mode, projects, status, conversation                                 string
		leaseExpiry, heartbeat, timeout, created, started, finished, updated int64
		sideEffects                                                          int
	)
	err := row.Scan(&job.ID, &mode, &job.Provider, &job.Profile, &job.PrimaryProject, &projects, &job.RegistryRevision, &job.Task, &job.RequestSHA256, &job.WrapperCallID, &job.OriginalCallID, &job.Actor, &job.TeamID, &conversation, &status, &job.Attempt, &job.ACPSessionID, &sideEffects, &job.LeaseOwner, &leaseExpiry, &heartbeat, &timeout, &job.ResultSummary, &job.ResultArtifact, &job.ResultSHA256, &job.ResultBytes, &job.ErrorCode, &job.StatusRevision, &created, &started, &finished, &updated)
	if err != nil {
		return domain.ExternalAgentJob{}, err
	}
	if err := json.Unmarshal([]byte(projects), &job.AdditionalProjects); err != nil {
		return domain.ExternalAgentJob{}, fmt.Errorf("decode external-agent projects: %w", err)
	}
	job.Mode = domain.ExternalAgentJobMode(mode)
	job.Status = domain.ExternalAgentJobStatus(status)
	job.ConversationKey = domain.ConversationKey(conversation)
	job.SideEffectsPossible = sideEffects != 0
	job.LeaseExpiry, job.HeartbeatAt, job.TimeoutAt = fromUnix(leaseExpiry), fromUnix(heartbeat), fromUnix(timeout)
	job.CreatedAt, job.StartedAt, job.FinishedAt, job.UpdatedAt = fromUnix(created), fromUnix(started), fromUnix(finished), fromUnix(updated)
	return job, nil
}

func insertJobEvent(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, job domain.ExternalAgentJob, kind string) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO external_agent_job_events (job_id, status_revision, event_kind, created_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`, job.ID, job.StatusRevision, kind, job.UpdatedAt.UnixNano())
	return err
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func unix(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}

func fromUnix(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}
