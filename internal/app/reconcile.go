package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/fsartifact"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	externalagent "github.com/Dauno/slack-local-agent/internal/usecase/externalagent"
)

// ReconcileJob is the local operator boundary. It loads actor and destination
// from durable state, so neither can be supplied through CLI input.
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
	definition, resolved, err := resolveReconciliationAgent(setup, *job)
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, err
	}
	if !resolved.IsAgentCLI() || resolved.Session == nil {
		return domain.ExternalAgentJobStatusView{}, fmt.Errorf("session recovery is not supported for job %q: no resumable agent_cli descriptor matches its provider and profile", job.ID)
	}
	revision, err := agentExecutionFingerprint(definition, resolved, setup.paths.SandboxProjectRoots, setup.cfg)
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, fmt.Errorf("fingerprint agent CLI recovery scope: %w", err)
	}
	cliModel, err := buildAgentCLIModel(ctx, resolved, setup.cfg, setup.paths, nil, nil)
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, err
	}
	artifactStore, err := fsartifact.New(setup.paths.ArtifactDir, int64(setup.cfg.ExternalAgent.MaxResultArtifactBytes))
	if err != nil {
		return domain.ExternalAgentJobStatusView{}, fmt.Errorf("initialize external-agent result artifact store: %w", err)
	}
	policy := domain.ResultDeliveryPolicy{
		MaxMarkdownParts: setup.cfg.ExternalAgent.Delivery.MaxMarkdownParts,
		MaxFileBytes:     int64(setup.cfg.ExternalAgent.Delivery.MaxFileBytes), MaxInlineResultBytes: int64(setup.cfg.ExternalAgent.MaxInlineResultBytes),
		MaxResultArtifactBytes: int64(setup.cfg.ExternalAgent.MaxResultArtifactBytes),
	}
	if err := policy.Validate(); err != nil {
		return domain.ExternalAgentJobStatusView{}, err
	}
	jobStore := adaptersqlite.NewExternalAgentJobStore(store)
	var nativeResults port.TrustedResultStore
	if setup.cfg.Orchestration.ResultHandles.Enabled {
		payloads, payloadErr := fsartifact.NewTypedStore(filepath.Join(setup.paths.ArtifactDir, "v2-results"), int64(setup.cfg.ExternalAgent.MaxResultArtifactBytes))
		if payloadErr != nil {
			return domain.ExternalAgentJobStatusView{}, fmt.Errorf("initialize native external-agent result payload store: %w", payloadErr)
		}
		nativeResults, err = adaptersqlite.NewResultStore(store, payloads)
		if err != nil {
			return domain.ExternalAgentJobStatusView{}, fmt.Errorf("initialize native external-agent result store: %w", err)
		}
	}
	dispatcher := &externalAgentJobDispatcher{
		children: []preparedAgentTool{{definition: definition, model: cliModel, cliResolved: resolved, projectRoots: setup.paths.SandboxProjectRoots, registryRevision: revision}},
		store:    jobStore, artifacts: artifactStore, results: nativeResults, policy: policy, partLabels: setup.cfg.Slack.PartLabels,
		reconciliationTimeout: time.Duration(setup.cfg.ExternalAgent.ReconciliationTimeoutSeconds) * time.Second,
	}
	if root, ok := setup.defs.Agents["root_agent"]; ok {
		dispatcher.global = root.EffectiveDelegatedGlobalInstruction()
	}
	runtime := &recoverableExternalAgentJobDispatcher{externalAgentJobDispatcher: dispatcher}
	service, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Duration(setup.cfg.ExternalAgent.DefaultJobTimeoutSeconds) * time.Second,
		MaxTimeout:     time.Duration(setup.cfg.ExternalAgent.MaxJobTimeoutSeconds) * time.Second,
		LeaseTTL:       30 * time.Second, PollInterval: time.Second, Concurrency: 1, MaxAttempts: 2,
	}, externalagent.Dependencies{Store: jobStore, Runtime: runtime, Artifacts: artifactStore, NativeResults: nativeResults, MaxResultBytes: int64(setup.cfg.ExternalAgent.MaxResultArtifactBytes)})
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

func resolveReconciliationAgent(setup runtimeSetup, job domain.ExternalAgentJob) (agentdef.AgentDef, *agentdef.ResolvedModel, error) {
	if setup.defs == nil {
		return agentdef.AgentDef{}, nil, errors.New("durable external-agent definitions are unavailable")
	}
	definition, resolved, found := durableAgentDescriptor(setup.defs, job.Provider, job.Profile)
	if !found {
		return agentdef.AgentDef{}, nil, fmt.Errorf("durable job %q uses provider/profile %s/%s, but no current agent_cli definition matches it", job.ID, job.Provider, job.Profile)
	}
	return definition, resolved, nil
}

// durableAgentDescriptor finds the current durable definition for a job's
// provider and profile. A job whose definition was renamed or removed matches
// nothing, and the caller must report that rather than guess a replacement.
func durableAgentDescriptor(defs *agentdef.Definitions, provider, profile string) (agentdef.AgentDef, *agentdef.ResolvedModel, bool) {
	if defs == nil {
		return agentdef.AgentDef{}, nil, false
	}
	for _, definition := range defs.Agents {
		if definition.ExecutionMode != agentdef.ExecutionModeDurableJob || definition.Model != profile {
			continue
		}
		resolved, err := defs.ResolveModel(definition.Model)
		if err != nil {
			return agentdef.AgentDef{}, nil, false
		}
		if resolved.Provider.Name == provider {
			return definition, resolved, true
		}
	}
	return agentdef.AgentDef{}, nil, false
}

var _ interface {
	ReconcileJob(context.Context, string, int) (domain.ExternalAgentJobStatusView, error)
} = (*Application)(nil)

var _ port.ExternalAgentJobRuntime = (*externalAgentJobDispatcher)(nil)
var _ port.ExternalAgentSessionRecoveryRuntime = (*recoverableExternalAgentJobDispatcher)(nil)
