package sqlite

import (
	"context"
	"database/sql"
)

// migrateV43 adds operator-visible transcript metadata. Historical rows stay
// empty because no trusted descriptor can be reconstructed from job state.
func migrateV43(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 43, []string{
		`ALTER TABLE external_agent_jobs ADD COLUMN transcript_path TEXT NOT NULL DEFAULT ''`,
	})
}
