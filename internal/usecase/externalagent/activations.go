package externalagent

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

const (
	defaultActivationRetryBase   = time.Second
	defaultActivationRetryMax    = time.Minute
	defaultActivationMaxAttempts = 8
)

// ActivationConfig controls durable activation polling and leases. Backoff is
// only used while an activation is still before the model_started boundary.
type ActivationConfig struct {
	PollInterval   time.Duration
	LeaseTTL       time.Duration
	RetryBase      time.Duration
	RetryMax       time.Duration
	StuckThreshold time.Duration
	// MaxAttempts bounds pre-model retries. A retryable pre-model failure
	// beyond this bound becomes a terminal activation failure instead of an
	// unbounded backoff loop.
	MaxAttempts int
}

type ActivationDependencies struct {
	Store     port.ExternalAgentJobActivationStore
	Handler   port.ExternalAgentJobCompletionHandler
	Clock     port.Clock
	Logger    port.Logger
	Metrics   port.MetricRecorder
	Scheduler *workpoll.Scheduler
}

// ActivationWorker consumes the activation outbox one claimed activation at a
// time. SQLite serializes later revisions of the same conversation; keeping
// processing single-flight here also prevents an in-process overlap.
type ActivationWorker struct {
	cfg     ActivationConfig
	store   port.ExternalAgentJobActivationStore
	handler port.ExternalAgentJobCompletionHandler
	clock   port.Clock
	logger  port.Logger
	metrics port.MetricRecorder
	owner   string

	stopClaims chan struct{}
	stopOnce   sync.Once
	stopped    chan struct{}
	scheduler  *workpoll.Scheduler
}

func NewActivationWorker(cfg ActivationConfig, deps ActivationDependencies) (*ActivationWorker, error) {
	if cfg.PollInterval <= 0 || cfg.LeaseTTL <= 0 || deps.Store == nil || deps.Handler == nil {
		return nil, errors.New("activation worker settings and dependencies are required")
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = defaultActivationRetryBase
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = defaultActivationRetryMax
	}
	if cfg.RetryMax < cfg.RetryBase {
		return nil, errors.New("activation retry maximum must not be below its base delay")
	}
	if cfg.MaxAttempts < 0 || cfg.MaxAttempts > 64 {
		return nil, errors.New("activation maximum attempts must be between 0 and 64")
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = defaultActivationMaxAttempts
	}
	if cfg.StuckThreshold < 0 {
		return nil, errors.New("activation stuck threshold must not be negative")
	}
	if cfg.StuckThreshold == 0 {
		cfg.StuckThreshold = 5 * time.Minute
	}
	if deps.Clock == nil {
		deps.Clock = port.SystemClock{}
	}
	if deps.Logger == nil {
		deps.Logger = noopLogger{}
	}
	if deps.Metrics == nil {
		deps.Metrics = port.NoopMetricRecorder{}
	}
	if deps.Scheduler == nil {
		deps.Scheduler, _ = workpoll.New(cfg.PollInterval, workpoll.Options{})
	}
	return &ActivationWorker{
		cfg:        cfg,
		store:      deps.Store,
		handler:    deps.Handler,
		clock:      deps.Clock,
		logger:     deps.Logger,
		metrics:    deps.Metrics,
		owner:      "activation-worker_" + randomID(),
		stopClaims: make(chan struct{}),
		stopped:    make(chan struct{}),
		scheduler:  deps.Scheduler,
	}, nil
}

func (w *ActivationWorker) Run(ctx context.Context) {
	if w == nil {
		return
	}
	defer close(w.stopped)
	w.scheduler.Run(ctx, w.stopClaims, func() bool {
		worked, err := w.processOne(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			w.logProcessingError(err)
		}
		if _, err := w.SnapshotHealth(ctx, w.clock.Now().UTC()); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Error("external-agent activation health snapshot failed", "error_code", activationErrorCode(err))
		}
		return worked
	})
}

// StopAdmission prevents new claims while allowing the current activation to
// finish or reach its durable retry/unknown decision.
func (w *ActivationWorker) StopAdmission() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.stopClaims) })
}

// WaitStopped waits for the polling loop and its current activation to stop.
func (w *ActivationWorker) WaitStopped(ctx context.Context) error {
	if w == nil {
		return nil
	}
	select {
	case <-w.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ProcessOne claims and makes one durable activation decision. A nil error
// means either no work was available or the claimed work reached a durable
// state; persistence conflicts remain visible to the caller.
func (w *ActivationWorker) ProcessOne(ctx context.Context) error {
	_, err := w.processOne(ctx)
	return err
}

func (w *ActivationWorker) processOne(ctx context.Context) (bool, error) {
	if w == nil {
		return false, errors.New("activation worker is unavailable")
	}
	now := w.clock.Now().UTC()
	activation, err := w.store.ClaimNextActivation(ctx, now, w.owner, w.cfg.LeaseTTL)
	if err != nil {
		w.recordCASConflict(err)
		return false, err
	}
	if activation == nil {
		activation = w.claimActivationFallback(ctx, now)
	}
	if activation == nil {
		return false, nil
	}
	w.metrics.AddCounter(domain.MetricExternalAgentActivationClaimTotal, 1, port.MetricLabels{"activation_outcome": "claimed"})
	if activation.State == domain.ActivationModelStarted || activation.State == domain.ActivationResponsePrepared {
		w.metrics.AddCounter(domain.MetricExternalAgentActivationReconcileTotal, 1, port.MetricLabels{"activation_outcome": boundedActivationOutcome(activation.State)})
	}
	var processErr error
	switch activation.State {
	case domain.ActivationPending, domain.ActivationProcessing:
		processErr = w.processBeforeModel(ctx, activation)
	case domain.ActivationModelStarted, domain.ActivationResponsePrepared:
		processErr = w.reconcileAfterModel(ctx, activation)
	case domain.ActivationCompleted:
		processErr = nil
	case domain.ActivationFailed, domain.ActivationCompletionUnknown:
		processErr = w.processFallback(ctx, activation)
	default:
		processErr = wrapActivationError(activation, errors.New("activation state is invalid"))
	}
	if processErr != nil {
		w.recordCASConflict(processErr)
	}
	w.recordActivationOutcome(ctx, activation)
	return true, processErr
}

// claimActivationFallback claims terminal fallback work only when both the
// store and the handler support it. A handler without a fallback publisher
// never claims these rows, and the rows keep their pending fallback state.
func (w *ActivationWorker) claimActivationFallback(ctx context.Context, now time.Time) *domain.ExternalAgentJobActivation {
	if _, ok := w.handler.(port.ActivationFallbackPublisher); !ok {
		return nil
	}
	store, ok := w.store.(port.ExternalAgentJobActivationFallbackStore)
	if !ok {
		return nil
	}
	activation, err := store.ClaimNextActivationFallback(ctx, now, w.owner, w.cfg.LeaseTTL)
	if err != nil {
		w.recordCASConflict(err)
		return nil
	}
	if activation != nil {
		w.metrics.AddCounter(domain.MetricExternalAgentActivationClaimTotal, 1, port.MetricLabels{"activation_outcome": "fallback_claimed"})
	}
	return activation
}

func (w *ActivationWorker) processFallback(ctx context.Context, activation *domain.ExternalAgentJobActivation) error {
	if activation == nil || !activation.FallbackRequired || activation.FallbackSlackTS != "" {
		return nil
	}
	publisher, ok := w.handler.(port.ActivationFallbackPublisher)
	if !ok {
		return nil
	}
	// Fallback publication is one bounded Slack publish; the claim lease
	// expires on failure so the row is reclaimed without renewal. Terminal
	// states are never renewed by the lease store.
	return publisher.PublishActivationFallback(ctx, *activation)
}

func (w *ActivationWorker) processBeforeModel(ctx context.Context, activation *domain.ExternalAgentJobActivation) error {
	handlerErr := w.invokeWithLease(ctx, activation, w.handler.HandleJobCompletion)
	current, err := w.store.GetActivation(context.WithoutCancel(ctx), activation.ActivationID)
	if err != nil {
		return wrapActivationError(activation, err)
	}
	if current == nil {
		return wrapActivationError(activation, errors.New("activation disappeared after processing"))
	}
	switch current.State {
	case domain.ActivationModelStarted, domain.ActivationResponsePrepared:
		// A handler may return an error after crossing the durable model
		// boundary. Reconcile the persisted state; never invoke the normal
		// handler again.
		return w.reconcileAfterModel(context.WithoutCancel(ctx), current)
	case domain.ActivationCompleted, domain.ActivationCompletionUnknown, domain.ActivationFailed:
		return nil
	case domain.ActivationPending, domain.ActivationProcessing:
		if handlerErr == nil {
			handlerErr = errors.New("activation handler returned without a durable state transition")
		}
		return w.resolveBeforeModelFailure(ctx, current, handlerErr)
	default:
		return wrapActivationError(current, errors.New("activation state is invalid after processing"))
	}
}

func (w *ActivationWorker) reconcileAfterModel(ctx context.Context, activation *domain.ExternalAgentJobActivation) error {
	reconcileErr := w.invokeWithLease(ctx, activation, w.handler.ReconcileJobCompletion)
	current, err := w.store.GetActivation(context.WithoutCancel(ctx), activation.ActivationID)
	if err != nil {
		return wrapActivationError(activation, err)
	}
	if current == nil {
		return wrapActivationError(activation, errors.New("activation disappeared during reconciliation"))
	}
	switch current.State {
	case domain.ActivationCompleted, domain.ActivationCompletionUnknown, domain.ActivationFailed:
		// The handler's durable terminal update is authoritative even if its
		// caller observed a provider error after committing it.
		return nil
	case domain.ActivationModelStarted:
		var classified *port.ActivationProcessError
		if reconcileErr != nil && errors.As(reconcileErr, &classified) && classified.Retryable {
			// A busy conversation or a transient durable-session read must not
			// turn an otherwise recoverable model boundary into unknown. The
			// lease expiry will make this activation reclaimable without replay.
			return wrapActivationError(current, reconcileErr)
		}
		// No durable final response can be proven. This is terminal and must
		// not be retried because retrying could execute the model twice.
		if err := w.store.MarkActivationCompletionUnknown(context.WithoutCancel(ctx), current, "completion_unknown", w.clock.Now().UTC()); err != nil {
			return wrapActivationError(current, err)
		}
		return nil
	case domain.ActivationResponsePrepared:
		if reconcileErr == nil {
			reconcileErr = errors.New("activation response remains unpublished")
		}
		if _, retryable := classifyActivationError(reconcileErr); !retryable {
			code, _ := classifyActivationError(reconcileErr)
			if err := w.store.FailActivation(context.WithoutCancel(ctx), current, code, w.clock.Now().UTC()); err != nil {
				return wrapActivationError(current, err)
			}
			return nil
		}
		// response_prepared is retried only by lease expiry/reconciliation;
		// it is never moved through the pre-model backoff path.
		return wrapActivationError(current, reconcileErr)
	case domain.ActivationPending, domain.ActivationProcessing:
		return wrapActivationError(current, errors.New("activation reconciliation crossed an invalid state"))
	default:
		return wrapActivationError(current, errors.New("activation state is invalid during reconciliation"))
	}
}

func (w *ActivationWorker) resolveBeforeModelFailure(ctx context.Context, activation *domain.ExternalAgentJobActivation, processErr error) error {
	code, retryable := classifyActivationError(processErr)
	now := w.clock.Now().UTC()
	if retryable {
		if activation.Attempt >= w.cfg.MaxAttempts {
			return w.failBeforeModelTerminal(ctx, activation, "activation_retry_exhausted", now)
		}
		next := now.Add(w.retryDelay(activation.Attempt))
		if err := w.store.RetryActivation(context.WithoutCancel(ctx), activation, code, next, now); err != nil {
			return wrapActivationError(activation, err)
		}
		return nil
	}
	return w.failBeforeModelTerminal(ctx, activation, code, now)
}

func (w *ActivationWorker) failBeforeModelTerminal(ctx context.Context, activation *domain.ExternalAgentJobActivation, code string, now time.Time) error {
	if err := w.store.FailActivation(context.WithoutCancel(ctx), activation, code, now); err != nil {
		return wrapActivationError(activation, err)
	}
	return nil
}

func (w *ActivationWorker) invokeWithLease(ctx context.Context, activation *domain.ExternalAgentJobActivation, invoke func(context.Context, domain.ExternalAgentJobActivation) error) error {
	if activation == nil {
		return errors.New("activation is unavailable")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	leaseStore, hasLease := w.store.(port.ExternalAgentJobActivationLeaseStore)
	if !hasLease {
		return invoke(runCtx, *activation)
	}
	done := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		w.renewLease(runCtx, leaseStore, activation, done, cancel)
	}()
	err := invoke(runCtx, *activation)
	close(done)
	<-heartbeatDone
	return err
}

func (w *ActivationWorker) renewLease(
	ctx context.Context,
	store port.ExternalAgentJobActivationLeaseStore,
	activation *domain.ExternalAgentJobActivation,
	done <-chan struct{},
	cancel context.CancelFunc,
) {
	interval := w.cfg.LeaseTTL / 3
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
			if err := store.RenewActivationLease(context.WithoutCancel(ctx), activation, w.clock.Now().UTC(), w.cfg.LeaseTTL); err != nil {
				cancel()
				return
			}
		}
	}
}

// SnapshotHealth exposes bounded activation state and updates the worker's
// stuck gauge. The optional store contract keeps health independent from the
// claim/processing path.
func (w *ActivationWorker) SnapshotHealth(ctx context.Context, now time.Time) (domain.ExternalAgentJobActivationHealth, error) {
	if w == nil || w.store == nil {
		return domain.ExternalAgentJobActivationHealth{}, errors.New("activation health store is unavailable")
	}
	store, ok := w.store.(port.ExternalAgentJobActivationHealthStore)
	if !ok {
		return domain.ExternalAgentJobActivationHealth{}, errors.New("activation health store is unavailable")
	}
	health, err := store.ActivationHealth(ctx, now, w.cfg.StuckThreshold)
	if err != nil {
		return domain.ExternalAgentJobActivationHealth{}, err
	}
	w.metrics.SetGauge(domain.MetricExternalAgentActivationStuck, int64(health.Stuck), nil)
	return health, nil
}

func (w *ActivationWorker) recordActivationOutcome(ctx context.Context, claimed *domain.ExternalAgentJobActivation) {
	if w == nil || claimed == nil {
		return
	}
	current, err := w.store.GetActivation(context.WithoutCancel(ctx), claimed.ActivationID)
	if err != nil || current == nil {
		return
	}
	w.metrics.AddCounter(domain.MetricExternalAgentActivationTotal, 1, port.MetricLabels{
		"activation_outcome": boundedActivationOutcome(current.State),
		"terminal_status":    boundedTerminalStatus(current.TerminalStatus),
	})
}

func (w *ActivationWorker) recordCASConflict(err error) {
	if w == nil || (!errors.Is(err, port.ErrActivationStateConflict) && !errors.Is(err, port.ErrActivationClaimConflict)) {
		return
	}
	w.metrics.AddCounter(domain.MetricExternalAgentActivationCASConflictTotal, 1, nil)
}

func (w *ActivationWorker) retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := min(attempt-1, 20)
	delay := float64(w.cfg.RetryBase) * math.Pow(2, float64(shift))
	if delay >= float64(w.cfg.RetryMax) {
		return w.cfg.RetryMax
	}
	return time.Duration(delay)
}

func classifyActivationError(err error) (string, bool) {
	if err == nil {
		return "activation_retryable", true
	}
	if classified, ok := errors.AsType[*port.ActivationProcessError](err); ok {
		code := safeActivationErrorCode(classified.Code, classified.Retryable)
		return code, classified.Retryable
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"actor revoked", "actor_revoked", "access revoked", "access_revoked", "actor_not_allowed",
		"access denied", "access_denied", "unauthorized", "forbidden", "destination invalid",
		"destination_invalid", "result_destination_mismatch", "identity invalid", "identity_invalid",
		"activation_identity_invalid", "result_identity_invalid", "artifact invalid", "artifact_invalid",
		"result_artifact_missing", "result_artifact_owner_ref_mismatch", "result_artifact_bytes_mismatch",
		"result_artifact_digest_mismatch", "result_artifact_invalid",
		"activation_artifact_invalid", "confirmation not allowed", "confirmation_not_allowed",
		"activation_confirmation_not_allowed", "pending confirmation", "pending_confirmation",
	} {
		if strings.Contains(message, marker) {
			return "activation_failed", false
		}
	}
	return "activation_retryable", true
}

func safeActivationErrorCode(code string, retryable bool) string {
	code = strings.TrimSpace(code)
	if code == "" {
		if retryable {
			return "activation_retryable"
		}
		return "activation_failed"
	}
	if len(code) > 128 {
		return "activation_retryable"
	}
	for _, r := range code {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return "activation_retryable"
		}
	}
	return code
}

func activationErrorCode(err error) string {
	if errors.Is(err, port.ErrActivationStateConflict) || errors.Is(err, port.ErrActivationClaimConflict) {
		return "activation_state_conflict"
	}
	code, _ := classifyActivationError(err)
	return code
}

func (w *ActivationWorker) logProcessingError(err error) {
	if w == nil {
		return
	}
	args := []any{"error_code", activationErrorCode(err)}
	var processingErr *activationProcessingError
	if errors.As(err, &processingErr) && processingErr.activation != nil {
		activation := processingErr.activation
		args = append(args,
			"activation_id", activation.ActivationID,
			"job_id", activation.JobID,
			"status_revision", activation.StatusRevision,
			"kind", boundedResultKind(activation.Kind),
			"attempt", activation.Attempt,
		)
	}
	w.logger.Error("external-agent activation processing failed", args...)
}

func boundedActivationOutcome(state domain.ExternalAgentJobActivationState) string {
	switch state {
	case domain.ActivationPending, domain.ActivationProcessing, domain.ActivationModelStarted,
		domain.ActivationResponsePrepared, domain.ActivationCompleted, domain.ActivationCompletionUnknown,
		domain.ActivationFailed:
		return string(state)
	default:
		return "unknown"
	}
}

func boundedTerminalStatus(status domain.ExternalAgentJobStatus) string {
	switch status {
	case domain.JobCompleted, domain.JobFailed, domain.JobCancelled, domain.JobCompletionUnknown, domain.JobAbandoned:
		return string(status)
	default:
		return "unknown"
	}
}

type activationProcessingError struct {
	activation *domain.ExternalAgentJobActivation
	err        error
}

func (e *activationProcessingError) Error() string {
	if e == nil || e.err == nil {
		return "external-agent activation processing failed"
	}
	return e.err.Error()
}

func (e *activationProcessingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func wrapActivationError(activation *domain.ExternalAgentJobActivation, err error) error {
	if err == nil {
		return nil
	}
	if activation == nil {
		return err
	}
	return &activationProcessingError{activation: activation, err: err}
}
