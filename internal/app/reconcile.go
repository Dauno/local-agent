package app

import (
	"context"
	"errors"
	"fmt"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// ReconcileJob is the local operator boundary for a completion-unknown job.
//
// Reconciliation resumes the agent's session to learn what actually happened,
// which is the only honest way to close a job whose side effects are unknown.
// No runtime in this release captures a session: the `session` descriptor block
// is parsed and validated, but the engine does not yet record the identifier.
//
// The command therefore reports plainly that the job must be inspected and
// closed by hand. It still runs the full schema preflight first, so an operator
// who is really facing a migration problem is told about that instead.
func (a *Application) ReconcileJob(ctx context.Context, jobID string, expectedRevision int) (domain.ExternalAgentJobStatusView, error) {
	if expectedRevision < 0 {
		return domain.ExternalAgentJobStatusView{}, errors.New("expected status revision is required")
	}
	setup, err := a.loadRuntimeSetup()
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, err
	}
	lock, err := a.schemaLock(setup.paths.DatabaseFile)
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, schemaLockFailure(err)
	}
	defer func() { _ = lock.Release() }()
	// Rollout-completeness preflight: read-only reads under the lock, strictly
	// before OpenCurrent's own mode=rw open, mirroring run.
	a.traceSchemaEvent("preflight")
	if err := a.requireRolloutComplete(ctx, setup.paths.DatabaseFile); err != nil {
		return domain.ExternalAgentJobStatusView{}, rolloutPreflightFailure(err)
	}
	store, err := a.openCurrentTraced(ctx, setup.paths.DatabaseFile)
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, schemaOpenFailure(err)
	}
	defer func() { _ = store.Close() }()
	job, err := adaptersqlite.NewExternalAgentJobStore(store).GetJob(ctx, jobID)
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, err
	}
	if job == nil {
		return domain.ExternalAgentJobStatusView{}, errors.New("external-agent job was not found")
	}
	return domain.ExternalAgentJobStatusView{}, fmt.Errorf(
		"session recovery is not supported for job %q: inspect the workspace and close the job explicitly", job.ID)
}

var _ interface {
	ReconcileJob(context.Context, string, int) (domain.ExternalAgentJobStatusView, error)
} = (*Application)(nil)

var _ port.ExternalAgentJobRuntime = (*acpJobDispatcher)(nil)
