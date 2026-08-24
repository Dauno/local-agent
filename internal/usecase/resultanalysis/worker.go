package resultanalysis

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

// WorkerConfig bounds the durable worker's tick cadence and claim lease.
// Every other semantic component a worker needs (prompt version,
// segmentation version, and the limits snapshot) comes from each
// analysis' own durable record, never from live configuration: a resumed
// analysis is always driven under the exact bounds and versions it was
// created under, per TRD 07's Analysis Identity section, so a
// configuration change never mutates a running or completed analysis.
type WorkerConfig struct {
	Interval time.Duration
	Lease    time.Duration
}

// WorkerDependencies carries every durable store and the pure analyzer the
// worker drives. Every field is required; NewWorker fails closed if any is
// nil, the same discipline every other TRD 07 durable dependency in this
// codebase follows.
type WorkerDependencies struct {
	Analyses   port.AnalysisStore
	Active     port.AnalysisActiveLister
	Running    port.AnalysisRunningMarker
	Steps      port.AnalysisStepStore
	Segments   port.AnalysisSegmentStore
	Evidence   port.AnalysisEvidenceStore
	Payloads   port.AnalysisStepPayloadStore
	Completion port.AnalysisCompletionStore
	Source     port.TrustedResultStore
	Analyzer   port.ResultAnalyzer
	Clock      port.Clock
	Logger     port.Logger
	Sanitize   func(string) string
	Scheduler  *workpoll.Scheduler
}

// Worker drains every non-terminal analysis' step queue: it claims steps
// with AnalysisStepStore.ClaimNext, dispatches them to LeafRunner or
// ReductionRunner by kind, and completes an analysis once its root
// reduction step finishes. It starts with an immediate reconciliation
// tick, then runs on Interval until its context is cancelled; shutdown is
// awaitable through WaitStopped. An in-flight claim is left in place on
// cancellation for lease-based recovery, never force-failed, matching
// TRD 07's "cancellation stops unstarted work" rule.
type Worker struct {
	cfg       WorkerConfig
	deps      WorkerDependencies
	stopped   chan struct{}
	stopOnce  sync.Once
	scheduler *workpoll.Scheduler
}

const (
	workerPanicBackoff    = time.Second
	workerMaxPanicBackoff = 30 * time.Second
	// workerListPageSize bounds one ListActive page. It is a worker-owned
	// constant, not configuration: pagination width has no TRD 07 policy
	// meaning, unlike every bound in domain.AnalysisLimits.
	workerListPageSize = 100
)

// NewWorker validates its configuration and dependencies once at
// construction, so a misconfigured worker fails at startup rather than on
// its first tick.
func NewWorker(cfg WorkerConfig, deps WorkerDependencies) (*Worker, error) {
	if cfg.Interval <= 0 {
		return nil, errors.New("result analysis worker interval must be positive")
	}
	if cfg.Lease <= 0 {
		return nil, errors.New("result analysis worker lease must be positive")
	}
	if deps.Analyses == nil || deps.Active == nil || deps.Running == nil || deps.Steps == nil || deps.Segments == nil ||
		deps.Evidence == nil || deps.Payloads == nil || deps.Completion == nil || deps.Source == nil || deps.Analyzer == nil {
		return nil, errors.New("result analysis worker dependencies are incomplete")
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
	return &Worker{cfg: cfg, deps: deps, stopped: make(chan struct{}), scheduler: deps.Scheduler}, nil
}

// WaitStopped blocks until Run returns. A nil worker returns immediately.
func (w *Worker) WaitStopped(ctx context.Context) error {
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

// Run supervises the worker: an immediate reconciliation tick, then
// periodic ticks until the context is cancelled. Panics are recovered with
// bounded exponential backoff; Run returns only when the context is done.
func (w *Worker) Run(ctx context.Context) {
	defer w.stopOnce.Do(func() { close(w.stopped) })
	backoff := workerPanicBackoff
	for {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					w.deps.Logger.Error("result analysis worker panicked, will retry",
						"panic", w.deps.Sanitize(fmt.Sprintf("%v", recovered)),
						"stack", w.deps.Sanitize(string(debug.Stack())))
				}
			}()
			w.runWorker(ctx)
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, workerMaxPanicBackoff)
	}
}

func (w *Worker) runWorker(ctx context.Context) {
	w.scheduler.Run(ctx, nil, func() bool { return w.tick(ctx) })
}

// tick processes every non-terminal analysis once, in stable analysis-id
// pages. A failure listing active analyses stops the tick; a failure on
// one analysis is logged and never blocks another analysis in the same
// tick.
func (w *Worker) tick(ctx context.Context) bool {
	after := ""
	found := false
	for {
		if ctx.Err() != nil {
			return found
		}
		page, err := w.deps.Active.ListActive(ctx, after, workerListPageSize)
		if err != nil {
			if ctx.Err() == nil {
				w.deps.Logger.Error("result analysis list active failed", "error", w.deps.Sanitize(err.Error()))
			}
			return found
		}
		found = found || len(page) > 0
		for _, record := range page {
			if ctx.Err() != nil {
				return found
			}
			w.processAnalysis(ctx, record)
		}
		if len(page) < workerListPageSize {
			return found
		}
		after = page[len(page)-1].AnalysisID
	}
}

// processAnalysis drives one non-terminal analysis for one tick: wall-time
// enforcement, preparation (building the manifest and steps once), draining
// claimable steps, then completing the analysis if its root reduction step
// just finished.
func (w *Worker) processAnalysis(ctx context.Context, record port.AnalysisRecord) {
	now := w.deps.Clock.Now().UTC()
	limits := record.Limits
	if limits.Validate() != nil {
		// The persisted limits snapshot could not be reconstructed (see
		// scanAnalysisRecordRow): this analysis can never be safely driven
		// under a borrowed or unbounded configuration, so it fails closed
		// rather than running with a zero-value limits struct.
		w.failAnalysis(ctx, record, domain.AnalysisFailureIdentityStale, now)
		return
	}
	if now.Sub(record.CreatedAt) > time.Duration(limits.WallTimeSeconds)*time.Second {
		w.failAnalysis(ctx, record, domain.AnalysisFailureWallTimeExceeded, now)
		return
	}
	if err := w.ensurePrepared(ctx, record, limits, now); err != nil {
		if ctx.Err() == nil {
			w.deps.Logger.Warn("result analysis prepare failed", "error", w.deps.Sanitize(err.Error()))
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	w.drainAnalysis(ctx, record, limits)
	w.maybeComplete(ctx, record)
}

func (w *Worker) failAnalysis(ctx context.Context, record port.AnalysisRecord, code domain.AnalysisFailureCode, now time.Time) {
	if _, err := w.deps.Analyses.Fail(ctx, record.AnalysisID, record.Scope, code, now); err != nil && ctx.Err() == nil {
		w.deps.Logger.Warn("result analysis fail transition failed", "error", w.deps.Sanitize(err.Error()))
	}
}

// ensurePrepared builds the durable segment manifest and step tree for an
// analysis that has neither yet (its first tick, in this process or an
// earlier one), then marks it running. An analysis that already has a
// durable manifest is left untouched: BuildManifest re-reads the entire
// source, so it only runs once per analysis, not once per tick.
func (w *Worker) ensurePrepared(ctx context.Context, record port.AnalysisRecord, limits domain.AnalysisLimits, now time.Time) error {
	segments, err := w.deps.Segments.ListSegments(ctx, record.AnalysisID)
	if err != nil {
		return err
	}
	if len(segments) > 0 {
		if _, err := w.deps.Running.MarkRunning(ctx, record.AnalysisID, record.Scope, now); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			w.deps.Logger.Debug("result analysis mark running failed", "error", w.deps.Sanitize(err.Error()))
		}
		return nil
	}

	manifest, identity, err := BuildManifest(ctx, w.deps.Source, record.Identity.SourceResultID, record.Scope, record.Identity.SegmentationVersion, limits)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		w.failAnalysis(ctx, record, inferFailureCode(err, domain.AnalysisFailureSegmentInvalid), now)
		return nil
	}
	if identity.SHA256 != record.Identity.SourceSHA256 {
		w.failAnalysis(ctx, record, domain.AnalysisFailureIdentityStale, now)
		return nil
	}

	prepared, err := PrepareSteps(record.AnalysisID, manifest, limits, now)
	if err != nil {
		w.failAnalysis(ctx, record, inferFailureCode(err, domain.AnalysisFailureSegmentInvalid), now)
		return nil
	}
	for _, step := range prepared {
		if _, err := w.deps.Steps.Prepare(ctx, step); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return err
		}
	}
	if err := w.deps.Segments.WriteManifest(ctx, record.AnalysisID, manifest, now); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return err
	}
	if _, err := w.deps.Running.MarkRunning(ctx, record.AnalysisID, record.Scope, now); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		w.deps.Logger.Debug("result analysis mark running failed", "error", w.deps.Sanitize(err.Error()))
	}
	return nil
}

// drainAnalysis claims and runs every currently eligible step for one
// analysis, up to limits.MaxConcurrentLeaves concurrently, stopping when
// ClaimNext finds nothing left to claim: either every step is settled
// (completed or failed), or the remaining claimable steps are blocked
// waiting on a lease to expire or on children to complete.
func (w *Worker) drainAnalysis(ctx context.Context, record port.AnalysisRecord, limits domain.AnalysisLimits) {
	segments, err := w.deps.Segments.ListSegments(ctx, record.AnalysisID)
	if err != nil {
		if ctx.Err() == nil {
			w.deps.Logger.Warn("result analysis list segments failed", "error", w.deps.Sanitize(err.Error()))
		}
		return
	}
	segmentByOrdinal := make(map[int]domain.AnalysisSegment, len(segments))
	for _, segment := range segments {
		segmentByOrdinal[segment.Ordinal] = segment
	}

	allSteps, err := paginatedAnalysisSteps(ctx, w.deps.Steps, record.AnalysisID)
	if err != nil {
		if ctx.Err() == nil {
			w.deps.Logger.Warn("result analysis list steps failed", "error", w.deps.Sanitize(err.Error()))
		}
		return
	}
	stepByID := make(map[string]port.AnalysisStep, len(allSteps))
	for _, step := range allSteps {
		stepByID[step.StepID] = step
	}
	rootStepID := rootReductionStepID(allSteps)

	identity, _, err := w.deps.Source.Resolve(ctx, record.Identity.SourceResultID, record.Scope)
	if err != nil {
		if ctx.Err() == nil {
			w.deps.Logger.Warn("result analysis resolve source failed", "error", w.deps.Sanitize(err.Error()))
		}
		return
	}
	rootManifest := domain.AnalysisSegmentManifest{SourceSHA256: identity.SHA256, SourceBytes: identity.Bytes, Segments: segments}

	width := max(limits.MaxConcurrentLeaves, 1)
	sem := make(chan struct{}, width)
	var wg sync.WaitGroup
	for ctx.Err() == nil {
		claimed, ok, err := w.deps.Steps.ClaimNext(ctx, record.AnalysisID, w.deps.Clock.Now().UTC(), w.cfg.Lease)
		if err != nil {
			if ctx.Err() == nil {
				w.deps.Logger.Warn("result analysis claim failed", "error", w.deps.Sanitize(err.Error()))
			}
			break
		}
		if !ok {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(step port.AnalysisStep) {
			defer wg.Done()
			defer func() { <-sem }()
			w.processStep(ctx, record, limits, segmentByOrdinal, stepByID, rootManifest, rootStepID, step)
		}(claimed)
	}
	wg.Wait()
}

// processStep runs exactly one already-claimed step through LeafRunner or
// ReductionRunner. Every branch already ends in a durable Complete, Retry,
// or Fail on the exact claim token (inside the runners), or intentionally
// leaves the claim in place on cancellation, matching RunLeaf and
// RunReduction's own outcome contracts.
func (w *Worker) processStep(ctx context.Context, record port.AnalysisRecord, limits domain.AnalysisLimits,
	segmentByOrdinal map[int]domain.AnalysisSegment, stepByID map[string]port.AnalysisStep,
	rootManifest domain.AnalysisSegmentManifest, rootStepID string, step port.AnalysisStep) {
	now := w.deps.Clock.Now().UTC()
	claim := domain.AnalysisStepClaim{AnalysisID: record.AnalysisID, StepID: step.StepID, Generation: step.Generation, LeaseUntil: step.LeaseUntil}

	if step.Attempt >= limits.MaxAttemptsPerStep {
		if err := w.deps.Steps.Fail(ctx, claim, domain.AnalysisFailureAttemptsExhausted, now); err != nil && ctx.Err() == nil {
			w.deps.Logger.Warn("result analysis step fail (attempts exhausted) failed", "error", w.deps.Sanitize(err.Error()))
		}
		w.failAnalysis(ctx, record, domain.AnalysisFailureAttemptsExhausted, now)
		return
	}

	switch step.Kind {
	case domain.AnalysisStepLeaf:
		w.runLeafStep(ctx, record, limits, segmentByOrdinal, claim, step, now)
	case domain.AnalysisStepReduction:
		w.runReductionStep(ctx, record, segmentByOrdinal, stepByID, rootManifest, rootStepID, claim, step, now)
	}
}

func (w *Worker) runLeafStep(ctx context.Context, record port.AnalysisRecord, limits domain.AnalysisLimits,
	segmentByOrdinal map[int]domain.AnalysisSegment, claim domain.AnalysisStepClaim, step port.AnalysisStep, now time.Time) {
	segment, ok := segmentByOrdinal[step.SegmentOrdinal]
	if !ok {
		if err := w.deps.Steps.Fail(ctx, claim, domain.AnalysisFailureSegmentInvalid, now); err != nil && ctx.Err() == nil {
			w.deps.Logger.Warn("result analysis leaf fail (missing segment) failed", "error", w.deps.Sanitize(err.Error()))
		}
		return
	}
	segmentText, err := readFullRange(ctx, w.deps.Source, record.Identity.SourceResultID, record.Scope, record.Identity.SourceSHA256, segment.OffsetBytes, segment.LengthBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if err := w.deps.Steps.Fail(ctx, claim, domain.AnalysisFailureSegmentInvalid, now); err != nil && ctx.Err() == nil {
			w.deps.Logger.Warn("result analysis leaf fail (segment read) failed", "error", w.deps.Sanitize(err.Error()))
		}
		return
	}
	input := port.AnalysisLeafInput{
		ObjectiveClass: record.Identity.ObjectiveClass, ObjectiveText: record.ObjectiveText,
		SegmentOrdinal: segment.Ordinal, SegmentText: segmentText, PromptVersion: record.Identity.PromptVersion,
	}
	runner := &LeafRunner{Steps: w.deps.Steps, Source: w.deps.Source, Analyzer: w.deps.Analyzer, Evidence: w.deps.Evidence, Payloads: w.deps.Payloads}
	if _, _, err := runner.RunLeaf(ctx, claim, segment, input, record.Identity.SourceResultID, record.Scope, limits, now); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			w.deps.Logger.Warn("result analysis leaf run failed", "error", w.deps.Sanitize(err.Error()))
		}
	}
}

func (w *Worker) runReductionStep(ctx context.Context, record port.AnalysisRecord, segmentByOrdinal map[int]domain.AnalysisSegment,
	stepByID map[string]port.AnalysisStep, rootManifest domain.AnalysisSegmentManifest, rootStepID string,
	claim domain.AnalysisStepClaim, step port.AnalysisStep, now time.Time) {
	children := make([]port.AnalysisStep, 0, len(step.ChildStepIDs))
	for _, childID := range step.ChildStepIDs {
		child, ok := stepByID[childID]
		if !ok {
			if err := w.deps.Steps.Fail(ctx, claim, domain.AnalysisFailureReductionIrreducible, now); err != nil && ctx.Err() == nil {
				w.deps.Logger.Warn("result analysis reduction fail (missing child) failed", "error", w.deps.Sanitize(err.Error()))
			}
			return
		}
		children = append(children, child)
	}
	isRoot := step.StepID != "" && step.StepID == rootStepID
	coverage := domain.AnalysisCoverage{}
	if isRoot {
		coverage = rootManifest.Coverage()
	}
	runner := &ReductionRunner{Steps: w.deps.Steps, Payloads: w.deps.Payloads, Evidence: w.deps.Evidence, Analyzer: w.deps.Analyzer}
	if _, err := runner.RunReduction(ctx, claim, children, isRoot, record.Identity, record.ObjectiveText, nil, record.Identity.PromptVersion, coverage, now); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			w.deps.Logger.Warn("result analysis reduction run failed", "error", w.deps.Sanitize(err.Error()))
		}
	}
}

// maybeComplete checks whether every step of record is now settled: if any
// step failed, the analysis fails with that step's failure code; if every
// step completed, the root reduction step's terminal packet is read back
// and CompleteTerminalAnalysis commits the analysis. If any step is still
// prepared or claimed, this is a no-op: the analysis is still in progress.
func (w *Worker) maybeComplete(ctx context.Context, record port.AnalysisRecord) {
	if ctx.Err() != nil {
		return
	}
	allSteps, err := paginatedAnalysisSteps(ctx, w.deps.Steps, record.AnalysisID)
	if err != nil {
		if ctx.Err() == nil {
			w.deps.Logger.Warn("result analysis list steps (completion check) failed", "error", w.deps.Sanitize(err.Error()))
		}
		return
	}
	if len(allSteps) == 0 {
		return
	}
	now := w.deps.Clock.Now().UTC()
	for _, step := range allSteps {
		if step.State == domain.AnalysisStepFailed {
			code := step.FailureCode
			if code == "" {
				code = domain.AnalysisFailureReductionIrreducible
			}
			w.failAnalysis(ctx, record, code, now)
			return
		}
		if step.State != domain.AnalysisStepCompleted {
			return
		}
	}

	rootStepID := rootReductionStepID(allSteps)
	if rootStepID == "" {
		return
	}
	payload, err := w.deps.Payloads.ReadPayload(ctx, record.AnalysisID, rootStepID)
	if err != nil {
		if ctx.Err() == nil {
			w.deps.Logger.Warn("result analysis read root payload failed", "error", w.deps.Sanitize(err.Error()))
		}
		return
	}
	packet, err := DecodePacketPayload(payload)
	if err != nil {
		w.failAnalysis(ctx, record, domain.AnalysisFailureReductionIrreducible, now)
		return
	}
	identity, _, err := w.deps.Source.Resolve(ctx, record.Identity.SourceResultID, record.Scope)
	if err != nil {
		if ctx.Err() == nil {
			w.deps.Logger.Warn("result analysis resolve source (completion) failed", "error", w.deps.Sanitize(err.Error()))
		}
		return
	}

	input := CompletionInput{
		AnalysisID: record.AnalysisID, Scope: record.Scope, SourceResultID: record.Identity.SourceResultID,
		SourceBytes: identity.Bytes, PromptVersion: record.Identity.PromptVersion, Packet: packet,
	}
	// CompleteTerminalAnalysis's underlying AnalysisCompletionStore already
	// marks the analysis row 'completed' as part of its one atomic
	// transaction (alongside the host_reduction representation and the
	// result_references row); a second, separate call to
	// AnalysisStore.Complete here would be redundant and would always fail
	// with "already terminal", since the row is no longer in 'preparing'
	// or 'running' state by the time this line is reached.
	if err := CompleteTerminalAnalysis(ctx, w.deps.Completion, allSteps, input, now); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		w.failAnalysis(ctx, record, inferFailureCode(err, domain.AnalysisFailureReductionIrreducible), now)
		return
	}
}

// paginatedAnalysisSteps pages every step of analysisID through
// AnalysisStepStore.List.
func paginatedAnalysisSteps(ctx context.Context, steps port.AnalysisStepStore, analysisID string) ([]port.AnalysisStep, error) {
	var all []port.AnalysisStep
	after := ""
	for {
		page, err := steps.List(ctx, analysisID, after, 500)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 500 {
			return all, nil
		}
		after = page[len(page)-1].StepID
	}
}

// inferFailureCode recovers the closed domain.AnalysisFailureCode that a
// typed ErrAnalysisValidation error names in its message, falling back to
// fallback when none of the closed codes appear. BuildManifest,
// PrepareSteps, and CompleteTerminalAnalysis all format their typed errors
// as "%w: %s: ..." with the failure code as %s, but return a plain error
// value, not a typed one carrying the code as a field, so this is a
// string match rather than an errors.As extraction.
func inferFailureCode(err error, fallback domain.AnalysisFailureCode) domain.AnalysisFailureCode {
	if err == nil {
		return fallback
	}
	message := err.Error()
	for _, code := range []domain.AnalysisFailureCode{
		domain.AnalysisFailureSourceTooLarge,
		domain.AnalysisFailureCoverageIncomplete,
		domain.AnalysisFailureReductionIrreducible,
		domain.AnalysisFailureEvidenceSelectorInvalid,
		domain.AnalysisFailureLeafSchemaInvalid,
		domain.AnalysisFailureSegmentInvalid,
	} {
		if strings.Contains(message, string(code)) {
			return code
		}
	}
	return fallback
}
