package sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/recoverableresult"
)

// TestSessionDeleteDropsRecoverableResultRefsForDeletedEvents pins FIND-127:
// deleting a session must remove the recoverable_result_refs rows its own
// events own, in the same transaction as the session delete, without
// touching refs owned by a different session's event. It then proves the
// deletion is load-bearing by running real retention cleanup afterward.
func TestSessionDeleteDropsRecoverableResultRefsForDeletedEvents(t *testing.T) {
	ctx := t.Context()
	store, _ := newTestStore(t)
	service := NewAdkSessionService(store)

	deletedRef := strings.Repeat("a", 64)
	survivingRef := strings.Repeat("b", 64)
	insertRecoverableResultRow(t, store, deletedRef)
	insertRecoverableResultRow(t, store, survivingRef)

	deletedSession, err := service.Create(ctx, &adksession.CreateRequest{AppName: "app", UserID: "user", SessionID: "sess-deleted"})
	if err != nil {
		t.Fatal(err)
	}
	deletedEvent := adksession.NewEvent(ctx, "invocation-1")
	deletedEvent.ID = "event-1"
	deletedEvent.Content = genai.NewContentFromText(fmt.Sprintf("names ref %s here", deletedRef), "model")
	if err := service.AppendEvent(ctx, deletedSession.Session, deletedEvent); err != nil {
		t.Fatal(err)
	}

	survivingSession, err := service.Create(ctx, &adksession.CreateRequest{AppName: "app", UserID: "user", SessionID: "sess-surviving"})
	if err != nil {
		t.Fatal(err)
	}
	survivingEvent := adksession.NewEvent(ctx, "invocation-2")
	survivingEvent.ID = "event-1"
	survivingEvent.Content = genai.NewContentFromText(fmt.Sprintf("names ref %s here", survivingRef), "model")
	if err := service.AppendEvent(ctx, survivingSession.Session, survivingEvent); err != nil {
		t.Fatal(err)
	}

	referenced, err := store.IsRecoverableResultReferenced(ctx, deletedRef)
	if err != nil || !referenced {
		t.Fatalf("before delete: IsRecoverableResultReferenced(deletedRef) = %v, %v; want true", referenced, err)
	}
	referenced, err = store.IsRecoverableResultReferenced(ctx, survivingRef)
	if err != nil || !referenced {
		t.Fatalf("before delete: IsRecoverableResultReferenced(survivingRef) = %v, %v; want true", referenced, err)
	}

	if err := service.Delete(ctx, &adksession.DeleteRequest{AppName: "app", UserID: "user", SessionID: "sess-deleted"}); err != nil {
		t.Fatalf("Delete() = %v", err)
	}

	deletedOwnerID := adkEventRefOwnerID("app", "user", "sess-deleted", "event-1")
	var orphanRows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recoverable_result_refs WHERE owner_kind = ? AND owner_id = ?`,
		recoverableRefOwnerKindEvent, deletedOwnerID).Scan(&orphanRows); err != nil {
		t.Fatal(err)
	}
	if orphanRows != 0 {
		t.Fatalf("recoverable_result_refs rows for the deleted session's event = %d, want 0", orphanRows)
	}

	health, err := store.CheckRecoverableReferenceHealth(ctx)
	if err != nil {
		t.Fatalf("CheckRecoverableReferenceHealth() = %v", err)
	}
	if health.DanglingOwners != 0 {
		t.Fatalf("DanglingOwners = %d, want 0 after session delete: %#v", health.DanglingOwners, health)
	}

	// The other session's ref must be untouched: Delete must not remove
	// refs it does not own.
	referenced, err = store.IsRecoverableResultReferenced(ctx, survivingRef)
	if err != nil || !referenced {
		t.Fatalf("after delete: IsRecoverableResultReferenced(survivingRef) = %v, %v; want true", referenced, err)
	}
	referenced, err = store.IsRecoverableResultReferenced(ctx, deletedRef)
	if err != nil || referenced {
		t.Fatalf("after delete: IsRecoverableResultReferenced(deletedRef) = %v, %v; want false", referenced, err)
	}

	// Retention cleanup must now be able to remove the result whose last
	// owner was the deleted session; the surviving result must not move.
	resultStore := recoverableresult.NewStore(store.db, filepath.Join(t.TempDir(), "recoverable-results"), 1<<20, 4096, 1, 100)
	resultStore.SetReferenceChecker(store)
	deleted, err := resultStore.DeleteExpired(ctx, deletedSession.Session.LastUpdateTime().AddDate(1, 0, 0), 100)
	if err != nil {
		t.Fatalf("DeleteExpired() = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteExpired() deleted = %d, want 1", deleted)
	}

	var remaining int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recoverable_results WHERE ref = ?`, deletedRef).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("recoverable_results rows for deletedRef = %d, want 0 (must be cleaned up)", remaining)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recoverable_results WHERE ref = ?`, survivingRef).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("recoverable_results rows for survivingRef = %d, want 1 (must survive)", remaining)
	}
}
