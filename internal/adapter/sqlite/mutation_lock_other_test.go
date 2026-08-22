//go:build !unix

package sqlite

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

// TestUnsupportedPlatformFailsClosed pins the non-Unix contract: the locker
// returns the typed unsupported error before touching the filesystem, and
// never degrades into a no-op lock.
func TestUnsupportedPlatformFailsClosed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "local-agent.db")
	if err := os.WriteFile(dbPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := (FileSchemaLocker{}).AcquireExclusive(dbPath)
	if lock != nil {
		lock.Release()
		t.Fatal("unsupported platform returned a held lock")
	}
	if !errors.Is(err, rollout.ErrMutationLockUnsupported) {
		t.Fatalf("err = %v, want ErrMutationLockUnsupported", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "local-agent.db" {
		t.Fatalf("platform stub created files: %d entries", len(entries))
	}
}
