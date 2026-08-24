//go:build !unix

package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

// TestUnsupportedBackupPlatformFailsClosed pins FIND-165's platform split on
// the backup primitive: both methods refuse without creating any file.
func TestUnsupportedBackupPlatformFailsClosed(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "backup.db")
	src := filepath.Join(dir, "source.db")

	backupper := FileDatabaseBackupper{}
	if _, err := backupper.BackupInto(context.Background(), src, dest); !errors.Is(err, rollout.ErrBackupPrimitiveUnsupported) {
		t.Fatalf("BackupInto err = %v, want ErrBackupPrimitiveUnsupported", err)
	}
	if _, err := backupper.VerifyBackup(context.Background(), dest, 33); !errors.Is(err, rollout.ErrBackupPrimitiveUnsupported) {
		t.Fatalf("VerifyBackup err = %v, want ErrBackupPrimitiveUnsupported", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup artifact stat = %v, want no file created", err)
	}
}
