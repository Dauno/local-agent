package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// migrateV32 persists the explicit completion route and separates the
// notification identity from the result identity. It is additive and applies
// cleanly after v30 or after the pending v31 repair migration: v31 only
// repairs job rows and retires activations, so v32 never assumes repaired
// data and fails closed on any row it cannot classify.
func migrateV32(ctx context.Context, tx *sql.Tx) error {
	if err := execMigration(ctx, tx, 32, []string{
		`ALTER TABLE external_agent_job_notifications ADD COLUMN root_activation_required INTEGER NOT NULL DEFAULT 0 CHECK (root_activation_required IN (0, 1))`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN notification_sha256 TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN notification_bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE external_agent_job_notifications ADD COLUMN result_sha256 TEXT NOT NULL DEFAULT ''`,
	}); err != nil {
		return err
	}
	// Grandfathered file-mode rows predate the v26 delivery-shape update
	// trigger and must be normalized before the backfill UPDATEs them,
	// otherwise the upgrade aborts permanently. Backfill runs before the v32
	// immutability triggers exist so historical rows can still receive their
	// persisted identities.
	if err := normalizeV32GrandfatheredDeliveries(ctx, tx); err != nil {
		return err
	}
	if err := backfillV32NotificationIdentities(ctx, tx); err != nil {
		return err
	}
	return execMigration(ctx, tx, 32, []string{
		`CREATE TRIGGER external_agent_job_notifications_route_insert
			BEFORE INSERT ON external_agent_job_notifications
			WHEN NEW.root_activation_required NOT IN (0, 1) OR
				(NEW.root_activation_required = 1 AND (length(NEW.terminal_status) = 0 OR length(NEW.notification_sha256) != 64 OR NEW.notification_bytes <= 0))
			BEGIN SELECT RAISE(ABORT, 'invalid external-agent completion route'); END`,
		`CREATE TRIGGER external_agent_job_notifications_route_update
			BEFORE UPDATE OF root_activation_required ON external_agent_job_notifications
			WHEN NEW.root_activation_required != OLD.root_activation_required
			BEGIN SELECT RAISE(ABORT, 'external-agent completion route is immutable'); END`,
		`CREATE TRIGGER external_agent_job_notifications_identity_update
			BEFORE UPDATE ON external_agent_job_notifications
			WHEN NEW.notification_sha256 != OLD.notification_sha256 OR
				NEW.notification_bytes != OLD.notification_bytes OR
				NEW.result_sha256 != OLD.result_sha256 OR NEW.result_bytes != OLD.result_bytes
			BEGIN SELECT RAISE(ABORT, 'external-agent delivery identity is immutable'); END`,
	})
}

// normalizeV32GrandfatheredDeliveries reclassifies the file-mode delivery_v1
// rows the historical v25 insert trigger admitted with the column default
// upload_state='not_applicable'. The v26 delivery-shape update trigger rejects
// that shape on any UPDATE, so the rows must be normalized before the identity
// backfill touches them; otherwise the whole upgrade transaction aborts on
// every startup. Rows that already carry publication evidence are left
// untouched (their upload state can never be re-derived) and are skipped by
// the backfill: they remain historical audit rows and never block the upgrade.
// No row is deleted and no notification content is changed.
func normalizeV32GrandfatheredDeliveries(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE external_agent_job_notifications
		SET upload_state = 'unknown'
		WHERE policy_version = 'delivery_v1' AND delivery_mode = 'file'
			AND upload_state = 'not_applicable' AND publish_state != 'published'
			AND length(artifact_ref) > 0`); err != nil {
		return fmt.Errorf("normalize grandfathered external-agent deliveries: %w", err)
	}
	return nil
}

// backfillV32NotificationIdentities recomputes the notification identity over
// canonical Markdown in Go and mirrors the job result identity where it is
// complete. Rows that cannot be classified keep empty/zero identity and never
// become activation-eligible (fail-closed). No content, digest, or binding is
// ever logged per row.
func backfillV32NotificationIdentities(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT n.job_id, n.status_revision, n.kind, n.terminal_status, n.canonical_markdown,
		j.mode, j.result_sha256, j.result_bytes,
		n.policy_version, n.delivery_mode, n.upload_state
		FROM external_agent_job_notifications n
		JOIN external_agent_jobs j ON j.job_id = n.job_id`)
	if err != nil {
		return fmt.Errorf("read external-agent notification identities: %w", err)
	}
	defer rows.Close()
	update, err := tx.PrepareContext(ctx, `UPDATE external_agent_job_notifications SET
		notification_sha256 = ?, notification_bytes = ?, result_sha256 = ?,
		result_bytes = CASE WHEN result_bytes = 0 THEN ? ELSE result_bytes END,
		root_activation_required = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ?`)
	if err != nil {
		return fmt.Errorf("prepare external-agent notification identity backfill: %w", err)
	}
	defer update.Close()
	for rows.Next() {
		var jobID, kind, terminalStatus, markdown, jobMode, jobResultSHA string
		var revision int
		var jobResultBytes int64
		var policyVersion, deliveryMode, uploadState string
		if err := rows.Scan(&jobID, &revision, &kind, &terminalStatus, &markdown, &jobMode, &jobResultSHA, &jobResultBytes, &policyVersion, &deliveryMode, &uploadState); err != nil {
			return fmt.Errorf("scan external-agent notification identity: %w", err)
		}
		// Grandfathered file-mode rows that normalization could not repair
		// (published evidence or missing artifact reference) abort any UPDATE
		// through the v26 shape trigger. They keep the v32 column defaults:
		// historical audit rows without identity, never activation-eligible.
		if policyVersion == string(domain.JobDeliveryPolicyV1) &&
			deliveryMode == string(domain.JobResultDeliveryFile) &&
			uploadState == string(domain.JobResultUploadNotApplicable) {
			continue
		}
		notificationSHA := ""
		notificationBytes := int64(0)
		if utf8.ValidString(markdown) && strings.TrimSpace(markdown) != "" {
			digest := sha256.Sum256([]byte(markdown))
			notificationSHA = fmt.Sprintf("%x", digest)
			notificationBytes = int64(len([]byte(markdown)))
		}
		resultSHA, resultBytes := domain.ValidResultIdentity(jobResultSHA, jobResultBytes)
		rootRequired := 0
		if terminalStatus != "" && jobMode == string(domain.JobDetached) && notificationSHA != "" {
			rootRequired = 1
		}
		if _, err := update.ExecContext(ctx, notificationSHA, notificationBytes, resultSHA, resultBytes, rootRequired, jobID, revision, kind); err != nil {
			return fmt.Errorf("backfill external-agent notification identity: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read external-agent notification identities: %w", err)
	}
	return nil
}
