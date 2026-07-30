package sqlite

import (
	"context"
	"errors"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/port"
)

// IsRecoverableResultReferenced fails closed by treating any exact opaque-ref
// occurrence in durable ADK events or continuity capsules as active.
func (s *Store) IsRecoverableResultReferenced(ctx context.Context, ref string) (bool, error) {
	if s == nil || s.db == nil || strings.TrimSpace(ref) == "" {
		return false, errors.New("recoverable reference check requires a store and reference")
	}
	var referenced bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM adk_events WHERE instr(COALESCE(content, ''), ?) > 0
		UNION ALL
		SELECT 1 FROM continuity_capsules WHERE instr(capsule_json, ?) > 0
	)`, ref, ref).Scan(&referenced)
	return referenced, err
}

var _ port.RecoverableResultReferenceChecker = (*Store)(nil)
