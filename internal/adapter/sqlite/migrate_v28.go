package sqlite

import (
	"context"
	"database/sql"
)

// migrateV28 adds durable idempotency for the compatibility builder launcher.
func migrateV28(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 28, []string{
		`CREATE TABLE builder_launcher_deliveries (
			id TEXT PRIMARY KEY,
			conversation_key TEXT NOT NULL,
			message_ts TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'prepared',
			claim_token TEXT NOT NULL DEFAULT '',
			lease_until INTEGER NOT NULL DEFAULT 0,
			attempt INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(id) > 0),
			CHECK (length(conversation_key) > 0),
			CHECK (status IN ('prepared', 'published')),
			CHECK (attempt >= 0)
		) WITHOUT ROWID`,
		`CREATE INDEX builder_launcher_delivery_recovery ON builder_launcher_deliveries (status, lease_until)`,
	})
}
