package sqlite

import (
	"context"
	"database/sql"
)

// migrateV30 adds the content-free ACP live progress projection. Existing rows
// receive no projection row; they simply report an empty phase until a live
// recorder writes one. The projection intentionally has no free-form payload
// column: raw ACP content can never enter this table.
func migrateV30(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 30, []string{
		`CREATE TABLE external_agent_job_progress (
			job_id TEXT PRIMARY KEY,
			attempt INTEGER NOT NULL DEFAULT 0,
			phase TEXT NOT NULL DEFAULT 'starting',
			last_event_kind TEXT NOT NULL DEFAULT '',
			last_transport_activity_at INTEGER NOT NULL DEFAULT 0,
			last_session_update_at INTEGER NOT NULL DEFAULT 0,
			last_meaningful_progress_at INTEGER NOT NULL DEFAULT 0,
			prompt_started_at INTEGER NOT NULL DEFAULT 0,
			active_tool_count INTEGER NOT NULL DEFAULT 0,
			last_tool_call_id TEXT NOT NULL DEFAULT '',
			last_tool_kind TEXT NOT NULL DEFAULT '',
			last_tool_status TEXT NOT NULL DEFAULT '',
			tool_overflow INTEGER NOT NULL DEFAULT 0,
			pending_permission INTEGER NOT NULL DEFAULT 0,
			stop_reason TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			FOREIGN KEY (job_id) REFERENCES external_agent_jobs(job_id) ON DELETE CASCADE,
			CHECK (attempt >= 0),
			CHECK (phase IN ('starting', 'session_ready', 'agent_processing', 'planning',
				'tool_pending', 'waiting_permission', 'tool_running', 'responding',
				'completed', 'cancelled', 'failed')),
			CHECK (last_event_kind IN ('', 'process_started', 'initialize_response', 'session_new',
				'prompt_sent', 'plan', 'tool_call', 'tool_call_update', 'permission_requested',
				'permission_responded', 'message_chunk', 'thought_chunk', 'usage_update',
				'config_option_update', 'unknown_notification', 'transport_activity',
				'prompt_response', 'process_failed')),
			CHECK (active_tool_count BETWEEN 0 AND 16),
			CHECK (last_tool_kind IN ('', 'read', 'edit', 'delete', 'move', 'search',
				'execute', 'think', 'fetch', 'other')),
			CHECK (last_tool_status IN ('', 'pending', 'running', 'terminal')),
			CHECK (tool_overflow IN (0, 1)),
			CHECK (pending_permission IN (0, 1)),
			CHECK (stop_reason IN ('', 'end_turn', 'cancelled', 'max_tokens', 'refusal')),
			CHECK (length(last_tool_call_id) <= 256),
			CHECK (last_transport_activity_at >= 0),
			CHECK (last_session_update_at >= 0),
			CHECK (last_meaningful_progress_at >= 0),
			CHECK (prompt_started_at >= 0)
		) WITHOUT ROWID`,
	})
}
