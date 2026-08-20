package recoverableresult_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/adapter/recoverableresult"
	"github.com/Dauno/slack-local-agent/internal/port"

	_ "modernc.org/sqlite"
)

// TestPutInsertsResultRowBeforeReturning is the FIND-115 regression: Put's
// INSERT into recoverable_results must commit before Put hands ref back to
// the caller, because the sqlite adapter's recoverable-reference index only
// records a ref as live if it already exists in recoverable_results at the
// moment content naming it is written. A caller can only ever act on ref
// after Put returns, so this test opens a second, independent *sql.DB
// connection to the same database file and checks, with no wait of any
// kind, that the row is already visible there the instant Put returns. If
// Put ever stopped blocking on the INSERT (for example by moving it into a
// goroutine, or by building the returned ref before the INSERT runs), this
// test fails without needing a clock: it does not sleep or retry, since the
// invariant under test is "already true by the time Put returns", not
// "eventually true".
func TestPutInsertsResultRowBeforeReturning(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 1024*1024, 4096, 7, 100)

	ctx := t.Context()
	result, err := store.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "text",
		Content:         "put write order fixture",
	})
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	other, err := sql.Open("sqlite", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	var count int
	if err := other.QueryRowContext(ctx, `SELECT COUNT(*) FROM recoverable_results WHERE ref = ?`, result.Ref).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("recoverable_results row for ref %s visible from a second connection immediately after Put() returned = %d rows, want 1: "+
			"Put returned ref before its INSERT was durably committed", result.Ref, count)
	}
}
