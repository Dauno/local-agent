package sqlite

import (
	"context"
	"database/sql"
)

// migrateV45 records the host-owned completion policy and the final scope of
// each root activation. It does not create activations or change delivery
// evidence. Historical detached jobs with a complete binding retain the old
// workstream-only behavior; all other historical jobs remain delivery-only.
func migrateV45(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 45, []string{
		`ALTER TABLE external_agent_jobs ADD COLUMN completion_policy TEXT NOT NULL DEFAULT 'delivery_only'
			CHECK (completion_policy IN ('delivery_only', 'workstream_only', 'automatic_root'))`,
		`ALTER TABLE external_agent_job_activations ADD COLUMN activation_scope TEXT NOT NULL DEFAULT 'legacy'
			CHECK (activation_scope IN ('legacy', 'conversation', 'workstream'))`,
		`UPDATE external_agent_jobs SET completion_policy = CASE
			WHEN mode = 'detached' AND length(workstream_id) > 0 AND length(task_id) > 0
				AND length(execution_identity) > 0 THEN 'workstream_only'
			ELSE 'delivery_only' END`,
		`UPDATE external_agent_job_activations SET activation_scope = CASE
			WHEN length(workstream_id) > 0 AND length(task_id) > 0
				AND length(execution_identity) > 0 THEN 'workstream'
			ELSE 'legacy' END`,
		`CREATE TRIGGER external_agent_jobs_completion_policy_immutable
			BEFORE UPDATE OF completion_policy ON external_agent_jobs
			WHEN NEW.completion_policy != OLD.completion_policy
			BEGIN SELECT RAISE(ABORT, 'external-agent completion policy is immutable'); END`,
		`CREATE TRIGGER external_agent_job_activations_scope_immutable
			BEFORE UPDATE OF activation_scope ON external_agent_job_activations
			WHEN NEW.activation_scope != OLD.activation_scope
			BEGIN SELECT RAISE(ABORT, 'external-agent activation scope is immutable'); END`,
	})
}
