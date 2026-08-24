package externalagent

import (
	"context"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type projectionRegistry struct {
	alive *bool
}

func (r *projectionRegistry) Register(string, int, int)      {}
func (r *projectionRegistry) ProcessAlive(string, int) *bool { return r.alive }

func TestStatusProjectionMergesLiveProgress(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), t.TempDir()+"/projection.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	service, err := New(Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute, PollInterval: time.Second, Concurrency: 1, MaxAttempts: 1, ProgressWarningTimeout: 10 * time.Second},
		Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}, ProgressStore: jobStore, ProcessRegistry: &projectionRegistry{alive: new(true)}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), testRequest(domain.JobDetached))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.ClaimNext(t.Context(), time.Now().UTC(), "worker_x", time.Minute)
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("claim job: %v %v", claimed, err)
	}
	base := time.Now().UTC().Add(-time.Minute)
	progress := domain.ExternalAgentJobProgress{
		JobID: job.ID, Attempt: 1, Phase: domain.ACPPhaseToolRunning,
		LastEventKind:           domain.ACPEventToolCallUpdate,
		LastTransportActivityAt: base, LastSessionUpdateAt: base,
		LastMeaningfulProgressAt: base.Add(-20 * time.Second),
		PromptStartedAt:          base.Add(-time.Minute),
		ActiveToolCount:          2, PendingPermission: true,
	}
	if err := jobStore.WriteJobProgress(t.Context(), job.ID, "worker_x", 1, progress); err != nil {
		t.Fatalf("write progress: %v", err)
	}
	view, err := service.StatusProjection(t.Context(), job.ID, job.Actor, job.ConversationKey)
	if err != nil {
		t.Fatalf("status projection: %v", err)
	}
	if view.Phase != domain.ACPPhaseToolRunning || view.ActiveToolCount != 2 || !view.PendingPermission {
		t.Fatalf("projected view = %+v", view)
	}
	// Transport stale and process known alive: possibly stalled at read time.
	if view.Health != domain.ACPHealthPossiblyStalled {
		t.Fatalf("health = %s, want possibly_stalled", view.Health)
	}
	if view.ProcessAlive == nil || !*view.ProcessAlive {
		t.Fatalf("process liveness = %v", view.ProcessAlive)
	}
	if view.PromptElapsedSeconds < 55 {
		t.Fatalf("prompt elapsed = %d", view.PromptElapsedSeconds)
	}
}

func TestStatusProjectionAuthorizationHidesIdentity(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), t.TempDir()+"/auth.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	service, err := New(Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute, PollInterval: time.Second, Concurrency: 1, MaxAttempts: 1},
		Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}, ProgressStore: jobStore})
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), testRequest(domain.JobDetached))
	if err != nil {
		t.Fatal(err)
	}
	_ = jobStore.AssignACPSession(t.Context(), job.ID, "worker_x", 1, "ses_secret_identity")
	view, err := service.StatusProjection(t.Context(), job.ID, "intruder", job.ConversationKey)
	if err == nil {
		t.Fatal("unauthorized projection must fail")
	}
	if view != nil && view.ACPSessionID != "" {
		t.Fatal("unauthorized projection leaked session identity")
	}
}

var _ port.ExternalAgentJobStatusProjectionReader = (*Service)(nil)
