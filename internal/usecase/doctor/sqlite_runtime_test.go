package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

// resolvedDatabasePath returns the exact database path validDependencies'
// configuration resolves to, so a FIND-129 test can craft an error that
// names it precisely.
func resolvedDatabasePath(t *testing.T) string {
	t.Helper()
	cfg := config.Default()
	paths, err := cfg.ResolvePaths("/tmp/project")
	if err != nil {
		t.Fatal(err)
	}
	return paths.DatabaseFile
}

type fakeSQLiteRuntimeChecker struct {
	health  domain.SQLiteRuntimeHealth
	err     error
	gotPath string
	calls   int
}

func (f *fakeSQLiteRuntimeChecker) CheckSQLiteRuntime(_ context.Context, path string) (domain.SQLiteRuntimeHealth, error) {
	f.calls++
	f.gotPath = path
	return f.health, f.err
}

func healthySQLiteRuntime() domain.SQLiteRuntimeHealth {
	return domain.SQLiteRuntimeHealth{
		SchemaVersion: 45, JournalMode: "wal", Synchronous: 2,
		BusyTimeoutMillis: 5000, ForeignKeys: true, MaxOpenConnections: 4,
	}
}

func TestDoctorReportsHealthySQLiteRuntime(t *testing.T) {
	deps, _, _ := validDependencies()
	checker := &fakeSQLiteRuntimeChecker{health: healthySQLiteRuntime()}
	deps.SQLiteRuntime = checker
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	result, ok := findResult(report, "SQLite connection model")
	if !ok || result.Status != StatusPass {
		t.Fatalf("SQLite connection model result = %#v", result)
	}
	if checker.calls != 1 {
		t.Fatalf("checker calls = %d, want 1", checker.calls)
	}
	for _, want := range []string{"schema_version=45", "journal_mode=wal", "synchronous=2", "busy_timeout_ms=5000", "foreign_keys=true", "max_open_connections=4"} {
		if !strings.Contains(result.Detail, want) {
			t.Fatalf("detail %q missing %q", result.Detail, want)
		}
	}
}

func TestDoctorFailsSQLiteRuntimeOnContractMismatch(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(domain.SQLiteRuntimeHealth) domain.SQLiteRuntimeHealth
		want   string
	}{
		// A schema version below 41 is informational at v41-era contract
		// checks only under the pre-upgrade branch (TRD 09 checkpoint 2); a
		// v41-detected database must not carry a mismatching user_version.
		{"journal_mode", func(h domain.SQLiteRuntimeHealth) domain.SQLiteRuntimeHealth { h.JournalMode = "delete"; return h }, "journal_mode=delete"},
		{"synchronous", func(h domain.SQLiteRuntimeHealth) domain.SQLiteRuntimeHealth { h.Synchronous = 1; return h }, "synchronous=1"},
		{"busy_timeout", func(h domain.SQLiteRuntimeHealth) domain.SQLiteRuntimeHealth { h.BusyTimeoutMillis = 1000; return h }, "busy_timeout_ms=1000"},
		{"foreign_keys", func(h domain.SQLiteRuntimeHealth) domain.SQLiteRuntimeHealth { h.ForeignKeys = false; return h }, "foreign_keys=off"},
		{"pool", func(h domain.SQLiteRuntimeHealth) domain.SQLiteRuntimeHealth { h.MaxOpenConnections = 1; return h }, "max_open_connections=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, _ := validDependencies()
			deps.SQLiteRuntime = &fakeSQLiteRuntimeChecker{health: tc.mutate(healthySQLiteRuntime())}
			service, err := New(deps)
			if err != nil {
				t.Fatal(err)
			}
			report := service.Run(t.Context(), false)
			result, ok := findResult(report, "SQLite connection model")
			if !ok || result.Status != StatusFail || !strings.Contains(result.Detail, tc.want) {
				t.Fatalf("result = %#v, want failure containing %q", result, tc.want)
			}
			if result.Remediation == "" {
				t.Fatal("expected actionable remediation")
			}
		})
	}
}

func TestDoctorSQLiteRuntimeCheckerErrorIsReported(t *testing.T) {
	deps, _, _ := validDependencies()
	deps.SQLiteRuntime = &fakeSQLiteRuntimeChecker{err: errors.New("database is locked")}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	result, ok := findResult(report, "SQLite connection model")
	if !ok || result.Status != StatusFail || !strings.Contains(result.Detail, "database is locked") {
		t.Fatalf("result = %#v", result)
	}
}

// TestDoctorSQLiteRuntimeCheckerErrorDoesNotExposeDatabasePath pins
// FIND-129: OpenReadOnly's own open errors name the exact configured
// database path, and that path must not reach Result.Detail.
func TestDoctorSQLiteRuntimeCheckerErrorDoesNotExposeDatabasePath(t *testing.T) {
	deps, _, _ := validDependencies()
	dbPath := resolvedDatabasePath(t)
	deps.SQLiteRuntime = &fakeSQLiteRuntimeChecker{err: fmt.Errorf("open SQLite database %s: permission denied", dbPath)}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	result, ok := findResult(report, "SQLite connection model")
	if !ok || result.Status != StatusFail {
		t.Fatalf("result = %#v", result)
	}
	if strings.Contains(result.Detail, dbPath) {
		t.Fatalf("detail leaked the database path: %q", result.Detail)
	}
	if !strings.Contains(result.Detail, "permission denied") {
		t.Fatalf("detail lost the useful error text: %q", result.Detail)
	}
}

// TestDoctorRejectsNilSQLiteRuntimeChecker pins FIND-183: the schema
// inspector is mandatory. A composition without it cannot run a single
// schema read, so New must reject it instead of falling back to ungated
// checks.
func TestDoctorRejectsNilSQLiteRuntimeChecker(t *testing.T) {
	deps, _, _ := validDependencies()
	deps.SQLiteRuntime = nil
	if _, err := New(deps); err == nil || !strings.Contains(err.Error(), "runtime checker") {
		t.Fatalf("err = %v, want runtime checker required", err)
	}
}

type fakeRecoverableReferenceChecker struct {
	health  domain.RecoverableReferenceHealth
	err     error
	gotPath string
	calls   int
}

func (f *fakeRecoverableReferenceChecker) CheckRecoverableReferenceHealth(_ context.Context, path string) (domain.RecoverableReferenceHealth, error) {
	f.calls++
	f.gotPath = path
	return f.health, f.err
}

func TestDoctorReportsHealthyRecoverableReferenceIndex(t *testing.T) {
	deps, _, _ := validDependencies()
	checker := &fakeRecoverableReferenceChecker{health: domain.RecoverableReferenceHealth{
		TotalRefRows: 5, DistinctRefs: 3, EventOwners: 2, CapsuleOwners: 1,
	}}
	deps.RecoverableRefs = checker
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	result, ok := findResult(report, "recoverable reference index")
	if !ok || result.Status != StatusPass {
		t.Fatalf("recoverable reference index result = %#v", result)
	}
	if checker.calls != 1 {
		t.Fatalf("checker calls = %d, want 1", checker.calls)
	}
	for _, forbidden := range []string{"ref=", "owner_id=", "session_id="} {
		if strings.Contains(result.Detail, forbidden) {
			t.Fatalf("detail leaked identity marker %q: %q", forbidden, result.Detail)
		}
	}
	for _, want := range []string{"total_ref_rows=5", "distinct_refs=3", "adk_event_owners=2", "continuity_capsule_owners=1"} {
		if !strings.Contains(result.Detail, want) {
			t.Fatalf("detail %q missing %q", result.Detail, want)
		}
	}
}

func TestDoctorFailsRecoverableReferenceIndexOnDanglingRefs(t *testing.T) {
	deps, _, _ := validDependencies()
	deps.RecoverableRefs = &fakeRecoverableReferenceChecker{health: domain.RecoverableReferenceHealth{
		TotalRefRows: 4, DistinctRefs: 4, DanglingRefs: 1,
	}}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	result, ok := findResult(report, "recoverable reference index")
	if !ok || result.Status != StatusFail || !strings.Contains(result.Detail, "dangling_refs=1") {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(result.Remediation, "Stop retention cleanup") {
		t.Fatalf("remediation = %q", result.Remediation)
	}
}

func TestDoctorFailsRecoverableReferenceIndexOnDanglingOwners(t *testing.T) {
	deps, _, _ := validDependencies()
	deps.RecoverableRefs = &fakeRecoverableReferenceChecker{health: domain.RecoverableReferenceHealth{
		TotalRefRows: 4, DistinctRefs: 4, DanglingOwners: 2,
	}}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	result, ok := findResult(report, "recoverable reference index")
	if !ok || result.Status != StatusFail || !strings.Contains(result.Detail, "dangling_owners=2") {
		t.Fatalf("result = %#v", result)
	}
}

func TestDoctorRecoverableReferenceCheckerErrorIsReported(t *testing.T) {
	deps, _, _ := validDependencies()
	deps.RecoverableRefs = &fakeRecoverableReferenceChecker{err: errors.New("database is locked")}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	result, ok := findResult(report, "recoverable reference index")
	if !ok || result.Status != StatusFail || !strings.Contains(result.Detail, "database is locked") {
		t.Fatalf("result = %#v", result)
	}
}

// TestDoctorRecoverableReferenceCheckerErrorDoesNotExposeDatabasePath pins
// FIND-129 for the second checkpoint-6 result: an error naming the exact
// configured database path must not carry that path into Result.Detail.
// The production check issues no query with a ref, owner ID, or session ID
// bound as a parameter (every recoverableReferenceHealthCount call and the
// dangling-owners query take no arguments), so the realistic error surface
// this check can produce is the database path plus a missing-object name —
// never fixture content — and this test exercises exactly that surface.
func TestDoctorRecoverableReferenceCheckerErrorDoesNotExposeDatabasePath(t *testing.T) {
	deps, _, _ := validDependencies()
	dbPath := resolvedDatabasePath(t)
	deps.RecoverableRefs = &fakeRecoverableReferenceChecker{err: fmt.Errorf(
		"open SQLite database %s: recoverable reference table \"recoverable_result_refs\" is missing", dbPath)}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	result, ok := findResult(report, "recoverable reference index")
	if !ok || result.Status != StatusFail {
		t.Fatalf("result = %#v", result)
	}
	if strings.Contains(result.Detail, dbPath) {
		t.Fatalf("detail leaked the database path: %q", result.Detail)
	}
	for _, forbidden := range []string{"ref=", "owner_id=", "session_id="} {
		if strings.Contains(result.Detail, forbidden) {
			t.Fatalf("detail leaked identity marker %q: %q", forbidden, result.Detail)
		}
	}
	if !strings.Contains(result.Detail, "is missing") {
		t.Fatalf("detail lost the useful error text: %q", result.Detail)
	}
}

func TestDoctorSkipsRecoverableReferenceIndexWhenCheckerAbsent(t *testing.T) {
	deps, _, _ := validDependencies()
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	if _, ok := findResult(report, "recoverable reference index"); ok {
		t.Fatal("recoverable reference index result present with no checker configured")
	}
}
