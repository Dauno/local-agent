package sqlite

import (
	"context"
	"database/sql"
)

// migrateV44 adds the bounded, redaction-safe process-failure classification
// to the live progress projection. Historical rows stay empty: no trusted
// class can be reconstructed for a failure that already happened.
func migrateV44(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 44, []string{
		`ALTER TABLE external_agent_job_progress ADD COLUMN error_class TEXT NOT NULL DEFAULT ''
			CHECK (error_class IN ('', 'protocol_line_too_large', 'protocol_stdout_too_large',
				'provider_reported_failure', 'process_exit', 'timeout', 'no_response'))`,
	})
}
