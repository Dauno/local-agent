package sqlite

import (
	"fmt"
	"strings"
	"testing"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// TestRecoverableReferenceOwnerPlanHasNoEventScan is the FIND-128 plan gate.
// It runs EXPLAIN QUERY PLAN on the exact production
// recoverableReferenceDanglingOwnersQuery constant and requires: each
// recoverable_result_refs lookup uses its primary key (owner_kind first
// component), the adk_events lookup uses its complete primary key, and the
// plan never scans adk_events in full.
func TestRecoverableReferenceOwnerPlanHasNoEventScan(t *testing.T) {
	store, _ := newTestStore(t)
	rows, err := store.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+recoverableReferenceDanglingOwnersQuery)
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
	text := plan.String()

	if strings.Contains(text, "SCAN e") || strings.Contains(text, "SCAN adk_events") {
		t.Fatalf("query plan scans adk_events:\n%s", text)
	}
	if !strings.Contains(text, "SEARCH e USING PRIMARY KEY (app_name=? AND user_id=? AND session_id=? AND id=?)") {
		t.Fatalf("query plan does not search adk_events by its complete primary key:\n%s", text)
	}
	if got := strings.Count(text, "SEARCH r USING PRIMARY KEY (owner_kind=?)"); got != 2 {
		t.Fatalf("query plan searches recoverable_result_refs by primary key %d times, want 2 (one per owner kind):\n%s", got, text)
	}
	if !strings.Contains(text, "SEARCH c USING PRIMARY KEY (session_id=?)") {
		t.Fatalf("query plan does not search continuity_capsules by its primary key:\n%s", text)
	}
}

// TestCheckRecoverableReferenceHealthAdkEventOwnerFixtures exercises the
// FIND-128 decomposition against real data: a healthy adk_event owner
// (created through the real AppendEvent write path), a well-formed but
// nonexistent adk_event owner, a malformed adk_event owner_id with no
// unit-separator structure, and a healthy continuity_capsule owner.
func TestCheckRecoverableReferenceHealthAdkEventOwnerFixtures(t *testing.T) {
	ctx := t.Context()
	store, _ := newTestStore(t)

	healthyEventRef := strings.Repeat("1", 64)
	missingEventRef := strings.Repeat("2", 64)
	malformedEventRef := strings.Repeat("3", 64)
	healthyCapsuleRef := strings.Repeat("4", 64)
	for _, ref := range []string{healthyEventRef, missingEventRef, malformedEventRef, healthyCapsuleRef} {
		insertRecoverableResultRow(t, store, ref)
	}

	// Healthy adk_event owner: a real event, indexed through AppendEvent.
	service := NewAdkSessionService(store)
	created, err := service.Create(ctx, &adksession.CreateRequest{AppName: "app", UserID: "user", SessionID: "sess-live"})
	if err != nil {
		t.Fatal(err)
	}
	event := adksession.NewEvent(ctx, "invocation-1")
	event.ID = "event-1"
	event.Content = genai.NewContentFromText(fmt.Sprintf("names ref %s", healthyEventRef), "model")
	if err := service.AppendEvent(ctx, created.Session, event); err != nil {
		t.Fatal(err)
	}

	// Missing adk_event owner: well-formed owner_id, no matching adk_events row.
	seedRecoverableResultRefRow(t, store, missingEventRef, recoverableRefOwnerKindEvent,
		adkEventRefOwnerID("app", "user", "ghost-session", "ghost-event"))

	// Malformed adk_event owner: no unit-separator structure at all.
	seedRecoverableResultRefRow(t, store, malformedEventRef, recoverableRefOwnerKindEvent, "not-a-composite-owner-id")

	// Healthy continuity_capsule owner.
	seedContinuityCapsuleRow(t, store, "session-live")
	seedRecoverableResultRefRow(t, store, healthyCapsuleRef, recoverableRefOwnerKindCapsule, "session-live")

	health, err := store.CheckRecoverableReferenceHealth(ctx)
	if err != nil {
		t.Fatalf("CheckRecoverableReferenceHealth() = %v", err)
	}
	if health.EventOwners != 3 {
		t.Fatalf("EventOwners = %d, want 3: %#v", health.EventOwners, health)
	}
	if health.CapsuleOwners != 1 {
		t.Fatalf("CapsuleOwners = %d, want 1: %#v", health.CapsuleOwners, health)
	}
	if health.DanglingRefs != 0 {
		t.Fatalf("DanglingRefs = %d, want 0: %#v", health.DanglingRefs, health)
	}
	if health.DanglingOwners != 2 {
		t.Fatalf("DanglingOwners = %d, want 2 (missing + malformed): %#v", health.DanglingOwners, health)
	}
}
