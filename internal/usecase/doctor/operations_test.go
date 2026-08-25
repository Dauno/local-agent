package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
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
		if result.Name == "external-agent artifacts" && strings.Contains(result.Detail, "xoxb-secret-token") {
			t.Fatal("doctor leaked artifact checker detail")
		}
	}
}

type knowledgeCheckerStub struct {
	health domain.KnowledgeRetrievalHealth
	err    error
	calls  int
}

func (c *knowledgeCheckerStub) CheckKnowledgeRetrievalState(context.Context, string) (domain.KnowledgeRetrievalHealth, error) {
	c.calls++
	return c.health, c.err
}

func TestOfflineDoctorReportsKnowledgeRetrievalState(t *testing.T) {
	t.Run("pass with bounded counts", func(t *testing.T) {
		deps, _, _ := validDependencies()
		checker := &knowledgeCheckerStub{health: domain.KnowledgeRetrievalHealth{LexicalQueuePending: 2, LexicalRepairableMismatch: 1}}
		deps.Knowledge = checker
		service, err := New(deps)
		if err != nil {
			t.Fatal(err)
		}
		report := service.Run(t.Context(), false)
		if report.ExitCode() != 0 || checker.calls != 1 {
			t.Fatalf("report=%#v checker calls=%d", report, checker.calls)
		}
		for _, result := range report.Results {
			if result.Name == "knowledge retrieval state" && !strings.Contains(result.Detail, "pending=2") {
				t.Fatalf("knowledge result = %#v", result)
			}
		}
	})
	t.Run("failure with bounded remediation", func(t *testing.T) {
		deps, _, _ := validDependencies()
		deps.Knowledge = &knowledgeCheckerStub{err: errors.New("knowledge_retrieval_fts has 3 unrepairable orphan rows")}
		service, err := New(deps)
		if err != nil {
			t.Fatal(err)
		}
		report := service.Run(t.Context(), false)
		if report.ExitCode() != 1 {
			t.Fatalf("report=%#v", report)
		}
		for _, result := range report.Results {
			if result.Name != "knowledge retrieval state" {
				continue
			}
			if result.Status != StatusFail {
				t.Fatalf("knowledge result = %#v", result)
			}
			// Hallazgo 12: the remediation for a damaged reconstructible
			// knowledge state points the operator at the bounded
			// `knowledge rebuild-index` command, not the destructive
			// `init --reset-state`.
			if !strings.Contains(result.Remediation, "knowledge rebuild-index") {
				t.Fatalf("remediation = %q, want it to mention knowledge rebuild-index", result.Remediation)
			}
			if strings.Contains(result.Remediation, "--reset-state") {
				t.Fatalf("remediation = %q, must not suggest the destructive reset", result.Remediation)
			}
		}
	})
}
