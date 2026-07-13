package memoryprojector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestWriteTopicFileQuotesYAMLMetadata(t *testing.T) {
	dir := t.TempDir()
	topic := domain.Topic{ID: "mem_1", Slug: "safe", Title: "title\n---\nowned: true", Description: "x: [not yaml]", Tags: []string{"x\n---"}, Content: "body", BundlePath: "topics", CurrentRev: 1, UpdatedAt: time.Now().UTC()}
	if err := writeTopicFile(dir, topic, nil, nil, nil, nil, nil); err != nil {
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

func TestRemoveStaleOKFFiles(t *testing.T) {
	dir := t.TempDir()
	topicsDir := filepath.Join(dir, "topics")
	if err := os.MkdirAll(topicsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(topicsDir, "old.md"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	keepDir := filepath.Join(dir, "facts")
	if err := os.MkdirAll(keepDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keepDir, "keep.md"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleOKFFiles(dir, []domain.Topic{{Slug: "keep", BundlePath: "facts"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(topicsDir, "old.md")); !os.IsNotExist(err) {
		t.Fatalf("stale topic still exists: %v", err)
	}
}

func TestWriteOKFLogGroupsByDate(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	topic := domain.Topic{ID: "mem_1", Slug: "test", Title: "Test", BundlePath: "facts", CurrentRev: 2, UpdatedAt: now}
	snapshot := port.ProjectionSnapshot{
		Topics: []domain.Topic{topic},
		Revisions: map[domain.TopicID][]domain.TopicRevision{
			"mem_1": {
				{ID: 1, TopicID: "mem_1", RevisionNumber: 1, Content: "rev1", ChangeReason: "first", CreatedAt: now.Add(-48 * time.Hour)},
				{ID: 2, TopicID: "mem_1", RevisionNumber: 2, Content: "rev2", ChangeReason: "second", CreatedAt: now},
			},
		},
	}
	if err := writeOKFLog(dir, snapshot); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "log.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "## "+now.Format("2006-01-02")) {
		t.Fatalf("log missing date heading:\n%s", text)
	}
	if strings.Contains(text, "---") {
		t.Fatalf("log must not have frontmatter:\n%s", text)
	}
}

func TestNestedIndexHasNoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	topic := domain.Topic{ID: "mem_1", Slug: "test", Title: "Test", BundlePath: "facts", CurrentRev: 1, UpdatedAt: time.Now().UTC()}
	snapshot := port.ProjectionSnapshot{Topics: []domain.Topic{topic}}
	if err := renderBundle(dir, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, indexPath := range []string{"facts/index.md", "index.md"} {
		data, err := os.ReadFile(filepath.Join(dir, indexPath))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if indexPath == "facts/index.md" {
			if strings.Contains(text, "okf_version") {
				t.Fatalf("nested index %q must not have okf_version:\n%s", indexPath, text)
			}
			if strings.Contains(text, "---") {
				t.Fatalf("nested index %q must not have frontmatter:\n%s", indexPath, text)
			}
		}
	}
}

func TestTopicLinksUseBundlePaths(t *testing.T) {
	dir := t.TempDir()
	a := domain.Topic{ID: "mem_a", Slug: "alpha", Title: "Alpha", BundlePath: "projects", CurrentRev: 1, UpdatedAt: time.Now().UTC()}
	b := domain.Topic{ID: "mem_b", Slug: "beta", Title: "Beta", BundlePath: "facts", CurrentRev: 1, UpdatedAt: time.Now().UTC()}
	topicByID := map[domain.TopicID]domain.Topic{"mem_a": a, "mem_b": b}
	links := []domain.TopicLink{
		{SourceTopicID: "mem_a", TargetTopicID: "mem_b", Relation: "related"},
	}
	if err := writeTopicFile(dir, a, nil, links, nil, topicByID, []domain.Topic{a, b}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "alpha.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "/facts/beta.md") {
		t.Fatalf("topic link missing bundle path:\n%s", text)
	}
	if strings.Contains(text, "mem_b") {
		t.Fatalf("topic link exposed internal ID:\n%s", text)
	}
}

func TestEvidenceShownAsProvenance(t *testing.T) {
	dir := t.TempDir()
	topic := domain.Topic{ID: "mem_x", Slug: "test", Title: "Test", BundlePath: "facts", CurrentRev: 1, UpdatedAt: time.Now().UTC()}
	evidence := []domain.Evidence{
		{ID: 1, TopicRevision: 1, SourceKey: "slack:T:dm:D", SourceTS: "1", AuthorID: "U1", Type: domain.EvidenceSource},
	}
	if err := writeTopicFile(dir, topic, nil, nil, evidence, nil, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "test.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "# Provenance") {
		t.Fatalf("evidence missing provenance section:\n%s", text)
	}
	if strings.Contains(text, "# Citations") {
		t.Fatalf("evidence must not use Citations without resolvable URIs:\n%s", text)
	}
}

func TestWriteTopicFileRejectsInvalidUTF8(t *testing.T) {
	dir := t.TempDir()
	topic := domain.Topic{ID: "mem_1", Slug: "invalid", Title: "Valid", BundlePath: "facts", Content: "abc\xfe\xfe", CurrentRev: 1, UpdatedAt: time.Now().UTC()}
	snapshot := port.ProjectionSnapshot{Topics: []domain.Topic{topic}}
	err := renderBundle(dir, snapshot)
	if err == nil {
		t.Fatal("renderBundle accepted invalid UTF-8 content")
	}
}

func TestBundleStagingKeepsPriorBundleOnFailure(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "memory")
	outputDir := filepath.Join(bundleDir, "output")
	// Create a real prior bundle
	if err := makeSafeDir(outputDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "existing.md"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Project with a topic that has invalid UTF-8 will fail
	reader := &stubProjectionReader{
		snapshot: port.ProjectionSnapshot{
			Topics: []domain.Topic{{ID: "mem_1", Slug: "bad", Title: "Bad", BundlePath: "facts", Content: "\xff\xfe", CurrentRev: 1, UpdatedAt: time.Now().UTC()}},
		},
	}
	p := New()
	err := p.Project(t.Context(), reader, outputDir)
	if err == nil {
		t.Fatal("Project accepted invalid UTF-8")
	}
	// Prior bundle must still be intact
	data, readErr := os.ReadFile(filepath.Join(outputDir, "existing.md"))
	if readErr != nil {
		t.Fatalf("prior bundle destroyed on failed projection: %v", readErr)
	}
	if string(data) != "existing" {
		t.Fatalf("prior bundle content changed: %q", string(data))
	}
}

type stubProjectionReader struct {
	snapshot port.ProjectionSnapshot
}

func (r *stubProjectionReader) ReadProjectionSnapshot(_ context.Context) (port.ProjectionSnapshot, error) {
	return r.snapshot, nil
}
