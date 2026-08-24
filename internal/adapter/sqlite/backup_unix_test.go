//go:build unix

package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

func vacuumSourceFixture(t *testing.T, version int) (string, int) {
	t.Helper()
	path, raw := createSchemaAtVersion(t, version)
	t.Cleanup(func() { _ = raw.Close() })
	var current int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		t.Fatal(err)
	}
	return path, current
}

// TestVacuumIntoAcceptsPrecreatedEmptyFile pins the SQLite-compatibility
// assumption FIND-150 required against modernc.org/sqlite: VACUUM INTO must
// succeed writing into an existing zero-byte file at the exact destination.
func TestVacuumIntoAcceptsPrecreatedEmptyFile(t *testing.T) {
	srcPath, _ := vacuumSourceFixture(t, 33)
	dest := filepath.Join(t.TempDir(), "precreated-empty.db")
	if err := os.WriteFile(dest, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := vacuumInto(context.Background(), srcPath, dest); err != nil {
		t.Fatalf("VACUUM INTO pre-created empty file: %v", err)
	}
	store, err := OpenReadOnly(context.Background(), dest)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer store.Close()
	var outcome string
	if err := store.DB().QueryRow("PRAGMA integrity_check").Scan(&outcome); err != nil || outcome != "ok" {
		t.Fatalf("backup integrity=%q err=%v", outcome, err)
	}
}

func TestBackupIntoProducesVerifiedIdentity(t *testing.T) {
	ctx := context.Background()
	srcPath, sourceVersion := vacuumSourceFixture(t, 40)
	dest := filepath.Join(t.TempDir(), "local-agent.pre-v41.v40.20260821T143000Z.db")

	identity, err := FileDatabaseBackupper{}.BackupInto(ctx, srcPath, dest)
	if err != nil {
		t.Fatalf("BackupInto: %v", err)
	}
	if identity.Path != dest || identity.SourceVersion != sourceVersion {
		t.Fatalf("identity = %+v, want path %q source %d", identity, dest, sourceVersion)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("backup mode = %04o, want 0600", got)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Bytes != int64(len(data)) {
		t.Fatalf("identity bytes = %d, want %d", identity.Bytes, len(data))
	}
	sum := sha256.Sum256(data)
	if identity.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("identity SHA-256 does not match the artifact bytes")
	}
	reverified, err := FileDatabaseBackupper{}.VerifyBackup(ctx, dest, sourceVersion)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if reverified.Bytes != identity.Bytes || reverified.SHA256 != identity.SHA256 {
		t.Fatalf("reverified = %+v, want bytes/sha of %+v", reverified, identity)
	}
}

func TestBackupIntoFailsClosedOnPreplacedDestination(t *testing.T) {
	ctx := context.Background()
	srcPath, _ := vacuumSourceFixture(t, 33)

	t.Run("non-empty file", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "occupied.db")
		if err := os.WriteFile(dest, []byte("previous content"), 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		_, backupErr := FileDatabaseBackupper{}.BackupInto(ctx, srcPath, dest)
		if backupErr == nil {
			t.Fatal("BackupInto over a non-empty destination must fail")
		}
		if !strings.Contains(backupErr.Error(), "exclusively") {
			t.Fatalf("err = %v, want the exclusive-create failure before VACUUM INTO", backupErr)
		}
		after, readErr := os.ReadFile(dest)
		if readErr != nil || string(after) != string(before) {
			t.Fatalf("pre-placed file changed: %q vs %q (%v)", after, before, readErr)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.txt")
		if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "linked.db")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, backupErr := (FileDatabaseBackupper{}).BackupInto(ctx, srcPath, link); backupErr == nil {
			t.Fatal("BackupInto over a symlinked destination must fail")
		}
		content, readErr := os.ReadFile(target)
		if readErr != nil || string(content) != "target" {
			t.Fatalf("symlink target was followed: %q (%v)", content, readErr)
		}
	})
}

func makeValidBackup(t *testing.T, dir string) (srcPath, backupPath string, sourceVersion int) {
	t.Helper()
	ctx := context.Background()
	srcPath, sourceVersion = vacuumSourceFixture(t, 33)
	backupPath = filepath.Join(dir, "valid.db")
	if _, seedErr := (FileDatabaseBackupper{}).BackupInto(ctx, srcPath, backupPath); seedErr != nil {
		t.Fatalf("seed valid backup: %v", seedErr)
	}
	return srcPath, backupPath, sourceVersion
}

func TestVerifyBackupFailsClosedOnDamagedArtifacts(t *testing.T) {
	_, validBackup, sourceVersion := makeValidBackup(t, t.TempDir())

	cases := []struct {
		name    string
		build   func(t *testing.T) string
		wantSrc int
	}{
		{
			name: "missing file",
			build: func(*testing.T) string {
				return filepath.Join(t.TempDir(), "absent.db")
			},
			wantSrc: sourceVersion,
		},
		{
			name: "truncated file",
			build: func(t *testing.T) string {
				full, err := os.ReadFile(validBackup)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "truncated.db")
				if err := os.WriteFile(path, full[:len(full)/2], 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
			wantSrc: sourceVersion,
		},
		{
			name: "zeroed file with correct size",
			build: func(t *testing.T) string {
				full, err := os.ReadFile(validBackup)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "zeroed.db")
				if err := os.WriteFile(path, make([]byte, len(full)), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
			wantSrc: sourceVersion,
		},
		{
			name: "wrong source version",
			build: func(t *testing.T) string {
				full, err := os.ReadFile(validBackup)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "wrongversion.db")
				if err := os.WriteFile(path, full, 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
			wantSrc: sourceVersion + 1,
		},
		{
			name: "group-writeable mode",
			build: func(t *testing.T) string {
				full, err := os.ReadFile(validBackup)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "loose.db")
				if err := os.WriteFile(path, full, 0o600); err != nil {
					t.Fatal(err)
				}
				// Umask strips creation-mode bits; chmod pins them.
				if err := os.Chmod(path, 0o620); err != nil {
					t.Fatal(err)
				}
				return path
			},
			wantSrc: sourceVersion,
		},
		{
			name: "symlink artifact",
			build: func(t *testing.T) string {
				link := filepath.Join(t.TempDir(), "link.db")
				if err := os.Symlink(validBackup, link); err != nil {
					t.Fatal(err)
				}
				return link
			},
			wantSrc: sourceVersion,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := testCase.build(t)
			_, err := FileDatabaseBackupper{}.VerifyBackup(context.Background(), path, testCase.wantSrc)
			if !errors.Is(err, rollout.ErrBackupVerificationFailed) {
				t.Fatalf("err = %v, want ErrBackupVerificationFailed", err)
			}
		})
	}
}
