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

func TestStopAdmissionDrainsRunningJobWithoutClaimingQueuedWork(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "shutdown.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	runtime := &fakeJobRuntime{result: domain.AcpInvocationResult{Text: "drained", ResultBytes: 7}, block: make(chan struct{})}
	service, err := New(Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1}, Dependencies{Store: jobStore, Runtime: runtime})
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
	runtime := &fakeRecoveryRuntime{wait: renewed, result: domain.AcpInvocationResult{Text: "done", ResultBytes: 4}}
	clock := fixedClock{now: time.Now().UTC()}
	service, err := New(Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: 30 * time.Millisecond, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 2}, Dependencies{Store: jobStore, Runtime: runtime, Clock: clock})
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
	service, err := New(Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: 20 * time.Millisecond, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 2}, Dependencies{Store: jobStore, Runtime: &fakeRecoveryRuntime{}})
	if err != nil {
		t.Fatal(err)
	}
	job := createCompletionUnknownJob(t, service, jobStore)
	reconciling, err := jobStore.BeginReconciliationExpected(t.Context(), job.ID, job.Actor, job.ConversationKey, job.StatusRevision, time.Now().UTC(), "reconciler-test", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	service.recoverExpired(t.Context())
	recovered, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || recovered == nil || recovered.Status != domain.JobCompletionUnknown || recovered.StatusRevision != reconciling.StatusRevision+1 {
		t.Fatalf("recovered job = %#v, err=%v", recovered, err)
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
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1}, Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}})
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
	if err := jobStore.Transition(t.Context(), created.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.AcpInvocationResult{
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
	if _, err := service.ReadResult(t.Context(), created.ID, job.Actor, job.ConversationKey); err == nil || !strings.Contains(err.Error(), "result_artifact_invalid") {
		t.Fatalf("altered ResultBytes was accepted: %v", err)
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
	service, err := New(Config{DefaultTimeout: time.Second, MaxTimeout: time.Minute, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1}, Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}, Artifacts: fakeResultArtifacts{data: []byte(content)}, MaxResultBytes: 1024})
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
	if err := jobStore.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.AcpInvocationResult{
		DeliveryMode: domain.JobResultDeliveryFile, ArtifactRef: "job_" + job.ID[4:] + "-delivery.result", ResultSHA256: fmt.Sprintf("%x", digest), ResultBytes: int64(len(content)),
		DeliveryArtifactRef: "job_" + job.ID[4:] + "-delivery.result", DeliveryContentSHA256: fmt.Sprintf("%x", digest), DeliveryContentBytes: int64(len(content)), DeliveryPolicyVersion: domain.JobDeliveryPolicyV1, DeliveryMaxMarkdownParts: 6,
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
	if err := jobStore.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.AcpInvocationResult{
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
	if err := jobStore.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.AcpInvocationResult{
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
	if _, err := service.ReadResultChunk(t.Context(), job.ID, request.Actor, request.ConversationKey, 0, 5); err == nil || !strings.Contains(err.Error(), "result_artifact_invalid") {
		t.Fatalf("tampered file-mode artifact error = %v", err)
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
	result domain.AcpInvocationResult
	block  chan struct{}
	errs   []error
}

type fakeRecoveryRuntime struct {
	delay  time.Duration
	wait   <-chan struct{}
	result domain.AcpInvocationResult
}

type recordingJobStore struct {
	*sqlite.ExternalAgentJobStore
	renewed      chan struct{}
	once         sync.Once
	mu           sync.Mutex
	lastRenewErr error
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

func (r *fakeRecoveryRuntime) Run(context.Context, domain.ExternalAgentJob) (domain.AcpInvocationResult, error) {
	return domain.AcpInvocationResult{}, errors.New("not used")
}

func (r *fakeRecoveryRuntime) Reconcile(ctx context.Context, _ domain.ExternalAgentJob) (domain.AcpInvocationResult, error) {
	if r.wait != nil {
		select {
		case <-ctx.Done():
			return domain.AcpInvocationResult{}, ctx.Err()
		case <-r.wait:
			return r.result, nil
		}
	}
	timer := time.NewTimer(r.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return domain.AcpInvocationResult{}, ctx.Err()
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
	if err := store.AssignACPSession(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, "session-1"); err != nil {
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
