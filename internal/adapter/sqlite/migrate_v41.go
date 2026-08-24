package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// migrateV41 replaces the linear instr text scan over adk_events and
// continuity_capsules (docs/root-orchestrator-v2/hallazgos/worker-prompt-trd08-checkpoint3.md)
// with an indexed relation, recoverable_result_refs, that AppendEvent and
// ContinuityStore.Commit maintain at write time. It backfills the relation
// for every ref already named by existing rows, and aborts the whole
// migration if it cannot prove it processed every row of both tables.
func migrateV41(ctx context.Context, tx *sql.Tx) error {
	if err := execMigration(ctx, tx, 41, []string{
		`CREATE TABLE recoverable_result_refs (
			ref TEXT NOT NULL,
			owner_kind TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (owner_kind, owner_id, ref),
			CHECK (length(ref) = 64 AND ref NOT GLOB '*[^0-9a-f]*'),
			CHECK (owner_kind IN ('adk_event', 'continuity_capsule')),
			CHECK (length(owner_id) > 0),
			CHECK (created_at > 0)
		) WITHOUT ROWID`,
		`CREATE INDEX recoverable_result_refs_by_ref ON recoverable_result_refs (ref)`,
	}); err != nil {
		return err
	}
	return backfillRecoverableResultRefIndex(ctx, tx)
}

// backfillRecoverableResultRefIndex populates recoverable_result_refs for
// every adk_events and continuity_capsules row that predates v41. Coverage
// is proved by counting: every row visited by the SELECT is counted as it is
// scanned, and that count must equal a fresh COUNT(*) of the same table
// inside this same migration transaction. Re-running the old instr check per
// row would cost the ~17 minutes this checkpoint exists to remove; counting
// costs one extra pass with no text comparison.
func backfillRecoverableResultRefIndex(ctx context.Context, tx *sql.Tx) error {
	now := time.Now().UTC().Unix()

	eventRows, err := tx.QueryContext(ctx, `SELECT app_name, user_id, session_id, id, content FROM adk_events`)
	if err != nil {
		return fmt.Errorf("v41 backfill: query adk_events: %w", err)
	}
	type eventRow struct {
		appName, userID, sessionID, id string
		content                        sql.NullString
	}
	var events []eventRow
	for eventRows.Next() {
		var row eventRow
		if err := eventRows.Scan(&row.appName, &row.userID, &row.sessionID, &row.id, &row.content); err != nil {
			_ = eventRows.Close()
			return fmt.Errorf("v41 backfill: scan adk_events row: %w", err)
		}
		events = append(events, row)
	}
	if err := eventRows.Err(); err != nil {
		_ = eventRows.Close()
		return fmt.Errorf("v41 backfill: iterate adk_events: %w", err)
	}
	if err := eventRows.Close(); err != nil {
		return fmt.Errorf("v41 backfill: close adk_events query: %w", err)
	}

	capsuleRows, err := tx.QueryContext(ctx, `SELECT session_id, capsule_json FROM continuity_capsules`)
	if err != nil {
		return fmt.Errorf("v41 backfill: query continuity_capsules: %w", err)
	}
	type capsuleRow struct {
		sessionID, capsuleJSON string
	}
	var capsules []capsuleRow
	for capsuleRows.Next() {
		var row capsuleRow
		if err := capsuleRows.Scan(&row.sessionID, &row.capsuleJSON); err != nil {
			_ = capsuleRows.Close()
			return fmt.Errorf("v41 backfill: scan continuity_capsules row: %w", err)
		}
		capsules = append(capsules, row)
	}
	if err := capsuleRows.Err(); err != nil {
		_ = capsuleRows.Close()
		return fmt.Errorf("v41 backfill: iterate continuity_capsules: %w", err)
	}
	if err := capsuleRows.Close(); err != nil {
		return fmt.Errorf("v41 backfill: close continuity_capsules query: %w", err)
	}

	// Coverage proof: the SELECTs above ran to completion (rows.Err checked
	// above), and this transaction holds a write lock for its whole
	// duration, so no writer can have changed either table between the
	// SELECT and this COUNT(*). A mismatch means the scan above is wrong,
	// not that the table changed under it, and the migration must not
	// proceed on an unproven backfill.
	var eventTotal int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM adk_events`).Scan(&eventTotal); err != nil {
		return fmt.Errorf("v41 backfill: count adk_events: %w", err)
	}
	if err := verifyBackfillCoverage("adk_events", len(events), eventTotal); err != nil {
		return err
	}
	var capsuleTotal int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM continuity_capsules`).Scan(&capsuleTotal); err != nil {
		return fmt.Errorf("v41 backfill: count continuity_capsules: %w", err)
	}
	if err := verifyBackfillCoverage("continuity_capsules", len(capsules), capsuleTotal); err != nil {
		return err
	}

	for _, row := range events {
		if !row.content.Valid || row.content.String == "" {
			continue
		}
		ownerID := adkEventRefOwnerID(row.appName, row.userID, row.sessionID, row.id)
		if err := indexRecoverableResultRefs(ctx, tx, recoverableRefOwnerKindEvent, ownerID, row.content.String, now); err != nil {
			return fmt.Errorf("v41 backfill: index adk_events refs: %w", err)
		}
	}
	for _, row := range capsules {
		if err := indexRecoverableResultRefs(ctx, tx, recoverableRefOwnerKindCapsule, row.sessionID, row.capsuleJSON, now); err != nil {
			return fmt.Errorf("v41 backfill: index continuity_capsules refs: %w", err)
		}
	}
	return nil
}

// verifyBackfillCoverage is the fail-closed coverage proof, isolated as a
// pure function so it can be tested directly against both a matching and a
// mismatched count without needing to fabricate a live concurrent writer
// (which the migration transaction's own lock rules out).
func verifyBackfillCoverage(table string, processed, total int) error {
	if processed != total {
		return fmt.Errorf("v41 backfill: processed %d %s rows, table has %d: refusing to migrate without proven coverage", processed, table, total)
	}
	return nil
}
