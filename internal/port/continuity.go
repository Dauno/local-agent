package port

import (
	"context"
	"errors"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

var (
	ErrContinuityCASConflict = errors.New("continuity CAS conflict")
	ErrContinuityValidation  = errors.New("continuity validation failure")
	ErrContinuityUnavailable = errors.New("continuity storage unavailable")
)

// ContinuityStore persists and retrieves continuity capsules for sessions.
// Commit uses compare-and-swap: it only succeeds when the stored revision
// matches expectedRevision. A CAS conflict must be returned as an error.
type ContinuityStore interface {
	Latest(ctx context.Context, sessionID string) (domain.ContinuityCapsule, error)
	Commit(ctx context.Context, sessionID string, capsule domain.ContinuityCapsule, expectedRevision int64) error
}
