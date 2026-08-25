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

// An operator closes a completion-unknown job that needs no recovery. The job
// reaches its terminal abandoned status and notifies its conversation.
func TestAbandonCompletionUnknownClosesJobAndNotifies(t *testing.T) {
	jobStore, job, now := completionUnknownFixture(t)
	awaiting, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || awaiting == nil {
		t.Fatalf("job = %#v, err = %v", awaiting, err)
	}
	closed, err := jobStore.AbandonCompletionUnknown(t.Context(), job.ID, awaiting.Actor, awaiting.ConversationKey, awaiting.StatusRevision, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != domain.JobAbandoned || closed.ErrorCode != abandonedByOperatorCode {
		t.Fatalf("closed = %#v", closed)
	}
	if closed.StatusRevision != awaiting.StatusRevision+1 || closed.FinishedAt.IsZero() {
		t.Fatalf("closure must bump the revision and finish the job: %#v", closed)
	}
	var notifications int
	if err := jobStore.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM external_agent_job_notifications WHERE job_id = ? AND status_revision = ?`,
		job.ID, closed.StatusRevision).Scan(&notifications); err != nil {
		t.Fatal(err)
	}
	if notifications != 1 {
		t.Fatalf("a closed job must enqueue exactly one terminal notification, got %d", notifications)
	}
}

// Closure is a compare-and-set. A stale revision, a foreign actor, and a
// second closure are all refused.
func TestAbandonCompletionUnknownRefusesStaleAndUnauthorizedClosure(t *testing.T) {
	jobStore, job, now := completionUnknownFixture(t)
	awaiting, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || awaiting == nil {
		t.Fatalf("job = %#v, err = %v", awaiting, err)
	}
	if _, err := jobStore.AbandonCompletionUnknown(t.Context(), job.ID, awaiting.Actor, awaiting.ConversationKey, awaiting.StatusRevision+1, now); !errors.Is(err, port.ErrExternalAgentJobRevisionConflict) {
		t.Fatalf("stale revision err = %v, want a revision conflict", err)
	}
	if _, err := jobStore.AbandonCompletionUnknown(t.Context(), job.ID, "U-someone-else", awaiting.ConversationKey, awaiting.StatusRevision, now); err == nil {
		t.Fatal("a foreign actor closed the job")
	}
	if _, err := jobStore.AbandonCompletionUnknown(t.Context(), job.ID, awaiting.Actor, "slack:T1:dm:D-other", awaiting.StatusRevision, now); err == nil {
		t.Fatal("a foreign destination closed the job")
	}
	if _, err := jobStore.AbandonCompletionUnknown(t.Context(), job.ID, awaiting.Actor, awaiting.ConversationKey, awaiting.StatusRevision, now); err != nil {
		t.Fatal(err)
	}
	if _, err := jobStore.AbandonCompletionUnknown(t.Context(), job.ID, awaiting.Actor, awaiting.ConversationKey, awaiting.StatusRevision+1, now); err == nil {
		t.Fatal("an abandoned job was closed twice")
	}
}

// A running job is not closable. Only a completion-unknown job is.
func TestAbandonCompletionUnknownRefusesRunningJob(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "close-running.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := NewExternalAgentJobStore(store)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	job := testExternalAgentJob(now)
	if _, _, err := jobStore.CreateIfAbsent(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), now, "worker-1", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claimed = %#v, err = %v", claimed, err)
	}
	if _, err := jobStore.AbandonCompletionUnknown(t.Context(), job.ID, claimed.Actor, claimed.ConversationKey, claimed.StatusRevision, now); err == nil {
		t.Fatal("a running job was closed")
	}
}

func completionUnknownFixture(t *testing.T) (*ExternalAgentJobStore, domain.ExternalAgentJob, time.Time) {
	t.Helper()
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "close-job.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := NewExternalAgentJobStore(store)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	job := testExternalAgentJob(now)
	if _, _, err := jobStore.CreateIfAbsent(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), now, "worker-1", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claimed = %#v, err = %v", claimed, err)
	}
	if err := jobStore.AssignExternalAgentSession(t.Context(), job.ID, "worker-1", 1, "session-1", ""); err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Transition(t.Context(), job.ID, "worker-1", 1, domain.JobCompletionUnknown, nil, "completion_unknown", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return jobStore, job, now.Add(2 * time.Second)
}
