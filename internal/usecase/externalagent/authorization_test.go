package externalagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestJobStatusAndCancellationRequireActorAndConversation(t *testing.T) {
	store, err := sqlite.Initialize(context.Background(), t.TempDir()+"/jobs.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)
	service, err := New(
		Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Second, PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1},
		Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), testRequest(domain.JobDetached))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Status(t.Context(), job.ID, "U-other", job.ConversationKey); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("status authorization error = %v", err)
	}
	if _, err := service.CancelForConversation(t.Context(), job.ID, job.Actor, "slack:T:dm:other"); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("conversation authorization error = %v", err)
	}
}
