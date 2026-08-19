package sqlite

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/doctor"
)

// doctorFixtureJobsChecker is the real composition-root wiring for the doctor
// job-store checks: it opens the database by path, migrates it, and returns
// the bounded content-free aggregates.
type doctorFixtureJobsChecker struct{}

func (doctorFixtureJobsChecker) CheckExternalAgentJobs(ctx context.Context, path string) error {
	store, err := OpenExisting(ctx, path)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.CheckExternalAgentJobStore(ctx)
}

func (doctorFixtureJobsChecker) CheckExternalAgentActivationHealth(ctx context.Context, path string) (domain.ExternalAgentJobActivationHealth, error) {
	store, err := OpenExisting(ctx, path)
	if err != nil {
		return domain.ExternalAgentJobActivationHealth{}, err
	}
	defer store.Close()
	return NewExternalAgentJobStore(store).ActivationHealth(ctx, time.Now().UTC(), 5*time.Minute)
}

func (doctorFixtureJobsChecker) CheckExternalAgentResultIdentityHealth(ctx context.Context, path string) (domain.ExternalAgentJobIdentityHealth, error) {
	store, err := OpenExisting(ctx, path)
	if err != nil {
		return domain.ExternalAgentJobIdentityHealth{}, err
	}
	defer store.Close()
	return NewExternalAgentJobStore(store).IdentityHealth(ctx)
}

type doctorFixtureSecrets struct{}

func (doctorFixtureSecrets) Resolve(keys ...string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		switch key {
		case "DEEPSEEK_API_KEY":
			values[key] = "sk-fixture-model-key"
		case doctor.SlackBotTokenKey:
			values[key] = "xoxb-fixture-bot-token"
		case doctor.SlackAppTokenKey:
			values[key] = "xapp-fixture-app-token"
		default:
			values[key] = "fixture-value"
		}
	}
	return values, nil
}

type doctorFixtureDatabase struct{}

func (doctorFixtureDatabase) CheckDatabase(context.Context, string) error { return nil }

// runDoctorOnFixture copies the fixture database into a fresh project layout
// and runs the offline doctor service against it exactly as the CLI does.
func runDoctorOnFixture(t *testing.T, fixturePath string) doctor.Report {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "local-agent.db"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	deps := doctor.Dependencies{
		ConfigPath: filepath.Join(stateDir, "config.yaml"),
		LoadConfig: func(string) (config.Config, error) { return config.Default(), nil },
		Secrets:    doctorFixtureSecrets{},
		Database:   doctorFixtureDatabase{},
		Jobs:       doctorFixtureJobsChecker{},
	}
	service, err := doctor.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	return service.Run(t.Context(), false)
}

func findDoctorResult(report doctor.Report, name string) (doctor.Result, bool) {
	for _, result := range report.Results {
		if result.Name == name {
			return result, true
		}
	}
	return doctor.Result{}, false
}

// assertDoctorOutputIsContentFree fails when any result name, detail, or
// remediation carries a seeded job ID, actor, team, channel, conversation key,
// digest, or result content value.
func assertDoctorOutputIsContentFree(t *testing.T, report doctor.Report, forbidden []string) {
	t.Helper()
	for _, result := range report.Results {
		combined := result.Name + " " + result.Detail + " " + result.Remediation
		for _, value := range forbidden {
			if strings.Contains(combined, value) {
				t.Fatalf("doctor output leaked %q in result %q: %s", value, result.Name, combined)
			}
		}
	}
}

func buildFreshDoctorFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fresh.db")
	store, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func buildDetachedTerminalNoResultDoctorFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "detached-terminal-no-result.db")
	store, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	for _, terminalStatus := range []string{"failed", "cancelled", "completion_unknown", "abandoned"} {
		jobID := "detached-" + terminalStatus
		insertIdentityJobRow(t, db, jobID, "detached", terminalStatus, "", 0)
		insertIdentityActivationRowWithTerminalStatus(t, db, jobID, "failed", 0, "", terminalStatus)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func buildCurrentIncompleteIdentityDoctorFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "current-incomplete.db")
	store, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	insertIdentityJobRow(t, store.DB(), "current-incomplete", "detached", "completed", "", 0)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// buildUpgradedDoctorFixture reproduces the real v30 -> v31 -> v32 matrix with
// the same rows as the migration tests: foreground activations in every
// claimable state, terminal activations preserved, the foreground inline
// identity repair matrix, and a detached pending activation. The database is
// left at v30; the doctor run performs the real upgrade chain.
func buildUpgradedDoctorFixture(t *testing.T) string {
	t.Helper()
	path, raw := createSchemaAtVersion(t, 30)
	for _, state := range []string{"pending", "processing", "model_started", "response_prepared", "completed", "failed", "completion_unknown"} {
		insertV30JobRow(t, raw, "fg-"+state, "foreground", "completed", "ok", "", "", 0)
		insertV30ActivationRow(t, raw, "fg-"+state, state)
	}
	insertV30JobRow(t, raw, "det-pending", "detached", "completed", "ok", "", "", 0)
	insertV30ActivationRow(t, raw, "det-pending", "pending")
	insertV30LegacyNotification(t, raw, "fg-pending", "OpenCode job `fg-pending` completed.\n\nok")
	// The v30-era helper seeds an ancient retry due; a healthy upgraded store
	// has no overdue legacy rows, so clear it before the doctor run.
	if _, err := raw.ExecContext(context.Background(), `UPDATE external_agent_job_notifications
		SET next_attempt_at = 0 WHERE job_id = 'fg-pending'`); err != nil {
		t.Fatal(err)
	}
	insertV30JobRow(t, raw, "fg-invalid-utf8", "foreground", "completed", "\xff\xfe", "", "", 0)
	insertV30JobRow(t, raw, "detached-inline", "detached", "completed", "d", "", "", 0)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// buildCorruptDoctorFixture seeds a fresh v32 database with the defective
// shapes P2-02 must detect: a completed job without result identity, a
// terminal notification without notification identity, a non-terminal
// foreground activation, an activation without content bytes, and a retired
// foreground activation (expected evidence, never a defect).
func buildCorruptDoctorFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corrupt.db")
	store, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	db := store.DB()
	digest := func(value string) string {
		sum := sha256.Sum256([]byte(value))
		return fmt.Sprintf("%x", sum)
	}
	insertIdentityJobRow(t, db, "corrupt-completed-no-identity", "detached", "completed", "", 0)
	insertIdentityJobRow(t, db, "corrupt-foreground", "foreground", "completed", digest("secret result"), int64(len("secret result")))
	insertIdentityJobRow(t, db, "corrupt-detached", "detached", "completed", digest("secret result"), int64(len("secret result")))
	insertIdentityJobRow(t, db, "corrupt-retired", "foreground", "completed", digest("secret result"), int64(len("secret result")))
	insertIdentityNotificationRow(t, db, "corrupt-detached", strings.Repeat("f", 64), 17, "completed")
	insertIdentityNotificationRow(t, db, "corrupt-completed-no-identity", "", 0, "completed")
	insertIdentityActivationRow(t, db, "corrupt-foreground", "pending", 12, "")
	insertIdentityActivationRow(t, db, "corrupt-detached", "pending", 0, "")
	insertIdentityActivationRow(t, db, "corrupt-retired", "failed", 12, domain.ActivationForegroundRetiredCode)
	// Close before the caller reads the raw file bytes. Under WAL, a recent
	// commit can still live in the sidecar -wal file until checkpoint; Close
	// checkpoints it back into the main file, exactly as every other fixture
	// builder in this file already does.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDoctorResultIdentityFreshFixturePasses(t *testing.T) {
	report := runDoctorOnFixture(t, buildFreshDoctorFixture(t))
	if report.ExitCode() != 0 {
		t.Fatalf("fresh doctor exit code = %d, results = %#v", report.ExitCode(), report.Results)
	}
	result, ok := findDoctorResult(report, "external-agent result identity")
	if !ok || result.Status != doctor.StatusPass {
		t.Fatalf("fresh identity result = %#v, want pass", result)
	}
	assertDoctorOutputIsContentFree(t, report, []string{"U12345678", "T12345678", "D12345678", "slack:"})
}

func TestDoctorDetachedTerminalActivationsWithoutResultPass(t *testing.T) {
	report := runDoctorOnFixture(t, buildDetachedTerminalNoResultDoctorFixture(t))
	if report.ExitCode() != 0 {
		t.Fatalf("detached terminal no-result doctor exit code = %d, results = %#v", report.ExitCode(), report.Results)
	}
	result, ok := findDoctorResult(report, "external-agent result identity")
	if !ok || result.Status != doctor.StatusPass {
		t.Fatalf("detached terminal no-result identity result = %#v, want pass", result)
	}
}

func TestDoctorCurrentCompletedIdentityDoesNotPass(t *testing.T) {
	report := runDoctorOnFixture(t, buildCurrentIncompleteIdentityDoctorFixture(t))
	if report.ExitCode() != 1 {
		t.Fatalf("current incomplete identity doctor exit code = %d, results = %#v", report.ExitCode(), report.Results)
	}
	result, ok := findDoctorResult(report, "external-agent result identity")
	if !ok || result.Status != doctor.StatusFail || !strings.Contains(result.Detail, "1 current completed jobs without result identity") {
		t.Fatalf("current incomplete identity result = %#v", result)
	}
}

func TestDoctorResultIdentityUpgradedFixturePasses(t *testing.T) {
	report := runDoctorOnFixture(t, buildUpgradedDoctorFixture(t))
	if report.ExitCode() != 0 {
		t.Fatalf("upgraded doctor exit code = %d, results = %#v", report.ExitCode(), report.Results)
	}
	result, ok := findDoctorResult(report, "external-agent result identity")
	if !ok || result.Status != doctor.StatusPass {
		t.Fatalf("upgraded identity result = %#v, want pass", result)
	}
	// The real v31 repair retires four foreground activations and leaves the
	// historical fail-closed rows informational: the aggregate must report
	// them without failing the upgrade gate.
	if !strings.Contains(result.Detail, "4 retired foreground activations") {
		t.Fatalf("upgraded identity detail missing retired count: %q", result.Detail)
	}
	if !strings.Contains(result.Detail, "3 historical completed jobs without result identity") {
		t.Fatalf("upgraded identity detail missing historical count: %q", result.Detail)
	}
	if result, ok := findDoctorResult(report, "external-agent activations"); !ok || result.Status != doctor.StatusPass {
		t.Fatalf("upgraded activation health must pass: %#v", result)
	}
	assertDoctorOutputIsContentFree(t, report, []string{
		"fg-pending", "fg-processing", "fg-model_started", "fg-response_prepared", "fg-completed",
		"fg-failed", "fg-completion_unknown", "det-pending", "fg-invalid-utf8", "detached-inline",
		"U12345678", "T12345678", "D12345678", "slack:", "prepared response",
	})
}

func TestDoctorResultIdentityCorruptFixtureFailsContentFree(t *testing.T) {
	report := runDoctorOnFixture(t, buildCorruptDoctorFixture(t))
	if report.ExitCode() != 1 {
		t.Fatalf("corrupt doctor exit code = %d, results = %#v", report.ExitCode(), report.Results)
	}
	result, ok := findDoctorResult(report, "external-agent result identity")
	if !ok || result.Status != doctor.StatusFail {
		t.Fatalf("corrupt identity result = %#v, want fail", result)
	}
	// The defect must be identified by bounded counts, never by values.
	for _, marker := range []string{"1 non-terminal foreground activations (defect)", "1 notifications without identity", "1 activations without content"} {
		if !strings.Contains(result.Detail, marker) {
			t.Fatalf("corrupt identity detail missing %q: %q", marker, result.Detail)
		}
	}
	if !strings.Contains(result.Detail, "1 retired foreground activations") {
		t.Fatalf("corrupt identity detail missing retired count: %q", result.Detail)
	}
	assertDoctorOutputIsContentFree(t, report, []string{
		"corrupt-foreground", "corrupt-detached", "corrupt-retired", "corrupt-completed-no-identity",
		"U12345678", "T12345678", "D12345678", "slack:", "secret result",
	})
}

// TestDoctorResultIdentityUpgradedGateDoesNotCountRetiredAsStuck proves the
// upgraded database is not blocked by the activation health check either:
// retired foreground activations are excluded from stuck.
func TestDoctorResultIdentityUpgradedGateDoesNotCountRetiredAsStuck(t *testing.T) {
	report := runDoctorOnFixture(t, buildUpgradedDoctorFixture(t))
	for _, result := range report.Results {
		if result.Name == "external-agent activations" && result.Status == doctor.StatusFail {
			t.Fatalf("upgraded activation health failed: %#v", result)
		}
		if result.Name == "external-agent jobs" && result.Status == doctor.StatusFail {
			t.Fatalf("upgraded job store failed: %#v", result)
		}
	}
}
