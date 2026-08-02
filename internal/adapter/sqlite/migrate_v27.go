package sqlite

import (
	"context"
	"database/sql"
)

// migrateV27 adds leased, typed claims to the existing first-experience table.
// Legacy suggested-prompt rows retain their meaning through the default kind.
func migrateV27(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 27, []string{
		`ALTER TABLE standard_prompt_deliveries ADD COLUMN delivery_kind TEXT NOT NULL DEFAULT 'suggested_prompts'`,
		`ALTER TABLE standard_prompt_deliveries ADD COLUMN claim_token TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE standard_prompt_deliveries ADD COLUMN lease_until INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE standard_prompt_deliveries ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX standard_prompt_delivery_recovery ON standard_prompt_deliveries (delivery_kind, status, lease_until)`,
	})
}
