package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

type fakeResultRetentionChecker struct {
	health  domain.ResultRetentionHealth
	err     error
	gotAges domain.ResultRetentionAges
	gotNow  time.Time
	gotPath string
	calls   int
}

func (f *fakeResultRetentionChecker) CheckResultRetention(_ context.Context, path string, ages domain.ResultRetentionAges, now time.Time) (domain.ResultRetentionHealth, error) {
	f.calls++
	f.gotPath = path
	f.gotAges = ages
	f.gotNow = now
	return f.health, f.err
}

// TestDoctorReportsResultRetentionCandidateCounts pins hallazgo 6: the
// offline doctor check surfaces the bounded per-class candidate counts
// derived from the configured per-class ages, and always notes that the
// exported class is not scanned yet.
func TestDoctorReportsResultRetentionCandidateCounts(t *testing.T) {
	deps, _, _ := validDependencies()
	checker := &fakeResultRetentionChecker{health: domain.ResultRetentionHealth{
		Classes: []domain.ResultRetentionClassCounts{
			{Class: domain.ResultRetentionContext, Candidates: 2, ReferenceProtected: 1, MaterializationPending: 0},
		},
		ExportedNotImplemented: true,
	}}
	deps.ResultRetention = checker
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	result, ok := findResult(report, "v2 result retention")
	if !ok || result.Status != StatusPass {
		t.Fatalf("v2 result retention result = %#v", result)
	}
	if checker.calls != 1 {
		t.Fatalf("retention checker calls = %d, want 1", checker.calls)
	}
	cfg := config.Default()
	wantAges := domain.ResultRetentionAges{
		Context:      time.Duration(cfg.Orchestration.ResultHandles.Retention.ContextDays) * 24 * time.Hour,
		Conversation: time.Duration(cfg.Orchestration.ResultHandles.Retention.ConversationDays) * 24 * time.Hour,
		Workstream:   time.Duration(cfg.Orchestration.ResultHandles.Retention.WorkstreamDays) * 24 * time.Hour,
	}
	if checker.gotAges != wantAges {
		t.Fatalf("retention ages = %#v, want %#v", checker.gotAges, wantAges)
	}
	if !strings.Contains(result.Detail, "context candidates=2 reference_protected=1 materialization_pending=0") {
		t.Fatalf("retention detail missing context counts: %q", result.Detail)
	}
	if !strings.Contains(result.Detail, "exported class is never scanned") {
		t.Fatalf("retention detail missing exported note: %q", result.Detail)
	}
}

func TestDoctorResultRetentionCheckerErrorIsReported(t *testing.T) {
	deps, _, _ := validDependencies()
	deps.ResultRetention = &fakeResultRetentionChecker{err: errors.New("database is locked")}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	result, ok := findResult(report, "v2 result retention")
	if !ok || result.Status != StatusFail || !strings.Contains(result.Detail, "database is locked") {
		t.Fatalf("v2 result retention error result = %#v", result)
	}
}

func TestDoctorSkipsResultRetentionWhenCheckerAbsent(t *testing.T) {
	deps, _, _ := validDependencies()
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	if _, ok := findResult(report, "v2 result retention"); ok {
		t.Fatal("v2 result retention result present with no checker configured")
	}
}
