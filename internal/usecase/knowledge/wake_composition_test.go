package knowledge

import (
	"context"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/testutil"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

// wakePollTimer is a workpoll.Timer whose channel never fires on its own.
type wakePollTimer struct{ c chan time.Time }

func (t *wakePollTimer) C() <-chan time.Time { return t.c }
func (t *wakePollTimer) Stop() bool          { return true }

type wakePollTimers struct{ created chan *wakePollTimer }

func newWakePollTimers() *wakePollTimers {
	return &wakePollTimers{created: make(chan *wakePollTimer, 16)}
}

func (f *wakePollTimers) New(time.Duration) workpoll.Timer {
	timer := &wakePollTimer{c: make(chan time.Time, 1)}
	f.created <- timer
	return timer
}

func newWakePollScheduler(t *testing.T) (*workpoll.Scheduler, *wakePollTimers) {
	t.Helper()
	timers := newWakePollTimers()
	scheduler, err := workpoll.New(time.Hour, workpoll.Options{NewTimer: timers.New})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler, timers
}

// waitOnePoll blocks on the fake timer's creation channel only: it carries
// no real duration of its own, so a hang here surfaces as the go test
// binary's own -timeout, never as a flaky arbitrary wait inside the test.
func waitOnePoll(t *testing.T, timers *wakePollTimers) {
	t.Helper()
	<-timers.created
}

// wakingQueue mirrors internal/app's wakingKnowledgeQueue: it wakes the
// given scheduler exactly once per committed Enqueue.
type wakingQueue struct {
	port.KnowledgeQueueStore
	wake func()
}

func (q wakingQueue) Enqueue(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, itemID string) (domain.KnowledgeQueueItem, error) {
	item, err := q.KnowledgeQueueStore.Enqueue(ctx, kind, itemID)
	if err == nil && q.wake != nil {
		q.wake()
	}
	return item, err
}

// TestProjectionSchedulerConsumesWithoutRecoveryTimer pins FIND-122 for the
// knowledge projection outbox class.
func TestProjectionSchedulerConsumesWithoutRecoveryTimer(t *testing.T) {
	newWorker := func(t *testing.T, store *workerFakeStore, scheduler *workpoll.Scheduler) *ProjectionWorker {
		t.Helper()
		clock := &workerTestClock{now: time.Unix(1_700_000_000, 0).UTC()}
		store.now = func() time.Time { return clock.now }
		worker, err := NewProjectionWorker(ProjectionWorkerConfig{
			Interval: time.Hour, MaxRetries: 3, RetentionDays: 90, OutputDir: t.TempDir(),
		}, ProjectionWorkerDependencies{
			Store: store, Reader: workerTestReader{}, Projector: &workerFakeProjector{}, Clock: clock, Logger: workerTestLogger{},
		})
		if err != nil {
			t.Fatal(err)
		}
		worker.scheduler = scheduler
		return worker
	}

	t.Run("restart empty then produced", func(t *testing.T) {
		store := newWorkerFakeStore()
		scheduler, timers := newWakePollScheduler(t)
		worker := newWorker(t, store, scheduler)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go worker.Run(ctx)
		waitOnePoll(t, timers) // initial poll: no projection trigger is pending.

		store.enqueue()
		scheduler.Wake() // the durable projection outbox producer calls this on commit.
		waitOnePoll(t, timers)
		if snap := store.snapshot(); snap.completed != 1 {
			t.Fatalf("projection batch after wake-driven poll = %+v", snap)
		}
	})

	t.Run("pending before restart", func(t *testing.T) {
		store := newWorkerFakeStore()
		store.enqueue()
		scheduler, timers := newWakePollScheduler(t)
		worker := newWorker(t, store, scheduler)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go worker.Run(ctx)
		waitOnePoll(t, timers) // initial poll must consume the pre-existing durable trigger.
		if snap := store.snapshot(); snap.completed != 1 {
			t.Fatalf("pre-existing projection batch after initial poll = %+v", snap)
		}
	})
}

// TestLexicalSchedulerConsumesWithoutRecoveryTimer pins FIND-122 for the
// knowledge lexical queue class.
func TestLexicalSchedulerConsumesWithoutRecoveryTimer(t *testing.T) {
	newDeps := func() (*workerFakeSource, *workerFakeIndex, *workerFakeLister, *workerFakeResolver) {
		source := newWorkerFakeSource()
		index := newWorkerFakeIndex(nil)
		lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
		resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
		return source, index, lister, resolver
	}

	t.Run("restart empty then produced", func(t *testing.T) {
		queue := newWorkerFakeQueue(nil)
		source, index, lister, resolver := newDeps()
		source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_wake"] = workerTestProjectClaim("kclaim_wake", "subject", "value", domain.KnowledgeClaimAsserted, 1)
		scheduler, timers := newWakePollScheduler(t)
		waking := wakingQueue{KnowledgeQueueStore: queue, wake: scheduler.Wake}
		worker, err := NewLexicalWorker(workerTestConfig(), LexicalWorkerDependencies{
			Queue: waking, Source: source, Index: index, Resolver: resolver, Lister: lister,
			Clock: &workerTestClock{now: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}, Logger: workerTestLogger{}, Scheduler: scheduler,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go worker.Run(ctx)
		waitOnePoll(t, timers) // initial poll: the lexical queue is empty.

		if _, err := waking.Enqueue(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_wake"); err != nil {
			t.Fatal(err)
		}
		waitOnePoll(t, timers) // poll triggered by the producer's wake, not the timer.
		rows, _ := index.ListLexical(t.Context(), domain.KnowledgeRetrievalClaim, "", 10)
		if len(rows) != 1 {
			t.Fatalf("lexical index rows after wake-driven poll = %+v", rows)
		}
	})

	t.Run("pending before restart", func(t *testing.T) {
		now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
		queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
			{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_wake2", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
		})
		source, index, lister, resolver := newDeps()
		source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_wake2"] = workerTestProjectClaim("kclaim_wake2", "subject", "value", domain.KnowledgeClaimAsserted, 1)
		scheduler, timers := newWakePollScheduler(t)
		worker, err := NewLexicalWorker(workerTestConfig(), LexicalWorkerDependencies{
			Queue: queue, Source: source, Index: index, Resolver: resolver, Lister: lister,
			Clock: &workerTestClock{now: now}, Logger: workerTestLogger{}, Scheduler: scheduler,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go worker.Run(ctx)
		waitOnePoll(t, timers) // initial poll must consume the pre-existing durable item.
		rows, _ := index.ListLexical(t.Context(), domain.KnowledgeRetrievalClaim, "", 10)
		if len(rows) != 1 {
			t.Fatalf("pre-existing lexical index rows after initial poll = %+v", rows)
		}
	})
}

// TestEmbeddingSchedulerConsumesWithoutRecoveryTimer pins FIND-122 for the
// knowledge embedding queue class.
func TestEmbeddingSchedulerConsumesWithoutRecoveryTimer(t *testing.T) {
	newDeps := func() (*workerFakeSource, *workerFakeVectorIndex, *workerFakeLister, *workerFakeResolver) {
		source := newWorkerFakeSource()
		index := newWorkerFakeVectorIndex(nil)
		lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
		resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
		return source, index, lister, resolver
	}

	t.Run("restart empty then produced", func(t *testing.T) {
		queue := newWorkerFakeQueue(nil)
		source, index, lister, resolver := newDeps()
		source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_ewake"] = workerTestProjectClaim("kclaim_ewake", "subject", "value", domain.KnowledgeClaimAsserted, 1)
		scheduler, timers := newWakePollScheduler(t)
		waking := wakingQueue{KnowledgeQueueStore: queue, wake: scheduler.Wake}
		worker, err := NewEmbeddingWorker(embeddingWorkerTestConfig(), EmbeddingWorkerDependencies{
			Queue: waking, Source: source, Index: index, Provider: testutil.NewFakeEmbeddingProvider(3),
			Resolver: resolver, Lister: lister, Clock: &workerTestClock{now: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)},
			Logger: workerTestLogger{}, Scheduler: scheduler,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go worker.Run(ctx)
		waitOnePoll(t, timers) // initial poll: the embedding queue is empty.

		if _, err := waking.Enqueue(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_ewake"); err != nil {
			t.Fatal(err)
		}
		waitOnePoll(t, timers) // poll triggered by the producer's wake, not the timer.
		rows, _ := index.ListVector(t.Context(), domain.KnowledgeRetrievalClaim, embeddingWorkerFingerprint(), "", 10)
		if len(rows) != 1 {
			t.Fatalf("vector index rows after wake-driven poll = %+v", rows)
		}
	})

	t.Run("pending before restart", func(t *testing.T) {
		now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
		queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
			{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_ewake2", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
		})
		source, index, lister, resolver := newDeps()
		source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_ewake2"] = workerTestProjectClaim("kclaim_ewake2", "subject", "value", domain.KnowledgeClaimAsserted, 1)
		scheduler, timers := newWakePollScheduler(t)
		worker, err := NewEmbeddingWorker(embeddingWorkerTestConfig(), EmbeddingWorkerDependencies{
			Queue: queue, Source: source, Index: index, Provider: testutil.NewFakeEmbeddingProvider(3),
			Resolver: resolver, Lister: lister, Clock: &workerTestClock{now: now}, Logger: workerTestLogger{}, Scheduler: scheduler,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go worker.Run(ctx)
		waitOnePoll(t, timers) // initial poll must consume the pre-existing durable item.
		rows, _ := index.ListVector(t.Context(), domain.KnowledgeRetrievalClaim, embeddingWorkerFingerprint(), "", 10)
		if len(rows) != 1 {
			t.Fatalf("pre-existing vector index rows after initial poll = %+v", rows)
		}
	})
}

// TestKnowledgeWakesGroupFiresEachDistinctConsumerScheduler is the
// composition-group gate for knowledgeWakes / retrievalSchedules: one
// committed human-command mutation must wake the projection, lexical, and
// embedding schedulers, and each of their independent consumers must
// actually advance, not just receive a counted callback.
func TestKnowledgeWakesGroupFiresEachDistinctConsumerScheduler(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	projectionStore := newWorkerFakeStore()
	projectionStore.now = func() time.Time { return now }
	projectionStore.enqueue()
	projectionScheduler, projectionTimers := newWakePollScheduler(t)
	projectionWorker, err := NewProjectionWorker(ProjectionWorkerConfig{Interval: time.Hour, MaxRetries: 3, RetentionDays: 90, OutputDir: t.TempDir()}, ProjectionWorkerDependencies{
		Store: projectionStore, Reader: workerTestReader{}, Projector: &workerFakeProjector{}, Clock: &workerTestClock{now: now}, Logger: workerTestLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	projectionWorker.scheduler = projectionScheduler

	lexicalQueue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_group", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	lexicalSource := newWorkerFakeSource()
	lexicalSource.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_group"] = workerTestProjectClaim("kclaim_group", "subject", "value", domain.KnowledgeClaimAsserted, 1)
	lexicalIndex := newWorkerFakeIndex(nil)
	lexicalScheduler, lexicalTimers := newWakePollScheduler(t)
	lexicalWorker, err := NewLexicalWorker(workerTestConfig(), LexicalWorkerDependencies{
		Queue: lexicalQueue, Source: lexicalSource, Index: lexicalIndex,
		Resolver: &workerFakeResolver{content: map[string]string{}, err: map[string]error{}},
		Lister:   &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}},
		Clock:    &workerTestClock{now: now}, Logger: workerTestLogger{}, Scheduler: lexicalScheduler,
	})
	if err != nil {
		t.Fatal(err)
	}

	embeddingQueue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_group_e", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	embeddingSource := newWorkerFakeSource()
	embeddingSource.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_group_e"] = workerTestProjectClaim("kclaim_group_e", "subject", "value", domain.KnowledgeClaimAsserted, 1)
	embeddingIndex := newWorkerFakeVectorIndex(nil)
	embeddingScheduler, embeddingTimers := newWakePollScheduler(t)
	embeddingWorker, err := NewEmbeddingWorker(embeddingWorkerTestConfig(), EmbeddingWorkerDependencies{
		Queue: embeddingQueue, Source: embeddingSource, Index: embeddingIndex, Provider: testutil.NewFakeEmbeddingProvider(3),
		Resolver: &workerFakeResolver{content: map[string]string{}, err: map[string]error{}},
		Lister:   &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}},
		Clock:    &workerTestClock{now: now}, Logger: workerTestLogger{}, Scheduler: embeddingScheduler,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go projectionWorker.Run(ctx)
	go lexicalWorker.Run(ctx)
	go embeddingWorker.Run(ctx)
	waitOnePoll(t, projectionTimers) // initial polls consume every pre-existing durable item.
	waitOnePoll(t, lexicalTimers)
	waitOnePoll(t, embeddingTimers)
	if snap := projectionStore.snapshot(); snap.completed != 1 {
		t.Fatalf("projection batch after initial poll = %+v", snap)
	}
	if rows, _ := lexicalIndex.ListLexical(t.Context(), domain.KnowledgeRetrievalClaim, "", 10); len(rows) != 1 {
		t.Fatalf("lexical rows after initial poll = %+v", rows)
	}
	if rows, _ := embeddingIndex.ListVector(t.Context(), domain.KnowledgeRetrievalClaim, embeddingWorkerFingerprint(), "", 10); len(rows) != 1 {
		t.Fatalf("embedding rows after initial poll = %+v", rows)
	}

	knowledgeStore := newFakeKnowledgeStore()
	service, err := New(Config{Enabled: true}, Dependencies{
		Store: knowledgeStore, Coordinator: newCountingCoordinator(),
		Wakes: []func(){projectionScheduler.Wake, lexicalScheduler.Wake, embeddingScheduler.Wake},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Execute(t.Context(), testBinding(), "evt-group-wake", rememberText("group-subject")); err != nil {
		t.Fatal(err)
	}
	// The single committed mutation must wake all three, distinct scheduler
	// instances: each consumer must complete one more poll cycle.
	waitOnePoll(t, projectionTimers)
	waitOnePoll(t, lexicalTimers)
	waitOnePoll(t, embeddingTimers)
}
