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
		done := make(chan struct{})
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					r.logger.Error("memory curator panicked, will retry", "panic", r.sanitize(fmt.Sprintf("%v", recovered)), "stack", r.sanitize(string(debug.Stack())))
				}
				close(done)
			}()
			r.runWorker(ctx)
		}()
		<-done

		select {
		case <-ctx.Done():
			return
		default:
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
		backoff = nextPanicBackoff(backoff)
	}
}

func nextPanicBackoff(current time.Duration) time.Duration {
	return min(current*2, maxPanicBackoff)
}

func (r *Runner) runWorker(ctx context.Context) {
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
			r.errorOutboxRetry(ctx, item, "memory outbox load messages failed", err, "item_id", item.ID, "error", err)
			return
		}
		if len(messages) == 0 {
			err := errors.New("source exchange is no longer available")
			r.warnOutboxRetry(ctx, item, "memory outbox source exchange unavailable", err, "item_id", item.ID)
			return
		}

		trusted, err := r.memory.TrustedEntityOperations(ctx, item.ConversationKey, messages)
		if err != nil {
			r.warnOutboxRetry(ctx, item, "trusted entity topic lookup failed", err, "item_id", item.ID, "error", err)
			return
		}
		topics, err := r.memory.RelevantTopics(ctx, messages)
		if err != nil {
			r.warnOutboxRetry(ctx, item, "memory curator topic lookup failed", err, "item_id", item.ID, "error", err)
			return
		}
		patch, err := r.curator.ProposePatch(ctx, item.ConversationKey, item.ExchangeTS, messages, topics)
		if err != nil {
			if errors.Is(err, port.ErrModelCallLimitReached) {
				if len(trusted) > 0 {
					r.logger.Debug("memory curator skipped by shared model-call limit; applying trusted entity operations", "item_id", item.ID)
				} else {
					r.logger.Debug("memory curator deferred by shared model-call limit", "item_id", item.ID)
					if rescheduleErr := r.rescheduleOutbox(ctx, item); rescheduleErr != nil {
						r.logger.Warn("memory curator reschedule failed", "item_id", item.ID, "error", rescheduleErr)
					}
					return
				}
			}
			if errors.Is(err, port.ErrCuratorResponseIncomplete) && len(trusted) == 0 {
				r.logger.Warn("memory curator response incomplete; discarding optional patch", "item_id", item.ID, "error", err)
				patch = domain.MemoryPatch{ConversationKey: item.ConversationKey, ExchangeTS: item.ExchangeTS}
			} else if len(trusted) == 0 {
				r.warnOutboxRetry(ctx, item, "memory curator proposal failed", err, "item_id", item.ID, "error", err)
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
		if err := r.memory.Validate(patch); err != nil {
			if len(trusted) > 0 {
				r.logger.Warn("optional curator patch rejected; applying trusted entity operations", "item_id", item.ID, "error", err)
				patch.Operations = trusted
			} else {
				r.logger.Warn("optional curator patch rejected; discarding", "item_id", item.ID, "error", err)
				patch.Operations = nil
			}
		}
		if _, applyErr := r.memory.ValidateAndApply(ctx, patch); applyErr != nil {
			r.warnOutboxRetry(ctx, item, "memory patch validation failed", applyErr, "item_id", item.ID, "error", applyErr)
			return
		}
		if err := r.projector.Project(ctx, r.reader, r.cfg.MemoryDir); err != nil {
			r.errorOutboxRetry(ctx, item, "memory projection failed", err, "error", err)
			return
		}
		if err := r.store.CompleteOutboxItem(ctx, item.ID, item.LeaseUntil); err != nil {
			r.logger.Warn("memory outbox completion failed", "item_id", item.ID, "error", err)
			return
		}
		r.logger.Debug("memory curator processed exchange", "item_id", item.ID, "operations", len(patch.Operations))
	}
}

func (r *Runner) warnOutboxRetry(ctx context.Context, item *domain.OutboxItem, msg string, cause error, logArgs ...any) {
	r.logger.Warn(msg, logArgs...)
	r.retryOutbox(ctx, item, cause)
}

func (r *Runner) errorOutboxRetry(ctx context.Context, item *domain.OutboxItem, msg string, cause error, logArgs ...any) {
	r.logger.Error(msg, logArgs...)
	r.retryOutbox(ctx, item, cause)
}

func (r *Runner) retryOutbox(ctx context.Context, item *domain.OutboxItem, cause error) {
	if item.Attempts >= r.cfg.MaxRetries {
		_ = r.store.FailOutboxItem(ctx, item.ID, item.LeaseUntil, cause.Error())
		return
	}
	delay := time.Minute * time.Duration(1<<min(item.Attempts-1, 5))
	_ = r.store.RetryOutboxItem(ctx, item.ID, item.LeaseUntil, r.clock.Now().UTC().Add(delay))
}

func (r *Runner) rescheduleOutbox(ctx context.Context, item *domain.OutboxItem) error {
	return r.store.RescheduleOutboxItem(ctx, item.ID, item.LeaseUntil, r.clock.Now().UTC())
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
