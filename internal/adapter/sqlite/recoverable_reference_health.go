package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// recoverableReferenceDanglingOwnersQuery is the exact production query for
// the dangling-owner count (FIND-128). It is a package-level const, not a
// string literal, so the plan test can run EXPLAIN QUERY PLAN on the query
// production actually issues.
//
// The naive form compared owner_id against a concatenation of adk_events'
// four primary-key columns, which cannot use an index: SQLite has to
// evaluate that expression for every adk_events row, once per
// recoverable_result_refs candidate (a "SCAN e" per correlated lookup). This
// form decomposes owner_id with instr/substr into its four components and
// compares each one against its own column, so the correlated lookup can
// search adk_events by its primary key instead. An owner_id that does not
// decompose into exactly four unit-separator-delimited components (sep1,
// sep2, or sep3 is 0) is treated as dangling: it cannot name a real event,
// and it must not silently match one by accident.
const recoverableReferenceDanglingOwnersQuery = `
WITH adk_event_owner_parts AS (
	SELECT
		r.ref AS ref,
		r.owner_id AS owner_id,
		instr(r.owner_id, char(31)) AS sep1
	FROM recoverable_result_refs r
	WHERE r.owner_kind = 'adk_event'
),
adk_event_owner_offsets AS (
	SELECT
		ref, owner_id, sep1,
		CASE WHEN sep1 > 0 AND instr(substr(owner_id, sep1 + 1), char(31)) > 0
			THEN sep1 + instr(substr(owner_id, sep1 + 1), char(31))
			ELSE 0
		END AS sep2
	FROM adk_event_owner_parts
),
adk_event_owner_split AS (
	SELECT
		ref, owner_id, sep1, sep2,
		CASE WHEN sep2 > sep1 AND instr(substr(owner_id, sep2 + 1), char(31)) > 0
			THEN sep2 + instr(substr(owner_id, sep2 + 1), char(31))
			ELSE 0
		END AS sep3
	FROM adk_event_owner_offsets
)
SELECT COUNT(*) FROM (
	SELECT s.ref FROM adk_event_owner_split s
	WHERE s.sep1 = 0 OR s.sep2 = 0 OR s.sep3 = 0
	   OR NOT EXISTS (
	       SELECT 1 FROM adk_events e
	       WHERE e.app_name = substr(s.owner_id, 1, s.sep1 - 1)
	         AND e.user_id = substr(s.owner_id, s.sep1 + 1, s.sep2 - s.sep1 - 1)
	         AND e.session_id = substr(s.owner_id, s.sep2 + 1, s.sep3 - s.sep2 - 1)
	         AND e.id = substr(s.owner_id, s.sep3 + 1)
	   )
	UNION ALL
	SELECT r.ref FROM recoverable_result_refs r
	WHERE r.owner_kind = 'continuity_capsule'
	  AND NOT EXISTS (SELECT 1 FROM continuity_capsules c WHERE c.session_id = r.owner_id)
)`

// CheckRecoverableReferenceHealth is the offline, content-free doctor check
// for the recoverable_result_refs relation (TRD 08 checkpoint 6). It reports
// bounded counts and categories only: total rows, distinct refs, owner
// counts by kind, and two structural-integrity counts that must both be
// zero. It never reads a ref, an owner ID, or a session ID into its result.
func (s *Store) CheckRecoverableReferenceHealth(ctx context.Context) (domain.RecoverableReferenceHealth, error) {
	if s == nil || s.db == nil {
		return domain.RecoverableReferenceHealth{}, errors.New("SQLite store is not configured")
	}
	if err := checkRecoverableReferenceRequiredObjects(ctx, s.db); err != nil {
		return domain.RecoverableReferenceHealth{}, err
	}
	health := domain.RecoverableReferenceHealth{}
	var err error
	health.TotalRefRows, err = recoverableReferenceHealthCount(ctx, s.db, `SELECT COUNT(*) FROM recoverable_result_refs`)
	if err != nil {
		return health, fmt.Errorf("count total ref rows: %w", err)
	}
	health.DistinctRefs, err = recoverableReferenceHealthCount(ctx, s.db, `SELECT COUNT(DISTINCT ref) FROM recoverable_result_refs`)
	if err != nil {
		return health, fmt.Errorf("count distinct refs: %w", err)
	}
	health.EventOwners, err = recoverableReferenceHealthCount(ctx, s.db,
		`SELECT COUNT(DISTINCT owner_id) FROM recoverable_result_refs WHERE owner_kind = 'adk_event'`)
	if err != nil {
		return health, fmt.Errorf("count adk_event owners: %w", err)
	}
	health.CapsuleOwners, err = recoverableReferenceHealthCount(ctx, s.db,
		`SELECT COUNT(DISTINCT owner_id) FROM recoverable_result_refs WHERE owner_kind = 'continuity_capsule'`)
	if err != nil {
		return health, fmt.Errorf("count continuity_capsule owners: %w", err)
	}
	health.DanglingRefs, err = recoverableReferenceHealthCount(ctx, s.db,
		`SELECT COUNT(*) FROM recoverable_result_refs r
		 WHERE NOT EXISTS (SELECT 1 FROM recoverable_results rr WHERE rr.ref = r.ref)`)
	if err != nil {
		return health, fmt.Errorf("count dangling refs: %w", err)
	}
	health.DanglingOwners, err = recoverableReferenceHealthCount(ctx, s.db, recoverableReferenceDanglingOwnersQuery)
	if err != nil {
		return health, fmt.Errorf("count dangling owners: %w", err)
	}
	return health, nil
}

func recoverableReferenceHealthCount(ctx context.Context, db *sql.DB, query string) (int, error) {
	var count int
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func checkRecoverableReferenceRequiredObjects(ctx context.Context, db *sql.DB) error {
	objects := []struct{ kind, name string }{
		{"table", "recoverable_result_refs"},
		{"table", "recoverable_results"},
		{"table", "adk_events"},
		{"table", "continuity_capsules"},
		{"index", "recoverable_result_refs_by_ref"},
	}
	for _, object := range objects {
		var name string
		err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = ? AND name = ?`, object.kind, object.name).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("recoverable reference %s %q is missing", object.kind, object.name)
		}
		if err != nil {
			return fmt.Errorf("inspect recoverable reference %s %q: %w", object.kind, object.name, err)
		}
	}
	return nil
}
