package resultanalysis_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/fsartifact"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

// wakeTimer is a workpoll.Timer whose channel never fires on its own.
type wakeTimer struct{ c chan time.Time }

func (t *wakeTimer) C() <-chan time.Time { return t.c }
func (t *wakeTimer) Stop() bool          { return true }

type wakeTimers struct{ created chan *wakeTimer }

func newWakeTimers() *wakeTimers { return &wakeTimers{created: make(chan *wakeTimer, 16)} }

func (f *wakeTimers) New(time.Duration) workpoll.Timer {
	timer := &wakeTimer{c: make(chan time.Time, 1)}
	f.created <- timer
	return timer
}

func newWakeScheduler(t *testing.T) (*workpoll.Scheduler, *wakeTimers) {
	t.Helper()
	timers := newWakeTimers()
	scheduler, err := workpoll.New(time.Hour, workpoll.Options{NewTimer: timers.New})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler, timers
}

// waitPoll blocks on the fake timer's creation channel only: it carries no
// real duration of its own, so a hang here surfaces as the go test binary's
// own -timeout, never as a flaky arbitrary wait inside the test.
func waitPoll(t *testing.T, timers *wakeTimers) {
	t.Helper()
	<-timers.created
}

type noopWakeLogger struct{}

func (noopWakeLogger) Debug(string, ...any) {}
func (noopWakeLogger) Info(string, ...any)  {}
func (noopWakeLogger) Warn(string, ...any)  {}
func (noopWakeLogger) Error(string, ...any) {}

// wakeAnalyzer is a hermetic port.ResultAnalyzer that produces one real leaf
// finding and one real reduction, so a tick can actually drain an analysis
// to completion without a live model.
type wakeAnalyzer struct{}

func (wakeAnalyzer) AnalyzeLeaf(_ context.Context, input port.AnalysisLeafInput) (domain.AnalysisLeaf, error) {
	return domain.AnalysisLeaf{
		ObjectiveClass:  input.ObjectiveClass,
		ObjectiveDigest: domain.AnalysisObjectiveDigest(input.ObjectiveClass, input.ObjectiveText),
		SegmentOrdinal:  input.SegmentOrdinal,
		Findings:        []domain.AnalysisStatement{{Text: fmt.Sprintf("finding for segment %d", input.SegmentOrdinal)}},
	}, nil
}

func (wakeAnalyzer) Reduce(_ context.Context, input port.AnalysisReductionInput) (domain.AnalysisPacket, error) {
	packet := domain.AnalysisPacket{
		ObjectiveClass:  input.ObjectiveClass,
		ObjectiveDigest: domain.AnalysisObjectiveDigest(input.ObjectiveClass, input.ObjectiveText),
		Findings:        []domain.AnalysisStatement{{Text: "combined finding"}},
	}
	for _, child := range input.Children {
		packet.Lineage = append(packet.Lineage, child.StepID)
	}
	return packet, nil
}

// wakeAnalysisStores are the real SQLite v40 adapters composeResultAnalysis
// itself uses, plus one already-materialized source. Building them once and
// handing them to separate producer and consumer Service/Worker pairs lets
// a restart fixture model a durable store that outlives the process that
// wrote to it, distinct from any scheduler.
type wakeAnalysisStores struct {
	analyses   *adaptersqlite.AnalysisStore
	steps      *adaptersqlite.AnalysisStepStore
	segments   *adaptersqlite.AnalysisSegmentStore
	evidence   *adaptersqlite.AnalysisEvidenceStore
	completion *adaptersqlite.AnalysisCompletionStore
	source     *adaptersqlite.ResultStore
	resultID   string
}

func newWakeAnalysisStores(t *testing.T) wakeAnalysisStores {
	t.Helper()
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "wake-analysis.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	payloads, err := fsartifact.NewTypedStore(filepath.Join(t.TempDir(), "wake-analysis-payloads"), 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	source, err := adaptersqlite.NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := source.Materialize(t.Context(), port.ResultMaterialization{
		Producer: domain.ResultProducer{Kind: domain.ResultProducerExternalAgentJob, ID: "wake-op", Revision: 0},
		Payload:  "short source content", Scope: testScope(), Retention: domain.ResultRetentionWorkstream,
		MediaType: "text/plain; charset=utf-8",
	})
	if err != nil {
		t.Fatal(err)
	}
	return wakeAnalysisStores{
		analyses: adaptersqlite.NewAnalysisStore(store), steps: adaptersqlite.NewAnalysisStepStore(store),
		segments: adaptersqlite.NewAnalysisSegmentStore(store), evidence: adaptersqlite.NewAnalysisEvidenceStore(store),
		completion: adaptersqlite.NewAnalysisCompletionStore(store), source: source, resultID: handle.ResultID,
	}
}

// newWakeAnalysisService builds only the tool-facing Service over stores,
// sharing scheduler with any worker built over the same stores exactly like
// composeResultAnalysis wires ServiceDependencies.Wake.
func newWakeAnalysisService(t *testing.T, stores wakeAnalysisStores, scheduler *workpoll.Scheduler) *resultanalysis.Service {
	t.Helper()
	service, err := resultanalysis.NewService(resultanalysis.ServiceConfig{
		SegmentationVersion: resultanalysis.SegmenterTextV1, PromptVersion: "prompt-v1",
		ModelFingerprint: domain.AnalysisModelFingerprint("provider", "model"), Limits: testLimits(),
	}, resultanalysis.ServiceDependencies{
		Source: stores.source, Analyses: stores.analyses, Steps: stores.steps, Evidence: stores.evidence, Payloads: stores.steps,
		Analyzer: wakeAnalyzer{}, Wake: scheduler.Wake,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// newWakeAnalysisWorker builds only the durable Worker over stores, sharing
// scheduler with any service built over the same stores exactly like
// composeResultAnalysis wires WorkerDependencies.Scheduler.
func newWakeAnalysisWorker(t *testing.T, stores wakeAnalysisStores, scheduler *workpoll.Scheduler) *resultanalysis.Worker {
	t.Helper()
	worker, err := resultanalysis.NewWorker(resultanalysis.WorkerConfig{Interval: time.Hour, Lease: time.Minute}, resultanalysis.WorkerDependencies{
		Analyses: stores.analyses, Active: stores.analyses, Running: stores.analyses, Steps: stores.steps, Segments: stores.segments,
		Evidence: stores.evidence, Payloads: stores.steps, Completion: stores.completion, Source: stores.source, Analyzer: wakeAnalyzer{},
		Clock: port.SystemClock{}, Logger: noopWakeLogger{}, Scheduler: scheduler,
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

// newWakeAnalysisPipeline builds the service and worker over the same real
// SQLite v40 adapters composeResultAnalysis itself uses, sharing one
// scheduler between them exactly like composeResultAnalysis wires
// ServiceDependencies.Wake and WorkerDependencies.Scheduler.
func newWakeAnalysisPipeline(t *testing.T, scheduler *workpoll.Scheduler) (*resultanalysis.Service, *resultanalysis.Worker, string) {
	t.Helper()
	stores := newWakeAnalysisStores(t)
	return newWakeAnalysisService(t, stores, scheduler), newWakeAnalysisWorker(t, stores, scheduler), stores.resultID
}

// TestAnalysisSchedulerConsumesWithoutRecoveryTimer pins FIND-122 for the
// result-analysis class: the shared scheduler used by both Service.Wake and
// Worker.Scheduler must actually drive the worker to drain a freshly
// requested analysis to completion, never through the recovery timer.
func TestAnalysisSchedulerConsumesWithoutRecoveryTimer(t *testing.T) {
	t.Run("restart empty then produced", func(t *testing.T) {
		scheduler, timers := newWakeScheduler(t)
		service, worker, source := newWakeAnalysisPipeline(t, scheduler)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go worker.Run(ctx)
		waitPoll(t, timers) // initial poll: no analysis is active yet.

		result, err := service.RequestAnalysis(t.Context(), source, testScope(), "ws-1", "What changed?", time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		// The tick triggered by the producer's wake, not the timer, drains
		// at least the one durable leaf step it can claim synchronously.
		// Under load the same tick's drain loop can also claim the root
		// reduction once that leaf's completion has committed, so this
		// asserts the durable property that actually matters (the leaf was
		// consumed and the analysis is no longer merely preparing), not a
		// specific intermediate step count.
		waitPoll(t, timers)
		status, err := service.Status(t.Context(), result.AnalysisID, testScope())
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		assertLeafConsumedByWakeDrivenPoll(t, status)
	})

	t.Run("pending before restart", func(t *testing.T) {
		stores := newWakeAnalysisStores(t)

		// The producer runs against scheduler A, which no worker ever
		// drives: RequestAnalysis's internal wake is inert, exactly like a
		// process that writes durable state and then exits before it ever
		// polls.
		producerScheduler, _ := newWakeScheduler(t)
		producer := newWakeAnalysisService(t, stores, producerScheduler)
		result, err := producer.RequestAnalysis(t.Context(), stores.resultID, testScope(), "ws-1", "What changed?", time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}

		// Restart: a brand-new scheduler and a brand-new worker consume the
		// same durable stores. Nothing ever calls this scheduler's Wake(),
		// so only its own initial poll can discover the pre-existing
		// durable analysis; no wake retained from the producer process can
		// substitute for that property.
		restartScheduler, restartTimers := newWakeScheduler(t)
		worker := newWakeAnalysisWorker(t, stores, restartScheduler)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go worker.Run(ctx)
		waitPoll(t, restartTimers) // initial poll must drain the pre-existing durable analysis.
		status, err := producer.Status(t.Context(), result.AnalysisID, testScope())
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		assertLeafConsumedByWakeDrivenPoll(t, status)
	})
}

// assertLeafConsumedByWakeDrivenPoll checks the durable property that
// actually matters after exactly one observed poll cycle: the leaf step was
// consumed and the analysis is no longer merely preparing. It accepts
// AnalysisRunning (the common case, at least one leaf completed) and
// AnalysisCompleted (the same tick's drain loop also claimed and completed
// the root reduction, which is valid, not a race to paper over) without
// requiring a specific intermediate step count. waitPoll's channel receive
// already happens strictly after the tick's synchronous work, including
// this status's durable writes, committed — so this call can never observe
// a state older than the poll it is gated on.
func assertLeafConsumedByWakeDrivenPoll(t *testing.T, status resultanalysis.AnalysisStatusResult) {
	t.Helper()
	if status.State == domain.AnalysisPreparing {
		t.Fatalf("analysis after wake-driven poll is still preparing = %#v", status)
	}
	switch status.State {
	case domain.AnalysisRunning:
		if status.LeafCompleted < 1 {
			t.Fatalf("analysis after wake-driven poll = %#v, want at least one leaf completed", status)
		}
	case domain.AnalysisCompleted:
		// The drain loop consumed the leaf and the root reduction in the
		// same tick: still a real, durable consequence of the one observed
		// poll, not a state the test manufactured.
	default:
		t.Fatalf("analysis after wake-driven poll = %#v, want running or completed", status)
	}
}
