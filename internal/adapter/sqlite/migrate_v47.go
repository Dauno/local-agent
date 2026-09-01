package sqlite

import (
	"context"
	"database/sql"
)

// migrateV47 makes the workstream task the only durable owner of the job
// association. Jobs remain generic; the journal also records system settlement
// of an associated task.
func migrateV47(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 47, []string{
		`DROP TRIGGER external_agent_jobs_completion_policy_immutable`,
		`DROP TRIGGER external_agent_jobs_completion_binding_immutable`,
		`DROP INDEX external_agent_jobs_by_completion_binding`,
		`UPDATE external_agent_job_notifications SET root_activation_required = 0
			WHERE job_id IN (SELECT job_id FROM external_agent_jobs WHERE completion_policy = 'workstream_only')`,
		`UPDATE workstream_tasks SET job_id = (
			SELECT j.job_id FROM external_agent_jobs j
			WHERE j.workstream_id = workstream_tasks.workstream_id
				AND j.task_id = workstream_tasks.task_id
				AND j.execution_identity = workstream_tasks.execution_identity
			ORDER BY j.created_at ASC
			LIMIT 1
		)
		WHERE length(job_id) = 0 AND EXISTS (
			SELECT 1 FROM external_agent_jobs j
			WHERE j.workstream_id = workstream_tasks.workstream_id
				AND j.task_id = workstream_tasks.task_id
				AND j.execution_identity = workstream_tasks.execution_identity
		)`,
		`UPDATE external_agent_jobs SET completion_policy = 'delivery_only' WHERE completion_policy = 'workstream_only'`,
		`ALTER TABLE external_agent_jobs DROP COLUMN workstream_id`,
		`ALTER TABLE external_agent_jobs DROP COLUMN task_id`,
		`ALTER TABLE external_agent_jobs DROP COLUMN execution_identity`,
		`ALTER TABLE external_agent_jobs DROP COLUMN admission_revision`,
		`CREATE TRIGGER external_agent_jobs_completion_policy_immutable
			BEFORE UPDATE OF completion_policy ON external_agent_jobs
			WHEN NEW.completion_policy != OLD.completion_policy
			BEGIN SELECT RAISE(ABORT, 'external-agent completion policy is immutable'); END`,
		`CREATE TRIGGER workstream_tasks_job_exists_insert
			BEFORE INSERT ON workstream_tasks
			WHEN length(NEW.job_id) > 0
				AND NOT EXISTS (SELECT 1 FROM external_agent_jobs WHERE job_id = NEW.job_id)
			BEGIN SELECT RAISE(ABORT, 'workstream task job does not exist'); END`,
		`CREATE TRIGGER external_agent_jobs_referenced_by_workstream_task
			BEFORE DELETE ON external_agent_jobs
			WHEN EXISTS (SELECT 1 FROM workstream_tasks WHERE job_id = OLD.job_id)
			BEGIN SELECT RAISE(ABORT, 'external-agent job is referenced by a workstream task'); END`,
		`CREATE TRIGGER workstream_result_links_task_exists
			BEFORE INSERT ON workstream_result_links
			WHEN length(NEW.task_id) > 0
				AND NOT EXISTS (SELECT 1 FROM workstream_tasks
					WHERE workstream_id = NEW.workstream_id AND task_id = NEW.task_id)
			BEGIN SELECT RAISE(ABORT, 'workstream result link task does not exist'); END`,
		`CREATE TRIGGER workstream_result_links_task_exists_update
			BEFORE UPDATE OF workstream_id, task_id ON workstream_result_links
			WHEN length(NEW.task_id) > 0
				AND NOT EXISTS (SELECT 1 FROM workstream_tasks
					WHERE workstream_id = NEW.workstream_id AND task_id = NEW.task_id)
			BEGIN SELECT RAISE(ABORT, 'workstream result link task does not exist'); END`,

		`DROP TRIGGER workstream_transitions_immutable_update`,
		`DROP TRIGGER workstream_transitions_immutable_delete`,
		`DROP INDEX workstream_transitions_by_workstream`,
		`DROP INDEX workstream_transitions_by_source`,
		`CREATE TABLE workstream_transitions_v47 (
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
			CHECK (action IN ('create_workstream', 'activate_workstream', 'pause_workstream', 'resume_workstream', 'cancel_workstream', 'complete_workstream', 'propose_task', 'reject_task', 'queue_task', 'start_task', 'settle_task', 'revise_plan', 'record_constraint', 'propose_decision', 'request_human_decision', 'approve_decision', 'reject_decision', 'resolve_question', 'block_workstream', 'unblock_workstream', 'link_completed_result'))
		) WITHOUT ROWID`,
		`INSERT INTO workstream_transitions_v47
			SELECT workstream_id, from_revision, to_revision, source, source_id, actor, action,
			payload_digest, payload_json, state_digest, state_json, committed_at FROM workstream_transitions`,
		`DROP TABLE workstream_transitions`,
		`ALTER TABLE workstream_transitions_v47 RENAME TO workstream_transitions`,
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
