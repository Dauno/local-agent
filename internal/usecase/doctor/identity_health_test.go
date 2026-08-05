package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

type fakeIdentityJobsChecker struct {
	identity domain.ExternalAgentJobIdentityHealth
	err      error
}

func (f fakeIdentityJobsChecker) CheckExternalAgentJobs(context.Context, string) error { return nil }

func (f fakeIdentityJobsChecker) CheckExternalAgentActivationHealth(context.Context, string) (domain.ExternalAgentJobActivationHealth, error) {
	return domain.ExternalAgentJobActivationHealth{}, nil
}

func (f fakeIdentityJobsChecker) CheckExternalAgentResultIdentityHealth(context.Context, string) (domain.ExternalAgentJobIdentityHealth, error) {
	return f.identity, f.err
}

func TestDoctorResultIdentityOperationalDecision(t *testing.T) {
	for _, test := range []struct {
		name     string
		identity domain.ExternalAgentJobIdentityHealth
		wantPass bool
	}{
		{name: "complete identity passes", identity: domain.ExternalAgentJobIdentityHealth{}, wantPass: true},
		{name: "informational counts pass", identity: domain.ExternalAgentJobIdentityHealth{
			JobsCompletedWithoutResultIdentityLegacy: 3,
			RetiredForegroundActivations:             4,
		}, wantPass: true},
		{name: "current completed identity fails", identity: domain.ExternalAgentJobIdentityHealth{JobsCompletedWithoutResultIdentity: 1}, wantPass: false},
		{name: "notification without identity fails", identity: domain.ExternalAgentJobIdentityHealth{NotificationsWithoutIdentity: 1}, wantPass: false},
		{name: "activation without content fails", identity: domain.ExternalAgentJobIdentityHealth{ActivationsWithoutContent: 1}, wantPass: false},
		{name: "activation without identity fails", identity: domain.ExternalAgentJobIdentityHealth{ActivationsWithoutIdentity: 1}, wantPass: false},
		{name: "non-terminal foreground activation fails", identity: domain.ExternalAgentJobIdentityHealth{ForegroundActivationsActive: 1}, wantPass: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps, _, _ := validDependencies()
			deps.Jobs = fakeIdentityJobsChecker{identity: test.identity}
			service, err := New(deps)
			if err != nil {
				t.Fatal(err)
			}
			report := service.Run(t.Context(), false)
			result, ok := findResult(report, "external-agent result identity")
			if !ok {
				t.Fatalf("identity result missing: %#v", report.Results)
			}
			if got := result.Status == StatusPass; got != test.wantPass {
				t.Fatalf("identity result = %#v, want pass=%v", result, test.wantPass)
			}
			if !test.wantPass && result.Remediation == "" {
				t.Fatalf("identity failure lacks remediation: %#v", result)
			}
			combined := result.Detail + " " + result.Remediation
			for _, forbidden := range []string{"job-", "U12345678", "slack:", "sha256", "digest=", "ref="} {
				if strings.Contains(combined, forbidden) {
					t.Fatalf("identity result leaked %q: %q", forbidden, combined)
				}
			}
		})
	}
}

func TestDoctorResultIdentityCheckerErrorIsActionable(t *testing.T) {
	deps, _, _ := validDependencies()
	deps.Jobs = fakeIdentityJobsChecker{err: errors.New("identity health store is not configured")}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	result, ok := findResult(report, "external-agent result identity")
	if !ok || result.Status != StatusFail || !strings.Contains(result.Detail, "identity health store is not configured") {
		t.Fatalf("identity checker error = %#v", result)
	}
	if result.Remediation == "" {
		t.Fatalf("identity remediation missing: %#v", result)
	}
}

func findResult(report Report, name string) (Result, bool) {
	for _, result := range report.Results {
		if result.Name == name {
			return result, true
		}
	}
	return Result{}, false
}
