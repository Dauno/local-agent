package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestExternalAgentJobStoreClaimsAndTerminalizesOneAttempt(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := NewExternalAgentJobStore(store)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	job := testExternalAgentJob(now)
	created, existing, err := jobStore.CreateIfAbsent(t.Context(), job)
	if err != nil || !created || existing != nil {
		t.Fatalf("create = %v, existing = %#v, err = %v", created, existing, err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), now, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.Status != domain.JobRunning || claimed.Attempt != 1 {
		t.Fatalf("claimed = %#v", claimed)
	}
	if err := jobStore.AssignExternalAgentSession(t.Context(), job.ID, "worker-1", 1, "session-1"); err != nil {
		t.Fatal(err)
	}
	if err := jobStore.MarkSideEffectsPossible(t.Context(), job.ID, "worker-1", 1); err != nil {
		t.Fatal(err)
	}
	result := &domain.ExternalAgentInvocationResult{Text: "done", Inline: true, ResultBytes: 4}
	if err := jobStore.Transition(t.Context(), job.ID, "worker-1", 1, domain.JobCompleted, result, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	finished, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || finished == nil {
		t.Fatalf("finished = %#v, err = %v", finished, err)
	}
	if finished.Status != domain.JobCompleted || !finished.SideEffectsPossible || finished.ExternalAgentSessionID != "session-1" || finished.ResultSummary != "done" {
		t.Fatalf("finished = %#v", finished)
	}
	if err := jobStore.Transition(t.Context(), job.ID, "worker-1", 1, domain.JobFailed, nil, "late", now.Add(2*time.Second)); err == nil {
		t.Fatal("stale attempt terminalized a completed job")
	}
}

func TestExternalAgentJobStoreAtomicallyBindsNativeCompletedResult(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "native-job.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create = %v, err = %v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "native-worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, err = %v", claimed, err)
	}
	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	const content = "native completed result"
	handle, err := results.Materialize(t.Context(), port.ResultMaterialization{
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
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, result, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var referencedResult, referenceState string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT result_id, state FROM result_references
		WHERE owner_kind = ? AND owner_id = ?`, externalAgentResultOwnerKind, job.ID+":2").Scan(&referencedResult, &referenceState); err != nil {
		t.Fatal(err)
	}
	if referencedResult != handle.ResultID || referenceState != "live" {
		t.Fatalf("native reference = %q/%q", referencedResult, referenceState)
	}
	resolvedResult, err := jobs.NativeResultIDForJob(t.Context(), job.ID, 2)
	if err != nil || resolvedResult != handle.ResultID {
		t.Fatalf("native job binding = %q, %v", resolvedResult, err)
	}
	if _, err := jobs.NativeResultIDForJob(t.Context(), job.ID, 1); !errors.Is(err, domain.ErrResultUnavailable) {
		t.Fatalf("stale native job binding error = %v", err)
	}
}

func TestExternalAgentJobStoreRejectsNativeResultWithMismatchedBytes(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "native-job-bytes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create = %v, err = %v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "native-worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, err = %v", claimed, err)
	}
	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := results.Materialize(t.Context(), port.ResultMaterialization{
		Producer: domain.ResultProducer{Kind: domain.ResultProducerExternalAgentJob, ID: job.ID, Revision: claimed.StatusRevision + 1},
		Payload:  "native result", Scope: domain.ResultScope{Actor: job.Actor, TeamID: job.TeamID, ConversationKey: string(job.ConversationKey), Project: job.PrimaryProject},
		Retention: domain.ResultRetentionContext, MediaType: "text/plain; charset=utf-8",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := &domain.ExternalAgentInvocationResult{Text: "native result", Inline: true, NativeResultID: handle.ResultID, ResultSHA256: handle.SHA256, ResultBytes: handle.Bytes + 1}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, result, "", now.Add(time.Second)); err == nil {
		t.Fatal("native result with mismatched bytes was accepted")
	}
	var state string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT state FROM result_records WHERE result_id = ?`, handle.ResultID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.ResultQuarantined) {
		t.Fatalf("mismatched native result state = %q", state)
	}
}

func TestExternalAgentJobStoreQuarantinesNativeResultWhenTerminalRevisionChanges(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "native-cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create = %v, err = %v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "native-worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, err = %v", claimed, err)
	}
	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	const content = "result raced with cancellation"
	handle, err := results.Materialize(t.Context(), port.ResultMaterialization{
		Producer: domain.ResultProducer{Kind: domain.ResultProducerExternalAgentJob, ID: job.ID, Revision: claimed.StatusRevision + 1},
		Payload:  content, Scope: domain.ResultScope{Actor: job.Actor, TeamID: job.TeamID, ConversationKey: string(job.ConversationKey), Project: job.PrimaryProject},
		Retention: domain.ResultRetentionContext, MediaType: "text/plain; charset=utf-8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.RequestCancellation(t.Context(), job.ID, job.Actor); err != nil {
		t.Fatal(err)
	}
	result := &domain.ExternalAgentInvocationResult{
		Text: content, Inline: true, NativeResultID: handle.ResultID,
		ResultSHA256: handle.SHA256, ResultBytes: handle.Bytes,
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, result, "", now.Add(time.Second)); err == nil {
		t.Fatal("cancel-requested job accepted stale native completion")
	}
	var state string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT state FROM result_records WHERE result_id = ?`, handle.ResultID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.ResultQuarantined) {
		t.Fatalf("raced native result state = %q", state)
	}
	var references int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_references WHERE result_id = ?`, handle.ResultID).Scan(&references); err != nil {
		t.Fatal(err)
	}
	if references != 0 {
		t.Fatalf("raced native result references = %d", references)
	}
}

func TestExternalAgentJobStoreRecoveryQuarantinesUnboundNativeResult(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "native-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create = %v, err = %v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "native-worker", time.Second)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, err = %v", claimed, err)
	}
	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	const content = "published before worker crash"
	handle, err := results.Materialize(t.Context(), port.ResultMaterialization{
		Producer: domain.ResultProducer{Kind: domain.ResultProducerExternalAgentJob, ID: job.ID, Revision: claimed.StatusRevision + 1},
		Payload:  content, Scope: domain.ResultScope{Actor: job.Actor, TeamID: job.TeamID, ConversationKey: string(job.ConversationKey), Project: job.PrimaryProject},
		Retention: domain.ResultRetentionContext, MediaType: "text/plain; charset=utf-8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecoverExpired(t.Context(), job.ID, claimed.Attempt, claimed.StatusRevision, now.Add(2*time.Second), domain.JobCompletionUnknown, "completion_unknown"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT state FROM result_records WHERE result_id = ?`, handle.ResultID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.ResultQuarantined) {
		t.Fatalf("crash-orphan native result state = %q", state)
	}
}

func TestExternalAgentJobStoreExpiredCancellationDoesNotRequeue(t *testing.T) {
	for _, tc := range []struct {
		name           string
		withSideEffect bool
		wantStatus     domain.ExternalAgentJobStatus
		wantErrorCode  string
	}{
		{name: "safe cancellation", wantStatus: domain.JobCancelled},
		{name: "possible side effect", withSideEffect: true, wantStatus: domain.JobCompletionUnknown, wantErrorCode: "completion_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "expired-cancellation.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			jobs := NewExternalAgentJobStore(store)
			now := time.Now().UTC()
			job := testExternalAgentJob(now)
			if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
				t.Fatalf("create = %v, err = %v", created, err)
			}
			claimed, err := jobs.ClaimNext(t.Context(), now, "cancel-recovery-worker", time.Minute)
			if err != nil || claimed == nil {
				t.Fatalf("claim = %#v, err = %v", claimed, err)
			}
			if tc.withSideEffect {
				if err := jobs.MarkSideEffectsPossible(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt); err != nil {
					t.Fatal(err)
				}
			}
			cancelled, err := jobs.RequestCancellation(t.Context(), job.ID, job.Actor)
			if err != nil || cancelled == nil || cancelled.Status != domain.JobCancelRequested {
				t.Fatalf("request cancellation = %#v, err = %v", cancelled, err)
			}
			if err := jobs.RecoverExpired(t.Context(), job.ID, claimed.Attempt, cancelled.StatusRevision, now.Add(2*time.Minute), tc.wantStatus, tc.wantErrorCode); err != nil {
				t.Fatal(err)
			}
			finished, err := jobs.GetJob(t.Context(), job.ID)
			if err != nil || finished == nil {
				t.Fatalf("finished = %#v, err = %v", finished, err)
			}
			if finished.Status != tc.wantStatus || finished.ErrorCode != tc.wantErrorCode {
				t.Fatalf("finished = %#v, want status=%s error=%q", finished, tc.wantStatus, tc.wantErrorCode)
			}
		})
	}
}

func TestExternalAgentJobStoreRejectsStaleLeaseAndDeduplicatesRequest(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := NewExternalAgentJobStore(store)
	job := testExternalAgentJob(time.Now().UTC())
	if created, _, err := jobStore.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("first create = %v, err = %v", created, err)
	}
	created, existing, err := jobStore.CreateIfAbsent(t.Context(), job)
	if err != nil || created || existing == nil || existing.ID != job.ID {
		t.Fatalf("duplicate create = %v, existing = %#v, err = %v", created, existing, err)
	}
	if err := jobStore.RenewLease(t.Context(), job.ID, "wrong-owner", 1, time.Now().UTC(), time.Minute); err == nil {
		t.Fatal("stale owner renewed an unclaimed job")
	}
}

func TestExternalAgentJobStoreDeduplicatesByOriginalCallID(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	first := testExternalAgentJob(now)
	if created, _, err := jobStore.CreateIfAbsent(t.Context(), first); err != nil || !created {
		t.Fatalf("first create = %v, err = %v", created, err)
	}
	second := first
	second.ID = "job_2"
	created, existing, err := jobStore.CreateIfAbsent(t.Context(), second)
	if err != nil || created || existing == nil || existing.ID != first.ID {
		t.Fatalf("original call dedupe = %v, existing = %#v, err = %v", created, existing, err)
	}
}

func TestExternalAgentJobStoreRejectsEveryOwnerMutationAfterLeaseExpiry(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	if created, _, err := jobStore.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create = %v, err = %v", created, err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), now.Add(-time.Minute), "worker-expired", time.Second)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, err = %v", claimed, err)
	}
	if err := jobStore.RenewLease(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, now, time.Minute); err == nil {
		t.Fatal("expired lease was renewed")
	}
	if err := jobStore.AssignExternalAgentSession(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, "late-session"); err == nil {
		t.Fatal("expired lease received an external-agent session")
	}
	if err := jobStore.MarkSideEffectsPossible(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt); err == nil {
		t.Fatal("expired lease changed side-effect state")
	}
	if err := jobStore.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobFailed, nil, "late", now); err == nil {
		t.Fatal("expired lease performed a terminal transition")
	}
}

func testExternalAgentJob(now time.Time) domain.ExternalAgentJob {
	request := domain.ExternalAgentJobRequest{
		Provider: "agentcli", Profile: "build", PrimaryProject: "workspace", RegistryRevision: "r1",
		Task: "task", Mode: domain.JobForeground, WrapperCallID: "wrapper-1", OriginalCallID: "original-1",
		Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678",
		Timeout: time.Hour,
	}
	return domain.ExternalAgentJob{
		ID: "job_1", Mode: request.Mode, Provider: request.Provider, Profile: request.Profile,
		PrimaryProject: request.PrimaryProject, RegistryRevision: request.RegistryRevision, Task: request.Task,
		RequestSHA256: domain.ExternalAgentJobRequestDigest(request), WrapperCallID: request.WrapperCallID,
		OriginalCallID: request.OriginalCallID, Actor: request.Actor, TeamID: request.TeamID,
		ConversationKey: request.ConversationKey, Status: domain.JobQueued, TimeoutAt: now.Add(time.Hour),
		CreatedAt: now, UpdatedAt: now,
	}
}

func TestExpiredRecoveryToQueuedRetryCreatesNoTerminalNotification(t *testing.T) {
	for _, mode := range []domain.ExternalAgentJobMode{domain.JobForeground, domain.JobDetached} {
		t.Run(string(mode), func(t *testing.T) {
			store, jobs, now := newActivationTestStore(t)
			job := activationTestJob("activation-retry-"+string(mode), now)
			job.Mode = mode
			seedActivationTestBinding(t, jobs, job, now)
			if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
				t.Fatalf("create = %v, err = %v", created, err)
			}
			claimed, err := jobs.ClaimNext(t.Context(), now, "job-worker-"+job.ID, time.Minute)
			if err != nil || claimed == nil {
				t.Fatalf("claim = %#v, err = %v", claimed, err)
			}
			if err := jobs.RecoverExpired(t.Context(), job.ID, claimed.Attempt, claimed.StatusRevision, now.Add(2*time.Minute), domain.JobQueued, ""); err != nil {
				t.Fatal(err)
			}
			current, err := jobs.GetJob(t.Context(), job.ID)
			if err != nil || current == nil || current.Status != domain.JobQueued || current.LeaseOwner != "" || !current.LeaseExpiry.IsZero() || !current.HeartbeatAt.IsZero() {
				t.Fatalf("recovered job = %#v, err = %v", current, err)
			}
			if rows := terminalNotificationRowsForJob(t, store, job.ID); len(rows) != 0 {
				t.Fatalf("queued retry enqueued terminal notifications: %#v", rows)
			}
			if got := activationCountForJob(t, store, job.ID); got != 0 {
				t.Fatalf("queued retry activation count = %d", got)
			}
			if retried, err := jobs.ClaimNext(t.Context(), now.Add(2*time.Minute+time.Second), "worker-retry", time.Minute); err != nil || retried == nil || retried.Status != domain.JobRunning {
				t.Fatalf("recovered job not claimable: %#v, err = %v", retried, err)
			}
		})
	}
}
