package knowledge

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

// LexicalWorkerConfig bounds the durable lexical index worker.
type LexicalWorkerConfig struct {
	Interval   time.Duration
	MaxRetries int
	BatchSize  int
	Limits     domain.KnowledgeRetrievalLimits
}

// LexicalWorkerDependencies carries the lexical queue, the private
// authoritative source, the worker-owned index surface, the document
// resolver, the identity lister, the shared redactor, and the injected
// clock. Sanitize is the last-mile logger scrubber: worker logs never carry
// identities, content, handles, or digests beyond bounded counts and codes.
type LexicalWorkerDependencies struct {
	Queue     port.KnowledgeQueueStore
	Source    port.KnowledgeIndexSource
	Index     port.KnowledgeLexicalIndex
	Resolver  port.KnowledgeDocumentResolver
	Lister    port.KnowledgeIdentityLister
	Clock     port.Clock
	Logger    port.Logger
	Sanitize  func(string) string
	Redact    func(string) string
	Metrics   port.MetricRecorder
	Scheduler *workpoll.Scheduler
}

// QueueDepthSource is the consumer-owned content-free queue depth surface
// sampled by the lexical worker: depth counts pending plus processing rows
// only, excluding the terminal done and failed states. The embedding depth
// is sampled through the same surface so the embedding queue table stays
// private to its store adapter.
type QueueDepthSource interface {
	LexicalDepth(ctx context.Context) (int, error)
	EmbeddingDepth(ctx context.Context) (int, error)
}

// LexicalWorker claims generation-CAS queue work, rebuilds one identity's
// lexical index rows from authoritative truth, and completes only the
// claimed generation. Missing, deleted, ineligible, or redacted-empty items
// remove index rows and complete; unverifiable documents fail with the
// closed source_invalid code; transient SQLite failures retry with finite
// delay and exhaust to attempts_exhausted. A concurrent truth mutation
// bumps the queue generation so the stale claimant's completion conflicts
// and the fresh work survives. Cancellation stops the worker without
// marking in-flight claims failed.
type LexicalWorker struct {
	cfg       LexicalWorkerConfig
	queue     port.KnowledgeQueueStore
	source    port.KnowledgeIndexSource
	index     port.KnowledgeLexicalIndex
	resolver  port.KnowledgeDocumentResolver
	lister    port.KnowledgeIdentityLister
	clock     port.Clock
	logger    port.Logger
	sanitize  func(string) string
	redact    func(string) string
	metrics   port.MetricRecorder
	stopped   chan struct{}
	stopOnce  sync.Once
	scheduler *workpoll.Scheduler
}

const (
	lexicalWorkerPanicBackoff    = time.Second
	lexicalWorkerMaxPanicBackoff = 30 * time.Second
	lexicalClaimLease            = time.Minute
	lexicalRetryBaseDelay        = 10 * time.Second
	lexicalRetryMaxDelayShifts   = 4
	lexicalWorkerKinds           = 3
)

func NewLexicalWorker(cfg LexicalWorkerConfig, deps LexicalWorkerDependencies) (*LexicalWorker, error) {
	if cfg.Interval <= 0 || cfg.MaxRetries <= 0 {
		return nil, errors.New("knowledge lexical worker settings are invalid")
	}
	if cfg.BatchSize < 1 || cfg.BatchSize > domain.HardMaxKnowledgeRetrievalWorkerBatchSize {
		return nil, errors.New("knowledge lexical worker batch size is not bounded")
	}
	if err := cfg.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("knowledge lexical worker limits are invalid: %w", err)
	}
	if deps.Queue == nil || deps.Source == nil || deps.Index == nil || deps.Resolver == nil || deps.Lister == nil {
		return nil, errors.New("knowledge lexical worker dependencies are incomplete")
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
	if deps.Scheduler == nil {
		deps.Scheduler, _ = workpoll.New(cfg.Interval, workpoll.Options{})
	}
	return &LexicalWorker{
		cfg: cfg, queue: deps.Queue, source: deps.Source, index: deps.Index,
		resolver: deps.Resolver, lister: deps.Lister, clock: deps.Clock,
		logger: deps.Logger, sanitize: deps.Sanitize, redact: deps.Redact,
		metrics:   deps.Metrics,
		stopped:   make(chan struct{}),
		scheduler: deps.Scheduler,
	}, nil
}

// WaitStopped blocks until Run returns. A nil worker returns immediately.
func (w *LexicalWorker) WaitStopped(ctx context.Context) error {
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

// Run supervises the worker: startup reconciliation, an immediate drain
// tick, then periodic ticks until the context is cancelled. Panics are
// recovered with bounded exponential backoff; Run returns only when the
// context is done.
func (w *LexicalWorker) Run(ctx context.Context) {
	defer w.stopOnce.Do(func() { close(w.stopped) })
	backoff := lexicalWorkerPanicBackoff
	for {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					w.logger.Error("knowledge lexical worker panicked, will retry",
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
		backoff = min(backoff*2, lexicalWorkerMaxPanicBackoff)
	}
}

func (w *LexicalWorker) runWorker(ctx context.Context) {
	if err := w.Reconcile(ctx); err != nil && ctx.Err() == nil {
		w.logger.Warn("knowledge lexical reconciliation failed", "error", w.sanitize(err.Error()))
	}
	w.scheduler.Run(ctx, nil, func() bool { return w.tick(ctx) })
}

// tick claims and processes a bounded fair batch per kind. Claim failures
// stop this tick; per-item failures settle through the queue retry budget.
// Both closed queue depth gauges are sampled once per tick, after the
// batch: depth counts pending plus processing rows only.
func (w *LexicalWorker) tick(ctx context.Context) bool {
	found := false
	for _, kind := range []domain.KnowledgeRetrievalItemKind{
		domain.KnowledgeRetrievalClaim,
		domain.KnowledgeRetrievalPreference,
		domain.KnowledgeRetrievalDocument,
	} {
		found = w.drainKind(ctx, kind) || found
	}
	w.recordDepths(ctx)
	return found
}

// recordDepths samples the lexical and embedding queue depths through the
// consumer-owned count surface. Depth excludes terminal done and failed
// rows; sampling failures are silent because the gauge keeps its last
// value and worker failures surface through the queue claim path.
func (w *LexicalWorker) recordDepths(ctx context.Context) {
	if w == nil || w.metrics == nil || ctx.Err() != nil {
		return
	}
	source, ok := w.queue.(QueueDepthSource)
	if !ok {
		return
	}
	if depth, err := source.LexicalDepth(ctx); err == nil && depth >= 0 {
		w.metrics.SetGauge(domain.MetricKnowledgeLexicalQueueDepth, int64(depth), nil)
	}
	if depth, err := source.EmbeddingDepth(ctx); err == nil && depth >= 0 {
		w.metrics.SetGauge(domain.MetricKnowledgeEmbeddingQueueDepth, int64(depth), nil)
	}
}

// drainKind processes up to BatchSize items of one kind with fresh claims,
// so a single item that keeps conflicting cannot starve the tick forever.
func (w *LexicalWorker) drainKind(ctx context.Context, kind domain.KnowledgeRetrievalItemKind) bool {
	found := false
	for claimed := 0; claimed < w.cfg.BatchSize; claimed++ {
		if ctx.Err() != nil {
			return found
		}
		item, ok, err := w.queue.ClaimNext(ctx, kind, w.clock.Now(), lexicalClaimLease)
		if err != nil {
			w.logger.Error("knowledge lexical queue claim failed", "error", w.sanitize(err.Error()))
			return found
		}
		if !ok {
			return found
		}
		found = true
		w.processItem(ctx, item)
	}
	return found
}

// processItem rebuilds one identity's lexical index rows and settles the
// claim. Every branch ends in Complete, Retry, or Fail on the exact claim
// token, so a reclaimed claim can never be mutated by a stale worker.
func (w *LexicalWorker) processItem(ctx context.Context, item domain.KnowledgeQueueItem) {
	claim := domain.KnowledgeQueueClaim{Kind: item.Kind, ID: item.ID, Generation: item.Generation, LeaseUntil: item.LeaseUntil}
	if err := w.indexItem(ctx, item); err != nil {
		if ctx.Err() != nil {
			// Shutdown must not fail an in-flight claim: the lease expires
			// and a later run recovers it.
			return
		}
		var sourceErr *sourceInvalidError
		if errors.As(err, &sourceErr) {
			if failErr := w.queue.Fail(ctx, claim, domain.KnowledgeQueueFailureSourceInvalid); failErr != nil {
				w.logger.Warn("knowledge lexical terminal failure transition failed", "error", w.sanitize(failErr.Error()))
			}
			return
		}
		if item.Attempts >= w.cfg.MaxRetries {
			if failErr := w.queue.Fail(ctx, claim, domain.KnowledgeQueueFailureAttemptsExhausted); failErr != nil {
				w.logger.Warn("knowledge lexical terminal failure transition failed", "error", w.sanitize(failErr.Error()))
			}
			return
		}
		next := w.clock.Now().UTC().Add(lexicalRetryDelay(item.Attempts))
		if retryErr := w.queue.Retry(ctx, claim, next); retryErr != nil {
			w.logger.Warn("knowledge lexical retry transition failed", "error", w.sanitize(retryErr.Error()))
		}
		return
	}
	if err := w.queue.Complete(ctx, claim); err != nil {
		// A completion CAS conflict means a concurrent mutation bumped the
		// generation; the fresh claim will replace our index rows. Any
		// other failure leaves the row processing until lease recovery.
		if errors.Is(err, port.ErrKnowledgeCASConflict) {
			w.logger.Debug("knowledge lexical completion conflicted; fresh generation will re-index", "kind", string(item.Kind))
			return
		}
		w.logger.Warn("knowledge lexical completion failed; lease recovery will re-index", "error", w.sanitize(err.Error()))
	}
}

// sourceInvalidError marks an unverifiable document so the worker fails the
// claim with the closed source_invalid code after removing index rows.
type sourceInvalidError struct{ err error }

func (e *sourceInvalidError) Error() string { return e.err.Error() }
func (e *sourceInvalidError) Unwrap() error { return e.err }

// indexItem rebuilds one identity's lexical rows from current truth. It
// never writes authoritative knowledge and never logs content or digests.
func (w *LexicalWorker) indexItem(ctx context.Context, item domain.KnowledgeQueueItem) error {
	authoritative, err := w.source.ReadIndexSource(ctx, item.Kind, item.ID)
	if errors.Is(err, port.ErrKnowledgeNotFound) {
		// Missing or deleted truth: remove any stale index rows and
		// complete successfully.
		if deleteErr := w.index.DeleteLexical(ctx, item.Kind, item.ID); deleteErr != nil {
			return deleteErr
		}
		return nil
	}
	if err != nil {
		return err
	}
	eligible, err := lexicalIndexable(authoritative)
	if err != nil {
		return &sourceInvalidError{err: err}
	}
	if !eligible {
		if deleteErr := w.index.DeleteLexical(ctx, item.Kind, item.ID); deleteErr != nil {
			return deleteErr
		}
		return nil
	}
	var content string
	if authoritative.Document != nil {
		resolved, resolveErr := w.resolver.Resolve(ctx, *authoritative.Document, w.cfg.Limits)
		if resolveErr != nil {
			// Unverifiable documents remove old rows and reach the bounded
			// failed queue state; they never serve stale content.
			if deleteErr := w.index.DeleteLexical(ctx, item.Kind, item.ID); deleteErr != nil {
				return deleteErr
			}
			return &sourceInvalidError{err: fmt.Errorf("document content is unverifiable: %w", resolveErr)}
		}
		content = string(resolved)
	}
	text, err := BuildKnowledgeIndexText(item.Kind, authoritative, content, w.redact)
	if err != nil {
		return err
	}
	if text.SourceDigest == "" {
		// Text that redacts to empty removes the index rows and completes.
		if deleteErr := w.index.DeleteLexical(ctx, item.Kind, item.ID); deleteErr != nil {
			return deleteErr
		}
		return nil
	}
	revision := authoritativeRevision(item.Kind, authoritative)
	return w.index.ReplaceLexical(ctx, item.Kind, item.ID, revision, text.SourceDigest, text.Subject, text.Body)
}

// lexicalIndexable reports whether one authoritative item is eligible for
// lexical indexing. Validity windows stay query-time predicates; only
// durable status gates indexing here.
func lexicalIndexable(item port.KnowledgeAuthoritativeItem) (bool, error) {
	switch item.Kind {
	case domain.KnowledgeRetrievalClaim:
		switch item.Claim.Status {
		case domain.KnowledgeClaimAsserted, domain.KnowledgeClaimVerified, domain.KnowledgeClaimDisputed:
			return true, nil
		default:
			return false, nil
		}
	case domain.KnowledgeRetrievalPreference:
		return item.Preference.Status == domain.KnowledgePreferenceActive, nil
	case domain.KnowledgeRetrievalDocument:
		return item.Document.Status == domain.KnowledgeDocumentActive, nil
	default:
		return false, fmt.Errorf("unknown indexable item kind %q", item.Kind)
	}
}

func authoritativeRevision(kind domain.KnowledgeRetrievalItemKind, item port.KnowledgeAuthoritativeItem) int {
	switch kind {
	case domain.KnowledgeRetrievalClaim:
		return item.Claim.Revision
	case domain.KnowledgeRetrievalPreference:
		return item.Preference.Revision
	case domain.KnowledgeRetrievalDocument:
		return item.Document.Revision
	default:
		return 0
	}
}

// Reconcile pages truth, index, and queue identities per kind and enqueues
// missing or stale truth, missing index rows, revision-mismatched index
// rows, and orphan index identities. It returns without waiting for queue
// drains and never reads content.
func (w *LexicalWorker) Reconcile(ctx context.Context) error {
	for _, kind := range []domain.KnowledgeRetrievalItemKind{
		domain.KnowledgeRetrievalClaim,
		domain.KnowledgeRetrievalPreference,
		domain.KnowledgeRetrievalDocument,
	} {
		if err := w.reconcileKind(ctx, kind); err != nil {
			return err
		}
	}
	return nil
}

func (w *LexicalWorker) reconcileKind(ctx context.Context, kind domain.KnowledgeRetrievalItemKind) error {
	pageSize := w.cfg.BatchSize
	truthRevisions := make(map[string]int)
	truthIDs := make([]string, 0)
	for afterID := ""; ; {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		page, err := w.lister.ListTruthIdentities(ctx, kind, afterID, pageSize)
		if err != nil {
			return fmt.Errorf("list truth identities: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, identity := range page {
			truthRevisions[identity.ID] = identity.Revision
			truthIDs = append(truthIDs, identity.ID)
		}
		if len(page) < pageSize {
			break
		}
		afterID = page[len(page)-1].ID
	}
	indexRows := make(map[string]port.KnowledgeLexicalIndexRow)
	for afterID := ""; ; {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		page, err := w.index.ListLexical(ctx, kind, afterID, pageSize)
		if err != nil {
			return fmt.Errorf("list lexical index identities: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			indexRows[row.ID] = row
		}
		if len(page) < pageSize {
			break
		}
		afterID = page[len(page)-1].ID
	}
	truthSet := make(map[string]bool, len(truthIDs))
	for _, id := range truthIDs {
		truthSet[id] = true
	}
	enqueue := func(id string) error {
		if _, err := w.queue.Enqueue(ctx, kind, id); err != nil {
			return fmt.Errorf("enqueue reconciliation identity: %w", err)
		}
		return nil
	}
	// Every current truth identity is re-enqueued unconditionally: the
	// worker rebuilds index rows idempotently, so queue rows that appear
	// current are still refreshed and a redactor rotation (which changes
	// the redacted source digest without advancing the truth revision)
	// cannot leave stale FTS text behind. The generation bump is bounded
	// and a matching rebuild replaces rows with identical content.
	for id := range truthRevisions {
		if err := enqueue(id); err != nil {
			return err
		}
	}
	for id := range indexRows {
		if !truthSet[id] {
			// Orphan index identities are enqueued so the worker removes
			// them safely; they are never deleted during reconciliation.
			if err := enqueue(id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Rebuild removes every reconstructible lexical index row and re-enqueues
// every truth identity. It never modifies claims, preferences, documents,
// revisions, tombstones, or the projection outbox.
func (w *LexicalWorker) Rebuild(ctx context.Context) error {
	if err := w.index.ClearLexical(ctx); err != nil {
		return fmt.Errorf("clear lexical index: %w", err)
	}
	for _, kind := range []domain.KnowledgeRetrievalItemKind{
		domain.KnowledgeRetrievalClaim,
		domain.KnowledgeRetrievalPreference,
		domain.KnowledgeRetrievalDocument,
	} {
		for afterID := ""; ; {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			page, err := w.lister.ListTruthIdentities(ctx, kind, afterID, w.cfg.BatchSize)
			if err != nil {
				return fmt.Errorf("list truth identities for rebuild: %w", err)
			}
			if len(page) == 0 {
				break
			}
			for _, identity := range page {
				if _, err := w.queue.Enqueue(ctx, kind, identity.ID); err != nil {
					return fmt.Errorf("enqueue rebuild identity: %w", err)
				}
			}
			if len(page) < w.cfg.BatchSize {
				break
			}
			afterID = page[len(page)-1].ID
		}
	}
	return nil
}

// lexicalRetryDelay is finite and bounded by attempts.
func lexicalRetryDelay(attempts int) time.Duration {
	shifts := min(max(attempts-1, 0), lexicalRetryMaxDelayShifts)
	return lexicalRetryBaseDelay * time.Duration(1<<shifts)
}
