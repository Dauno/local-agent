package sqlite

import (
	"context"
	"database/sql"
)

// v23 adds the Agent Builder v2 draft contract. The table rewrite is required
// because v20 declared model NOT NULL, while ACP drafts must store model NULL.
func migrateV23(ctx context.Context, tx *sql.Tx) error {
	legacyColumns, err := tableHasColumn(ctx, tx, "agent_drafts", "kind")
	if err != nil {
		return err
	}
	selectV2Fields := `
				'llm', 'foreground', 0, '', status, created_at, expires_at`
	if legacyColumns {
		selectV2Fields = `
				CASE WHEN kind = '' THEN 'llm' ELSE kind END,
				CASE WHEN execution_mode = '' THEN 'foreground' ELSE execution_mode END,
				timeout_seconds, canonical_yaml, status, created_at, expires_at`
	}
	statements := []string{
		`CREATE TABLE agent_drafts_v2 (
			draft_id TEXT PRIMARY KEY,
			team_id TEXT NOT NULL,
			actor_id TEXT NOT NULL,
			conversation_key TEXT NOT NULL,
			name TEXT NOT NULL,
			description TEXT NOT NULL,
			instruction TEXT NOT NULL,
			model TEXT,
			definition_hash TEXT NOT NULL,
			catalog_revision INTEGER NOT NULL,
			kind TEXT NOT NULL DEFAULT 'llm',
			execution_mode TEXT NOT NULL DEFAULT 'foreground',
			timeout_seconds INTEGER NOT NULL DEFAULT 0,
			canonical_yaml TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'draft',
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			CHECK (length(draft_id) > 0),
			CHECK (length(team_id) > 0),
			CHECK (length(actor_id) > 0),
			CHECK (length(conversation_key) > 0),
			CHECK (length(name) > 0),
			CHECK (catalog_revision >= 0),
			CHECK (kind IN ('llm', 'acp')),
			CHECK (execution_mode IN ('', 'foreground', 'durable_job')),
			CHECK (timeout_seconds >= 0 AND timeout_seconds <= 86400),
			CHECK (status IN ('draft', 'previewed', 'install_requested', 'installed', 'cancelled', 'expired', 'failed')),
			CHECK (expires_at > created_at)
		) WITHOUT ROWID`,
		`INSERT INTO agent_drafts_v2
			SELECT draft_id, team_id, actor_id, conversation_key, name, description,
				instruction, model, definition_hash, catalog_revision,
				` + selectV2Fields + `
			FROM agent_drafts`,
		`DROP TABLE agent_drafts`,
		`ALTER TABLE agent_drafts_v2 RENAME TO agent_drafts`,
		`CREATE INDEX IF NOT EXISTS agent_drafts_by_expiry
			ON agent_drafts (status, expires_at)`,
		`CREATE INDEX IF NOT EXISTS agent_drafts_by_actor_conversation
			ON agent_drafts (team_id, actor_id, conversation_key, created_at DESC)`,
	}
	return execMigration(ctx, tx, 23, statements)
}

func tableHasColumn(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}
