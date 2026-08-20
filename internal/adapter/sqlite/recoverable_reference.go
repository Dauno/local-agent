package sqlite

import (
	"context"
	"errors"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/port"
)

// recoverableReferenceCheckQuery is the exact production query for
// IsRecoverableResultReferenced. It is a package-level const, not a string
// literal, so the Gate B test (recoverable_reference_index_test.go) can run
// EXPLAIN QUERY PLAN on the query production actually issues instead of a
// hand-copied stand-in that could drift out of sync (FIND-112).
const recoverableReferenceCheckQuery = `SELECT EXISTS(SELECT 1 FROM recoverable_result_refs WHERE ref = ?)`

// IsRecoverableResultReferenced fails closed by consulting
// recoverable_result_refs, the indexed relation AppendEvent and
// ContinuityStore.Commit maintain at write time (TRD 08 checkpoint 3). It no
// longer scans adk_events or continuity_capsules text.
func (s *Store) IsRecoverableResultReferenced(ctx context.Context, ref string) (bool, error) {
	if s == nil || s.db == nil || strings.TrimSpace(ref) == "" {
		return false, errors.New("recoverable reference check requires a store and reference")
	}
	var referenced bool
	err := s.db.QueryRowContext(ctx, recoverableReferenceCheckQuery, ref).Scan(&referenced)
	return referenced, err
}

var _ port.RecoverableResultReferenceChecker = (*Store)(nil)
