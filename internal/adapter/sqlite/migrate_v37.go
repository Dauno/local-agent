package sqlite

import (
	"context"
	"database/sql"
)

// migrateV37 adds the immutable completion binding to jobs and its delivery
// snapshots and extends the immutable workstream journal with the start_task
// execution-admission action. Historical rows remain unbound and cannot
// manufacture a new root activation after this migration.
func migrateV37(ctx context.Context, tx *sql.Tx) error {
	if err := migrateV37Bindings(ctx, tx); err != nil {
		return err
	}
	return migrateV37Journal(ctx, tx)
}

func migrateV37Bindings(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 37, []string{
		`ALTER TABLE external_agent_jobs ADD COLUMN workstream_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE external_agent_jobs ADD COLUMN task_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE external_agent_jobs ADD COLUMN execution_identity TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE external_agent_jobs ADD COLUMN admission_revision INTEGER NOT NULL DEFAULT 0 CHECK (admission_revision >= 0)`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN workstream_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN task_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN execution_identity TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN admission_revision INTEGER NOT NULL DEFAULT 0 CHECK (admission_revision >= 0)`,
		`ALTER TABLE external_agent_job_activations ADD COLUMN workstream_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE external_agent_job_activations ADD COLUMN task_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE external_agent_job_activations ADD COLUMN execution_identity TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE external_agent_job_activations ADD COLUMN admission_revision INTEGER NOT NULL DEFAULT 0 CHECK (admission_revision >= 0)`,
		`ALTER TABLE external_agent_job_activations ADD COLUMN result_sha256 TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE external_agent_job_activations ADD COLUMN fallback_required INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE external_agent_job_activations ADD COLUMN fallback_slack_ts TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX external_agent_jobs_by_completion_binding
			ON external_agent_jobs (workstream_id, task_id, execution_identity, admission_revision)`,
		`CREATE INDEX external_agent_job_activations_by_workstream
			ON external_agent_job_activations (workstream_id, task_id, execution_identity, admission_revision)`,
		`CREATE TRIGGER external_agent_jobs_completion_binding_immutable
			BEFORE UPDATE ON external_agent_jobs
			WHEN NEW.workstream_id != OLD.workstream_id OR NEW.task_id != OLD.task_id OR
				NEW.execution_identity != OLD.execution_identity OR NEW.admission_revision != OLD.admission_revision
			BEGIN SELECT RAISE(ABORT, 'external-agent completion binding is immutable'); END`,
		`CREATE TRIGGER external_agent_job_notifications_completion_binding_immutable
			BEFORE UPDATE ON external_agent_job_notifications
			WHEN NEW.workstream_id != OLD.workstream_id OR NEW.task_id != OLD.task_id OR
				NEW.execution_identity != OLD.execution_identity OR NEW.admission_revision != OLD.admission_revision OR
				NEW.result_sha256 != OLD.result_sha256
			BEGIN SELECT RAISE(ABORT, 'external-agent notification completion binding is immutable'); END`,
		`CREATE TRIGGER external_agent_job_activations_completion_binding_immutable
			BEFORE UPDATE ON external_agent_job_activations
			WHEN NEW.workstream_id != OLD.workstream_id OR NEW.task_id != OLD.task_id OR
				NEW.execution_identity != OLD.execution_identity OR NEW.admission_revision != OLD.admission_revision OR
				NEW.result_sha256 != OLD.result_sha256
			BEGIN SELECT RAISE(ABORT, 'external-agent activation completion binding is immutable'); END`,
	})
}

// migrateV37Journal rebuilds only the immutable workstream journal table so
// the start_task execution-admission action can join the action constraint.
// Every existing journal row, index, and trigger is preserved.
func migrateV37Journal(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 37, []string{
		`DROP TRIGGER workstream_transitions_immutable_update`,
		`DROP TRIGGER workstream_transitions_immutable_delete`,
		`DROP INDEX workstream_transitions_by_workstream`,
		`DROP INDEX workstream_transitions_by_source`,
		`CREATE TABLE workstream_transitions_v37 (
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
			CHECK (action IN ('create_workstream', 'activate_workstream', 'pause_workstream', 'resume_workstream', 'cancel_workstream', 'complete_workstream', 'propose_task', 'reject_task', 'start_task', 'revise_plan', 'record_constraint', 'propose_decision', 'request_human_decision', 'approve_decision', 'reject_decision', 'resolve_question', 'block_workstream', 'unblock_workstream', 'link_completed_result'))
		) WITHOUT ROWID`,
		`INSERT INTO workstream_transitions_v37
			SELECT workstream_id, from_revision, to_revision, source, source_id, actor, action,
			payload_digest, payload_json, state_digest, state_json, committed_at FROM workstream_transitions`,
		`DROP TABLE workstream_transitions`,
		`ALTER TABLE workstream_transitions_v37 RENAME TO workstream_transitions`,
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
