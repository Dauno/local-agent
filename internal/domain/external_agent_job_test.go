package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestExternalAgentJobAllowsOnlyDeclaredTransitions(t *testing.T) {
	job := domain.ExternalAgentJob{ID: "job_1", Status: domain.JobQueued}
	for _, next := range []domain.ExternalAgentJobStatus{domain.JobRunning, domain.JobCancelled} {
		if err := job.Transition(next); err != nil {
			t.Fatalf("queued -> %s: %v", next, err)
		}
		job.Status = domain.JobQueued
	}
	if err := job.Transition(domain.JobCompleted); err == nil {
		t.Fatal("queued -> completed was accepted")
	}
}

func TestExternalAgentJobRequestDigestExcludesHostPaths(t *testing.T) {
	request := domain.ExternalAgentJobRequest{
		Provider: "opencode", Profile: "build", PrimaryProject: "workspace",
		AdditionalProjects: []string{"docs"}, RegistryRevision: "rev-1",
		Task: "create a document", Mode: domain.JobDetached,
		PermissionOptionKind: domain.ACPPermissionAllowOnce,
		Timeout:              2 * time.Hour,
		PrimaryPath:          "/private/one", AdditionalPaths: []string{"/private/two"},
	}
	left := domain.ExternalAgentJobRequestDigest(request)
	request.PrimaryPath = "/another/private/path"
	request.AdditionalPaths = []string{"/different"}
	right := domain.ExternalAgentJobRequestDigest(request)
	if left == "" || left != right || len(left) != 64 || strings.Contains(left, "private") {
		t.Fatalf("digest = %q / %q", left, right)
	}
}
