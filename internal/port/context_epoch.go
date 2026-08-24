package port

import (
	"context"
	"errors"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

var (
	ErrContextEpochNotFound       = errors.New("context epoch not found")
	ErrContextEpochCASConflict    = errors.New("context epoch CAS conflict")
	ErrContextEpochValidation     = errors.New("context epoch validation failure")
	ErrContextEpochUnavailable    = errors.New("context epoch store unavailable")
	ErrContextEpochSessionMissing = errors.New("context epoch session not found")
)

// ContextEpochStore persists only bounded frame/epoch metadata. Append uses
// expectedPreviousEpoch as the session-local CAS value and never mutates ADK
// events or session state.
type ContextEpochStore interface {
	Append(ctx context.Context, epoch domain.ContextEpoch, expectedPreviousEpoch int64) error
	Latest(ctx context.Context, appName, userID, sessionID string) (domain.ContextEpoch, error)
	Range(ctx context.Context, appName, userID, sessionID string, afterEpoch, limit int64) ([]domain.ContextEpoch, error)
}
