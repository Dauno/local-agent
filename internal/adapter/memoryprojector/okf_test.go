package memoryprojector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestWriteTopicFileQuotesYAMLMetadata(t *testing.T) {
	dir := t.TempDir()
	topic := domain.Topic{ID: "mem_1", Slug: "safe", Title: "title\n---\nowned: true", Description: "x: [not yaml]", Tags: []string{"x\n---"}, Content: "body", CurrentRev: 1, UpdatedAt: time.Now().UTC()}
	if err := writeTopicFile(dir, topic, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "safe.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `title: "title\n---\nowned: true"`) || !strings.Contains(text, `tags: ["x\n---"]`) {
		t.Fatalf("front matter was not safely quoted:\n%s", text)
	}
}

func TestAtomicWriteRefusesSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "outside")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "topic.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(link, "replace"); err == nil {
		t.Fatal("atomicWrite() overwrote a symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "keep" {
		t.Fatalf("symlink target changed: %q, %v", data, err)
	}
}

func TestRemoveStaleTopicFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.md"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.md"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleTopicFiles(dir, []domain.Topic{{Slug: "keep"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.md")); !os.IsNotExist(err) {
		t.Fatalf("stale topic still exists: %v", err)
	}
}
