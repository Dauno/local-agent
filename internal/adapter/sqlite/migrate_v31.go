package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// migrateV31 repairs the historical identity of foreground inline results and
// retires claimable foreground activations before any worker starts. It is a
// data migration only: no schema changes, no log per row, and no re-execution
// of the configured redactor (foreground rows were persisted after redaction).
// Every change runs inside the caller's upgrade transaction; any error rolls
// back completely and the schema version does not advance.
func migrateV31(ctx context.Context, tx *sql.Tx) error {
	now := time.Now().UTC().UnixNano()
	if err := repairV31ForegroundInlineIdentity(ctx, tx); err != nil {
		return err
	}
	return retireV31ForegroundActivations(ctx, tx, now)
}

// repairV31ForegroundInlineIdentity recomputes the result identity of
// completed foreground inline rows from the persisted summary after applying
// host-owned control sanitization. Byte counts always use len([]byte(...)) in
// Go, never SQLite length(TEXT). The SELECT deliberately has no
// length(result_summary) predicate: SQLite text functions stop at the first
// NUL, so a historical summary starting with U+0000 reports length 0 and
// would otherwise never be repaired. Validity, emptiness and sanitization are
// decided in Go. A summary that is not valid UTF-8 or that sanitizes to empty
// stays unavailable with an empty identity; no digest is ever fabricated and
// no row content is logged.
func repairV31ForegroundInlineIdentity(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT job_id, result_summary, result_sha256, result_bytes
		FROM external_agent_jobs
		WHERE status = 'completed' AND mode = 'foreground' AND result_artifact = ''`)
	if err != nil {
		return fmt.Errorf("read historical foreground inline results: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var jobID, summary, sha string
		var bytes int64
		if err := rows.Scan(&jobID, &summary, &sha, &bytes); err != nil {
			return fmt.Errorf("scan historical foreground inline result: %w", err)
		}
		sanitized, err := domain.SanitizeResultText(summary)
		if err != nil {
			if sha != "" || bytes != 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE external_agent_jobs
					SET result_sha256 = '', result_bytes = 0 WHERE job_id = ?`, jobID); err != nil {
					return fmt.Errorf("fail closed historical foreground inline identity: %w", err)
				}
			}
			continue
		}
		digest := sha256.Sum256([]byte(sanitized))
		wantSHA := fmt.Sprintf("%x", digest)
		wantBytes := int64(len([]byte(sanitized)))
		if sha == wantSHA && bytes == wantBytes {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE external_agent_jobs
			SET result_summary = ?, result_sha256 = ?, result_bytes = ? WHERE job_id = ?`,
			sanitized, wantSHA, wantBytes, jobID); err != nil {
			return fmt.Errorf("repair historical foreground inline identity: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read historical foreground inline results: %w", err)
	}
	return nil
}

// retireV31ForegroundActivations terminalizes every non-terminal activation
// owned by a foreground job using only legal state-machine transitions, clears
// its lease, and stamps the bounded foreground_activation_retired code.
// Terminal activations, notification rows, Slack evidence and conversation
// transcripts are preserved untouched.
func retireV31ForegroundActivations(ctx context.Context, tx *sql.Tx, now int64) error {
	foreground := `EXISTS (SELECT 1 FROM external_agent_jobs j WHERE j.job_id = external_agent_job_activations.job_id AND j.mode = 'foreground')`
	// pending -> processing -> failed: the state trigger requires the
	// intermediate processing hop for pending rows.
	if _, err := tx.ExecContext(ctx, `UPDATE external_agent_job_activations
		SET state = ?, updated_at = ?
		WHERE state = ? AND `+foreground,
		domain.ActivationProcessing, now, domain.ActivationPending); err != nil {
		return fmt.Errorf("retire pending foreground activations: %w", err)
	}
	// processing -> failed (includes the rows promoted from pending above).
	if _, err := tx.ExecContext(ctx, `UPDATE external_agent_job_activations
		SET state = ?, last_error_code = ?, lease_owner = '', lease_expiry = 0, next_attempt_at = 0, updated_at = ?
		WHERE state = ? AND `+foreground,
		domain.ActivationFailed, domain.ActivationForegroundRetiredCode, now, domain.ActivationProcessing); err != nil {
		return fmt.Errorf("retire processing foreground activations: %w", err)
	}
	// response_prepared -> failed: the prepared response is never published.
	if _, err := tx.ExecContext(ctx, `UPDATE external_agent_job_activations
		SET state = ?, last_error_code = ?, lease_owner = '', lease_expiry = 0, next_attempt_at = 0, updated_at = ?
		WHERE state = ? AND `+foreground,
		domain.ActivationFailed, domain.ActivationForegroundRetiredCode, now, domain.ActivationResponsePrepared); err != nil {
		return fmt.Errorf("retire prepared foreground activations: %w", err)
	}
	// model_started -> completion_unknown: never replayed or claimed again.
	if _, err := tx.ExecContext(ctx, `UPDATE external_agent_job_activations
		SET state = ?, last_error_code = ?, lease_owner = '', lease_expiry = 0, next_attempt_at = 0, updated_at = ?
		WHERE state = ? AND `+foreground,
		domain.ActivationCompletionUnknown, domain.ActivationForegroundRetiredCode, now, domain.ActivationModelStarted); err != nil {
		return fmt.Errorf("retire started foreground activations: %w", err)
	}
	return nil
}
