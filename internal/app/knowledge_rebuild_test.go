package app_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/app"
	"github.com/Dauno/slack-local-agent/internal/cli"
	"github.com/Dauno/slack-local-agent/internal/config"
)

// setupInitializedProject runs the real init wizard against a temp project
// root and returns the ready application plus its resolved paths, mirroring
// TestRealCLISetupDoctorManifestAndVersion's setup.
func setupInitializedProject(t *testing.T) (*app.Application, config.Paths) {
	t.Helper()
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
	input := strings.NewReader("\n\n\nxoxb-token\nxapp-token\nU12345678\n\n\n\n\nmodel-key\ny\n")
	var output, stderr bytes.Buffer
	command, err := cli.NewRoot(application, cli.Streams{In: input, Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := cli.Execute(t.Context(), command, []string{"init"}, &stderr); code != 0 {
		t.Fatalf("init exit=%d stderr=%s output=%s", code, stderr.String(), output.String())
	}
	paths, err := config.Default().ResolvePaths(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	return application, paths
}

// TestKnowledgeRebuildIndexRequiresKnowledgeGate pins hallazgo 12's bounded
// scope: rebuild-index refuses to run when orchestration.knowledge.enabled
// is off (the default), with an actionable error rather than a silent
// no-op.
func TestKnowledgeRebuildIndexRequiresKnowledgeGate(t *testing.T) {
	application, _ := setupInitializedProject(t)
	var output, stderr bytes.Buffer
	command, err := cli.NewRoot(application, cli.Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	code := cli.Execute(t.Context(), command, []string{"knowledge", "rebuild-index"}, &stderr)
	if code == 0 {
		t.Fatalf("rebuild-index succeeded with the knowledge gate disabled: %s", output.String())
	}
	if !strings.Contains(stderr.String(), "orchestration.knowledge.enabled") {
		t.Fatalf("stderr = %q, want an actionable knowledge-gate message", stderr.String())
	}
}

// TestKnowledgeRebuildIndexRebuildsLexicalIndex pins hallazgo 12: with the
// knowledge gate on, the operator has a bounded, non-destructive path to
// rebuild the reconstructible lexical index, and the command reports what
// it did without ever mentioning --reset-state.
func TestKnowledgeRebuildIndexRebuildsLexicalIndex(t *testing.T) {
	application, paths := setupInitializedProject(t)
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Orchestration.Knowledge.Enabled = true
	if err := config.Save(paths.ConfigFile, cfg); err != nil {
		t.Fatal(err)
	}
	var output, stderr bytes.Buffer
	command, err := cli.NewRoot(application, cli.Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := cli.Execute(t.Context(), command, []string{"knowledge", "rebuild-index"}, &stderr); code != 0 {
		t.Fatalf("rebuild-index exit=%d stderr=%s output=%s", code, stderr.String(), output.String())
	}
	if !strings.Contains(output.String(), "Lexical index cleared and re-enqueued") {
		t.Fatalf("output = %q, want lexical rebuild confirmation", output.String())
	}
	if !strings.Contains(output.String(), "Embedding index:") {
		t.Fatalf("output = %q, want an explicit embedding-index status line", output.String())
	}
	if strings.Contains(output.String(), "--reset-state") {
		t.Fatalf("output = %q, must never suggest the destructive reset", output.String())
	}

	// Running it again must succeed identically: rebuild is idempotent.
	output.Reset()
	command, _ = cli.NewRoot(application, cli.Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := cli.Execute(t.Context(), command, []string{"knowledge", "rebuild-index"}, &stderr); code != 0 {
		t.Fatalf("second rebuild-index exit=%d stderr=%s", code, stderr.String())
	}
}
