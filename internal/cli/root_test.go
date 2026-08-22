package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
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
	resetCalls      int
	prepared        int
	identity        bootstrap.Identity
	access          bootstrap.AccessControl
	secrets         bootstrap.Secrets
	applyHook       func()
}

type inspectionBackend struct {
	*fakeBackend
	view *domain.ExternalAgentJobInspection
}

type reconciliationBackend struct {
	*fakeBackend
	view     domain.ExternalAgentJobStatusView
	calls    int
	expected int
}

func (b *inspectionBackend) InspectJob(context.Context, string) (*domain.ExternalAgentJobInspection, error) {
	return b.view, nil
}

func (b *reconciliationBackend) ReconcileJob(_ context.Context, _ string, expectedRevision int) (domain.ExternalAgentJobStatusView, error) {
	b.calls++
	b.expected = expectedRevision
	return b.view, nil
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
func (f *fakeBackend) ResetState(context.Context) error { f.resetCalls++; return nil }
func (*fakeBackend) Version() string                    { return "local-agent test-version" }

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

// TestDoctorRendersSkipWithoutChangingExitCode proves the SKIP label renders
// alongside PASS/FAIL and that a skipped check is inert for the exit code.
func TestDoctorRendersSkipWithoutChangingExitCode(t *testing.T) {
	b := setupBackend()
	b.report.Results = []doctor.Result{
		{Name: "SQLite connection model", Status: doctor.StatusPass, Detail: "schema v41"},
		{Name: "v40 result analysis", Status: doctor.StatusSkipped, Detail: "requires schema v40, database is v33"},
	}
	var output, stderr bytes.Buffer
	root, _ := NewRoot(b, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := Execute(t.Context(), root, []string{"doctor"}, &stderr); code != 0 {
		t.Fatalf("exit=%d want=0", code)
	}
	rendered := output.String()
	if !strings.Contains(rendered, "PASS SQLite connection model") {
		t.Fatalf("PASS line missing: %q", rendered)
	}
	if line := "SKIP v40 result analysis"; !strings.Contains(rendered, line) ||
		!strings.Contains(rendered, "requires schema v40, database is v33") {
		t.Fatalf("SKIP line missing: %q", rendered)
	}
	if strings.Contains(rendered, "\nFAIL ") {
		t.Fatalf("a skip must not render as FAIL: %q", rendered)
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

func TestInitResetStateRequiresConfirmation(t *testing.T) {
	for _, tt := range []struct {
		name      string
		input     string
		wantCalls int
	}{
		{name: "declined", input: "n\n", wantCalls: 0},
		{name: "confirmed", input: "y\n", wantCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := setupBackend()
			var output, stderr bytes.Buffer
			root, err := NewRoot(backend, Streams{In: strings.NewReader(tt.input), Out: &output, Err: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			if code := Execute(t.Context(), root, []string{"init", "--reset-state"}, &stderr); code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, stderr.String())
			}
			if backend.resetCalls != tt.wantCalls {
				t.Fatalf("ResetState calls = %d, want %d", backend.resetCalls, tt.wantCalls)
			}
		})
	}
}

func TestJobsInspectPrintsOnlySafeDeliveryFields(t *testing.T) {
	backend := &inspectionBackend{
		fakeBackend: setupBackend(),
		view: &domain.ExternalAgentJobInspection{
			JobID: "job_123", Status: domain.JobCompleted, StatusRevision: 4,
			Deliveries: []domain.ExternalAgentJobDeliveryInspection{{
				StatusRevision: 4, NotificationKind: domain.JobNotificationTerminal,
				DeliveryMode: domain.JobResultDeliveryFile, PublishState: domain.NotificationPublished,
				Attempts: 2, LastErrorCode: "notification_publish_ambiguous",
				LeaseOwner:        "worker-secret",
				LeaseOwnerPresent: true, LeaseExpiry: time.Date(2026, 8, 1, 12, 1, 0, 0, time.UTC),
				RecoveredSlackTS: "1710000000.000001", UploadState: domain.JobResultUploadCompleted,
				SlackFileIDPresent: true,
			}},
		},
	}
	var output, stderr bytes.Buffer
	root, err := NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := Execute(t.Context(), root, []string{"jobs", "inspect", "job_123"}, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	text := output.String()
	for _, expected := range []string{"status: completed", "status_revision: 4", "delivery_revision: 4", "delivery_mode: file", "notification_kind: terminal", "publish_state: published", "attempts: 2", "lease_owner: worker-secret", "lease_owner_present: true", "lease_expiry: 2026-08-01T12:01:00Z", "last_error_code: notification_publish_ambiguous", "upload_state: completed", "recovered_slack_ts: 1710000000.000001"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("output missing %q: %s", expected, text)
		}
	}
	encoded, err := json.Marshal(backend.view)
	if err != nil || !strings.Contains(string(encoded), `"lease_owner":"worker-secret"`) {
		t.Fatalf("JSON inspection missing lease owner: %s (err=%v)", encoded, err)
	}
	for _, forbidden := range []string{"task", "result text", "artifact", "U123", "slack:T"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Fatalf("unsafe field %q in output: %s", forbidden, text)
		}
	}
}

func TestJobsInspectMissingJobReturnsSafeResult(t *testing.T) {
	backend := &inspectionBackend{fakeBackend: setupBackend()}
	var output, stderr bytes.Buffer
	root, _ := NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := Execute(t.Context(), root, []string{"jobs", "inspect", "job_missing"}, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if output.String() != "job: not found\n" || stderr.Len() != 0 {
		t.Fatalf("safe missing output=%q stderr=%q", output.String(), stderr.String())
	}
}

func TestJobsInspectRendersSessionAndProgress(t *testing.T) {
	base := time.Date(2026, 8, 4, 2, 41, 2, 0, time.UTC)
	backend := &inspectionBackend{fakeBackend: setupBackend(), view: &domain.ExternalAgentJobInspection{
		JobID: "job_bb3ed6", Status: domain.JobRunning, StatusRevision: 1,
		ACPSessionID: "ses_full_identity_0123456789", Phase: domain.ACPPhaseToolRunning,
		Health: domain.ACPHealthPossiblyStalled, LastEventKind: domain.ACPEventToolCallUpdate,
		LastTransportActivityAt: base, PromptElapsedSeconds: 3780,
		ActiveToolCount: 1, PendingPermission: true, StopReason: "",
	}}
	var output, stderr bytes.Buffer
	root, _ := NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := Execute(t.Context(), root, []string{"jobs", "inspect", "job_bb3ed6"}, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	text := output.String()
	for _, want := range []string{
		"job_id: job_bb3ed6",
		"acp_session_id: ses_full_identity_0123456789",
		"phase: tool_running",
		"health: possibly_stalled",
		"last_event: tool_call_update",
		"prompt_elapsed: 1h 03m",
		"active_tools: 1",
		"pending_permission: true",
		"process: unknown",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("inspection output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "task") || strings.Contains(text, "result") {
		t.Fatalf("inspection output leaked unsafe content:\n%s", text)
	}
}

func TestJobsInspectRendersPendingSession(t *testing.T) {
	backend := &inspectionBackend{fakeBackend: setupBackend(), view: &domain.ExternalAgentJobInspection{
		JobID: "job_queued", Status: domain.JobQueued, StatusRevision: 0,
	}}
	var output, stderr bytes.Buffer
	root, _ := NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := Execute(t.Context(), root, []string{"jobs", "inspect", "job_queued"}, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(output.String(), "acp_session_id: pending") {
		t.Fatalf("queued output missing pending session:\n%s", output.String())
	}
}

func TestJobsReconcileRendersSessionID(t *testing.T) {
	backend := &reconciliationBackend{fakeBackend: setupBackend(), view: domain.ExternalAgentJobStatusView{
		JobID: "job_123", Status: domain.JobCompleted, StatusRevision: 5,
		ResultAvailable: true, ACPSessionID: "ses_reconciled_identity",
		ResultSHA256: strings.Repeat("a", 64), ResultBytes: 1024, DeliveryMode: domain.JobResultDeliveryFile,
	}}
	var output, stderr bytes.Buffer
	root, _ := NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := Execute(t.Context(), root, []string{"jobs", "reconcile", "job_123", "--expect-revision", "5", "--confirm"}, &stderr); code != 0 {
		t.Fatalf("confirmed exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(output.String(), "acp_session_id: ses_reconciled_identity") {
		t.Fatalf("reconcile output missing session ID:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "result_available: true") {
		t.Fatalf("reconcile output missing strict result availability:\n%s", output.String())
	}
}

func TestJobsReconcileRendersStrictResultAvailability(t *testing.T) {
	view := domain.ExternalAgentJobStatusView{JobID: "job_123", Status: domain.JobCompleted, StatusRevision: 5}
	for _, tt := range []struct {
		name string
		view domain.ExternalAgentJobStatusView
		want string
	}{
		{
			name: "incomplete identity promises no result",
			view: view,
			want: "result_available: false",
		},
		{
			name: "complete identity projects the result",
			view: domain.ExternalAgentJobStatusView{JobID: "job_123", Status: domain.JobCompleted, StatusRevision: 5,
				ResultAvailable: true, ResultSHA256: strings.Repeat("a", 64), ResultBytes: 1024, DeliveryMode: domain.JobResultDeliveryMarkdown},
			want: "result_available: true",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := &reconciliationBackend{fakeBackend: setupBackend(), view: tt.view}
			var output, stderr bytes.Buffer
			root, _ := NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
			if code := Execute(t.Context(), root, []string{"jobs", "reconcile", "job_123", "--expect-revision", "5", "--confirm"}, &stderr); code != 0 {
				t.Fatalf("confirmed exit=%d stderr=%q", code, stderr.String())
			}
			if !strings.Contains(output.String(), tt.want) {
				t.Fatalf("reconcile output missing %q:\n%s", tt.want, output.String())
			}
		})
	}
}

func TestJobsReconcileRequiresConfirmationAndRevision(t *testing.T) {
	backend := &reconciliationBackend{fakeBackend: setupBackend(), view: domain.ExternalAgentJobStatusView{JobID: "job_123", Status: domain.JobCompleted, StatusRevision: 5}}
	var output, stderr bytes.Buffer
	root, err := NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := Execute(t.Context(), root, []string{"jobs", "reconcile", "job_123", "--expect-revision", "4"}, &stderr); code != 1 || backend.calls != 0 {
		t.Fatalf("missing confirmation exit=%d calls=%d stderr=%q", code, backend.calls, stderr.String())
	}
	output.Reset()
	stderr.Reset()
	root, err = NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := Execute(t.Context(), root, []string{"jobs", "reconcile", "job_123", "--expect-revision", "4", "--confirm"}, &stderr); code != 0 {
		t.Fatalf("confirmed exit=%d stderr=%q", code, stderr.String())
	}
	if backend.calls != 1 || backend.expected != 4 || !strings.Contains(output.String(), "status_revision: 5") || strings.Contains(output.String(), "task") {
		t.Fatalf("reconcile output=%q calls=%d expected=%d", output.String(), backend.calls, backend.expected)
	}
}
