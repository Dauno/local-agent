package sqlite

import (
	"context"
	"database/sql"
)

// migrateV25 adds the continuity_capsules table for durable session
// continuity tracking.
func migrateV25(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 25, []string{
		`CREATE TABLE continuity_capsules (
			session_id TEXT PRIMARY KEY,
			revision INTEGER NOT NULL DEFAULT 0,
			capsule_json TEXT NOT NULL,
			source_digest TEXT NOT NULL DEFAULT '',
			covered_through INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(session_id) > 0),
			CHECK (length(capsule_json) > 0),
			CHECK (revision >= 0)
		) WITHOUT ROWID`,
	})
}
