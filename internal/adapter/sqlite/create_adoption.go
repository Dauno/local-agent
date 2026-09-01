package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

const createAdoptionPostflightDetail = "created at v45 by init; no postflight queries ran"

// recordAdoptionAtCreation writes Create's second, separate transaction on
// the pool OpenExisting already opened. Transaction 1 (the v1-v41 migration
// chain) has already committed by the time this runs; a crash between the
// two leaves exactly Recovery Table row 3 (Adoption), which db upgrade
// recovers without special-cased code. The file was never backed up, so the
// durable record says NotRequired rather than fabricating backup keys; the
// quarantine marker records the correct vacuous disposition for a database
// with zero rows.
func recordAdoptionAtCreation(ctx context.Context, db *sql.DB) error {
	now := time.Now().UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin adoption-at-creation transaction: %w", err)
	}
	insert := `INSERT INTO runtime_state (state_key, state_value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (state_key) DO NOTHING`
	values := []struct {
		key   string
		value string
	}{
		{rollout.KeyBaseline, rollout.FormatBaseline(rollout.IdentityBaseline{})},
		{rollout.KeyCutoff, fmt.Sprintf("%d", now.UnixNano())},
		{rollout.KeyPostflightStatus, string(rollout.PostflightPassed)},
		{rollout.KeyPostflightDetail, createAdoptionPostflightDetail},
		{rollout.KeyBackupNotRequiredAt, now.Format(time.RFC3339)},
		{rollout.KeyLegacyQuarantineAt, now.Format(time.RFC3339)},
	}
	for _, entry := range values {
		if _, err := tx.ExecContext(ctx, insert, entry.key, entry.value, now.UnixNano()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("write runtime_state key %s: %w", entry.key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit adoption-at-creation transaction: %w", err)
	}
	return nil
}
