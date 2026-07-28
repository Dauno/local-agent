package fsartifact_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/adapter/fsartifact"
)

func TestResultArtifactStoreWritesPrivateAtomicArtifact(t *testing.T) {
	dir := t.TempDir()
	store, err := fsartifact.New(dir, 4096)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Put(context.Background(), "job_123", "final result 🚀")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reference == "" || result.SHA256 == "" || result.Bytes <= 0 {
		t.Fatalf("artifact = %+v", result)
	}
	path := filepath.Join(dir, result.Reference)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "final result 🚀" {
		t.Fatalf("artifact data = %q, err = %v", data, err)
	}
}

func TestResultArtifactStoreRejectsOverflowAndPathLikeIDs(t *testing.T) {
	store, err := fsartifact.New(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), "../escape", "safe"); err == nil {
		t.Fatal("path-like job ID accepted")
	}
	if _, err := store.Put(context.Background(), "job_1", strings.Repeat("x", 5)); err == nil {
		t.Fatal("oversized artifact accepted")
	}
}

func TestResultArtifactStoreCreateNoReplaceIsAtomic(t *testing.T) {
	store, err := fsartifact.New(t.TempDir(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	const writers = 8
	results := make(chan error, writers)
	var group sync.WaitGroup
	for i := 0; i < writers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := store.Put(context.Background(), "same-job", "complete result")
			results <- err
		}()
	}
	group.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("successful create calls = %d, want exactly one", wins)
	}
}
