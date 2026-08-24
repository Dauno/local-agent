//go:build !unix

package sqlite

import (
	"context"
	"fmt"

	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

// FileDatabaseBackupper fails closed on platforms without the backup
// primitive. It never opens or creates a file; the !unix lock stub already
// refuses every caller before this type is reachable, and this stub keeps
// the package compilable without ever degrading into a no-op backup.
type FileDatabaseBackupper struct{}

func (FileDatabaseBackupper) BackupInto(context.Context, string, string) (rollout.BackupIdentity, error) {
	return rollout.BackupIdentity{}, fmt.Errorf("%w: no backup primitive is implemented for this platform", rollout.ErrBackupPrimitiveUnsupported)
}

func (FileDatabaseBackupper) VerifyBackup(context.Context, string, int) (rollout.BackupIdentity, error) {
	return rollout.BackupIdentity{}, fmt.Errorf("%w: no backup primitive is implemented for this platform", rollout.ErrBackupPrimitiveUnsupported)
}
