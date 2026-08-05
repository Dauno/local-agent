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
	// Preserve the distinction between pre-v32 fail-closed rows and rows
	// created after the upgrade. Reusing the existing event ledger avoids a v33
	// schema change while keeping the marker content-free and idempotent.
	if _, err := tx.ExecContext(ctx, `INSERT INTO external_agent_job_events
		(job_id, status_revision, event_kind, created_at)
		SELECT job_id, status_revision, ?, updated_at
		FROM external_agent_jobs
		WHERE status = 'completed' AND result_sha256 = '' AND result_bytes = 0
		ON CONFLICT DO NOTHING`, legacyResultIdentityEvent); err != nil {
		return fmt.Errorf("mark legacy result identities: %w", err)
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
// canonical Markdown in Go and derives the result identity only from a
// snapshot compatible with the notification row. Rows that cannot be
// classified keep empty/zero identity and never become activation-eligible
// (fail-closed). The result identity is written as an atomic pair: a row
// never keeps bytes without a digest or a digest without bytes. No content,
// digest, or binding is ever logged per row.
func backfillV32NotificationIdentities(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT n.job_id, n.status_revision, n.kind, n.terminal_status, n.canonical_markdown,
		j.mode, j.status_revision, j.status, j.result_sha256, j.result_bytes,
		n.policy_version, n.delivery_mode, n.upload_state, n.content_sha256, n.result_bytes
		FROM external_agent_job_notifications n
		JOIN external_agent_jobs j ON j.job_id = n.job_id`)
	if err != nil {
		return fmt.Errorf("read external-agent notification identities: %w", err)
	}
	defer rows.Close()
	update, err := tx.PrepareContext(ctx, `UPDATE external_agent_job_notifications SET
		notification_sha256 = ?, notification_bytes = ?, result_sha256 = ?,
		result_bytes = ?,
		root_activation_required = ?
		WHERE job_id = ? AND status_revision = ? AND kind = ?`)
	if err != nil {
		return fmt.Errorf("prepare external-agent notification identity backfill: %w", err)
	}
	defer update.Close()
	for rows.Next() {
		var jobID, kind, terminalStatus, markdown, jobMode, jobStatus, jobResultSHA string
		var revision, jobRevision int
		var jobResultBytes int64
		var policyVersion, deliveryMode, uploadState, contentSHA string
		var rowResultBytes int64
		if err := rows.Scan(&jobID, &revision, &kind, &terminalStatus, &markdown, &jobMode, &jobRevision, &jobStatus, &jobResultSHA, &jobResultBytes, &policyVersion, &deliveryMode, &uploadState, &contentSHA, &rowResultBytes); err != nil {
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
		// delivery_v1 rows already carry their own complete content identity
		// (content_sha256 over the delivered bytes); the job mirror can never
		// diverge from it, so the identity is always self-consistent. Legacy
		// rows mirror the job result identity only when the notification
		// snapshots the job's current terminal event: same kind, same
		// status_revision, and a terminal status that matches (or was never
		// recorded pre-v29). A notification for an older event never receives
		// the identity of a later result.
		resultSHA, resultBytes := "", int64(0)
		switch {
		case policyVersion == string(domain.JobDeliveryPolicyV1):
			resultSHA, resultBytes = domain.ValidResultIdentity(contentSHA, rowResultBytes)
		case kind == domain.JobNotificationTerminal && revision == jobRevision &&
			(terminalStatus == "" || terminalStatus == jobStatus):
			resultSHA, resultBytes = domain.ValidResultIdentity(jobResultSHA, jobResultBytes)
		}
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
