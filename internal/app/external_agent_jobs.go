package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/agentcli"
	slackadapter "github.com/Dauno/slack-local-agent/internal/adapter/slack"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	externalagent "github.com/Dauno/slack-local-agent/internal/usecase/externalagent"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

type externalAgentSchedules struct {
	jobs          *workpoll.Scheduler
	notifications *workpoll.Scheduler
	activations   *workpoll.Scheduler
}

func newExternalAgentSchedules() (externalAgentSchedules, error) {
	jobs, err := workpoll.New(time.Second, workpoll.Options{})
	if err != nil {
		return externalAgentSchedules{}, err
	}
	notifications, err := workpoll.New(time.Second, workpoll.Options{})
	if err != nil {
		return externalAgentSchedules{}, err
	}
	activations, err := workpoll.New(time.Second, workpoll.Options{})
	if err != nil {
		return externalAgentSchedules{}, err
	}
	return externalAgentSchedules{jobs: jobs, notifications: notifications, activations: activations}, nil
}

type externalAgentJobDispatcher struct {
	children              []preparedAgentTool
	global                string
	store                 port.ExternalAgentJobStore
	sanitize              func(string) string
	artifacts             port.ResultArtifactStore
	results               port.TrustedResultStore
	policy                domain.ResultDeliveryPolicy
	partLabels            bool
	reconciliationTimeout time.Duration
	progressStore         port.ExternalAgentJobProgressStore
	processRegistry       port.ExternalAgentProcessRegistry
	progressWarnAfter     time.Duration
	logger                port.Logger
	metrics               port.MetricRecorder
	progressGauge         *externalagent.ActiveProgressGauge
}

func (d *externalAgentJobDispatcher) Run(ctx context.Context, job domain.ExternalAgentJob) (domain.ExternalAgentInvocationResult, error) {
	profileMatched := false
	if matched, result, err := d.runAgentCLI(ctx, job, &profileMatched); matched {
		return result, err
	}
	if profileMatched {
		return domain.ExternalAgentInvocationResult{}, errors.New("durable external-agent job scope revision does not match current configuration")
	}
	return domain.ExternalAgentInvocationResult{}, errors.New("durable external-agent job provider/profile is unavailable")
}

// runAgentCLI executes a durable job whose leaf is an agent CLI.
//
// Progress comes from the descriptor's `stream.activity` selection, so a CLI
// job keeps its projection fresh and its stall warning honest. Session recovery
// is not implemented: a job left in completion_unknown must be closed by an
// operator.
func (d *externalAgentJobDispatcher) runAgentCLI(ctx context.Context, job domain.ExternalAgentJob, profileMatched *bool) (bool, domain.ExternalAgentInvocationResult, error) {
	for _, child := range d.children {
		if child.model == nil || child.cliResolved == nil {
			continue
		}
		if job.Provider != child.cliResolved.Provider.Name || job.Profile != child.definition.Model {
			continue
		}
		*profileMatched = true
		if job.RegistryRevision == "" || job.RegistryRevision != child.registryRevision {
			continue
		}
		// Side effects become possible the moment the CLI starts, because it
		// decides on its own once given an approval mode. The job is marked
		// before the process exists, never after.
		if d.store != nil {
			if err := d.store.MarkSideEffectsPossible(ctx, job.ID, job.LeaseOwner, job.Attempt); err != nil {
				return true, domain.ExternalAgentInvocationResult{}, err
			}
		}
		runCtx := ctx
		recorder := d.newRecorder(job)
		if recorder != nil {
			recorder.Start(ctx)
			defer recorder.Close()
			runCtx = agentcli.WithActivityReporter(ctx, func(activity agentcli.Activity) {
				recorder.Record(agentCLIProgressEvent(activity))
			})
		}
		text, runErr := generateAgentCLIText(runCtx, child.model, d.global, child.definition.Instruction, job.PrimaryProject, job.Task)
		if runErr != nil {
			if recorder != nil {
				recorder.Record(domain.ExternalAgentProgressEvent{
					Kind: domain.ExternalAgentEventProcessFailed, ErrorClass: externalAgentFailureClass(runErr),
				})
			}
			return true, domain.ExternalAgentInvocationResult{}, runErr
		}
		// Inline is detached-delivery metadata that only materialize may set. A
		// foreground result must carry none of it.
		result := domain.ExternalAgentInvocationResult{Text: text}
		switch {
		case job.Mode == domain.JobDetached && d.artifacts != nil:
			result, runErr = d.materialize(ctx, job, result)
		case job.Mode == domain.JobDetached && d.results != nil:
			runErr = errors.New("native result delivery store is unavailable")
		case job.Mode == domain.JobForeground:
			result, runErr = d.normalizeForegroundResult(ctx, job, result)
		case d.sanitize != nil:
			result.Text = d.sanitize(result.Text)
		}
		return true, result, runErr
	}
	return false, domain.ExternalAgentInvocationResult{}, nil
}

// agentCLIProgressEvent maps one observed agent CLI step onto the host's
// content-free progress vocabulary. A reported step is always a tool step: the
// descriptors select command execution, file changes, tool calls, and searches
// through `report_types`, and never message or reasoning text.
//
// The tool identity stays nil. A CLI reports a step that already finished, so
// there is no call to track from pending to terminal, and the projection reads
// a nil tool as "the agent did something" without altering the active count.
func agentCLIProgressEvent(activity agentcli.Activity) domain.ExternalAgentProgressEvent {
	if activity.Kind == agentcli.ActivityProcessStarted {
		return domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventProcessStarted, PID: activity.PID}
	}
	return domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventToolCall}
}

// externalAgentFailureClass is the bounded classification the progress projection stores
// for a failed run. It never carries the error text.
func externalAgentFailureClass(err error) string {
	var externalAgentErr *domain.ExternalAgentError
	if errors.As(err, &externalAgentErr) && externalAgentErr.Code != "" {
		return string(externalAgentErr.Code)
	}
	return string(domain.ExternalAgentErrorProcessExit)
}

// normalizeForegroundResult produces the complete persisted identity for a
// foreground result without creating detached delivery metadata. Only the
// final post-transformation text, its exact UTF-8 byte count and lowercase
// hex SHA-256 digest are set; Delivery* fields, artifact refs and policy
// fields stay empty for the synchronous path.
func (d *externalAgentJobDispatcher) normalizeForegroundResult(ctx context.Context, job domain.ExternalAgentJob, result domain.ExternalAgentInvocationResult) (domain.ExternalAgentInvocationResult, error) {
	text, size, digest, err := d.normalizeResultText(result.Text, d.policy.MaxInlineResultBytes)
	if err != nil {
		return domain.ExternalAgentInvocationResult{}, err
	}
	result.Text = text
	result.ArtifactRef = ""
	result.DeliveryArtifactRef = ""
	result.ResultBytes = size
	result.ResultSHA256 = digest
	result.NativeResultID, err = d.materializeNativeResult(ctx, job, text, size, digest)
	if err != nil {
		return result, err
	}
	return result, nil
}

// materializeNativeResult commits the complete sanitized external-agent payload before
// the caller can transition its job to a terminal status or create delivery
// metadata. The terminal status revision is exactly one past the leased job
// snapshot supplied to this runtime invocation.
func (d *externalAgentJobDispatcher) materializeNativeResult(ctx context.Context, job domain.ExternalAgentJob, text string, size int64, digest string) (string, error) {
	if d.results == nil {
		return "", nil
	}
	if job.StatusRevision < 0 {
		return "", &domain.ExternalAgentError{Code: domain.ExternalAgentErrorResultArtifactInvalid, Err: errors.New("external-agent result producer revision is invalid")}
	}
	retention := domain.ResultRetentionContext
	if job.Mode == domain.JobDetached {
		retention = domain.ResultRetentionConversation
	}
	handle, err := d.results.Materialize(ctx, port.ResultMaterialization{
		Producer: domain.ResultProducer{Kind: domain.ResultProducerExternalAgentJob, ID: job.ID, Revision: job.StatusRevision + 1},
		Payload:  text, Scope: domain.ResultScope{Actor: job.Actor, TeamID: job.TeamID, ConversationKey: string(job.ConversationKey), Project: job.PrimaryProject},
		Retention: retention, MediaType: "text/plain; charset=utf-8",
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", err
	}
	if err != nil || handle.Validate() != nil || handle.SHA256 != digest || handle.Bytes != size || !hasArtifactAvailability(handle.Availability) {
		return "", &domain.ExternalAgentError{Code: domain.ExternalAgentErrorResultArtifactInvalid, Err: errors.New("native external-agent result materialization failed")}
	}
	return handle.ResultID, nil
}

func hasArtifactAvailability(values []domain.ResultAvailability) bool {
	return slices.Contains(values, domain.ResultAvailabilityPrivateArtifact)
}

// normalizeResultText applies the host redactor and domain control
// sanitization to produce the exact final text, its UTF-8 byte count and
// lowercase hex SHA-256 digest. maxBytes bounds the final UTF-8 bytes.
// Failures are typed and never include result content.
func (d *externalAgentJobDispatcher) normalizeResultText(text string, maxBytes int64) (string, int64, string, error) {
	if d.sanitize != nil {
		text = d.sanitize(text)
	}
	var err error
	text, err = domain.SanitizeResultText(text)
	if err != nil {
		return "", 0, "", &domain.ExternalAgentError{Code: domain.ExternalAgentErrorResultArtifactInvalid, Err: errors.New("external-agent result identity is invalid")}
	}
	size := int64(len([]byte(text)))
	if size <= 0 {
		return "", 0, "", &domain.ExternalAgentError{Code: domain.ExternalAgentErrorResultArtifactInvalid, Err: errors.New("external-agent result identity is invalid")}
	}
	if size > maxBytes {
		return "", 0, "", &domain.ExternalAgentError{Code: domain.ExternalAgentErrorResultTooLarge, Err: errors.New("external-agent result exceeds the configured delivery bound")}
	}
	digest := sha256.Sum256([]byte(text))
	return text, size, fmt.Sprintf("%x", digest), nil
}

// newRecorder installs the host-owned progress recorder for one job attempt.
// Monitoring is observational: recorder failure never fails the agent prompt.
func (d *externalAgentJobDispatcher) newRecorder(job domain.ExternalAgentJob) *externalagent.ProgressRecorder {
	if d.progressStore == nil {
		return nil
	}
	return externalagent.NewProgressRecorder(d.progressStore, d.processRegistry, nil, d.logger, d.metrics, d.progressGauge, d.progressWarnAfter, job.ID, job.LeaseOwner, job.Attempt)
}

func (d *externalAgentJobDispatcher) materialize(ctx context.Context, job domain.ExternalAgentJob, result domain.ExternalAgentInvocationResult) (domain.ExternalAgentInvocationResult, error) {
	if err := d.policy.Validate(); err != nil {
		return domain.ExternalAgentInvocationResult{}, err
	}
	var content []byte
	var err error
	if result.ArtifactRef != "" {
		if d.artifacts == nil {
			return domain.ExternalAgentInvocationResult{}, errors.New("external-agent result artifact cannot be verified")
		}
		content, err = d.artifacts.Get(ctx, job.ID, result.ArtifactRef, result.ResultSHA256, d.policy.MaxResultArtifactBytes)
		if err != nil {
			return domain.ExternalAgentInvocationResult{}, &domain.ExternalAgentError{Code: domain.ExternalAgentErrorResultArtifactInvalid, Err: errors.New("verified external-agent result artifact is unavailable")}
		}
	} else {
		content = []byte(result.Text)
	}
	text := string(content)
	// Provider/source artifacts never become delivery bindings. From this point
	// onward only host-created native and delivery identities may survive.
	result.ArtifactRef = ""
	result.DeliveryArtifactRef = ""
	var size int64
	var contentSHA string
	text, size, contentSHA, err = d.normalizeResultText(text, d.policy.MaxFileBytes)
	if err != nil {
		return domain.ExternalAgentInvocationResult{}, err
	}
	result.Text = text
	result.ResultSHA256 = contentSHA
	result.ResultBytes = size
	result.NativeResultID, err = d.materializeNativeResult(ctx, job, text, size, contentSHA)
	if err != nil {
		return result, err
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
			if _, readErr := d.artifacts.Get(ctx, ownerID, ownerID+".result", contentSHA, d.policy.MaxFileBytes); readErr != nil {
				return result, fmt.Errorf("store sanitized result artifact: %w", putErr)
			}
			artifact = domain.ResultArtifact{Reference: ownerID + ".result", SHA256: contentSHA, Bytes: size}
		}
		if artifact.Reference == "" || !strings.EqualFold(artifact.SHA256, contentSHA) || artifact.Bytes != size {
			return result, &domain.ExternalAgentError{Code: domain.ExternalAgentErrorResultDeliveryFailed, Err: errors.New("sanitized external-agent result artifact identity is invalid")}
		}
		artifactRef = artifact.Reference
	}
	result.Inline = mode == domain.JobResultDeliveryMarkdown
	result.ArtifactRef = artifactRef
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

// Reconcile resumes an agent's session to learn what happened after a crash.
// No runtime in this release captures a session, so a completion-unknown job
// must be closed by an operator. The `session` descriptor block exists for
// exactly this and is not wired yet.
func (d *externalAgentJobDispatcher) Reconcile(_ context.Context, job domain.ExternalAgentJob) (domain.ExternalAgentInvocationResult, error) {
	return domain.ExternalAgentInvocationResult{}, fmt.Errorf(
		"session recovery is not supported for job %q: inspect the workspace and close the job explicitly", job.ID)
}

func newExternalAgentJobService(cfg config.Config, models runtimeModels, infra *runtimeInfrastructure, supplied ...externalAgentSchedules) (*externalagent.Service, *externalagent.NotificationWorker, error) {
	children := externalAgentChildren(models.preparedAgentTools)
	if len(children) == 0 {
		return nil, nil, nil
	}
	var schedules externalAgentSchedules
	if len(supplied) > 0 {
		schedules = supplied[0]
	} else {
		var err error
		schedules, err = newExternalAgentSchedules()
		if err != nil {
			return nil, nil, err
		}
	}
	policy := domain.ResultDeliveryPolicy{
		MaxMarkdownParts:       cfg.ExternalAgent.Delivery.MaxMarkdownParts,
		MaxFileBytes:           int64(cfg.ExternalAgent.Delivery.MaxFileBytes),
		MaxInlineResultBytes:   int64(cfg.ExternalAgent.MaxInlineResultBytes),
		MaxResultArtifactBytes: int64(cfg.ExternalAgent.MaxResultArtifactBytes),
	}
	if err := policy.Validate(); err != nil {
		return nil, nil, fmt.Errorf("validate durable external-agent result delivery policy: %w", err)
	}
	maxResultChunkBytes := int64(0)
	if cfg.Context.RecoverableResults != nil {
		maxResultChunkBytes = int64(cfg.Context.RecoverableResults.ChunkMaxBytes)
	}
	store := adaptersqlite.NewExternalAgentJobStore(infra.store)
	if store == nil {
		return nil, nil, errors.New("initialize external-agent job store")
	}
	var nativeResults port.TrustedResultStore
	var err error
	if cfg.Orchestration.ResultHandles.Enabled {
		if models.resultPayloadStore == nil {
			return nil, nil, errors.New("initialize native external-agent result payload store")
		}
		nativeResults, err = adaptersqlite.NewResultStore(infra.store, models.resultPayloadStore)
		if err != nil {
			return nil, nil, fmt.Errorf("initialize native external-agent result store: %w", err)
		}
	}
	global := ""
	if models.rootDef != nil {
		global = models.rootDef.EffectiveDelegatedGlobalInstruction()
	}
	service, err := externalagent.New(externalagent.Config{
		DefaultTimeout:         time.Duration(cfg.ExternalAgent.DefaultJobTimeoutSeconds) * time.Second,
		MaxTimeout:             time.Duration(cfg.ExternalAgent.MaxJobTimeoutSeconds) * time.Second,
		LeaseTTL:               30 * time.Second,
		PollInterval:           time.Second,
		Concurrency:            cfg.ExternalAgent.WorkerConcurrency,
		MaxAttempts:            2,
		ProgressWarningTimeout: time.Duration(cfg.ExternalAgent.ProgressWarningSeconds) * time.Second,
	}, externalagent.Dependencies{
		Store: store, Runtime: &externalAgentJobDispatcher{children: children, global: global, store: store, sanitize: models.redactor.String, results: nativeResults,
			artifacts: models.artifactStore, policy: policy, partLabels: cfg.Slack.PartLabels,
			reconciliationTimeout: time.Duration(cfg.ExternalAgent.ReconciliationTimeoutSeconds) * time.Second,
			progressStore:         store, processRegistry: infra.processRegistry,
			progressWarnAfter: time.Duration(cfg.ExternalAgent.ProgressWarningSeconds) * time.Second,
			logger:            models.logger, metrics: models.metrics,
			progressGauge: externalagent.NewActiveProgressGauge(models.metrics)},
		Publisher: nil, Artifacts: models.artifactStore, NativeResults: nativeResults,
		ProgressStore: store, ProcessRegistry: infra.processRegistry,
		MaxResultBytes: int64(cfg.ExternalAgent.MaxResultArtifactBytes), MaxResultChunkBytes: maxResultChunkBytes,
		Logger: models.logger, Metrics: models.metrics,
		Scheduler: schedules.jobs, JobWake: schedules.jobs.Wake, NotificationWake: schedules.notifications.Wake,
	})
	if err != nil {
		return nil, nil, err
	}
	if models.artifactStore == nil {
		return nil, nil, errors.New("initialize durable external-agent result delivery: verified artifact store is unavailable")
	}
	uploader := slackadapter.NewGeneratedFileUploader(infra.api, infra.slackTimeout)
	notificationPublisher := slackadapter.NewDurableJobNotificationPublisher(infra.publisher, infra.history, uploader, models.artifactStore, store, infra.api, cfg.Slack.PartLabels)
	notificationWorker, err := externalagent.NewNotificationWorker(externalagent.NotificationConfig{PollInterval: time.Second, LeaseTTL: 30 * time.Second, StuckThreshold: 5 * time.Minute}, externalagent.NotificationDependencies{Store: store, Publisher: notificationPublisher, HostCompleter: service, Logger: models.logger, Metrics: models.metrics, Scheduler: schedules.notifications, ActivationWake: schedules.activations.Wake})
	if err != nil {
		return nil, nil, err
	}
	return service, notificationWorker, nil
}

// newExternalAgentActivationWorker builds the root-activation consumer
// composeRuntime wires over schedules.activations. It selects that
// scheduler from the full bundle itself, so a composition test can call
// the identical production construction instead of reproducing it, and so
// a regression that swaps in the wrong scheduler here is caught by that
// test rather than only by composeRuntime itself.
func newExternalAgentActivationWorker(store port.ExternalAgentJobActivationStore, handler port.ExternalAgentJobCompletionHandler, logger port.Logger, metrics port.MetricRecorder, schedules externalAgentSchedules) (*externalagent.ActivationWorker, error) {
	return externalagent.NewActivationWorker(externalagent.ActivationConfig{
		PollInterval: time.Second, LeaseTTL: 30 * time.Second, StuckThreshold: 5 * time.Minute,
	}, externalagent.ActivationDependencies{
		Store: store, Handler: handler, Logger: logger, Metrics: metrics, Scheduler: schedules.activations,
	})
}

// externalAgentChildren selects the leaves the durable worker can dispatch. An
// external-agent child carries a model plus its resolved provider. Dropping one
// here leaves its enqueued jobs with no runtime able to claim them, and the
// worker fails them instead.
func externalAgentChildren(prepared []preparedAgentTool) []preparedAgentTool {
	children := make([]preparedAgentTool, 0, len(prepared))
	for _, child := range prepared {
		if child.cliResolved != nil {
			children = append(children, child)
		}
	}
	return children
}

// durableExternalAgentConfigured reports whether any external-agent leaf runs
// as a durable job, which is what needs the Slack delivery scope.
func durableExternalAgentConfigured(models runtimeModels) bool {
	for _, child := range models.preparedAgentTools {
		if child.definition.ExecutionMode == agentdef.ExecutionModeDurableJob && child.cliResolved != nil {
			return true
		}
	}
	return false
}
