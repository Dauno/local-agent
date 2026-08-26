//go:build unix

package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	sysunix "golang.org/x/sys/unix"

	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

// FileDatabaseBackupper implements rollout.DatabaseBackupper. BackupInto
// creates the destination exclusively through a dirFD-relative Openat with
// O_EXCL|O_NOFOLLOW and mode 0600 from the moment of creation, runs
// VACUUM INTO over a read-only source connection, and verifies the artifact
// before reporting its identity. It never opens the source mode=rw and
// never migrates.
type FileDatabaseBackupper struct{}

func (FileDatabaseBackupper) BackupInto(ctx context.Context, srcPath, destPath string) (rollout.BackupIdentity, error) {
	absDest, err := filepath.Abs(destPath)
	if err != nil {
		return rollout.BackupIdentity{}, fmt.Errorf("resolve backup destination %q: %w", destPath, err)
	}
	resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(absDest))
	if err != nil {
		return rollout.BackupIdentity{}, fmt.Errorf("resolve backup directory: %w", err)
	}
	dirFD, err := sysunix.Open(resolvedDir, sysunix.O_RDONLY|sysunix.O_DIRECTORY|sysunix.O_CLOEXEC|sysunix.O_NOFOLLOW, 0)
	if err != nil {
		return rollout.BackupIdentity{}, fmt.Errorf("open backup directory %q: %w", resolvedDir, err)
	}
	name := filepath.Base(absDest)
	// O_EXCL fails while nothing has been created or followed: a non-empty
	// file or a symlink pre-placed at this exact name aborts before
	// VACUUM INTO ever runs (FIND-150). A zero-byte file also collides here;
	// VACUUM INTO's tolerance of an existing empty file is exercised by the
	// driver-level gate, not by relaxing this create.
	fd, openErr := sysunix.Openat(dirFD, name, sysunix.O_CREAT|sysunix.O_EXCL|sysunix.O_WRONLY|sysunix.O_NOFOLLOW|sysunix.O_CLOEXEC, 0o600)
	closeErr := sysunix.Close(dirFD)
	if openErr != nil {
		return rollout.BackupIdentity{}, fmt.Errorf("create backup file %q exclusively: %w", name, openErr)
	}
	if closeErr != nil {
		_ = sysunix.Close(fd)
		return rollout.BackupIdentity{}, fmt.Errorf("close backup directory descriptor: %w", closeErr)
	}
	if closeErr := sysunix.Close(fd); closeErr != nil {
		_ = os.Remove(absDest)
		return rollout.BackupIdentity{}, fmt.Errorf("close exclusive backup file descriptor: %w", closeErr)
	}

	sourceVersion, err := readUserVersion(ctx, srcPath)
	if err != nil {
		_ = os.Remove(absDest)
		return rollout.BackupIdentity{}, err
	}
	if err := vacuumInto(ctx, srcPath, absDest); err != nil {
		_ = os.Remove(absDest)
		return rollout.BackupIdentity{}, err
	}
	// Defense in depth on top of the exclusive-create mode bits: VACUUM
	// INTO writes through its own handle without a documented guarantee it
	// preserves them.
	if err := os.Chmod(absDest, 0o600); err != nil {
		_ = os.Remove(absDest)
		return rollout.BackupIdentity{}, fmt.Errorf("restrict backup file mode: %w", err)
	}
	bytes, digest, verifyErr := verifyBackupArtifact(ctx, absDest, sourceVersion)
	if verifyErr != nil {
		removeErr := os.Remove(absDest)
		if removeErr != nil {
			return rollout.BackupIdentity{}, errors.Join(verifyErr, fmt.Errorf("remove partial backup %q: %w", absDest, removeErr))
		}
		return rollout.BackupIdentity{}, verifyErr
	}
	return rollout.BackupIdentity{
		Path:          absDest,
		Bytes:         bytes,
		SHA256:        digest,
		SourceVersion: sourceVersion,
		VerifiedAt:    time.Now().UTC(),
	}, nil
}

// VerifyBackup revalidates one live artifact against wantSourceVersion,
// which every caller derives from its own live lock-held schema read. It is
// a read against a file that may not be this process's own backup, so it
// never removes anything; ownership and mode checks fail closed first.
func (FileDatabaseBackupper) VerifyBackup(ctx context.Context, backupPath string, wantSourceVersion int) (rollout.BackupIdentity, error) {
	absPath, err := filepath.Abs(backupPath)
	if err != nil {
		return rollout.BackupIdentity{}, fmt.Errorf("%w: resolve recorded backup path %q: %v", rollout.ErrBackupVerificationFailed, backupPath, err)
	}
	info, statErr := os.Lstat(absPath)
	if statErr != nil {
		return rollout.BackupIdentity{}, fmt.Errorf("%w: stat recorded backup %q: %v", rollout.ErrBackupVerificationFailed, absPath, statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return rollout.BackupIdentity{}, fmt.Errorf("%w: recorded backup %q is a symlink", rollout.ErrBackupVerificationFailed, absPath)
	}
	if !info.Mode().IsRegular() {
		return rollout.BackupIdentity{}, fmt.Errorf("%w: recorded backup %q is not a regular file", rollout.ErrBackupVerificationFailed, absPath)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return rollout.BackupIdentity{}, fmt.Errorf("%w: recorded backup %q carries group or other write bits", rollout.ErrBackupVerificationFailed, absPath)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	effectiveUID := os.Geteuid()
	if !ok || effectiveUID < 0 || uint64(stat.Uid) != uint64(effectiveUID) {
		return rollout.BackupIdentity{}, fmt.Errorf("%w: recorded backup %q is not owned by this process's effective user", rollout.ErrBackupVerificationFailed, absPath)
	}
	bytes, digest, verifyErr := verifyBackupArtifact(ctx, absPath, wantSourceVersion)
	if verifyErr != nil {
		return rollout.BackupIdentity{}, verifyErr
	}
	return rollout.BackupIdentity{
		Path:          absPath,
		Bytes:         bytes,
		SHA256:        digest,
		SourceVersion: wantSourceVersion,
		VerifiedAt:    time.Now().UTC(),
	}, nil
}

// verifyBackupArtifact streams the file's full bytes once for size and
// SHA-256, then asserts integrity_check, foreign_key_check, and
// user_version == wantSourceVersion over a fresh read-only connection.
func verifyBackupArtifact(ctx context.Context, path string, wantSourceVersion int) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("%w: open backup %q: %v", rollout.ErrBackupVerificationFailed, path, err)
	}
	hasher := sha256.New()
	size, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		return 0, "", fmt.Errorf("%w: hash backup %q: %v", rollout.ErrBackupVerificationFailed, path, copyErr)
	}
	if closeErr != nil {
		return 0, "", fmt.Errorf("%w: close backup %q: %v", rollout.ErrBackupVerificationFailed, path, closeErr)
	}
	digest := hex.EncodeToString(hasher.Sum(nil))

	store, err := OpenReadOnly(ctx, path)
	if err != nil {
		return 0, "", fmt.Errorf("%w: open backup %q as SQLite: %v", rollout.ErrBackupVerificationFailed, path, err)
	}
	defer func() { _ = store.Close() }()
	var outcome string
	if err := store.DB().QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&outcome); err != nil {
		return 0, "", fmt.Errorf("%w: integrity check on %q: %v", rollout.ErrBackupVerificationFailed, path, err)
	}
	if outcome != "ok" {
		return 0, "", fmt.Errorf("%w: integrity check on %q reported %q", rollout.ErrBackupVerificationFailed, path, outcome)
	}
	fkRows, err := store.DB().QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return 0, "", fmt.Errorf("%w: foreign key check on %q: %v", rollout.ErrBackupVerificationFailed, path, err)
	}
	defer func() { _ = fkRows.Close() }()
	hasFKRows := fkRows.Next()
	fkIterationErr := fkRows.Err()
	fkCloseErr := fkRows.Close()
	if hasFKRows {
		return 0, "", fmt.Errorf("%w: foreign key check on %q returned rows", rollout.ErrBackupVerificationFailed, path)
	}
	if fkIterationErr != nil {
		return 0, "", fmt.Errorf("%w: run foreign key check on %q: %v", rollout.ErrBackupVerificationFailed, path, fkIterationErr)
	}
	if fkCloseErr != nil {
		return 0, "", fmt.Errorf("%w: finish foreign key check on %q: %v", rollout.ErrBackupVerificationFailed, path, fkCloseErr)
	}
	var version int
	if err := store.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, "", fmt.Errorf("%w: read backup user_version on %q: %v", rollout.ErrBackupVerificationFailed, path, err)
	}
	if version != wantSourceVersion {
		return 0, "", fmt.Errorf("%w: backup %q holds user_version %d, want %d", rollout.ErrBackupVerificationFailed, path, version, wantSourceVersion)
	}
	return size, digest, nil
}

func vacuumInto(ctx context.Context, srcPath, destPath string) error {
	store, err := OpenReadOnly(ctx, srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if _, err := store.DB().ExecContext(ctx, "VACUUM INTO ?", destPath); err != nil {
		return fmt.Errorf("VACUUM INTO %q: %w", destPath, err)
	}
	return nil
}

func readUserVersion(ctx context.Context, path string) (int, error) {
	store, err := OpenReadOnly(ctx, path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = store.Close() }()
	version, err := currentUserVersion(ctx, store.DB())
	if err != nil {
		return 0, err
	}
	return version, nil
}
