package app

import (
	"context"
	"errors"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type synchronousExternalAgentJobs interface {
	StartAndWait(context.Context, domain.ExternalAgentJobRequest) (domain.AcpInvocationResult, error)
}

type foregroundExternalAgentRuntime struct {
	direct port.ExternalAgentRuntime
	jobs   synchronousExternalAgentJobs
}

func newForegroundExternalAgentRuntime(direct port.ExternalAgentRuntime, jobs synchronousExternalAgentJobs) port.ExternalAgentRuntime {
	return &foregroundExternalAgentRuntime{direct: direct, jobs: jobs}
}

func (r *foregroundExternalAgentRuntime) Run(ctx context.Context, request domain.AcpInvocationRequest) (domain.AcpInvocationResult, error) {
	if r == nil || r.direct == nil {
		return domain.AcpInvocationResult{}, errors.New("foreground ACP runtime is not configured")
	}
	// Worker dispatch carries JobID and must reach the direct client, otherwise
	// the durable worker would recursively create another foreground job.
	if request.JobID != "" || r.jobs == nil || request.Actor == "" || request.ConversationKey == "" {
		return r.direct.Run(ctx, request)
	}
	originalCallID := request.OriginalCallID
	if originalCallID == "" {
		originalCallID = request.JobID
	}
	provider := request.ProviderName
	if provider == "" {
		provider, _, _ = strings.Cut(request.ProfileName, "/")
	}
	return r.jobs.StartAndWait(ctx, domain.ExternalAgentJobRequest{
		Provider: provider, Profile: request.ProfileName,
		PrimaryProject: request.PrimaryProject, AdditionalProjects: request.AdditionalProjects,
		RegistryRevision: request.RegistryRevision, Task: request.Task, Mode: domain.JobForeground,
		PermissionOptionKind: request.PermissionOptionKind, Timeout: request.Timeout,
		PrimaryPath: request.PrimaryPath, AdditionalPaths: request.AdditionalPaths,
		WrapperCallID: originalCallID, OriginalCallID: originalCallID,
		Actor: request.Actor, TeamID: request.TeamID, ConversationKey: request.ConversationKey,
	})
}

func (r *foregroundExternalAgentRuntime) Probe(ctx context.Context, primaryPath string, additionalPaths []string, options []domain.ACPConfigOption) error {
	return r.direct.Probe(ctx, primaryPath, additionalPaths, options)
}

func (r *foregroundExternalAgentRuntime) Describe(ctx context.Context) (domain.ACPInitResult, error) {
	return r.direct.Describe(ctx)
}

func (r *foregroundExternalAgentRuntime) ReconcileInvocation(ctx context.Context, request domain.AcpInvocationRequest, sessionID string) (domain.AcpInvocationResult, error) {
	recoverer, ok := r.direct.(interface {
		ReconcileInvocation(context.Context, domain.AcpInvocationRequest, string) (domain.AcpInvocationResult, error)
	})
	if !ok {
		return domain.AcpInvocationResult{}, errors.New("session recovery is unsupported by the ACP runtime")
	}
	return recoverer.ReconcileInvocation(ctx, request, sessionID)
}

func (r *foregroundExternalAgentRuntime) setJobRunner(jobs synchronousExternalAgentJobs) {
	r.jobs = jobs
}
