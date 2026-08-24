package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// countingExecer wraps a real sqlExecer and counts every ExecContext call, so
// a test can assert a constant statement bound against the real database
// instead of trusting a mock's call expectations.
type countingExecer struct {
	inner sqlExecer
	execs int
}

func (c *countingExecer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.execs++
	return c.inner.ExecContext(ctx, query, args...)
}

// TestIndexRecoverableResultRefsUsesConstantStatementBound is Gate FIND-132:
// one owner write must issue a constant bounded number of SQL statements no
// matter how many hex-64 windows its text contains. The corpus mixes many
// unrelated hashes, duplicate windows, and live refs at the start, middle,
// and end of the text, and the test asserts both the exact stored rows and
// the statement count. Restoring the pre-fix per-window ExecContext loop
// makes the statement-count assertion fail: it issues one statement per
// window, so it scales with corpus size instead of staying constant.
func TestIndexRecoverableResultRefsUsesConstantStatementBound(t *testing.T) {
	ctx := t.Context()
	store, _ := newTestStore(t)

	liveStart := strings.Repeat("a1", 32)
	liveMiddle := strings.Repeat("b2", 32)
	liveEnd := strings.Repeat("c3", 32)
	for _, ref := range []string{liveStart, liveMiddle, liveEnd} {
		insertRecoverableResultRow(t, store, ref)
	}

	const unrelatedCount = 2000
	var text strings.Builder
	text.WriteString(liveStart)
	text.WriteString(" ")
	// liveStart appears twice (duplicate window) and must still yield one row.
	text.WriteString(liveStart)
	text.WriteString(" ")
	for i := 0; i < unrelatedCount; i++ {
		text.WriteString(fmt.Sprintf("%064x ", i))
	}
	text.WriteString(liveMiddle)
	text.WriteString(" ")
	for i := 0; i < unrelatedCount; i++ {
		text.WriteString(fmt.Sprintf("%064x ", unrelatedCount+i))
	}
	text.WriteString(liveEnd)

	windows := extractHex64Windows(text.String())
	if len(windows) < 2*unrelatedCount+3 {
		t.Fatalf("test corpus does not extract enough windows: got %d", len(windows))
	}

	counter := &countingExecer{inner: store.db}
	now := time.Now().UTC().Unix()
	ownerID := adkEventRefOwnerID("app", "user", "sess", "event-bulk")
	if err := indexRecoverableResultRefs(ctx, counter, recoverableRefOwnerKindEvent, ownerID, text.String(), now); err != nil {
		t.Fatalf("indexRecoverableResultRefs: %v", err)
	}

	const statementBound = 4
	if counter.execs > statementBound {
		t.Fatalf("indexRecoverableResultRefs issued %d statements for %d windows, want <= %d (constant bound)", counter.execs, len(windows), statementBound)
	}

	rows, err := store.db.QueryContext(ctx, `SELECT ref FROM recoverable_result_refs WHERE owner_kind = ? AND owner_id = ? ORDER BY ref`, recoverableRefOwnerKindEvent, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			t.Fatal(err)
		}
		got = append(got, ref)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := sortedStrings([]string{liveStart, liveMiddle, liveEnd})
	if len(got) != len(want) {
		t.Fatalf("indexed refs = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("indexed refs = %v, want %v", got, want)
		}
	}
}
