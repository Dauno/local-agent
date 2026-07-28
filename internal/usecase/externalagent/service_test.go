package externalagent

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	runtime := &fakeJobRuntime{result: domain.AcpInvocationResult{Text: "done", Inline: true, ResultBytes: 4}}
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: 2 * time.Second, PollInterval: 10 * time.Millisecond, Concurrency: 1, MaxAttempts: 2}, Dependencies{Store: sqlite.NewExternalAgentJobStore(store), Runtime: runtime})
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

func TestRunningJobCancellationIsIdempotentBeforeSideEffects(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runtime := &fakeJobRuntime{block: make(chan struct{})}
	jobStore := sqlite.NewExternalAgentJobStore(store)
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 10 * time.Millisecond, Concurrency: 1, MaxAttempts: 2}, Dependencies{Store: jobStore, Runtime: runtime})
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

func TestJobTotalTimeoutIsTerminalAndNeverRequeued(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	runtime := &fakeJobRuntime{block: make(chan struct{})}
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2}, Dependencies{Store: jobStore, Runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), testRequestWithTimeout(domain.JobDetached, 30*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go service.Run(ctx)
	finished := waitForJob(t, jobStore, job.ID, domain.JobFailed)
	if finished.ErrorCode != "acp_job_timeout" {
		t.Fatalf("finished = %#v", finished)
	}
}

func TestForegroundWaitReturnsWhenQueuedJobTotalTimeoutExpires(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2}, Dependencies{Store: sqlite.NewExternalAgentJobStore(store), Runtime: &fakeJobRuntime{}})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = service.StartAndWait(t.Context(), testRequestWithTimeout(domain.JobForeground, 20*time.Millisecond))
	if err == nil || time.Since(started) > time.Second || !strings.Contains(err.Error(), "acp_job_timeout") {
		t.Fatalf("error = %v, elapsed = %s", err, time.Since(started))
	}
}

func TestStartRejectsUnboundConversation(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2}, Dependencies{Store: sqlite.NewExternalAgentJobStore(store), Runtime: &fakeJobRuntime{}})
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
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2}, Dependencies{Store: jobStore, Runtime: runtime, Publisher: publisher})
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

func TestTransientACPProcessExitRetriesBeforeSession(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	runtime := &fakeJobRuntime{
		result: domain.AcpInvocationResult{Text: "done", Inline: true, ResultBytes: 4},
		errs:   []error{&domain.ACPError{Code: domain.ACPErrorProcessExit, Err: context.Canceled}},
	}
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2}, Dependencies{Store: jobStore, Runtime: runtime})
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

func TestNonRetryableACPErrorPreservesCode(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	runtime := &fakeJobRuntime{errs: []error{&domain.ACPError{Code: domain.ACPErrorConfigDrift, Err: context.Canceled}}}
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: 5 * time.Millisecond, Concurrency: 1, MaxAttempts: 2}, Dependencies{Store: jobStore, Runtime: runtime})
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
	finished := waitForJob(t, jobStore, job.ID, domain.JobFailed)
	if runtime.calls != 1 || finished.ErrorCode != string(domain.ACPErrorConfigDrift) {
		t.Fatalf("calls = %d, error code = %q", runtime.calls, finished.ErrorCode)
	}
}

type fakeJobRuntime struct {
	mu     sync.Mutex
	calls  int
	result domain.AcpInvocationResult
	block  chan struct{}
	errs   []error
}

type fakeJobPublisher struct {
	calls int
}

func (p *fakeJobPublisher) PublishJobTerminal(context.Context, domain.ExternalAgentJob) error {
	p.calls++
	return nil
}

func (r *fakeJobRuntime) Run(ctx context.Context, _ domain.ExternalAgentJob) (domain.AcpInvocationResult, error) {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return domain.AcpInvocationResult{}, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return domain.AcpInvocationResult{}, err
		}
	}
	if call <= len(r.errs) {
		return domain.AcpInvocationResult{}, r.errs[call-1]
	}
	return r.result, nil
}

func testRequest(mode domain.ExternalAgentJobMode) domain.ExternalAgentJobRequest {
	return testRequestWithTimeout(mode, time.Minute)
}

func testRequestWithTimeout(mode domain.ExternalAgentJobMode, timeout time.Duration) domain.ExternalAgentJobRequest {
	return domain.ExternalAgentJobRequest{Provider: "opencode", Profile: "build", PrimaryProject: "workspace", RegistryRevision: "r1", Task: "task", Mode: mode, WrapperCallID: "wrapper-1", OriginalCallID: "original-1", Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678", Timeout: timeout}
}

func waitForJob(t *testing.T, store port.ExternalAgentJobStore, id string, status domain.ExternalAgentJobStatus) *domain.ExternalAgentJob {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
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
