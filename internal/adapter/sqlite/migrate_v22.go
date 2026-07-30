package sqlite

import (
	"context"
	"database/sql"
)

// v22 adds immutable result-delivery metadata without rewriting legacy rows.
// Agent Builder draft columns are intentionally in v23 because v22 may already
// exist in deployed databases. Rows carrying legacy_v1 remain readable and are
// deliberately not replayed.
func migrateV22(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 22, []string{
		`ALTER TABLE external_agent_job_notifications ADD COLUMN delivery_mode TEXT NOT NULL DEFAULT 'markdown'`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN policy_version TEXT NOT NULL DEFAULT 'legacy_v1'`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN artifact_ref TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN result_bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN max_markdown_parts INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN upload_state TEXT NOT NULL DEFAULT 'not_applicable'`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN slack_file_id TEXT NOT NULL DEFAULT ''`,
		`CREATE TRIGGER external_agent_job_notifications_delivery_insert
			BEFORE INSERT ON external_agent_job_notifications
			WHEN NEW.policy_version != 'legacy_v1' AND (
				NEW.delivery_mode NOT IN ('markdown', 'file') OR
				length(NEW.content_sha256) != 64 OR NEW.result_bytes <= 0 OR NEW.max_markdown_parts < 1 OR NEW.max_markdown_parts > 8 OR
				(NEW.delivery_mode = 'markdown' AND (length(NEW.artifact_ref) > 0 OR NEW.upload_state != 'not_applicable')) OR
				(NEW.delivery_mode = 'file' AND length(NEW.artifact_ref) = 0) OR
				NEW.upload_state NOT IN ('not_applicable', 'pending', 'url_requested', 'bytes_uploaded', 'completed', 'unknown')
			)
			BEGIN SELECT RAISE(ABORT, 'invalid external-agent result delivery'); END`,
		`CREATE TRIGGER external_agent_job_notifications_delivery_update
			BEFORE UPDATE ON external_agent_job_notifications
			WHEN NEW.policy_version != OLD.policy_version OR (OLD.policy_version != 'legacy_v1' AND (
				NEW.delivery_mode != OLD.delivery_mode OR
				NEW.artifact_ref != OLD.artifact_ref OR NEW.canonical_markdown != OLD.canonical_markdown OR
				NEW.content_sha256 != OLD.content_sha256 OR NEW.renderer_version != OLD.renderer_version OR
				NEW.result_bytes != OLD.result_bytes OR NEW.max_markdown_parts != OLD.max_markdown_parts OR
				NEW.channel_id != OLD.channel_id OR
				NEW.thread_ts != OLD.thread_ts OR NEW.job_id != OLD.job_id OR
				NEW.status_revision != OLD.status_revision OR NEW.kind != OLD.kind
			))
			BEGIN SELECT RAISE(ABORT, 'external-agent result delivery identity is immutable'); END`,
		`CREATE TRIGGER external_agent_job_notifications_delivery_shape_update
			BEFORE UPDATE ON external_agent_job_notifications
			WHEN NEW.policy_version != 'legacy_v1' AND (
				NEW.delivery_mode NOT IN ('markdown', 'file') OR length(NEW.content_sha256) != 64 OR NEW.result_bytes <= 0 OR
				NEW.max_markdown_parts < 1 OR NEW.max_markdown_parts > 8 OR
				(NEW.delivery_mode = 'markdown' AND (length(NEW.artifact_ref) > 0 OR NEW.upload_state != 'not_applicable')) OR
				(NEW.delivery_mode = 'file' AND length(NEW.artifact_ref) = 0) OR
				NEW.upload_state NOT IN ('not_applicable', 'pending', 'url_requested', 'bytes_uploaded', 'completed', 'unknown')
			)
			BEGIN SELECT RAISE(ABORT, 'invalid external-agent result delivery'); END`,
	})
}
