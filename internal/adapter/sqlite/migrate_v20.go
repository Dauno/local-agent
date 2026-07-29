package sqlite

import (
	"context"
	"database/sql"
)

func migrateV20(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 20, []string{
		`CREATE TABLE agent_drafts (
			draft_id TEXT PRIMARY KEY,
			team_id TEXT NOT NULL,
			actor_id TEXT NOT NULL,
			conversation_key TEXT NOT NULL,
			name TEXT NOT NULL,
			description TEXT NOT NULL,
			instruction TEXT NOT NULL,
			model TEXT NOT NULL,
			definition_hash TEXT NOT NULL,
			catalog_revision INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'draft',
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			CHECK (length(draft_id) > 0),
			CHECK (length(team_id) > 0),
			CHECK (length(actor_id) > 0),
			CHECK (length(conversation_key) > 0),
			CHECK (length(name) > 0),
			CHECK (catalog_revision >= 0),
			CHECK (status IN ('draft', 'previewed', 'install_requested', 'installed', 'cancelled', 'expired', 'failed')),
			CHECK (expires_at > created_at)
		) WITHOUT ROWID`,
		`CREATE INDEX agent_drafts_by_expiry
			ON agent_drafts (status, expires_at)`,
		`CREATE INDEX agent_drafts_by_actor_conversation
			ON agent_drafts (team_id, actor_id, conversation_key, created_at DESC)`,
	})
}
