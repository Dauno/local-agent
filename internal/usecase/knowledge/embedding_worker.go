package knowledge

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

// EmbeddingWorkerConfig bounds the durable embedding worker. ProviderID,
// Model, and Dimensions identify the resolved provider contract and are
// used once at construction to compute the frozen model fingerprint.
type EmbeddingWorkerConfig struct {
	Interval   time.Duration
	MaxRetries int
	BatchSize  int
	Limits     domain.KnowledgeRetrievalLimits
	ProviderID string
	Model      string
	Dimensions int
}

// EmbeddingWorkerDependencies carries the embedding queue, the private
// authoritative source, the worker-owned vector index, the embedding
// provider, the document resolver, the identity lister, the shared redactor,
// and the injected clock. Sanitize is the last-mile logger scrubber.
type EmbeddingWorkerDependencies struct {
	Queue     port.KnowledgeQueueStore
	Source    port.KnowledgeIndexSource
	Index     port.KnowledgeVectorIndex
	Provider  port.EmbeddingProvider
	Resolver  port.KnowledgeDocumentResolver
	Lister    port.KnowledgeIdentityLister
	Clock     port.Clock
	Logger    port.Logger
	Sanitize  func(string) string
	Redact    func(string) string
	Metrics   port.MetricRecorder
	Scheduler *workpoll.Scheduler
}

// EmbeddingWorker claims generation-CAS embedding queue work, rebuilds one
// batch of identities' vector rows from authoritative truth through one
// bounded provider call, and completes only the claimed generations. Items
// whose current-fingerprint row already carries the same revision and
// source digest skip provider contact entirely. Missing, deleted,
// ineligible, or redacted-empty items remove vector rows and complete;
// unverifiable documents fail with the closed source_invalid code; malformed
// provider output fails terminal with provider_invalid; transient failures
// retry with finite delay and exhaust to attempts_exhausted. Cancellation
// stops the worker without marking in-flight claims failed.
type EmbeddingWorker struct {
	cfg         EmbeddingWorkerConfig
	queue       port.KnowledgeQueueStore
	source      port.KnowledgeIndexSource
	index       port.KnowledgeVectorIndex
	provider    port.EmbeddingProvider
	resolver    port.KnowledgeDocumentResolver
	lister      port.KnowledgeIdentityLister
	fingerprint string
	clock       port.Clock
	logger      port.Logger
	sanitize    func(string) string
	redact      func(string) string
	metrics     port.MetricRecorder
	stopped     chan struct{}
	stopOnce    sync.Once
	scheduler   *workpoll.Scheduler
}

const (
	embeddingWorkerPanicBackoff    = time.Second
	embeddingWorkerMaxPanicBackoff = 30 * time.Second
	embeddingClaimLease            = time.Minute
	embeddingRetryBaseDelay        = 10 * time.Second
	embeddingRetryMaxDelayShifts   = 4
)

func NewEmbeddingWorker(cfg EmbeddingWorkerConfig, deps EmbeddingWorkerDependencies) (*EmbeddingWorker, error) {
	if cfg.Interval <= 0 || cfg.MaxRetries <= 0 {
		return nil, errors.New("knowledge embedding worker settings are invalid")
	}
	if cfg.BatchSize < 1 || cfg.BatchSize > domain.HardMaxKnowledgeRetrievalWorkerBatchSize {
		return nil, errors.New("knowledge embedding worker batch size is not bounded")
	}
	if err := cfg.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("knowledge embedding worker limits are invalid: %w", err)
	}
	if cfg.ProviderID == "" || utf8.RuneCountInString(cfg.ProviderID) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return nil, errors.New("knowledge embedding worker provider ID is not bounded")
	}
	if cfg.Model == "" || utf8.RuneCountInString(cfg.Model) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return nil, errors.New("knowledge embedding worker model is not bounded")
	}
	if cfg.Dimensions < 1 || cfg.Dimensions > domain.HardMaxKnowledgeEmbeddingDimensions {
		return nil, errors.New("knowledge embedding worker dimensions are not bounded")
	}
	if deps.Queue == nil || deps.Source == nil || deps.Index == nil || deps.Provider == nil || deps.Resolver == nil || deps.Lister == nil {
		return nil, errors.New("knowledge embedding worker dependencies are incomplete")
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
	return &EmbeddingWorker{
		cfg: cfg, queue: deps.Queue, source: deps.Source, index: deps.Index,
		provider: deps.Provider, resolver: deps.Resolver, lister: deps.Lister,
		fingerprint: domain.ModelFingerprint(cfg.ProviderID, cfg.Model, cfg.Dimensions),
		clock:       deps.Clock, logger: deps.Logger, sanitize: deps.Sanitize,
		redact: deps.Redact, metrics: deps.Metrics,
		stopped:   make(chan struct{}),
		scheduler: deps.Scheduler,
	}, nil
}

// WaitStopped blocks until Run returns. A nil worker returns immediately.
func (w *EmbeddingWorker) WaitStopped(ctx context.Context) error {
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
func (w *EmbeddingWorker) Run(ctx context.Context) {
	defer w.stopOnce.Do(func() { close(w.stopped) })
	backoff := embeddingWorkerPanicBackoff
	for {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					w.logger.Error("knowledge embedding worker panicked, will retry",
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
		backoff = min(backoff*2, embeddingWorkerMaxPanicBackoff)
	}
}

func (w *EmbeddingWorker) runWorker(ctx context.Context) {
	if err := w.Reconcile(ctx); err != nil && ctx.Err() == nil {
		w.logger.Warn("knowledge embedding reconciliation failed", "error", w.sanitize(err.Error()))
	}
	w.scheduler.Run(ctx, nil, func() bool { return w.tick(ctx) })
}

// tick claims a bounded batch per kind and processes each batch through one
// provider call, then samples the embedding queue depth gauge.
func (w *EmbeddingWorker) tick(ctx context.Context) bool {
	found := false
	for _, kind := range []domain.KnowledgeRetrievalItemKind{
		domain.KnowledgeRetrievalClaim,
		domain.KnowledgeRetrievalPreference,
		domain.KnowledgeRetrievalDocument,
	} {
		found = w.drainKind(ctx, kind) || found
	}
	w.recordDepth(ctx)
	return found
}

// recordDepth samples the embedding queue depth through the consumer-owned
// count surface. Sampling failures are silent because the gauge keeps its
// last value and worker failures surface through the queue claim path.
func (w *EmbeddingWorker) recordDepth(ctx context.Context) {
	if w == nil || w.metrics == nil || ctx.Err() != nil {
		return
	}
	source, ok := w.queue.(QueueDepthSource)
	if !ok {
		return
	}
	if depth, err := source.EmbeddingDepth(ctx); err == nil && depth >= 0 {
		w.metrics.SetGauge(domain.MetricKnowledgeEmbeddingQueueDepth, int64(depth), nil)
	}
}

// drainKind claims up to BatchSize items of one kind with fresh claims and
// processes them through a single batched provider call, so unchanged items
// skip provider contact without breaking the batch.
func (w *EmbeddingWorker) drainKind(ctx context.Context, kind domain.KnowledgeRetrievalItemKind) bool {
	var claims []domain.KnowledgeQueueItem
	for len(claims) < w.cfg.BatchSize {
		if ctx.Err() != nil {
			return len(claims) > 0
		}
		item, ok, err := w.queue.ClaimNext(ctx, kind, w.clock.Now(), embeddingClaimLease)
		if err != nil {
			w.logger.Error("knowledge embedding queue claim failed", "error", w.sanitize(err.Error()))
			return len(claims) > 0
		}
		if !ok {
			break
		}
		claims = append(claims, item)
	}
	if len(claims) == 0 {
		return false
	}
	w.processBatch(ctx, kind, claims)
	return true
}

// pendingEmbed is one claimed item prepared for provider contact: the claim
// token, the canonical redacted text, the authoritative revision, and the
// computed source digest.
type pendingEmbed struct {
	item     domain.KnowledgeQueueItem
	text     KnowledgeIndexText
	revision int
}

// processBatch prepares every claimed item, skips unchanged ones, issues one
// batched provider call for the rest, replaces the vector rows, and settles
// every claim. Every branch ends in Complete, Retry, or Fail on the exact
// claim token.
func (w *EmbeddingWorker) processBatch(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, claims []domain.KnowledgeQueueItem) {
	var pending []pendingEmbed
	for _, item := range claims {
		prepared, ok := w.prepare(ctx, item)
		if !ok {
			continue
		}
		pending = append(pending, prepared)
	}
	if len(pending) == 0 {
		return
	}

	current, err := w.currentVectorRows(ctx, kind, pending)
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Warn("knowledge embedding vector state read failed", "error", w.sanitize(err.Error()))
		}
		for _, prepared := range pending {
			w.settle(ctx, prepared.item, err)
		}
		return
	}
	toEmbed := make([]pendingEmbed, 0, len(pending))
	for _, prepared := range pending {
		if row, found := current[prepared.item.ID]; found && row.Revision == prepared.revision && row.SourceDigest == prepared.text.SourceDigest {
			// Unchanged item: no provider contact, complete directly.
			w.complete(ctx, prepared.item)
			continue
		}
		toEmbed = append(toEmbed, prepared)
	}
	if len(toEmbed) == 0 {
		return
	}

	inputs := make([]string, len(toEmbed))
	for index, prepared := range toEmbed {
		inputs[index] = embeddingInput(prepared.text)
	}
	embedStarted := w.clock.Now()
	vectors, err := w.provider.Embed(ctx, inputs)
	if err != nil {
		w.recordEmbeddingRequest(embedStarted, domain.KnowledgeRetrievalOutcomeUnavailable)
		if ctx.Err() != nil {
			return
		}
		for _, prepared := range toEmbed {
			w.settle(ctx, prepared.item, err)
		}
		return
	}
	w.recordEmbeddingRequest(embedStarted, domain.KnowledgeRetrievalOutcomeSuccess)
	if len(vectors) != len(toEmbed) {
		for _, prepared := range toEmbed {
			w.settle(ctx, prepared.item, &providerOutputError{err: fmt.Errorf("provider returned %d vectors for %d inputs", len(vectors), len(toEmbed))})
		}
		return
	}
	for index, prepared := range toEmbed {
		encoded, encodeErr := domain.NormalizeAndEncodeEmbedding(vectors[index], w.cfg.Dimensions)
		if encodeErr != nil {
			w.settle(ctx, prepared.item, &providerOutputError{err: encodeErr})
			continue
		}
		if err := w.index.ReplaceVector(ctx, prepared.item.Kind, prepared.item.ID, prepared.revision, prepared.text.SourceDigest, w.fingerprint, encoded); err != nil {
			w.settle(ctx, prepared.item, err)
			continue
		}
		w.complete(ctx, prepared.item)
	}
}

// prepare reads one claimed item's current truth and builds its canonical
// text, settling terminal source cases immediately. It returns false when
// the claim was already settled and must not enter the batch.
func (w *EmbeddingWorker) prepare(ctx context.Context, item domain.KnowledgeQueueItem) (pendingEmbed, bool) {
	authoritative, err := w.source.ReadIndexSource(ctx, item.Kind, item.ID)
	if errors.Is(err, port.ErrKnowledgeNotFound) {
		if deleteErr := w.index.DeleteVector(ctx, item.Kind, item.ID); deleteErr != nil {
			w.settle(ctx, item, deleteErr)
			return pendingEmbed{}, false
		}
		w.complete(ctx, item)
		return pendingEmbed{}, false
	}
	if err != nil {
		w.settle(ctx, item, err)
		return pendingEmbed{}, false
	}
	eligible, err := lexicalIndexable(authoritative)
	if err != nil {
		w.settle(ctx, item, &sourceInvalidError{err: err})
		return pendingEmbed{}, false
	}
	if !eligible {
		if deleteErr := w.index.DeleteVector(ctx, item.Kind, item.ID); deleteErr != nil {
			w.settle(ctx, item, deleteErr)
			return pendingEmbed{}, false
		}
		w.complete(ctx, item)
		return pendingEmbed{}, false
	}
	var content string
	if authoritative.Document != nil {
		resolved, resolveErr := w.resolver.Resolve(ctx, *authoritative.Document, w.cfg.Limits)
		if resolveErr != nil {
			if deleteErr := w.index.DeleteVector(ctx, item.Kind, item.ID); deleteErr != nil {
				w.settle(ctx, item, deleteErr)
				return pendingEmbed{}, false
			}
			w.settle(ctx, item, &sourceInvalidError{err: fmt.Errorf("document content is unverifiable: %w", resolveErr)})
			return pendingEmbed{}, false
		}
		content = string(resolved)
	}
	text, err := BuildKnowledgeIndexText(item.Kind, authoritative, content, w.redact)
	if err != nil {
		w.settle(ctx, item, err)
		return pendingEmbed{}, false
	}
	if text.SourceDigest == "" {
		if deleteErr := w.index.DeleteVector(ctx, item.Kind, item.ID); deleteErr != nil {
			w.settle(ctx, item, deleteErr)
			return pendingEmbed{}, false
		}
		w.complete(ctx, item)
		return pendingEmbed{}, false
	}
	return pendingEmbed{item: item, text: text, revision: authoritativeRevision(item.Kind, authoritative)}, true
}

// currentVectorRows reads the current-fingerprint vector row metadata for
// the claimed identities. The index pages in the same stable identity order
// the queue claims in, so the read stops once the last claimed identity has
// been passed.
func (w *EmbeddingWorker) currentVectorRows(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, pending []pendingEmbed) (map[string]port.KnowledgeVectorIndexRow, error) {
	target := make(map[string]bool, len(pending))
	lastID := ""
	for _, prepared := range pending {
		target[prepared.item.ID] = true
		if prepared.item.ID > lastID {
			lastID = prepared.item.ID
		}
	}
	rows := make(map[string]port.KnowledgeVectorIndexRow, len(pending))
	for afterID := ""; ; {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		page, err := w.index.ListVector(ctx, kind, w.fingerprint, afterID, domain.HardMaxKnowledgeQueueListLimit)
		if err != nil {
			return nil, fmt.Errorf("list vector index state: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			if target[row.ID] {
				rows[row.ID] = row
			}
		}
		if page[len(page)-1].ID >= lastID || len(page) < domain.HardMaxKnowledgeQueueListLimit {
			break
		}
		afterID = page[len(page)-1].ID
	}
	return rows, nil
}

// embeddingInput composes the canonical redacted embedding text from the
// subject and body of one index identity.
func embeddingInput(text KnowledgeIndexText) string {
	switch {
	case text.Subject == "":
		return text.Body
	case text.Body == "":
		return text.Subject
	default:
		return text.Subject + "\n" + text.Body
	}
}

// recordEmbeddingRequest observes the frozen embedding request duration
// metric with the closed outcome label around every batched provider call.
func (w *EmbeddingWorker) recordEmbeddingRequest(started time.Time, outcome domain.KnowledgeRetrievalOutcome) {
	if w == nil || w.metrics == nil {
		return
	}
	w.metrics.Observe(domain.MetricKnowledgeEmbeddingRequestDuration, w.clock.Now().Sub(started).Seconds(), port.MetricLabels{domain.MetricLabelOutcome: string(outcome)})
}

// providerOutputError marks malformed provider output so the worker fails
// the claim terminal with provider_invalid: a wrong-dimension, non-finite,
// or zero-norm vector will not fix itself.
type providerOutputError struct{ err error }

func (e *providerOutputError) Error() string { return e.err.Error() }
func (e *providerOutputError) Unwrap() error { return e.err }

// settle resolves one claim after an error. Provider-invalid output and
// source-invalid content fail terminal; cancellation leaves the claim for
// lease recovery; everything else retries with finite bounded delay and
// exhausts to attempts_exhausted.
func (w *EmbeddingWorker) settle(ctx context.Context, item domain.KnowledgeQueueItem, err error) {
	claim := domain.KnowledgeQueueClaim{Kind: item.Kind, ID: item.ID, Generation: item.Generation, LeaseUntil: item.LeaseUntil}
	if ctx.Err() != nil {
		return
	}
	var providerErr *providerOutputError
	if errors.As(err, &providerErr) {
		if failErr := w.queue.Fail(ctx, claim, domain.KnowledgeQueueFailureProviderInvalid); failErr != nil {
			w.logger.Warn("knowledge embedding terminal failure transition failed", "error", w.sanitize(failErr.Error()))
		}
		return
	}
	var sourceErr *sourceInvalidError
	if errors.As(err, &sourceErr) {
		if failErr := w.queue.Fail(ctx, claim, domain.KnowledgeQueueFailureSourceInvalid); failErr != nil {
			w.logger.Warn("knowledge embedding terminal failure transition failed", "error", w.sanitize(failErr.Error()))
		}
		return
	}
	if item.Attempts >= w.cfg.MaxRetries {
		if failErr := w.queue.Fail(ctx, claim, domain.KnowledgeQueueFailureAttemptsExhausted); failErr != nil {
			w.logger.Warn("knowledge embedding terminal failure transition failed", "error", w.sanitize(failErr.Error()))
		}
		return
	}
	next := w.clock.Now().UTC().Add(embeddingRetryDelay(item.Attempts))
	if retryErr := w.queue.Retry(ctx, claim, next); retryErr != nil {
		w.logger.Warn("knowledge embedding retry transition failed", "error", w.sanitize(retryErr.Error()))
	}
}

// complete completes one claim; a CAS conflict means a concurrent mutation
// bumped the generation and the fresh claim will replace the vector row.
func (w *EmbeddingWorker) complete(ctx context.Context, item domain.KnowledgeQueueItem) {
	claim := domain.KnowledgeQueueClaim{Kind: item.Kind, ID: item.ID, Generation: item.Generation, LeaseUntil: item.LeaseUntil}
	if err := w.queue.Complete(ctx, claim); err != nil {
		if errors.Is(err, port.ErrKnowledgeCASConflict) {
			w.logger.Debug("knowledge embedding completion conflicted; fresh generation will re-embed", "kind", string(item.Kind))
			return
		}
		w.logger.Warn("knowledge embedding completion failed; lease recovery will re-embed", "error", w.sanitize(err.Error()))
	}
}

// Reconcile pages truth and current-fingerprint vector identities per kind
// and enqueues every current truth identity plus orphan vector identities.
// Every truth identity is re-enqueued unconditionally so redactor rotations
// and provider configuration changes rebuild vectors without advancing
// truth revisions. It returns without waiting for queue drains and never
// reads content or vector bytes.
func (w *EmbeddingWorker) Reconcile(ctx context.Context) error {
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

func (w *EmbeddingWorker) reconcileKind(ctx context.Context, kind domain.KnowledgeRetrievalItemKind) error {
	pageSize := w.cfg.BatchSize
	truthSet := make(map[string]bool)
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
			truthSet[identity.ID] = true
		}
		if len(page) < pageSize {
			break
		}
		afterID = page[len(page)-1].ID
	}
	enqueue := func(id string) error {
		if _, err := w.queue.Enqueue(ctx, kind, id); err != nil {
			return fmt.Errorf("enqueue reconciliation identity: %w", err)
		}
		return nil
	}
	for id := range truthSet {
		if err := enqueue(id); err != nil {
			return err
		}
	}
	for afterID := ""; ; {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		page, err := w.index.ListVector(ctx, kind, w.fingerprint, afterID, pageSize)
		if err != nil {
			return fmt.Errorf("list vector index identities: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			if !truthSet[row.ID] {
				if err := enqueue(row.ID); err != nil {
					return err
				}
			}
		}
		if len(page) < pageSize {
			break
		}
		afterID = page[len(page)-1].ID
	}
	return nil
}

// Rebuild removes every reconstructible vector row and re-enqueues every
// truth identity. It never modifies claims, preferences, documents,
// revisions, tombstones, or the projection outbox.
func (w *EmbeddingWorker) Rebuild(ctx context.Context) error {
	if err := w.index.ClearVector(ctx); err != nil {
		return fmt.Errorf("clear vector index: %w", err)
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

// embeddingRetryDelay is finite and bounded by attempts, mirroring the
// lexical worker's delay shape.
func embeddingRetryDelay(attempts int) time.Duration {
	shifts := min(max(attempts-1, 0), embeddingRetryMaxDelayShifts)
	return embeddingRetryBaseDelay * time.Duration(1<<shifts)
}
