package externalagent

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// progressFlushInterval is the implementation constant for coalesced durable
// writes. Repeated chunks and usage updates persist at most once per interval
// per active job.
const progressFlushInterval = 30 * time.Second

// progressWriteTimeout bounds one synchronous final flush.
const progressWriteTimeout = 5 * time.Second

// ActiveProgressGauge tracks the in-process count of active monitored jobs.
// The gauge value is absolute, so every recorder shares one holder.
type ActiveProgressGauge struct {
	mu      sync.Mutex
	count   int64
	metrics port.MetricRecorder
}

func NewActiveProgressGauge(metrics port.MetricRecorder) *ActiveProgressGauge {
	if metrics == nil {
		metrics = port.NoopMetricRecorder{}
	}
	return &ActiveProgressGauge{metrics: metrics}
}

func (g *ActiveProgressGauge) inc() {
	g.mu.Lock()
	g.count++
	g.metrics.SetGauge(domain.MetricExternalAgentACPActiveJobs, g.count, nil)
	g.mu.Unlock()
}

func (g *ActiveProgressGauge) dec() {
	g.mu.Lock()
	g.count--
	if g.count < 0 {
		g.count = 0
	}
	g.metrics.SetGauge(domain.MetricExternalAgentACPActiveJobs, g.count, nil)
	g.mu.Unlock()
}

// ProgressRecorder folds content-free ACP events into a per-job projection,
// persists phase boundaries immediately and coalesced activity at most once
// per flush interval, and emits one passive inactivity warning per silent
// episode. It never cancels, retries, closes, loads, resumes, or transitions
// a job. Monitoring failure is observable but never fails the ACP invocation.
type ProgressRecorder struct {
	store     port.ExternalAgentJobProgressStore
	registry  port.ACPProcessRegistry
	clock     port.Clock
	logger    port.Logger
	metrics   port.MetricRecorder
	gauge     *ActiveProgressGauge
	warnAfter time.Duration

	jobID     string
	owner     string
	attempt   int
	sessionID string
	pid       int

	mu        sync.Mutex
	proj      domain.ExternalAgentJobProgress
	lastFlush time.Time
	warned    bool
	started   bool
	closed    bool
	flushCh   chan struct{}
	stopCh    chan struct{}
	stopped   chan struct{}
}

// NewProgressRecorder creates the recorder bound to one job attempt. The
// owner is the job lease owner captured at claim time.
func NewProgressRecorder(store port.ExternalAgentJobProgressStore, registry port.ACPProcessRegistry, clock port.Clock, logger port.Logger, metrics port.MetricRecorder, gauge *ActiveProgressGauge, warnAfter time.Duration, jobID, owner string, attempt int) *ProgressRecorder {
	if clock == nil {
		clock = systemClock{}
	}
	if logger == nil {
		logger = noopLogger{}
	}
	if metrics == nil {
		metrics = port.NoopMetricRecorder{}
	}
	if warnAfter <= 0 {
		warnAfter = 900 * time.Second
	}
	if gauge == nil {
		gauge = NewActiveProgressGauge(metrics)
	}
	return &ProgressRecorder{
		store: store, registry: registry, clock: clock, logger: logger, metrics: metrics,
		gauge: gauge, warnAfter: warnAfter, jobID: jobID, owner: owner, attempt: attempt,
		proj: domain.ExternalAgentJobProgress{
			JobID: jobID, Attempt: attempt, Phase: domain.ACPPhaseStarting,
		},
		flushCh: make(chan struct{}, 1), stopCh: make(chan struct{}), stopped: make(chan struct{}),
	}
}

// SetSessionID records the persisted session ID in memory only, for
// correlated lifecycle logs. It is never part of the durable projection.
func (r *ProgressRecorder) SetSessionID(sessionID string) {
	r.mu.Lock()
	changed := sessionID != "" && sessionID != r.sessionID
	r.sessionID = sessionID
	attempt := r.attempt
	r.mu.Unlock()
	if changed {
		r.logger.Info("external-agent ACP session established",
			"job_id", r.jobID, "attempt", attempt, "acp_session_id", sessionID, "phase", domain.ACPPhaseSessionReady)
	}
}

// Start launches the per-job monitor goroutine. It is safe to call once.
func (r *ProgressRecorder) Start(ctx context.Context) {
	r.mu.Lock()
	if r.started || r.closed {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.mu.Unlock()
	if r.gauge != nil {
		r.gauge.inc()
	}
	interval := r.warnAfter / 3
	if interval <= 0 || interval > progressFlushInterval {
		interval = progressFlushInterval
	}
	go func() {
		defer close(r.stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.stopCh:
				return
			case <-r.flushCh:
				r.maintenance(true)
			case <-ticker.C:
				r.maintenance(false)
			}
		}
	}()
}

// Record folds one content-free event into the in-memory projection. The ACP
// reader is never blocked on SQLite: durable writes happen on the monitor
// goroutine or during Close.
func (r *ProgressRecorder) Record(event domain.ACPProgressEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	now := r.clock.Now().UTC()
	if event.Kind == domain.ACPEventProcessStarted && event.PID > 0 && r.registry != nil {
		r.registry.Register(r.jobID, r.attempt, event.PID)
		r.pid = event.PID
	}
	if event.Kind == domain.ACPEventProcessStarted {
		r.logger.Info("external-agent ACP process started",
			"job_id", r.jobID, "attempt", r.attempt, "pid", event.PID, "phase", domain.ACPPhaseStarting)
	}
	previousPhase := r.proj.Phase
	r.proj.Apply(event, now)
	r.proj.UpdatedAt = now
	r.metrics.AddCounter(domain.MetricExternalAgentACPProgressEventTotal, 1, port.MetricLabels{
		"event_kind": string(r.proj.LastEventKind),
	})
	if r.proj.Phase != previousPhase {
		r.metrics.AddCounter(domain.MetricExternalAgentACPPhaseTransitionTotal, 1, port.MetricLabels{
			"phase": string(r.proj.Phase),
		})
		r.logger.Debug("external-agent ACP phase transition",
			"job_id", r.jobID, "attempt", r.attempt, "phase", r.proj.Phase, "last_event_kind", r.proj.LastEventKind)
	}
	if r.immediate(event) || r.proj.Phase != previousPhase {
		r.signalFlush()
	}
}

func (r *ProgressRecorder) immediate(event domain.ACPProgressEvent) bool {
	switch event.Kind {
	case domain.ACPEventProcessStarted, domain.ACPEventSessionNew,
		domain.ACPEventPromptSent, domain.ACPEventPermissionRequested,
		domain.ACPEventPermissionResponded, domain.ACPEventPromptResponse,
		domain.ACPEventProcessFailed:
		return true
	case domain.ACPEventToolCallUpdate:
		return event.Tool != nil && event.Tool.Status == domain.ACPToolStatusTerminal
	default:
		return false
	}
}

// Close stops the monitor and performs the final synchronous flush when the
// projection is not already terminal. It is safe to call more than once.
func (r *ProgressRecorder) Close() {
	r.mu.Lock()
	already := r.closed
	r.closed = true
	waitStopped := r.started
	r.mu.Unlock()
	if already {
		return
	}
	close(r.stopCh)
	if waitStopped {
		<-r.stopped
	}
	r.flushSynchronous()
	if r.gauge != nil {
		r.gauge.dec()
	}
}

// RecordAndClose folds a terminal event and flushes it synchronously. It is
// used by hosts that observe prompt completion outside the monitor path.
func (r *ProgressRecorder) RecordAndClose(event domain.ACPProgressEvent) {
	r.Record(event)
	r.Close()
}

func (r *ProgressRecorder) signalFlush() {
	select {
	case r.flushCh <- struct{}{}:
	default:
	}
}

// maintenance runs on the monitor goroutine: evaluate the passive warning and
// flush when the event was immediate or the flush interval elapsed. Durable
// writes never happen while the projection mutex is held, so a slow SQLite
// write can never block the ACP reader path.
func (r *ProgressRecorder) maintenance(immediate bool) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	now := r.clock.Now().UTC()
	health := DeriveProgressHealth(r.proj, now, r.warnAfter, r.processAliveLocked(), false)
	switch {
	case health == domain.ACPHealthPossiblyStalled && !r.warned:
		r.warned = true
		idle := now.Sub(r.proj.LastTransportActivityAt)
		if idle < 0 {
			idle = 0
		}
		r.logger.Warn("external-agent ACP possibly stalled",
			"job_id", r.jobID, "attempt", r.attempt, "acp_session_id", r.sessionID,
			"phase", r.proj.Phase, "last_event_kind", r.proj.LastEventKind,
			"idle_seconds", int(idle.Seconds()), "pid", r.pid)
		r.metrics.AddCounter(domain.MetricExternalAgentACPInactivityWarningTotal, 1, port.MetricLabels{
			"health": string(health),
		})
	case health != domain.ACPHealthPossiblyStalled && r.warned:
		// Re-arm only after new activity.
		r.warned = false
	}
	shouldFlush := immediate || r.lastFlush.IsZero() || now.Sub(r.lastFlush) >= progressFlushInterval
	if !shouldFlush {
		r.mu.Unlock()
		return
	}
	proj := r.proj
	proj.UpdatedAt = now
	r.mu.Unlock()
	r.flushNow(proj)
}

func (r *ProgressRecorder) processAliveLocked() *bool {
	if r.registry == nil {
		return nil
	}
	return r.registry.ProcessAlive(r.jobID, r.attempt)
}

func (r *ProgressRecorder) flushSynchronous() {
	r.mu.Lock()
	proj := r.proj
	proj.UpdatedAt = r.clock.Now().UTC()
	r.mu.Unlock()
	r.flushNow(proj)
}

// flushNow writes one projection snapshot. It never holds the projection
// mutex during the write; a slow store degrades only the recorder, never the
// ACP reader.
func (r *ProgressRecorder) flushNow(proj domain.ExternalAgentJobProgress) {
	if r.store == nil {
		r.metrics.AddCounter(domain.MetricExternalAgentACPProgressPersistFailureTotal, 1, port.MetricLabels{"outcome": "unconfigured"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), progressWriteTimeout)
	defer cancel()
	if err := r.store.WriteJobProgress(ctx, r.jobID, r.owner, r.attempt, proj); err != nil {
		r.metrics.AddCounter(domain.MetricExternalAgentACPProgressPersistFailureTotal, 1, port.MetricLabels{"outcome": "write_failed"})
		r.logger.Warn("external-agent ACP progress persist failed",
			"job_id", r.jobID, "attempt", r.attempt, "error_class", progressFailureClass(err))
		return
	}
	r.mu.Lock()
	r.lastFlush = proj.UpdatedAt
	r.mu.Unlock()
}

func progressFailureClass(err error) string {
	var acpErr *domain.ACPError
	if errors.As(err, &acpErr) && acpErr.Code != "" {
		return string(acpErr.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "write_timeout"
	}
	return "write_failed"
}

// DeriveProgressHealth is the read-time and warning-time health policy. It
// never changes job state. A missing process handle reports uncertainty, not
// death, so it cannot claim possibly_stalled on its own. A durable terminal
// job is always terminal, even when its projection shows a failed phase
// without a stop reason.
func DeriveProgressHealth(proj domain.ExternalAgentJobProgress, now time.Time, warnAfter time.Duration, alive *bool, jobTerminal bool) domain.ACPProgressHealth {
	if warnAfter <= 0 {
		warnAfter = 900 * time.Second
	}
	if jobTerminal {
		return domain.ACPHealthTerminal
	}
	switch proj.Phase {
	case domain.ACPPhaseCompleted, domain.ACPPhaseCancelled:
		return domain.ACPHealthTerminal
	case domain.ACPPhaseFailed:
		if proj.StopReason == "" {
			// The process or transport ended without a terminal prompt response.
			return domain.ACPHealthDisconnected
		}
		return domain.ACPHealthTerminal
	}
	if proj.Phase == "" || proj.Phase == domain.ACPPhaseStarting || proj.Phase == domain.ACPPhaseSessionReady {
		return domain.ACPHealthActive
	}
	if alive != nil && !*alive {
		return domain.ACPHealthDisconnected
	}
	if !proj.LastMeaningfulProgressAt.IsZero() && now.Sub(proj.LastMeaningfulProgressAt) < warnAfter {
		return domain.ACPHealthActive
	}
	if alive != nil && *alive && now.Sub(proj.LastTransportActivityAt) >= warnAfter {
		return domain.ACPHealthPossiblyStalled
	}
	return domain.ACPHealthQuiet
}
