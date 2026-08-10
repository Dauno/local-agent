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
	children              []preparedAgentTool
	global                string
	store                 port.ExternalAgentJobStore
	sanitize              func(string) string
	artifacts             port.ResultArtifactStore
	policy                domain.ResultDeliveryPolicy
	partLabels            bool
	reconciliationTimeout time.Duration
	progressStore         port.ExternalAgentJobProgressStore
	processRegistry       port.ACPProcessRegistry
	progressWarnAfter     time.Duration
	logger                port.Logger
	metrics               port.MetricRecorder
	progressGauge         *externalagent.ActiveProgressGauge
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
		primary, err := resolveACPProject(child.projectRoots, job.PrimaryProject)
		if err != nil {
			return domain.AcpInvocationResult{}, err
		}
		options := make([]domain.ACPConfigOption, 0, len(child.acpResolved.ConfigOptions))
		for _, option := range child.acpResolved.ConfigOptions {
			options = append(options, domain.ACPConfigOption{ID: option.ID, Value: option.Value})
		}
		recorder := d.newRecorder(job)
		onProgress := func(domain.ACPProgressEvent) {}
		if recorder != nil {
			recorder.Start(ctx)
			defer recorder.Close()
			onProgress = recorder.Record
		}
		result, runErr := child.acpRuntime.Run(ctx, domain.AcpInvocationRequest{
			JobID: job.ID, PrimaryProject: job.PrimaryProject, PrimaryPath: primary,
			ProfileName: job.Profile, ConfigOptions: options, PermissionOptionKind: child.acpResolved.PermissionOptionKind,
			GlobalInstruction: d.global, AgentInstruction: child.definition.Instruction, Task: job.Task,
			Timeout: time.Until(job.TimeoutAt),
			OnSessionCreated: func(sessionID string) error {
				if recorder != nil {
					recorder.SetSessionID(sessionID)
				}
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
			OnProgress: onProgress,
		})
		if runErr == nil && d.artifacts != nil && job.Mode == domain.JobDetached {
			result, runErr = d.materialize(ctx, job, result)
		} else if runErr == nil && job.Mode == domain.JobForeground {
			result, runErr = d.normalizeForegroundResult(result)
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

// normalizeForegroundResult produces the complete persisted identity for a
// foreground result without creating detached delivery metadata. Only the
// final post-transformation text, its exact UTF-8 byte count and lowercase
// hex SHA-256 digest are set; Delivery* fields, artifact refs and policy
// fields stay empty for the synchronous path.
func (d *acpJobDispatcher) normalizeForegroundResult(result domain.AcpInvocationResult) (domain.AcpInvocationResult, error) {
	text, size, digest, err := d.normalizeResultText(result.Text, d.policy.MaxInlineResultBytes)
	if err != nil {
		return domain.AcpInvocationResult{}, err
	}
	result.Text = text
	result.ResultBytes = size
	result.ResultSHA256 = digest
	return result, nil
}

// normalizeResultText applies the host redactor and domain control
// sanitization to produce the exact final text, its UTF-8 byte count and
// lowercase hex SHA-256 digest. maxBytes bounds the final UTF-8 bytes.
// Failures are typed and never include result content.
func (d *acpJobDispatcher) normalizeResultText(text string, maxBytes int64) (string, int64, string, error) {
	if d.sanitize != nil {
		text = d.sanitize(text)
	}
	var err error
	text, err = domain.SanitizeResultText(text)
	if err != nil {
		return "", 0, "", &domain.ACPError{Code: domain.ACPErrorResultArtifactInvalid, Err: errors.New("ACP result identity is invalid")}
	}
	size := int64(len([]byte(text)))
	if size <= 0 {
		return "", 0, "", &domain.ACPError{Code: domain.ACPErrorResultArtifactInvalid, Err: errors.New("ACP result identity is invalid")}
	}
	if size > maxBytes {
		return "", 0, "", &domain.ACPError{Code: domain.ACPErrorResultTooLarge, Err: errors.New("ACP result exceeds the configured delivery bound")}
	}
	digest := sha256.Sum256([]byte(text))
	return text, size, fmt.Sprintf("%x", digest), nil
}

// newRecorder installs the host-owned progress recorder for one job attempt.
// Monitoring is observational: recorder failure never fails the ACP prompt.
func (d *acpJobDispatcher) newRecorder(job domain.ExternalAgentJob) *externalagent.ProgressRecorder {
	if d.progressStore == nil {
		return nil
	}
	return externalagent.NewProgressRecorder(d.progressStore, d.processRegistry, nil, d.logger, d.metrics, d.progressGauge, d.progressWarnAfter, job.ID, job.LeaseOwner, job.Attempt)
}

func (d *acpJobDispatcher) materialize(ctx context.Context, job domain.ExternalAgentJob, result domain.AcpInvocationResult) (domain.AcpInvocationResult, error) {
	if err := d.policy.Validate(); err != nil {
		return domain.AcpInvocationResult{}, err
	}
	var content []byte
	var err error
	if result.ArtifactRef != "" {
		if d.artifacts == nil {
			return domain.AcpInvocationResult{}, errors.New("ACP result artifact cannot be verified")
		}
		content, err = d.artifacts.Get(ctx, job.ID, result.ArtifactRef, result.ResultSHA256, d.policy.MaxResultArtifactBytes)
		if err != nil {
			return domain.AcpInvocationResult{}, &domain.ACPError{Code: domain.ACPErrorResultArtifactInvalid, Err: errors.New("verified ACP result artifact is unavailable")}
		}
	} else {
		content = []byte(result.Text)
	}
	text := string(content)
	var size int64
	var contentSHA string
	text, size, contentSHA, err = d.normalizeResultText(text, d.policy.MaxFileBytes)
	if err != nil {
		return domain.AcpInvocationResult{}, err
	}
	canonical := fmt.Sprintf("OpenCode job `%s` completed.\n\n%s", job.ID, text)
	parts := slackadapter.RenderMarkdownParts(canonical, d.partLabels)
	mode := domain.JobResultDeliveryMarkdown
	artifactRef := ""
	if result.ArtifactRef != "" || len(parts) > d.policy.MaxMarkdownParts {
		mode = domain.JobResultDeliveryFile
		ownerID := job.ID + "-delivery"
		artifact, putErr := d.artifacts.Put(ctx, ownerID, text)
		if putErr != nil {
			if d.artifacts == nil {
				return domain.AcpInvocationResult{}, fmt.Errorf("store sanitized result artifact: %w", putErr)
			}
			if _, readErr := d.artifacts.Get(ctx, ownerID, ownerID+".result", contentSHA, d.policy.MaxFileBytes); readErr != nil {
				return domain.AcpInvocationResult{}, fmt.Errorf("store sanitized result artifact: %w", putErr)
			}
			artifact = domain.ResultArtifact{Reference: ownerID + ".result", SHA256: contentSHA, Bytes: size}
		}
		if artifact.Reference == "" || !strings.EqualFold(artifact.SHA256, contentSHA) || artifact.Bytes != size {
			return domain.AcpInvocationResult{}, &domain.ACPError{Code: domain.ACPErrorResultDeliveryFailed, Err: errors.New("sanitized ACP result artifact identity is invalid")}
		}
		artifactRef = artifact.Reference
	}
	result.Text = text
	result.Inline = mode == domain.JobResultDeliveryMarkdown
	result.ArtifactRef = artifactRef
	result.ResultSHA256 = contentSHA
	result.ResultBytes = size
	result.DeliveryMode = mode
	result.DeliveryCanonicalMarkdown = canonical
	result.DeliveryPolicyVersion = domain.JobDeliveryPolicyV1
	result.DeliveryMaxMarkdownParts = d.policy.MaxMarkdownParts
	result.DeliveryContentSHA256 = contentSHA
	result.DeliveryContentBytes = size
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
		primary, err := resolveACPProject(child.projectRoots, job.PrimaryProject)
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
		recoveryTimeout := d.reconciliationTimeout
		if recoveryTimeout <= 0 {
			recoveryTimeout = 30 * time.Minute
		}
		result, runErr := recoverer.ReconcileInvocation(ctx, domain.AcpInvocationRequest{
			JobID: job.ID, PrimaryProject: job.PrimaryProject, PrimaryPath: primary,
			ProfileName: job.Profile, ProviderName: job.Provider, ConfigOptions: options,
			GlobalInstruction: d.global, AgentInstruction: child.definition.Instruction,
			PermissionOptionKind: child.acpResolved.PermissionOptionKind, Timeout: recoveryTimeout,
		}, job.ACPSessionID)
		if runErr == nil && d.artifacts != nil && job.Mode == domain.JobDetached {
			result, runErr = d.materialize(ctx, job, result)
		} else if runErr == nil && job.Mode == domain.JobForeground {
			result, runErr = d.normalizeForegroundResult(result)
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
	maxResultChunkBytes := int64(0)
	if cfg.Context.RecoverableResults != nil {
		maxResultChunkBytes = int64(cfg.Context.RecoverableResults.ChunkMaxBytes)
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
		DefaultTimeout:         time.Duration(cfg.ACP.DefaultJobTimeoutSeconds) * time.Second,
		MaxTimeout:             time.Duration(cfg.ACP.MaxJobTimeoutSeconds) * time.Second,
		LeaseTTL:               30 * time.Second,
		PollInterval:           time.Second,
		Concurrency:            cfg.ACP.WorkerConcurrency,
		MaxAttempts:            2,
		ProgressWarningSeconds: time.Duration(cfg.ACP.ProgressWarningSeconds) * time.Second,
	}, externalagent.Dependencies{
		Store: store, Runtime: &acpJobDispatcher{children: children, global: global, store: store, sanitize: models.redactor.String,
			artifacts: models.artifactStore, policy: policy, partLabels: cfg.Slack.PartLabels,
			reconciliationTimeout: time.Duration(cfg.ACP.ReconciliationTimeoutSeconds) * time.Second,
			progressStore:         store, processRegistry: infra.processRegistry,
			progressWarnAfter: time.Duration(cfg.ACP.ProgressWarningSeconds) * time.Second,
			logger:            models.logger, metrics: models.metrics,
			progressGauge: externalagent.NewActiveProgressGauge(models.metrics)},
		Publisher: nil, Artifacts: models.artifactStore,
		ProgressStore: store, ProcessRegistry: infra.processRegistry,
		MaxResultBytes: int64(cfg.ACP.MaxResultArtifactBytes), MaxResultChunkBytes: maxResultChunkBytes,
		Logger: models.logger, Metrics: models.metrics,
	})
	if err != nil {
		return nil, nil, err
	}
	if models.artifactStore == nil {
		return nil, nil, errors.New("initialize durable ACP result delivery: verified artifact store is unavailable")
	}
	uploader := slackadapter.NewGeneratedFileUploader(infra.api, infra.slackTimeout)
	notificationPublisher := slackadapter.NewDurableJobNotificationPublisher(infra.publisher, infra.history, uploader, models.artifactStore, store, infra.api, cfg.Slack.PartLabels)
	notificationWorker, err := externalagent.NewNotificationWorker(externalagent.NotificationConfig{PollInterval: time.Second, LeaseTTL: 30 * time.Second, StuckThreshold: 5 * time.Minute}, externalagent.NotificationDependencies{Store: store, Publisher: notificationPublisher, HostCompleter: service, Logger: models.logger, Metrics: models.metrics})
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
