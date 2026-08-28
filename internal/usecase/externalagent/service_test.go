package externalagent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/fsartifact"
	"github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestDetachedJobIsPersistedBeforeWorkerCompletesIt(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runtime := &fakeJobRuntime{result: domain.ExternalAgentInvocationResult{Text: "done", Inline: true, ResultBytes: 4}}
	service, err := New(
		Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: 2 * time.Second, PollInterval: 10 * time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: sqlite.NewExternalAgentJobStore(store), Runtime: runtime},
	)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), testRequest(domain.JobDetached))
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != domain.JobQueued {
		t.Fatalf("accepted job status = %s", job.Status)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go service.Run(ctx)
	finished := waitForJob(t, sqlite.NewExternalAgentJobStore(store), job.ID, domain.JobCompleted)
	if runtime.calls != 1 || finished.ResultSummary != "done" {
		t.Fatalf("calls = %d, finished = %#v", runtime.calls, finished)
	}
}

func TestJobAndNotificationWakesFollowDurableCommits(t *testing.T) {
	store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "wake.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	jobWakes, notificationWakes := 0, 0
	service, err := New(Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute, PollInterval: time.Minute, Concurrency: 1, MaxAttempts: 1}, Dependencies{
		Store: jobStore, Runtime: &fakeJobRuntime{}, JobWake: func() { jobWakes++ }, NotificationWake: func() { notificationWakes++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), testRequest(domain.JobDetached))
	if err != nil {
		t.Fatal(err)
	}
	if jobWakes != 1 {
		t.Fatalf("job wake count = %d, want 1", jobWakes)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), time.Now().UTC(), "wake-owner", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext() = %#v, %v", claimed, err)
	}
	if err := service.transition(
		t.Context(),
		job.ID,
		claimed.LeaseOwner,
		claimed.Attempt,
		domain.JobCompleted,
		&domain.ExternalAgentInvocationResult{Text: "done", Inline: true, ResultBytes: 4},
		"",
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if notificationWakes != 1 {
		t.Fatalf("notification wake count = %d, want 1", notificationWakes)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start(t.Context(), testRequest(domain.JobDetached)); err == nil {
		t.Fatal("Start() after store close succeeded")
	}
	if jobWakes != 1 || notificationWakes != 1 {
		t.Fatalf("failed write wake counts = jobs %d, notifications %d", jobWakes, notificationWakes)
	}
}

func TestStopAdmissionDrainsRunningJobWithoutClaimingQueuedWork(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "shutdown.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	runtime := &fakeJobRuntime{result: domain.ExternalAgentInvocationResult{Text: "drained", ResultBytes: 7}, block: make(chan struct{})}
	service, err := New(
		Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1},
		Dependencies{Store: jobStore, Runtime: runtime},
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Start(t.Context(), testRequest(domain.JobDetached))
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := testRequest(domain.JobDetached)
	secondRequest.OriginalCallID = "original-2"
	second, err := service.Start(t.Context(), secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { service.Run(context.Background()); close(done) }()
	waitForJob(t, jobStore, first.ID, domain.JobRunning)
	service.StopAdmission()
	close(runtime.block)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("service did not stop after draining")
	}
	finished, err := jobStore.GetJob(t.Context(), first.ID)
	if err != nil || finished == nil || finished.Status != domain.JobCompleted {
		t.Fatalf("running job after drain = %#v, err=%v", finished, err)
	}
	queued, err := jobStore.GetJob(t.Context(), second.ID)
	if err != nil || queued == nil || queued.Status != domain.JobQueued {
		t.Fatalf("queued job after stop = %#v, err=%v", queued, err)
	}
}

func TestReconciliationRenewsLeaseUntilCompletion(t *testing.T) {
	store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "reconcile.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	renewed := make(chan struct{})
	jobStore := &recordingJobStore{ExternalAgentJobStore: sqlite.NewExternalAgentJobStore(store), renewed: renewed}
	runtime := &fakeRecoveryRuntime{wait: renewed, result: domain.ExternalAgentInvocationResult{Text: "done", ResultBytes: 4}}
	clock := fixedClock{now: time.Now().UTC()}
	service, err := New(
		Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: 30 * time.Millisecond, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: jobStore, Runtime: runtime, Clock: clock},
	)
	if err != nil {
		t.Fatal(err)
	}
	job := createCompletionUnknownJob(t, service, jobStore)
	if _, err := service.ReconcileExpected(t.Context(), job.ID, job.Actor, job.ConversationKey, job.StatusRevision); err != nil {
		jobStore.mu.Lock()
		renewErr := jobStore.lastRenewErr
		jobStore.mu.Unlock()
		t.Fatalf("reconcile: %v; renew: %v", err, renewErr)
	}
	completed, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || completed == nil || completed.Status != domain.JobCompleted {
		t.Fatalf("completed job = %#v, err=%v", completed, err)
	}
}

func TestExpiredReconciliationReturnsToCompletionUnknown(t *testing.T) {
	store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "expired-reconcile.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	service, err := New(
		Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: 20 * time.Millisecond, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: jobStore, Runtime: &fakeRecoveryRuntime{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	job := createCompletionUnknownJob(t, service, jobStore)
	reconciling, err := jobStore.BeginReconciliationExpected(t.Context(), job.ID, job.Actor, job.ConversationKey, job.StatusRevision, time.Now().UTC(), "reconciler-test", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	service.recoverExpired(t.Context(), nil, nil)
	recovered, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || recovered == nil || recovered.Status != domain.JobCompletionUnknown || recovered.StatusRevision != reconciling.StatusRevision+1 {
		t.Fatalf("recovered job = %#v, err=%v", recovered, err)
	}
}

// A daemon restart leaves the old worker lease expired. A persisted session ID
// lets the new worker reconcile without replaying the original task or waiting
// for an operator command.
func TestExpiredRunningJobAutomaticallyReconcilesPersistedSession(t *testing.T) {
	store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "automatic-reconcile.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	runtime := &fakeRecoveryRuntime{result: domain.ExternalAgentInvocationResult{Text: "done", ResultBytes: 4}}
	// The recovery lease must outlive one heartbeat under test load. Expiry of
	// the dead worker's lease is driven by its own claim TTL below, not by this.
	service, err := New(
		Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: 2 * time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: jobStore, Runtime: runtime},
	)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), testRequest(domain.JobDetached))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimed, err := jobStore.ClaimNext(t.Context(), now, "dead-worker", time.Second)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, err = %v", claimed, err)
	}
	if err := jobStore.AssignExternalAgentSession(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, "session-automatic"); err != nil {
		t.Fatal(err)
	}
	if err := jobStore.MarkSideEffectsPossible(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.Run(runCtx)
	}()
	completed := waitForJob(t, jobStore, job.ID, domain.JobCompleted)
	cancel()
	<-done
	if completed.ExternalAgentSessionID != "session-automatic" || completed.ResultSummary != "done" {
		t.Fatalf("completed job = %#v", completed)
	}
}

// A job the actor cancelled must never be resumed automatically. It reaches
// completion_unknown like any job with possible side effects, but resuming it
// would complete work that was stopped on purpose.
func TestCancelledJobIsNotAutomaticallyResumed(t *testing.T) {
	store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "cancelled-no-resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	runtime := &fakeRecoveryRuntime{result: domain.ExternalAgentInvocationResult{Text: "done", ResultBytes: 4}}
	service, err := New(
		Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: 20 * time.Millisecond, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: jobStore, Runtime: runtime},
	)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), testRequest(domain.JobDetached))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), time.Now().UTC(), "dead-worker", time.Second)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, err = %v", claimed, err)
	}
	if err := jobStore.AssignExternalAgentSession(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, "session-cancelled"); err != nil {
		t.Fatal(err)
	}
	if err := jobStore.MarkSideEffectsPossible(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt); err != nil {
		t.Fatal(err)
	}
	// The actor cancels, then the worker dies and its lease expires.
	if _, err := jobStore.RequestCancellation(t.Context(), job.ID, job.Actor); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.Run(runCtx)
	}()
	awaiting := waitForJob(t, jobStore, job.ID, domain.JobCompletionUnknown)
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	final, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || final == nil {
		t.Fatalf("job = %#v, err = %v", final, err)
	}
	if final.Status != domain.JobCompletionUnknown || final.StatusRevision != awaiting.StatusRevision {
		t.Fatalf("a cancelled job must rest untouched in completion_unknown, got %#v", final)
	}
}

// A reconciliation that loses its own lease returns to completion_unknown and
// stops there. Resuming it again would retry without a bound.
func TestExpiredReconciliationIsNotAutomaticallyRetried(t *testing.T) {
	store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "expired-no-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	service, err := New(
		Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: 20 * time.Millisecond, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: jobStore, Runtime: &fakeRecoveryRuntime{result: domain.ExternalAgentInvocationResult{Text: "done", ResultBytes: 4}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	job := createCompletionUnknownJob(t, service, jobStore)
	reconciling, err := jobStore.BeginReconciliationExpected(t.Context(), job.ID, job.Actor, job.ConversationKey, job.StatusRevision, time.Now().UTC(), "reconciler-dead", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	var workers sync.WaitGroup
	sem := make(chan struct{}, 1)
	service.recoverExpired(t.Context(), sem, &workers)
	workers.Wait()
	recovered, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || recovered == nil {
		t.Fatalf("job = %#v, err = %v", recovered, err)
	}
	if recovered.Status != domain.JobCompletionUnknown || recovered.StatusRevision != reconciling.StatusRevision+1 {
		t.Fatalf("an expired reconciliation must rest in completion_unknown, got %#v", recovered)
	}
}

func TestRunningJobCancellationIsIdempotentBeforeSideEffects(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runtime := &fakeJobRuntime{block: make(chan struct{})}
	jobStore := sqlite.NewExternalAgentJobStore(store)
	service, err := New(
		Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 10 * time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: jobStore, Runtime: runtime},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(domain.JobDetached)
	job, err := service.Start(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go service.Run(ctx)
	waitForJob(t, jobStore, job.ID, domain.JobRunning)
	if _, err := service.Cancel(t.Context(), job.ID, request.Actor); err != nil {
		t.Fatal(err)
	}
	waitForJob(t, jobStore, job.ID, domain.JobCancelled)
	if _, err := service.Cancel(t.Context(), job.ID, request.Actor); err != nil {
		t.Fatal(err)
	}
}

// TestJobTotalTimeoutIsTerminalAndNeverRequeued exercises the total-timeout
// path deterministically: it advances a fake clock past the job's TimeoutAt
// and calls the unexported execute() directly instead of running the real
// background scheduler, so the assertion never depends on goroutine
// scheduling latency winning a race against a wall-clock poll deadline.
func TestJobTotalTimeoutIsTerminalAndNeverRequeued(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	runtime := &fakeJobRuntime{block: make(chan struct{})}
	clock := &fakeClock{now: time.Now().UTC()}
	service, err := New(
		Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: 10 * time.Second, PollInterval: time.Hour, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: jobStore, Runtime: runtime, Clock: clock},
	)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), testRequestWithTimeout(domain.JobDetached, 30*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), clock.Now(), "worker-test", time.Minute)
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("claim = %#v, err = %v", claimed, err)
	}
	clock.advance(31 * time.Millisecond)
	service.execute(t.Context(), claimed)

	finished, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || finished == nil {
		t.Fatalf("finished job = %#v, err = %v", finished, err)
	}
	if finished.Status != domain.JobFailed || finished.ErrorCode != "acp_job_timeout" {
		t.Fatalf("finished = %#v", finished)
	}
	requeued, err := jobStore.ClaimNext(t.Context(), clock.Now(), "worker-other", time.Minute)
	if err != nil || requeued != nil {
		t.Fatalf("requeued = %#v, err = %v; want no further claim after terminal timeout", requeued, err)
	}
}

func TestForegroundWaitReturnsWhenQueuedJobTotalTimeoutExpires(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := New(
		Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: sqlite.NewExternalAgentJobStore(store), Runtime: &fakeJobRuntime{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = service.StartAndWait(t.Context(), testRequestWithTimeout(domain.JobForeground, 20*time.Millisecond))
	if err == nil || time.Since(started) > time.Second || !strings.Contains(err.Error(), "acp_job_timeout") {
		t.Fatalf("error = %v, elapsed = %s", err, time.Since(started))
	}
}

func TestForegroundWaitDoesNotBlockOnExpiredJobCancellation(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := &blockingCancellationStore{
		ExternalAgentJobStore: sqlite.NewExternalAgentJobStore(store),
		started:               make(chan struct{}),
		release:               make(chan struct{}),
		finished:              make(chan struct{}),
		polled:                make(chan struct{}),
	}
	clock := &fakeClock{now: time.Now().UTC()}
	timeout := time.Hour
	service, err := New(
		Config{DefaultTimeout: time.Second, MaxTimeout: 24 * time.Hour, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}, Clock: clock},
	)
	if err != nil {
		t.Fatal(err)
	}

	waitErr := make(chan error, 1)
	go func() {
		_, err := service.StartAndWait(t.Context(), testRequestWithTimeout(domain.JobForeground, timeout))
		waitErr <- err
	}()

	// Wait for the foreground wait loop to observe the job it just created,
	// then push the fake clock past its deadline. This drives the timeout
	// path deterministically instead of racing a real-time sleep against a
	// real poll interval.
	select {
	case <-jobStore.polled:
	case <-time.After(2 * time.Second):
		t.Fatal("foreground wait loop did not poll the new job")
	}
	clock.advance(timeout + time.Second)

	select {
	case err := <-waitErr:
		if err == nil || !strings.Contains(err.Error(), "acp_job_timeout") {
			t.Fatalf("error = %v, want acp_job_timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("foreground wait did not return after the fake clock passed the deadline")
	}
	select {
	case <-jobStore.started:
	case <-time.After(2 * time.Second):
		t.Fatal("foreground timeout did not attempt durable cancellation")
	}
	close(jobStore.release)
	select {
	case <-jobStore.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("background cancellation did not finish")
	}
}

func TestStartRejectsUnboundConversation(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := New(
		Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: sqlite.NewExternalAgentJobStore(store), Runtime: &fakeJobRuntime{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(domain.JobDetached)
	request.ConversationKey = "unbound"
	if _, err := service.Start(t.Context(), request); err == nil || !strings.Contains(err.Error(), "conversation binding") {
		t.Fatalf("error = %v", err)
	}
}

func TestSafeRetryDoesNotPublishQueuedAsTerminal(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	runtime := &fakeJobRuntime{block: make(chan struct{})}
	publisher := &fakeJobPublisher{}
	service, err := New(
		Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: jobStore, Runtime: runtime, Publisher: publisher},
	)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), testRequest(domain.JobDetached))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { service.Run(ctx); close(done) }()
	waitForJob(t, jobStore, job.ID, domain.JobRunning)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("service did not stop")
	}
	queued, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || queued == nil || queued.Status != domain.JobQueued {
		t.Fatalf("retry state = %#v, err = %v", queued, err)
	}
	if publisher.calls != 0 {
		t.Fatalf("terminal publications = %d, want 0 for queued retry", publisher.calls)
	}
}

func TestTransientExternalAgentProcessExitRetriesBeforeSession(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	runtime := &fakeJobRuntime{
		result: domain.ExternalAgentInvocationResult{Text: "done", Inline: true, ResultBytes: 4},
		errs:   []error{&domain.ExternalAgentError{Code: domain.ExternalAgentErrorProcessExit, Err: context.Canceled}},
	}
	service, err := New(
		Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: jobStore, Runtime: runtime},
	)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), testRequest(domain.JobDetached))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go service.Run(ctx)
	finished := waitForJob(t, jobStore, job.ID, domain.JobCompleted)
	if runtime.calls != 2 || finished.Attempt != 2 {
		t.Fatalf("calls = %d, finished attempt = %d", runtime.calls, finished.Attempt)
	}
}

func TestNonRetryableExternalAgentErrorPreservesCode(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	runtime := &fakeJobRuntime{errs: []error{&domain.ExternalAgentError{Code: domain.ExternalAgentErrorConfigDrift, Err: context.Canceled}}}
	service, err := New(
		Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: jobStore, Runtime: runtime},
	)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), testRequest(domain.JobDetached))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), time.Now().UTC(), "worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, err = %v", claimed, err)
	}
	service.execute(t.Context(), claimed)
	finished, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || finished == nil || finished.Status != domain.JobFailed {
		t.Fatalf("finished = %#v, err = %v; want failed", finished, err)
	}
	if runtime.calls != 1 || finished.ErrorCode != string(domain.ExternalAgentErrorConfigDrift) {
		t.Fatalf("calls = %d, error code = %q", runtime.calls, finished.ErrorCode)
	}
}

func TestHostCompletionReadsOnlyAuthorizedCompleteResult(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	content := "complete sanitized result"
	digest := sha256.Sum256([]byte(content))
	job := testRequest(domain.JobDetached)
	service, err := New(
		Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1},
		Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Start(context.Background(), job)
	if err != nil || created == nil {
		t.Fatalf("start = %#v, err = %v", created, err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), time.Now().UTC(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Transition(t.Context(), created.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.ExternalAgentInvocationResult{
		Text: content, ResultSHA256: fmt.Sprintf("%x", digest), ResultBytes: int64(len(content)), DeliveryMode: domain.JobResultDeliveryMarkdown,
	}, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReadResult(t.Context(), created.ID, job.Actor, job.ConversationKey)
	if err != nil || result.Text != content || result.ContentSHA256 != fmt.Sprintf("%x", digest) {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if _, err := service.ReadResult(t.Context(), created.ID, "U99999999", job.ConversationKey); err == nil {
		t.Fatal("wrong actor read the job result")
	}
	if _, err := service.ReadResult(t.Context(), created.ID, job.Actor, "slack:T12345678:dm:D99999999"); err == nil {
		t.Fatal("wrong conversation read the job result")
	}
	turn, err := service.HostCompletionTurn(t.Context(), created.ID, job.Actor, job.ConversationKey)
	if err != nil || turn.Text != content || turn.PendingConfirmation != nil {
		t.Fatalf("completion turn = %#v, err = %v", turn, err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE external_agent_jobs SET result_bytes = result_bytes + 1 WHERE job_id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReadResult(t.Context(), created.ID, job.Actor, job.ConversationKey); err == nil || !strings.Contains(err.Error(), string(domain.ResultErrorIdentityInvalid)) {
		t.Fatalf("altered ResultBytes was accepted: %v", err)
	}
}

func TestIdentityVerificationFailureRecordsBoundedCounter(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	metrics := &recordingNotificationMetrics{}
	jobStore := sqlite.NewExternalAgentJobStore(store)
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1}, Dependencies{
		Store: jobStore, Runtime: &fakeJobRuntime{}, Metrics: metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	content := "complete sanitized result"
	digest := sha256.Sum256([]byte(content))
	request := testRequest(domain.JobDetached)
	created, err := service.Start(t.Context(), request)
	if err != nil || created == nil {
		t.Fatalf("start = %#v, err = %v", created, err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), time.Now().UTC(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Transition(t.Context(), created.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.ExternalAgentInvocationResult{
		Text: content, ResultSHA256: fmt.Sprintf("%x", digest), ResultBytes: int64(len(content)), DeliveryMode: domain.JobResultDeliveryMarkdown,
	}, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE external_agent_jobs SET result_bytes = result_bytes + 1 WHERE job_id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReadResult(t.Context(), created.ID, request.Actor, request.ConversationKey); err == nil {
		t.Fatal("altered identity was accepted")
	}
	count := int64(0)
	for _, sample := range metrics.samples {
		if sample.Name == domain.MetricExternalAgentResultIdentityInvalidTotal && len(sample.Labels) == 0 {
			count += int64(sample.Value)
		}
	}
	if count != 1 {
		t.Fatalf("identity failure counter = %d, want 1; samples = %#v", count, metrics.samples)
	}
}

// TestIdentityFailureCounterCoversFullChunkAndAdapterReads pins CR7: the
// label-free identity-failure counter increments exactly once per classified
// identity failure regardless of the read route (full inline read, chunk
// reads, or classified artifact-adapter mismatches), and healthy reads never
// increment it.
func TestIdentityFailureCounterCoversFullChunkAndAdapterReads(t *testing.T) {
	content := "complete sanitized result"
	digest := sha256.Sum256([]byte(content))
	tests := []struct {
		name      string
		mode      domain.ExternalAgentJobMode
		complete  func(t *testing.T, job *domain.ExternalAgentJob, db *sqlite.Store, jobStore port.ExternalAgentJobStore, artifacts port.ResultArtifactStore, stateDir string) domain.ExternalAgentInvocationResult
		tamper    func(t *testing.T, job *domain.ExternalAgentJob, db *sqlite.Store, stateDir string)
		read      func(service *Service, job *domain.ExternalAgentJob) error
		wantCount int64
	}{
		{
			name: "full read inline identity failure",
			mode: domain.JobForeground,
			complete: func(t *testing.T, job *domain.ExternalAgentJob, db *sqlite.Store, jobStore port.ExternalAgentJobStore, artifacts port.ResultArtifactStore, stateDir string) domain.ExternalAgentInvocationResult {
				return domain.ExternalAgentInvocationResult{Text: content, ResultSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("other"))), ResultBytes: int64(len(content))}
			},
			read: func(service *Service, job *domain.ExternalAgentJob) error {
				_, err := service.ReadResult(t.Context(), job.ID, "U12345678", "slack:T12345678:dm:D12345678")
				return err
			},
			wantCount: 1,
		},
		{
			name: "full read artifact owner mismatch",
			mode: domain.JobDetached,
			complete: func(t *testing.T, job *domain.ExternalAgentJob, db *sqlite.Store, jobStore port.ExternalAgentJobStore, artifacts port.ResultArtifactStore, stateDir string) domain.ExternalAgentInvocationResult {
				artifact, err := artifacts.(*fsartifact.Store).Put(t.Context(), job.ID+"-delivery", content)
				if err != nil {
					t.Fatal(err)
				}
				return domain.ExternalAgentInvocationResult{
					DeliveryMode:             domain.JobResultDeliveryFile,
					ArtifactRef:              "foreign-delivery.result",
					ResultSHA256:             artifact.SHA256,
					ResultBytes:              artifact.Bytes,
					DeliveryArtifactRef:      "foreign-delivery.result",
					DeliveryContentSHA256:    artifact.SHA256,
					DeliveryContentBytes:     artifact.Bytes,
					DeliveryPolicyVersion:    domain.JobDeliveryPolicyV1,
					DeliveryMaxMarkdownParts: 6,
				}
			},
			read: func(service *Service, job *domain.ExternalAgentJob) error {
				_, err := service.ReadResult(t.Context(), job.ID, "U12345678", "slack:T12345678:dm:D12345678")
				return err
			},
			wantCount: 1,
		},
		{
			name: "chunk read inline identity failure",
			mode: domain.JobForeground,
			complete: func(t *testing.T, job *domain.ExternalAgentJob, db *sqlite.Store, jobStore port.ExternalAgentJobStore, artifacts port.ResultArtifactStore, stateDir string) domain.ExternalAgentInvocationResult {
				return domain.ExternalAgentInvocationResult{Text: content, ResultSHA256: fmt.Sprintf("%x", digest), ResultBytes: int64(len(content))}
			},
			tamper: func(t *testing.T, job *domain.ExternalAgentJob, db *sqlite.Store, stateDir string) {
				if _, err := db.DB().ExecContext(t.Context(), `UPDATE external_agent_jobs SET result_bytes = result_bytes + 1 WHERE job_id = ?`, job.ID); err != nil {
					t.Fatal(err)
				}
			},
			read: func(service *Service, job *domain.ExternalAgentJob) error {
				_, err := service.ReadResultChunk(t.Context(), job.ID, "U12345678", "slack:T12345678:dm:D12345678", 0, 4)
				return err
			},
			wantCount: 1,
		},
		{
			name: "chunk read artifact size mismatch",
			mode: domain.JobDetached,
			complete: func(t *testing.T, job *domain.ExternalAgentJob, db *sqlite.Store, jobStore port.ExternalAgentJobStore, artifacts port.ResultArtifactStore, stateDir string) domain.ExternalAgentInvocationResult {
				artifact, err := artifacts.(*fsartifact.Store).Put(t.Context(), job.ID+"-delivery", content)
				if err != nil {
					t.Fatal(err)
				}
				return domain.ExternalAgentInvocationResult{
					DeliveryMode:             domain.JobResultDeliveryFile,
					ArtifactRef:              artifact.Reference,
					ResultSHA256:             artifact.SHA256,
					ResultBytes:              artifact.Bytes,
					DeliveryArtifactRef:      artifact.Reference,
					DeliveryContentSHA256:    artifact.SHA256,
					DeliveryContentBytes:     artifact.Bytes,
					DeliveryPolicyVersion:    domain.JobDeliveryPolicyV1,
					DeliveryMaxMarkdownParts: 6,
				}
			},
			tamper: func(t *testing.T, job *domain.ExternalAgentJob, db *sqlite.Store, stateDir string) {
				ref := domain.CanonicalArtifactReference(job.ID)
				if err := os.WriteFile(filepath.Join(stateDir, "artifacts", ref), []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			read: func(service *Service, job *domain.ExternalAgentJob) error {
				_, err := service.ReadResultChunk(t.Context(), job.ID, "U12345678", "slack:T12345678:dm:D12345678", 0, 4)
				return err
			},
			wantCount: 1,
		},
		{
			name: "healthy full and chunk reads do not increment",
			mode: domain.JobForeground,
			complete: func(t *testing.T, job *domain.ExternalAgentJob, db *sqlite.Store, jobStore port.ExternalAgentJobStore, artifacts port.ResultArtifactStore, stateDir string) domain.ExternalAgentInvocationResult {
				return domain.ExternalAgentInvocationResult{Text: content, ResultSHA256: fmt.Sprintf("%x", digest), ResultBytes: int64(len(content))}
			},
			read: func(service *Service, job *domain.ExternalAgentJob) error {
				if _, err := service.ReadResult(t.Context(), job.ID, "U12345678", "slack:T12345678:dm:D12345678"); err != nil {
					return err
				}
				_, err := service.ReadResultChunk(t.Context(), job.ID, "U12345678", "slack:T12345678:dm:D12345678", 0, 4)
				return err
			},
			wantCount: 0,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			stateDir := t.TempDir()
			store, err := sqlite.Initialize(t.Context(), filepath.Join(stateDir, "jobs.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			metrics := &recordingNotificationMetrics{}
			jobStore := sqlite.NewExternalAgentJobStore(store)
			artifacts, err := fsartifact.New(filepath.Join(stateDir, "artifacts"), 4096)
			if err != nil {
				t.Fatal(err)
			}
			service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1}, Dependencies{
				Store: jobStore, Runtime: &fakeJobRuntime{}, Artifacts: artifacts, MaxResultBytes: 4096, Metrics: metrics,
			})
			if err != nil {
				t.Fatal(err)
			}
			request := testRequest(testCase.mode)
			created, err := service.Start(t.Context(), request)
			if err != nil || created == nil {
				t.Fatalf("start = %#v, err = %v", created, err)
			}
			result := testCase.complete(t, created, store, jobStore, artifacts, stateDir)
			completeJobWithResult(t, jobStore, created, result)
			if testCase.tamper != nil {
				testCase.tamper(t, created, store, stateDir)
			}
			readErr := testCase.read(service, created)
			if readErr != nil {
				if testCase.wantCount == 0 {
					t.Fatalf("healthy read failed: %v", readErr)
				}
				if !strings.Contains(readErr.Error(), string(domain.ResultErrorIdentityInvalid)) && !strings.Contains(readErr.Error(), string(domain.ResultErrorArtifactOwnerRefMismatch)) &&
					!strings.Contains(readErr.Error(), string(domain.ResultErrorArtifactBytesMismatch)) &&
					!strings.Contains(readErr.Error(), string(domain.ResultErrorArtifactDigestMismatch)) {
					t.Fatalf("read error = %q, want an identity-class code", readErr.Error())
				}
			}
			count := int64(0)
			for _, sample := range metrics.samples {
				if sample.Name == domain.MetricExternalAgentResultIdentityInvalidTotal && len(sample.Labels) == 0 {
					count += int64(sample.Value)
				}
			}
			if count != testCase.wantCount {
				t.Fatalf("identity failure counter = %d, want %d; samples = %#v", count, testCase.wantCount, metrics.samples)
			}
		})
	}
}

// TestReadResultChunkRangeErrorsAreRequestErrorsNotIdentityFailures pins
// CR11: client range errors (out-of-bounds or misaligned offsets, a max_bytes
// bound smaller than the next UTF-8 character) are bounded request errors that
// never increment the identity-failure counter in either delivery mode, while
// real durable corruption still does.
func TestReadResultChunkRangeErrorsAreRequestErrorsNotIdentityFailures(t *testing.T) {
	const content = "a🔥bc"
	digest := sha256.Sum256([]byte(content))
	identityCount := func(metrics *recordingNotificationMetrics) int64 {
		var count int64
		for _, sample := range metrics.samples {
			if sample.Name == domain.MetricExternalAgentResultIdentityInvalidTotal && len(sample.Labels) == 0 {
				count += int64(sample.Value)
			}
		}
		return count
	}
	run := func(t *testing.T, mode domain.ExternalAgentJobMode, read func(*Service, *domain.ExternalAgentJob) error) *recordingNotificationMetrics {
		t.Helper()
		stateDir := t.TempDir()
		store, err := sqlite.Initialize(t.Context(), filepath.Join(stateDir, "jobs.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		metrics := &recordingNotificationMetrics{}
		jobStore := sqlite.NewExternalAgentJobStore(store)
		artifacts, err := fsartifact.New(filepath.Join(stateDir, "artifacts"), 256*1024)
		if err != nil {
			t.Fatal(err)
		}
		service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1}, Dependencies{
			Store: jobStore, Runtime: &fakeJobRuntime{}, Artifacts: artifacts, MaxResultBytes: 256 * 1024, MaxResultChunkBytes: int64(len(content)), Metrics: metrics,
		})
		if err != nil {
			t.Fatal(err)
		}
		request := testRequest(mode)
		created, err := service.Start(t.Context(), request)
		if err != nil || created == nil {
			t.Fatalf("start = %#v, err = %v", created, err)
		}
		var result domain.ExternalAgentInvocationResult
		if mode == domain.JobDetached {
			artifact, err := artifacts.Put(t.Context(), created.ID+"-delivery", content)
			if err != nil {
				t.Fatal(err)
			}
			result = domain.ExternalAgentInvocationResult{DeliveryMode: domain.JobResultDeliveryFile, ArtifactRef: artifact.Reference, ResultSHA256: artifact.SHA256, ResultBytes: artifact.Bytes}
		} else {
			result = domain.ExternalAgentInvocationResult{Text: content, ResultSHA256: fmt.Sprintf("%x", digest), ResultBytes: int64(len(content)), DeliveryMode: domain.JobResultDeliveryMarkdown}
		}
		completeJobWithResult(t, jobStore, created, result)
		if err := read(service, created); err == nil {
			t.Fatal("expected a bounded request error")
		}
		return metrics
	}

	// Inline mode: misaligned offset, a bound smaller than the next rune, and
	// an out-of-bounds offset are all request errors that never count.
	metrics := run(t, domain.JobForeground, func(service *Service, job *domain.ExternalAgentJob) error {
		_, err := service.ReadResultChunk(t.Context(), job.ID, job.Actor, job.ConversationKey, 2, 4)
		assertRequestError := func(err error, step string) {
			if err == nil || !strings.Contains(err.Error(), string(domain.ResultErrorChunkRequestInvalid)) {
				t.Fatalf("%s error = %v, want %s", step, err, domain.ResultErrorChunkRequestInvalid)
			}
		}
		assertRequestError(err, "misaligned offset")
		_, err = service.ReadResultChunk(t.Context(), job.ID, job.Actor, job.ConversationKey, 1, 1)
		assertRequestError(err, "bound smaller than next rune")
		_, err = service.ReadResultChunk(t.Context(), job.ID, job.Actor, job.ConversationKey, int64(len(content))+1, 4)
		assertRequestError(err, "out-of-bounds offset")
		return err
	})
	if got := identityCount(metrics); got != 0 {
		t.Fatalf("inline request errors counted as identity failures: %d", got)
	}

	// File mode: the same three ranges are request errors that never count.
	metrics = run(t, domain.JobDetached, func(service *Service, job *domain.ExternalAgentJob) error {
		assertRequestError := func(err error, step string) {
			if err == nil || !strings.Contains(err.Error(), string(domain.ResultErrorChunkRequestInvalid)) {
				t.Fatalf("%s error = %v, want %s", step, err, domain.ResultErrorChunkRequestInvalid)
			}
		}
		_, err := service.ReadResultChunk(t.Context(), job.ID, job.Actor, job.ConversationKey, 2, 4)
		assertRequestError(err, "misaligned offset")
		_, err = service.ReadResultChunk(t.Context(), job.ID, job.Actor, job.ConversationKey, 1, 1)
		assertRequestError(err, "bound smaller than next rune")
		_, err = service.ReadResultChunk(t.Context(), job.ID, job.Actor, job.ConversationKey, int64(len(content))+1, 4)
		assertRequestError(err, "out-of-bounds offset")
		return err
	})
	if got := identityCount(metrics); got != 0 {
		t.Fatalf("file-mode request errors counted as identity failures: %d", got)
	}

	// Real durable corruption still increments the identity-failure counter.
	stateDir := t.TempDir()
	store, err := sqlite.Initialize(t.Context(), filepath.Join(stateDir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	corruptionMetrics := &recordingNotificationMetrics{}
	jobStore := sqlite.NewExternalAgentJobStore(store)
	artifacts, err := fsartifact.New(filepath.Join(stateDir, "artifacts"), 256*1024)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1}, Dependencies{
		Store: jobStore, Runtime: &fakeJobRuntime{}, Artifacts: artifacts, MaxResultBytes: 256 * 1024, MaxResultChunkBytes: int64(len(content)), Metrics: corruptionMetrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(domain.JobDetached)
	created, err := service.Start(t.Context(), request)
	if err != nil || created == nil {
		t.Fatalf("start = %#v, err = %v", created, err)
	}
	artifact, err := artifacts.Put(t.Context(), created.ID+"-delivery", content)
	if err != nil {
		t.Fatal(err)
	}
	completeJobWithResult(
		t,
		jobStore,
		created,
		domain.ExternalAgentInvocationResult{DeliveryMode: domain.JobResultDeliveryFile, ArtifactRef: artifact.Reference, ResultSHA256: artifact.SHA256, ResultBytes: artifact.Bytes},
	)
	if err := os.WriteFile(filepath.Join(stateDir, "artifacts", artifact.Reference), []byte("tampered!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReadResultChunk(
		t.Context(),
		created.ID,
		request.Actor,
		request.ConversationKey,
		0,
		4,
	); err == nil ||
		!strings.Contains(err.Error(), string(domain.ResultErrorArtifactBytesMismatch)) {
		t.Fatalf("tampered artifact error = %v", err)
	}
	if got := identityCount(corruptionMetrics); got != 1 {
		t.Fatalf("identity failure counter = %d, want 1 for real corruption", got)
	}
}

func TestStatusMismatchSignalsDoNotRevealComparedBindings(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger := &recordingNotificationLogger{}
	metrics := &recordingNotificationMetrics{}
	jobStore := sqlite.NewExternalAgentJobStore(store)
	service, err := New(Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1}, Dependencies{
		Store: jobStore, Runtime: &fakeJobRuntime{}, Logger: logger, Metrics: metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(domain.JobDetached)
	job, err := service.Start(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name              string
		actor             string
		conversation      domain.ConversationKey
		actorMatch        string
		conversationMatch string
	}{
		{name: "actor mismatch", actor: "U99999999", conversation: job.ConversationKey, actorMatch: "false", conversationMatch: "true"},
		{name: "conversation mismatch", actor: job.Actor, conversation: "slack:T12345678:dm:D99999999", actorMatch: "true", conversationMatch: "false"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := service.Status(t.Context(), job.ID, testCase.actor, testCase.conversation); err == nil || !strings.Contains(err.Error(), "not authorized") {
				t.Fatalf("Status error = %v", err)
			}
			found := false
			for _, sample := range metrics.samples {
				if sample.Name != domain.MetricExternalAgentStatusAuthorizationTotal {
					continue
				}
				if sample.Labels["actor_match"] == testCase.actorMatch && sample.Labels["conversation_match"] == testCase.conversationMatch {
					found = true
				}
			}
			if !found {
				t.Fatalf("authorization metrics = %#v", metrics.samples)
			}
		})
	}
	for _, log := range logger.errors {
		if strings.Contains(log, "U99999999") || strings.Contains(log, "D99999999") {
			t.Fatalf("authorization log revealed compared binding: %s", log)
		}
	}
}

func TestHostCompletionVerifiesPrivateArtifactDigest(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	content := "file result"
	digest := sha256.Sum256([]byte(content))
	request := testRequest(domain.JobDetached)
	service, err := New(
		Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1},
		Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}, Artifacts: fakeResultArtifacts{data: []byte(content)}, MaxResultBytes: 1024},
	)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), time.Now().UTC(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.ExternalAgentInvocationResult{
		DeliveryMode:             domain.JobResultDeliveryFile,
		ArtifactRef:              "job_" + job.ID[4:] + "-delivery.result",
		ResultSHA256:             fmt.Sprintf("%x", digest),
		ResultBytes:              int64(len(content)),
		DeliveryArtifactRef:      "job_" + job.ID[4:] + "-delivery.result",
		DeliveryContentSHA256:    fmt.Sprintf("%x", digest),
		DeliveryContentBytes:     int64(len(content)),
		DeliveryPolicyVersion:    domain.JobDeliveryPolicyV1,
		DeliveryMaxMarkdownParts: 6,
	}, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReadResult(t.Context(), job.ID, request.Actor, request.ConversationKey)
	if err != nil || result.Text != content || result.DeliveryMode != domain.JobResultDeliveryFile {
		t.Fatalf("artifact result = %#v, err = %v", result, err)
	}
}

func TestReadResultChunkPaginatesMarkdownAndBindsJobActor(t *testing.T) {
	store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1}, Dependencies{
		Store: jobStore, Runtime: &fakeJobRuntime{}, MaxResultChunkBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	content := "a🔥bc"
	digest := sha256.Sum256([]byte(content))
	request := testRequest(domain.JobDetached)
	job, err := service.Start(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), time.Now().UTC(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.ExternalAgentInvocationResult{
		Text: content, ResultSHA256: fmt.Sprintf("%x", digest), ResultBytes: int64(len(content)), DeliveryMode: domain.JobResultDeliveryMarkdown,
	}, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	first, err := service.ReadResultChunk(t.Context(), job.ID, request.Actor, request.ConversationKey, 0, 2)
	if err != nil || first.Content != "a" || first.NextOffsetBytes != 1 || first.EOF {
		t.Fatalf("first chunk = %#v, err = %v", first, err)
	}
	second, err := service.ReadResultChunk(t.Context(), job.ID, request.Actor, request.ConversationKey, first.NextOffsetBytes, 4)
	if err != nil || second.Content != "🔥" || second.NextOffsetBytes != 5 || second.EOF {
		t.Fatalf("second chunk = %#v, err = %v", second, err)
	}
	third, err := service.ReadResultChunk(t.Context(), job.ID, request.Actor, request.ConversationKey, second.NextOffsetBytes, 4)
	if err != nil || third.Content != "bc" || !third.EOF || third.SHA256 != fmt.Sprintf("%x", digest) {
		t.Fatalf("third chunk = %#v, err = %v", third, err)
	}
	if _, err := service.ReadResultChunk(t.Context(), job.ID, "U-other", request.ConversationKey, 0, 4); err == nil {
		t.Fatal("wrong actor read Markdown result chunk")
	}
	if _, err := service.ReadResultChunk(t.Context(), job.ID, request.Actor, "slack:T12345678:dm:D99999999", 0, 4); err == nil {
		t.Fatal("wrong conversation read Markdown result chunk")
	}
}

func TestReadResultChunkStreamsAndReverifiesFileModeArtifact(t *testing.T) {
	stateDir := t.TempDir()
	store, err := sqlite.Initialize(t.Context(), filepath.Join(stateDir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	artifacts, err := fsartifact.New(filepath.Join(stateDir, "artifacts"), 256*1024)
	if err != nil {
		t.Fatal(err)
	}
	jobStore := sqlite.NewExternalAgentJobStore(store)
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1}, Dependencies{
		Store: jobStore, Runtime: &fakeJobRuntime{}, Artifacts: artifacts, MaxResultBytes: 256 * 1024, MaxResultChunkBytes: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(domain.JobDetached)
	job, err := service.Start(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	content := "file🔥result"
	artifact, err := artifacts.Put(t.Context(), job.ID+"-delivery", content)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), time.Now().UTC(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.ExternalAgentInvocationResult{
		DeliveryMode: domain.JobResultDeliveryFile, ArtifactRef: artifact.Reference, ResultSHA256: artifact.SHA256, ResultBytes: artifact.Bytes,
	}, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	chunk, err := service.ReadResultChunk(t.Context(), job.ID, request.Actor, request.ConversationKey, 0, 5)
	if err != nil || chunk.Content != "file" || chunk.NextOffsetBytes != 4 || chunk.EOF {
		t.Fatalf("file chunk = %#v, err = %v", chunk, err)
	}
	if _, err := service.ReadResultChunk(t.Context(), job.ID, "U-other", request.ConversationKey, 0, 5); err == nil {
		t.Fatal("wrong actor read file-mode result chunk")
	}
	if err := os.WriteFile(filepath.Join(stateDir, "artifacts", artifact.Reference), []byte("file🔥tamper"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReadResultChunk(
		t.Context(),
		job.ID,
		request.Actor,
		request.ConversationKey,
		0,
		5,
	); err == nil ||
		!strings.Contains(err.Error(), string(domain.ResultErrorArtifactDigestMismatch)) {
		t.Fatalf("tampered file-mode artifact error = %v", err)
	}
}

func TestStartAndWaitReturnsVerifiedForegroundResult(t *testing.T) {
	store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	content := "foreground 🔥 result"
	digest := sha256.Sum256([]byte(content))
	jobStore := sqlite.NewExternalAgentJobStore(store)
	runtime := &fakeJobRuntime{result: domain.ExternalAgentInvocationResult{
		Text: content, Inline: true, ResultSHA256: fmt.Sprintf("%x", digest), ResultBytes: int64(len(content)),
	}}
	service, err := New(
		Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: jobStore, Runtime: runtime},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequestWithTimeout(domain.JobForeground, time.Minute)
	created, err := service.Start(t.Context(), request)
	if err != nil || created == nil {
		t.Fatalf("start = %#v, err = %v", created, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go service.Run(ctx)
	result, err := service.StartAndWait(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != content || !result.Inline || result.ResultSHA256 != fmt.Sprintf("%x", digest) || result.ResultBytes != int64(len(content)) {
		t.Fatalf("synchronous result = %#v", result)
	}
	read, err := service.ReadResult(t.Context(), created.ID, request.Actor, request.ConversationKey)
	if err != nil || read.Text != content || read.ContentSHA256 != fmt.Sprintf("%x", digest) || read.ContentBytes != int64(len(content)) {
		t.Fatalf("verified read = %#v, err = %v", read, err)
	}
	chunk, err := service.ReadResultChunk(t.Context(), created.ID, request.Actor, request.ConversationKey, 0, 0)
	if err != nil || chunk.Content != content || !chunk.EOF || chunk.SHA256 != fmt.Sprintf("%x", digest) {
		t.Fatalf("verified chunk = %#v, err = %v", chunk, err)
	}
}

func TestStartAndWaitReturnsNativeHandleWithoutForegroundPayload(t *testing.T) {
	catalog, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "native-foreground.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	payloads, err := fsartifact.NewTypedStore(filepath.Join(t.TempDir(), "native-results"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	nativeResults, err := sqlite.NewResultStore(catalog, payloads)
	if err != nil {
		t.Fatal(err)
	}
	jobStore := sqlite.NewExternalAgentJobStore(catalog)
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2}, Dependencies{
		Store: jobStore, Runtime: &fakeJobRuntime{}, NativeResults: nativeResults,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := testRequestWithTimeout(domain.JobForeground, time.Second)
	request.OriginalCallID = "native-foreground-call"
	job, err := service.Start(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), time.Now().UTC(), "native-worker", time.Minute)
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	content := "native foreground payload"
	handle, err := nativeResults.Materialize(t.Context(), port.ResultMaterialization{
		Producer: domain.ResultProducer{Kind: domain.ResultProducerExternalAgentJob, ID: job.ID, Revision: claimed.StatusRevision + 1},
		Payload:  content, Scope: domain.ResultScope{Actor: job.Actor, TeamID: job.TeamID, ConversationKey: string(job.ConversationKey), Project: job.PrimaryProject},
		Retention: domain.ResultRetentionContext, MediaType: "text/plain; charset=utf-8",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := &domain.ExternalAgentInvocationResult{
		Text: content, Inline: true, NativeResultID: handle.ResultID,
		ResultSHA256: handle.SHA256, ResultBytes: handle.Bytes,
	}
	if err := jobStore.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, result, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	foreground, err := service.StartAndWait(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if foreground.Text != "" || foreground.NativeResultID != handle.ResultID || foreground.NativeJobID != job.ID || foreground.NativeResultHandle.ResultID != handle.ResultID {
		t.Fatalf("native foreground result = %+v", foreground)
	}
}

func TestStartAndWaitRejectsIncompleteForegroundIdentity(t *testing.T) {
	content := "done"
	digest := sha256.Sum256([]byte(content))
	invalid := string([]byte{0xff, 0xfe})
	invalidDigest := sha256.Sum256([]byte(invalid))
	artifactContent := "x"
	artifactDigest := sha256.Sum256([]byte(artifactContent))
	for _, testCase := range []struct {
		name      string
		result    domain.ExternalAgentInvocationResult
		artifacts port.ResultArtifactStore
		wantCode  string
	}{
		{name: "empty SHA", result: domain.ExternalAgentInvocationResult{Text: content, Inline: true, ResultBytes: int64(len(content))}, wantCode: string(domain.ResultErrorIdentityInvalid)},
		{name: "wrong SHA", result: domain.ExternalAgentInvocationResult{Text: content, Inline: true, ResultBytes: int64(len(content)), ResultSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("other")))}, wantCode: string(domain.ResultErrorIdentityInvalid)},
		{name: "altered bytes", result: domain.ExternalAgentInvocationResult{Text: content, Inline: true, ResultBytes: int64(len(content)) + 1, ResultSHA256: fmt.Sprintf("%x", digest)}, wantCode: string(domain.ResultErrorIdentityInvalid)},
		{name: "invalid UTF-8", result: domain.ExternalAgentInvocationResult{Text: invalid, Inline: true, ResultBytes: int64(len(invalid)), ResultSHA256: fmt.Sprintf("%x", invalidDigest)}, wantCode: string(domain.ResultErrorIdentityInvalid)},
		{name: "absent artifact", result: domain.ExternalAgentInvocationResult{DeliveryMode: domain.JobResultDeliveryFile, ArtifactRef: "job-delivery.result", ResultSHA256: fmt.Sprintf("%x", artifactDigest), ResultBytes: int64(len(artifactContent))}, wantCode: string(domain.ResultErrorArtifactMissing)},
		{name: "unverifiable artifact", result: domain.ExternalAgentInvocationResult{DeliveryMode: domain.JobResultDeliveryFile, ArtifactRef: "job-delivery.result", ResultSHA256: fmt.Sprintf("%x", artifactDigest), ResultBytes: int64(len(artifactContent))}, artifacts: fakeResultArtifacts{}, wantCode: string(domain.ResultErrorArtifactInvalid)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "jobs.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			jobStore := sqlite.NewExternalAgentJobStore(store)
			service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2}, Dependencies{
				Store: jobStore, Runtime: &fakeJobRuntime{result: testCase.result}, Artifacts: testCase.artifacts,
			})
			if err != nil {
				t.Fatal(err)
			}
			request := testRequestWithTimeout(domain.JobForeground, 5*time.Second)
			job, err := service.Start(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			completeJobWithResult(t, jobStore, job, testCase.result)
			if _, err := service.StartAndWait(t.Context(), request); err == nil || !strings.Contains(err.Error(), testCase.wantCode) {
				t.Fatalf("error = %v, want code %s", err, testCase.wantCode)
			}
		})
	}
}

func TestStartAndWaitRejectsTamperedForegroundBinding(t *testing.T) {
	store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	service, err := New(
		Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2},
		Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	content := "bound result"
	digest := sha256.Sum256([]byte(content))
	validResult := domain.ExternalAgentInvocationResult{Text: content, Inline: true, ResultSHA256: fmt.Sprintf("%x", digest), ResultBytes: int64(len(content))}
	for _, testCase := range []struct {
		name  string
		query string
		value string
	}{
		{name: "actor tampered", query: `UPDATE external_agent_jobs SET actor = ? WHERE job_id = ?`, value: "U-tampered"},
		{name: "conversation tampered", query: `UPDATE external_agent_jobs SET conversation_key = ? WHERE job_id = ?`, value: "slack:T12345678:dm:D99999999"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := testRequestWithTimeout(domain.JobForeground, time.Second)
			request.OriginalCallID = "tampered-" + testCase.name
			created, err := service.Start(t.Context(), request)
			if err != nil || created == nil {
				t.Fatalf("start = %#v, err = %v", created, err)
			}
			completeJobWithResult(t, jobStore, created, validResult)
			if _, err := store.DB().ExecContext(t.Context(), testCase.query, testCase.value, created.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := service.StartAndWait(t.Context(), request); err == nil || !strings.Contains(err.Error(), "not authorized") {
				t.Fatalf("tampered binding error = %v", err)
			}
		})
	}
	request := testRequestWithTimeout(domain.JobForeground, time.Second)
	request.OriginalCallID = "tampered-control"
	created, err := service.Start(t.Context(), request)
	if err != nil || created == nil {
		t.Fatalf("start = %#v, err = %v", created, err)
	}
	completeJobWithResult(t, jobStore, created, validResult)
	result, err := service.StartAndWait(t.Context(), request)
	if err != nil || result.Text != content {
		t.Fatalf("untampered control = %#v, err = %v", result, err)
	}
}

func TestReadResultRejectsIncompleteIdentity(t *testing.T) {
	content := "sanitized result"
	digest := sha256.Sum256([]byte(content))
	invalid := string([]byte{0xff, 0xfe})
	invalidDigest := sha256.Sum256([]byte(invalid))
	for _, testCase := range []struct {
		name     string
		result   domain.ExternalAgentInvocationResult
		wantCode string
	}{
		{name: "empty SHA", result: domain.ExternalAgentInvocationResult{Text: content, ResultBytes: int64(len(content))}, wantCode: string(domain.ResultErrorIdentityInvalid)},
		{name: "wrong SHA", result: domain.ExternalAgentInvocationResult{Text: content, ResultBytes: int64(len(content)), ResultSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("other")))}, wantCode: string(domain.ResultErrorIdentityInvalid)},
		{name: "altered bytes", result: domain.ExternalAgentInvocationResult{Text: content, ResultBytes: int64(len(content)) + 1, ResultSHA256: fmt.Sprintf("%x", digest)}, wantCode: string(domain.ResultErrorIdentityInvalid)},
		{name: "invalid UTF-8", result: domain.ExternalAgentInvocationResult{Text: invalid, ResultBytes: int64(len(invalid)), ResultSHA256: fmt.Sprintf("%x", invalidDigest)}, wantCode: string(domain.ResultErrorIdentityInvalid)},
		{name: "absent artifact", result: domain.ExternalAgentInvocationResult{DeliveryMode: domain.JobResultDeliveryFile, ArtifactRef: "job-delivery.result", ResultSHA256: fmt.Sprintf("%x", digest), ResultBytes: int64(len(content))}, wantCode: string(domain.ResultErrorArtifactMissing)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "jobs.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			jobStore := sqlite.NewExternalAgentJobStore(store)
			service, err := New(
				Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2},
				Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}},
			)
			if err != nil {
				t.Fatal(err)
			}
			request := testRequest(domain.JobForeground)
			created, err := service.Start(t.Context(), request)
			if err != nil || created == nil {
				t.Fatalf("start = %#v, err = %v", created, err)
			}
			completeJobWithResult(t, jobStore, created, testCase.result)
			if _, err := service.ReadResult(t.Context(), created.ID, request.Actor, request.ConversationKey); err == nil || !strings.Contains(err.Error(), testCase.wantCode) {
				t.Fatalf("error = %v, want code %s", err, testCase.wantCode)
			}
		})
	}
}

func TestVerifiedResultFailuresAreBoundedAndRedacted(t *testing.T) {
	const (
		redactedContent      = "REDACTED-TOP-SECRET-RESULT-7f3c9a"
		redactedRef          = "job_redacted7f3c-delivery.result"
		redactedPath         = "/var/run/local-agent-redacted-7f3c/artifacts"
		redactedActor        = "U-redacted-actor-7f3c"
		redactedConversation = "slack:T-redacted-team-7f3c:dm:D-redacted-channel-7f3c"
	)
	redactedDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(redactedContent)))
	foreignDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("other-redacted-content")))
	request := testRequestWithTimeout(domain.JobForeground, time.Minute)
	request.Actor = redactedActor
	request.ConversationKey = domain.ConversationKey(redactedConversation)
	redactionForbidden := []string{redactedContent, redactedRef, redactedPath, redactedActor, redactedConversation, redactedDigest, foreignDigest}

	readResult := func(service *Service, jobID string) error {
		_, err := service.ReadResult(t.Context(), jobID, request.Actor, request.ConversationKey)
		return err
	}
	readChunk := func(service *Service, jobID string) error {
		_, err := service.ReadResultChunk(t.Context(), jobID, request.Actor, request.ConversationKey, 0, 16)
		return err
	}

	tests := []struct {
		name     string
		newStore func(t *testing.T) port.ResultArtifactStore
		complete func(t *testing.T, jobID string, store port.ResultArtifactStore) (domain.ExternalAgentInvocationResult, func(t *testing.T))
		read     func(service *Service, jobID string) error
		wantCode string
	}{
		{
			name: "inline digest mismatch",
			complete: func(t *testing.T, jobID string, store port.ResultArtifactStore) (domain.ExternalAgentInvocationResult, func(t *testing.T)) {
				return domain.ExternalAgentInvocationResult{Text: redactedContent, ResultSHA256: foreignDigest, ResultBytes: int64(len(redactedContent))}, nil
			},
			read:     readResult,
			wantCode: string(domain.ResultErrorIdentityInvalid),
		},
		{
			name: "inline byte count mismatch",
			complete: func(t *testing.T, jobID string, store port.ResultArtifactStore) (domain.ExternalAgentInvocationResult, func(t *testing.T)) {
				return domain.ExternalAgentInvocationResult{Text: redactedContent, ResultSHA256: redactedDigest, ResultBytes: int64(len(redactedContent)) + 1}, nil
			},
			read:     readResult,
			wantCode: string(domain.ResultErrorIdentityInvalid),
		},
		{
			name: "inline digest mismatch through chunk reader",
			complete: func(t *testing.T, jobID string, store port.ResultArtifactStore) (domain.ExternalAgentInvocationResult, func(t *testing.T)) {
				return domain.ExternalAgentInvocationResult{Text: redactedContent, ResultSHA256: foreignDigest, ResultBytes: int64(len(redactedContent))}, nil
			},
			read:     readChunk,
			wantCode: string(domain.ResultErrorIdentityInvalid),
		},
		{
			name: "file artifact missing",
			newStore: func(t *testing.T) port.ResultArtifactStore {
				store, err := fsartifact.New(t.TempDir(), 4096)
				if err != nil {
					t.Fatal(err)
				}
				return store
			},
			complete: func(t *testing.T, jobID string, store port.ResultArtifactStore) (domain.ExternalAgentInvocationResult, func(t *testing.T)) {
				return domain.ExternalAgentInvocationResult{
					DeliveryMode: domain.JobResultDeliveryFile,
					ArtifactRef:  jobID + "-delivery.result",
					ResultSHA256: redactedDigest,
					ResultBytes:  int64(len(redactedContent)),
				}, nil
			},
			read:     readResult,
			wantCode: string(domain.ResultErrorArtifactMissing),
		},
		{
			name: "file owner and reference mismatch",
			newStore: func(t *testing.T) port.ResultArtifactStore {
				store, err := fsartifact.New(t.TempDir(), 4096)
				if err != nil {
					t.Fatal(err)
				}
				return store
			},
			complete: func(t *testing.T, jobID string, store port.ResultArtifactStore) (domain.ExternalAgentInvocationResult, func(t *testing.T)) {
				artifact, err := store.(*fsartifact.Store).Put(t.Context(), jobID+"-delivery", redactedContent)
				if err != nil {
					t.Fatal(err)
				}
				return domain.ExternalAgentInvocationResult{DeliveryMode: domain.JobResultDeliveryFile, ArtifactRef: redactedRef, ResultSHA256: artifact.SHA256, ResultBytes: artifact.Bytes}, nil
			},
			read:     readResult,
			wantCode: string(domain.ResultErrorArtifactOwnerRefMismatch),
		},
		{
			name: "file byte count mismatch",
			newStore: func(t *testing.T) port.ResultArtifactStore {
				store, err := fsartifact.New(t.TempDir(), 4096)
				if err != nil {
					t.Fatal(err)
				}
				return store
			},
			complete: func(t *testing.T, jobID string, store port.ResultArtifactStore) (domain.ExternalAgentInvocationResult, func(t *testing.T)) {
				artifact, err := store.(*fsartifact.Store).Put(t.Context(), jobID+"-delivery", redactedContent)
				if err != nil {
					t.Fatal(err)
				}
				return domain.ExternalAgentInvocationResult{
					DeliveryMode: domain.JobResultDeliveryFile,
					ArtifactRef:  artifact.Reference,
					ResultSHA256: artifact.SHA256,
					ResultBytes:  artifact.Bytes + 1,
				}, nil
			},
			read:     readResult,
			wantCode: string(domain.ResultErrorArtifactBytesMismatch),
		},
		{
			name: "file digest mismatch",
			newStore: func(t *testing.T) port.ResultArtifactStore {
				store, err := fsartifact.New(t.TempDir(), 4096)
				if err != nil {
					t.Fatal(err)
				}
				return store
			},
			complete: func(t *testing.T, jobID string, store port.ResultArtifactStore) (domain.ExternalAgentInvocationResult, func(t *testing.T)) {
				artifact, err := store.(*fsartifact.Store).Put(t.Context(), jobID+"-delivery", redactedContent)
				if err != nil {
					t.Fatal(err)
				}
				return domain.ExternalAgentInvocationResult{DeliveryMode: domain.JobResultDeliveryFile, ArtifactRef: artifact.Reference, ResultSHA256: foreignDigest, ResultBytes: artifact.Bytes}, nil
			},
			read:     readResult,
			wantCode: string(domain.ResultErrorArtifactDigestMismatch),
		},
		{
			name: "file digest mismatch through chunk reader",
			newStore: func(t *testing.T) port.ResultArtifactStore {
				store, err := fsartifact.New(t.TempDir(), 4096)
				if err != nil {
					t.Fatal(err)
				}
				return store
			},
			complete: func(t *testing.T, jobID string, store port.ResultArtifactStore) (domain.ExternalAgentInvocationResult, func(t *testing.T)) {
				artifact, err := store.(*fsartifact.Store).Put(t.Context(), jobID+"-delivery", redactedContent)
				if err != nil {
					t.Fatal(err)
				}
				return domain.ExternalAgentInvocationResult{DeliveryMode: domain.JobResultDeliveryFile, ArtifactRef: artifact.Reference, ResultSHA256: foreignDigest, ResultBytes: artifact.Bytes}, nil
			},
			read:     readChunk,
			wantCode: string(domain.ResultErrorArtifactDigestMismatch),
		},
		{
			name: "unmapped adapter error fails closed",
			newStore: func(t *testing.T) port.ResultArtifactStore {
				return leakyResultArtifacts{content: redactedContent}
			},
			complete: func(t *testing.T, jobID string, store port.ResultArtifactStore) (domain.ExternalAgentInvocationResult, func(t *testing.T)) {
				return domain.ExternalAgentInvocationResult{
					DeliveryMode: domain.JobResultDeliveryFile,
					ArtifactRef:  jobID + "-delivery.result",
					ResultSHA256: redactedDigest,
					ResultBytes:  int64(len(redactedContent)),
				}, nil
			},
			read:     readResult,
			wantCode: string(domain.ResultErrorArtifactInvalid),
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			db, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "jobs.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			jobStore := sqlite.NewExternalAgentJobStore(db)
			var artifacts port.ResultArtifactStore
			if testCase.newStore != nil {
				artifacts = testCase.newStore(t)
			}
			service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1}, Dependencies{
				Store: jobStore, Runtime: &fakeJobRuntime{}, Artifacts: artifacts, MaxResultBytes: 4096,
			})
			if err != nil {
				t.Fatal(err)
			}
			created, err := service.Start(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			result, tamper := testCase.complete(t, created.ID, artifacts)
			completeJobWithResult(t, jobStore, created, result)
			if tamper != nil {
				tamper(t)
			}
			readErr := testCase.read(service, created.ID)
			if readErr == nil {
				t.Fatal("verified read accepted a broken identity")
			}
			if readErr.Error() != testCase.wantCode {
				t.Fatalf("error = %q, want exact code %q", readErr.Error(), testCase.wantCode)
			}
			forbidden := append(append([]string(nil), redactionForbidden...), created.ID+"-delivery.result", created.ID)
			for _, value := range forbidden {
				if strings.Contains(readErr.Error(), value) {
					t.Fatalf("error %q leaked %q", readErr.Error(), value)
				}
			}
		})
	}
}

// leakyResultArtifacts simulates a misbehaving adapter whose unmapped error
// embeds synthetic sensitive values; the service must fail closed to the
// generic bounded code and never surface them.
type leakyResultArtifacts struct{ content string }

func (f leakyResultArtifacts) Put(context.Context, string, string) (domain.ResultArtifact, error) {
	return domain.ResultArtifact{}, errors.New("not used")
}

func (f leakyResultArtifacts) Get(context.Context, string, string, string, int64) ([]byte, error) {
	return nil, errors.New(
		"artifact read exploded: digest " + fmt.Sprintf(
			"%x",
			sha256.Sum256([]byte(f.content)),
		) + " ref job_redacted7f3c-delivery.result path /var/run/local-agent-redacted-7f3c/artifacts owner U-redacted-actor-7f3c conversation slack:T-redacted-team-7f3c:dm:D-redacted-channel-7f3c content " + f.content,
	)
}

func TestStartAndWaitKeepsTerminalStatusErrors(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		transitions []struct {
			status    domain.ExternalAgentJobStatus
			errorCode string
		}
		want string
	}{
		{name: "failed", transitions: []struct {
			status    domain.ExternalAgentJobStatus
			errorCode string
		}{{status: domain.JobFailed, errorCode: "acp_job_timeout"}}, want: "external-agent job ended with status failed: acp_job_timeout"},
		{name: "cancelled", transitions: []struct {
			status    domain.ExternalAgentJobStatus
			errorCode string
		}{{status: domain.JobCancelRequested}, {status: domain.JobCancelled}}, want: "external-agent job ended with status cancelled"},
		{name: "abandoned", transitions: []struct {
			status    domain.ExternalAgentJobStatus
			errorCode string
		}{{status: domain.JobCompletionUnknown, errorCode: "completion_unknown"}, {status: domain.JobAbandoned}}, want: "external-agent job ended with status abandoned"},
		{name: "completion_unknown", transitions: []struct {
			status    domain.ExternalAgentJobStatus
			errorCode string
		}{{status: domain.JobCompletionUnknown, errorCode: "completion_unknown"}}, want: "external-agent job ended with status completion_unknown: completion_unknown"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "jobs.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			jobStore := sqlite.NewExternalAgentJobStore(store)
			service, err := New(
				Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2},
				Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}},
			)
			if err != nil {
				t.Fatal(err)
			}
			request := testRequestWithTimeout(domain.JobForeground, time.Second)
			created, err := service.Start(t.Context(), request)
			if err != nil || created == nil {
				t.Fatalf("start = %#v, err = %v", created, err)
			}
			claimed, err := jobStore.ClaimNext(t.Context(), time.Now().UTC(), "worker-test", time.Minute)
			if err != nil || claimed == nil {
				t.Fatalf("claim = %#v, err = %v", claimed, err)
			}
			now := time.Now().UTC()
			for _, transition := range testCase.transitions {
				if err := jobStore.Transition(t.Context(), created.ID, claimed.LeaseOwner, claimed.Attempt, transition.status, nil, transition.errorCode, now); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := service.StartAndWait(t.Context(), request); err == nil || err.Error() != testCase.want {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
		})
	}
}

type fakeResultArtifacts struct{ data []byte }

func (f fakeResultArtifacts) Put(context.Context, string, string) (domain.ResultArtifact, error) {
	return domain.ResultArtifact{}, errors.New("not used")
}

func (f fakeResultArtifacts) Get(_ context.Context, _ string, _ string, expected string, max int64) ([]byte, error) {
	if int64(len(f.data)) > max {
		return nil, errors.New("overflow")
	}
	digest := sha256.Sum256(f.data)
	if expected != fmt.Sprintf("%x", digest) {
		return nil, errors.New("digest mismatch")
	}
	return append([]byte(nil), f.data...), nil
}

type fakeJobRuntime struct {
	mu     sync.Mutex
	calls  int
	result domain.ExternalAgentInvocationResult
	block  chan struct{}
	errs   []error
}

type fakeRecoveryRuntime struct {
	delay  time.Duration
	wait   <-chan struct{}
	result domain.ExternalAgentInvocationResult
}

type recordingJobStore struct {
	*sqlite.ExternalAgentJobStore
	renewed      chan struct{}
	once         sync.Once
	mu           sync.Mutex
	lastRenewErr error
}

type blockingCancellationStore struct {
	*sqlite.ExternalAgentJobStore
	started    chan struct{}
	release    chan struct{}
	finished   chan struct{}
	polled     chan struct{}
	pollSignal sync.Once
}

func (s *blockingCancellationStore) RequestCancellation(ctx context.Context, jobID, actor string) (*domain.ExternalAgentJob, error) {
	close(s.started)
	<-s.release
	result, err := s.ExternalAgentJobStore.RequestCancellation(ctx, jobID, actor)
	close(s.finished)
	return result, err
}

// GetJob signals polled the first time StartAndWait's wait loop observes the
// job it just created. A test can wait on that signal instead of a real-time
// sleep before advancing a fake clock past the job's deadline.
func (s *blockingCancellationStore) GetJob(ctx context.Context, jobID string) (*domain.ExternalAgentJob, error) {
	job, err := s.ExternalAgentJobStore.GetJob(ctx, jobID)
	if s.polled != nil {
		s.pollSignal.Do(func() { close(s.polled) })
	}
	return job, err
}

func (s *recordingJobStore) RenewLease(ctx context.Context, jobID, owner string, attempt int, now time.Time, ttl time.Duration) error {
	if err := s.ExternalAgentJobStore.RenewLease(ctx, jobID, owner, attempt, now, ttl); err != nil {
		s.mu.Lock()
		s.lastRenewErr = err
		s.mu.Unlock()
		return err
	}
	s.once.Do(func() { close(s.renewed) })
	return nil
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func (r *fakeRecoveryRuntime) Run(context.Context, domain.ExternalAgentJob) (domain.ExternalAgentInvocationResult, error) {
	return domain.ExternalAgentInvocationResult{}, errors.New("not used")
}

func (r *fakeRecoveryRuntime) Reconcile(ctx context.Context, _ domain.ExternalAgentJob) (domain.ExternalAgentInvocationResult, error) {
	if r.wait != nil {
		select {
		case <-ctx.Done():
			return domain.ExternalAgentInvocationResult{}, ctx.Err()
		case <-r.wait:
			return r.result, nil
		}
	}
	timer := time.NewTimer(r.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return domain.ExternalAgentInvocationResult{}, ctx.Err()
	case <-timer.C:
		return r.result, nil
	}
}

type fakeJobPublisher struct {
	calls int
}

func (p *fakeJobPublisher) PublishJobTerminal(context.Context, domain.ExternalAgentJob) error {
	p.calls++
	return nil
}

func (r *fakeJobRuntime) Run(ctx context.Context, _ domain.ExternalAgentJob) (domain.ExternalAgentInvocationResult, error) {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return domain.ExternalAgentInvocationResult{}, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return domain.ExternalAgentInvocationResult{}, err
		}
	}
	if call <= len(r.errs) {
		return domain.ExternalAgentInvocationResult{}, r.errs[call-1]
	}
	return r.result, nil
}

func completeJobWithResult(t *testing.T, store port.ExternalAgentJobStore, job *domain.ExternalAgentJob, result domain.ExternalAgentInvocationResult) {
	t.Helper()
	claimed, err := store.ClaimNext(t.Context(), time.Now().UTC(), "worker-test", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, err = %v", claimed, err)
	}
	if err := store.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &result, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func testRequest(mode domain.ExternalAgentJobMode) domain.ExternalAgentJobRequest {
	return testRequestWithTimeout(mode, time.Minute)
}

func createCompletionUnknownJob(t *testing.T, service *Service, store port.ExternalAgentJobStore) *domain.ExternalAgentJob {
	t.Helper()
	job, err := service.Start(t.Context(), testRequest(domain.JobDetached))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimed, err := store.ClaimNext(t.Context(), now, "worker-test", time.Second)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, err=%v", claimed, err)
	}
	if err := store.AssignExternalAgentSession(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, "session-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompletionUnknown, nil, "completion_unknown", now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	unknown, err := store.GetJob(t.Context(), job.ID)
	if err != nil || unknown == nil {
		t.Fatalf("unknown job = %#v, err=%v", unknown, err)
	}
	return unknown
}

func testRequestWithTimeout(mode domain.ExternalAgentJobMode, timeout time.Duration) domain.ExternalAgentJobRequest {
	return domain.ExternalAgentJobRequest{
		Provider:         "agentcli",
		Profile:          "build",
		PrimaryProject:   "workspace",
		RegistryRevision: "r1",
		Task:             "task",
		Mode:             mode,
		WrapperCallID:    "wrapper-1",
		OriginalCallID:   "original-1",
		Actor:            "U12345678",
		TeamID:           "T12345678",
		ConversationKey:  "slack:T12345678:dm:D12345678",
		Timeout:          timeout,
	}
}

func waitForJob(t *testing.T, store port.ExternalAgentJobStore, id string, status domain.ExternalAgentJobStatus) *domain.ExternalAgentJob {
	t.Helper()
	// Generous on purpose: a loaded machine must not fail a test that only
	// waits for a status. A job that never arrives still fails, just later.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		job, err := store.GetJob(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job != nil && job.Status == status {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, _ := store.GetJob(t.Context(), id)
	t.Fatalf("job %s did not reach %s, current = %#v", id, status, job)
	return nil
}
