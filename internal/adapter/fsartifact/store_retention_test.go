package fsartifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestArtifactCleanupRemovesOnlyOldOwnedRegularFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "job-old.result")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	newFile := filepath.Join(dir, "job-new.result")
	if err := os.WriteFile(newFile, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "job-link.result")
	if err := os.Symlink(old, link); err != nil {
		t.Fatal(err)
	}
	removed, err := store.Cleanup(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatalf("old artifact still exists: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink was removed or unreadable: %v", err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}
