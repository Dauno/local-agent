package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const activationTerminalStates = `('completed', 'failed', 'completion_unknown')`

const activationColumns = `a.job_id, a.status_revision, a.kind, a.activation_id, a.terminal_status,
	a.notification_sha256, a.result_sha256, a.actor, a.team_id, a.conversation_key, a.workstream_id, a.task_id,
	a.execution_identity, a.admission_revision, a.original_call_id,
	a.delivery_mode, a.content_bytes, a.slack_message_ts, a.published_at, a.state,
	a.attempt, a.lease_owner, a.lease_expiry, a.next_attempt_at, a.last_error_code,
	a.response_body, a.response_sha256, a.exchange_intent_id, a.correlation_id,
	a.response_slack_ts, a.fallback_required, a.fallback_slack_ts, a.created_at, a.updated_at`

func (s *ExternalAgentJobStore) GetActivation(ctx context.Context, activationID string) (*domain.ExternalAgentJobActivation, error) {
	if s == nil || s.db == nil || strings.TrimSpace(activationID) == "" {
		return nil, nil
	}
	activation, err := loadActivation(ctx, s.db, `WHERE a.activation_id = ?`, activationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get external-agent activation: %w", err)
	}
	return &activation, nil
}

// ActivationHealth returns content-free activation counts. An expired lease
// or a retry overdue by the stuck threshold is reported as stuck without
// loading any job task, result, or artifact data. Retired foreground
// activations (terminal rows stamped with the bounded
// foreground_activation_retired code) are explicitly excluded from the stuck
// count: they are expected v31 repair evidence, never a defect.
func (s *ExternalAgentJobStore) ActivationHealth(ctx context.Context, now time.Time, stuckThreshold time.Duration) (domain.ExternalAgentJobActivationHealth, error) {
	if s == nil || s.db == nil {
		return domain.ExternalAgentJobActivationHealth{}, errors.New("external-agent activation health store is not configured")
	}
	if stuckThreshold < 0 {
		return domain.ExternalAgentJobActivationHealth{}, errors.New("activation stuck threshold must not be negative")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	var health domain.ExternalAgentJobActivationHealth
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*)
		FROM external_agent_job_activations GROUP BY state`)
	if err != nil {
		return health, fmt.Errorf("count external-agent activation states: %w", err)
	}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			_ = rows.Close()
			return health, fmt.Errorf("scan external-agent activation state count: %w", err)
		}
		switch domain.ExternalAgentJobActivationState(state) {
		case domain.ActivationPending:
			health.Pending += count
		case domain.ActivationProcessing:
			health.Processing += count
		case domain.ActivationModelStarted:
			health.ModelStarted += count
		case domain.ActivationResponsePrepared:
			health.ResponsePrepared += count
		case domain.ActivationCompleted:
			health.Completed += count
		case domain.ActivationCompletionUnknown:
			health.CompletionUnknown += count
		case domain.ActivationFailed:
			health.Failed += count
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return health, fmt.Errorf("read external-agent activation state counts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return health, fmt.Errorf("close external-agent activation state counts: %w", err)
	}
	health.Processed = health.Completed + health.Failed
	cutoff := now.Add(-stuckThreshold).UnixNano()
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_agent_job_activations
		WHERE state NOT IN (?, ?, ?) AND last_error_code != ? AND (
			(state = ? AND next_attempt_at > 0 AND next_attempt_at <= ?) OR
			(state IN (?, ?, ?) AND lease_expiry > 0 AND lease_expiry <= ?)
		)`,
		domain.ActivationCompleted, domain.ActivationCompletionUnknown, domain.ActivationFailed,
		domain.ActivationForegroundRetiredCode,
		domain.ActivationPending, cutoff,
		domain.ActivationProcessing, domain.ActivationModelStarted, domain.ActivationResponsePrepared, now.UnixNano(),
	).Scan(&health.Stuck); err != nil {
		return health, fmt.Errorf("count stuck external-agent activations: %w", err)
	}
	return health, nil
}

func (s *ExternalAgentJobStore) ClaimNextActivation(ctx context.Context, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJobActivation, error) {
	return s.claimActivation(ctx, now, owner, leaseTTL, "", nil)
}

func (s *ExternalAgentJobStore) ReconcileActivation(ctx context.Context, activationID, actor, teamID string, conversationKey domain.ConversationKey, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJobActivation, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(teamID) == "" || conversationKey == "" {
		return nil, errors.New("external-agent activation binding is required")
	}
	activation, err := s.GetActivation(ctx, activationID)
	if err != nil || activation == nil {
		return activation, err
	}
	if activation.Actor != actor || activation.TeamID != teamID || activation.ConversationKey != conversationKey {
		return nil, errors.New("external-agent activation is not authorized")
	}
	return s.claimActivation(ctx, now, owner, leaseTTL, activationID, activation)
}

func (s *ExternalAgentJobStore) claimActivation(ctx context.Context, now time.Time, owner string, leaseTTL time.Duration, activationID string, expected *domain.ExternalAgentJobActivation) (*domain.ExternalAgentJobActivation, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("external-agent activation store is not configured")
	}
	if strings.TrimSpace(owner) == "" || leaseTTL <= 0 {
		return nil, errors.New("activation lease owner and positive TTL are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("begin external-agent activation claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	candidateWhere := `(
		(a.state = ? AND a.next_attempt_at <= ?) OR
		(a.state IN (?, ?, ?) AND a.lease_expiry > 0 AND a.lease_expiry <= ?)
	) AND NOT EXISTS (
		SELECT 1 FROM external_agent_job_activations prior
		WHERE prior.conversation_key = a.conversation_key
			AND prior.state NOT IN ` + activationTerminalStates + `
			AND (
				prior.published_at < a.published_at OR
				(prior.published_at = a.published_at AND prior.status_revision < a.status_revision) OR
				(prior.published_at = a.published_at AND prior.status_revision = a.status_revision AND prior.job_id < a.job_id) OR
				(prior.published_at = a.published_at AND prior.status_revision = a.status_revision AND prior.job_id = a.job_id AND prior.kind < a.kind)
			)
	)`
	args := []any{domain.ActivationPending, now.UnixNano(), domain.ActivationProcessing, domain.ActivationModelStarted, domain.ActivationResponsePrepared, now.UnixNano()}
	if activationID != "" {
		candidateWhere += ` AND a.activation_id = ?`
		args = append(args, activationID)
	}
	query := `SELECT a.job_id, a.status_revision, a.kind
		FROM external_agent_job_activations a
		WHERE ` + candidateWhere + `
		ORDER BY a.published_at ASC, a.status_revision ASC, a.job_id ASC, a.kind ASC LIMIT 1`
	var jobID, kind string
	var revision int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&jobID, &revision, &kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("select claimable external-agent activation: %w", err)
	}
	if expected != nil && (expected.JobID != jobID || expected.StatusRevision != revision || expected.Kind != kind) {
		return nil, port.ErrActivationClaimConflict
	}
	leaseExpiry := now.Add(leaseTTL)
	updateArgs := make([]any, 0, 8+len(args))
	updateArgs = append(updateArgs, domain.ActivationPending, domain.ActivationProcessing, owner, leaseExpiry.UnixNano(), now.UnixNano(), jobID, revision, kind)
	updateArgs = append(updateArgs, args...)
	changed, err := tx.ExecContext(ctx, `UPDATE external_agent_job_activations AS a SET
		state = CASE WHEN state = ? THEN ? ELSE state END,
		lease_owner = ?, lease_expiry = ?, attempt = attempt + 1, updated_at = ?
		WHERE a.job_id = ? AND a.status_revision = ? AND a.kind = ? AND `+candidateWhere, updateArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim external-agent activation: %w", err)
	}
	if affected, _ := changed.RowsAffected(); affected != 1 {
		return nil, port.ErrActivationClaimConflict
	}
	activation, err := loadActivation(ctx, tx, `WHERE a.job_id = ? AND a.status_revision = ? AND a.kind = ?`, jobID, revision, kind)
	if err != nil {
		return nil, fmt.Errorf("load claimed external-agent activation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit external-agent activation claim: %w", err)
	}
	return &activation, nil
}

func (s *ExternalAgentJobStore) RetryActivation(ctx context.Context, activation *domain.ExternalAgentJobActivation, errorCode string, nextAttemptAt, now time.Time) error {
	if err := validateActivationMutation(activation, now, nextAttemptAt); err != nil {
		return err
	}
	code := safeActivationError(errorCode)
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_job_activations SET
		state = ?, lease_owner = '', lease_expiry = 0, next_attempt_at = ?, last_error_code = ?, updated_at = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND state = ?
		AND lease_owner = ? AND attempt = ? AND lease_expiry > ?`,
		domain.ActivationPending, nextAttemptAt.UTC().UnixNano(), code, now.UTC().UnixNano(),
		activation.JobID, activation.StatusRevision, activation.Kind, domain.ActivationProcessing,
		activation.LeaseOwner, activation.Attempt, now.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("retry external-agent activation: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return port.ErrActivationStateConflict
	}
	return nil
}

func (s *ExternalAgentJobStore) MarkActivationModelStarted(ctx context.Context, activation *domain.ExternalAgentJobActivation, now time.Time) error {
	if err := validateActivationMutation(activation, now, time.Time{}); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_job_activations SET
		state = ?, last_error_code = '', updated_at = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND state = ?
		AND lease_owner = ? AND attempt = ? AND lease_expiry > ?`,
		domain.ActivationModelStarted, now.UTC().UnixNano(), activation.JobID, activation.StatusRevision,
		activation.Kind, domain.ActivationProcessing, activation.LeaseOwner, activation.Attempt, now.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("mark external-agent activation model started: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return port.ErrActivationStateConflict
	}
	return nil
}

// PrepareActivationResponseWithExchange closes the response-preparation gap in
// one SQLite transaction. The deterministic intent and correlation IDs make a
// retry return the same durable exchange instead of creating another reply.
func (s *ExternalAgentJobStore) PrepareActivationResponseWithExchange(
	ctx context.Context,
	activation *domain.ExternalAgentJobActivation,
	metadata domain.ConversationMetadata,
	message domain.Message,
	retain int,
	now time.Time,
) (port.PreparedAssistantExchange, error) {
	if s == nil || s.db == nil {
		return port.PreparedAssistantExchange{}, errors.New("external-agent activation store is not configured")
	}
	if activation == nil || activation.JobID == "" || activation.Kind == "" || activation.LeaseOwner == "" || activation.Attempt <= 0 {
		return port.PreparedAssistantExchange{}, errors.New("external-agent activation response identity is required")
	}
	if now.IsZero() {
		return port.PreparedAssistantExchange{}, errors.New("external-agent activation response time is required")
	}
	if retain <= 0 {
		return port.PreparedAssistantExchange{}, errors.New("message retention must be positive")
	}
	message = message.WithInferredSource()
	if message.Role != domain.RoleAssistant || message.Source != domain.MessageSourceAssistant || !utf8.ValidString(message.Content) || strings.TrimSpace(message.Content) == "" {
		return port.PreparedAssistantExchange{}, errors.New("external-agent assistant response is invalid")
	}
	digest := sha256.Sum256([]byte(message.Content))
	responseSHA256 := hex.EncodeToString(digest[:])
	intentID, correlationID := activationExchangeIDs(activation.ActivationID)

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("begin prepare external-agent assistant exchange: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := loadActivation(ctx, tx, `WHERE a.job_id = ? AND a.status_revision = ? AND a.kind = ?`, activation.JobID, activation.StatusRevision, activation.Kind)
	if err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("load external-agent activation response state: %w", err)
	}
	if !sameActivationIdentity(&current, activation) {
		return port.PreparedAssistantExchange{}, port.ErrActivationStateConflict
	}
	if current.State == domain.ActivationResponsePrepared || current.State == domain.ActivationCompleted {
		if current.ResponseBody != message.Content || current.ResponseSHA256 != responseSHA256 || current.ExchangeIntentID == "" || current.CorrelationID == "" {
			return port.PreparedAssistantExchange{}, errors.New("external-agent activation response is immutable")
		}
		if err := tx.Commit(); err != nil {
			return port.PreparedAssistantExchange{}, fmt.Errorf("commit existing external-agent assistant exchange: %w", err)
		}
		return port.PreparedAssistantExchange{ID: current.ExchangeIntentID, CorrelationID: current.CorrelationID}, nil
	}
	if current.State != domain.ActivationModelStarted || current.LeaseOwner != activation.LeaseOwner || current.Attempt != activation.Attempt || current.LeaseExpiry.IsZero() || !current.LeaseExpiry.After(now.UTC()) {
		return port.PreparedAssistantExchange{}, port.ErrActivationStateConflict
	}
	if current.ConversationKey != metadata.Key || current.TeamID != metadata.TeamID || current.ConversationKey == "" {
		return port.PreparedAssistantExchange{}, errors.New("external-agent activation conversation metadata conflicts")
	}

	source, err := sourceExchangeTx(ctx, tx, metadata.Key)
	if err != nil && !strings.Contains(err.Error(), "assistant exchange has no persisted user source") {
		return port.PreparedAssistantExchange{}, err
	}
	source = append(source, message)
	payload, err := json.Marshal(sourceMessagesWrapper{Messages: marshalMessages(source)})
	if err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("encode external-agent assistant exchange source: %w", err)
	}
	nowNanos := now.UTC().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_exchange_intents (
			id, conversation_key, team_id, channel_id, channel_kind, root_ts, last_ts,
			assistant_content, assistant_external_ts, assistant_created_at, retain, source_messages, created_at, publish_status, correlation_id, presentation_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, 'prepared', ?, '')
		ON CONFLICT (id) DO NOTHING`,
		intentID, string(metadata.Key), metadata.TeamID, metadata.ChannelID, string(metadata.ChannelKind), metadata.RootTS, metadata.LastTS,
		message.Content, message.CreatedAt.UnixNano(), retain, string(payload), nowNanos, correlationID,
	); err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("prepare external-agent assistant exchange: %w", err)
	}
	intent, err := loadAssistantExchangeIntent(ctx, tx, intentID)
	if err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("load external-agent assistant exchange: %w", err)
	}
	if intent.ConversationKey != metadata.Key || intent.AssistantContent != message.Content || intent.CorrelationID != correlationID || !activationExchangeMemoryIneligible(intent.SourceMessages) {
		return port.PreparedAssistantExchange{}, errors.New("external-agent assistant exchange identity conflicts")
	}
	result, err := tx.ExecContext(ctx, `UPDATE external_agent_job_activations SET
		state = ?, response_body = ?, response_sha256 = ?, exchange_intent_id = ?, correlation_id = ?, last_error_code = '', updated_at = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND state = ? AND lease_owner = ? AND attempt = ? AND lease_expiry > ?`,
		domain.ActivationResponsePrepared, message.Content, responseSHA256, intentID, correlationID, nowNanos,
		activation.JobID, activation.StatusRevision, activation.Kind, domain.ActivationModelStarted, activation.LeaseOwner, activation.Attempt, nowNanos)
	if err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("mark external-agent activation response prepared: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return port.PreparedAssistantExchange{}, port.ErrActivationStateConflict
	}
	if err := tx.Commit(); err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("commit external-agent assistant exchange: %w", err)
	}
	return port.PreparedAssistantExchange{ID: intentID, CorrelationID: correlationID}, nil
}

func activationExchangeIDs(activationID string) (string, string) {
	digest := sha256.Sum256([]byte(activationID))
	encoded := hex.EncodeToString(digest[:])
	return "activation_exchange_" + encoded[:32], "activation_response_" + encoded[:32]
}

func activationExchangeMemoryIneligible(sourceJSON string) bool {
	var wrapper sourceMessagesWrapper
	return json.Unmarshal([]byte(sourceJSON), &wrapper) == nil
}

func sameActivationIdentity(left, right *domain.ExternalAgentJobActivation) bool {
	if left == nil || right == nil {
		return false
	}
	return left.ActivationID == right.ActivationID && left.JobID == right.JobID &&
		left.StatusRevision == right.StatusRevision && left.Kind == right.Kind &&
		left.TerminalStatus == right.TerminalStatus && left.NotificationSHA256 == right.NotificationSHA256 && left.ResultSHA256 == right.ResultSHA256 &&
		left.Actor == right.Actor && left.TeamID == right.TeamID && left.ConversationKey == right.ConversationKey &&
		left.WorkstreamID == right.WorkstreamID && left.TaskID == right.TaskID && left.ExecutionIdentity == right.ExecutionIdentity && left.AdmissionRevision == right.AdmissionRevision &&
		left.OriginalCallID == right.OriginalCallID && left.DeliveryMode == right.DeliveryMode &&
		left.ContentBytes == right.ContentBytes && left.SlackMessageTS == right.SlackMessageTS &&
		left.PublishedAt.Equal(right.PublishedAt)
}

func (s *ExternalAgentJobStore) PrepareActivationResponse(ctx context.Context, activation *domain.ExternalAgentJobActivation, responseBody, responseSHA256, exchangeIntentID, correlationID string, now time.Time) error {
	if err := validateActivationMutation(activation, now, time.Time{}); err != nil {
		return err
	}
	if !utf8.ValidString(responseBody) || strings.TrimSpace(responseBody) == "" || strings.TrimSpace(exchangeIntentID) == "" || strings.TrimSpace(correlationID) == "" {
		return errors.New("external-agent activation response is incomplete")
	}
	digest := sha256.Sum256([]byte(responseBody))
	computedDigest := hex.EncodeToString(digest[:])
	if responseSHA256 == "" {
		responseSHA256 = computedDigest
	}
	if strings.ToLower(responseSHA256) != computedDigest {
		return errors.New("external-agent activation response digest does not match body")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_job_activations SET
		state = ?, response_body = ?, response_sha256 = ?, exchange_intent_id = ?, correlation_id = ?, last_error_code = '', updated_at = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND state = ?
		AND lease_owner = ? AND attempt = ? AND lease_expiry > ?`,
		domain.ActivationResponsePrepared, responseBody, computedDigest, exchangeIntentID, correlationID, now.UTC().UnixNano(),
		activation.JobID, activation.StatusRevision, activation.Kind, domain.ActivationModelStarted,
		activation.LeaseOwner, activation.Attempt, now.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("prepare external-agent activation response: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return port.ErrActivationStateConflict
	}
	return nil
}

func (s *ExternalAgentJobStore) CompleteActivation(ctx context.Context, activation *domain.ExternalAgentJobActivation, responseSlackTS string, now time.Time) error {
	if err := validateActivationMutation(activation, now, time.Time{}); err != nil {
		return err
	}
	if !validSlackTimestamp(responseSlackTS) {
		return errors.New("external-agent activation response timestamp is invalid")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_job_activations SET
		state = ?, response_slack_ts = ?, lease_owner = '', lease_expiry = 0, updated_at = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND state = ?
		AND lease_owner = ? AND attempt = ? AND lease_expiry > ?`,
		domain.ActivationCompleted, responseSlackTS, now.UTC().UnixNano(), activation.JobID, activation.StatusRevision,
		activation.Kind, domain.ActivationResponsePrepared, activation.LeaseOwner, activation.Attempt, now.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("complete external-agent activation: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return port.ErrActivationStateConflict
	}
	return nil
}

func (s *ExternalAgentJobStore) FailActivation(ctx context.Context, activation *domain.ExternalAgentJobActivation, errorCode string, now time.Time) error {
	if err := validateActivationMutation(activation, now, time.Time{}); err != nil {
		return err
	}
	code := safeActivationError(errorCode)
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_job_activations SET
		state = ?, last_error_code = ?, fallback_required = ?, lease_owner = '', lease_expiry = 0, updated_at = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND state IN (?, ?)
		AND lease_owner = ? AND attempt = ? AND lease_expiry > ?`,
		domain.ActivationFailed, code, boolInt(domain.ActivationFallbackRequired(code)), now.UTC().UnixNano(), activation.JobID, activation.StatusRevision,
		activation.Kind, domain.ActivationProcessing, domain.ActivationResponsePrepared, activation.LeaseOwner,
		activation.Attempt, now.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("fail external-agent activation: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return port.ErrActivationStateConflict
	}
	return nil
}

func (s *ExternalAgentJobStore) MarkActivationCompletionUnknown(ctx context.Context, activation *domain.ExternalAgentJobActivation, errorCode string, now time.Time) error {
	if err := validateActivationMutation(activation, now, time.Time{}); err != nil {
		return err
	}
	code := safeActivationError(errorCode)
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_job_activations SET
		state = ?, last_error_code = ?, fallback_required = ?, lease_owner = '', lease_expiry = 0, updated_at = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND state = ?
		AND lease_owner = ? AND attempt = ? AND lease_expiry > ?`,
		domain.ActivationCompletionUnknown, code, boolInt(domain.ActivationFallbackRequired(code)), now.UTC().UnixNano(), activation.JobID, activation.StatusRevision,
		activation.Kind, domain.ActivationModelStarted, activation.LeaseOwner, activation.Attempt, now.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("mark external-agent activation completion unknown: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return port.ErrActivationStateConflict
	}
	return nil
}

// ClaimNextActivationFallback claims one terminal activation whose host-owned
// fallback publication is still pending. Terminal states never carry a lease
// after their CAS, so this path is only reachable for fallback-required rows.
func (s *ExternalAgentJobStore) ClaimNextActivationFallback(ctx context.Context, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJobActivation, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("external-agent activation store is not configured")
	}
	if strings.TrimSpace(owner) == "" || leaseTTL <= 0 {
		return nil, errors.New("activation fallback lease owner and positive TTL are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("begin external-agent activation fallback claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	where := `(a.state IN (?, ?) AND a.fallback_required = 1 AND a.fallback_slack_ts = ''
		AND (a.lease_expiry = 0 OR a.lease_expiry <= ?))`
	query := `SELECT a.job_id, a.status_revision, a.kind
		FROM external_agent_job_activations a
		WHERE ` + where + `
		ORDER BY a.published_at ASC, a.status_revision ASC, a.job_id ASC, a.kind ASC LIMIT 1`
	var jobID, kind string
	var revision int
	if err := tx.QueryRowContext(ctx, query, domain.ActivationFailed, domain.ActivationCompletionUnknown, now.UnixNano()).Scan(&jobID, &revision, &kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("select claimable external-agent activation fallback: %w", err)
	}
	leaseExpiry := now.Add(leaseTTL)
	changed, err := tx.ExecContext(ctx, `UPDATE external_agent_job_activations AS a SET
		lease_owner = ?, lease_expiry = ?, attempt = attempt + 1, updated_at = ?
		WHERE a.job_id = ? AND a.status_revision = ? AND a.kind = ? AND `+where,
		append([]any{owner, leaseExpiry.UnixNano(), now.UnixNano(), jobID, revision, kind},
			domain.ActivationFailed, domain.ActivationCompletionUnknown, now.UnixNano())...)
	if err != nil {
		return nil, fmt.Errorf("claim external-agent activation fallback: %w", err)
	}
	if affected, _ := changed.RowsAffected(); affected != 1 {
		return nil, port.ErrActivationClaimConflict
	}
	activation, err := loadActivation(ctx, tx, `WHERE a.job_id = ? AND a.status_revision = ? AND a.kind = ?`, jobID, revision, kind)
	if err != nil {
		return nil, fmt.Errorf("load claimed external-agent activation fallback: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit external-agent activation fallback claim: %w", err)
	}
	return &activation, nil
}

// CompleteActivationFallback closes one terminal fallback in a single
// transaction: the assistant message is persisted, the durable published
// intent is consumed, and fallback_slack_ts commits by CAS. A retry after any
// crash either observes the committed fallback and returns without side
// effects, or re-runs the whole transaction exactly once.
func (s *ExternalAgentJobStore) CompleteActivationFallback(ctx context.Context, activation *domain.ExternalAgentJobActivation, exchangeIntentID, slackTS string, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("external-agent activation store is not configured")
	}
	if activation == nil || activation.JobID == "" || activation.Kind == "" || activation.LeaseOwner == "" || activation.Attempt <= 0 {
		return errors.New("external-agent activation fallback identity is required")
	}
	if strings.TrimSpace(exchangeIntentID) == "" || !validSlackTimestamp(slackTS) {
		return errors.New("external-agent activation fallback exchange identity is invalid")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin complete external-agent activation fallback: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := loadActivation(ctx, tx, `WHERE a.job_id = ? AND a.status_revision = ? AND a.kind = ?`, activation.JobID, activation.StatusRevision, activation.Kind)
	if err != nil {
		return fmt.Errorf("load external-agent activation fallback state: %w", err)
	}
	if !sameActivationIdentity(&current, activation) {
		return port.ErrActivationStateConflict
	}
	if current.FallbackSlackTS != "" {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit completed external-agent activation fallback: %w", err)
		}
		return nil // A prior completion committed before its caller observed success.
	}
	if !current.FallbackRequired || !domain.ActivationFallbackRequired(current.LastErrorCode) {
		return errors.New("external-agent activation fallback is not required")
	}
	if current.State != domain.ActivationFailed && current.State != domain.ActivationCompletionUnknown {
		return port.ErrActivationStateConflict
	}
	if current.LeaseOwner != activation.LeaseOwner || current.Attempt != activation.Attempt || current.LeaseExpiry.IsZero() || !current.LeaseExpiry.After(now.UTC()) {
		return port.ErrActivationStateConflict
	}

	intent, err := loadAssistantExchangeIntent(ctx, tx, exchangeIntentID)
	if err != nil {
		return fmt.Errorf("load external-agent activation fallback intent: %w", err)
	}
	if intent.PublishStatus != "published" || intent.AssistantExternalTS != slackTS {
		return errors.New("cannot complete an unpublished external-agent activation fallback")
	}
	if intent.ConversationKey != current.ConversationKey {
		return errors.New("external-agent activation fallback intent conversation conflicts")
	}

	if err := appendMessageTx(ctx, tx, intent.metadata(), intent.assistantMessage(), intent.Retain); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_exchange_intents WHERE id = ?`, exchangeIntentID); err != nil {
		return fmt.Errorf("delete completed external-agent activation fallback intent: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE external_agent_job_activations SET
		fallback_slack_ts = ?, lease_owner = '', lease_expiry = 0, updated_at = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND state IN (?, ?)
		AND fallback_required = 1 AND fallback_slack_ts = ''
		AND lease_owner = ? AND attempt = ?`,
		slackTS, now.UTC().UnixNano(), activation.JobID, activation.StatusRevision, activation.Kind,
		domain.ActivationFailed, domain.ActivationCompletionUnknown, activation.LeaseOwner, activation.Attempt)
	if err != nil {
		return fmt.Errorf("complete external-agent activation fallback: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		committed, err := loadActivation(ctx, tx, `WHERE a.job_id = ? AND a.status_revision = ? AND a.kind = ?`, activation.JobID, activation.StatusRevision, activation.Kind)
		if err == nil && committed.FallbackSlackTS != "" {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit completed external-agent activation fallback: %w", err)
			}
			return nil
		}
		return port.ErrActivationStateConflict
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit complete external-agent activation fallback: %w", err)
	}
	return nil
}

// activationFallbackIntentPrefix is the deterministic ID prefix of staged
// activation fallback exchanges. The generic assistant-exchange reconciler
// must never consume these intents: their completion is owned exclusively by
// the CompleteActivationFallback transaction.
const activationFallbackIntentPrefix = "activation_fallback_exchange_"

func fallbackExchangeIDs(activationID string) (string, string) {
	digest := sha256.Sum256([]byte(activationID))
	encoded := hex.EncodeToString(digest[:])
	return activationFallbackIntentPrefix + encoded[:32], "activation_fallback_response_" + encoded[:32]
}

// PrepareActivationFallbackExchange stages the host-owned fallback assistant
// exchange durably before Slack is contacted. The intent and correlation IDs
// are deterministic per activation, so a retry returns the same durable
// exchange instead of creating or publishing another fallback. The activation
// stays in its terminal state; only the fallback intent is staged.
func (s *ExternalAgentJobStore) PrepareActivationFallbackExchange(
	ctx context.Context,
	activation *domain.ExternalAgentJobActivation,
	metadata domain.ConversationMetadata,
	message domain.Message,
	retain int,
	now time.Time,
) (port.PreparedAssistantExchange, error) {
	if s == nil || s.db == nil {
		return port.PreparedAssistantExchange{}, errors.New("external-agent activation store is not configured")
	}
	if activation == nil || activation.JobID == "" || activation.Kind == "" || activation.LeaseOwner == "" || activation.Attempt <= 0 {
		return port.PreparedAssistantExchange{}, errors.New("external-agent activation fallback identity is required")
	}
	if now.IsZero() {
		return port.PreparedAssistantExchange{}, errors.New("external-agent activation fallback time is required")
	}
	if retain <= 0 {
		return port.PreparedAssistantExchange{}, errors.New("message retention must be positive")
	}
	message = message.WithInferredSource()
	if message.Role != domain.RoleAssistant || message.Source != domain.MessageSourceAssistant || !utf8.ValidString(message.Content) || strings.TrimSpace(message.Content) == "" {
		return port.PreparedAssistantExchange{}, errors.New("external-agent assistant fallback is invalid")
	}
	intentID, correlationID := fallbackExchangeIDs(activation.ActivationID)

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("begin prepare external-agent assistant fallback: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := loadActivation(ctx, tx, `WHERE a.job_id = ? AND a.status_revision = ? AND a.kind = ?`, activation.JobID, activation.StatusRevision, activation.Kind)
	if err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("load external-agent activation fallback state: %w", err)
	}
	if !sameActivationIdentity(&current, activation) {
		return port.PreparedAssistantExchange{}, port.ErrActivationStateConflict
	}
	if !current.FallbackRequired || current.FallbackSlackTS != "" || !domain.ActivationFallbackRequired(current.LastErrorCode) {
		return port.PreparedAssistantExchange{}, errors.New("external-agent activation fallback is not required")
	}
	if current.State != domain.ActivationFailed && current.State != domain.ActivationCompletionUnknown {
		return port.PreparedAssistantExchange{}, port.ErrActivationStateConflict
	}
	if current.LeaseOwner != activation.LeaseOwner || current.Attempt != activation.Attempt || current.LeaseExpiry.IsZero() || !current.LeaseExpiry.After(now.UTC()) {
		return port.PreparedAssistantExchange{}, port.ErrActivationStateConflict
	}
	if current.ConversationKey != metadata.Key || current.TeamID != metadata.TeamID || current.ConversationKey == "" {
		return port.PreparedAssistantExchange{}, errors.New("external-agent activation fallback conversation metadata conflicts")
	}

	source, err := sourceExchangeTx(ctx, tx, metadata.Key)
	if err != nil && !strings.Contains(err.Error(), "assistant exchange has no persisted user source") {
		return port.PreparedAssistantExchange{}, err
	}
	source = append(source, message)
	payload, err := json.Marshal(sourceMessagesWrapper{Messages: marshalMessages(source)})
	if err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("encode external-agent assistant fallback source: %w", err)
	}
	nowNanos := now.UTC().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_exchange_intents (
			id, conversation_key, team_id, channel_id, channel_kind, root_ts, last_ts,
			assistant_content, assistant_external_ts, assistant_created_at, retain, source_messages, created_at, publish_status, correlation_id, presentation_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, 'prepared', ?, '')
		ON CONFLICT (id) DO NOTHING`,
		intentID, string(metadata.Key), metadata.TeamID, metadata.ChannelID, string(metadata.ChannelKind), metadata.RootTS, metadata.LastTS,
		message.Content, message.CreatedAt.UnixNano(), retain, string(payload), nowNanos, correlationID,
	); err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("prepare external-agent assistant fallback: %w", err)
	}
	intent, err := loadAssistantExchangeIntent(ctx, tx, intentID)
	if err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("load external-agent assistant fallback: %w", err)
	}
	if intent.ConversationKey != metadata.Key || intent.AssistantContent != message.Content || intent.CorrelationID != correlationID || !activationExchangeMemoryIneligible(intent.SourceMessages) {
		return port.PreparedAssistantExchange{}, errors.New("external-agent assistant fallback identity conflicts")
	}
	if err := tx.Commit(); err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("commit external-agent assistant fallback: %w", err)
	}
	return port.PreparedAssistantExchange{ID: intentID, CorrelationID: correlationID}, nil
}

func (s *ExternalAgentJobStore) RenewActivationLease(ctx context.Context, activation *domain.ExternalAgentJobActivation, now time.Time, leaseTTL time.Duration) error {
	if activation == nil || activation.JobID == "" || activation.LeaseOwner == "" || activation.Attempt <= 0 || leaseTTL <= 0 {
		return errors.New("invalid external-agent activation lease renewal")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE external_agent_job_activations SET lease_expiry = ?, updated_at = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ? AND state IN (?, ?, ?)
		AND lease_owner = ? AND attempt = ? AND lease_expiry > ?`,
		now.UTC().Add(leaseTTL).UnixNano(), now.UTC().UnixNano(), activation.JobID, activation.StatusRevision,
		activation.Kind, domain.ActivationProcessing, domain.ActivationModelStarted, domain.ActivationResponsePrepared,
		activation.LeaseOwner, activation.Attempt, now.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("renew external-agent activation lease: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return port.ErrActivationStateConflict
	}
	return nil
}

func validateActivationMutation(activation *domain.ExternalAgentJobActivation, now, nextAttemptAt time.Time) error {
	if activation == nil || activation.JobID == "" || activation.Kind == "" || activation.LeaseOwner == "" || activation.Attempt <= 0 {
		return errors.New("external-agent activation mutation identity is required")
	}
	if now.IsZero() {
		return errors.New("external-agent activation mutation time is required")
	}
	if !nextAttemptAt.IsZero() && nextAttemptAt.Before(now) {
		return errors.New("external-agent activation retry must be scheduled in the future")
	}
	return nil
}

func safeActivationError(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return "activation_retryable"
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return "activation_retryable"
		}
	}
	return value
}

func validSlackTimestamp(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	dot := strings.IndexByte(value, '.')
	if dot <= 0 || dot == len(value)-1 || strings.IndexByte(value[dot+1:], '.') >= 0 {
		return false
	}
	for _, r := range value {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func loadActivation(ctx context.Context, queryer queryRower, where string, args ...any) (domain.ExternalAgentJobActivation, error) {
	var activation domain.ExternalAgentJobActivation
	var terminalStatus, deliveryMode, state, conversation string
	var publishedAt, leaseExpiry, nextAttemptAt, createdAt, updatedAt int64
	var fallbackRequired int
	row := queryer.QueryRowContext(ctx, `SELECT `+activationColumns+` FROM external_agent_job_activations a `+where, args...)
	err := row.Scan(
		&activation.JobID, &activation.StatusRevision, &activation.Kind, &activation.ActivationID, &terminalStatus,
		&activation.NotificationSHA256, &activation.ResultSHA256, &activation.Actor, &activation.TeamID, &conversation, &activation.WorkstreamID, &activation.TaskID,
		&activation.ExecutionIdentity, &activation.AdmissionRevision, &activation.OriginalCallID,
		&deliveryMode, &activation.ContentBytes, &activation.SlackMessageTS, &publishedAt, &state, &activation.Attempt,
		&activation.LeaseOwner, &leaseExpiry, &nextAttemptAt, &activation.LastErrorCode, &activation.ResponseBody,
		&activation.ResponseSHA256, &activation.ExchangeIntentID, &activation.CorrelationID, &activation.ResponseSlackTS,
		&fallbackRequired, &activation.FallbackSlackTS,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return domain.ExternalAgentJobActivation{}, err
	}
	activation.TerminalStatus = domain.ExternalAgentJobStatus(terminalStatus)
	activation.ConversationKey = domain.ConversationKey(conversation)
	activation.DeliveryMode = domain.JobResultDeliveryMode(deliveryMode)
	activation.PublishedAt, activation.LeaseExpiry = fromUnix(publishedAt), fromUnix(leaseExpiry)
	activation.NextAttemptAt, activation.CreatedAt = fromUnix(nextAttemptAt), fromUnix(createdAt)
	activation.UpdatedAt = fromUnix(updatedAt)
	activation.State = domain.ExternalAgentJobActivationState(state)
	activation.FallbackRequired = fallbackRequired == 1
	return activation, nil
}
