package recoverableresult_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/recoverableresult"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestTypedStoreRecoversDeterministicPublishedPayload(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := recoverableresult.NewTypedStore(dir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := store.StorageFor(strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	payload := "one € result"
	digest := sha256.Sum256([]byte(payload))
	expectedDigest := hex.EncodeToString(digest[:])

	if err := store.Publish(ctx, storage, payload); err != nil {
		t.Fatalf("publish payload: %v", err)
	}
	// A restart can derive the same location and validate the already-published
	// payload; no producer needs to run again.
	restarted, err := recoverableresult.NewTypedStore(dir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Publish(ctx, storage, payload); err != nil {
		t.Fatalf("recover published payload: %v", err)
	}
	if err := restarted.Verify(ctx, storage, expectedDigest, int64(len(payload))); err != nil {
		t.Fatalf("verify published payload: %v", err)
	}
	chunk, err := restarted.ReadRange(ctx, storage, expectedDigest, int64(len(payload)), 0, 5)
	if err != nil {
		t.Fatalf("read UTF-8 range: %v", err)
	}
	if chunk.Content != "one " || chunk.NextOffsetBytes != 4 || chunk.EOF {
		t.Fatalf("chunk = %+v", chunk)
	}
	if _, err := restarted.ReadRange(ctx, storage, expectedDigest, int64(len(payload)), 5, 5); !errors.Is(err, domain.ErrResultInvalid) {
		t.Fatalf("mid-rune range error = %v", err)
	}
	if err := restarted.Publish(ctx, storage, "different"); !errors.Is(err, port.ErrResultPayloadConflict) {
		t.Fatalf("conflicting published payload error = %v", err)
	}
}

func TestTypedStoreVerifyEnforcesPhysicalMaximum(t *testing.T) {
	dir := t.TempDir()
	store, err := recoverableresult.NewTypedStore(dir, 8)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := store.StorageFor(strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("123456789")
	if err := os.WriteFile(filepath.Join(dir, storage.Key), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if err := store.Verify(context.Background(), storage, hex.EncodeToString(digest[:]), int64(len(payload))); !errors.Is(err, domain.ErrResultUnavailable) {
		t.Fatalf("oversized verify error = %v", err)
	}
}

func TestTypedStoreExistingPayloadPreservesCancellation(t *testing.T) {
	dir := t.TempDir()
	store, err := recoverableresult.NewTypedStore(dir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := store.StorageFor(strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(context.Background(), storage, "existing payload"); err != nil {
		t.Fatal(err)
	}
	ctx := &cancelOnSecondErrContext{}
	if err := store.Publish(ctx, storage, "existing payload"); !errors.Is(err, context.Canceled) {
		t.Fatalf("existing publish cancellation error = %v", err)
	}
}

type cancelOnSecondErrContext struct{ calls int }

func (*cancelOnSecondErrContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*cancelOnSecondErrContext) Done() <-chan struct{}       { return nil }
func (c *cancelOnSecondErrContext) Err() error {
	c.calls++
	if c.calls >= 2 {
		return context.Canceled
	}
	return nil
}
func (*cancelOnSecondErrContext) Value(any) any { return nil }
