package app

import (
	"context"
	"errors"
	"time"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// CloseJob closes a completion-unknown job without resuming its session.
//
// It is the operator's alternative to reconciliation: the external state was
// inspected by hand and needs no recovery. The job reaches its terminal
// abandoned status, and its conversation is notified through the durable
// outbox like any other terminal transition.
//
// Actor and destination come from durable state, so neither can be supplied
// through CLI input.
func (a *Application) CloseJob(ctx context.Context, jobID string, expectedRevision int) (domain.ExternalAgentJobStatusView, error) {
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
	// before OpenCurrent's own mode=rw open, mirroring run and reconcile.
	a.traceSchemaEvent("preflight")
	if err := a.requireRolloutComplete(ctx, setup.paths.DatabaseFile); err != nil {
		return domain.ExternalAgentJobStatusView{}, rolloutPreflightFailure(err)
	}
	store, err := a.openCurrentTraced(ctx, setup.paths.DatabaseFile)
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, schemaOpenFailure(err)
	}
	defer func() { _ = store.Close() }()
	jobStore := adaptersqlite.NewExternalAgentJobStore(store)
	job, err := jobStore.GetJob(ctx, jobID)
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, err
	}
	if job == nil {
		return domain.ExternalAgentJobStatusView{}, errors.New("external-agent job was not found")
	}
	closed, err := jobStore.AbandonCompletionUnknown(ctx, job.ID, job.Actor, job.ConversationKey, expectedRevision, time.Now().UTC())
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, err
	}
	return closed.StatusView(), nil
}

var _ interface {
	CloseJob(context.Context, string, int) (domain.ExternalAgentJobStatusView, error)
} = (*Application)(nil)

var _ port.ExternalAgentJobAbandoner = (*adaptersqlite.ExternalAgentJobStore)(nil)
