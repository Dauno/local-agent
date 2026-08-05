package sqlite

import (
	"context"
	"errors"
	"fmt"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// IdentityHealth returns a content-free aggregate of durable result identity
// completeness. Every query is an aggregate COUNT that never loads job rows,
// notification rows, activation rows, digest values, references, or content.
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
	}{
		{
			name: "completed jobs without result identity",
			query: `SELECT COUNT(*) FROM external_agent_jobs
				WHERE status = 'completed' AND (length(result_sha256) != 64 OR result_bytes <= 0)`,
			target: &health.JobsCompletedWithoutResultIdentity,
		},
		{
			name: "notifications without notification identity",
			query: `SELECT COUNT(*) FROM external_agent_job_notifications
				WHERE length(notification_sha256) != 64 OR notification_bytes <= 0`,
			target: &health.NotificationsWithoutIdentity,
		},
		{
			name: "activations without content bytes",
			query: `SELECT COUNT(*) FROM external_agent_job_activations
				WHERE terminal_status = 'completed' AND content_bytes <= 0`,
			target: &health.ActivationsWithoutContent,
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
		args := []any{}
		if entry.name == "retired foreground activations" {
			args = append(args, domain.ActivationForegroundRetiredCode)
		}
		if err := s.db.QueryRowContext(ctx, entry.query, args...).Scan(entry.target); err != nil {
			return domain.ExternalAgentJobIdentityHealth{}, fmt.Errorf("count %s: %w", entry.name, err)
		}
	}
	return health, nil
}
