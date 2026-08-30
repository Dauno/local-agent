package externalagent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

type Config struct {
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	LeaseTTL       time.Duration
	PollInterval   time.Duration
	Concurrency    int
	MaxAttempts    int
	// ProgressWarningTimeout controls passive health derivation and one-warning
	// per silent-episode emission. It never cancels or mutates a job.
	ProgressWarningTimeout time.Duration
}

type Dependencies struct {
	Store         port.ExternalAgentJobStore
	Runtime       port.ExternalAgentJobRuntime
	Publisher     port.ExternalAgentJobPublisher
	Artifacts     port.ResultArtifactStore
	NativeResults port.TrustedResultStore
	// ProgressStore and ProcessRegistry are optional live-observability
	// dependencies. Without them, status projection reports no live fields.
	ProgressStore   port.ExternalAgentJobProgressStore
	ProcessRegistry port.ExternalAgentProcessRegistry
	// MaxResultBytes bounds host-completion reads. The artifact adapter applies
	// its own bound as a second, independent check.
	MaxResultBytes int64
	// MaxResultChunkBytes bounds each host-completion read. A zero value uses
	// the same 16 KiB default as recoverable result readers.
	MaxResultChunkBytes int64
	Clock               port.Clock
	Logger              port.Logger
	Metrics             port.MetricRecorder
	Scheduler           *workpoll.Scheduler
	JobWake             func()
	NotificationWake    func()
}

type Service struct {
	cfg                 Config
	store               port.ExternalAgentJobStore
	runtime             port.ExternalAgentJobRuntime
	publisher           port.ExternalAgentJobPublisher
	artifacts           port.ResultArtifactStore
	nativeResults       port.TrustedResultStore
	progressStore       port.ExternalAgentJobProgressStore
	processRegistry     port.ExternalAgentProcessRegistry
	maxResultBytes      int64
	maxResultChunkBytes int64
	clock               port.Clock
	logger              port.Logger
	metrics             port.MetricRecorder
	scheduler           *workpoll.Scheduler
	jobWake             func()
	notificationWake    func()
	stopClaims          chan struct{}
	stopOnce            sync.Once
	admissionMu         sync.RWMutex
	stopped             chan struct{}
}

var (
	_ port.ExternalAgentJobReader             = (*Service)(nil)
	_ port.ExternalAgentJobWrapperReader      = (*Service)(nil)
	_ port.ExternalAgentJobNativeResultReader = (*Service)(nil)
	_ port.ExternalAgentJobActivationReader   = (*Service)(nil)
	_ port.ExternalAgentJobHostCompleter      = (*Service)(nil)
)

const (
	defaultResultChunkBytes       int64 = 16 * 1024
	foregroundCancellationTimeout       = 5 * time.Second
)

func New(cfg Config, deps Dependencies) (*Service, error) {
	if cfg.DefaultTimeout <= 0 || cfg.MaxTimeout < cfg.DefaultTimeout || cfg.LeaseTTL <= 0 || cfg.PollInterval <= 0 || cfg.Concurrency <= 0 || cfg.MaxAttempts <= 0 {
		return nil, errors.New("external-agent job settings are invalid")
	}
	if deps.Store == nil || deps.Runtime == nil {
		return nil, errors.New("external-agent job store and runtime are required")
	}
	if deps.Clock == nil {
		deps.Clock = port.SystemClock{}
	}
	if deps.MaxResultBytes <= 0 || deps.MaxResultBytes > domain.MaxExternalAgentResultBytes {
		deps.MaxResultBytes = domain.MaxExternalAgentResultBytes
	}
	if deps.MaxResultChunkBytes <= 0 || deps.MaxResultChunkBytes > deps.MaxResultBytes {
		deps.MaxResultChunkBytes = min(defaultResultChunkBytes, deps.MaxResultBytes)
	}
	if cfg.ProgressWarningTimeout <= 0 {
		cfg.ProgressWarningTimeout = 900 * time.Second
	}
	logger := deps.Logger
	if logger == nil {
		logger = noopLogger{}
	}
	metrics := deps.Metrics
	if metrics == nil {
		metrics = port.NoopMetricRecorder{}
	}
	if deps.Scheduler == nil {
		deps.Scheduler, _ = workpoll.New(cfg.PollInterval, workpoll.Options{})
	}
	if deps.JobWake == nil {
		deps.JobWake = deps.Scheduler.Wake
	}
	return &Service{
		cfg: cfg, store: deps.Store, runtime: deps.Runtime, publisher: deps.Publisher,
		artifacts: deps.Artifacts, nativeResults: deps.NativeResults, progressStore: deps.ProgressStore, processRegistry: deps.ProcessRegistry,
		maxResultBytes: deps.MaxResultBytes, maxResultChunkBytes: deps.MaxResultChunkBytes,
		clock: deps.Clock, logger: logger, metrics: metrics, scheduler: deps.Scheduler, jobWake: deps.JobWake, notificationWake: deps.NotificationWake,
		stopClaims: make(chan struct{}), stopped: make(chan struct{}),
	}, nil
}

func (s *Service) Start(ctx context.Context, request domain.ExternalAgentJobRequest) (*domain.ExternalAgentJob, error) {
	if stringsEmpty(request.Provider, request.Profile, request.PrimaryProject, request.Task, request.Actor, request.TeamID, string(request.ConversationKey)) {
		return nil, errors.New("external-agent job request is missing required fields")
	}
	if _, err := domain.ConversationReplyTarget(request.ConversationKey); err != nil {
		return nil, fmt.Errorf("external-agent job conversation binding is invalid: %w", err)
	}
	if utf8.RuneCountInString(request.Task) > domain.MaxExternalAgentTaskRunes {
		return nil, errors.New("external-agent task exceeds the configured character budget")
	}
	if request.Mode != domain.JobForeground && request.Mode != domain.JobDetached {
		return nil, fmt.Errorf("unsupported external-agent job mode %q", request.Mode)
	}
	timeout := request.Timeout
	if timeout <= 0 {
		timeout = s.cfg.DefaultTimeout
	}
	if timeout > s.cfg.MaxTimeout {
		return nil, errors.New("external-agent job timeout exceeds administrative maximum")
	}
	workstreamID, taskID, executionIdentity, admissionRevision := request.WorkstreamID, request.TaskID, request.ExecutionIdentity, request.AdmissionRevision
	if err := domain.ValidateCompletionBinding(workstreamID, taskID, executionIdentity, admissionRevision); err != nil {
		// A detached job with no exact trusted binding still uses the
		// conversation completion route. Never let a partial model value
		// become workstream authority.
		workstreamID, taskID, executionIdentity, admissionRevision = "", "", "", 0
	}
	now := s.clock.Now().UTC()
	completionPolicy := domain.ExternalAgentCompletionDeliveryOnly
	if request.Mode == domain.JobDetached {
		completionPolicy = domain.ExternalAgentCompletionAutomaticRoot
	}
	job := domain.ExternalAgentJob{
		ID: "job_" + randomID(), Mode: request.Mode, CompletionPolicy: completionPolicy, Provider: request.Provider, Profile: request.Profile,
		PrimaryProject: request.PrimaryProject, RegistryRevision: request.RegistryRevision,
		Task: request.Task, RequestSHA256: domain.ExternalAgentJobRequestDigest(request),
		WrapperCallID: request.WrapperCallID, OriginalCallID: request.OriginalCallID, Actor: request.Actor, TeamID: request.TeamID,
		ConversationKey: request.ConversationKey, WorkstreamID: workstreamID, TaskID: taskID,
		ExecutionIdentity: executionIdentity, AdmissionRevision: admissionRevision,
		Status: domain.JobQueued, TimeoutAt: now.Add(timeout), CreatedAt: now, UpdatedAt: now,
	}
	if job.OriginalCallID == "" {
		job.OriginalCallID = job.ID
	}
	created, existing, err := s.store.CreateIfAbsent(ctx, job)
	if err != nil {
		return nil, fmt.Errorf("queue external-agent job: %w", err)
	}
	if !created {
		if existing == nil {
			return nil, errors.New("external-agent request identity already exists but job is unavailable")
		}
		return existing, nil
	}
	s.jobWake()
	return &job, nil
}

func (s *Service) StartAndWait(ctx context.Context, request domain.ExternalAgentJobRequest) (domain.ExternalAgentInvocationResult, error) {
	job, err := s.Start(ctx, request)
	if err != nil {
		return domain.ExternalAgentInvocationResult{}, err
	}
	for {
		current, err := s.store.GetJob(ctx, job.ID)
		if err != nil {
			return domain.ExternalAgentInvocationResult{}, err
		}
		if current == nil {
			return domain.ExternalAgentInvocationResult{}, errors.New("external-agent job disappeared")
		}
		switch current.Status {
		case domain.JobCompleted:
			return s.verifiedForegroundResult(ctx, current, request.Actor, request.ConversationKey)
		case domain.JobFailed, domain.JobCancelled, domain.JobAbandoned, domain.JobCompletionUnknown:
			if current.ErrorCode == "" {
				return domain.ExternalAgentInvocationResult{}, fmt.Errorf("external-agent job ended with status %s", current.Status)
			}
			return domain.ExternalAgentInvocationResult{}, fmt.Errorf("external-agent job ended with status %s: %s", current.Status, current.ErrorCode)
		}
		if !current.TimeoutAt.After(s.clock.Now().UTC()) {
			// A queued foreground job can outlive its total budget. Cancellation is
			// best effort, but must not keep the synchronous caller past that budget.
			if current.Status == domain.JobQueued {
				s.cancelForegroundAsync(ctx, job.ID, request.Actor)
			}
			return domain.ExternalAgentInvocationResult{}, errors.New("external-agent job ended with status failed: acp_job_timeout")
		}
		timer := time.NewTimer(s.cfg.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			s.cancelForegroundAsync(ctx, job.ID, request.Actor)
			return domain.ExternalAgentInvocationResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// cancelForegroundAsync preserves the durable cancellation attempt after the
// foreground context ends without allowing a blocked store transaction to hold
// the synchronous caller. A queued job that is not cancelled remains guarded by
// TimeoutAt before the worker can invoke the external runtime.
func (s *Service) cancelForegroundAsync(ctx context.Context, jobID, actor string) {
	go func() {
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), foregroundCancellationTimeout)
		defer cancel()
		_, _ = s.Cancel(cancelCtx, jobID, actor)
	}()
}

func (s *Service) Cancel(ctx context.Context, jobID, actor string) (*domain.ExternalAgentJob, error) {
	return s.requestCancellation(ctx, jobID, actor)
}

// verifiedForegroundResult maps a completed row to the synchronous invocation
// shape only after the row passes the request binding and the same verified
// identity as the durable readers. Content with an incomplete or tampered
// identity is never returned.
func (s *Service) verifiedForegroundResult(ctx context.Context, job *domain.ExternalAgentJob, actor string, conversationKey domain.ConversationKey) (domain.ExternalAgentInvocationResult, error) {
	if job.Actor != actor || job.ConversationKey != conversationKey {
		return domain.ExternalAgentInvocationResult{}, errors.New("external-agent job operation is not authorized")
	}
	verified, err := s.verifiedResultForJob(ctx, job)
	if err != nil {
		return domain.ExternalAgentInvocationResult{}, err
	}
	if s.nativeResults != nil {
		handle, err := s.nativeResultHandle(ctx, job)
		if err != nil {
			return domain.ExternalAgentInvocationResult{}, err
		}
		return domain.ExternalAgentInvocationResult{
			NativeResultID: handle.ResultID, NativeJobID: job.ID, NativeResultHandle: handle,
			ResultSHA256: handle.SHA256, ResultBytes: handle.Bytes,
		}, nil
	}
	return domain.ExternalAgentInvocationResult{
		Text:         verified.Text,
		Inline:       job.ResultArtifact == "",
		ArtifactRef:  job.ResultArtifact,
		ResultSHA256: job.ResultSHA256,
		ResultBytes:  job.ResultBytes,
	}, nil
}

// NativeResultHandleForJob returns only bounded metadata for V2 jobs. A
// service without a native store is an intentional legacy-only runtime.
func (s *Service) NativeResultHandleForJob(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey) (domain.ResultHandle, bool, error) {
	job, err := s.Status(ctx, jobID, actor, conversationKey)
	if err != nil {
		return domain.ResultHandle{}, false, err
	}
	if s.nativeResults == nil {
		return domain.ResultHandle{}, false, nil
	}
	handle, err := s.nativeResultHandle(ctx, job)
	return handle, true, err
}

func (s *Service) nativeResultHandle(ctx context.Context, job *domain.ExternalAgentJob) (domain.ResultHandle, error) {
	if job == nil || job.Status != domain.JobCompleted || s.nativeResults == nil {
		return domain.ResultHandle{}, domain.ErrResultUnavailable
	}
	bindingStore, ok := s.store.(port.ExternalAgentJobNativeResultStore)
	if !ok {
		return domain.ResultHandle{}, domain.ErrResultUnavailable
	}
	resultID, err := bindingStore.NativeResultIDForJob(ctx, job.ID, job.StatusRevision)
	if err != nil {
		return domain.ResultHandle{}, err
	}
	identity, handle, err := s.nativeResults.Resolve(ctx, resultID, domain.ResultScope{
		Actor: job.Actor, TeamID: job.TeamID, ConversationKey: string(job.ConversationKey), Project: job.PrimaryProject,
	})
	if err != nil {
		return domain.ResultHandle{}, err
	}
	if identity.Producer.Kind != domain.ResultProducerExternalAgentJob || identity.Producer.ID != job.ID || identity.Producer.Revision != job.StatusRevision ||
		handle.SHA256 != job.ResultSHA256 || handle.Bytes != job.ResultBytes {
		return domain.ResultHandle{}, domain.ErrResultUnavailable
	}
	return handle, nil
}

func (s *Service) Status(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey) (*domain.ExternalAgentJob, error) {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(actor) == "" || strings.TrimSpace(string(conversationKey)) == "" {
		return nil, errors.New("external-agent job operation binding is required")
	}
	job, err := s.store.GetJob(ctx, jobID)
	if err != nil || job == nil {
		return job, err
	}
	actorMatch := job.Actor == actor
	conversationMatch := job.ConversationKey == conversationKey
	s.metrics.AddCounter(domain.MetricExternalAgentStatusAuthorizationTotal, 1, port.MetricLabels{
		"actor_match":        strconv.FormatBool(actorMatch),
		"conversation_match": strconv.FormatBool(conversationMatch),
	})
	if !actorMatch || !conversationMatch {
		s.logger.Warn("external-agent job authorization mismatch", "job_id", job.ID, "actor_match", actorMatch, "conversation_match", conversationMatch)
		return nil, errors.New("external-agent job operation is not authorized")
	}
	return job, nil
}

// StatusByWrapperCallID resolves and authorizes the job referenced by a
// confirmation button without exposing an unbound job lookup to Slack.
func (s *Service) StatusByWrapperCallID(ctx context.Context, wrapperCallID, actor string, conversationKey domain.ConversationKey) (*domain.ExternalAgentJob, error) {
	if strings.TrimSpace(wrapperCallID) == "" || utf8.RuneCountInString(wrapperCallID) > 2048 ||
		strings.TrimSpace(actor) == "" || !domain.ValidKnowledgeConversationKey(conversationKey) {
		return nil, errors.New("external-agent job operation binding is required")
	}
	wrapperStore, ok := s.store.(port.ExternalAgentJobWrapperStore)
	if !ok {
		return nil, errors.New("external-agent job wrapper lookup is unavailable")
	}
	job, err := wrapperStore.GetJobByWrapperCallID(ctx, wrapperCallID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, errors.New("external-agent job was not found")
	}
	if job.Actor != actor || job.ConversationKey != conversationKey {
		return nil, errors.New("external-agent job operation is not authorized")
	}
	return job, nil
}

// StatusAtRevision returns the authorized job only when it still represents
// the terminal snapshot captured by an activation.
func (s *Service) StatusAtRevision(
	ctx context.Context,
	jobID, actor string,
	conversationKey domain.ConversationKey,
	expectedRevision int,
	expectedStatus domain.ExternalAgentJobStatus,
) (*domain.ExternalAgentJob, error) {
	job, err := s.Status(ctx, jobID, actor, conversationKey)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, errors.New("external-agent job was not found")
	}
	if job.StatusRevision != expectedRevision || job.Status != expectedStatus {
		return nil, errors.New("external-agent job revision is no longer current")
	}
	return job, nil
}

// StatusProjection returns the authorized status view merged with the
// content-free live external-agent projection and read-time health. It never opens or
// probes the external-agent session. A missing progress store yields the plain view.
func (s *Service) StatusProjection(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey) (*domain.ExternalAgentJobStatusView, error) {
	job, err := s.Status(ctx, jobID, actor, conversationKey)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, errors.New("external-agent job was not found")
	}
	view := job.StatusView()
	if s.progressStore == nil {
		return &view, nil
	}
	progress, err := s.progressStore.ReadJobProgress(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if progress == nil {
		return &view, nil
	}
	var alive *bool
	if s.processRegistry != nil {
		alive = s.processRegistry.ProcessAlive(jobID, progress.Attempt)
	}
	now := s.clock.Now().UTC()
	view.Health = DeriveProgressHealth(*progress, now, s.cfg.ProgressWarningTimeout, alive, isTerminalStatus(job.Status))
	view.Phase = progress.Phase
	view.LastEventKind = progress.LastEventKind
	view.LastTransportActivityAt = progress.LastTransportActivityAt
	view.LastSessionUpdateAt = progress.LastSessionUpdateAt
	view.LastMeaningfulProgressAt = progress.LastMeaningfulProgressAt
	view.ActiveToolCount = progress.ActiveToolCount
	view.PendingPermission = progress.PendingPermission
	view.StopReason = progress.StopReason
	view.ErrorClass = progress.ErrorClass
	view.ProcessAlive = alive
	if !progress.PromptStartedAt.IsZero() {
		elapsed := max(now.Sub(progress.PromptStartedAt), 0)
		view.PromptElapsedSeconds = int64(elapsed / time.Second)
	}
	return &view, nil
}

// ReadResult returns the complete sanitized result for an authorized,
// completed job. It re-verifies inline bytes as well as private artifact reads
// so a stale or tampered database row cannot turn into a successful delivery.
func (s *Service) ReadResult(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey) (domain.ExternalAgentJobResult, error) {
	job, err := s.Status(ctx, jobID, actor, conversationKey)
	if err != nil {
		return domain.ExternalAgentJobResult{}, err
	}
	if job == nil {
		return domain.ExternalAgentJobResult{}, errors.New("external-agent job was not found")
	}
	return s.verifiedResultForJob(ctx, job)
}

// verifiedResultForJob is the common verified identity reader shared by
// ReadResult and StartAndWait, so the synchronous path accepts and rejects
// exactly the same identity as the durable reads: completed status, non-empty
// exact UTF-8 bytes, a non-empty SHA-256 that matches the content, and a
// coherent artifact read. Failures are closed and classified with a bounded
// domain.ResultErrorCode; the error string is exactly the code and never
// carries result content, digest, reference, actor or expected identity.
func (s *Service) verifiedResultForJob(ctx context.Context, job *domain.ExternalAgentJob) (domain.ExternalAgentJobResult, error) {
	if job == nil {
		return domain.ExternalAgentJobResult{}, errors.New("external-agent job was not found")
	}
	if job.Status != domain.JobCompleted {
		return domain.ExternalAgentJobResult{}, fmt.Errorf("external-agent job is not completed: %s", job.Status)
	}
	content := []byte(job.ResultSummary)
	mode := domain.JobResultDeliveryMarkdown
	if job.ResultArtifact != "" {
		mode = domain.JobResultDeliveryFile
		if s.artifacts == nil {
			return domain.ExternalAgentJobResult{}, resultReadError(domain.ResultErrorArtifactMissing)
		}
		maxBytes := s.maxResultBytes
		if job.ResultBytes > 0 && job.ResultBytes < maxBytes {
			maxBytes = job.ResultBytes
		}
		var err error
		content, err = s.artifacts.Get(ctx, job.ID+"-delivery", job.ResultArtifact, job.ResultSHA256, maxBytes)
		if err != nil {
			return domain.ExternalAgentJobResult{}, s.mapArtifactReadError(err)
		}
	}
	if len(content) == 0 || int64(len(content)) > s.maxResultBytes || !utf8.Valid(content) {
		return domain.ExternalAgentJobResult{}, s.resultIdentityError(mode, domain.ResultErrorIdentityInvalid, domain.ResultErrorArtifactBytesMismatch)
	}
	if job.ResultBytes <= 0 || int64(len(content)) != job.ResultBytes {
		return domain.ExternalAgentJobResult{}, s.resultIdentityError(mode, domain.ResultErrorIdentityInvalid, domain.ResultErrorArtifactBytesMismatch)
	}
	if strings.TrimSpace(job.ResultSHA256) == "" {
		return domain.ExternalAgentJobResult{}, s.resultIdentityError(mode, domain.ResultErrorIdentityInvalid, domain.ResultErrorArtifactDigestMismatch)
	}
	digest := sha256.Sum256(content)
	contentSHA := fmt.Sprintf("%x", digest)
	if !strings.EqualFold(job.ResultSHA256, contentSHA) {
		return domain.ExternalAgentJobResult{}, s.resultIdentityError(mode, domain.ResultErrorIdentityInvalid, domain.ResultErrorArtifactDigestMismatch)
	}
	return domain.ExternalAgentJobResult{
		JobID: job.ID, StatusRevision: job.StatusRevision, Text: string(content),
		ContentSHA256: contentSHA, ContentBytes: int64(len(content)), DeliveryMode: mode,
	}, nil
}

// resultReadError builds a bounded result-read failure whose string
// representation is exactly the code.
func resultReadError(code domain.ResultErrorCode) error {
	return &domain.ResultError{Code: code}
}

// identityFailureCode reports whether a bounded classification is an identity
// failure: an incoherent inline identity or an artifact owner/ref, byte, or
// digest mismatch. Availability failures (missing artifact, unavailable store)
// and unmapped fallbacks are deliberately excluded: they are not identity
// verification failures.
func identityFailureCode(code domain.ResultErrorCode) bool {
	switch code {
	case domain.ResultErrorIdentityInvalid,
		domain.ResultErrorArtifactOwnerRefMismatch,
		domain.ResultErrorArtifactBytesMismatch,
		domain.ResultErrorArtifactDigestMismatch:
		return true
	default:
		return false
	}
}

// classifiedResultError is the single counter point for verified result reads.
// It returns the bounded classified error and, when the code is an identity
// failure, increments the label-free identity-failure counter exactly once.
// No job ID, digest, reference, path, owner, actor, conversation, or content
// value is recorded.
func (s *Service) classifiedResultError(code domain.ResultErrorCode) error {
	if s != nil && s.metrics != nil && identityFailureCode(code) {
		s.metrics.AddCounter(domain.MetricExternalAgentResultIdentityInvalidTotal, 1, nil)
	}
	return resultReadError(code)
}

// resultIdentityError classifies an identity failure by delivery mode: inline
// rows collapse to the given inline code, file-mode rows to the artifact code,
// so operators can distinguish the cause without ever seeing the values
// involved. Classification flows through the shared identity-failure counter
// point used by full reads, chunk reads, and classified adapter errors.
func (s *Service) resultIdentityError(mode domain.JobResultDeliveryMode, inline, file domain.ResultErrorCode) error {
	if mode == domain.JobResultDeliveryFile {
		return s.classifiedResultError(file)
	}
	return s.classifiedResultError(inline)
}

// mapArtifactReadError fails closed: an adapter error already carrying a
// recognized bounded code keeps its code; anything else becomes the generic
// result_artifact_invalid so no unmapped detail can leak. Classified identity
// failures are routed through the shared identity-failure counter point.
func (s *Service) mapArtifactReadError(err error) error {
	if err == nil {
		return nil
	}
	return s.classifiedResultError(domain.ResultErrorCodeOf(err))
}

// ReadResultChunk exposes only one verified UTF-8 range of a completed job.
// Status performs the actor/conversation check before the artifact owner is
// derived from the authoritative job row.
func (s *Service) ReadResultChunk(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey, offsetBytes, maxBytes int64) (domain.ResultChunk, error) {
	job, err := s.Status(ctx, jobID, actor, conversationKey)
	if err != nil {
		return domain.ResultChunk{}, err
	}
	if job == nil {
		return domain.ResultChunk{}, errors.New("external-agent job was not found")
	}
	return s.readResultChunkForJob(ctx, job, offsetBytes, maxBytes)
}

// ReadResultChunkAtRevision refuses to read a later reconciliation revision
// through an older activation. The check and the read use the same job
// snapshot, so callers cannot mix revision N identity with revision N+2 data.
func (s *Service) ReadResultChunkAtRevision(
	ctx context.Context,
	jobID, actor string,
	conversationKey domain.ConversationKey,
	expectedRevision int,
	expectedStatus domain.ExternalAgentJobStatus,
	offsetBytes, maxBytes int64,
) (domain.ResultChunk, error) {
	job, err := s.StatusAtRevision(ctx, jobID, actor, conversationKey, expectedRevision, expectedStatus)
	if err != nil {
		return domain.ResultChunk{}, err
	}
	return s.readResultChunkForJob(ctx, job, offsetBytes, maxBytes)
}

func (s *Service) readResultChunkForJob(ctx context.Context, job *domain.ExternalAgentJob, offsetBytes, requestedMaxBytes int64) (domain.ResultChunk, error) {
	if job.Status != domain.JobCompleted {
		return domain.ResultChunk{}, fmt.Errorf("external-agent job is not completed: %s", job.Status)
	}
	maxBytes := s.resultChunkMax(requestedMaxBytes)
	if job.ResultArtifact != "" {
		if s.artifacts == nil {
			return domain.ResultChunk{}, s.classifiedResultError(domain.ResultErrorArtifactMissing)
		}
		verified, ok := s.artifacts.(port.ResultArtifactChunkReader)
		if !ok {
			return domain.ResultChunk{}, s.classifiedResultError(domain.ResultErrorArtifactMissing)
		}
		if job.ResultBytes <= 0 || job.ResultBytes > s.maxResultBytes {
			return domain.ResultChunk{}, s.classifiedResultError(domain.ResultErrorArtifactBytesMismatch)
		}
		chunk, err := verified.ReadChunk(ctx, domain.ResultArtifactChunkRequest{
			OwnerID:        job.ID + "-delivery",
			Reference:      job.ResultArtifact,
			ExpectedBytes:  job.ResultBytes,
			ExpectedSHA256: job.ResultSHA256,
			OffsetBytes:    offsetBytes,
			MaxBytes:       maxBytes,
		})
		if err != nil {
			return domain.ResultChunk{}, s.mapArtifactReadError(err)
		}
		return chunk, nil
	}
	if job.ResultSummary == "" {
		return domain.ResultChunk{}, s.classifiedResultError(domain.ResultErrorIdentityInvalid)
	}
	return s.readInlineResultChunk(job.ResultSummary, job.ResultBytes, job.ResultSHA256, offsetBytes, maxBytes)
}

func (s *Service) resultChunkMax(requested int64) int64 {
	if requested <= 0 || requested > s.maxResultChunkBytes {
		return s.maxResultChunkBytes
	}
	return requested
}

func (s *Service) readInlineResultChunk(content string, expectedBytes int64, expectedSHA256 string, offsetBytes, maxBytes int64) (domain.ResultChunk, error) {
	data := []byte(content)
	if expectedBytes <= 0 || int64(len(data)) != expectedBytes || !utf8.Valid(data) || strings.TrimSpace(expectedSHA256) == "" {
		return domain.ResultChunk{}, s.classifiedResultError(domain.ResultErrorIdentityInvalid)
	}
	digest := sha256.Sum256(data)
	actualSHA256 := fmt.Sprintf("%x", digest)
	if !strings.EqualFold(actualSHA256, expectedSHA256) {
		return domain.ResultChunk{}, s.classifiedResultError(domain.ResultErrorIdentityInvalid)
	}
	// Range validation failures are client request errors, not identity
	// corruption: they never increment the identity-failure counter because
	// the stored result identity was verified complete and exact above.
	if offsetBytes < 0 || offsetBytes > expectedBytes {
		return domain.ResultChunk{}, resultReadError(domain.ResultErrorChunkRequestInvalid)
	}
	if offsetBytes == expectedBytes {
		return domain.ResultChunk{OffsetBytes: offsetBytes, NextOffsetBytes: offsetBytes, EOF: true, SHA256: actualSHA256}, nil
	}
	if !utf8.RuneStart(data[offsetBytes]) {
		return domain.ResultChunk{}, resultReadError(domain.ResultErrorChunkRequestInvalid)
	}
	end := offsetBytes + maxBytes
	if end < offsetBytes || end > expectedBytes {
		end = expectedBytes
	}
	completeBytes := completeUTF8Prefix(data[offsetBytes:end])
	if completeBytes == 0 {
		return domain.ResultChunk{}, resultReadError(domain.ResultErrorChunkRequestInvalid)
	}
	nextOffset := offsetBytes + int64(completeBytes)
	return domain.ResultChunk{
		Content:         string(data[offsetBytes:nextOffset]),
		OffsetBytes:     offsetBytes,
		NextOffsetBytes: nextOffset,
		EOF:             nextOffset == expectedBytes,
		SHA256:          actualSHA256,
	}, nil
}

func completeUTF8Prefix(data []byte) int {
	position := 0
	for position < len(data) {
		if !utf8.FullRune(data[position:]) {
			break
		}
		runeValue, size := utf8.DecodeRune(data[position:])
		if runeValue == utf8.RuneError && size == 1 {
			return 0
		}
		position += size
	}
	return position
}

// HostCompletionTurn is the non-privileged completion phase for a detached
// invocation. It is deliberately deterministic: it reads the already
// materialized result and never starts external-agent or requests confirmation.
func (s *Service) HostCompletionTurn(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey) (port.AgentTurn, error) {
	result, err := s.ReadResult(ctx, jobID, actor, conversationKey)
	if err != nil {
		return port.AgentTurn{}, err
	}
	return port.AgentTurn{Text: result.Text}, nil
}

func (s *Service) CancelForConversation(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey) (*domain.ExternalAgentJob, error) {
	if _, err := s.Status(ctx, jobID, actor, conversationKey); err != nil {
		return nil, err
	}
	return s.requestCancellation(ctx, jobID, actor)
}

func (s *Service) requestCancellation(ctx context.Context, jobID, actor string) (*domain.ExternalAgentJob, error) {
	job, err := s.store.RequestCancellation(ctx, jobID, actor)
	if err == nil {
		s.jobWake()
		s.wakeNotifications()
	}
	return job, err
}

func (s *Service) Reconcile(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey) (domain.ExternalAgentInvocationResult, error) {
	return s.ReconcileExpected(ctx, jobID, actor, conversationKey, -1)
}

func (s *Service) ReconcileExpected(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey, expectedRevision int) (domain.ExternalAgentInvocationResult, error) {
	job, err := s.Status(ctx, jobID, actor, conversationKey)
	if err != nil {
		return domain.ExternalAgentInvocationResult{}, err
	}
	if job == nil {
		return domain.ExternalAgentInvocationResult{}, errors.New("external-agent job was not found")
	}
	if job.Status != domain.JobCompletionUnknown {
		return domain.ExternalAgentInvocationResult{}, errors.New("external-agent job is not awaiting reconciliation")
	}
	if expectedRevision < -1 {
		return domain.ExternalAgentInvocationResult{}, errors.New("expected status revision is invalid")
	}
	if expectedRevision >= 0 && job.StatusRevision != expectedRevision {
		return domain.ExternalAgentInvocationResult{}, port.ErrExternalAgentJobRevisionConflict
	}
	recovery, ok := s.runtime.(port.ExternalAgentSessionRecoveryRuntime)
	if !ok {
		return domain.ExternalAgentInvocationResult{}, errors.New("session recovery is unsupported; inspect external state and close the completion_unknown job explicitly")
	}
	reconciler, ok := s.store.(port.ExternalAgentJobReconciler)
	if !ok {
		return domain.ExternalAgentInvocationResult{}, errors.New("durable job store does not support reconciliation")
	}
	owner := "reconciler_" + randomID()
	var reconciling *domain.ExternalAgentJob
	if expected, ok := s.store.(port.ExternalAgentJobExpectedReconciler); ok && expectedRevision >= 0 {
		reconciling, err = expected.BeginReconciliationExpected(ctx, jobID, actor, conversationKey, expectedRevision, s.clock.Now().UTC(), owner, s.cfg.LeaseTTL)
	} else {
		reconciling, err = reconciler.BeginReconciliation(ctx, jobID, actor, conversationKey, s.clock.Now().UTC(), owner, s.cfg.LeaseTTL)
	}
	if err != nil {
		return domain.ExternalAgentInvocationResult{}, err
	}
	reconcileCtx, cancelReconcile := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	heartbeatStopped := make(chan struct{})
	go func() {
		defer close(heartbeatStopped)
		s.heartbeat(reconcileCtx, reconciling, heartbeatDone, cancelReconcile)
	}()
	result, runErr := recovery.Reconcile(reconcileCtx, *reconciling)
	close(heartbeatDone)
	<-heartbeatStopped
	cancelReconcile()
	next, code := domain.JobCompleted, ""
	if runErr != nil {
		next, code = domain.JobCompletionUnknown, "completion_unknown"
	}
	transitionErr := s.transition(context.WithoutCancel(ctx), reconciling.ID, reconciling.LeaseOwner, reconciling.Attempt, next, &result, code, s.clock.Now().UTC())
	if transitionErr != nil {
		return domain.ExternalAgentInvocationResult{}, errors.New("external-agent reconciliation state update failed")
	}
	if runErr != nil {
		return domain.ExternalAgentInvocationResult{}, safeReconciliationError(runErr)
	}
	return result, nil
}

func safeReconciliationError(err error) error {
	var externalAgentErr *domain.ExternalAgentError
	if errors.As(err, &externalAgentErr) && externalAgentErr.Code != "" {
		return fmt.Errorf("external-agent reconciliation failed: %s", externalAgentErr.Code)
	}
	return errors.New("external-agent reconciliation failed; job remains completion_unknown")
}

func (s *Service) Run(ctx context.Context) {
	defer close(s.stopped)
	var workers sync.WaitGroup
	sem := make(chan struct{}, s.cfg.Concurrency)
	s.scheduler.Run(ctx, s.stopClaims, func() bool {
		worked := s.recoverExpired(ctx, sem, &workers)
		return s.claimAvailable(ctx, sem, &workers) || worked
	})
	workers.Wait()
}

// StopAdmission prevents new durable claims while allowing already running
// jobs to finish under their current context.
func (s *Service) StopAdmission() {
	if s == nil {
		return
	}
	s.admissionMu.Lock()
	s.stopOnce.Do(func() { close(s.stopClaims) })
	s.admissionMu.Unlock()
}

// WaitStopped waits for the claim loop and all workers to finish.
func (s *Service) WaitStopped(ctx context.Context) error {
	if s == nil {
		return nil
	}
	select {
	case <-s.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) ShutdownStats(ctx context.Context) (domain.ExternalAgentJobShutdownStats, error) {
	store, ok := s.store.(port.ExternalAgentJobShutdownStore)
	if !ok {
		return domain.ExternalAgentJobShutdownStats{}, errors.New("external-agent shutdown stats are unavailable")
	}
	return store.ShutdownStats(ctx)
}

func (s *Service) claimAvailable(ctx context.Context, sem chan struct{}, workers *sync.WaitGroup) bool {
	claimed := false
	for {
		s.admissionMu.RLock()
		select {
		case <-s.stopClaims:
			s.admissionMu.RUnlock()
			return claimed
		default:
		}
		select {
		case sem <- struct{}{}:
		default:
			s.admissionMu.RUnlock()
			return claimed
		}
		job, err := s.store.ClaimNext(ctx, s.clock.Now().UTC(), "worker_"+randomID(), s.cfg.LeaseTTL)
		s.admissionMu.RUnlock()
		if err != nil || job == nil {
			<-sem
			return claimed
		}
		claimed = true
		workers.Add(1)
		go func(job *domain.ExternalAgentJob) {
			defer workers.Done()
			defer func() { <-sem }()
			s.execute(ctx, job)
		}(job)
	}
}

func (s *Service) execute(parent context.Context, job *domain.ExternalAgentJob) {
	if !job.TimeoutAt.After(s.clock.Now().UTC()) {
		now := s.clock.Now().UTC()
		_ = s.transition(context.WithoutCancel(parent), job.ID, job.LeaseOwner, job.Attempt, domain.JobFailed, nil, "acp_job_timeout", now)
		return
	}
	ctx := parent
	cancel := func() {}
	if !job.TimeoutAt.IsZero() {
		ctx, cancel = context.WithDeadline(parent, job.TimeoutAt)
	}
	defer cancel()
	heartbeatDone := make(chan struct{})
	go s.heartbeat(ctx, job, heartbeatDone, cancel)
	result, runErr := s.runtime.Run(ctx, *job)
	close(heartbeatDone)
	now := s.clock.Now().UTC()
	current, _ := s.store.GetJob(context.WithoutCancel(parent), job.ID)
	if current == nil {
		return
	}
	timedOut := !job.TimeoutAt.IsZero() && !job.TimeoutAt.After(now)
	next, code := terminalOutcome(current, runErr, ctx.Err(), s.cfg.MaxAttempts, timedOut)
	if err := s.transition(context.WithoutCancel(parent), job.ID, job.LeaseOwner, job.Attempt, next, &result, code, now); err != nil {
		return
	}
	if next == domain.JobInterruptedSafe && job.TimeoutAt.After(now) {
		_ = s.transition(context.WithoutCancel(parent), job.ID, job.LeaseOwner, job.Attempt, domain.JobQueued, nil, "", now)
	}
	// Terminal delivery is handled by the independent durable notification worker.
}

func (s *Service) heartbeat(ctx context.Context, job *domain.ExternalAgentJob, done <-chan struct{}, cancel context.CancelFunc) {
	interval := s.cfg.LeaseTTL / 3
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := s.clock.Now().UTC()
			current, err := s.store.GetJob(context.WithoutCancel(ctx), job.ID)
			if err == nil && current != nil && current.Status == domain.JobCancelRequested {
				cancel()
				continue
			}
			if err := s.store.RenewLease(context.WithoutCancel(ctx), job.ID, job.LeaseOwner, job.Attempt, now, s.cfg.LeaseTTL); err != nil {
				cancel()
				return
			}
		}
	}
}

// recoverExpired returns every job whose lease expired to a resting status.
//
// A job that was running and holds a session is reconciled in the background,
// under the same concurrency limit as an ordinary claim. A cancelled job and a
// reconciliation that lost its own lease are never resumed: the first was
// stopped on purpose, and the second would retry without a bound.
func (s *Service) recoverExpired(ctx context.Context, sem chan struct{}, workers *sync.WaitGroup) bool {
	jobs, err := s.store.ListExpiredRunning(ctx, s.clock.Now().UTC())
	if err != nil {
		return false
	}
	recovered := false
	for _, job := range jobs {
		now := s.clock.Now().UTC()
		next, code := domain.JobQueued, ""
		if job.Status == domain.JobCancelRequested {
			if job.SideEffectsPossible || job.ExternalAgentSessionID != "" {
				next, code = domain.JobCompletionUnknown, "completion_unknown"
			} else {
				next = domain.JobCancelled
			}
		} else if job.Status == domain.JobReconciling {
			next, code = domain.JobCompletionUnknown, "completion_unknown"
		} else if job.SideEffectsPossible || job.ExternalAgentSessionID != "" {
			next, code = domain.JobCompletionUnknown, "completion_unknown"
		} else if !job.TimeoutAt.After(now) || job.Attempt >= s.cfg.MaxAttempts {
			next, code = domain.JobFailed, "job_lease_lost"
		}
		recovery, ok := s.store.(port.ExpiredExternalAgentJobRecovery)
		if ok {
			if err := recovery.RecoverExpired(ctx, job.ID, job.Attempt, job.StatusRevision, now, next, code); err == nil {
				recovered = true
				if next == domain.JobQueued {
					s.jobWake()
				}
				if notificationTerminal(next) {
					s.wakeNotifications()
				}
				if s.resumableAfterExpiry(job, next) && workers != nil && sem != nil {
					workers.Add(1)
					go func(job domain.ExternalAgentJob) {
						defer workers.Done()
						select {
						case sem <- struct{}{}:
						case <-ctx.Done():
							return
						}
						defer func() { <-sem }()
						_, _ = s.Reconcile(ctx, job.ID, job.Actor, job.ConversationKey)
					}(job)
				}
			}
		}
	}
	return recovered
}

// resumableAfterExpiry reports whether an expired job may be resumed without
// an operator command. Only a running job with a session qualifies.
func (s *Service) resumableAfterExpiry(job domain.ExternalAgentJob, next domain.ExternalAgentJobStatus) bool {
	return next == domain.JobCompletionUnknown && job.Status == domain.JobRunning && job.ExternalAgentSessionID != ""
}

func (s *Service) transition(
	ctx context.Context,
	jobID, owner string,
	attempt int,
	next domain.ExternalAgentJobStatus,
	result *domain.ExternalAgentInvocationResult,
	errorCode string,
	now time.Time,
) error {
	if err := s.store.Transition(ctx, jobID, owner, attempt, next, result, errorCode, now); err != nil {
		return err
	}
	if next == domain.JobQueued {
		s.jobWake()
	}
	if notificationTerminal(next) {
		s.wakeNotifications()
	}
	return nil
}

func (s *Service) wakeNotifications() {
	if s.notificationWake != nil {
		s.notificationWake()
	}
}

func notificationTerminal(status domain.ExternalAgentJobStatus) bool {
	switch status {
	case domain.JobCompleted, domain.JobFailed, domain.JobCancelled, domain.JobCompletionUnknown, domain.JobAbandoned:
		return true
	default:
		return false
	}
}

func terminalOutcome(job *domain.ExternalAgentJob, runErr, contextErr error, maxAttempts int, timedOut bool) (domain.ExternalAgentJobStatus, string) {
	if timedOut {
		if job.SideEffectsPossible || job.ExternalAgentSessionID != "" {
			return domain.JobCompletionUnknown, "completion_unknown"
		}
		return domain.JobFailed, "acp_job_timeout"
	}
	if runErr == nil {
		return domain.JobCompleted, ""
	}
	if code := externalAgentFailureCode(
		runErr,
	); code == string(domain.ExternalAgentErrorResultTooLarge) || code == string(domain.ExternalAgentErrorResultArtifactInvalid) || code == string(domain.ExternalAgentErrorResultDeliveryFailed) ||
		strings.HasPrefix(code, "result_") {
		return domain.JobFailed, code
	}
	if job.Status == domain.JobCancelRequested {
		if job.SideEffectsPossible || job.ExternalAgentSessionID != "" {
			return domain.JobCompletionUnknown, "completion_unknown"
		}
		return domain.JobCancelled, ""
	}
	if contextErr != nil && (job.SideEffectsPossible || job.ExternalAgentSessionID != "") {
		return domain.JobCompletionUnknown, "completion_unknown"
	}
	if contextErr != nil && job.Attempt < maxAttempts {
		return domain.JobInterruptedSafe, "job_lease_lost"
	}
	if contextErr != nil {
		return domain.JobFailed, "acp_job_timeout"
	}
	code := externalAgentFailureCode(runErr)
	if !job.SideEffectsPossible && job.ExternalAgentSessionID == "" && job.Attempt < maxAttempts &&
		(code == string(domain.ExternalAgentErrorProcessExit) || code == string(domain.ExternalAgentErrorIdleTimeout)) {
		return domain.JobInterruptedSafe, code
	}
	if job.SideEffectsPossible || job.ExternalAgentSessionID != "" {
		return domain.JobCompletionUnknown, "completion_unknown"
	}
	return domain.JobFailed, code
}

func externalAgentFailureCode(err error) string {
	var externalAgentErr *domain.ExternalAgentError
	if errors.As(err, &externalAgentErr) && externalAgentErr.Code != "" {
		return string(externalAgentErr.Code)
	}
	return string(domain.ExternalAgentErrorProcessExit)
}

func isTerminalStatus(status domain.ExternalAgentJobStatus) bool {
	switch status {
	case domain.JobCompletionUnknown, domain.JobCompleted, domain.JobFailed, domain.JobCancelled, domain.JobAbandoned:
		return true
	default:
		return false
	}
}

func randomID() string {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		return "local"
	}
	return hex.EncodeToString(data)
}

func stringsEmpty(values ...string) bool {
	return slices.Contains(values, "")
}
