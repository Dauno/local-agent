package acpclient_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/acpclient"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestACPRecoveryUnsupportedLeavesOriginalTaskUnreplayed(t *testing.T) {
	client := acpclient.New("python3", []string{"-c", fakeACPAgentScript(false)})
	job := domain.ExternalAgentJob{ID: "job-1", ACPSessionID: "session-real-1", PrimaryProject: "workspace", Task: "MUTATE ORIGINAL", TimeoutAt: time.Now().Add(time.Minute)}
	_, err := client.Reconcile(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "session recovery is unsupported") || strings.Contains(err.Error(), "MUTATE ORIGINAL") {
		t.Fatalf("recovery error = %v", err)
	}
}
