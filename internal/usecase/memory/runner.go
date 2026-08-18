package memory

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type RunnerConfig struct {
	Interval      time.Duration
	MaxRetries    int
	RetentionDays int
	MemoryDir     string
}

type RunnerDependencies struct {
	Store            port.MemoryWorkerStore
	ExchangeFinder   port.AssistantExchangeFinder
	Curator          port.MemoryCurator
	Memory           *Service
	Projector        port.OKFProjector
	ProjectionReader port.ProjectionReader
	Clock            port.Clock
	Logger           port.Logger
	Sanitize         func(string) string
}

type Runner struct {
	cfg       RunnerConfig
	store     port.MemoryWorkerStore
	finder    port.AssistantExchangeFinder
	curator   port.MemoryCurator
	memory    *Service
	projector port.OKFProjector
	reader    port.ProjectionReader
	clock     port.Clock
	logger    port.Logger
	sanitize  func(string) string
}

const (
	initialPanicBackoff = time.Second
	maxPanicBackoff     = 30 * time.Second
)

func NewRunner(cfg RunnerConfig, deps RunnerDependencies) (*Runner, error) {
	if cfg.Interval <= 0 || cfg.MaxRetries <= 0 || cfg.RetentionDays < 0 {
		return nil, errors.New("memory runner settings are invalid")
	}
	if deps.Store == nil || deps.ExchangeFinder == nil || deps.Curator == nil || deps.Memory == nil || deps.Projector == nil || deps.ProjectionReader == nil {
		return nil, errors.New("memory runner dependencies are incomplete")
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
	return &Runner{cfg: cfg, store: deps.Store, finder: deps.ExchangeFinder, curator: deps.Curator, memory: deps.Memory, projector: deps.Projector, reader: deps.ProjectionReader, clock: deps.Clock, logger: deps.Logger, sanitize: deps.Sanitize}, nil
}

// Run supervises the periodic worker and restarts it after a panic with bounded
// exponential backoff. Context cancellation stops both the worker and retry wait.
func (r *Runner) Run(ctx context.Context) {
	backoff := initialPanicBackoff
	for {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					r.logger.Error("memory curator panicked, will retry", "panic", r.sanitize(fmt.Sprintf("%v", recovered)), "stack", r.sanitize(string(debug.Stack())))
				}
			}()
			r.runWorker(ctx)
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = nextPanicBackoff(backoff)
	}
}

func nextPanicBackoff(current time.Duration) time.Duration {
	return min(current*2, maxPanicBackoff)
}

func (r *Runner) runWorker(ctx context.Context) {
	// Startup recovery removes promotion residue (leftover staging or
	// backup directories) without requiring an outbox item, so forgotten
	// content from an interrupted cleanup never survives a restart even
	// when no new trigger is pending. It is mutex-serialized with
	// promotions by the shared projector.
	if ctx.Err() == nil {
		if err := r.projector.Recover(r.cfg.MemoryDir); err != nil {
			r.logger.Warn("memory projection startup recovery failed", "error", r.sanitize(err.Error()))
		}
	}
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.store.ReconcileAssistantExchanges(ctx, r.finder); err != nil {
				r.logger.Warn("assistant exchange reconciliation failed", "error", err)
			}
			if err := r.store.CleanupOutbox(ctx, r.clock.Now().UTC().AddDate(0, 0, -r.cfg.RetentionDays)); err != nil {
				r.logger.Warn("memory outbox cleanup failed", "error", err)
			}
			r.processOutbox(ctx)
		}
	}
}

func (r *Runner) processOutbox(ctx context.Context) {
	for {
		item, err := r.store.ClaimNextOutboxItem(ctx)
		if err != nil {
			r.logger.Error("memory outbox claim failed", "error", err)
			return
		}
		if item == nil {
			return
		}

		messages, err := r.store.LoadOutboxMessages(ctx, item)
		if err != nil {
			r.logger.Error("memory outbox load messages failed", "item_id", item.ID, "error", err)
			r.retryOutbox(ctx, item, err)
			return
		}
		if len(messages) == 0 {
			err := errors.New("source exchange is no longer available")
			r.logger.Warn("memory outbox source exchange unavailable", "item_id", item.ID)
			r.retryOutbox(ctx, item, err)
			return
		}

		trusted, err := r.memory.TrustedEntityOperations(ctx, item.ConversationKey, messages)
		if err != nil {
			r.logger.Warn("trusted entity topic lookup failed", "item_id", item.ID, "error", err)
			r.retryOutbox(ctx, item, err)
			return
		}
		topics, err := r.memory.RelevantTopics(ctx, messages)
		if err != nil {
			r.logger.Warn("memory curator topic lookup failed", "item_id", item.ID, "error", err)
			r.retryOutbox(ctx, item, err)
			return
		}
		patch, err := r.curator.ProposePatch(ctx, item.ConversationKey, item.ExchangeTS, messages, topics)
		if err != nil {
			if errors.Is(err, port.ErrModelCallLimitReached) {
				if len(trusted) > 0 {
					r.logger.Debug("memory curator skipped by shared model-call limit; applying trusted entity operations", "item_id", item.ID)
				} else {
					r.logger.Debug("memory curator deferred by shared model-call limit", "item_id", item.ID)
					if rescheduleErr := r.store.RescheduleOutboxItem(ctx, item.ID, item.LeaseUntil, r.clock.Now().UTC()); rescheduleErr != nil {
						r.logger.Warn("memory curator reschedule failed", "item_id", item.ID, "error", rescheduleErr)
					}
					return
				}
			}
			if errors.Is(err, port.ErrCuratorResponseIncomplete) && len(trusted) == 0 {
				r.logger.Warn("memory curator response incomplete; discarding optional patch", "item_id", item.ID, "error", err)
				patch = domain.MemoryPatch{ConversationKey: item.ConversationKey, ExchangeTS: item.ExchangeTS}
			} else if len(trusted) == 0 {
				r.logger.Warn("memory curator proposal failed", "item_id", item.ID, "error", err)
				r.retryOutbox(ctx, item, err)
				return
			}
			if len(trusted) > 0 {
				r.logger.Warn("memory curator proposal failed; applying trusted entity operations", "item_id", item.ID, "error", err)
				patch = domain.MemoryPatch{ConversationKey: item.ConversationKey, ExchangeTS: item.ExchangeTS}
			}
		}
		patch.Operations = mergeTrustedEntityOperations(trusted, patch.Operations)
		for _, message := range messages {
			if message.Role == domain.RoleUser && message.UserID != "" {
				patch.SourceAuthorID = message.UserID
				break
			}
		}
		if err := r.memory.validatePatch(patch); err != nil {
			if len(trusted) > 0 {
				r.logger.Warn("optional curator patch rejected; applying trusted entity operations", "item_id", item.ID, "error", err)
				patch.Operations = trusted
			} else {
				r.logger.Warn("optional curator patch rejected; discarding", "item_id", item.ID, "error", err)
				patch.Operations = nil
			}
		}
		if _, applyErr := r.memory.ValidateAndApply(ctx, patch); applyErr != nil {
			r.logger.Warn("memory patch validation failed", "item_id", item.ID, "error", applyErr)
			r.retryOutbox(ctx, item, applyErr)
			return
		}
		if err := r.projector.Project(ctx, r.reader, r.cfg.MemoryDir); err != nil {
			if ctx.Err() != nil {
				// Cancellation during a projection must not settle the
				// item: the lease expires and a later run recovers it.
				r.logger.Debug("memory projection interrupted by shutdown; lease recovery will re-render", "item_id", item.ID)
				return
			}
			if errors.Is(err, port.ErrProjectionCleanup) {
				// The live bundle is consistent (the promoted snapshot or
				// the restored previous bundle), but promotion residue
				// cleanup failed and the residue can retain forgotten
				// content. The item must not complete and must never
				// exhaust through the normal retry budget: reschedule
				// attempt-neutrally until the residue is actually removed.
				r.logger.Warn("memory projection residue cleanup incomplete; outbox item kept pending", "item_id", item.ID, "error", r.sanitize(err.Error()))
				if rescheduleErr := r.store.RescheduleOutboxItem(ctx, item.ID, item.LeaseUntil, r.clock.Now().UTC()); rescheduleErr != nil {
					r.logger.Warn("memory projection cleanup reschedule failed", "item_id", item.ID, "error", rescheduleErr)
				}
				return
			}
			r.logger.Error("memory projection failed", "error", err)
			r.retryOutbox(ctx, item, err)
			return
		}
		if err := r.store.CompleteOutboxItem(ctx, item.ID, item.LeaseUntil); err != nil {
			r.logger.Warn("memory outbox completion failed", "item_id", item.ID, "error", err)
			return
		}
		r.logger.Debug("memory curator processed exchange", "item_id", item.ID, "operations", len(patch.Operations))
	}
}

func (r *Runner) retryOutbox(ctx context.Context, item *domain.OutboxItem, cause error) {
	if item.Attempts >= r.cfg.MaxRetries {
		_ = r.store.FailOutboxItem(ctx, item.ID, item.LeaseUntil, cause.Error())
		return
	}
	delay := time.Minute * time.Duration(1<<min(item.Attempts-1, 5))
	_ = r.store.RetryOutboxItem(ctx, item.ID, item.LeaseUntil, r.clock.Now().UTC().Add(delay))
}

func mergeTrustedEntityOperations(trusted, proposed []domain.MemoryOp) []domain.MemoryOp {
	if len(trusted) == 0 {
		return proposed
	}
	trustedSlugs := make(map[string]struct{}, len(trusted))
	for _, op := range trusted {
		trustedSlugs[op.TopicSlug] = struct{}{}
	}
	result := append([]domain.MemoryOp(nil), trusted...)
	for _, op := range proposed {
		if _, superseded := trustedSlugs[op.TopicSlug]; !superseded {
			result = append(result, op)
		}
	}
	return result
}
