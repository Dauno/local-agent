package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/acpclient"
	"github.com/Dauno/slack-local-agent/internal/adapter/fsartifact"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	externalagent "github.com/Dauno/slack-local-agent/internal/usecase/externalagent"
)

// ReconcileJob is the local operator boundary. It loads the persisted actor
// and destination, so neither can be supplied as CLI input.
func (a *Application) ReconcileJob(ctx context.Context, jobID string, expectedRevision int) (domain.ExternalAgentJobStatusView, error) {
	if expectedRevision < 0 {
		return domain.ExternalAgentJobStatusView{}, errors.New("expected status revision is required")
	}
	setup, err := a.loadRuntimeSetup()
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, err
	}
	if setup.defs == nil {
		return domain.ExternalAgentJobStatusView{}, errors.New("durable ACP definitions are unavailable")
	}
	lock, err := a.schemaLock(setup.paths.DatabaseFile)
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, schemaLockFailure(err)
	}
	defer func() { _ = lock.Release() }()
	// Rollout-completeness preflight (checkpoint 5): read-only reads under the
	// lock, strictly before OpenCurrent's own mode=rw open, mirroring run.
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
	definition, resolved, err := resolveReconciliationAgent(setup, *job)
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, err
	}
	// Reconciliation resumes the agent's session to learn what happened. An
	// agent CLI has no session that this release can capture, so there is
	// nothing to resume and no honest answer to give. The operator is told
	// plainly rather than shown an ACP error for a CLI job.
	if resolved.IsAgentCLI() {
		return domain.ExternalAgentJobStatusView{}, fmt.Errorf(
			"session recovery is not supported for agent_cli job %q: inspect the workspace and close the job explicitly", job.ID)
	}
	revision, err := agentExecutionFingerprint(definition, resolved, setup.paths.SandboxProjectRoots, setup.cfg)
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, fmt.Errorf("fingerprint ACP recovery scope: %w", err)
	}
	artifactStore, err := fsartifact.New(setup.paths.ArtifactDir, int64(setup.cfg.ACP.MaxResultArtifactBytes))
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, fmt.Errorf("initialize ACP result artifact store: %w", err)
	}
	policy := domain.ResultDeliveryPolicy{
		MaxMarkdownParts:       setup.cfg.ACP.Delivery.MaxMarkdownParts,
		MaxFileBytes:           int64(setup.cfg.ACP.Delivery.MaxFileBytes),
		MaxInlineResultBytes:   int64(setup.cfg.ACP.MaxInlineResultBytes),
		MaxResultArtifactBytes: int64(setup.cfg.ACP.MaxResultArtifactBytes),
	}
	if err := policy.Validate(); err != nil {
		return domain.ExternalAgentJobStatusView{}, err
	}
	var nativeResults port.TrustedResultStore
	if setup.cfg.Orchestration.ResultHandles.Enabled {
		payloads, payloadErr := fsartifact.NewTypedStore(filepath.Join(setup.paths.ArtifactDir, "v2-results"), int64(setup.cfg.ACP.MaxResultArtifactBytes))
		if payloadErr != nil {
			return domain.ExternalAgentJobStatusView{}, fmt.Errorf("initialize native ACP result payload store: %w", payloadErr)
		}
		nativeResults, err = adaptersqlite.NewResultStore(store, payloads)
		if err != nil {
			return domain.ExternalAgentJobStatusView{}, fmt.Errorf("initialize native ACP result store: %w", err)
		}
	}
	resolvedCopy := *resolved
	dispatcher := &acpJobDispatcher{
		children: []preparedAgentTool{{definition: definition, acpRuntime: acpclient.NewWithBounds(resolved.Command, resolved.Args, acpclient.Bounds{
			MaxFrameBytes:          setup.cfg.ACP.MaxFrameBytes,
			MaxInlineResultBytes:   setup.cfg.ACP.MaxInlineResultBytes,
			MaxResultArtifactBytes: setup.cfg.ACP.MaxResultArtifactBytes,
			StderrTailBytes:        setup.cfg.ACP.StderrTailBytes,
		}), acpResolved: &resolvedCopy, projectRoots: setup.paths.SandboxProjectRoots, registryRevision: revision}},
		global: setup.defs.Agents["root_agent"].EffectiveDelegatedGlobalInstruction(), store: jobStore,
		artifacts: artifactStore, results: nativeResults, policy: policy, partLabels: setup.cfg.Slack.PartLabels,
		reconciliationTimeout: time.Duration(setup.cfg.ACP.ReconciliationTimeoutSeconds) * time.Second,
	}
	service, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Duration(setup.cfg.ACP.DefaultJobTimeoutSeconds) * time.Second,
		MaxTimeout:     time.Duration(setup.cfg.ACP.MaxJobTimeoutSeconds) * time.Second,
		LeaseTTL:       30 * time.Second, PollInterval: time.Second, Concurrency: 1, MaxAttempts: 2,
	}, externalagent.Dependencies{Store: jobStore, Runtime: dispatcher, Artifacts: artifactStore, NativeResults: nativeResults, MaxResultBytes: int64(setup.cfg.ACP.MaxResultArtifactBytes)})
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, err
	}
	if _, err := service.ReconcileExpected(ctx, job.ID, job.Actor, job.ConversationKey, expectedRevision); err != nil {
		return domain.ExternalAgentJobStatusView{}, err
	}
	updated, err := jobStore.GetJob(ctx, job.ID)
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, err
	}
	if updated == nil {
		return domain.ExternalAgentJobStatusView{}, errors.New("external-agent job disappeared after reconciliation")
	}
	return updated.StatusView(), nil
}

// resolveReconciliationAgent finds the durable leaf a job was started from.
// Both external-agent families are searched: an ACP leaf keys on `runtime`, an
// agent CLI leaf on `model`, and the job stores whichever one applies as its
// profile.
func resolveReconciliationAgent(setup runtimeSetup, job domain.ExternalAgentJob) (agentdef.AgentDef, *agentdef.ResolvedModel, error) {
	for _, definition := range setup.defs.Agents {
		if definition.ExecutionMode != agentdef.ExecutionModeDurableJob {
			continue
		}
		reference := definition.Model
		if definition.AgentClass == "AcpAgent" {
			reference = definition.Runtime
		}
		if reference != job.Profile {
			continue
		}
		resolved, err := setup.defs.ResolveModel(reference)
		if err != nil {
			return agentdef.AgentDef{}, nil, err
		}
		if resolved.Provider.Name != job.Provider {
			continue
		}
		return definition, resolved, nil
	}
	return agentdef.AgentDef{}, nil, errors.New("durable external-agent job provider/profile is unavailable")
}

var _ interface {
	ReconcileJob(context.Context, string, int) (domain.ExternalAgentJobStatusView, error)
} = (*Application)(nil)

var _ port.ExternalAgentJobRuntime = (*acpJobDispatcher)(nil)
