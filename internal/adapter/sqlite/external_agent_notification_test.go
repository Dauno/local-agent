package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestTerminalTransitionEnqueuesOneDurableNotification(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
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
	claimed, err := jobs.ClaimNext(t.Context(), now, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.AcpInvocationResult{Text: "safe summary"}, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	claimedNotification, err := jobs.ClaimNextNotification(t.Context(), now.Add(2*time.Second), "publisher-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimedNotification == nil || claimedNotification.JobID != job.ID || claimedNotification.PublishState != domain.NotificationPublishing {
		t.Fatalf("notification = %#v", claimedNotification)
	}
	if claimedNotification.CanonicalMarkdown == "" || claimedNotification.ContentSHA256 == "" || claimedNotification.Target.ChannelID == "" {
		t.Fatalf("notification lacks canonical delivery data: %#v", claimedNotification)
	}
	if duplicate, err := jobs.ClaimNextNotification(t.Context(), now.Add(2*time.Second), "publisher-2", time.Minute); err != nil || duplicate != nil {
		t.Fatalf("duplicate claim = %#v, err = %v", duplicate, err)
	}
}

func TestNotificationRestartAndAmbiguousPublishAreReconciledBeforeRetry(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	if _, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobFailed, nil, "acp_process_exit", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	first, err := jobs.ClaimNextNotification(t.Context(), now.Add(2*time.Second), "publisher-1", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first claim = %#v, err = %v", first, err)
	}
	if err := jobs.MarkNotificationUnknown(t.Context(), first, "ambiguous"); err != nil {
		t.Fatal(err)
	}
	recovered, err := jobs.ClaimNextNotification(t.Context(), now.Add(3*time.Second), "publisher-2", time.Minute)
	if err != nil || recovered == nil || !recovered.NeedsReconciliation {
		t.Fatalf("recovered claim = %#v, err = %v", recovered, err)
	}
	if err := jobs.MarkNotificationPublished(t.Context(), recovered, "1710000000.000001", now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if again, err := jobs.ClaimNextNotification(t.Context(), now.Add(5*time.Second), "publisher-3", time.Minute); err != nil || again != nil {
		t.Fatalf("published notification was claimable: %#v, err = %v", again, err)
	}
	if err := jobs.MarkNotificationPublished(t.Context(), recovered, "1710000000.000002", now.Add(5*time.Second)); !errors.Is(err, ErrNotificationStateConflict) {
		t.Fatalf("duplicate publish err = %v", err)
	}
}

func TestTerminalTransitionRollsBackWhenNotificationCannotBeBuilt(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	job.ConversationKey = "not-a-slack-key"
	if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create = %v, err = %v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.AcpInvocationResult{Text: "done"}, "", now.Add(time.Second)); err == nil {
		t.Fatal("terminal transition succeeded without a valid notification target")
	}
	current, err := jobs.GetJob(t.Context(), job.ID)
	if err != nil || current == nil || current.Status != domain.JobRunning || current.StatusRevision != claimed.StatusRevision {
		t.Fatalf("transition was not rolled back: job=%#v err=%v", current, err)
	}
}
