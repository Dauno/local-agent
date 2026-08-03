package externalagent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type Config struct {
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	LeaseTTL       time.Duration
	PollInterval   time.Duration
	Concurrency    int
	MaxAttempts    int
}

type Dependencies struct {
	Store     port.ExternalAgentJobStore
	Runtime   port.ExternalAgentJobRuntime
	Publisher port.ExternalAgentJobPublisher
	Artifacts port.ResultArtifactStore
	// MaxResultBytes bounds host-completion reads. The artifact adapter applies
	// its own bound as a second, independent check.
	MaxResultBytes int64
	// MaxResultChunkBytes bounds each host-completion read. A zero value uses
	// the same 16 KiB default as recoverable result readers.
	MaxResultChunkBytes int64
	Clock               port.Clock
	Logger              port.Logger
	Metrics             port.MetricRecorder
}

type Service struct {
	cfg                 Config
	store               port.ExternalAgentJobStore
	runtime             port.ExternalAgentJobRuntime
	publisher           port.ExternalAgentJobPublisher
	artifacts           port.ResultArtifactStore
	maxResultBytes      int64
	maxResultChunkBytes int64
	clock               port.Clock
	logger              port.Logger
	metrics             port.MetricRecorder
	stopClaims          chan struct{}
	stopOnce            sync.Once
	admissionMu         sync.RWMutex
	stopped             chan struct{}
}

var _ port.ExternalAgentJobReader = (*Service)(nil)
var _ port.ExternalAgentJobHostCompleter = (*Service)(nil)

const defaultResultChunkBytes int64 = 16 * 1024

func New(cfg Config, deps Dependencies) (*Service, error) {
	if cfg.DefaultTimeout <= 0 || cfg.MaxTimeout < cfg.DefaultTimeout || cfg.LeaseTTL <= 0 || cfg.PollInterval <= 0 || cfg.Concurrency <= 0 || cfg.MaxAttempts <= 0 {
		return nil, errors.New("external-agent job settings are invalid")
	}
	if deps.Store == nil || deps.Runtime == nil {
		return nil, errors.New("external-agent job store and runtime are required")
	}
	if deps.Clock == nil {
		deps.Clock = systemClock{}
	}
	if deps.MaxResultBytes <= 0 || deps.MaxResultBytes > domain.MaxExternalAgentResultBytes {
		deps.MaxResultBytes = domain.MaxExternalAgentResultBytes
	}
	if deps.MaxResultChunkBytes <= 0 || deps.MaxResultChunkBytes > deps.MaxResultBytes {
		deps.MaxResultChunkBytes = defaultResultChunkBytes
		if deps.MaxResultChunkBytes > deps.MaxResultBytes {
			deps.MaxResultChunkBytes = deps.MaxResultBytes
		}
	}
	logger := deps.Logger
	if logger == nil {
		logger = noopLogger{}
	}
	metrics := deps.Metrics
	if metrics == nil {
		metrics = port.NoopMetricRecorder{}
	}
	return &Service{cfg: cfg, store: deps.Store, runtime: deps.Runtime, publisher: deps.Publisher,
		artifacts: deps.Artifacts, maxResultBytes: deps.MaxResultBytes, maxResultChunkBytes: deps.MaxResultChunkBytes,
		clock: deps.Clock, logger: logger, metrics: metrics,
		stopClaims: make(chan struct{}), stopped: make(chan struct{})}, nil
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
	now := s.clock.Now().UTC()
	job := domain.ExternalAgentJob{
		ID: "job_" + randomID(), Mode: request.Mode, Provider: request.Provider, Profile: request.Profile,
		PrimaryProject: request.PrimaryProject, AdditionalProjects: append([]string(nil), request.AdditionalProjects...),
		RegistryRevision: request.RegistryRevision, Task: request.Task, RequestSHA256: domain.ExternalAgentJobRequestDigest(request),
		WrapperCallID: request.WrapperCallID, OriginalCallID: request.OriginalCallID, Actor: request.Actor, TeamID: request.TeamID,
		ConversationKey: request.ConversationKey, Status: domain.JobQueued, TimeoutAt: now.Add(timeout), CreatedAt: now, UpdatedAt: now,
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
	return &job, nil
}

func (s *Service) StartAndWait(ctx context.Context, request domain.ExternalAgentJobRequest) (domain.AcpInvocationResult, error) {
	job, err := s.Start(ctx, request)
	if err != nil {
		return domain.AcpInvocationResult{}, err
	}
	for {
		current, err := s.store.GetJob(ctx, job.ID)
		if err != nil {
			return domain.AcpInvocationResult{}, err
		}
		if current == nil {
			return domain.AcpInvocationResult{}, errors.New("external-agent job disappeared")
		}
		switch current.Status {
		case domain.JobCompleted:
			return domain.AcpInvocationResult{Text: current.ResultSummary, ArtifactRef: current.ResultArtifact, ResultSHA256: current.ResultSHA256, ResultBytes: current.ResultBytes, Inline: current.ResultArtifact == ""}, nil
		case domain.JobFailed, domain.JobCancelled, domain.JobAbandoned, domain.JobCompletionUnknown:
			if current.ErrorCode == "" {
				return domain.AcpInvocationResult{}, fmt.Errorf("external-agent job ended with status %s", current.Status)
			}
			return domain.AcpInvocationResult{}, fmt.Errorf("external-agent job ended with status %s: %s", current.Status, current.ErrorCode)
		}
		if !current.TimeoutAt.After(s.clock.Now().UTC()) {
			// A queued foreground job can outlive its total budget; do not wait forever.
			if current.Status == domain.JobQueued {
				_, _ = s.Cancel(context.WithoutCancel(ctx), job.ID, request.Actor)
			}
			return domain.AcpInvocationResult{}, errors.New("external-agent job ended with status failed: acp_job_timeout")
		}
		timer := time.NewTimer(s.cfg.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			_, _ = s.Cancel(context.WithoutCancel(ctx), job.ID, request.Actor)
			return domain.AcpInvocationResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Service) Cancel(ctx context.Context, jobID, actor string) (*domain.ExternalAgentJob, error) {
	return s.store.RequestCancellation(ctx, jobID, actor)
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
	if job.Status != domain.JobCompleted {
		return domain.ExternalAgentJobResult{}, fmt.Errorf("external-agent job is not completed: %s", job.Status)
	}

	content := []byte(job.ResultSummary)
	mode := domain.JobResultDeliveryMarkdown
	if job.ResultArtifact != "" {
		if s.artifacts == nil {
			return domain.ExternalAgentJobResult{}, errors.New("result_artifact_invalid")
		}
		maxBytes := s.maxResultBytes
		if job.ResultBytes > 0 && job.ResultBytes < maxBytes {
			maxBytes = job.ResultBytes
		}
		content, err = s.artifacts.Get(ctx, job.ID+"-delivery", job.ResultArtifact, job.ResultSHA256, maxBytes)
		if err != nil {
			return domain.ExternalAgentJobResult{}, errors.New("result_artifact_invalid")
		}
		mode = domain.JobResultDeliveryFile
	}
	if len(content) == 0 || int64(len(content)) > s.maxResultBytes {
		return domain.ExternalAgentJobResult{}, errors.New("result_artifact_invalid")
	}
	if job.ResultBytes <= 0 || int64(len(content)) != job.ResultBytes {
		return domain.ExternalAgentJobResult{}, errors.New("result_artifact_invalid")
	}
	digest := sha256.Sum256(content)
	contentSHA := fmt.Sprintf("%x", digest)
	if job.ResultSHA256 != "" && !strings.EqualFold(job.ResultSHA256, contentSHA) {
		return domain.ExternalAgentJobResult{}, errors.New("result_artifact_invalid")
	}
	return domain.ExternalAgentJobResult{
		JobID: job.ID, StatusRevision: job.StatusRevision, Text: string(content),
		ContentSHA256: contentSHA, ContentBytes: int64(len(content)), DeliveryMode: mode,
	}, nil
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
	if job.Status != domain.JobCompleted {
		return domain.ResultChunk{}, fmt.Errorf("external-agent job is not completed: %s", job.Status)
	}
	maxBytes = s.resultChunkMax(maxBytes)
	if job.ResultArtifact != "" {
		if s.artifacts == nil {
			return domain.ResultChunk{}, errors.New("result_artifact_invalid")
		}
		verified, ok := s.artifacts.(port.ResultArtifactChunkReader)
		if !ok {
			return domain.ResultChunk{}, errors.New("result_artifact_invalid")
		}
		if job.ResultBytes <= 0 || job.ResultBytes > s.maxResultBytes {
			return domain.ResultChunk{}, errors.New("result_artifact_invalid")
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
			return domain.ResultChunk{}, errors.New("result_artifact_invalid")
		}
		return chunk, nil
	}
	if job.ResultSummary == "" {
		return domain.ResultChunk{}, errors.New("result_artifact_invalid")
	}
	return readInlineResultChunk(job.ResultSummary, job.ResultBytes, job.ResultSHA256, offsetBytes, maxBytes)
}

func (s *Service) resultChunkMax(requested int64) int64 {
	if requested <= 0 || requested > s.maxResultChunkBytes {
		return s.maxResultChunkBytes
	}
	return requested
}

func readInlineResultChunk(content string, expectedBytes int64, expectedSHA256 string, offsetBytes, maxBytes int64) (domain.ResultChunk, error) {
	data := []byte(content)
	if expectedBytes <= 0 || int64(len(data)) != expectedBytes || !utf8.Valid(data) || strings.TrimSpace(expectedSHA256) == "" {
		return domain.ResultChunk{}, errors.New("result_artifact_invalid")
	}
	digest := sha256.Sum256(data)
	actualSHA256 := fmt.Sprintf("%x", digest)
	if !strings.EqualFold(actualSHA256, expectedSHA256) {
		return domain.ResultChunk{}, errors.New("result_artifact_invalid")
	}
	if offsetBytes < 0 || offsetBytes > expectedBytes {
		return domain.ResultChunk{}, errors.New("result_artifact_invalid")
	}
	if offsetBytes == expectedBytes {
		return domain.ResultChunk{OffsetBytes: offsetBytes, NextOffsetBytes: offsetBytes, EOF: true, SHA256: actualSHA256}, nil
	}
	if !utf8.RuneStart(data[offsetBytes]) {
		return domain.ResultChunk{}, errors.New("result_artifact_invalid")
	}
	end := offsetBytes + maxBytes
	if end < offsetBytes || end > expectedBytes {
		end = expectedBytes
	}
	completeBytes := completeUTF8Prefix(data[offsetBytes:end])
	if completeBytes == 0 {
		return domain.ResultChunk{}, errors.New("result_artifact_invalid")
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
// materialized result and never starts ACP or requests confirmation.
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
	return s.store.RequestCancellation(ctx, jobID, actor)
}

func (s *Service) Reconcile(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey) (domain.AcpInvocationResult, error) {
	return s.ReconcileExpected(ctx, jobID, actor, conversationKey, -1)
}

func (s *Service) ReconcileExpected(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey, expectedRevision int) (domain.AcpInvocationResult, error) {
	job, err := s.Status(ctx, jobID, actor, conversationKey)
	if err != nil {
		return domain.AcpInvocationResult{}, err
	}
	if job == nil {
		return domain.AcpInvocationResult{}, errors.New("external-agent job was not found")
	}
	if job.Status != domain.JobCompletionUnknown {
		return domain.AcpInvocationResult{}, errors.New("external-agent job is not awaiting reconciliation")
	}
	if expectedRevision < -1 {
		return domain.AcpInvocationResult{}, errors.New("expected status revision is invalid")
	}
	if expectedRevision >= 0 && job.StatusRevision != expectedRevision {
		return domain.AcpInvocationResult{}, port.ErrExternalAgentJobRevisionConflict
	}
	recovery, ok := s.runtime.(port.ExternalAgentSessionRecoveryRuntime)
	if !ok {
		return domain.AcpInvocationResult{}, errors.New("session recovery is unsupported; inspect external state and close the completion_unknown job explicitly")
	}
	reconciler, ok := s.store.(port.ExternalAgentJobReconciler)
	if !ok {
		return domain.AcpInvocationResult{}, errors.New("durable job store does not support reconciliation")
	}
	owner := "reconciler_" + randomID()
	var reconciling *domain.ExternalAgentJob
	if expected, ok := s.store.(port.ExternalAgentJobExpectedReconciler); ok && expectedRevision >= 0 {
		reconciling, err = expected.BeginReconciliationExpected(ctx, jobID, actor, conversationKey, expectedRevision, s.clock.Now().UTC(), owner, s.cfg.LeaseTTL)
	} else {
		reconciling, err = reconciler.BeginReconciliation(ctx, jobID, actor, conversationKey, s.clock.Now().UTC(), owner, s.cfg.LeaseTTL)
	}
	if err != nil {
		return domain.AcpInvocationResult{}, err
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
	transitionErr := s.store.Transition(context.WithoutCancel(ctx), reconciling.ID, reconciling.LeaseOwner, reconciling.Attempt, next, &result, code, s.clock.Now().UTC())
	if transitionErr != nil {
		return domain.AcpInvocationResult{}, errors.New("external-agent reconciliation state update failed")
	}
	if runErr != nil {
		return domain.AcpInvocationResult{}, safeReconciliationError(runErr)
	}
	return result, nil
}

func safeReconciliationError(err error) error {
	var acpErr *domain.ACPError
	if errors.As(err, &acpErr) && acpErr.Code != "" {
		return fmt.Errorf("external-agent reconciliation failed: %s", acpErr.Code)
	}
	return errors.New("external-agent reconciliation failed; job remains completion_unknown")
}

func (s *Service) Run(ctx context.Context) {
	defer close(s.stopped)
	var workers sync.WaitGroup
	sem := make(chan struct{}, s.cfg.Concurrency)
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopClaims:
			workers.Wait()
			return
		default:
		}
		s.recoverExpired(ctx)
		s.claimAvailable(ctx, sem, &workers)
		select {
		case <-ctx.Done():
			workers.Wait()
			return
		case <-s.stopClaims:
			workers.Wait()
			return
		case <-ticker.C:
		}
	}
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

func (s *Service) claimAvailable(ctx context.Context, sem chan struct{}, workers *sync.WaitGroup) {
	for {
		s.admissionMu.RLock()
		select {
		case <-s.stopClaims:
			s.admissionMu.RUnlock()
			return
		default:
		}
		select {
		case sem <- struct{}{}:
		default:
			s.admissionMu.RUnlock()
			return
		}
		job, err := s.store.ClaimNext(ctx, s.clock.Now().UTC(), "worker_"+randomID(), s.cfg.LeaseTTL)
		s.admissionMu.RUnlock()
		if err != nil || job == nil {
			<-sem
			return
		}
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
		if err := s.store.Transition(context.WithoutCancel(parent), job.ID, job.LeaseOwner, job.Attempt, domain.JobFailed, nil, "acp_job_timeout", now); err == nil {
		}
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
	if err := s.store.Transition(context.WithoutCancel(parent), job.ID, job.LeaseOwner, job.Attempt, next, &result, code, now); err != nil {
		return
	}
	if next == domain.JobInterruptedSafe && job.TimeoutAt.After(now) {
		_ = s.store.Transition(context.WithoutCancel(parent), job.ID, job.LeaseOwner, job.Attempt, domain.JobQueued, nil, "", now)
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

func (s *Service) recoverExpired(ctx context.Context) {
	jobs, err := s.store.ListExpiredRunning(ctx, s.clock.Now().UTC())
	if err != nil {
		return
	}
	for _, job := range jobs {
		now := s.clock.Now().UTC()
		next, code := domain.JobQueued, ""
		if job.Status == domain.JobReconciling {
			next, code = domain.JobCompletionUnknown, "completion_unknown"
		} else if job.SideEffectsPossible || job.ACPSessionID != "" {
			next, code = domain.JobCompletionUnknown, "completion_unknown"
		} else if !job.TimeoutAt.After(now) || job.Attempt >= s.cfg.MaxAttempts {
			next, code = domain.JobFailed, "job_lease_lost"
		}
		recovery, ok := s.store.(port.ExpiredExternalAgentJobRecovery)
		if ok {
			_ = recovery.RecoverExpired(ctx, job.ID, job.Attempt, job.StatusRevision, now, next, code)
		}
	}
}

func terminalOutcome(job *domain.ExternalAgentJob, runErr, contextErr error, maxAttempts int, timedOut bool) (domain.ExternalAgentJobStatus, string) {
	if timedOut {
		if job.SideEffectsPossible || job.ACPSessionID != "" {
			return domain.JobCompletionUnknown, "completion_unknown"
		}
		return domain.JobFailed, "acp_job_timeout"
	}
	if runErr == nil {
		return domain.JobCompleted, ""
	}
	if code := acpFailureCode(runErr); code == string(domain.ACPErrorResultTooLarge) || code == string(domain.ACPErrorResultArtifactInvalid) || code == string(domain.ACPErrorResultDeliveryFailed) || strings.HasPrefix(code, "result_") {
		return domain.JobFailed, code
	}
	if job.Status == domain.JobCancelRequested {
		if job.SideEffectsPossible || job.ACPSessionID != "" {
			return domain.JobCompletionUnknown, "completion_unknown"
		}
		return domain.JobCancelled, ""
	}
	if contextErr != nil && (job.SideEffectsPossible || job.ACPSessionID != "") {
		return domain.JobCompletionUnknown, "completion_unknown"
	}
	if contextErr != nil && job.Attempt < maxAttempts {
		return domain.JobInterruptedSafe, "job_lease_lost"
	}
	if contextErr != nil {
		return domain.JobFailed, "acp_job_timeout"
	}
	code := acpFailureCode(runErr)
	if !job.SideEffectsPossible && job.ACPSessionID == "" && job.Attempt < maxAttempts &&
		(code == string(domain.ACPErrorProcessExit) || code == string(domain.ACPErrorIdleTimeout)) {
		return domain.JobInterruptedSafe, code
	}
	if job.SideEffectsPossible || job.ACPSessionID != "" {
		return domain.JobCompletionUnknown, "completion_unknown"
	}
	return domain.JobFailed, code
}

func acpFailureCode(err error) string {
	var acpErr *domain.ACPError
	if errors.As(err, &acpErr) && acpErr.Code != "" {
		return string(acpErr.Code)
	}
	return string(domain.ACPErrorProcessExit)
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
	for _, value := range values {
		if value == "" {
			return true
		}
	}
	return false
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
