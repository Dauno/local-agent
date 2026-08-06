package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
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
	if err := jobStore.AssignACPSession(t.Context(), job.ID, "worker-1", 1, "session-1"); err != nil {
		t.Fatal(err)
	}
	if err := jobStore.MarkSideEffectsPossible(t.Context(), job.ID, "worker-1", 1); err != nil {
		t.Fatal(err)
	}
	result := &domain.AcpInvocationResult{Text: "done", Inline: true, ResultBytes: 4}
	if err := jobStore.Transition(t.Context(), job.ID, "worker-1", 1, domain.JobCompleted, result, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	finished, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || finished == nil {
		t.Fatalf("finished = %#v, err = %v", finished, err)
	}
	if finished.Status != domain.JobCompleted || !finished.SideEffectsPossible || finished.ACPSessionID != "session-1" || finished.ResultSummary != "done" {
		t.Fatalf("finished = %#v", finished)
	}
	if err := jobStore.Transition(t.Context(), job.ID, "worker-1", 1, domain.JobFailed, nil, "late", now.Add(2*time.Second)); err == nil {
		t.Fatal("stale attempt terminalized a completed job")
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
	if err := jobStore.AssignACPSession(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, "late-session"); err == nil {
		t.Fatal("expired lease received an ACP session")
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
		Provider: "opencode", Profile: "build", PrimaryProject: "workspace", RegistryRevision: "r1",
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
