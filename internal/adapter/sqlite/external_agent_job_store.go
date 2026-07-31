package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.ExternalAgentJobStore = (*ExternalAgentJobStore)(nil)
var _ port.ExpiredExternalAgentJobRecovery = (*ExternalAgentJobStore)(nil)
var _ port.ExternalAgentJobNotificationStore = (*ExternalAgentJobStore)(nil)
var _ port.ExternalAgentJobNotificationRetryStore = (*ExternalAgentJobStore)(nil)
var _ port.ExternalAgentJobDeliveryStore = (*ExternalAgentJobStore)(nil)
var _ port.ArtifactReferenceChecker = (*ExternalAgentJobStore)(nil)
var _ port.ExternalAgentJobReconciler = (*ExternalAgentJobStore)(nil)

var ErrNotificationStateConflict = errors.New("external-agent notification state conflict")

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
		AND last_error_code NOT IN ('result_artifact_invalid', 'result_delivery_failed', 'result_destination_mismatch', 'notification_delivery_invalid', 'result_file_upload_unknown')
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
		AND last_error_code NOT IN ('result_artifact_invalid', 'result_delivery_failed', 'result_destination_mismatch', 'notification_delivery_invalid', 'result_file_upload_unknown')`,
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
	notification.NeedsReconciliation = previousState == string(domain.NotificationUnknown) || previousState == string(domain.NotificationPublishing)
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
		publish_state = ?, lease_owner = '', lease_expiry = 0, next_attempt_at = ?, last_error_code = ?, updated_at = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND publish_state = ?
		AND lease_owner = ? AND attempts = ?`, domain.NotificationPending, nextAttemptAt.UnixNano(), code, now.UnixNano(),
		notification.JobID, notification.StatusRevision, notification.Kind, domain.NotificationPublishing, notification.LeaseOwner, notification.Attempts)
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
		AND last_error_code NOT IN ('result_artifact_invalid', 'result_delivery_failed', 'result_destination_mismatch', 'notification_delivery_invalid')`, reference, domain.NotificationPublished).Scan(&count); err != nil {
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
	result, err := tx.ExecContext(ctx, `UPDATE external_agent_job_notifications SET
		publish_state = ?, lease_owner = '', lease_expiry = 0, next_attempt_at = ?, last_error_code = ?, updated_at = ?
		, upload_state = CASE WHEN delivery_mode = 'file' THEN ? ELSE upload_state END
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND publish_state = ?
		AND lease_owner = ? AND attempts = ?`, domain.NotificationUnknown, now.UnixNano(), code, now.UnixNano(),
		domain.JobResultUploadUnknown, notification.JobID, notification.StatusRevision, notification.Kind, domain.NotificationPublishing, notification.LeaseOwner, notification.Attempts)
	if err != nil {
		return fmt.Errorf("mark notification unknown: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotificationStateConflict
	}
	if permanentNotificationError(code) {
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

const notificationColumns = `job_id, status_revision, kind, canonical_markdown, content_sha256, renderer_version, channel_id, thread_ts, publish_state, lease_owner, lease_expiry, attempts, next_attempt_at, recovered_slack_ts, last_error_code, created_at, updated_at, delivery_mode, policy_version, artifact_ref, result_bytes, max_markdown_parts, upload_state, slack_file_id`

func loadNotification(ctx context.Context, queryer queryRower, jobID string, revision int, kind string) (domain.ExternalAgentJobNotification, error) {
	var n domain.ExternalAgentJobNotification
	var state string
	var leaseExpiry, nextAttempt, created, updated int64
	var deliveryMode, policyVersion, uploadState string
	row := queryer.QueryRowContext(ctx, `SELECT `+notificationColumns+` FROM external_agent_job_notifications WHERE job_id = ? AND status_revision = ? AND kind = ?`, jobID, revision, kind)
	err := row.Scan(&n.JobID, &n.StatusRevision, &n.Kind, &n.CanonicalMarkdown, &n.ContentSHA256, &n.RendererVersion, &n.Target.ChannelID, &n.Target.ThreadTS, &state, &n.LeaseOwner, &leaseExpiry, &n.Attempts, &nextAttempt, &n.RecoveredSlackTS, &n.LastErrorCode, &created, &updated, &deliveryMode, &policyVersion, &n.ArtifactRef, &n.ResultBytes, &n.MaxMarkdownParts, &uploadState, &n.SlackFileID)
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
