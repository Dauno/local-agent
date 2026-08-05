package fsartifact_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/adapter/fsartifact"
	"github.com/Dauno/slack-local-agent/internal/domain"
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

func TestResultArtifactStoreReadsVerifiedUTF8Chunks(t *testing.T) {
	store, err := fsartifact.New(t.TempDir(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	const content = "a🔥bc"
	artifact, err := store.Put(t.Context(), "job_chunk-delivery", content)
	if err != nil {
		t.Fatal(err)
	}
	read := func(offset, max int64) domain.ResultChunk {
		t.Helper()
		chunk, err := store.ReadChunk(t.Context(), domain.ResultArtifactChunkRequest{
			OwnerID: "job_chunk-delivery", Reference: artifact.Reference, ExpectedBytes: artifact.Bytes,
			ExpectedSHA256: artifact.SHA256, OffsetBytes: offset, MaxBytes: max,
		})
		if err != nil {
			t.Fatalf("read chunk at %d: %v", offset, err)
		}
		return chunk
	}
	first := read(0, 2)
	if first.Content != "a" || first.OffsetBytes != 0 || first.NextOffsetBytes != 1 || first.EOF || first.SHA256 != artifact.SHA256 {
		t.Fatalf("first chunk = %#v", first)
	}
	second := read(first.NextOffsetBytes, 4)
	if second.Content != "🔥" || second.NextOffsetBytes != 5 || second.EOF {
		t.Fatalf("second chunk = %#v", second)
	}
	third := read(second.NextOffsetBytes, 4)
	if third.Content != "bc" || third.NextOffsetBytes != artifact.Bytes || !third.EOF {
		t.Fatalf("third chunk = %#v", third)
	}
}

func TestResultArtifactErrorsCarryBoundedCodesWithoutValues(t *testing.T) {
	const (
		content      = "REDACTED-ARTIFACT-CONTENT-9c41"
		redactedRef  = "job_redacted9c41-delivery.result"
		redactedPath = "/var/run/local-agent-redacted-9c41/artifacts"
	)
	digest := sha256.Sum256([]byte(content))
	digestHex := fmt.Sprintf("%x", digest)
	forbidden := []string{content, redactedRef, redactedPath, digestHex}
	dir := t.TempDir()
	store, err := fsartifact.New(dir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Put(context.Background(), "job_9c41-delivery", content)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, artifact.Reference)

	assertBounded := func(t *testing.T, err error, want domain.ResultErrorCode) {
		t.Helper()
		if err == nil {
			t.Fatalf("expected error code %s", want)
		}
		var classified *domain.ResultError
		if !errors.As(err, &classified) || classified.Code != want {
			t.Fatalf("error = %v, want code %s", err, want)
		}
		if err.Error() != string(want) {
			t.Fatalf("error string = %q, want exact bounded code %q", err.Error(), want)
		}
		for _, value := range forbidden {
			if strings.Contains(err.Error(), value) {
				t.Fatalf("error %q leaked %q", err.Error(), value)
			}
		}
	}

	t.Run("Get", func(t *testing.T) {
		t.Run("missing file", func(t *testing.T) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			_, err := store.Get(context.Background(), "job_9c41-delivery", artifact.Reference, artifact.SHA256, 1024)
			assertBounded(t, err, domain.ResultErrorArtifactMissing)
			restored, err := store.Put(context.Background(), "job_9c41-delivery", content)
			if err != nil {
				t.Fatal(err)
			}
			artifact = restored
		})
		t.Run("reference not bound to owner", func(t *testing.T) {
			_, err := store.Get(context.Background(), "other-delivery", artifact.Reference, artifact.SHA256, 1024)
			assertBounded(t, err, domain.ResultErrorArtifactOwnerRefMismatch)
		})
		t.Run("digest missing", func(t *testing.T) {
			_, err := store.Get(context.Background(), "job_9c41-delivery", artifact.Reference, "", 1024)
			assertBounded(t, err, domain.ResultErrorArtifactDigestMismatch)
		})
		t.Run("digest mismatch", func(t *testing.T) {
			_, err := store.Get(context.Background(), "job_9c41-delivery", artifact.Reference, strings.Repeat("0", 64), 1024)
			assertBounded(t, err, domain.ResultErrorArtifactDigestMismatch)
		})
		t.Run("content exceeds read bound", func(t *testing.T) {
			_, err := store.Get(context.Background(), "job_9c41-delivery", artifact.Reference, artifact.SHA256, 4)
			assertBounded(t, err, domain.ResultErrorArtifactBytesMismatch)
		})
		t.Run("symlink replaced", func(t *testing.T) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "outside"), path); err != nil {
				t.Fatal(err)
			}
			_, err := store.Get(context.Background(), "job_9c41-delivery", artifact.Reference, artifact.SHA256, 1024)
			assertBounded(t, err, domain.ResultErrorArtifactMissing)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			restored, err := store.Put(context.Background(), "job_9c41-delivery", content)
			if err != nil {
				t.Fatal(err)
			}
			_ = restored
		})
	})

	request := domain.ResultArtifactChunkRequest{
		OwnerID: "job_9c41-delivery", Reference: artifact.Reference, ExpectedBytes: artifact.Bytes,
		ExpectedSHA256: artifact.SHA256, OffsetBytes: 0, MaxBytes: 32,
	}
	t.Run("ReadChunk", func(t *testing.T) {
		t.Run("reference not bound to owner", func(t *testing.T) {
			wrong := request
			wrong.OwnerID = "other-delivery"
			_, err := store.ReadChunk(context.Background(), wrong)
			assertBounded(t, err, domain.ResultErrorArtifactOwnerRefMismatch)
		})
		t.Run("expected size mismatch", func(t *testing.T) {
			wrong := request
			wrong.ExpectedBytes++
			_, err := store.ReadChunk(context.Background(), wrong)
			assertBounded(t, err, domain.ResultErrorArtifactBytesMismatch)
		})
		t.Run("expected digest mismatch", func(t *testing.T) {
			wrong := request
			wrong.ExpectedSHA256 = strings.Repeat("0", 64)
			_, err := store.ReadChunk(context.Background(), wrong)
			assertBounded(t, err, domain.ResultErrorArtifactDigestMismatch)
		})
		t.Run("tampered file content", func(t *testing.T) {
			if err := os.WriteFile(path, []byte(strings.Repeat("y", len(content))), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := store.ReadChunk(context.Background(), request)
			assertBounded(t, err, domain.ResultErrorArtifactDigestMismatch)
		})
	})
}

func TestResultArtifactStoreRejectsChunkBindingAndTampering(t *testing.T) {
	dir := t.TempDir()
	store, err := fsartifact.New(dir, 256*1024)
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("x", 128*1024)
	artifact, err := store.Put(t.Context(), "job_large-delivery", content)
	if err != nil {
		t.Fatal(err)
	}
	request := domain.ResultArtifactChunkRequest{
		OwnerID: "job_large-delivery", Reference: artifact.Reference, ExpectedBytes: artifact.Bytes,
		ExpectedSHA256: artifact.SHA256, OffsetBytes: 0, MaxBytes: 32,
	}
	chunk, err := store.ReadChunk(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunk.Content) != 32 || len(chunk.Content) >= len(content) || chunk.NextOffsetBytes != 32 {
		t.Fatalf("large artifact chunk = %#v, content length = %d", chunk, len(chunk.Content))
	}
	request.OwnerID = "other-job-delivery"
	if _, err := store.ReadChunk(t.Context(), request); err == nil {
		t.Fatal("wrong artifact owner was accepted")
	}
	request.OwnerID = "job_large-delivery"
	request.ExpectedBytes++
	if _, err := store.ReadChunk(t.Context(), request); err == nil {
		t.Fatal("wrong artifact size was accepted")
	}
	request.ExpectedBytes = artifact.Bytes
	request.ExpectedSHA256 = strings.Repeat("0", 64)
	if _, err := store.ReadChunk(t.Context(), request); err == nil {
		t.Fatal("wrong artifact digest was accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, artifact.Reference), []byte(strings.Repeat("y", len(content))), 0o600); err != nil {
		t.Fatal(err)
	}
	request.ExpectedSHA256 = artifact.SHA256
	if _, err := store.ReadChunk(t.Context(), request); err == nil {
		t.Fatal("tampered artifact was accepted")
	}
}
