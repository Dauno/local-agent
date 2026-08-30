package app_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/app"
	"github.com/Dauno/slack-local-agent/internal/cli"
)

func TestTemplateCommandsRunWithoutProjectArtifacts(t *testing.T) {
	rootDir := t.TempDir()
	application, err := app.New(rootDir, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}

	var output, stderr bytes.Buffer
	command, err := cli.NewRoot(application, cli.Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := cli.Execute(t.Context(), command, []string{"templates", "lint"}, &stderr); code != 0 {
		t.Fatalf("lint exit=%d stderr=%q", code, stderr.String())
	}
	for _, name := range []string{
		"agent.builder", "agent.preview", "confirmation.prompt", "confirmation.resolved",
		"job.accepted", "job.status", "job.status_error", "onboarding.welcome",
	} {
		if !strings.Contains(output.String(), name) {
			t.Fatalf("lint output missing %q:\n%s", name, output.String())
		}
	}
	if got := len(strings.Split(strings.TrimSpace(output.String()), "\n")); got != 8 {
		t.Fatalf("lint line count = %d, want 8:\n%s", got, output.String())
	}

	output.Reset()
	stderr.Reset()
	command, _ = cli.NewRoot(application, cli.Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := cli.Execute(t.Context(), command, []string{"templates", "preview", "agent.preview"}, &stderr); code != 0 {
		t.Fatalf("message preview exit=%d stderr=%q", code, stderr.String())
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &message); err != nil {
		t.Fatalf("message preview is not JSON: %v", err)
	}
	if _, ok := message["blocks"]; !ok {
		t.Fatalf("message preview = %s, want blocks", output.Bytes())
	}

	output.Reset()
	command, _ = cli.NewRoot(application, cli.Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := cli.Execute(t.Context(), command, []string{"templates", "preview", "agent.builder"}, &stderr); code != 0 {
		t.Fatalf("modal preview exit=%d stderr=%q", code, stderr.String())
	}
	var modal map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &modal); err != nil {
		t.Fatalf("modal preview is not JSON: %v", err)
	}
	for _, field := range []string{"type", "title", "callback_id", "blocks"} {
		if _, ok := modal[field]; !ok {
			t.Fatalf("modal preview = %s, missing %q", output.Bytes(), field)
		}
	}

	output.Reset()
	command, _ = cli.NewRoot(application, cli.Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := cli.Execute(t.Context(), command, []string{"templates", "preview", "confirmation.prompt"}, &stderr); code != 0 {
		t.Fatalf("default preview exit=%d stderr=%q", code, stderr.String())
	}
	defaultPreview := output.String()
	output.Reset()
	command, _ = cli.NewRoot(application, cli.Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := cli.Execute(t.Context(), command, []string{"templates", "preview", "confirmation.prompt", "--minimal"}, &stderr); code != 0 {
		t.Fatalf("minimal preview exit=%d stderr=%q", code, stderr.String())
	}
	minimalPreview := output.String()
	if defaultPreview == minimalPreview {
		t.Fatal("minimal and default previews are identical")
	}

	output.Reset()
	stderr.Reset()
	command, _ = cli.NewRoot(application, cli.Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := cli.Execute(t.Context(), command, []string{"templates", "preview", "missing"}, &stderr); code != 1 {
		t.Fatalf("unknown preview exit=%d stderr=%q", code, stderr.String())
	}
	for _, name := range []string{"agent.builder", "agent.preview", "confirmation.prompt", "job.status"} {
		if !strings.Contains(stderr.String(), name) {
			t.Fatalf("unknown preview error missing valid name %q: %s", name, stderr.String())
		}
	}

	entries, err := os.ReadDir(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("template commands created project artifacts: %#v", entries)
	}
}
