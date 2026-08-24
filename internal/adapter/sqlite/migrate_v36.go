package sqlite

import (
	"context"
	"database/sql"
)

// migrateV36 adds durable context epoch identities without copying frame
// content or backfilling existing ADK sessions.
func migrateV36(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 36, []string{
		`CREATE TABLE context_epochs (
			app_name TEXT NOT NULL,
			user_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			epoch_id TEXT NOT NULL,
			epoch_number INTEGER NOT NULL,
			covered_through_ordinal INTEGER NOT NULL,
			workstream_revision INTEGER NOT NULL,
			summary_identity TEXT NOT NULL DEFAULT '',
			knowledge_identities JSON NOT NULL DEFAULT '[]',
			result_identities JSON NOT NULL DEFAULT '[]',
			compiler_version TEXT NOT NULL,
			counter_version TEXT NOT NULL,
			source_digest TEXT NOT NULL,
			frame_tokens INTEGER NOT NULL,
			frame_code_points INTEGER NOT NULL,
			selected_source_count INTEGER NOT NULL,
			omitted_source_count INTEGER NOT NULL,
			reason TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (app_name, user_id, session_id, epoch_number),
			UNIQUE (epoch_id),
			FOREIGN KEY (app_name, user_id, session_id)
				REFERENCES adk_sessions(app_name, user_id, session_id) ON DELETE CASCADE,
			CHECK (length(app_name) > 0 AND length(user_id) > 0 AND length(session_id) > 0),
			CHECK (length(epoch_id) > 0 AND epoch_number > 0),
			CHECK (covered_through_ordinal >= -1 AND workstream_revision >= 0),
			CHECK (length(compiler_version) > 0 AND length(counter_version) > 0),
			CHECK (length(source_digest) = 64 AND source_digest NOT GLOB '*[^0-9a-f]*'),
			CHECK (frame_tokens >= 0 AND frame_code_points >= 0 AND selected_source_count >= 0 AND omitted_source_count >= 0),
			CHECK (length(reason) > 0 AND created_at > 0)
		) WITHOUT ROWID`,
		`CREATE INDEX context_epochs_by_session_number
			ON context_epochs (app_name, user_id, session_id, epoch_number)`,
	})
}
