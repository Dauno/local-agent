package sqlite

import (
	"context"
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
