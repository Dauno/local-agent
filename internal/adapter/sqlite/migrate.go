package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var current int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > SchemaVersion {
		return &FutureSchemaError{Found: current, Supported: SchemaVersion}
	}

	for version := current + 1; version <= SchemaVersion; version++ {
		switch version {
		case 1:
			if err := migrateV1(ctx, tx); err != nil {
				return err
			}
		default:
			return fmt.Errorf("no SQLite migration registered for version %d", version)
		}
	}

	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}

func migrateV1(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE dedupe_records (
			dedupe_key TEXT PRIMARY KEY,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			CHECK (length(dedupe_key) > 0),
			CHECK (expires_at > created_at)
		) WITHOUT ROWID`,
		`CREATE INDEX dedupe_records_by_expiry
			ON dedupe_records (expires_at)`,
		`CREATE TABLE conversations (
			conversation_key TEXT PRIMARY KEY,
			team_id TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			channel_kind TEXT NOT NULL,
			root_ts TEXT NOT NULL,
			last_ts TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(conversation_key) > 0),
			CHECK (length(team_id) > 0),
			CHECK (length(channel_id) > 0),
			CHECK (length(last_ts) > 0),
			CHECK (channel_kind IN ('dm', 'channel', 'group')),
			CHECK (
				(channel_kind = 'dm' AND root_ts = '') OR
				(channel_kind IN ('channel', 'group') AND length(root_ts) > 0)
			)
		) WITHOUT ROWID`,
		`CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conversation_key TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			user_id TEXT NOT NULL,
			external_ts TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			FOREIGN KEY (conversation_key) REFERENCES conversations(conversation_key) ON DELETE CASCADE,
			CHECK (role IN ('user', 'assistant'))
		)`,
		`CREATE INDEX messages_by_conversation_and_time
			ON messages (conversation_key, created_at DESC, id DESC)`,
	}

	for index, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply SQLite schema v1 statement %d: %w", index+1, err)
		}
	}
	return nil
}
