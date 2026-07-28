package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type failingArtifactChecker struct{}

func (failingArtifactChecker) CheckArtifactStore(context.Context, string, int) error {
	return errors.New("artifact directory contains xoxb-secret-token")
}

type failingJobChecker struct{}

func (failingJobChecker) CheckExternalAgentJobs(context.Context, string) error {
	return errors.New("notification outbox is unavailable")
}

func TestOfflineDoctorReportsArtifactAndJobStoreChecks(t *testing.T) {
	deps, _, _ := validDependencies()
	deps.Artifacts = failingArtifactChecker{}
	deps.Jobs = failingJobChecker{}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	if report.ExitCode() != 1 {
		t.Fatalf("report=%#v", report)
	}
	for _, result := range report.Results {
		if result.Name == "ACP artifacts" && strings.Contains(result.Detail, "xoxb-secret-token") {
			t.Fatal("doctor leaked artifact checker detail")
		}
	}
}
