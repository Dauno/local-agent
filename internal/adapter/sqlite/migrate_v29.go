package sqlite

import (
	"context"
	"database/sql"
)

// migrateV29 adds message provenance, immutable publication snapshots, and
// the internal activation outbox. Existing rows deliberately retain empty
// notification snapshots and therefore cannot manufacture activations.
func migrateV29(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 29, []string{
		`ALTER TABLE messages ADD COLUMN source TEXT NOT NULL DEFAULT 'human'`,
		`UPDATE messages SET source = 'assistant' WHERE role = 'assistant'`,
		`CREATE TRIGGER messages_source_insert
			BEFORE INSERT ON messages
			WHEN NOT (
				(NEW.role = 'user' AND NEW.source IN ('human', 'job_completion')) OR
				(NEW.role = 'assistant' AND NEW.source = 'assistant')
			)
			BEGIN SELECT RAISE(ABORT, 'message role and source are incompatible'); END`,
		`CREATE TRIGGER messages_source_update
			BEFORE UPDATE OF role, source ON messages
			WHEN NOT (
				(NEW.role = 'user' AND NEW.source IN ('human', 'job_completion')) OR
				(NEW.role = 'assistant' AND NEW.source = 'assistant')
			)
			BEGIN SELECT RAISE(ABORT, 'message role and source are incompatible'); END`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN terminal_status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN published_at INTEGER NOT NULL DEFAULT 0`,
		`CREATE TRIGGER external_agent_job_notifications_terminal_snapshot_insert
			BEFORE INSERT ON external_agent_job_notifications
			WHEN NEW.terminal_status != '' AND NEW.terminal_status NOT IN ('completed', 'failed', 'cancelled', 'completion_unknown', 'abandoned')
			BEGIN SELECT RAISE(ABORT, 'invalid external-agent terminal status snapshot'); END`,
		`CREATE TRIGGER external_agent_job_notifications_terminal_snapshot_update
			BEFORE UPDATE ON external_agent_job_notifications
			WHEN NEW.terminal_status != OLD.terminal_status OR NEW.published_at < OLD.published_at OR
				(OLD.published_at > 0 AND NEW.published_at != OLD.published_at)
			BEGIN SELECT RAISE(ABORT, 'external-agent notification snapshot is immutable'); END`,
		`CREATE TABLE external_agent_job_activations (
			job_id TEXT NOT NULL,
			status_revision INTEGER NOT NULL,
			kind TEXT NOT NULL,
			activation_id TEXT NOT NULL UNIQUE,
			terminal_status TEXT NOT NULL,
			notification_sha256 TEXT NOT NULL,
			actor TEXT NOT NULL,
			team_id TEXT NOT NULL,
			conversation_key TEXT NOT NULL,
			original_call_id TEXT NOT NULL,
			delivery_mode TEXT NOT NULL,
			content_bytes INTEGER NOT NULL DEFAULT 0,
			slack_message_ts TEXT NOT NULL,
			published_at INTEGER NOT NULL,
			state TEXT NOT NULL DEFAULT 'pending',
			attempt INTEGER NOT NULL DEFAULT 0,
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_expiry INTEGER NOT NULL DEFAULT 0,
			next_attempt_at INTEGER NOT NULL,
			last_error_code TEXT NOT NULL DEFAULT '',
			response_body TEXT NOT NULL DEFAULT '',
			response_sha256 TEXT NOT NULL DEFAULT '',
			exchange_intent_id TEXT NOT NULL DEFAULT '',
			correlation_id TEXT NOT NULL DEFAULT '',
			response_slack_ts TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (job_id, status_revision, kind),
			FOREIGN KEY (job_id) REFERENCES external_agent_jobs(job_id) ON DELETE CASCADE,
			CHECK (status_revision >= 0),
			CHECK (length(activation_id) > 0),
			CHECK (length(kind) > 0),
			CHECK (terminal_status IN ('completed', 'failed', 'cancelled', 'completion_unknown', 'abandoned')),
			CHECK (length(notification_sha256) = 64),
			CHECK (delivery_mode IN ('markdown', 'file')),
			CHECK (content_bytes >= 0),
			CHECK (length(slack_message_ts) > 0),
			CHECK (published_at > 0),
			CHECK (state IN ('pending', 'processing', 'model_started', 'response_prepared', 'completed', 'completion_unknown', 'failed')),
			CHECK (attempt >= 0),
			CHECK (response_sha256 = '' OR length(response_sha256) = 64),
			CHECK (state NOT IN ('response_prepared', 'completed') OR
				(length(response_body) > 0 AND length(response_sha256) = 64 AND length(exchange_intent_id) > 0 AND length(correlation_id) > 0)),
			CHECK (state != 'completed' OR length(response_slack_ts) > 0)
		) WITHOUT ROWID`,
		`CREATE INDEX external_agent_job_activations_claimable
			ON external_agent_job_activations (state, next_attempt_at, lease_expiry, published_at, status_revision, job_id)`,
		`CREATE INDEX external_agent_job_activations_conversation_order
			ON external_agent_job_activations (conversation_key, published_at, status_revision, job_id, kind)`,
		`CREATE TRIGGER external_agent_job_activations_state_transition
			BEFORE UPDATE OF state ON external_agent_job_activations
			WHEN NOT (
				NEW.state = OLD.state OR
				(OLD.state = 'pending' AND NEW.state = 'processing') OR
				(OLD.state = 'processing' AND NEW.state IN ('pending', 'model_started', 'failed')) OR
				(OLD.state = 'model_started' AND NEW.state IN ('response_prepared', 'completion_unknown')) OR
				(OLD.state = 'response_prepared' AND NEW.state IN ('completed', 'failed'))
			)
			BEGIN SELECT RAISE(ABORT, 'invalid external-agent activation state transition'); END`,
		`CREATE TRIGGER external_agent_job_activations_identity_immutable
			BEFORE UPDATE ON external_agent_job_activations
			WHEN NEW.job_id != OLD.job_id OR NEW.status_revision != OLD.status_revision OR NEW.kind != OLD.kind OR
				NEW.activation_id != OLD.activation_id OR NEW.terminal_status != OLD.terminal_status OR
				NEW.notification_sha256 != OLD.notification_sha256 OR NEW.actor != OLD.actor OR NEW.team_id != OLD.team_id OR
				NEW.conversation_key != OLD.conversation_key OR NEW.original_call_id != OLD.original_call_id OR
				NEW.delivery_mode != OLD.delivery_mode OR NEW.content_bytes != OLD.content_bytes OR
				NEW.slack_message_ts != OLD.slack_message_ts OR NEW.published_at != OLD.published_at OR
				NEW.created_at != OLD.created_at
			BEGIN SELECT RAISE(ABORT, 'external-agent activation identity is immutable'); END`,
		`CREATE TRIGGER external_agent_job_activations_response_immutable
			BEFORE UPDATE ON external_agent_job_activations
			WHEN (OLD.response_body != '' OR OLD.response_sha256 != '' OR OLD.exchange_intent_id != '' OR OLD.correlation_id != '' OR OLD.response_slack_ts != '') AND
				(NEW.response_body != OLD.response_body OR NEW.response_sha256 != OLD.response_sha256 OR
				 NEW.exchange_intent_id != OLD.exchange_intent_id OR NEW.correlation_id != OLD.correlation_id OR
				 (OLD.response_slack_ts != '' AND NEW.response_slack_ts != OLD.response_slack_ts))
			BEGIN SELECT RAISE(ABORT, 'external-agent activation response is immutable'); END`,
	})
}
