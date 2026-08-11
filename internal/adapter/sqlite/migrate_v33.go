package sqlite

import (
	"context"
	"database/sql"
)

// migrateV33 adds normalized durable workstream state. Existing conversation,
// protocol, continuity, result, and memory rows are not backfilled or altered.
func migrateV33(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 33, []string{
		`CREATE TABLE workstreams (
			workstream_id TEXT PRIMARY KEY,
			conversation_key TEXT NOT NULL,
			owner_actor TEXT NOT NULL,
			project TEXT NOT NULL,
			status TEXT NOT NULL,
			revision INTEGER NOT NULL DEFAULT 0,
			objective TEXT NOT NULL,
			current_phase TEXT NOT NULL DEFAULT '',
			continuation_of TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(workstream_id) > 0),
			CHECK (length(conversation_key) > 0),
			CHECK (length(owner_actor) > 0),
			CHECK (length(project) > 0),
			CHECK (length(objective) > 0),
			CHECK (status IN ('proposed', 'active', 'paused', 'blocked', 'completed', 'cancelled')),
			CHECK (revision >= 0)
		) WITHOUT ROWID`,
		`CREATE UNIQUE INDEX workstreams_one_non_terminal_conversation
			ON workstreams (conversation_key)
			WHERE status IN ('proposed', 'active', 'paused', 'blocked')`,
		`CREATE INDEX workstreams_by_conversation
			ON workstreams (conversation_key, updated_at DESC)`,

		`CREATE TABLE workstream_constraints (
			workstream_id TEXT NOT NULL,
			constraint_id TEXT NOT NULL,
			text TEXT NOT NULL,
			source_id TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (workstream_id, constraint_id),
			FOREIGN KEY (workstream_id) REFERENCES workstreams(workstream_id) ON DELETE CASCADE,
			CHECK (length(constraint_id) > 0),
			CHECK (length(text) > 0)
		) WITHOUT ROWID`,

		`CREATE TABLE workstream_decisions (
			workstream_id TEXT NOT NULL,
			decision_id TEXT NOT NULL,
			status TEXT NOT NULL,
			proposal TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT '',
			rationale TEXT NOT NULL DEFAULT '',
			effective_revision INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (workstream_id, decision_id),
			FOREIGN KEY (workstream_id) REFERENCES workstreams(workstream_id) ON DELETE CASCADE,
			CHECK (length(decision_id) > 0),
			CHECK (status IN ('proposed', 'approved', 'rejected', 'superseded')),
			CHECK (length(proposal) > 0),
			CHECK (effective_revision >= 0)
		) WITHOUT ROWID`,

		`CREATE TABLE workstream_tasks (
			workstream_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			project TEXT NOT NULL,
			description TEXT NOT NULL,
			status TEXT NOT NULL,
			result_identity TEXT NOT NULL DEFAULT '',
			confirmation_identity TEXT NOT NULL DEFAULT '',
			confirmation_status TEXT NOT NULL DEFAULT 'not_required',
			execution_identity TEXT NOT NULL DEFAULT '',
			integrated INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (workstream_id, task_id),
			FOREIGN KEY (workstream_id) REFERENCES workstreams(workstream_id) ON DELETE CASCADE,
			CHECK (length(task_id) > 0),
			CHECK (length(project) > 0),
			CHECK (length(description) > 0),
			CHECK (status IN ('proposed', 'awaiting_confirmation', 'queued', 'running', 'cancellation_requested', 'rejected', 'cancelled', 'completed', 'failed', 'completion_unknown')),
			CHECK (confirmation_status IN ('not_required', 'pending', 'approved', 'rejected')),
			CHECK (integrated IN (0, 1)),
			CHECK (status != 'awaiting_confirmation' OR (length(confirmation_identity) > 0 AND confirmation_status = 'pending')),
			CHECK (status NOT IN ('queued', 'running') OR length(confirmation_identity) = 0 OR confirmation_status = 'approved'),
			CHECK (status != 'running' OR length(execution_identity) > 0)
		) WITHOUT ROWID`,
		`CREATE INDEX workstream_tasks_by_status
			ON workstream_tasks (workstream_id, status, task_id)`,

		`CREATE TABLE workstream_task_inputs (
			workstream_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			input_identity TEXT NOT NULL,
			PRIMARY KEY (workstream_id, task_id, input_identity),
			FOREIGN KEY (workstream_id, task_id) REFERENCES workstream_tasks(workstream_id, task_id) ON DELETE CASCADE,
			CHECK (length(input_identity) > 0)
		) WITHOUT ROWID`,

		`CREATE TABLE workstream_task_dependencies (
			workstream_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			dependency_id TEXT NOT NULL,
			PRIMARY KEY (workstream_id, task_id, dependency_id),
			FOREIGN KEY (workstream_id, task_id) REFERENCES workstream_tasks(workstream_id, task_id) ON DELETE CASCADE,
			FOREIGN KEY (workstream_id, dependency_id) REFERENCES workstream_tasks(workstream_id, task_id) ON DELETE CASCADE,
			CHECK (length(dependency_id) > 0),
			CHECK (task_id != dependency_id)
		) WITHOUT ROWID`,

		`CREATE TABLE workstream_questions (
			workstream_id TEXT NOT NULL,
			question_id TEXT NOT NULL,
			text TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'open',
			resolution TEXT NOT NULL DEFAULT '',
			source_id TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (workstream_id, question_id),
			FOREIGN KEY (workstream_id) REFERENCES workstreams(workstream_id) ON DELETE CASCADE,
			CHECK (length(question_id) > 0),
			CHECK (length(text) > 0),
			CHECK (status IN ('open', 'resolved'))
		) WITHOUT ROWID`,

		`CREATE TABLE workstream_result_links (
			workstream_id TEXT NOT NULL,
			result_link_id TEXT NOT NULL,
			task_id TEXT NOT NULL DEFAULT '',
			result_identity TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (workstream_id, result_link_id),
			FOREIGN KEY (workstream_id) REFERENCES workstreams(workstream_id) ON DELETE CASCADE,
			CHECK (length(result_link_id) > 0),
			CHECK (length(result_identity) > 0)
		) WITHOUT ROWID`,

		`CREATE TABLE workstream_transitions (
			workstream_id TEXT NOT NULL,
			from_revision INTEGER NOT NULL,
			to_revision INTEGER NOT NULL,
			source TEXT NOT NULL,
			source_id TEXT NOT NULL,
			actor TEXT NOT NULL,
			action TEXT NOT NULL,
			payload_digest TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			state_digest TEXT NOT NULL,
			state_json TEXT NOT NULL,
			committed_at INTEGER NOT NULL,
			PRIMARY KEY (workstream_id, to_revision),
			FOREIGN KEY (workstream_id) REFERENCES workstreams(workstream_id) ON DELETE RESTRICT,
			CHECK (from_revision >= 0),
			CHECK (to_revision = from_revision + 1 OR (action = 'create_workstream' AND from_revision = 0 AND to_revision = 0)),
			CHECK (source IN ('human', 'root', 'system')),
			CHECK (length(source_id) > 0),
			CHECK (length(actor) > 0),
			CHECK (action IN ('create_workstream', 'activate_workstream', 'pause_workstream', 'resume_workstream', 'cancel_workstream', 'complete_workstream', 'propose_task', 'reject_task', 'revise_plan', 'record_constraint', 'propose_decision', 'request_human_decision', 'approve_decision', 'reject_decision', 'resolve_question', 'block_workstream', 'unblock_workstream'))
		) WITHOUT ROWID`,
		`CREATE INDEX workstream_transitions_by_workstream
			ON workstream_transitions (workstream_id, to_revision DESC)`,
		`CREATE UNIQUE INDEX workstream_transitions_by_source
			ON workstream_transitions (workstream_id, source_id)`,
		`CREATE TRIGGER workstream_transitions_immutable_update
			BEFORE UPDATE ON workstream_transitions
			BEGIN SELECT RAISE(ABORT, 'workstream transition journal is immutable'); END`,
		`CREATE TRIGGER workstream_transitions_immutable_delete
			BEFORE DELETE ON workstream_transitions
			BEGIN SELECT RAISE(ABORT, 'workstream transition journal is immutable'); END`,
	})
}
