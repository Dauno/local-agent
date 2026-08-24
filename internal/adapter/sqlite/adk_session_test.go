package sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
)

func TestAdkSessionServicePersistsStateAndEventOrder(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	service := NewAdkSessionService(store)

	created, err := service.Create(ctx, &session.CreateRequest{
		AppName:   "app",
		UserID:    "user",
		SessionID: "session",
		State: map[string]any{
			"app:setting":  "app-value",
			"user:setting": "user-value",
			"setting":      "session-value",
			"temp:discard": "temporary",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]string{
		"app:setting":  "app-value",
		"user:setting": "user-value",
		"setting":      "session-value",
	} {
		got, err := created.Session.State().Get(key)
		if err != nil || got != want {
			t.Fatalf("created state %q = %#v, %v; want %q", key, got, err, want)
		}
	}
	if _, err := created.Session.State().Get("temp:discard"); err == nil {
		t.Fatal("temporary create state was retained")
	}

	timestamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	first := session.NewEvent(ctx, "invocation")
	first.ID = "event-1"
	first.Timestamp = timestamp
	first.Actions.StateDelta = map[string]any{
		"setting":      "updated",
		"app:setting":  "updated-app",
		"user:setting": "updated-user",
		"temp:discard": "temporary",
	}
	if err := service.AppendEvent(ctx, created.Session, first); err != nil {
		t.Fatal(err)
	}

	second := session.NewEvent(ctx, "invocation")
	second.ID = "event-2"
	second.Timestamp = timestamp
	if err := service.AppendEvent(ctx, created.Session, second); err != nil {
		t.Fatal(err)
	}

	reloaded, err := service.Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"app:setting":  "updated-app",
		"user:setting": "updated-user",
		"setting":      "updated",
	} {
		got, err := reloaded.Session.State().Get(key)
		if err != nil || got != want {
			t.Fatalf("reloaded state %q = %#v, %v; want %q", key, got, err, want)
		}
	}
	if _, err := reloaded.Session.State().Get("temp:discard"); err == nil {
		t.Fatal("temporary event state was persisted")
	}
	if events := reloaded.Session.Events(); events.Len() != 2 || events.At(0).ID != "event-1" || events.At(1).ID != "event-2" {
		t.Fatalf("reloaded event order = %#v", events)
	}
	recent, err := service.Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session", NumRecentEvents: 1})
	if err != nil || recent.Session.Events().Len() != 1 || recent.Session.Events().At(0).ID != "event-2" {
		t.Fatalf("bounded recent events = %#v, %v", recent, err)
	}
	rangeEvents, err := service.LoadEventRange(ctx, "app", "user", "session", -1, 1)
	if err != nil || len(rangeEvents) != 1 || rangeEvents[0].ID != "event-1" {
		t.Fatalf("first event range = %#v, %v", rangeEvents, err)
	}
	rangeEvents, err = service.LoadEventRange(ctx, "app", "user", "session", 0, 1)
	if err != nil || len(rangeEvents) != 1 || rangeEvents[0].ID != "event-2" {
		t.Fatalf("second event range = %#v, %v", rangeEvents, err)
	}

	listed, err := service.List(ctx, &session.ListRequest{AppName: "app", UserID: "user"})
	if err != nil || len(listed.Sessions) != 1 || listed.Sessions[0].ID() != "session" {
		t.Fatalf("List() = %#v, %v", listed, err)
	}

	stale, err := service.Get(ctx, &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	third := session.NewEvent(ctx, "invocation")
	third.ID = "event-3"
	if err := service.AppendEvent(ctx, reloaded.Session, third); err != nil {
		t.Fatal(err)
	}
	if err := service.AppendEvent(ctx, stale.Session, session.NewEvent(ctx, "invocation")); err == nil || !strings.Contains(err.Error(), "stale session error") {
		t.Fatalf("stale AppendEvent() error = %v", err)
	}
}

// TestAppendEventRevisionEqualsEventCountEqualsMaxOrdinalPlusOne fixes DEC-08-3
// (docs/root-orchestrator-v2/08-sqlite-runtime-scaling-and-indexing-trd.md):
// for every ADK session, revision equals the persisted event count equals
// max(ordinal)+1. This invariant is what lets a caller read the session head
// through LatestEventOrdinal instead of loading the whole session, and it is
// not enforced by the schema. If a future change bumps revision without
// inserting an event, or inserts without bumping revision, this test must
// fail.
func TestAppendEventRevisionEqualsEventCountEqualsMaxOrdinalPlusOne(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	service := NewAdkSessionService(store)

	created, err := service.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}

	const eventCount = 7
	current := created.Session
	for i := 0; i < eventCount; i++ {
		event := session.NewEvent(ctx, "invocation")
		event.ID = fmt.Sprintf("evt-%d", i)
		event.Timestamp = time.Now()
		if err := service.AppendEvent(ctx, current, event); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}

		var revision int64
		if err := store.DB().QueryRowContext(ctx,
			`SELECT revision FROM adk_sessions WHERE app_name = ? AND user_id = ? AND session_id = ?`,
			"app", "user", "session",
		).Scan(&revision); err != nil {
			t.Fatalf("read revision: %v", err)
		}

		var eventRows int64
		if err := store.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM adk_events WHERE app_name = ? AND user_id = ? AND session_id = ?`,
			"app", "user", "session",
		).Scan(&eventRows); err != nil {
			t.Fatalf("count events: %v", err)
		}

		var maxOrdinal int64
		if err := store.DB().QueryRowContext(ctx,
			`SELECT MAX(ordinal) FROM adk_events WHERE app_name = ? AND user_id = ? AND session_id = ?`,
			"app", "user", "session",
		).Scan(&maxOrdinal); err != nil {
			t.Fatalf("max ordinal: %v", err)
		}

		if revision != eventRows || revision != maxOrdinal+1 {
			t.Fatalf("DEC-08-3 violated after event %d: revision=%d, event_count=%d, max(ordinal)+1=%d",
				i, revision, eventRows, maxOrdinal+1)
		}

		head, err := service.LatestEventOrdinal(ctx, "app", "user", "session")
		if err != nil {
			t.Fatalf("LatestEventOrdinal: %v", err)
		}
		if head != maxOrdinal {
			t.Fatalf("LatestEventOrdinal() = %d, want %d", head, maxOrdinal)
		}
	}
}
