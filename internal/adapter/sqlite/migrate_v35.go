package sqlite

import (
	"context"
	"database/sql"
)

// migrateV35 rebuilds only the immutable journal table because SQLite cannot
// extend its action CHECK constraint in place. Every existing journal row and
// its source/revision uniqueness constraints are preserved.
func migrateV35(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 35, []string{
		`DROP TRIGGER workstream_transitions_immutable_update`,
		`DROP TRIGGER workstream_transitions_immutable_delete`,
		`DROP INDEX workstream_transitions_by_workstream`,
		`DROP INDEX workstream_transitions_by_source`,
		`CREATE TABLE workstream_transitions_v35 (
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
			CHECK (action IN ('create_workstream', 'activate_workstream', 'pause_workstream', 'resume_workstream', 'cancel_workstream', 'complete_workstream', 'propose_task', 'reject_task', 'revise_plan', 'record_constraint', 'propose_decision', 'request_human_decision', 'approve_decision', 'reject_decision', 'resolve_question', 'block_workstream', 'unblock_workstream', 'link_completed_result'))
		) WITHOUT ROWID`,
		`INSERT INTO workstream_transitions_v35
			SELECT workstream_id, from_revision, to_revision, source, source_id, actor, action,
			payload_digest, payload_json, state_digest, state_json, committed_at FROM workstream_transitions`,
		`DROP TABLE workstream_transitions`,
		`ALTER TABLE workstream_transitions_v35 RENAME TO workstream_transitions`,
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
