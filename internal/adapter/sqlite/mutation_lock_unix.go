//go:build unix

package sqlite

import (
	"errors"
	"fmt"
	"path/filepath"

	sysunix "golang.org/x/sys/unix"

	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

// FileSchemaLocker implements rollout.SchemaLocker with a kernel-held
// advisory lock on `{database basename}.lock`, a sibling of the configured
// database. The lock file is created if missing and never deleted; its
// content carries no authority, only the OS-held lock does.
type FileSchemaLocker struct{}

// AcquireExclusive takes a non-blocking exclusive flock on the database's
// sibling lock file. The lock directory is resolved through
// filepath.EvalSymlinks and opened with O_DIRECTORY|O_NOFOLLOW; the lock
// file itself is opened relative to that verified directory descriptor with
// O_NOFOLLOW, so a symlink placed at the exact lock path fails closed with
// ELOOP instead of being followed.
func (FileSchemaLocker) AcquireExclusive(databasePath string) (rollout.Lock, error) {
	absPath, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve schema lock path %q: %w", databasePath, err)
	}
	resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(absPath))
	if err != nil {
		return nil, fmt.Errorf("resolve schema lock directory: %w", err)
	}
	dirFD, err := sysunix.Open(resolvedDir, sysunix.O_RDONLY|sysunix.O_DIRECTORY|sysunix.O_CLOEXEC|sysunix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open schema lock directory %q: %w", resolvedDir, err)
	}
	// The file descriptor Openat returns is independent of the directory
	// descriptor, so dirFD closes on every path after this point and the
	// returned lock owns only the lock-file descriptor (FIND-187).
	lockName := filepath.Base(absPath) + ".lock"
	fd, openErr := sysunix.Openat(dirFD, lockName, sysunix.O_RDWR|sysunix.O_CREAT|sysunix.O_CLOEXEC|sysunix.O_NOFOLLOW, 0o600)
	closeErr := sysunix.Close(dirFD)
	if openErr != nil {
		return nil, fmt.Errorf("open schema lock file %q: %w", lockName, openErr)
	}
	if closeErr != nil {
		_ = sysunix.Close(fd)
		return nil, fmt.Errorf("close schema lock directory descriptor: %w", closeErr)
	}
	if err := sysunix.Flock(fd, sysunix.LOCK_EX|sysunix.LOCK_NB); err != nil {
		_ = sysunix.Close(fd)
		if errors.Is(err, sysunix.EWOULDBLOCK) || errors.Is(err, sysunix.EAGAIN) {
			return nil, fmt.Errorf("%w: %q is in use by another local-agent process", rollout.ErrMutationLockHeld, lockName)
		}
		return nil, fmt.Errorf("lock schema lock file %q: %w", lockName, err)
	}
	return &fileSchemaLock{fd: fd}, nil
}

type fileSchemaLock struct{ fd int }

// Release frees the flock and closes the descriptor. The lock file stays on
// disk.
func (l *fileSchemaLock) Release() error {
	if l == nil || l.fd < 0 {
		return errors.New("schema mutation lock is not held")
	}
	unlockErr := sysunix.Flock(l.fd, sysunix.LOCK_UN)
	closeErr := sysunix.Close(l.fd)
	l.fd = -1
	if unlockErr != nil {
		return fmt.Errorf("unlock schema mutation lock: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close schema mutation lock descriptor: %w", closeErr)
	}
	return nil
}
