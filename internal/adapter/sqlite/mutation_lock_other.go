//go:build !unix

package sqlite

import (
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

// FileSchemaLocker fails closed on platforms without the kernel-held flock
// primitive. It never creates or opens any file and never degrades to a
// no-op lock.
type FileSchemaLocker struct{}

// AcquireExclusive always returns rollout.ErrMutationLockUnsupported on
// this platform.
func (FileSchemaLocker) AcquireExclusive(string) (rollout.Lock, error) {
	return nil, rollout.ErrMutationLockUnsupported
}
