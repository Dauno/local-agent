package sqlite

import (
	"context"
	"database/sql"
)

// migrateV24 adds the recoverable_result store for durable reference retention.
func migrateV24(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 24, []string{
		`CREATE TABLE recoverable_results (
			ref TEXT PRIMARY KEY,
			actor TEXT NOT NULL,
			conversation_key TEXT NOT NULL,
			kind TEXT NOT NULL,
			storage_locator TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			code_points INTEGER NOT NULL,
			sha256 TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			cleanup_claim TEXT NOT NULL DEFAULT '',
			cleanup_version INTEGER NOT NULL DEFAULT 0,
			cleanup_claimed_at INTEGER NOT NULL DEFAULT 0,
			CHECK (length(ref) > 0),
			CHECK (length(actor) > 0),
			CHECK (length(conversation_key) > 0),
			CHECK (length(kind) > 0),
			CHECK (size_bytes > 0),
			CHECK (length(sha256) == 64),
			CHECK (cleanup_version >= 0),
			CHECK (cleanup_claimed_at >= 0),
			CHECK (expires_at > created_at)
		) WITHOUT ROWID`,
		`CREATE INDEX recoverable_results_by_expiry ON recoverable_results (expires_at, cleanup_claim)`,
		`CREATE INDEX recoverable_results_by_conversation ON recoverable_results (conversation_key, actor)`,
	})
}
