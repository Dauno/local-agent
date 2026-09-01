package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/doctor"
)

// TRD 09 checkpoint 2 offline gates: real checkers against real SQLite
// fixtures. A v33 fixture with an on-disk journal_mode=delete matches the
// known deployment state; v41 must behave exactly like today; a future
// schema and an unreadable version must be fatal with no later checks.

type gateSecrets struct{ values map[string]string }

func (f gateSecrets) Resolve(keys ...string) (map[string]string, error) {
	resolved := make(map[string]string, len(keys))
	for _, key := range keys {
		resolved[key] = f.values[key]
	}
	return resolved, nil
}

// writeDoctorGateFixture builds a real database at the current schema, then
// optionally rewinds its header version and switches its on-disk journal
// mode. This exercises the exact bytes doctor will inspect; tables beyond
// the declared version stay present but every gated check consults the
// detected version, never the table census.
func writeDoctorGateFixture(t *testing.T, detected int, journalDelete bool) string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(stateDir, "local-agent.db")
	store, err := sqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if journalDelete {
		var mode string
		if err := raw.QueryRowContext(ctx, "PRAGMA journal_mode = delete").Scan(&mode); err != nil || mode != "delete" {
			t.Fatalf("journal_mode=delete: mode=%q err=%v", mode, err)
		}
	}
	if _, err := raw.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", detected)); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := raw.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != detected {
		t.Fatalf("user_version=%d err=%v, want %d", version, err, detected)
	}
	return dbPath
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func doctorGateDependencies(dbPath string, runtime doctor.SQLiteRuntimeChecker) doctor.Dependencies {
	deps := doctor.Dependencies{
		ConfigPath:      filepath.Join(filepath.Dir(dbPath), "config.yaml"),
		LoadConfig:      func(string) (config.Config, error) { return config.Default(), nil },
		Secrets:         gateSecrets{values: map[string]string{"DEEPSEEK_API_KEY": "secret-model-key", "SLACK_BOT_TOKEN": "xoxb-secret-token", "SLACK_APP_TOKEN": "xapp-secret-token"}},
		Database:        databaseChecker{},
		Jobs:            jobStoreChecker{},
		Knowledge:       knowledgeChecker{},
		ResultRetention: resultRetentionChecker{},
		ResultAnalysis:  resultAnalysisChecker{},
		SQLiteRuntime:   sqliteRuntimeChecker{},
		RecoverableRefs: recoverableReferenceChecker{},
	}
	if runtime != nil {
		deps.SQLiteRuntime = runtime
	}
	return deps
}

func findGateResult(t *testing.T, report doctor.Report, name string) doctor.Result {
	t.Helper()
	for _, result := range report.Results {
		if result.Name == name {
			return result
		}
	}
	t.Fatalf("result %q missing: %#v", name, report.Results)
	return doctor.Result{}
}

// TestDoctorV33JournalDeleteRunsGatedChecksAndLeavesBytesUnchanged proves
// the corrected pre-upgrade contract: connection model passes with the real
// journal mode reported informationally, v34+ floors skip, jobs (v30),
// activations (v29), and identity (v32) still run, exit code is 0, and the
// main database file keeps its exact SHA-256.
func TestDoctorV33JournalDeleteRunsGatedChecksAndLeavesBytesUnchanged(t *testing.T) {
	dbPath := writeDoctorGateFixture(t, 33, true)
	before := sha256File(t, dbPath)

	service, err := doctor.New(doctorGateDependencies(dbPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(context.Background(), false)

	// Byte identity is asserted first, before any report inspection, so a
	// write-capable checker can never hide behind an earlier status failure
	// (FIND-185).
	if after := sha256File(t, dbPath); after != before {
		t.Fatalf("fixture bytes changed: before=%s after=%s", before, after)
	}

	connectionModel := findGateResult(t, report, "SQLite connection model")
	if connectionModel.Status != doctor.StatusPass || connectionModel.Fatal {
		t.Fatalf("connection model = %#v", connectionModel)
	}
	for _, want := range []string{"schema v33", "current binary requires v47", "run local-agent db upgrade", "journal_mode=delete"} {
		if !strings.Contains(connectionModel.Detail, want) {
			t.Fatalf("detail %q missing %q", connectionModel.Detail, want)
		}
	}
	integrity := findGateResult(t, report, "SQLite")
	if integrity.Status != doctor.StatusPass || !strings.Contains(integrity.Detail, "integrity check") {
		t.Fatalf("SQLite = %#v", integrity)
	}
	wantSkips := map[string]string{
		"knowledge retrieval state":   "requires schema v39, database is v33",
		"v2 result retention":         "requires schema v34, database is v33",
		"v40 result analysis":         "requires schema v40, database is v33",
		"recoverable reference index": "requires schema v41, database is v33",
	}
	for name, detail := range wantSkips {
		result := findGateResult(t, report, name)
		if result.Status != doctor.StatusSkipped || result.Detail != detail {
			t.Fatalf("%s = %#v, want skip %q", name, result, detail)
		}
	}
	for _, skipped := range []string{"external-agent jobs", "external-agent activations"} {
		if result := findGateResult(t, report, skipped); result.Status != doctor.StatusSkipped {
			t.Fatalf("%s = %#v (%s)", skipped, result, result.Detail)
		}
	}
	if result := findGateResult(t, report, "external-agent result identity"); result.Status != doctor.StatusPass {
		t.Fatalf("external-agent result identity = %#v (%s)", result, result.Detail)
	}
	if code := report.ExitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0: %#v", code, report.Results)
	}
}

// TestDoctorV47RunsEverythingWithoutSkips proves no regression at the
// current release version: every check runs, none skips, WAL is enforced,
// and the run exits clean.
func TestDoctorV47RunsEverythingWithoutSkips(t *testing.T) {
	dbPath := writeDoctorGateFixture(t, 47, false)
	before := sha256File(t, dbPath)

	service, err := doctor.New(doctorGateDependencies(dbPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(context.Background(), false)

	connectionModel := findGateResult(t, report, "SQLite connection model")
	if connectionModel.Status != doctor.StatusPass ||
		!strings.Contains(connectionModel.Detail, "schema_version=47") ||
		!strings.Contains(connectionModel.Detail, "journal_mode=wal") {
		t.Fatalf("connection model = %#v", connectionModel)
	}
	for _, name := range []string{"SQLite", "knowledge retrieval state", "v2 result retention", "v40 result analysis", "recoverable reference index", "external-agent jobs", "external-agent activations", "external-agent result identity"} {
		result := findGateResult(t, report, name)
		if result.Status != doctor.StatusPass {
			t.Fatalf("%s = %#v (%s)", name, result, result.Detail)
		}
	}
	for _, result := range report.Results {
		if result.Status == doctor.StatusSkipped {
			if result.Name == "model API key" {
				continue
			}
			t.Fatalf("unexpected skip at v47: %#v", result)
		}
		if result.Status == doctor.StatusFail {
			t.Fatalf("unexpected failure at v47: %#v (%s)", result, result.Detail)
		}
	}
	if code := report.ExitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if after := sha256File(t, dbPath); after != before {
		t.Fatalf("fixture bytes changed: before=%s after=%s", before, after)
	}
}

type failingRuntimeChecker struct{}

func (failingRuntimeChecker) CheckSQLiteRuntime(context.Context, string) (domain.SQLiteRuntimeHealth, error) {
	return domain.SQLiteRuntimeHealth{}, errors.New("connection probe refused")
}

// TestDoctorFutureSchemaIsFatalWithNoLaterChecks proves the detected > 47
// branch fails the connection-model check fatally, stops every later check,
// and drives exit code 2.
func TestDoctorFutureSchemaIsFatalWithNoLaterChecks(t *testing.T) {
	dbPath := writeDoctorGateFixture(t, 48, false)
	service, err := doctor.New(doctorGateDependencies(dbPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	assertFatalStop(t, service.Run(context.Background(), false), "user_version=48")
}

// TestDoctorUnreadableSchemaIsFatalWithNoLaterChecks proves a failed schema
// read is treated as worse than an old one: fatal stop, exit code 2.
func TestDoctorUnreadableSchemaIsFatalWithNoLaterChecks(t *testing.T) {
	dbPath := writeDoctorGateFixture(t, 41, false)
	service, err := doctor.New(doctorGateDependencies(dbPath, failingRuntimeChecker{}))
	if err != nil {
		t.Fatal(err)
	}
	assertFatalStop(t, service.Run(context.Background(), false), "connection probe refused")
}

func assertFatalStop(t *testing.T, report doctor.Report, wantDetail string) {
	t.Helper()
	result := findGateResult(t, report, "SQLite connection model")
	if result.Status != doctor.StatusFail || !result.Fatal {
		t.Fatalf("connection model = %#v", result)
	}
	if !strings.Contains(result.Detail, wantDetail) {
		t.Fatalf("detail %q missing %q", result.Detail, wantDetail)
	}
	if code := report.ExitCode(); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	last := report.Results[len(report.Results)-1]
	if last.Name != "SQLite connection model" {
		t.Fatalf("checks continued past the fatal branch: %#v", report.Results)
	}
	for _, forbidden := range []string{"SQLite", "external-agent jobs", "recoverable reference index"} {
		for _, result := range report.Results {
			if result.Name == forbidden && &result != &last {
				t.Fatalf("check %q ran after the fatal branch", forbidden)
			}
		}
	}
}
