package knowledge

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// ProjectionWorkerConfig bounds the durable knowledge projection worker.
type ProjectionWorkerConfig struct {
	Interval      time.Duration
	MaxRetries    int
	RetentionDays int
	OutputDir     string
}

// ProjectionWorkerDependencies carries the batch store, the snapshot reader,
// and the shared serialized projector. Enabled is the authoritative gate
// state; when nil the worker is always enabled. The worker never re-executes
// a knowledge mutation: it only reads committed state and renders files.
type ProjectionWorkerDependencies struct {
	Store     port.KnowledgeProjectionStore
	Reader    port.ProjectionReader
	Projector port.OKFProjector
	Clock     port.Clock
	Logger    port.Logger
	Sanitize  func(string) string
	Enabled   func() bool
}

// ProjectionWorker polls the knowledge projection outbox, coalesces every
// due trigger into one snapshot render, and publishes through the shared
// projector. Retries preserve the attempt counter; after the retry budget a
// batch fails terminal with a stable bounded code. Cancellation stops the
// ticker and processing without marking an in-flight batch failed: the
// lease expires and another run recovers it.
type ProjectionWorker struct {
	cfg       ProjectionWorkerConfig
	store     port.KnowledgeProjectionStore
	reader    port.ProjectionReader
	projector port.OKFProjector
	clock     port.Clock
	logger    port.Logger
	sanitize  func(string) string
	enabled   func() bool
}

const (
	projectionPanicBackoff        = time.Second
	projectionMaxPanicBackoff     = 30 * time.Second
	projectionRetryBaseDelay      = time.Minute
	projectionRetryMaxDelayShifts = 5
)

func NewProjectionWorker(cfg ProjectionWorkerConfig, deps ProjectionWorkerDependencies) (*ProjectionWorker, error) {
	if cfg.Interval <= 0 || cfg.MaxRetries <= 0 || cfg.RetentionDays < 0 {
		return nil, errors.New("knowledge projection worker settings are invalid")
	}
	if strings.TrimSpace(cfg.OutputDir) == "" {
		return nil, errors.New("knowledge projection output directory is required")
	}
	if deps.Store == nil || deps.Reader == nil || deps.Projector == nil {
		return nil, errors.New("knowledge projection worker dependencies are incomplete")
	}
	if deps.Logger == nil {
		return nil, errors.New("logger is required")
	}
	if deps.Clock == nil {
		deps.Clock = port.SystemClock{}
	}
	if deps.Sanitize == nil {
		deps.Sanitize = func(value string) string { return value }
	}
	return &ProjectionWorker{
		cfg: cfg, store: deps.Store, reader: deps.Reader, projector: deps.Projector,
		clock: deps.Clock, logger: deps.Logger, sanitize: deps.Sanitize, enabled: deps.Enabled,
	}, nil
}

// enabledNow reports the authoritative gate state. A nil gate means the
// worker was started with knowledge enabled and stays enabled.
func (w *ProjectionWorker) enabledNow() bool {
	if w.enabled == nil {
		return true
	}
	return w.enabled()
}

// Run supervises the periodic worker and restarts it after a panic with
// bounded exponential backoff. Context cancellation stops both the worker
// and the retry wait.
func (w *ProjectionWorker) Run(ctx context.Context) {
	backoff := projectionPanicBackoff
	for {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					w.logger.Error("knowledge projection worker panicked, will retry",
						"panic", w.sanitize(fmt.Sprintf("%v", recovered)),
						"stack", w.sanitize(string(debug.Stack())))
				}
			}()
			w.runWorker(ctx)
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, projectionMaxPanicBackoff)
	}
}

func (w *ProjectionWorker) runWorker(ctx context.Context) {
	// Startup recovery removes promotion residue (leftover staging or
	// backup directories) without requiring a knowledge mutation, so
	// forgotten content from an interrupted cleanup never survives a
	// restart. It is mutex-serialized with promotions by the projector.
	w.recover(ctx)
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	// Immediate processing at startup drains whatever the previous run left
	// pending before the first poll tick.
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// recover runs projector recovery once at startup. A failure is logged
// sanitized and never blocks the worker: the next startup or the next
// promotion attempt retries the removal.
func (w *ProjectionWorker) recover(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if err := w.projector.Recover(w.cfg.OutputDir); err != nil {
		w.logger.Warn("knowledge projection startup recovery failed", "error", w.sanitize(err.Error()))
	}
}

func (w *ProjectionWorker) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if !w.enabledNow() {
		return
	}
	if w.cfg.RetentionDays > 0 {
		cutoff := w.clock.Now().UTC().AddDate(0, 0, -w.cfg.RetentionDays)
		if err := w.store.CleanupProjection(ctx, cutoff); err != nil {
			w.logger.Warn("knowledge projection cleanup failed", "error", w.sanitize(err.Error()))
		}
	}
	w.processBatches(ctx)
}

// processBatches claims and renders due batches until none remain, so a
// mutation committed during a render leaves a pending trigger that forces a
// second snapshot in the same run.
func (w *ProjectionWorker) processBatches(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		items, err := w.store.ClaimProjectionBatch(ctx)
		if err != nil {
			w.logger.Error("knowledge projection batch claim failed", "error", w.sanitize(err.Error()))
			return
		}
		if len(items) == 0 {
			return
		}
		w.processBatch(ctx, items)
	}
}

func (w *ProjectionWorker) processBatch(ctx context.Context, items []domain.KnowledgeProjectionItem) {
	ids := make([]int, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	leaseUntil := items[0].LeaseUntil

	if err := w.projector.Project(ctx, w.reader, w.cfg.OutputDir); err != nil {
		if ctx.Err() != nil {
			// Cancellation during a projection must not mark the batch
			// failed: the lease expires and a later run recovers it.
			w.logger.Debug("knowledge projection interrupted by shutdown; lease recovery will re-render", "batch", len(ids))
			return
		}
		if errors.Is(err, port.ErrProjectionCleanup) {
			// The live bundle is consistent (the promoted snapshot or the
			// restored previous bundle), but promotion residue cleanup
			// failed and the residue can retain forgotten content. The
			// batch must not complete and must never exhaust through the
			// normal projection budget: cleanup retries repeat until the
			// residue is actually removed.
			w.logger.Warn("knowledge projection residue cleanup incomplete; batch kept pending", "batch", len(ids), "error", w.sanitize(err.Error()))
			w.deferCleanupBatch(ctx, items, leaseUntil)
			return
		}
		w.logger.Error("knowledge projection failed", "error", w.sanitize(err.Error()))
		w.settleBatch(ctx, items, leaseUntil)
		return
	}

	if err := w.store.CompleteProjectionBatch(ctx, ids, leaseUntil); err != nil {
		// A completion CAS conflict means another claimant owns the rows or
		// the lease expired. The batch stays processing until lease
		// recovery; it is never silently lost.
		w.logger.Warn("knowledge projection completion conflicted; lease recovery will re-render", "batch", len(ids), "error", w.sanitize(err.Error()))
		return
	}
	w.logger.Debug("knowledge projection batch published", "triggers", len(ids))
}

// deferCleanupBatch reschedules the whole claimed batch after a
// ErrProjectionCleanup failure: the live bundle is consistent, but
// promotion residue (backup or staging) remains. Unlike settleBatch it
// never fails rows terminal and never consumes the projection attempt
// budget; every row is deferred at a fixed short delay, and each retry
// starts by removing the residue before re-rendering. Cleanup retries
// repeat until the residue is removed.
func (w *ProjectionWorker) deferCleanupBatch(ctx context.Context, items []domain.KnowledgeProjectionItem, leaseUntil time.Time) {
	ids := make([]int, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	next := w.clock.Now().UTC().Add(projectionRetryBaseDelay)
	if err := w.store.DeferProjectionCleanupBatch(ctx, ids, leaseUntil, next); err != nil {
		// A deferral CAS conflict means the lease was lost; lease recovery
		// picks the rows up on a later run.
		w.logger.Warn("knowledge projection cleanup deferral conflicted; lease recovery will re-render", "batch", len(ids), "error", w.sanitize(err.Error()))
	}
}

// settleBatch partitions the claimed batch by retry budget: rows that
// exhausted their attempts fail terminal with a stable bounded code, the
// rest are rescheduled preserving their attempt counter. Retries are
// grouped by attempt count so every row gets its own backoff schedule: a
// fresh trigger coalesced with an older row never inherits the older row's
// delay. A failed row never marks any other trigger done.
func (w *ProjectionWorker) settleBatch(ctx context.Context, items []domain.KnowledgeProjectionItem, leaseUntil time.Time) {
	var failIDs []int
	retryGroups := make(map[int][]int)
	for _, item := range items {
		if item.Attempts > w.cfg.MaxRetries {
			failIDs = append(failIDs, item.ID)
			continue
		}
		retryGroups[item.Attempts] = append(retryGroups[item.Attempts], item.ID)
	}
	if len(failIDs) > 0 {
		if err := w.store.FailProjectionBatch(ctx, failIDs, leaseUntil, port.KnowledgeProjectionExhaustedCode); err != nil {
			w.logger.Warn("knowledge projection terminal failure transition failed", "error", w.sanitize(err.Error()))
		} else {
			w.logger.Error("knowledge projection batch failed terminal", "triggers", len(failIDs))
		}
	}
	attempts := make([]int, 0, len(retryGroups))
	for attempt := range retryGroups {
		attempts = append(attempts, attempt)
	}
	sort.Ints(attempts)
	now := w.clock.Now().UTC()
	for _, attempt := range attempts {
		next := now.Add(projectionRetryDelay(attempt))
		if err := w.store.RetryProjectionBatch(ctx, retryGroups[attempt], leaseUntil, next); err != nil {
			w.logger.Warn("knowledge projection retry transition failed", "error", w.sanitize(err.Error()))
		}
	}
}

func projectionRetryDelay(attempts int) time.Duration {
	shifts := min(max(attempts-1, 0), projectionRetryMaxDelayShifts)
	return projectionRetryBaseDelay * time.Duration(1<<shifts)
}
