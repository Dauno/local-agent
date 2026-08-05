package sqlite

import (
	"context"
	"errors"
	"fmt"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// legacyResultIdentityEvent marks a completed job that was already present
// when v32 ran but whose result was intentionally left unavailable. The event
// table predates v32, so this preserves provenance without requiring v33.
const legacyResultIdentityEvent = "legacy_result_identity"

// IdentityHealth returns a content-free aggregate of durable result identity
// completeness. Every query is an aggregate COUNT that never loads job rows,
// notification rows, activation rows, digest values, references, or content.
// Digest predicates use SQLite's negated GLOB class plus an exact length check;
// this enforces lowercase hexadecimal without selecting digest values into Go.
// The only activation identity it inspects is the bounded
// foreground_activation_retired error code and the non-terminal state set.
func (s *ExternalAgentJobStore) IdentityHealth(ctx context.Context) (domain.ExternalAgentJobIdentityHealth, error) {
	if s == nil || s.db == nil {
		return domain.ExternalAgentJobIdentityHealth{}, errors.New("external-agent identity health store is not configured")
	}
	var health domain.ExternalAgentJobIdentityHealth
	queries := []struct {
		name   string
		query  string
		target *int
		args   []any
	}{
		{
			name: "current completed jobs without result identity",
			query: `SELECT COUNT(*) FROM external_agent_jobs
				WHERE status = 'completed'
					AND NOT EXISTS (
						SELECT 1 FROM external_agent_job_events e
						WHERE e.job_id = external_agent_jobs.job_id
							AND e.status_revision = external_agent_jobs.status_revision
							AND e.event_kind = ?
					)
					AND (result_bytes <= 0 OR length(result_sha256) != 64 OR result_sha256 GLOB '*[^0-9a-f]*')`,
			target: &health.JobsCompletedWithoutResultIdentity,
			args:   []any{legacyResultIdentityEvent},
		},
		{
			name: "legacy completed jobs without result identity",
			query: `SELECT COUNT(*) FROM external_agent_jobs j
				WHERE j.status = 'completed' AND EXISTS (
					SELECT 1 FROM external_agent_job_events e
					WHERE e.job_id = j.job_id AND e.status_revision = j.status_revision
						AND e.event_kind = ?
				)`,
			target: &health.JobsCompletedWithoutResultIdentityLegacy,
			args:   []any{legacyResultIdentityEvent},
		},
		{
			name: "notifications without notification identity",
			query: `SELECT COUNT(*) FROM external_agent_job_notifications
				WHERE length(notification_sha256) != 64 OR notification_sha256 GLOB '*[^0-9a-f]*' OR notification_bytes <= 0
					OR ((result_sha256 != '' OR result_bytes != 0) AND
						(result_bytes <= 0 OR length(result_sha256) != 64 OR result_sha256 GLOB '*[^0-9a-f]*'))
					OR (terminal_status = 'completed'
						AND NOT EXISTS (
							SELECT 1 FROM external_agent_job_events e
							WHERE e.job_id = external_agent_job_notifications.job_id
								AND e.event_kind = ?
						)
						AND (result_bytes <= 0 OR length(result_sha256) != 64 OR result_sha256 GLOB '*[^0-9a-f]*'))`,
			target: &health.NotificationsWithoutIdentity,
			args:   []any{legacyResultIdentityEvent},
		},
		{
			name: "activations without content bytes",
			query: `SELECT COUNT(*) FROM external_agent_job_activations
				WHERE terminal_status = 'completed' AND content_bytes <= 0`,
			target: &health.ActivationsWithoutContent,
		},
		{
			name: "activations without notification identity",
			query: `SELECT COUNT(*) FROM external_agent_job_activations
				WHERE length(notification_sha256) != 64 OR notification_sha256 GLOB '*[^0-9a-f]*'`,
			target: &health.ActivationsWithoutIdentity,
		},
		{
			name: "non-terminal foreground activations",
			query: `SELECT COUNT(*) FROM external_agent_job_activations a
				JOIN external_agent_jobs j ON j.job_id = a.job_id
				WHERE j.mode = 'foreground' AND a.state NOT IN ` + activationTerminalStates,
			target: &health.ForegroundActivationsActive,
		},
		{
			name: "retired foreground activations",
			query: `SELECT COUNT(*) FROM external_agent_job_activations
				WHERE last_error_code = ?`,
			target: &health.RetiredForegroundActivations,
		},
	}
	for _, entry := range queries {
		args := entry.args
		if entry.name == "retired foreground activations" {
			args = append(args, domain.ActivationForegroundRetiredCode)
		}
		if err := s.db.QueryRowContext(ctx, entry.query, args...).Scan(entry.target); err != nil {
			return domain.ExternalAgentJobIdentityHealth{}, fmt.Errorf("count %s: %w", entry.name, err)
		}
	}
	return health, nil
}
