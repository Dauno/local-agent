package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/recoverableresult"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// TestBindAndCleanupRecoverableResultsPreservesReferencedResult exercises the
// same composition helper composeRuntime calls (bindAndCleanupRecoverableResults),
// over real stores on a temporary SQLite database, and observes behavior
// rather than a private struct field (TRD 08 checkpoint 6, item 1).
func TestBindAndCleanupRecoverableResultsPreservesReferencedResult(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := adaptersqlite.Create(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	defer func() { _ = store.Close() }()

	resultStore := recoverableresult.NewStore(store.DB(), filepath.Join(dir, "recoverable-results"), 1<<20, 4096, 1, 100)

	referenced, err := resultStore.Put(ctx, port.PutResultRequest{
		Actor: "actor-1", ConversationKey: "conv-1", Kind: "text", Content: "referenced content",
	})
	if err != nil {
		t.Fatalf("Put(referenced) = %v", err)
	}
	unreferenced, err := resultStore.Put(ctx, port.PutResultRequest{
		Actor: "actor-1", ConversationKey: "conv-1", Kind: "text", Content: "unreferenced content",
	})
	if err != nil {
		t.Fatalf("Put(unreferenced) = %v", err)
	}

	continuityStore := adaptersqlite.NewContinuityStore(store)
	capsule := domain.ContinuityCapsule{
		Revision:     1,
		SourceDigest: "source-digest",
		Objective: &domain.ContinuityItem{
			ID:                    "objective-1",
			Kind:                  domain.ContinuityKindObjective,
			Text:                  "The recoverable result " + referenced.Ref + " is still needed",
			SourceEventOrdinal:    1,
			SourceSessionRevision: 1,
			SourceDigest:          "event-digest",
			Status:                domain.ContinuityStatusCurrent,
		},
	}
	if err := continuityStore.Commit(ctx, "adk:conversation", capsule, 0); err != nil {
		t.Fatalf("Commit() = %v", err)
	}

	// The cutoff is set far in the future so both results, regardless of
	// their real 1-day retention, are candidates for this cleanup pass. Only
	// the reference check should decide which one survives.
	cutoff := time.Now().UTC().Add(48 * time.Hour)
	deleted, err := bindAndCleanupRecoverableResults(ctx, store, resultStore, cutoff, 100)
	if err != nil {
		t.Fatalf("bindAndCleanupRecoverableResults() = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}

	if count := countRecoverableResult(t, store, referenced.Ref); count != 1 {
		t.Fatalf("referenced result rows = %d, want 1 (must survive cleanup)", count)
	}
	if count := countRecoverableResult(t, store, unreferenced.Ref); count != 0 {
		t.Fatalf("unreferenced result rows = %d, want 0 (must be deleted)", count)
	}
}

func countRecoverableResult(t *testing.T, store *adaptersqlite.Store, ref string) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM recoverable_results WHERE ref = ?`, ref).Scan(&count); err != nil {
		t.Fatalf("count recoverable_results: %v", err)
	}
	return count
}
