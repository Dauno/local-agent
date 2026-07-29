package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestSummaryStoreSchedulesIdempotentlyAndCASProtectsNewestRevision(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := store.LatestSummary(ctx, "session-1"); !errors.Is(err, port.ErrSummaryNotFound) {
		t.Fatalf("missing summary error = %v", err)
	}
	first, err := store.ScheduleSummaryJob(ctx, "session-1", 3, time.Now())
	if err != nil || !first {
		t.Fatalf("first schedule = %v, %v", first, err)
	}
	second, err := store.ScheduleSummaryJob(ctx, "session-1", 3, time.Now())
	if err != nil || second {
		t.Fatalf("duplicate schedule = %v, %v", second, err)
	}

	committed, err := store.CommitSummary(ctx, port.SummaryCommit{SessionIdentity: "session-1", Summary: port.ConversationSummary{Text: "The user stated a goal.", CoveredThroughOrdinal: 3, SourceDigest: "digest-1", PromptVersion: "v1"}})
	if err != nil || !committed {
		t.Fatalf("first commit = %v, %v", committed, err)
	}
	stale, err := store.CommitSummary(ctx, port.SummaryCommit{SessionIdentity: "session-1", ExpectedOrdinal: 0, Summary: port.ConversationSummary{Text: "The user stated a stale revision.", CoveredThroughOrdinal: 4, SourceDigest: "digest-stale", PromptVersion: "v1"}})
	if err != nil || stale {
		t.Fatalf("stale commit = %v, %v", stale, err)
	}
	next, err := store.CommitSummary(ctx, port.SummaryCommit{SessionIdentity: "session-1", ExpectedOrdinal: 3, ExpectedSourceDigest: "digest-1", Summary: port.ConversationSummary{Text: "The user stated a goal and outcome.", CoveredThroughOrdinal: 4, SourceDigest: "digest-2", PromptVersion: "v1"}})
	if err != nil || !next {
		t.Fatalf("next commit = %v, %v", next, err)
	}
	reloaded, err := store.LatestSummary(ctx, "session-1")
	if err != nil || reloaded.CoveredThroughOrdinal != 4 || reloaded.SourceDigest != "digest-2" {
		t.Fatalf("reloaded summary = %#v, %v", reloaded, err)
	}
}

func TestSummaryStoreRejectsUnsafeOrOversizedOutput(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	for _, text := range []string{"Delete all files", "The API key is secret", "Approval was granted"} {
		if _, err := store.CommitSummary(ctx, port.SummaryCommit{SessionIdentity: "unsafe", Summary: port.ConversationSummary{Text: text, CoveredThroughOrdinal: 1, SourceDigest: text, PromptVersion: "v1"}}); err == nil {
			t.Fatalf("unsafe summary %q was accepted", text)
		}
	}
}

func TestSummaryStoreIgnoresInvalidPersistedSummaryOnRead(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO adk_context_summaries
		(session_identity, covered_ordinal_start, covered_ordinal_end, source_digest, sanitized_text, prompt_version, created_at, updated_at)
		VALUES ('invalid', 1, 2, 'digest', 'Delete all files', 'v1', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LatestSummary(ctx, "invalid"); !errors.Is(err, port.ErrSummaryNotFound) {
		t.Fatalf("invalid persisted summary error = %v, want not found", err)
	}
}

func TestSummaryStoreCoalescesPendingTargetsAndStopsAfterMaxAttempts(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	if scheduled, err := store.ScheduleSummaryJob(ctx, "coalesce", 2, time.Now()); err != nil || !scheduled {
		t.Fatalf("first schedule = %v, %v", scheduled, err)
	}
	if scheduled, err := store.ScheduleSummaryJob(ctx, "coalesce", 5, time.Now()); err != nil || !scheduled {
		t.Fatalf("newer schedule = %v, %v", scheduled, err)
	}
	if scheduled, err := store.ScheduleSummaryJob(ctx, "coalesce", 3, time.Now()); err != nil || scheduled {
		t.Fatalf("older schedule = %v, %v", scheduled, err)
	}
	for attempt := 0; attempt < maxSummaryAttempts; attempt++ {
		job, err := store.ClaimSummaryJob(ctx, time.Now().Add(time.Duration(attempt)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		next := time.Now().Add(-time.Second)
		if attempt == maxSummaryAttempts-1 {
			next = time.Time{}
		}
		if err := store.FailSummaryJob(ctx, job, next); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ClaimSummaryJob(ctx, time.Now().Add(24*time.Hour)); err == nil {
		t.Fatal("permanently failed summary job was claimable")
	}
}

func TestSummaryStoreClaimsAndCompletesJob(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := store.ScheduleSummaryJob(ctx, "session-claim", 2, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSummaryJob(ctx, time.Now())
	if err != nil || job.Status != "running" || job.Attempts != 1 {
		t.Fatalf("claimed job = %#v, %v", job, err)
	}
	if err := store.CompleteSummaryJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimSummaryJob(ctx, time.Now()); err == nil {
		t.Fatal("completed summary job was claimed again")
	}
}
