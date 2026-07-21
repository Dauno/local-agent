package sqlite

import (
	"context"
	"database/sql"
)

func migrateV12(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 12, []string{
		`ALTER TABLE memory_exchange_intents ADD COLUMN presentation_json TEXT NOT NULL DEFAULT ''`,
	})
}
