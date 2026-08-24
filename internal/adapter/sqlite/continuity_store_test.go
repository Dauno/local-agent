package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestContinuityStoreCASAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "continuity.db")
	db, err := Initialize(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	store := NewContinuityStore(db)
	capsule := domain.ContinuityCapsule{Revision: 1, SourceDigest: "source", Objective: &domain.ContinuityItem{
		ID: "objective-1", Kind: domain.ContinuityKindObjective, Text: "The active objective is context safety",
		SourceEventOrdinal: 1, SourceSessionRevision: 1, SourceDigest: "event-digest", Status: domain.ContinuityStatusCurrent,
	}}
	if err := store.Commit(ctx, "adk:conversation", capsule, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(ctx, "adk:conversation", capsule, 0); !errors.Is(err, ErrContinuityCAS) {
		t.Fatalf("stale Commit() = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenExisting(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	loaded, err := NewContinuityStore(db).Latest(ctx, "adk:conversation")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 1 || loaded.Objective == nil || loaded.Objective.Text != capsule.Objective.Text {
		t.Fatalf("Latest() = %#v", loaded)
	}
}
