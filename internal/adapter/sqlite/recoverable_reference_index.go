package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Owner kinds for recoverable_result_refs (TRD 08 checkpoint 3). The relation
// tracks every 64-character lowercase hex window that names a live
// recoverable_results.ref, so IsRecoverableResultReferenced can answer by
// index lookup instead of a text scan over adk_events or continuity_capsules.
const (
	recoverableRefOwnerKindEvent   = "adk_event"
	recoverableRefOwnerKindCapsule = "continuity_capsule"
)

// sqlExecer is satisfied by both *sql.Tx and *sql.DB, so index maintenance
// runs the same way inside a caller's transaction or, for the migration
// backfill, inside the migration transaction.
type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// adkEventRefOwnerID composes the adk_events primary key into one owner_id.
// \x1f (unit separator) cannot appear in these Slack- and ADK-derived
// identifiers under normal operation, and even a collision only widens
// which owner a stray index row is attributed to: it cannot create a false
// match against a different ref.
func adkEventRefOwnerID(appName, userID, sessionID, eventID string) string {
	return appName + "\x1f" + userID + "\x1f" + sessionID + "\x1f" + eventID
}

// extractHex64Windows returns every distinct 64-character window that lies
// entirely inside a maximal run of lowercase hex characters ([0-9a-f]) in
// text. A recoverable_results.ref is always exactly 64 lowercase hex
// characters (store.go hex-encodes 32 random bytes), so any exact-substring
// occurrence of a ref necessarily falls inside one such run, and windowing
// every run of length >= 64 finds it. This is the fixed replacement for
// instr(text, ref) > 0: it does not narrow which occurrences count, it only
// changes where the ref list is looked up (a maximal-run scan of text
// instead of a per-candidate substring search).
func extractHex64Windows(text string) []string {
	const windowLen = 64
	seen := make(map[string]struct{})
	var out []string
	n := len(text)
	i := 0
	for i < n {
		if !isLowerHexByte(text[i]) {
			i++
			continue
		}
		start := i
		for i < n && isLowerHexByte(text[i]) {
			i++
		}
		runLen := i - start
		if runLen < windowLen {
			continue
		}
		for w := start; w+windowLen <= i; w++ {
			candidate := text[w : w+windowLen]
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			out = append(out, candidate)
		}
	}
	return out
}

func isLowerHexByte(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')
}

// indexRecoverableResultRefs records every hex-64 window of text that names
// a live recoverable_results.ref, attributed to (ownerKind, ownerID). A
// window that does not currently match a recoverable_results row is not
// stored: it can never be the answer to a later IsRecoverableResultReferenced
// lookup (which only ever asks about refs recoverableresult.Store still
// holds), so storing it would only grow the table for nothing.
//
// This issues exactly one INSERT statement no matter how many windows the
// text contains: the extracted windows are passed as one JSON array bind
// parameter, and json_each unpacks them inside the same statement. A prior
// version called ExecContext once per window, which made one owner write
// cost thousands of statements against a long hex run (FIND-132).
func indexRecoverableResultRefs(ctx context.Context, exec sqlExecer, ownerKind, ownerID, text string, createdAt int64) error {
	windows := extractHex64Windows(text)
	if len(windows) == 0 {
		return nil
	}
	payload, err := json.Marshal(windows)
	if err != nil {
		return fmt.Errorf("encode recoverable ref windows: %w", err)
	}
	_, err = exec.ExecContext(ctx, `INSERT OR IGNORE INTO recoverable_result_refs (ref, owner_kind, owner_id, created_at)
		SELECT w.value, ?, ?, ? FROM json_each(?) AS w
		WHERE EXISTS (SELECT 1 FROM recoverable_results WHERE ref = w.value)`,
		ownerKind, ownerID, createdAt, string(payload))
	return err
}

// replaceRecoverableResultRefs re-derives the index rows for one owner from
// its current text, dropping whatever windows the previous text named. It is
// for owners that are rewritten in place (continuity_capsules): an owner
// that is only ever inserted (adk_events) never needs this, since its text
// never changes after the row is created.
func replaceRecoverableResultRefs(ctx context.Context, exec sqlExecer, ownerKind, ownerID, text string, createdAt int64) error {
	if _, err := exec.ExecContext(ctx, `DELETE FROM recoverable_result_refs WHERE owner_kind = ? AND owner_id = ?`,
		ownerKind, ownerID); err != nil {
		return err
	}
	return indexRecoverableResultRefs(ctx, exec, ownerKind, ownerID, text, createdAt)
}
