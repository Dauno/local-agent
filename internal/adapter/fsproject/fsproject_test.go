package fsproject

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/envfile"
	"github.com/joho/godotenv"
)

func TestCreateFileCreatesParentsAndNeverReplaces(t *testing.T) {
	files := New()
	path := filepath.Join(t.TempDir(), "nested", "artifact.txt")
	created, err := files.CreateFile(t.Context(), path, []byte("first"), 0o640)
	if err != nil || !created {
		t.Fatalf("first CreateFile = (%v, %v)", created, err)
	}
	created, err = files.CreateFile(t.Context(), path, []byte("second"), 0o600)
	if err != nil || created {
		t.Fatalf("second CreateFile = (%v, %v)", created, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first" {
		t.Fatalf("CreateFile replaced existing content: %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %04o, want 0640", info.Mode().Perm())
	}
}

func TestOperationsRejectSymlinkTargetsAndParents(t *testing.T) {
	files := New()
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "managed.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := files.CreateFile(t.Context(), link, []byte("overwrite"), 0o600); !errors.Is(err, ErrUnsafeSymlink) {
		t.Fatalf("CreateFile symlink error = %v", err)
	}
	if _, err := files.ReadFile(t.Context(), link); !errors.Is(err, ErrUnsafeSymlink) {
		t.Fatalf("ReadFile symlink error = %v", err)
	}

	parentLink := filepath.Join(root, "linked-parent")
	if err := os.Symlink(outside, parentLink); err != nil {
		t.Fatal(err)
	}
	if err := files.EnsureDirectory(t.Context(), filepath.Join(parentLink, "child"), 0o755); !errors.Is(err, ErrUnsafeSymlink) {
		t.Fatalf("EnsureDirectory symlink error = %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "outside" {
		t.Fatalf("outside target changed: %q, %v", data, err)
	}
}

func TestPrepareEnvUpdatePreservesTargetUntilBatchCommit(t *testing.T) {
	files := New()
	path := filepath.Join(t.TempDir(), ".env")
	original := "# comment\r\nUNRELATED=keep\r\nMODEL_KEY=old\r\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := envfile.Render([]byte(original),
		[]string{"MODEL_KEY", "SLACK_BOT_TOKEN"},
		map[string]string{"MODEL_KEY": "new secret", "SLACK_BOT_TOKEN": "xoxb-token"},
	)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != original {
		t.Fatalf("prepare changed target: %q, %v", unchanged, err)
	}

	temporary := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(temporary, prepared, 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := godotenv.Read(temporary)
	if err != nil {
		t.Fatal(err)
	}
	if values["UNRELATED"] != "keep" || values["MODEL_KEY"] != "new secret" || values["SLACK_BOT_TOKEN"] != "xoxb-token" {
		t.Fatalf("prepared values = %#v", values)
	}
	if !strings.Contains(string(prepared), "# comment\r\n") {
		t.Fatalf("prepared env lost comments/newline style: %q", prepared)
	}

	if err := files.WriteBatch(t.Context(), map[string][]byte{path: prepared},
		map[string]os.FileMode{path: 0o600}, map[string]bool{path: true}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("committed env mode = %04o", info.Mode().Perm())
	}
}

func TestPrepareGitIgnoreFindsParentRepositoryAndPreservesContent(t *testing.T) {
	files := New()
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(repository, "projects", "agent")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	ignorePath := filepath.Join(repository, ".gitignore")
	if err := os.WriteFile(ignorePath, []byte("dist/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, content, changed, err := files.PrepareGitIgnore(t.Context(), project)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || path != ignorePath {
		t.Fatalf("PrepareGitIgnore = (%q, %v), want changed parent file", path, changed)
	}
	if string(content) != "dist/\nprojects/agent/.env\n" {
		t.Fatalf("prepared .gitignore = %q", content)
	}
	if err := files.WriteBatch(t.Context(), map[string][]byte{path: content},
		map[string]os.FileMode{path: 0o644}, nil); err != nil {
		t.Fatal(err)
	}
	_, _, changed, err = files.PrepareGitIgnore(t.Context(), project)
	if err != nil || changed {
		t.Fatalf("second PrepareGitIgnore changed=%v err=%v", changed, err)
	}
}

func TestPrepareGitIgnoreDoesNothingOutsideRepository(t *testing.T) {
	files := New()
	root := t.TempDir()
	path, content, changed, err := files.PrepareGitIgnore(t.Context(), root)
	if err != nil || changed || path != "" || content != nil {
		t.Fatalf("PrepareGitIgnore outside repo = (%q, %q, %v, %v)", path, content, changed, err)
	}
}

func TestPrepareGitIgnoreOverridesLaterNegation(t *testing.T) {
	files := New()
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ignorePath := filepath.Join(repository, ".gitignore")
	if err := os.WriteFile(ignorePath, []byte(".env\n!.env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, content, changed, err := files.PrepareGitIgnore(t.Context(), repository)
	if err != nil || !changed {
		t.Fatalf("PrepareGitIgnore changed=%v err=%v", changed, err)
	}
	if string(content) != ".env\n!.env\n.env\n" {
		t.Fatalf("prepared .gitignore=%q", content)
	}
}

func TestWriteBatchRollsBackWhenContextCancelsDuringCommit(t *testing.T) {
	files := New()
	root := t.TempDir()
	first := filepath.Join(root, "a.txt")
	second := filepath.Join(root, "b.txt")
	if err := os.WriteFile(first, []byte("first-original"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second-original"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := &cancelAfterContext{cancelAt: 5}
	err := files.WriteBatch(ctx,
		map[string][]byte{first: []byte("first-new"), second: []byte("second-new")},
		map[string]os.FileMode{first: 0o644, second: 0o644}, nil,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteBatch error = %v, want context.Canceled", err)
	}
	assertFileContent(t, first, "first-original")
	assertFileContent(t, second, "second-original")
	firstInfo, _ := os.Stat(first)
	secondInfo, _ := os.Stat(second)
	if firstInfo.Mode().Perm() != 0o640 || secondInfo.Mode().Perm() != 0o600 {
		t.Fatalf("rollback modes = %04o, %04o", firstInfo.Mode().Perm(), secondInfo.Mode().Perm())
	}
}

type cancelAfterContext struct {
	calls    atomic.Int32
	cancelAt int32
}

func (c *cancelAfterContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterContext) Done() <-chan struct{}       { return nil }
func (c *cancelAfterContext) Value(any) any               { return nil }
func (c *cancelAfterContext) Err() error {
	if c.calls.Add(1) >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s content = %q, want %q", path, data, want)
	}
}
