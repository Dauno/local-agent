package sqlite

import (
	"context"
	"database/sql"
)

func migrateV19(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 19, []string{
		`CREATE TABLE external_agent_job_notifications (
			job_id TEXT NOT NULL,
			status_revision INTEGER NOT NULL,
			kind TEXT NOT NULL,
			canonical_markdown TEXT NOT NULL,
			content_sha256 TEXT NOT NULL,
			renderer_version TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			thread_ts TEXT NOT NULL DEFAULT '',
			publish_state TEXT NOT NULL DEFAULT 'pending',
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_expiry INTEGER NOT NULL DEFAULT 0,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at INTEGER NOT NULL,
			recovered_slack_ts TEXT NOT NULL DEFAULT '',
			last_error_code TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (job_id, status_revision, kind),
			FOREIGN KEY (job_id) REFERENCES external_agent_jobs(job_id) ON DELETE CASCADE,
			CHECK (status_revision >= 0),
			CHECK (publish_state IN ('pending', 'publishing', 'published', 'unknown')),
			CHECK (attempts >= 0),
			CHECK (length(canonical_markdown) > 0),
			CHECK (length(content_sha256) > 0),
			CHECK (length(renderer_version) > 0),
			CHECK (length(channel_id) > 0)
		) WITHOUT ROWID`,
		`CREATE INDEX external_agent_job_notifications_claimable
			ON external_agent_job_notifications (publish_state, next_attempt_at, lease_expiry)`,
	})
}
