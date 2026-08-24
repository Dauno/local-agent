//go:build unix

package sqlite

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	sysunix "golang.org/x/sys/unix"

	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

func lockFixtureDB(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMutationLockContentionIsNonBlocking proves the flock is exclusive and
// kernel-held: a second, independently opened acquisition on the same path
// fails immediately with ErrMutationLockHeld, and releasing the first lets a
// later acquisition succeed. Release twice must report the lost hold.
func TestMutationLockContentionIsNonBlocking(t *testing.T) {
	dir := t.TempDir()
	dbPath := lockFixtureDB(t, dir, "local-agent.db")
	locker := FileSchemaLocker{}

	first, err := locker.AcquireExclusive(dbPath)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	lockFile := filepath.Join(dir, "local-agent.db.lock")
	if info, err := os.Stat(lockFile); err != nil {
		t.Fatalf("lock file missing: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %o, want 600", info.Mode().Perm())
	}

	if second, err := locker.AcquireExclusive(dbPath); err == nil {
		second.Release()
		first.Release()
		t.Fatal("second acquire succeeded while the first lock was held")
	} else if !errors.Is(err, rollout.ErrMutationLockHeld) {
		first.Release()
		t.Fatalf("second acquire err = %v, want ErrMutationLockHeld", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	again, err := locker.AcquireExclusive(dbPath)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	defer again.Release()
	if err := first.Release(); err == nil {
		t.Fatal("double release must fail")
	}
}

// TestMutationLockCanonicalizesSymlinkedParent proves the lock lands in the
// real directory behind a symlinked parent path.
func TestMutationLockCanonicalizesSymlinkedParent(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(link, "local-agent.db")
	if err := os.WriteFile(dbPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	locker := FileSchemaLocker{}
	lock, err := locker.AcquireExclusive(dbPath)
	if err != nil {
		t.Fatalf("acquire through symlinked parent: %v", err)
	}
	defer lock.Release()
	if _, err := os.Stat(filepath.Join(real, "local-agent.db.lock")); err != nil {
		t.Fatalf("lock file not in canonical parent: %v", err)
	}
}

// TestMutationLockRefusesSymlinkAtLockPath proves O_NOFOLLOW closes the
// pre-placed-symlink gap: opening fails closed instead of following.
func TestMutationLockRefusesSymlinkAtLockPath(t *testing.T) {
	dir := t.TempDir()
	dbPath := lockFixtureDB(t, dir, "local-agent.db")
	target := filepath.Join(dir, "innocent-target")
	if err := os.WriteFile(target, []byte("victim"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "local-agent.db.lock")
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatal(err)
	}

	lock, err := (FileSchemaLocker{}).AcquireExclusive(dbPath)
	if err == nil {
		lock.Release()
		t.Fatal("acquire followed a symlink placed at the lock path")
	}
	var errno sysunix.Errno
	if !errors.As(err, &errno) || !errors.Is(errno, sysunix.ELOOP) {
		t.Fatalf("err = %v, want an ELOOP open failure", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "victim" {
		t.Fatalf("target content changed: %q err=%v", data, err)
	}
	// Once the symlink is removed, the same path locks normally.
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	held, err := (FileSchemaLocker{}).AcquireExclusive(dbPath)
	if err != nil {
		t.Fatalf("acquire after removing symlink: %v", err)
	}
	held.Release()
}

// TestMutationLockFilesArePerDatabase proves the filename comes from the
// configured database basename: two databases in one directory hold
// independent locks at the same time.
func TestMutationLockFilesArePerDatabase(t *testing.T) {
	dir := t.TempDir()
	mainPath := lockFixtureDB(t, dir, "local-agent.db")
	stagingPath := lockFixtureDB(t, dir, "staging.db")
	locker := FileSchemaLocker{}

	mainLock, err := locker.AcquireExclusive(mainPath)
	if err != nil {
		t.Fatalf("acquire main: %v", err)
	}
	defer mainLock.Release()
	stagingLock, err := locker.AcquireExclusive(stagingPath)
	if err != nil {
		t.Fatalf("locking one database must never block the other: %v", err)
	}
	defer stagingLock.Release()
	for _, name := range []string{"local-agent.db.lock", "staging.db.lock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected distinct lock file %s: %v", name, err)
		}
	}
}
