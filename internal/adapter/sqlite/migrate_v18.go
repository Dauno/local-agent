package sqlite

import (
	"context"
	"database/sql"
)

func migrateV18(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 18, []string{
		`CREATE TABLE external_agent_jobs (
			job_id TEXT PRIMARY KEY,
			mode TEXT NOT NULL,
			provider TEXT NOT NULL,
			profile TEXT NOT NULL,
			primary_project TEXT NOT NULL,
			additional_projects TEXT NOT NULL,
			registry_revision TEXT NOT NULL,
			task TEXT NOT NULL,
			request_sha256 TEXT NOT NULL,
			wrapper_call_id TEXT NOT NULL,
			original_call_id TEXT NOT NULL UNIQUE,
			actor TEXT NOT NULL,
			slack_team_id TEXT NOT NULL,
			conversation_key TEXT NOT NULL,
			status TEXT NOT NULL,
			attempt INTEGER NOT NULL DEFAULT 0,
			acp_session_id TEXT NOT NULL DEFAULT '',
			side_effects_possible INTEGER NOT NULL DEFAULT 0,
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_expiry INTEGER NOT NULL DEFAULT 0,
			heartbeat_at INTEGER NOT NULL DEFAULT 0,
			timeout_at INTEGER NOT NULL,
			result_summary TEXT NOT NULL DEFAULT '',
			result_artifact TEXT NOT NULL DEFAULT '',
			result_sha256 TEXT NOT NULL DEFAULT '',
			result_bytes INTEGER NOT NULL DEFAULT 0,
			error_code TEXT NOT NULL DEFAULT '',
			status_revision INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			started_at INTEGER NOT NULL DEFAULT 0,
			finished_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL,
			CHECK (mode IN ('foreground', 'detached')),
			CHECK (status IN ('queued', 'running', 'cancel_requested', 'interrupted_safe', 'completion_unknown', 'reconciling', 'completed', 'failed', 'cancelled', 'abandoned')),
			CHECK (side_effects_possible IN (0, 1)),
			CHECK (attempt >= 0),
			CHECK (result_bytes >= 0),
			CHECK (status_revision >= 0)
		) WITHOUT ROWID`,
		`CREATE INDEX external_agent_jobs_claimable ON external_agent_jobs (status, lease_expiry, timeout_at)`,
		`CREATE INDEX external_agent_jobs_actor_conversation ON external_agent_jobs (actor, slack_team_id, conversation_key, updated_at DESC)`,
		`CREATE TABLE external_agent_job_events (
			job_id TEXT NOT NULL,
			status_revision INTEGER NOT NULL,
			event_kind TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (job_id, status_revision, event_kind),
			FOREIGN KEY (job_id) REFERENCES external_agent_jobs(job_id) ON DELETE CASCADE
		) WITHOUT ROWID`,
	})
}
