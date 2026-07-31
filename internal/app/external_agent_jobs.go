package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	slackadapter "github.com/Dauno/slack-local-agent/internal/adapter/slack"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	externalagent "github.com/Dauno/slack-local-agent/internal/usecase/externalagent"
)

type acpJobDispatcher struct {
	children   []preparedAgentTool
	global     string
	store      port.ExternalAgentJobStore
	sanitize   func(string) string
	artifacts  port.ResultArtifactStore
	policy     domain.ResultDeliveryPolicy
	partLabels bool
}

type acpInvocationRecoverer interface {
	ReconcileInvocation(context.Context, domain.AcpInvocationRequest, string) (domain.AcpInvocationResult, error)
}

func (d *acpJobDispatcher) Run(ctx context.Context, job domain.ExternalAgentJob) (domain.AcpInvocationResult, error) {
	profileMatched := false
	for _, child := range d.children {
		if child.acpRuntime == nil || child.acpResolved == nil {
			continue
		}
		if job.Provider != child.acpResolved.Provider.Name || job.Profile != child.definition.Runtime {
			continue
		}
		profileMatched = true
		if job.RegistryRevision == "" || job.RegistryRevision != child.registryRevision {
			continue
		}
		primary, additional, err := resolveACPProjects(child.projectRoots, job.PrimaryProject, job.AdditionalProjects)
		if err != nil {
			return domain.AcpInvocationResult{}, err
		}
		options := make([]domain.ACPConfigOption, 0, len(child.acpResolved.ConfigOptions))
		for _, option := range child.acpResolved.ConfigOptions {
			options = append(options, domain.ACPConfigOption{ID: option.ID, Value: option.Value})
		}
		result, runErr := child.acpRuntime.Run(ctx, domain.AcpInvocationRequest{
			JobID: job.ID, PrimaryProject: job.PrimaryProject, PrimaryPath: primary,
			AdditionalProjects: append([]string(nil), job.AdditionalProjects...), AdditionalPaths: additional,
			ProfileName: job.Profile, ConfigOptions: options, PermissionOptionKind: child.acpResolved.PermissionOptionKind,
			GlobalInstruction: d.global, AgentInstruction: child.definition.Instruction, Task: job.Task,
			Timeout: time.Until(job.TimeoutAt),
			OnSessionCreated: func(sessionID string) error {
				if d.store == nil {
					return errors.New("durable ACP job store is unavailable")
				}
				return d.store.AssignACPSession(ctx, job.ID, job.LeaseOwner, job.Attempt, sessionID)
			},
			BeforePermission: func() error {
				if d.store == nil {
					return errors.New("durable ACP job store is unavailable")
				}
				return d.store.MarkSideEffectsPossible(ctx, job.ID, job.LeaseOwner, job.Attempt)
			},
			OnSideEffectsPossible: func() error {
				if d.store == nil {
					return errors.New("durable ACP job store is unavailable")
				}
				return d.store.MarkSideEffectsPossible(ctx, job.ID, job.LeaseOwner, job.Attempt)
			},
		})
		if runErr == nil && d.artifacts != nil && job.Mode == domain.JobDetached {
			result, runErr = d.materialize(ctx, job, result)
		} else if runErr == nil && d.sanitize != nil {
			result.Text = d.sanitize(result.Text)
		}
		return result, runErr
	}
	if profileMatched {
		return domain.AcpInvocationResult{}, errors.New("durable ACP job scope revision does not match current configuration")
	}
	return domain.AcpInvocationResult{}, errors.New("durable ACP job provider/profile is unavailable")
}

func (d *acpJobDispatcher) materialize(ctx context.Context, job domain.ExternalAgentJob, result domain.AcpInvocationResult) (domain.AcpInvocationResult, error) {
	if err := d.policy.Validate(); err != nil {
		return domain.AcpInvocationResult{}, err
	}
	var content []byte
	var err error
	if result.ArtifactRef != "" {
		verified, ok := d.artifacts.(port.VerifiedResultArtifactStore)
		if !ok {
			return domain.AcpInvocationResult{}, errors.New("ACP result artifact cannot be verified")
		}
		content, err = verified.Get(ctx, job.ID, result.ArtifactRef, result.ResultSHA256, d.policy.MaxResultArtifactBytes)
		if err != nil {
			return domain.AcpInvocationResult{}, &domain.ACPError{Code: domain.ACPErrorResultArtifactInvalid, Err: errors.New("verified ACP result artifact is unavailable")}
		}
	} else {
		content = []byte(result.Text)
	}
	text := string(content)
	if d.sanitize != nil {
		text = d.sanitize(text)
	}
	text, err = domain.SanitizeResultText(text)
	if err != nil {
		return domain.AcpInvocationResult{}, &domain.ACPError{Code: domain.ACPErrorResultDeliveryFailed, Err: errors.New("ACP result sanitization failed")}
	}
	safeBytes := []byte(text)
	if int64(len(safeBytes)) > d.policy.MaxFileBytes {
		return domain.AcpInvocationResult{}, &domain.ACPError{Code: domain.ACPErrorResultTooLarge, Err: errors.New("sanitized ACP result exceeds configured delivery bound")}
	}
	digest := sha256.Sum256(safeBytes)
	contentSHA := fmt.Sprintf("%x", digest)
	canonical := fmt.Sprintf("OpenCode job `%s` completed.\n\n%s", job.ID, text)
	parts := slackadapter.RenderMarkdownParts(canonical, d.partLabels)
	mode := domain.JobResultDeliveryMarkdown
	artifactRef := ""
	if result.ArtifactRef != "" || len(parts) > d.policy.MaxMarkdownParts {
		mode = domain.JobResultDeliveryFile
		ownerID := job.ID + "-delivery"
		artifact, putErr := d.artifacts.Put(ctx, ownerID, text)
		if putErr != nil {
			verified, verifiedOK := d.artifacts.(port.VerifiedResultArtifactStore)
			if !verifiedOK {
				return domain.AcpInvocationResult{}, fmt.Errorf("store sanitized result artifact: %w", putErr)
			}
			if _, readErr := verified.Get(ctx, ownerID, ownerID+".result", contentSHA, d.policy.MaxFileBytes); readErr != nil {
				return domain.AcpInvocationResult{}, fmt.Errorf("store sanitized result artifact: %w", putErr)
			}
			artifact = domain.ResultArtifact{Reference: ownerID + ".result", SHA256: contentSHA, Bytes: int64(len(safeBytes))}
		}
		if artifact.Reference == "" || !strings.EqualFold(artifact.SHA256, contentSHA) || artifact.Bytes != int64(len(safeBytes)) {
			return domain.AcpInvocationResult{}, &domain.ACPError{Code: domain.ACPErrorResultDeliveryFailed, Err: errors.New("sanitized ACP result artifact identity is invalid")}
		}
		artifactRef = artifact.Reference
	}
	result.Text = text
	result.Inline = mode == domain.JobResultDeliveryMarkdown
	result.ArtifactRef = artifactRef
	result.ResultSHA256 = contentSHA
	result.ResultBytes = int64(len(safeBytes))
	result.DeliveryMode = mode
	result.DeliveryCanonicalMarkdown = canonical
	result.DeliveryPolicyVersion = domain.JobDeliveryPolicyV1
	result.DeliveryMaxMarkdownParts = d.policy.MaxMarkdownParts
	result.DeliveryContentSHA256 = contentSHA
	result.DeliveryContentBytes = int64(len(safeBytes))
	result.DeliveryArtifactRef = artifactRef
	if mode == domain.JobResultDeliveryFile {
		// File-mode content lives only in the verified private artifact.
		result.Text = ""
	}
	return result, nil
}

func (d *acpJobDispatcher) Reconcile(ctx context.Context, job domain.ExternalAgentJob) (domain.AcpInvocationResult, error) {
	profileMatched := false
	for _, child := range d.children {
		if child.acpRuntime == nil || child.acpResolved == nil || job.Provider != child.acpResolved.Provider.Name || job.Profile != child.definition.Runtime {
			continue
		}
		profileMatched = true
		if job.RegistryRevision == "" || job.RegistryRevision != child.registryRevision {
			continue
		}
		primary, additional, err := resolveACPProjects(child.projectRoots, job.PrimaryProject, job.AdditionalProjects)
		if err != nil {
			return domain.AcpInvocationResult{}, err
		}
		recoverer, ok := child.acpRuntime.(acpInvocationRecoverer)
		if !ok {
			return domain.AcpInvocationResult{}, errors.New("session recovery is unsupported by the composed ACP runtime")
		}
		options := make([]domain.ACPConfigOption, 0, len(child.acpResolved.ConfigOptions))
		for _, option := range child.acpResolved.ConfigOptions {
			options = append(options, domain.ACPConfigOption{ID: option.ID, Value: option.Value})
		}
		result, runErr := recoverer.ReconcileInvocation(ctx, domain.AcpInvocationRequest{
			JobID: job.ID, PrimaryProject: job.PrimaryProject, PrimaryPath: primary,
			AdditionalProjects: append([]string(nil), job.AdditionalProjects...), AdditionalPaths: additional,
			ProfileName: job.Profile, ProviderName: job.Provider, ConfigOptions: options,
			GlobalInstruction: d.global, AgentInstruction: child.definition.Instruction,
			PermissionOptionKind: child.acpResolved.PermissionOptionKind, Timeout: time.Until(job.TimeoutAt),
		}, job.ACPSessionID)
		if runErr == nil && d.artifacts != nil {
			result, runErr = d.materialize(ctx, job, result)
		} else if runErr == nil && d.sanitize != nil {
			result.Text = d.sanitize(result.Text)
		}
		return result, runErr
	}
	if profileMatched {
		return domain.AcpInvocationResult{}, errors.New("durable ACP recovery scope revision does not match current configuration")
	}
	return domain.AcpInvocationResult{}, errors.New("durable ACP job provider/profile is unavailable")
}

func newExternalAgentJobService(cfg config.Config, models runtimeModels, infra *runtimeInfrastructure) (*externalagent.Service, *externalagent.NotificationWorker, error) {
	var children []preparedAgentTool
	for _, child := range models.preparedAgentTools {
		if child.acpRuntime != nil {
			children = append(children, child)
		}
	}
	if len(children) == 0 {
		return nil, nil, nil
	}
	policy := domain.ResultDeliveryPolicy{
		MaxMarkdownParts:       cfg.ACP.Delivery.MaxMarkdownParts,
		MaxFileBytes:           int64(cfg.ACP.Delivery.MaxFileBytes),
		MaxInlineResultBytes:   int64(cfg.ACP.MaxInlineResultBytes),
		MaxResultArtifactBytes: int64(cfg.ACP.MaxResultArtifactBytes),
	}
	if err := policy.Validate(); err != nil {
		return nil, nil, fmt.Errorf("validate durable ACP result delivery policy: %w", err)
	}
	store := adaptersqlite.NewExternalAgentJobStore(infra.store)
	if store == nil {
		return nil, nil, errors.New("initialize external-agent job store")
	}
	global := ""
	if models.rootDef != nil {
		global = models.rootDef.EffectiveDelegatedGlobalInstruction()
	}
	service, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Duration(cfg.ACP.DefaultJobTimeoutSeconds) * time.Second,
		MaxTimeout:     time.Duration(cfg.ACP.MaxJobTimeoutSeconds) * time.Second,
		LeaseTTL:       30 * time.Second, PollInterval: time.Second, Concurrency: cfg.ACP.WorkerConcurrency, MaxAttempts: 2,
	}, externalagent.Dependencies{
		Store: store, Runtime: &acpJobDispatcher{children: children, global: global, store: store, sanitize: models.redactor.String,
			artifacts: models.artifactStore, policy: policy, partLabels: cfg.Slack.PartLabels},
		Publisher: nil, Artifacts: models.artifactStore, MaxResultBytes: int64(cfg.ACP.MaxResultArtifactBytes),
	})
	if err != nil {
		return nil, nil, err
	}
	verifiedArtifacts, ok := models.artifactStore.(port.VerifiedResultArtifactStore)
	if !ok {
		return nil, nil, errors.New("initialize durable ACP result delivery: verified artifact store is unavailable")
	}
	uploader := slackadapter.NewGeneratedFileUploader(infra.api, infra.slackTimeout)
	notificationPublisher := slackadapter.NewDurableJobNotificationPublisher(infra.publisher, infra.history, uploader, verifiedArtifacts, store, infra.api, cfg.Slack.PartLabels)
	notificationWorker, err := externalagent.NewNotificationWorker(externalagent.NotificationConfig{PollInterval: time.Second, LeaseTTL: 30 * time.Second}, externalagent.NotificationDependencies{Store: store, Publisher: notificationPublisher, HostCompleter: service})
	if err != nil {
		return nil, nil, err
	}
	return service, notificationWorker, nil
}

func durableACPConfigured(models runtimeModels) bool {
	for _, child := range models.preparedAgentTools {
		if child.acpRuntime != nil && child.definition.ExecutionMode == agentdef.ExecutionModeDurableJob {
			return true
		}
	}
	return false
}
