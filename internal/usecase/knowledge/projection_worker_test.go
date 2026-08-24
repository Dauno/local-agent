package knowledge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

type blockedProjectionTimer struct{ c chan time.Time }

func (t blockedProjectionTimer) C() <-chan time.Time { return t.c }
func (blockedProjectionTimer) Stop() bool            { return true }

type workerTestLogger struct{}

func (workerTestLogger) Debug(string, ...any) {}
func (workerTestLogger) Info(string, ...any)  {}
func (workerTestLogger) Warn(string, ...any)  {}
func (workerTestLogger) Error(string, ...any) {}

type workerTestClock struct{ now time.Time }

func (c *workerTestClock) Now() time.Time { return c.now }

type workerTestReader struct{}

func (workerTestReader) ReadProjectionSnapshot(context.Context) (port.ProjectionSnapshot, error) {
	return port.ProjectionSnapshot{}, nil
}

type workerFakeStore struct {
	mu                sync.Mutex
	nextID            int
	pending           []domain.KnowledgeProjectionItem
	attemptsByID      map[int]int
	now               func() time.Time
	claims            int
	completeConflicts int
	completed         []int
	retried           []int
	retriedAttempts   []int
	retryGroups       [][]int
	retryTimes        []time.Time
	deferred          []int
	deferredAttempts  []int
	deferredTimes     []time.Time
	failed            []int
	failedCodes       []string
}

func newWorkerFakeStore() *workerFakeStore {
	return &workerFakeStore{attemptsByID: map[int]int{}, now: time.Now}
}

func (s *workerFakeStore) enqueue() domain.KnowledgeProjectionItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	item := domain.KnowledgeProjectionItem{ID: s.nextID, Status: domain.KnowledgeProjectionPending}
	s.pending = append(s.pending, item)
	return item
}

func (s *workerFakeStore) enqueueWithAttempts(attempts int) domain.KnowledgeProjectionItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	item := domain.KnowledgeProjectionItem{ID: s.nextID, Status: domain.KnowledgeProjectionPending, Attempts: attempts}
	s.attemptsByID[s.nextID] = attempts
	s.pending = append(s.pending, item)
	return item
}

func (s *workerFakeStore) ClaimProjectionBatch(context.Context) ([]domain.KnowledgeProjectionItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	now := s.now()
	var due []domain.KnowledgeProjectionItem
	var rest []domain.KnowledgeProjectionItem
	for _, item := range s.pending {
		if item.NextAttempt.IsZero() || !item.NextAttempt.After(now) {
			due = append(due, item)
		} else {
			rest = append(rest, item)
		}
	}
	if len(due) == 0 {
		return nil, nil
	}
	s.pending = rest
	for i := range due {
		due[i].Attempts++
		due[i].Status = domain.KnowledgeProjectionProcessing
		due[i].LeaseUntil = time.Unix(1, 0).UTC()
		s.attemptsByID[due[i].ID] = due[i].Attempts
	}
	return due, nil
}

func (s *workerFakeStore) CompleteProjectionBatch(_ context.Context, ids []int, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completeConflicts > 0 {
		s.completeConflicts--
		// A completion conflict means the lease was lost; the rows stay
		// processing in the store and become re-claimable after expiry.
		for _, id := range ids {
			s.pending = append(s.pending, domain.KnowledgeProjectionItem{ID: id, Attempts: s.attemptsByID[id]})
		}
		return port.ErrKnowledgeCASConflict
	}
	s.completed = append(s.completed, ids...)
	return nil
}

func (s *workerFakeStore) RetryProjectionBatch(_ context.Context, ids []int, _ time.Time, nextAttempt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retryGroups = append(s.retryGroups, append([]int(nil), ids...))
	s.retryTimes = append(s.retryTimes, nextAttempt)
	for _, id := range ids {
		s.retried = append(s.retried, id)
		s.retriedAttempts = append(s.retriedAttempts, s.attemptsByID[id])
		s.pending = append(s.pending, domain.KnowledgeProjectionItem{
			ID: id, Status: domain.KnowledgeProjectionPending,
			Attempts: s.attemptsByID[id], NextAttempt: nextAttempt,
		})
	}
	return nil
}

func (s *workerFakeStore) DeferProjectionCleanupBatch(_ context.Context, ids []int, _ time.Time, nextAttempt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		attempts := s.attemptsByID[id]
		if attempts > 0 {
			attempts--
		}
		s.attemptsByID[id] = attempts
		s.deferred = append(s.deferred, id)
		s.deferredAttempts = append(s.deferredAttempts, attempts)
		s.pending = append(s.pending, domain.KnowledgeProjectionItem{
			ID: id, Status: domain.KnowledgeProjectionPending,
			Attempts: attempts, NextAttempt: nextAttempt,
		})
	}
	s.deferredTimes = append(s.deferredTimes, nextAttempt)
	return nil
}

func (s *workerFakeStore) FailProjectionBatch(_ context.Context, ids []int, _ time.Time, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = append(s.failed, ids...)
	s.failedCodes = append(s.failedCodes, code)
	return nil
}

func (s *workerFakeStore) CleanupProjection(context.Context, time.Time) error { return nil }

func (s *workerFakeStore) snapshot() workerFakeStoreSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return workerFakeStoreSnapshot{
		claims: s.claims, completed: len(s.completed), retried: append([]int(nil), s.retried...),
		retriedAttempts: append([]int(nil), s.retriedAttempts...), failed: append([]int(nil), s.failed...),
		failedCodes: append([]string(nil), s.failedCodes...), pending: len(s.pending),
		retryGroups: append([][]int(nil), s.retryGroups...), retryTimes: append([]time.Time(nil), s.retryTimes...),
		deferred: append([]int(nil), s.deferred...), deferredAttempts: append([]int(nil), s.deferredAttempts...),
		deferredTimes: append([]time.Time(nil), s.deferredTimes...),
	}
}

type workerFakeStoreSnapshot struct {
	claims           int
	completed        int
	retried          []int
	retriedAttempts  []int
	retryGroups      [][]int
	retryTimes       []time.Time
	deferred         []int
	deferredAttempts []int
	deferredTimes    []time.Time
	failed           []int
	failedCodes      []string
	pending          int
}

type workerFakeProjector struct {
	mu           sync.Mutex
	calls        int
	failFirst    int
	cleanupErr   error
	onProject    func(call int)
	rendered     chan struct{}
	recoverCalls int
}

func (f *workerFakeProjector) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *workerFakeProjector) recoverCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recoverCalls
}

func (f *workerFakeProjector) Recover(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recoverCalls++
	return nil
}

func (f *workerFakeProjector) Project(ctx context.Context, _ port.ProjectionReader, _ string) error {
	f.mu.Lock()
	f.calls++
	call := f.calls
	shouldFail := call <= f.failFirst
	cleanupErr := f.cleanupErr
	hook := f.onProject
	f.mu.Unlock()
	if shouldFail {
		return errors.New("transient projection failure")
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	if hook != nil {
		hook(call)
	}
	if f.rendered != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f.rendered <- struct{}{}:
		}
	}
	return nil
}

type workerBlockingProjector struct {
	entered chan struct{}
	once    sync.Once
}

func (p *workerBlockingProjector) Recover(string) error { return nil }

func (p *workerBlockingProjector) Project(ctx context.Context, _ port.ProjectionReader, _ string) error {
	p.once.Do(func() { close(p.entered) })
	<-ctx.Done()
	return ctx.Err()
}

func newWorkerUnderTest(t *testing.T, store port.KnowledgeProjectionStore, projector port.OKFProjector, maxRetries int, enabled func() bool) (*ProjectionWorker, *workerTestClock) {
	t.Helper()
	clock := &workerTestClock{now: time.Unix(1_700_000_000, 0).UTC()}
	if fake, ok := store.(*workerFakeStore); ok {
		fake.now = func() time.Time { return clock.now }
	}
	worker, err := NewProjectionWorker(ProjectionWorkerConfig{
		Interval: time.Hour, MaxRetries: maxRetries, RetentionDays: 90, OutputDir: t.TempDir(),
	}, ProjectionWorkerDependencies{
		Store: store, Reader: workerTestReader{}, Projector: projector,
		Clock: clock, Logger: workerTestLogger{}, Enabled: enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker, clock
}

func TestProjectionWorkerCoalescesPendingTriggersIntoOneRender(t *testing.T) {
	store := newWorkerFakeStore()
	for range 3 {
		store.enqueue()
	}
	projector := &workerFakeProjector{}
	worker, _ := newWorkerUnderTest(t, store, projector, 3, nil)
	worker.tick(t.Context())
	if projector.calls != 1 {
		t.Fatalf("render calls = %d, want 1 for three pending triggers", projector.calls)
	}
	snap := store.snapshot()
	if snap.completed != 3 || snap.retried != nil || snap.failed != nil {
		t.Fatalf("store state = %+v, want 3 completed and no retries or failures", snap)
	}
}

func TestProjectionWorkerMutationDuringRenderForcesSecondSnapshot(t *testing.T) {
	store := newWorkerFakeStore()
	store.enqueue()
	var mu sync.Mutex
	renders := 0
	projector := &workerFakeProjector{
		onProject: func(int) {
			mu.Lock()
			defer mu.Unlock()
			renders++
			if renders == 1 {
				store.enqueue()
			}
		},
	}
	worker, _ := newWorkerUnderTest(t, store, projector, 3, nil)
	worker.tick(t.Context())
	if projector.calls != 2 {
		t.Fatalf("render calls = %d, want 2: mutation during render must force a second snapshot", projector.calls)
	}
	snap := store.snapshot()
	if snap.completed != 2 {
		t.Fatalf("completed = %d, want 2", snap.completed)
	}
}

func TestProjectionWorkerTransientErrorRetriesPreservingAttempts(t *testing.T) {
	store := newWorkerFakeStore()
	store.enqueue()
	projector := &workerFakeProjector{failFirst: 1}
	worker, clock := newWorkerUnderTest(t, store, projector, 3, nil)
	worker.tick(t.Context())
	snap := store.snapshot()
	if snap.retriedAttempts == nil || len(snap.retriedAttempts) != 1 || snap.retriedAttempts[0] != 1 {
		t.Fatalf("retry attempts = %v, want [1] preserved", snap.retriedAttempts)
	}
	if snap.failed != nil {
		t.Fatalf("transient error must not fail terminal: %+v", snap)
	}
	clock.now = clock.now.Add(2 * time.Minute)
	worker.tick(t.Context())
	snap = store.snapshot()
	if snap.completed != 1 {
		t.Fatalf("recovered batch was not completed: %+v", snap)
	}
}

func TestProjectionWorkerMaxRetriesFailsTerminalWithStableCode(t *testing.T) {
	store := newWorkerFakeStore()
	store.enqueue()
	projector := &workerFakeProjector{failFirst: 100}
	worker, clock := newWorkerUnderTest(t, store, projector, 2, nil)
	worker.tick(t.Context())
	clock.now = clock.now.Add(time.Hour)
	worker.tick(t.Context())
	clock.now = clock.now.Add(time.Hour)
	worker.tick(t.Context())
	snap := store.snapshot()
	if len(snap.retried) != 2 {
		t.Fatalf("retries = %v, want 2 attempts retried", snap.retried)
	}
	if snap.retriedAttempts == nil || snap.retriedAttempts[0] != 1 || snap.retriedAttempts[1] != 2 {
		t.Fatalf("retry attempts = %v, want [1 2] preserved monotonically", snap.retriedAttempts)
	}
	if len(snap.failed) != 1 || len(snap.failedCodes) != 1 || snap.failedCodes[0] != port.KnowledgeProjectionExhaustedCode {
		t.Fatalf("terminal failure = %+v, want one failed with %q", snap, port.KnowledgeProjectionExhaustedCode)
	}
}

func TestProjectionWorkerHeterogeneousBatchSchedulesPerRowDelay(t *testing.T) {
	store := newWorkerFakeStore()
	fresh := store.enqueueWithAttempts(1)
	older := store.enqueueWithAttempts(2)
	projector := &workerFakeProjector{failFirst: 100}
	worker, _ := newWorkerUnderTest(t, store, projector, 5, nil)
	worker.tick(t.Context())
	snap := store.snapshot()
	if len(snap.retryGroups) != 2 || len(snap.retryTimes) != 2 {
		t.Fatalf("retry calls = %d, want 2 per-attempt groups", len(snap.retryGroups))
	}
	base := time.Unix(1_700_000_000, 0)
	delays := map[int]time.Duration{}
	for i, group := range snap.retryGroups {
		if len(group) != 1 {
			t.Fatalf("retry group %d has %d rows, want 1", i, len(group))
		}
		delays[group[0]] = snap.retryTimes[i].Sub(base)
	}
	// Fresh row claims attempts 2 -> 2-minute backoff; older row claims
	// attempts 3 -> 4-minute backoff. Neither inherits the other's delay.
	if delays[fresh.ID] != 2*time.Minute {
		t.Fatalf("fresh trigger retry delay = %v, want 2m", delays[fresh.ID])
	}
	if delays[older.ID] != 4*time.Minute {
		t.Fatalf("older trigger retry delay = %v, want 4m", delays[older.ID])
	}
}

func TestProjectionWorkerCompletionCASConflictKeepsTriggerPending(t *testing.T) {
	store := newWorkerFakeStore()
	store.enqueue()
	store.completeConflicts = 1
	projector := &workerFakeProjector{}
	worker, _ := newWorkerUnderTest(t, store, projector, 3, nil)
	worker.tick(t.Context())
	snap := store.snapshot()
	if snap.failed != nil || snap.retried != nil {
		t.Fatalf("completion conflict must not fail or retry: %+v", snap)
	}
	if snap.completed != 1 {
		t.Fatalf("conflicted trigger was lost: %+v", snap)
	}
	if projector.calls != 2 {
		t.Fatalf("render calls = %d, want 2: conflicted batch re-rendered after lease recovery", projector.calls)
	}
}

func TestProjectionWorkerDisabledGateClaimsNothingAndDrainsOnReEnable(t *testing.T) {
	store := newWorkerFakeStore()
	store.enqueue()
	projector := &workerFakeProjector{}
	var gate atomic.Bool
	worker, _ := newWorkerUnderTest(t, store, projector, 3, gate.Load)
	worker.tick(t.Context())
	if snap := store.snapshot(); snap.claims != 0 || projector.calls != 0 || snap.completed != 0 {
		t.Fatalf("disabled gate touched the outbox: %+v", snap)
	}
	gate.Store(true)
	worker.tick(t.Context())
	snap := store.snapshot()
	if snap.completed != 1 || projector.calls != 1 || snap.claims == 0 {
		t.Fatalf("re-enabled gate did not drain pending immediately: %+v", snap)
	}
}

func TestProjectionWorkerCancellationDuringProjectionDoesNotFailBatch(t *testing.T) {
	store := newWorkerFakeStore()
	store.enqueue()
	projector := &workerBlockingProjector{entered: make(chan struct{})}
	worker, _ := newWorkerUnderTest(t, store, projector, 3, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.processBatches(ctx)
	}()
	<-projector.entered
	cancel()
	<-done
	snap := store.snapshot()
	if snap.failed != nil || snap.retried != nil {
		t.Fatalf("cancellation must not fail or retry the batch: %+v", snap)
	}
}

func TestProjectionWorkerCleanupFailureKeepsBatchPending(t *testing.T) {
	store := newWorkerFakeStore()
	store.enqueue()
	projector := &workerFakeProjector{cleanupErr: port.ErrProjectionCleanup}
	worker, clock := newWorkerUnderTest(t, store, projector, 3, nil)
	worker.tick(t.Context())
	snap := store.snapshot()
	if snap.completed != 0 {
		t.Fatalf("cleanup failure must not complete the batch: %+v", snap)
	}
	if snap.failed != nil {
		t.Fatalf("cleanup failure must not fail the batch terminal: %+v", snap)
	}
	if snap.retried != nil {
		t.Fatalf("cleanup failure must not use the projection retry path: %+v", snap)
	}
	if len(snap.deferred) != 1 || snap.deferredAttempts[0] != 0 {
		t.Fatalf("cleanup failure must defer the batch without consuming attempts: %+v", snap)
	}
	// A later attempt removes the backup and completes the same trigger.
	projector.cleanupErr = nil
	clock.now = clock.now.Add(2 * time.Minute)
	worker.tick(t.Context())
	snap = store.snapshot()
	if snap.completed != 1 {
		t.Fatalf("recovered batch was not completed: %+v", snap)
	}
}

func TestProjectionWorkerCleanupRetriesNeverExhaustBudget(t *testing.T) {
	store := newWorkerFakeStore()
	store.enqueue()
	projector := &workerFakeProjector{cleanupErr: port.ErrProjectionCleanup}
	// MaxRetries=1: one extra consumed attempt would fail the row
	// terminal. Cleanup deferrals must cycle at the same budget value
	// until the backup can actually be removed.
	worker, clock := newWorkerUnderTest(t, store, projector, 1, nil)
	for i := range 4 {
		worker.tick(t.Context())
		snap := store.snapshot()
		if snap.failed != nil {
			t.Fatalf("cleanup retry %d failed the batch terminal: %+v", i, snap)
		}
		if snap.completed != 0 {
			t.Fatalf("cleanup retry %d completed the batch: %+v", i, snap)
		}
		clock.now = clock.now.Add(2 * time.Minute)
	}
	snap := store.snapshot()
	if len(snap.deferred) != 4 {
		t.Fatalf("cleanup deferrals = %d, want 4", len(snap.deferred))
	}
	for _, attempts := range snap.deferredAttempts {
		if attempts != 0 {
			t.Fatalf("cleanup deferral consumed the projection budget: attempts = %d", attempts)
		}
	}
	// Once the backup can be removed, the same trigger completes without
	// any terminal state.
	projector.cleanupErr = nil
	clock.now = clock.now.Add(2 * time.Minute)
	worker.tick(t.Context())
	snap = store.snapshot()
	if snap.completed != 1 {
		t.Fatalf("recovered batch was not completed: %+v", snap)
	}
}

func TestProjectionWorkerRecoversResidueAtStartup(t *testing.T) {
	store := newWorkerFakeStore()
	store.enqueue()
	projector := &workerFakeProjector{rendered: make(chan struct{}, 1)}
	worker, _ := newWorkerUnderTest(t, store, projector, 3, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go worker.Run(ctx)
	select {
	case <-projector.rendered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not process at startup")
	}
	if projector.recoverCount() != 1 {
		t.Fatalf("startup recovery calls = %d, want 1 before processing", projector.recoverCount())
	}
	cancel()
}

func TestProjectionWorkerProcessesImmediatelyOnStart(t *testing.T) {
	store := newWorkerFakeStore()
	store.enqueue()
	projector := &workerFakeProjector{rendered: make(chan struct{}, 1)}
	worker, _ := newWorkerUnderTest(t, store, projector, 3, nil)
	waiting := make(chan struct{})
	allowTimer := make(chan struct{})
	scheduler, err := workpoll.New(time.Hour, workpoll.Options{
		NewTimer: func(time.Duration) workpoll.Timer {
			close(waiting)
			<-allowTimer
			return blockedProjectionTimer{c: make(chan time.Time)}
		},
		Jitter: func(base time.Duration) time.Duration { return base },
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.scheduler = scheduler
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go worker.Run(ctx)
	<-waiting
	select {
	case <-projector.rendered:
	default:
		t.Fatal("worker waited for recovery timer before startup poll")
	}
	cancel()
	close(allowTimer)
	if projector.callCount() < 1 {
		t.Fatalf("render calls = %d, want at least 1", projector.callCount())
	}
}
