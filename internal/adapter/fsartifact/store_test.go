package fsartifact_test

import (
	"context"
	"crypto/sha256"
	"fmt"
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

func TestResultArtifactStoreVerifiedReadBindsOwnerAndDigest(t *testing.T) {
	store, err := fsartifact.New(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	const content = "complete sanitized result"
	artifact, err := store.Put(context.Background(), "job_1-delivery", content)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	data, err := store.Get(context.Background(), "job_1-delivery", artifact.Reference, fmt.Sprintf("%x", digest), 1024)
	if err != nil || string(data) != content {
		t.Fatalf("verified read = %q, err = %v", data, err)
	}
	if _, err := store.Get(context.Background(), "other", artifact.Reference, artifact.SHA256, 1024); err == nil {
		t.Fatal("artifact was readable by a different owner")
	}
	if _, err := store.Get(context.Background(), "job_1-delivery", artifact.Reference, "wrong", 1024); err == nil {
		t.Fatal("digest mismatch was accepted")
	}
}

func TestResultArtifactStoreRejectsSymlinkNonRegularAndReadOverflow(t *testing.T) {
	dir := t.TempDir()
	store, err := fsartifact.New(dir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Put(context.Background(), "job_1-delivery", "safe")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, artifact.Reference)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "outside"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), "job_1-delivery", artifact.Reference, artifact.SHA256, 1024); err == nil {
		t.Fatal("symlink artifact was accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), "job_1-delivery", artifact.Reference, artifact.SHA256, 1024); err == nil {
		t.Fatal("non-regular artifact was accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	overflow := []byte("12345")
	digest := sha256.Sum256(overflow)
	if err := os.WriteFile(path, overflow, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), "job_1-delivery", artifact.Reference, fmt.Sprintf("%x", digest), 4); err == nil {
		t.Fatal("artifact read exceeded the requested bound")
	}
}
