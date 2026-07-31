package sqlite

import (
	"context"
	"database/sql"
)

// v26 applies the v22 delivery evidence invariants to databases that already
// passed through later additive migrations before these checks were added.
func migrateV26(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 26, []string{
		`CREATE TRIGGER external_agent_job_notifications_delivery_evidence_insert_v26
			BEFORE INSERT ON external_agent_job_notifications
			WHEN NEW.policy_version != 'legacy_v1' AND NEW.publish_state = 'published' AND (
				length(NEW.recovered_slack_ts) = 0 OR
				(NEW.delivery_mode = 'file' AND (NEW.upload_state != 'completed' OR length(NEW.slack_file_id) = 0))
			)
			BEGIN SELECT RAISE(ABORT, 'published external-agent delivery lacks evidence'); END`,
		`CREATE TRIGGER external_agent_job_notifications_delivery_evidence_update_v26
			BEFORE UPDATE ON external_agent_job_notifications
			WHEN NEW.policy_version != 'legacy_v1' AND (
				(length(OLD.slack_file_id) > 0 AND NEW.slack_file_id != OLD.slack_file_id) OR
				(NEW.publish_state = 'published' AND (
					length(NEW.recovered_slack_ts) = 0 OR
					(NEW.delivery_mode = 'file' AND (NEW.upload_state != 'completed' OR length(NEW.slack_file_id) = 0))
				))
			)
			BEGIN SELECT RAISE(ABORT, 'published external-agent delivery lacks evidence'); END`,
		`CREATE TRIGGER external_agent_job_notifications_delivery_shape_v26
			BEFORE UPDATE ON external_agent_job_notifications
			WHEN NEW.policy_version = 'delivery_v1' AND (
				NEW.delivery_mode NOT IN ('markdown', 'file') OR length(NEW.content_sha256) != 64 OR NEW.result_bytes <= 0 OR
				NEW.max_markdown_parts < 1 OR NEW.max_markdown_parts > 8 OR
				(NEW.delivery_mode = 'markdown' AND (length(NEW.artifact_ref) > 0 OR NEW.upload_state != 'not_applicable')) OR
				(NEW.delivery_mode = 'file' AND (length(NEW.artifact_ref) = 0 OR NEW.upload_state = 'not_applicable')) OR
				NEW.upload_state NOT IN ('not_applicable', 'pending', 'url_requested', 'bytes_uploaded', 'completed', 'unknown')
			)
			BEGIN SELECT RAISE(ABORT, 'invalid external-agent result delivery'); END`,
	})
}
