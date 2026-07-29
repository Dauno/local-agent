package sqlite

import (
	"context"
	"database/sql"
)

func migrateV21(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 21, []string{
		`CREATE TABLE adk_context_summaries (
			session_identity TEXT PRIMARY KEY,
			covered_ordinal_start INTEGER NOT NULL,
			covered_ordinal_end INTEGER NOT NULL,
			source_digest TEXT NOT NULL,
			sanitized_text TEXT NOT NULL,
			prompt_version TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(session_identity) > 0),
			CHECK (covered_ordinal_start > 0),
			CHECK (covered_ordinal_end >= covered_ordinal_start),
			CHECK (length(source_digest) > 0),
			CHECK (length(sanitized_text) > 0),
			CHECK (length(prompt_version) > 0)
		) WITHOUT ROWID`,
		`CREATE TABLE adk_context_summary_jobs (
			session_identity TEXT NOT NULL,
			target_ordinal INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (session_identity, target_ordinal),
			CHECK (length(session_identity) > 0),
			CHECK (target_ordinal > 0),
			CHECK (status IN ('pending', 'running', 'done', 'failed')),
			CHECK (attempts >= 0)
		) WITHOUT ROWID`,
		`CREATE INDEX adk_context_summary_jobs_by_status
			ON adk_context_summary_jobs (status, next_attempt)`,
	})
}
