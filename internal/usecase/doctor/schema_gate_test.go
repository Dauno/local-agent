package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// TRD 09 checkpoint 2: minimum-schema gating, the three-way
// connection-model branch, and skip semantics.

type countingKnowledgeChecker struct{ calls int }

func (f *countingKnowledgeChecker) CheckKnowledgeRetrievalState(context.Context, string) (domain.KnowledgeRetrievalHealth, error) {
	f.calls++
	return domain.KnowledgeRetrievalHealth{}, nil
}

type countingRetentionChecker struct{ calls int }

func (f *countingRetentionChecker) CheckResultRetention(context.Context, string, domain.ResultRetentionAges, time.Time) (domain.ResultRetentionHealth, error) {
	f.calls++
	return domain.ResultRetentionHealth{}, nil
}

type countingAnalysisChecker struct{ calls int }

func (f *countingAnalysisChecker) CheckResultAnalysisState(context.Context, string) (domain.ResultAnalysisHealth, error) {
	f.calls++
	return domain.ResultAnalysisHealth{}, nil
}

// gateJobsChecker implements JobStoreChecker plus both optional health
// extensions with call counters.
type gateJobsChecker struct {
	jobsCalls       int
	activationCalls int
	identityCalls   int
}

func (f *gateJobsChecker) CheckExternalAgentJobs(context.Context, string) error {
	f.jobsCalls++
	return nil
}

func (f *gateJobsChecker) CheckExternalAgentActivationHealth(context.Context, string) (domain.ExternalAgentJobActivationHealth, error) {
	f.activationCalls++
	return domain.ExternalAgentJobActivationHealth{}, nil
}

func (f *gateJobsChecker) CheckExternalAgentResultIdentityHealth(context.Context, string) (domain.ExternalAgentJobIdentityHealth, error) {
	f.identityCalls++
	return domain.ExternalAgentJobIdentityHealth{}, nil
}

func runtimeWithSchema(version int) *fakeSQLiteRuntimeChecker {
	health := healthySQLiteRuntime()
	health.SchemaVersion = version
	return &fakeSQLiteRuntimeChecker{health: health}
}

func resultByName(t *testing.T, report Report, name string) Result {
	t.Helper()
	result, ok := findResult(report, name)
	if !ok {
		t.Fatalf("result %q missing: %#v", name, report.Results)
	}
	return result
}

// TestMinimumSchemaTableSkipsBelowFloorAndRunsAtFloor proves every gated
// check is skipped with the exact detail at detected = minimum-1 with zero
// checker calls, and runs exactly once at detected = minimum.
func TestMinimumSchemaTableSkipsBelowFloorAndRunsAtFloor(t *testing.T) {
	cases := []struct {
		name      string
		minimum   int
		configure func(*Dependencies)
		calls     func(Dependencies) int
	}{
		{
			name:      "SQLite",
			minimum:   minimumSchemaDatabaseFile,
			configure: func(deps *Dependencies) { deps.Database = &fakeDatabase{} },
			calls:     func(deps Dependencies) int { return deps.Database.(*fakeDatabase).calls },
		},
		{
			name:      "knowledge retrieval state",
			minimum:   minimumSchemaKnowledge,
			configure: func(deps *Dependencies) { deps.Knowledge = &countingKnowledgeChecker{} },
			calls:     func(deps Dependencies) int { return deps.Knowledge.(*countingKnowledgeChecker).calls },
		},
		{
			name:      "v2 result retention",
			minimum:   minimumSchemaResultRetention,
			configure: func(deps *Dependencies) { deps.ResultRetention = &countingRetentionChecker{} },
			calls:     func(deps Dependencies) int { return deps.ResultRetention.(*countingRetentionChecker).calls },
		},
		{
			name:      "v40 result analysis",
			minimum:   minimumSchemaResultAnalysis,
			configure: func(deps *Dependencies) { deps.ResultAnalysis = &countingAnalysisChecker{} },
			calls:     func(deps Dependencies) int { return deps.ResultAnalysis.(*countingAnalysisChecker).calls },
		},
		{
			name:      "recoverable reference index",
			minimum:   minimumSchemaRecoverableRefs,
			configure: func(deps *Dependencies) { deps.RecoverableRefs = &fakeRecoverableReferenceChecker{} },
			calls:     func(deps Dependencies) int { return deps.RecoverableRefs.(*fakeRecoverableReferenceChecker).calls },
		},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s below v%d", tc.name, tc.minimum), func(t *testing.T) {
			deps, _, _ := validDependencies()
			deps.SQLiteRuntime = runtimeWithSchema(tc.minimum - 1)
			tc.configure(&deps)
			service, err := New(deps)
			if err != nil {
				t.Fatal(err)
			}
			report := service.Run(t.Context(), false)
			result := resultByName(t, report, tc.name)
			if result.Status != StatusSkipped {
				t.Fatalf("status = %q, want skipped", result.Status)
			}
			want := fmt.Sprintf("requires schema v%d, database is v%d", tc.minimum, tc.minimum-1)
			if result.Detail != want {
				t.Fatalf("detail = %q, want %q", result.Detail, want)
			}
			if got := tc.calls(deps); got != 0 {
				t.Fatalf("skipped checker was called %d times", got)
			}
		})
		t.Run(fmt.Sprintf("%s at v%d", tc.name, tc.minimum), func(t *testing.T) {
			deps, _, _ := validDependencies()
			deps.SQLiteRuntime = runtimeWithSchema(tc.minimum)
			tc.configure(&deps)
			service, err := New(deps)
			if err != nil {
				t.Fatal(err)
			}
			report := service.Run(t.Context(), false)
			result := resultByName(t, report, tc.name)
			if result.Status != StatusPass {
				t.Fatalf("status = %q (%s), want pass", result.Status, result.Detail)
			}
			if got := tc.calls(deps); got != 1 {
				t.Fatalf("checker calls = %d, want 1", got)
			}
		})
	}
}

// TestJobStoreMinimumsBoundary proves the corrected jobs floor (30, not 18),
// the activations floor (29), and the identity floor (32): at v29 only the
// activations check runs; at v30 jobs joins it; identity joins at v32.
func TestJobStoreMinimumsBoundary(t *testing.T) {
	for _, tc := range []struct {
		detected            int
		wantJobsSkipped     bool
		wantIdentitySkipped bool
	}{
		{detected: 29, wantJobsSkipped: true, wantIdentitySkipped: true},
		{detected: 30, wantJobsSkipped: false, wantIdentitySkipped: true},
		{detected: 32, wantJobsSkipped: false, wantIdentitySkipped: false},
	} {
		t.Run(fmt.Sprintf("detected=%d", tc.detected), func(t *testing.T) {
			deps, _, _ := validDependencies()
			jobs := &gateJobsChecker{}
			deps.Jobs = jobs
			deps.SQLiteRuntime = runtimeWithSchema(tc.detected)
			service, err := New(deps)
			if err != nil {
				t.Fatal(err)
			}
			report := service.Run(t.Context(), false)
			jobsResult := resultByName(t, report, "external-agent jobs")
			if got := jobsResult.Status == StatusSkipped; got != tc.wantJobsSkipped {
				t.Fatalf("jobs status = %q, want skipped=%v", jobsResult.Status, tc.wantJobsSkipped)
			}
			if tc.wantJobsSkipped && jobs.jobsCalls != 0 {
				t.Fatalf("jobs checker called %d times while skipped", jobs.jobsCalls)
			}
			if !tc.wantJobsSkipped && jobs.jobsCalls != 1 {
				t.Fatalf("jobs checker calls = %d, want 1", jobs.jobsCalls)
			}
			activations := resultByName(t, report, "external-agent activations")
			if activations.Status != StatusPass || jobs.activationCalls != 1 {
				t.Fatalf("activations = %#v calls=%d", activations, jobs.activationCalls)
			}
			identity := resultByName(t, report, "external-agent result identity")
			if got := identity.Status == StatusSkipped; got != tc.wantIdentitySkipped {
				t.Fatalf("identity status = %q, want skipped=%v", identity.Status, tc.wantIdentitySkipped)
			}
			if tc.wantIdentitySkipped && jobs.identityCalls != 0 {
				t.Fatalf("identity checker called %d times while skipped", jobs.identityCalls)
			}
			if !tc.wantIdentitySkipped && jobs.identityCalls != 1 {
				t.Fatalf("identity checker calls = %d, want 1", jobs.identityCalls)
			}
			if report.ExitCode() != 0 {
				t.Fatalf("exit code = %d, want 0: %#v", report.ExitCode(), report.Results)
			}
		})
	}
}

// TestKnowledgeRetrievalMinimumBoundary proves the knowledge retrieval floor
// is v39 (not the v38 knowledge tables): at v38 the check skips; at v39 it
// runs.
func TestKnowledgeRetrievalMinimumBoundary(t *testing.T) {
	for _, tc := range []struct {
		detected    int
		wantSkipped bool
	}{
		{detected: 38, wantSkipped: true},
		{detected: 39, wantSkipped: false},
	} {
		t.Run(fmt.Sprintf("detected=%d", tc.detected), func(t *testing.T) {
			deps, _, _ := validDependencies()
			knowledge := &countingKnowledgeChecker{}
			deps.Knowledge = knowledge
			deps.SQLiteRuntime = runtimeWithSchema(tc.detected)
			service, err := New(deps)
			if err != nil {
				t.Fatal(err)
			}
			report := service.Run(t.Context(), false)
			result := resultByName(t, report, "knowledge retrieval state")
			if got := result.Status == StatusSkipped; got != tc.wantSkipped {
				t.Fatalf("status = %q, want skipped=%v", result.Status, tc.wantSkipped)
			}
			if tc.wantSkipped {
				if knowledge.calls != 0 {
					t.Fatalf("checker called %d times while skipped", knowledge.calls)
				}
				want := "requires schema v39, database is v38"
				if result.Detail != want {
					t.Fatalf("detail = %q, want %q", result.Detail, want)
				}
			} else if knowledge.calls != 1 {
				t.Fatalf("checker calls = %d, want 1", knowledge.calls)
			}
		})
	}
}

// TestConnectionModelPreUpgradeIsInformational proves a pre-v41 database
// passes the connection-model check with the upgrade detail and its real
// journal mode reported informationally, without failing on it.
func TestConnectionModelPreUpgradeIsInformational(t *testing.T) {
	deps, _, _ := validDependencies()
	health := healthySQLiteRuntime()
	health.SchemaVersion = 33
	health.JournalMode = "delete"
	deps.SQLiteRuntime = &fakeSQLiteRuntimeChecker{health: health}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	result := resultByName(t, report, "SQLite connection model")
	if result.Status != StatusPass || result.Fatal {
		t.Fatalf("connection model = %#v", result)
	}
	for _, want := range []string{
		"schema v33",
		"current binary requires v41",
		"run local-agent db upgrade",
		"journal_mode=delete",
	} {
		if !strings.Contains(result.Detail, want) {
			t.Fatalf("detail %q missing %q", result.Detail, want)
		}
	}
	if report.ExitCode() != 0 {
		t.Fatalf("exit code = %d, want 0", report.ExitCode())
	}
}

// TestConnectionModelFutureSchemaOrReadFailureIsFatal proves both fatal
// branches stop all later checks and drive exit code 2.
func TestConnectionModelFutureSchemaOrReadFailureIsFatal(t *testing.T) {
	cases := []struct {
		name    string
		runtime *fakeSQLiteRuntimeChecker
	}{
		{name: "future schema", runtime: runtimeWithSchema(42)},
		{name: "unreadable schema", runtime: &fakeSQLiteRuntimeChecker{err: errors.New("read schema version: I/O error")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, database, _ := validDependencies()
			deps.Jobs = &gateJobsChecker{}
			deps.RecoverableRefs = &fakeRecoverableReferenceChecker{}
			deps.SQLiteRuntime = tc.runtime
			service, err := New(deps)
			if err != nil {
				t.Fatal(err)
			}
			report := service.Run(t.Context(), false)
			result := resultByName(t, report, "SQLite connection model")
			if result.Status != StatusFail || !result.Fatal {
				t.Fatalf("connection model = %#v", result)
			}
			if code := report.ExitCode(); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if database.calls != 0 {
				t.Fatalf("database checker ran %d times after fatal schema branch", database.calls)
			}
			if _, ok := findResult(report, "external-agent jobs"); ok {
				t.Fatal("later checks ran after fatal schema branch")
			}
			if _, ok := findResult(report, "recoverable reference index"); ok {
				t.Fatal("later checks ran after fatal schema branch")
			}
			last := report.Results[len(report.Results)-1]
			if last.Name != "SQLite connection model" {
				t.Fatalf("results continued past the fatal branch: %#v", report.Results[len(report.Results)-3:])
			}
		})
	}
}

// TestConnectionModelV0Boundary pins FIND-184: a valid read with
// detected == 0 skips the connection-model check itself with the frozen v1
// detail and no pragma validation; the inspector still runs exactly once,
// and every later SQLite check with a higher floor is never called.
func TestConnectionModelV0Boundary(t *testing.T) {
	cases := []struct {
		name                  string
		detected              int
		wantConnectionSkipped bool
	}{
		{name: "v0 skips connection model", detected: 0, wantConnectionSkipped: true},
		{name: "v1 runs connection model", detected: 1, wantConnectionSkipped: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, _ := validDependencies()
			runtime := runtimeWithSchema(tc.detected)
			database := &fakeDatabase{}
			jobs := &gateJobsChecker{}
			knowledge := &countingKnowledgeChecker{}
			recoverable := &fakeRecoverableReferenceChecker{}
			deps.SQLiteRuntime = runtime
			deps.Database = database
			deps.Jobs = jobs
			deps.Knowledge = knowledge
			deps.RecoverableRefs = recoverable
			service, err := New(deps)
			if err != nil {
				t.Fatal(err)
			}
			report := service.Run(t.Context(), false)

			if runtime.calls != 1 {
				t.Fatalf("inspector calls = %d, want exactly 1", runtime.calls)
			}
			connection := resultByName(t, report, "SQLite connection model")
			if got := connection.Status == StatusSkipped; got != tc.wantConnectionSkipped {
				t.Fatalf("connection model status = %q (%s)", connection.Status, connection.Detail)
			}
			if tc.wantConnectionSkipped {
				if want := "requires schema v1, database is v0"; connection.Detail != want {
					t.Fatalf("detail = %q, want %q", connection.Detail, want)
				}
				if strings.Contains(connection.Detail, "journal_mode") || strings.Contains(connection.Detail, "synchronous") {
					t.Fatalf("skip detail must not validate pragmas: %q", connection.Detail)
				}
			}
			if tc.detected == 0 {
				if database.calls != 0 || jobs.jobsCalls != 0 || knowledge.calls != 0 || recoverable.calls != 0 {
					t.Fatalf("later checks ran at v0: db=%d jobs=%d knowledge=%d refs=%d",
						database.calls, jobs.jobsCalls, knowledge.calls, recoverable.calls)
				}
				for _, name := range []string{"SQLite", "external-agent jobs", "knowledge retrieval state", "recoverable reference index"} {
					if result := resultByName(t, report, name); result.Status != StatusSkipped {
						t.Fatalf("%s = %#v, want skipped at v0", name, result)
					}
				}
			} else {
				if database.calls != 1 {
					t.Fatalf("database calls = %d, want 1 at v1", database.calls)
				}
				if jobs.jobsCalls != 0 {
					t.Fatalf("jobs calls = %d, want 0 at v1", jobs.jobsCalls)
				}
			}
			if report.ExitCode() != 0 {
				t.Fatalf("exit code = %d, want 0", report.ExitCode())
			}
		})
	}
}

// TestSkipNeverChangesExitCode proves StatusSkipped is honest but inert: a
// run whose only non-pass results are skips exits zero.
func TestSkipNeverChangesExitCode(t *testing.T) {
	report := Report{Results: []Result{
		{Name: "a", Status: StatusPass},
		{Name: "b", Status: StatusSkipped, Detail: "requires schema v41, database is v33"},
	}}
	if code := report.ExitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	fatalFail := Report{Results: []Result{{Name: "c", Status: StatusFail, Fatal: true}}}
	if code := fatalFail.ExitCode(); code != 2 {
		t.Fatalf("fatal exit code = %d, want 2", code)
	}
}
