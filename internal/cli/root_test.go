package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/usecase/bootstrap"
	"github.com/Dauno/slack-local-agent/internal/usecase/doctor"
)

type fakeBackend struct {
	snapshot        bootstrap.Snapshot
	setupSecrets    bootstrap.Secrets
	prepareErr      error
	applyErr        error
	runErr          error
	manifestErr     error
	doctorErr       error
	report          doctor.Report
	manifestContent string
	manifestPath    string
	applyCalls      int
	prepared        int
	identity        bootstrap.Identity
	access          bootstrap.AccessControl
	secrets         bootstrap.Secrets
	applyHook       func()
}

func (f *fakeBackend) PrepareSetup(context.Context) (bootstrap.Snapshot, bootstrap.Secrets, error) {
	f.prepared++
	return f.snapshot, f.setupSecrets, f.prepareErr
}
func (f *fakeBackend) ApplySetup(_ context.Context, _ bootstrap.Snapshot, identity bootstrap.Identity, access bootstrap.AccessControl, secrets bootstrap.Secrets) error {
	f.applyCalls++
	f.identity, f.access, f.secrets = identity, access, secrets
	if f.applyHook != nil {
		f.applyHook()
	}
	return f.applyErr
}
func (f *fakeBackend) Doctor(context.Context, bool) (doctor.Report, error) {
	return f.report, f.doctorErr
}
func (f *fakeBackend) Run(context.Context) error { return f.runErr }
func (f *fakeBackend) Manifest(context.Context, bool) (string, string, error) {
	return f.manifestContent, f.manifestPath, f.manifestErr
}
func (f *fakeBackend) ResetState(context.Context) error { return nil }
func (*fakeBackend) Version() string                     { return "local-agent test-version" }

func setupBackend() *fakeBackend {
	return &fakeBackend{snapshot: bootstrap.Snapshot{Config: config.Default()}}
}

func TestInitWizardCompletesNineStepsWithoutLeakingSecrets(t *testing.T) {
	const (
		botToken = "xoxb-123456789-secret"
		appToken = "xapp-123456789-secret"
		modelKey = "model-api-secret"
	)
	input := strings.NewReader("\n\n\n" + botToken + "\n" + appToken + "\nU12345678\n\n\n\n\n" + modelKey + "\ny\n")
	var output, stderr bytes.Buffer
	backend := setupBackend()
	privacyVisibleAtApply := false
	backend.applyHook = func() {
		privacyVisibleAtApply = strings.Contains(output.String(), "Aviso de privacidad")
	}
	root, err := NewRoot(backend, Streams{In: input, Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := Execute(t.Context(), root, []string{"init"}, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s output=%s", code, stderr.String(), output.String())
	}
	if backend.prepared != 1 || backend.applyCalls != 1 || !privacyVisibleAtApply {
		t.Fatalf("prepare=%d apply=%d privacy-before-apply=%v", backend.prepared, backend.applyCalls, privacyVisibleAtApply)
	}
	if backend.identity.AgentName != "Dev Agent" || len(backend.access.AllowedUserIDs) != 1 || backend.access.AllowedUserIDs[0] != "U12345678" {
		t.Fatalf("unexpected confirmed setup: identity=%#v access=%#v", backend.identity, backend.access)
	}
	if backend.access.ContextEnabled {
		t.Fatalf("context enrichment unexpectedly enabled: %#v", backend.access)
	}
	if !strings.Contains(output.String(), "Contexto Slack opcional") {
		t.Fatalf("context privacy disclosure missing: %s", output.String())
	}
	if backend.secrets.ModelAPIKey != modelKey || backend.secrets.SlackBotToken != botToken || backend.secrets.SlackAppToken != appToken {
		t.Fatal("confirmed secrets were not passed to bootstrap")
	}
	for _, secret := range []string{botToken, appToken, modelKey} {
		if strings.Contains(output.String(), secret) || strings.Contains(stderr.String(), secret) {
			t.Fatalf("secret leaked in CLI output: %q", secret)
		}
	}
	for step := 1; step <= 9; step++ {
		if !strings.Contains(output.String(), "["+string(rune('0'+step))+"/9]") {
			t.Fatalf("wizard output missing step %d:\n%s", step, output.String())
		}
	}
	for _, command := range []string{"local-agent doctor", "local-agent doctor --live", "local-agent run"} {
		if !strings.Contains(output.String(), command) {
			t.Fatalf("next steps missing %q", command)
		}
	}
}

func TestInitCancellationKeepsBaseArtifactsWithoutApplying(t *testing.T) {
	backend := setupBackend()
	backend.setupSecrets = bootstrap.Secrets{
		ModelAPIKey: "existing-model", SlackBotToken: "xoxb-existing-token", SlackAppToken: "xapp-existing-token",
	}
	input := strings.NewReader(strings.Repeat("\n", 11) + "n\n")
	var output, stderr bytes.Buffer
	root, _ := NewRoot(backend, Streams{In: input, Out: &output, Err: &stderr})
	if code := Execute(t.Context(), root, []string{"init"}, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if backend.prepared != 1 || backend.applyCalls != 0 {
		t.Fatalf("prepare=%d apply=%d", backend.prepared, backend.applyCalls)
	}
	if !strings.Contains(output.String(), "artefactos base") {
		t.Fatalf("cancellation message missing: %s", output.String())
	}
}

func TestCommandExitCodes(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		backend func() *fakeBackend
		code    int
	}{
		{
			name: "doctor check failure", args: []string{"doctor"}, code: 1,
			backend: func() *fakeBackend {
				b := setupBackend()
				b.report.Results = []doctor.Result{{Name: "configuration", Status: doctor.StatusFail, Detail: "missing"}}
				return b
			},
		},
		{
			name: "operational run failure", args: []string{"run"}, code: 1,
			backend: func() *fakeBackend { b := setupBackend(); b.runErr = errors.New("not configured"); return b },
		},
		{
			name: "invalid usage", args: []string{"doctor", "extra"}, code: 2,
			backend: setupBackend,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output, stderr bytes.Buffer
			root, _ := NewRoot(tt.backend(), Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
			if got := Execute(t.Context(), root, tt.args, &stderr); got != tt.code {
				t.Fatalf("exit=%d want=%d stderr=%s", got, tt.code, stderr.String())
			}
		})
	}
}

func TestManifestAndVersionOutput(t *testing.T) {
	backend := setupBackend()
	backend.manifestContent = "settings:\n  socket_mode_enabled: true\n"
	backend.manifestPath = "/tmp/manifest.yaml"
	var output, stderr bytes.Buffer
	root, _ := NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := Execute(t.Context(), root, []string{"manifest"}, &stderr); code != 0 || output.String() != backend.manifestContent {
		t.Fatalf("manifest exit=%d output=%q", code, output.String())
	}

	output.Reset()
	root, _ = NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := Execute(t.Context(), root, []string{"version"}, &stderr); code != 0 || !strings.Contains(output.String(), "test-version") {
		t.Fatalf("version exit=%d output=%q", code, output.String())
	}
}
