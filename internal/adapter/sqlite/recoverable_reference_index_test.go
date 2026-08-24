package sqlite

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestExtractHex64Windows pins the windowing algorithm against the instr
// semantics it replaces (docs/root-orchestrator-v2/hallazgos/worker-prompt-trd08-checkpoint3.md):
// every exact-substring occurrence of a 64-hex ref must produce a matching
// window, including one embedded inside a longer hex run, and nothing that
// is not an exact 64-hex occurrence must produce one.
func TestExtractHex64Windows(t *testing.T) {
	ref := strings.Repeat("a1", 32) // 64 lowercase hex chars

	cases := []struct {
		name string
		text string
		want []string
	}{
		{"absent", "no hex here at all", nil},
		{"exact match, no surrounding hex", "prefix " + ref + " suffix", []string{ref}},
		{"embedded inside a longer hex run", "ff" + ref + "ee", []string{
			// The maximal run is 68 hex chars ("ff"+ref+"ee"); every 64-char
			// window of that run is a candidate, and instr would also find
			// ref as a substring regardless of what surrounds it, so this
			// windowing must not miss the embedded occurrence.
			"ff" + ref[:62],
			"f" + ref[:63],
			ref,
			ref[1:] + "e",
			ref[2:] + "ee",
		}},
		{"run shorter than 64 never yields a window", strings.Repeat("b", 63), nil},
		{"ref split across two runs by a non-hex separator", ref[:32] + "-" + ref[32:], nil},
		{"uppercase hex never matches a lowercase ref", strings.ToUpper(ref), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sortedStrings(extractHex64Windows(tc.text))
			want := sortedStrings(tc.want)
			if len(got) != len(want) {
				t.Fatalf("extractHex64Windows(%q) = %v, want %v", tc.text, got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("extractHex64Windows(%q) = %v, want %v", tc.text, got, want)
				}
			}
		})
	}
}

// TestIsRecoverableResultReferencedQueryPlanHasNoEventScan is Gate B
// (DEC-08-5): EXPLAIN QUERY PLAN of the exact production query must not
// contain "SCAN adk_events". It runs recoverableReferenceCheckQuery itself,
// the same const production calls, so a future edit that reintroduces a
// text scan makes this test fail without anyone having to remember to keep
// a hand-copied query in sync (FIND-112).
func TestIsRecoverableResultReferencedQueryPlanHasNoEventScan(t *testing.T) {
	store, _ := newTestStore(t)
	rows, err := store.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+recoverableReferenceCheckQuery, "irrelevant-ref")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.String(), "SCAN adk_events") {
		t.Fatalf("query plan contains SCAN adk_events:\n%s", plan.String())
	}
	if strings.Contains(plan.String(), "SCAN recoverable_result_refs") {
		t.Fatalf("query plan scans recoverable_result_refs instead of using its index:\n%s", plan.String())
	}
}

func insertRecoverableResultRow(t *testing.T, store *Store, ref string) {
	t.Helper()
	now := time.Now().UTC().Unix()
	if _, err := store.db.ExecContext(t.Context(), `INSERT INTO recoverable_results
		(ref, actor, conversation_key, kind, storage_locator, size_bytes, code_points, sha256, created_at, expires_at)
		VALUES (?, 'actor', 'slack:T1:dm:D1', 'test', ?, 1, 1, ?, ?, ?)`,
		ref, "locator-"+ref, strings.Repeat("0", 64), now, now+3600); err != nil {
		t.Fatalf("insert recoverable_results row for ref %s: %v", ref, err)
	}
}

// TestAppendEventIndexesEmbeddedRefWithoutNarrowingDetection is the minimal
// proof required by criterion 4: an event whose content names a ref only as
// a substring inside a longer hex run must still be detected as
// referencing, exactly like the instr scan it replaces did.
func TestAppendEventIndexesEmbeddedRefWithoutNarrowingDetection(t *testing.T) {
	ctx := t.Context()
	store, _ := newTestStore(t)
	service := NewAdkSessionService(store)

	embeddedRef := strings.Repeat("c2", 32)
	insertRecoverableResultRow(t, store, embeddedRef)
	absentRef := strings.Repeat("d3", 32)
	insertRecoverableResultRow(t, store, absentRef)
	splitRef := strings.Repeat("e4", 32)
	insertRecoverableResultRow(t, store, splitRef)

	created, err := service.Create(ctx, &adksession.CreateRequest{AppName: "app", UserID: "user", SessionID: "sess"})
	if err != nil {
		t.Fatal(err)
	}

	event := adksession.NewEvent(ctx, "invocation")
	event.ID = "event-1"
	// embeddedRef appears only inside a longer hex run ("ff"+ref+"ee");
	// splitRef appears split by a non-hex separator, so it must not match,
	// matching instr's exact-substring semantics.
	text := fmt.Sprintf("handoff ff%see more text %s-%s tail", embeddedRef, splitRef[:32], splitRef[32:])
	event.Content = genai.NewContentFromText(text, "model")
	if err := service.AppendEvent(ctx, created.Session, event); err != nil {
		t.Fatal(err)
	}

	referenced, err := store.IsRecoverableResultReferenced(ctx, embeddedRef)
	if err != nil || !referenced {
		t.Fatalf("IsRecoverableResultReferenced(embedded) = %v, %v; want true", referenced, err)
	}
	referenced, err = store.IsRecoverableResultReferenced(ctx, absentRef)
	if err != nil || referenced {
		t.Fatalf("IsRecoverableResultReferenced(absent) = %v, %v; want false", referenced, err)
	}
	referenced, err = store.IsRecoverableResultReferenced(ctx, splitRef)
	if err != nil || referenced {
		t.Fatalf("IsRecoverableResultReferenced(split) = %v, %v; want false", referenced, err)
	}
}

// TestContinuityCommitReplacesRefIndexOnRevision covers criterion 6: a
// capsule revision that stops naming a ref must drop that ref's index row,
// not leave it protected forever, and the CAS check must still run inside
// the same transaction as the index rewrite.
func TestContinuityCommitReplacesRefIndexOnRevision(t *testing.T) {
	ctx := t.Context()
	store, _ := newTestStore(t)
	continuity := NewContinuityStore(store)

	ref := strings.Repeat("f5", 32)
	insertRecoverableResultRow(t, store, ref)

	sessionID := "sess-1"
	capsuleV1 := domain.ContinuityCapsule{
		Revision:      1,
		ActiveResults: []domain.ActiveResultReference{{Kind: "text", ResultRef: ref, SHA256: strings.Repeat("0", 64), Description: "d"}},
	}
	if err := continuity.Commit(ctx, sessionID, capsuleV1, 0); err != nil {
		t.Fatalf("commit v1: %v", err)
	}
	referenced, err := store.IsRecoverableResultReferenced(ctx, ref)
	if err != nil || !referenced {
		t.Fatalf("after v1 commit: IsRecoverableResultReferenced = %v, %v; want true", referenced, err)
	}

	// Wrong expectedRevision must still fail with ErrContinuityCAS, and the
	// index for the still-live v1 row must be untouched.
	if err := continuity.Commit(ctx, sessionID, domain.ContinuityCapsule{Revision: 3}, 1); err == nil {
		t.Fatal("commit with wrong expectedRevision unexpectedly succeeded")
	} else if err != ErrContinuityCAS {
		t.Fatalf("commit with wrong expectedRevision error = %v, want ErrContinuityCAS", err)
	}
	referenced, err = store.IsRecoverableResultReferenced(ctx, ref)
	if err != nil || !referenced {
		t.Fatalf("after rejected CAS: IsRecoverableResultReferenced = %v, %v; want true", referenced, err)
	}

	capsuleV2 := domain.ContinuityCapsule{Revision: 2} // no longer names ref
	if err := continuity.Commit(ctx, sessionID, capsuleV2, 1); err != nil {
		t.Fatalf("commit v2: %v", err)
	}
	referenced, err = store.IsRecoverableResultReferenced(ctx, ref)
	if err != nil || referenced {
		t.Fatalf("after v2 commit: IsRecoverableResultReferenced = %v, %v; want false", referenced, err)
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recoverable_result_refs WHERE owner_kind = ? AND owner_id = ?`,
		recoverableRefOwnerKindCapsule, sessionID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("stale capsule index rows after v2 commit = %d, want 0", rows)
	}
}
