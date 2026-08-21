package app_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/app"
	"github.com/Dauno/slack-local-agent/internal/cli"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/usecase/doctor"
)

func TestRealCLISetupDoctorManifestAndVersion(t *testing.T) {
	clearEnvironment(t, "DEEPSEEK_API_KEY", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN")
	rootDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	application, err := app.New(rootDir, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}

	const (
		botToken = "xoxb-integration-token"
		appToken = "xapp-integration-token"
		modelKey = "integration-model-key"
	)
	input := strings.NewReader("\n\n\n" + botToken + "\n" + appToken + "\nU12345678\n\n\n\n\n" + modelKey + "\ny\n")
	var output, stderr bytes.Buffer
	command, err := cli.NewRoot(application, cli.Streams{In: input, Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := cli.Execute(t.Context(), command, []string{"init"}, &stderr); code != 0 {
		t.Fatalf("init exit=%d stderr=%s output=%s", code, stderr.String(), output.String())
	}
	for _, secret := range []string{botToken, appToken, modelKey} {
		if strings.Contains(output.String(), secret) || strings.Contains(stderr.String(), secret) {
			t.Fatalf("setup output leaked %q", secret)
		}
	}

	paths, err := config.Default().ResolvePaths(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.ConfigFile, paths.ManifestFile, paths.EnvExampleFile, paths.DatabaseFile, paths.EnvFile} {
		if info, statErr := os.Stat(path); statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("artifact %s: info=%v err=%v", path, info, statErr)
		}
	}
	if info, _ := os.Stat(paths.EnvFile); info.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode=%04o", info.Mode().Perm())
	}
	ignore, err := os.ReadFile(filepath.Join(rootDir, ".gitignore"))
	if err != nil || string(ignore) != ".env\n" {
		t.Fatalf(".gitignore=%q err=%v", ignore, err)
	}
	loaded, err := config.Load(paths.ConfigFile)
	if err != nil || len(loaded.Slack.AllowedUserIDs) != 1 || loaded.Slack.AllowedUserIDs[0] != "U12345678" {
		t.Fatalf("loaded config=%#v err=%v", loaded.Slack, err)
	}

	output.Reset()
	stderr.Reset()
	command, _ = cli.NewRoot(application, cli.Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := cli.Execute(t.Context(), command, []string{"doctor"}, &stderr); code != 0 {
		t.Fatalf("doctor exit=%d stderr=%s output=%s", code, stderr.String(), output.String())
	}
	if !strings.Contains(output.String(), "PASS configuration") || !strings.Contains(output.String(), "PASS SQLite") || !strings.Contains(output.String(), "PASS external-agent activations") {
		t.Fatalf("doctor output=%s", output.String())
	}

	output.Reset()
	command, _ = cli.NewRoot(application, cli.Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := cli.Execute(t.Context(), command, []string{"manifest"}, &stderr); code != 0 {
		t.Fatalf("manifest exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(output.String(), "socket_mode_enabled: true") || !strings.Contains(output.String(), "connections:write") {
		t.Fatalf("manifest output=%s", output.String())
	}
	if err := os.WriteFile(paths.ManifestFile, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	command, _ = cli.NewRoot(application, cli.Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := cli.Execute(t.Context(), command, []string{"manifest", "--write"}, &stderr); code != 0 {
		t.Fatalf("manifest --write exit=%d stderr=%s", code, stderr.String())
	}
	written, err := os.ReadFile(paths.ManifestFile)
	if err != nil || !strings.Contains(string(written), "socket_mode_enabled: true") || strings.Contains(string(written), "stale") {
		t.Fatalf("written manifest=%q err=%v", written, err)
	}

	output.Reset()
	command, _ = cli.NewRoot(application, cli.Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := cli.Execute(t.Context(), command, []string{"version"}, &stderr); code != 0 || !strings.Contains(output.String(), "go1.25") {
		t.Fatalf("version exit=%d output=%s", code, output.String())
	}
}

// TestApplicationDoctorExposesSQLiteRuntimeAndRecoverableReferenceHealth
// runs Application.Doctor(ctx, false) itself, through the real composition
// wired in application.go, over a temporary initialized project. It does
// not inspect doctor.Dependencies as a struct: it observes the two new
// checkpoint-6 results in the returned report (TRD 08 checkpoint 6, item 2).
func TestApplicationDoctorExposesSQLiteRuntimeAndRecoverableReferenceHealth(t *testing.T) {
	clearEnvironment(t, "DEEPSEEK_API_KEY", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN")
	rootDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	application, err := app.New(rootDir, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}

	input := strings.NewReader("\n\n\nxoxb-integration-token\nxapp-integration-token\nU12345678\n\n\n\n\nintegration-model-key\ny\n")
	var output, stderr bytes.Buffer
	command, err := cli.NewRoot(application, cli.Streams{In: input, Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := cli.Execute(t.Context(), command, []string{"init"}, &stderr); code != 0 {
		t.Fatalf("init exit=%d stderr=%s output=%s", code, stderr.String(), output.String())
	}

	report, err := application.Doctor(t.Context(), false)
	if err != nil {
		t.Fatalf("Doctor() = %v", err)
	}

	runtimeResult, ok := findDoctorResult(report, "SQLite connection model")
	if !ok || runtimeResult.Status != doctor.StatusPass {
		t.Fatalf("SQLite connection model result = %#v", runtimeResult)
	}
	for _, want := range []string{"schema_version=", "journal_mode=wal", "synchronous=2", "busy_timeout_ms=5000", "foreign_keys=true", "max_open_connections=4"} {
		if !strings.Contains(runtimeResult.Detail, want) {
			t.Fatalf("SQLite connection model detail %q missing %q", runtimeResult.Detail, want)
		}
	}

	referenceResult, ok := findDoctorResult(report, "recoverable reference index")
	if !ok || referenceResult.Status != doctor.StatusPass {
		t.Fatalf("recoverable reference index result = %#v", referenceResult)
	}
	if !strings.Contains(referenceResult.Detail, "total_ref_rows=") {
		t.Fatalf("recoverable reference index detail = %q", referenceResult.Detail)
	}
}

func findDoctorResult(report doctor.Report, name string) (doctor.Result, bool) {
	for _, result := range report.Results {
		if result.Name == name {
			return result, true
		}
	}
	return doctor.Result{}, false
}

func TestRunDoesNotBootstrapMissingProject(t *testing.T) {
	rootDir := t.TempDir()
	application, err := app.New(rootDir, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	err = application.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "local-agent init") {
		t.Fatalf("Run() error=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(rootDir, config.DefaultProjectStateDir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Run bootstrapped local state: %v", statErr)
	}
}

func clearEnvironment(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		value, existed := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		restoreKey, restoreValue, restoreExisted := key, value, existed
		t.Cleanup(func() {
			if restoreExisted {
				_ = os.Setenv(restoreKey, restoreValue)
			} else {
				_ = os.Unsetenv(restoreKey)
			}
		})
	}
}
