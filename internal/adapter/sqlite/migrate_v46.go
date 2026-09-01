package sqlite

import (
	"context"
	"database/sql"
)

// migrateV46 makes the workstream task the owner of its durable job
// association. Existing tasks remain unassociated.
func migrateV46(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 46, []string{
		`ALTER TABLE workstream_tasks ADD COLUMN job_id TEXT NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX workstream_tasks_by_job
			ON workstream_tasks (job_id) WHERE length(job_id) > 0`,
		`CREATE TRIGGER workstream_tasks_job_exists
			BEFORE UPDATE OF job_id ON workstream_tasks
			WHEN length(NEW.job_id) > 0
				AND NOT EXISTS (SELECT 1 FROM external_agent_jobs WHERE job_id = NEW.job_id)
			BEGIN SELECT RAISE(ABORT, 'workstream task job does not exist'); END`,
		`CREATE TRIGGER workstream_tasks_job_immutable
			BEFORE UPDATE OF job_id ON workstream_tasks
			WHEN length(OLD.job_id) > 0 AND NEW.job_id != OLD.job_id
			BEGIN SELECT RAISE(ABORT, 'workstream task job is immutable'); END`,
	})
}
