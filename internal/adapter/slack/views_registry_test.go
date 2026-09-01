package slack

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNewViewEngineRegistersEveryEmbeddedTemplate(t *testing.T) {
	engine, err := NewViewEngine()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"agent.builder", "agent.preview", "confirmation.prompt", "confirmation.resolved",
		"job.accepted", "job.status", "job.status_error", "onboarding.welcome",
	}
	if got := engine.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("complete template names = %#v, want %#v", got, want)
	}
}

func TestCompleteViewEngineRejectsAnUnlinkedTemplate(t *testing.T) {
	root := copyEmbeddedTemplates(t)
	unlinked := filepath.Join(root, "templates", "unlinked.json")
	content := `{"schema_version":2,"surface":"message","contract":{"inputs":{},"actions":{}},"layout":[{"type":"divider"}]}`
	if err := os.WriteFile(unlinked, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newCompleteViewEngine(os.DirFS(root)); err == nil || !strings.Contains(err.Error(), `template "unlinked" has no linked view`) {
		t.Fatalf("newCompleteViewEngine() error = %v, want unlinked template error", err)
	}
}

func TestCompleteViewEngineRejectsAViewContractMismatch(t *testing.T) {
	root := copyEmbeddedTemplates(t)
	path := filepath.Join(root, "templates", "agent", "preview.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), `"sha256": {"type": "text"`, `"sha256": {"type": "number"`, 1)
	if changed == string(data) {
		t.Fatal("preview sha256 input was not found")
	}
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newCompleteViewEngine(os.DirFS(root)); err == nil || !strings.Contains(err.Error(), "does not match number") {
		t.Fatalf("newCompleteViewEngine() error = %v, want binding mismatch", err)
	}
}

func copyEmbeddedTemplates(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := fs.WalkDir(viewsFS, "views/templates", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative := strings.TrimPrefix(path, "views/")
		target := filepath.Join(root, filepath.FromSlash(relative))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := fs.ReadFile(viewsFS, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	return root
}
