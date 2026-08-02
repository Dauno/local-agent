package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.StandardExperienceStore = (*Store)(nil)
var _ port.OnboardingDeliveryStore = (*Store)(nil)

const onboardingClaimLease = 2 * time.Minute

func (s *Store) CreateProgress(ctx context.Context, operation domain.ProgressOperation) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO standard_progress_operations
			(id, conversation_key, channel_id, thread_ts, message_ts, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, operation.ID, string(operation.ConversationKey), operation.ChannelID,
		operation.ThreadTS, operation.MessageTS, string(operation.State), operation.CreatedAt.UnixNano(), operation.UpdatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("create standard progress operation: %w", err)
	}
	return nil
}

func (s *Store) MarkProgressPublished(ctx context.Context, operationID, messageTS string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE standard_progress_operations SET message_ts = ?, updated_at = ? WHERE id = ? AND message_ts = ''`, messageTS, time.Now().UTC().UnixNano(), operationID)
	if err != nil {
		return fmt.Errorf("mark standard progress published: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect standard progress publication: %w", err)
	}
	if changed == 0 {
		var existing string
		if err := s.db.QueryRowContext(ctx, `SELECT message_ts FROM standard_progress_operations WHERE id = ?`, operationID).Scan(&existing); err != nil {
			return fmt.Errorf("read standard progress publication: %w", err)
		}
		if existing != messageTS {
			return errors.New("standard progress publication conflicts with persisted message")
		}
	}
	return nil
}

func (s *Store) SetProgressState(ctx context.Context, operationID string, state domain.ProgressState, updatedAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE standard_progress_operations SET state = ?, updated_at = ? WHERE id = ?`, string(state), updatedAt.UnixNano(), operationID); err != nil {
		return fmt.Errorf("update standard progress state: %w", err)
	}
	return nil
}

func (s *Store) ListRecoverableProgress(ctx context.Context) ([]domain.ProgressOperation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, conversation_key, channel_id, thread_ts, message_ts, state, created_at, updated_at
		FROM standard_progress_operations
		WHERE state NOT IN ('cleared', 'failed', 'interrupted')
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list recoverable standard progress: %w", err)
	}
	defer rows.Close()
	var operations []domain.ProgressOperation
	for rows.Next() {
		operation, err := scanProgress(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable standard progress: %w", err)
	}
	return operations, nil
}

func (s *Store) FindWaitingProgress(ctx context.Context, key domain.ConversationKey) (*domain.ProgressOperation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, conversation_key, channel_id, thread_ts, message_ts, state, created_at, updated_at
		FROM standard_progress_operations
		WHERE conversation_key = ? AND state = 'waiting_confirmation'
		ORDER BY updated_at DESC LIMIT 1`, string(key))
	operation, err := scanProgress(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

type progressScanner interface {
	Scan(...any) error
}

func scanProgress(scanner progressScanner) (domain.ProgressOperation, error) {
	var operation domain.ProgressOperation
	var state string
	var createdAt, updatedAt int64
	if err := scanner.Scan(&operation.ID, &operation.ConversationKey, &operation.ChannelID, &operation.ThreadTS, &operation.MessageTS, &state, &createdAt, &updatedAt); err != nil {
		return domain.ProgressOperation{}, err
	}
	operation.State = domain.ProgressState(state)
	operation.CreatedAt = time.Unix(0, createdAt).UTC()
	operation.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return operation, nil
}

func (s *Store) ClaimSuggestedPrompts(ctx context.Context, teamID, userID string, key domain.ConversationKey, createdAt time.Time) (string, bool, error) {
	deliveryID := "standard_prompts:" + teamID + ":" + userID
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO standard_prompt_deliveries
			(id, team_id, user_id, conversation_key, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (team_id, user_id) DO NOTHING`, deliveryID, teamID, userID, string(key), createdAt.UnixNano(), createdAt.UnixNano())
	if err != nil {
		return "", false, fmt.Errorf("claim standard suggested prompts: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("inspect standard suggested prompt claim: %w", err)
	}
	return deliveryID, changed == 1, nil
}

func (s *Store) MarkSuggestedPromptsPublished(ctx context.Context, deliveryID, messageTS string, updatedAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE standard_prompt_deliveries SET status = 'published', message_ts = ?, updated_at = ? WHERE id = ? AND status = 'prepared'`, messageTS, updatedAt.UnixNano(), deliveryID); err != nil {
		return fmt.Errorf("mark standard suggested prompts published: %w", err)
	}
	return nil
}

func (s *Store) ClaimOnboarding(ctx context.Context, teamID, userID string, key domain.ConversationKey, createdAt time.Time) (port.OnboardingDeliveryClaim, port.OnboardingDeliveryState, error) {
	if strings.TrimSpace(teamID) == "" || strings.TrimSpace(userID) == "" || strings.TrimSpace(string(key)) == "" {
		return port.OnboardingDeliveryClaim{}, port.OnboardingUnavailable, errors.New("onboarding claim identity is required")
	}
	claimToken, err := newOnboardingClaimToken()
	if err != nil {
		return port.OnboardingDeliveryClaim{}, port.OnboardingUnavailable, err
	}
	deliveryID := "standard_onboarding:" + teamID + ":" + userID
	now := createdAt.UTC()
	leaseUntil := now.Add(onboardingClaimLease).UnixNano()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO standard_prompt_deliveries
			(id, team_id, user_id, conversation_key, delivery_kind, claim_token, lease_until, attempt, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'onboarding', ?, ?, 1, ?, ?)
		ON CONFLICT (team_id, user_id) DO NOTHING`, deliveryID, teamID, userID, string(key), claimToken, leaseUntil, now.UnixNano(), now.UnixNano())
	if err != nil {
		return port.OnboardingDeliveryClaim{}, port.OnboardingUnavailable, fmt.Errorf("claim onboarding delivery: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return port.OnboardingDeliveryClaim{}, port.OnboardingUnavailable, fmt.Errorf("inspect onboarding delivery claim: %w", err)
	}
	if changed == 1 {
		return port.OnboardingDeliveryClaim{DeliveryID: deliveryID, ClaimToken: claimToken, ConversationKey: key}, port.OnboardingClaimed, nil
	}

	var existingID, existingKey, existingToken, kind, status string
	var existingLease int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT id, conversation_key, delivery_kind, status, claim_token, lease_until
		FROM standard_prompt_deliveries WHERE team_id = ? AND user_id = ?`, teamID, userID).
		Scan(&existingID, &existingKey, &kind, &status, &existingToken, &existingLease); err != nil {
		return port.OnboardingDeliveryClaim{}, port.OnboardingUnavailable, fmt.Errorf("read onboarding delivery claim: %w", err)
	}
	existing := port.OnboardingDeliveryClaim{DeliveryID: existingID, ClaimToken: existingToken, ConversationKey: domain.ConversationKey(existingKey)}
	if kind != "onboarding" {
		return existing, port.OnboardingUnavailable, nil
	}
	if status == "published" {
		return existing, port.OnboardingAlreadyPublished, nil
	}
	if status != "prepared" {
		return existing, port.OnboardingUnavailable, nil
	}
	if existingLease > now.UnixNano() {
		return existing, port.OnboardingInFlight, nil
	}

	result, err = s.db.ExecContext(ctx, `
		UPDATE standard_prompt_deliveries
		SET claim_token = ?, lease_until = ?, attempt = attempt + 1, updated_at = ?
		WHERE id = ? AND delivery_kind = 'onboarding' AND status = 'prepared' AND lease_until <= ?`, claimToken, leaseUntil, now.UnixNano(), existingID, now.UnixNano())
	if err != nil {
		return existing, port.OnboardingUnavailable, fmt.Errorf("renew onboarding delivery claim: %w", err)
	}
	changed, err = result.RowsAffected()
	if err != nil {
		return existing, port.OnboardingUnavailable, fmt.Errorf("inspect onboarding delivery renewal: %w", err)
	}
	if changed == 1 {
		return port.OnboardingDeliveryClaim{DeliveryID: existingID, ClaimToken: claimToken, ConversationKey: domain.ConversationKey(existingKey)}, port.OnboardingClaimed, nil
	}
	return existing, port.OnboardingInFlight, nil
}

func (s *Store) MarkOnboardingPublished(ctx context.Context, claim port.OnboardingDeliveryClaim, messageTS string, updatedAt time.Time) error {
	if strings.TrimSpace(claim.DeliveryID) == "" || strings.TrimSpace(claim.ClaimToken) == "" || strings.TrimSpace(messageTS) == "" {
		return errors.New("onboarding publication identity is required")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE standard_prompt_deliveries
		SET status = 'published', message_ts = ?, claim_token = '', lease_until = 0, updated_at = ?
		WHERE id = ? AND delivery_kind = 'onboarding' AND status = 'prepared' AND claim_token = ?`, messageTS, updatedAt.UTC().UnixNano(), claim.DeliveryID, claim.ClaimToken)
	if err != nil {
		return fmt.Errorf("mark onboarding published: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect onboarding publication: %w", err)
	}
	if changed == 1 {
		return nil
	}
	var status, existingTS string
	if err := s.db.QueryRowContext(ctx, `SELECT status, message_ts FROM standard_prompt_deliveries WHERE id = ?`, claim.DeliveryID).Scan(&status, &existingTS); err != nil {
		return fmt.Errorf("read onboarding publication: %w", err)
	}
	if status == "published" && existingTS == messageTS {
		return nil
	}
	return errors.New("onboarding publication claim is stale or conflicts with persisted message")
}

func newOnboardingClaimToken() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate onboarding claim token: %w", err)
	}
	return hex.EncodeToString(data), nil
}

func (s *Store) PrepareIncremental(ctx context.Context, operation domain.IncrementalOperation) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO standard_incremental_operations
			(id, conversation_key, channel_id, thread_ts, message_ts, renderer_version, latest_sequence, prefix_digest, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, operation.ID, string(operation.ConversationKey), operation.ChannelID,
		operation.ThreadTS, operation.MessageTS, operation.RendererVersion, operation.Sequence, operation.PrefixDigest,
		string(operation.Status), operation.CreatedAt.UnixNano(), operation.UpdatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("prepare standard incremental operation: %w", err)
	}
	return nil
}

func (s *Store) MarkIncrementalCreated(ctx context.Context, operationID, messageTS string, updatedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE standard_incremental_operations
		SET message_ts = ?, status = 'message_created', updated_at = ?
		WHERE id = ? AND status = 'prepared' AND message_ts = ''`, messageTS, updatedAt.UnixNano(), operationID)
	if err != nil {
		return fmt.Errorf("mark standard incremental message created: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect standard incremental creation: %w", err)
	}
	if changed == 0 {
		var existing string
		if err := s.db.QueryRowContext(ctx, `SELECT message_ts FROM standard_incremental_operations WHERE id = ?`, operationID).Scan(&existing); err != nil {
			return fmt.Errorf("read standard incremental creation: %w", err)
		}
		if existing != messageTS {
			return errors.New("standard incremental message conflicts with persisted identity")
		}
	}
	return nil
}

func (s *Store) AdvanceIncremental(ctx context.Context, operationID string, status domain.IncrementalStatus, sequence int, prefixDigest string, updatedAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE standard_incremental_operations
		SET status = ?, latest_sequence = ?, prefix_digest = ?, updated_at = ?
		WHERE id = ?`, string(status), sequence, prefixDigest, updatedAt.UnixNano(), operationID); err != nil {
		return fmt.Errorf("advance standard incremental operation: %w", err)
	}
	return nil
}

func (s *Store) ListUnfinishedIncremental(ctx context.Context) ([]domain.IncrementalOperation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, conversation_key, channel_id, thread_ts, message_ts, renderer_version,
			latest_sequence, prefix_digest, status, created_at, updated_at
		FROM standard_incremental_operations
		WHERE status NOT IN ('finalized', 'interrupted')
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list unfinished standard incremental operations: %w", err)
	}
	defer rows.Close()
	var operations []domain.IncrementalOperation
	for rows.Next() {
		var operation domain.IncrementalOperation
		var status string
		var createdAt, updatedAt int64
		if err := rows.Scan(&operation.ID, &operation.ConversationKey, &operation.ChannelID, &operation.ThreadTS, &operation.MessageTS,
			&operation.RendererVersion, &operation.Sequence, &operation.PrefixDigest, &status, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan unfinished standard incremental operation: %w", err)
		}
		operation.Status = domain.IncrementalStatus(status)
		operation.CreatedAt = time.Unix(0, createdAt).UTC()
		operation.UpdatedAt = time.Unix(0, updatedAt).UTC()
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unfinished standard incremental operations: %w", err)
	}
	return operations, nil
}
