package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.BuilderLauncherDeliveryStore = (*Store)(nil)

func (s *Store) ClaimBuilderLauncher(
	ctx context.Context,
	deliveryID string,
	key domain.ConversationKey,
	createdAt time.Time,
) (port.BuilderLauncherDeliveryClaim, port.BuilderLauncherDeliveryState, error) {
	if strings.TrimSpace(deliveryID) == "" || strings.TrimSpace(string(key)) == "" {
		return port.BuilderLauncherDeliveryClaim{}, "", errors.New("builder launcher claim identity is required")
	}
	claimToken, err := newOnboardingClaimToken()
	if err != nil {
		return port.BuilderLauncherDeliveryClaim{}, "", err
	}
	now := createdAt.UTC()
	leaseUntil := now.Add(onboardingClaimLease).UnixNano()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO builder_launcher_deliveries
			(id, conversation_key, claim_token, lease_until, attempt, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (id) DO NOTHING`, deliveryID, string(key), claimToken, leaseUntil, now.UnixNano(), now.UnixNano())
	if err != nil {
		return port.BuilderLauncherDeliveryClaim{}, "", fmt.Errorf("claim builder launcher delivery: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return port.BuilderLauncherDeliveryClaim{}, "", fmt.Errorf("inspect builder launcher claim: %w", err)
	}
	if changed == 1 {
		return port.BuilderLauncherDeliveryClaim{DeliveryID: deliveryID, ClaimToken: claimToken}, port.BuilderLauncherClaimed, nil
	}

	var status string
	var existingLease int64
	if err := s.db.QueryRowContext(ctx, `SELECT status, lease_until FROM builder_launcher_deliveries WHERE id = ?`, deliveryID).Scan(&status, &existingLease); err != nil {
		return port.BuilderLauncherDeliveryClaim{}, "", fmt.Errorf("read builder launcher claim: %w", err)
	}
	claim := port.BuilderLauncherDeliveryClaim{DeliveryID: deliveryID}
	if status == "published" {
		return claim, port.BuilderLauncherAlreadyPublished, nil
	}
	if existingLease > now.UnixNano() {
		return claim, port.BuilderLauncherInFlight, nil
	}
	result, err = s.db.ExecContext(ctx, `
		UPDATE builder_launcher_deliveries
		SET claim_token = ?, lease_until = ?, attempt = attempt + 1, updated_at = ?
		WHERE id = ? AND status = 'prepared' AND lease_until <= ?`, claimToken, leaseUntil, now.UnixNano(), deliveryID, now.UnixNano())
	if err != nil {
		return claim, "", fmt.Errorf("renew builder launcher claim: %w", err)
	}
	changed, err = result.RowsAffected()
	if err != nil {
		return claim, "", fmt.Errorf("inspect builder launcher renewal: %w", err)
	}
	if changed == 1 {
		return port.BuilderLauncherDeliveryClaim{DeliveryID: deliveryID, ClaimToken: claimToken}, port.BuilderLauncherClaimed, nil
	}
	return claim, port.BuilderLauncherInFlight, nil
}

func (s *Store) MarkBuilderLauncherPublished(ctx context.Context, claim port.BuilderLauncherDeliveryClaim, messageTS string, updatedAt time.Time) error {
	if strings.TrimSpace(claim.DeliveryID) == "" || strings.TrimSpace(claim.ClaimToken) == "" || strings.TrimSpace(messageTS) == "" {
		return errors.New("builder launcher publication identity is required")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE builder_launcher_deliveries
		SET status = 'published', message_ts = ?, claim_token = '', lease_until = 0, updated_at = ?
		WHERE id = ? AND status = 'prepared' AND claim_token = ?`, messageTS, updatedAt.UTC().UnixNano(), claim.DeliveryID, claim.ClaimToken)
	if err != nil {
		return fmt.Errorf("mark builder launcher published: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect builder launcher publication: %w", err)
	}
	if changed == 1 {
		return nil
	}
	var status, existingTS string
	if err := s.db.QueryRowContext(ctx, `SELECT status, message_ts FROM builder_launcher_deliveries WHERE id = ?`, claim.DeliveryID).Scan(&status, &existingTS); err != nil {
		return fmt.Errorf("read builder launcher publication: %w", err)
	}
	if status == "published" && existingTS == messageTS {
		return nil
	}
	return errors.New("builder launcher publication claim is stale or conflicts with persisted message")
}
