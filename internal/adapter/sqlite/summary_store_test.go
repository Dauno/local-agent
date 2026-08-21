package sqlite

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
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

// TestSummaryStoreDiscoveryMarkerIsSchedulableAfterCompletion is FIND-134
// lifecycle gate 1: internal/usecase/contextsummary always schedules the same
// fixed discovery-range marker (domain.SummaryDiscoveryTargetFloor here stands in
// for it). The primary key (session_identity, target_ordinal) retains done
// rows, so a naive repeat of that literal value after the first discovery
// completes collides. This proves the real store instead claims a fresh,
// claimable marker, and that resolving against a real ClosedTurns-shaped
// range still lands on the newly eligible turns.
func TestSummaryStoreDiscoveryMarkerIsSchedulableAfterCompletion(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	const session = "discovery-repeat"

	scheduled, err := store.ScheduleSummaryJob(ctx, session, domain.SummaryDiscoveryTargetFloor, time.Now().Add(-time.Second))
	if err != nil || !scheduled {
		t.Fatalf("first discovery schedule = %v, %v", scheduled, err)
	}
	firstJob, err := store.ClaimSummaryJob(ctx, time.Now())
	if err != nil {
		t.Fatalf("claim first discovery: %v", err)
	}
	if firstJob.TargetOrdinal != domain.SummaryDiscoveryTargetFloor {
		t.Fatalf("first discovery target = %d, want the floor marker %d", firstJob.TargetOrdinal, domain.SummaryDiscoveryTargetFloor)
	}
	if committed, err := store.CommitSummary(ctx, port.SummaryCommit{
		SessionIdentity: session,
		Summary:         port.ConversationSummary{Text: "The user stated a goal.", CoveredThroughOrdinal: 5, SourceDigest: "digest-1", PromptVersion: "v1"},
	}); err != nil || !committed {
		t.Fatalf("commit first discovery result = %v, %v", committed, err)
	}
	if err := store.CompleteSummaryJob(ctx, firstJob); err != nil {
		t.Fatalf("complete first discovery: %v", err)
	}

	// A later turn triggers another discovery request with the exact same
	// literal marker (mirroring ScheduleConversation, which never varies it).
	scheduled, err = store.ScheduleSummaryJob(ctx, session, domain.SummaryDiscoveryTargetFloor, time.Now().Add(-time.Second))
	if err != nil || !scheduled {
		t.Fatalf("second discovery schedule after completion = %v, %v (must not fail with a unique-constraint error)", scheduled, err)
	}
	secondJob, err := store.ClaimSummaryJob(ctx, time.Now())
	if err != nil {
		t.Fatalf("claim second discovery: %v", err)
	}
	if secondJob.TargetOrdinal == firstJob.TargetOrdinal {
		t.Fatalf("second discovery reused the first job's identity: %d", secondJob.TargetOrdinal)
	}
	if secondJob.TargetOrdinal < domain.SummaryDiscoveryTargetFloor {
		t.Fatalf("second discovery target %d fell below the discovery floor %d", secondJob.TargetOrdinal, domain.SummaryDiscoveryTargetFloor)
	}
	if err := store.CompleteSummaryJob(ctx, secondJob); err != nil {
		t.Fatalf("complete second discovery: %v", err)
	}
}

// TestSummaryStoreDiscoveryMarkerCoalescesWhileFirstIsRunning is FIND-134
// lifecycle gate 2: a ScheduleConversation call that arrives while a
// discovery job is already 'running' must not lose the turn it observed. It
// must not error (bot.Service only logs a scheduling error and continues,
// which would otherwise drop the follow-up silently), and once the running
// job completes, the durable follow-up it left behind must still be
// claimable after a simulated restart (a fresh claim call with no in-memory
// state carried over).
func TestSummaryStoreDiscoveryMarkerCoalescesWhileFirstIsRunning(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	const session = "discovery-running"

	if scheduled, err := store.ScheduleSummaryJob(ctx, session, domain.SummaryDiscoveryTargetFloor, time.Now().Add(-time.Second)); err != nil || !scheduled {
		t.Fatalf("first discovery schedule = %v, %v", scheduled, err)
	}
	runningJob, err := store.ClaimSummaryJob(ctx, time.Now())
	if err != nil {
		t.Fatalf("claim running discovery: %v", err)
	}

	// A turn lands while the first discovery is still 'running'.
	scheduled, err := store.ScheduleSummaryJob(ctx, session, domain.SummaryDiscoveryTargetFloor, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("schedule while running returned an error instead of coalescing or following up: %v", err)
	}
	if !scheduled {
		t.Fatal("schedule while running silently dropped the request: no follow-up row remains, and the turn it observed is lost")
	}

	if committed, err := store.CommitSummary(ctx, port.SummaryCommit{
		SessionIdentity: session,
		Summary:         port.ConversationSummary{Text: "The user stated a goal.", CoveredThroughOrdinal: 5, SourceDigest: "digest-1", PromptVersion: "v1"},
	}); err != nil || !committed {
		t.Fatalf("commit running discovery result = %v, %v", committed, err)
	}
	if err := store.CompleteSummaryJob(ctx, runningJob); err != nil {
		t.Fatalf("complete running discovery: %v", err)
	}

	// Restart: a fresh claim call, no scheduler state or wake carried over,
	// must still find the follow-up the mid-flight request left behind.
	followUp, err := store.ClaimSummaryJob(ctx, time.Now())
	if err != nil {
		t.Fatalf("claim follow-up after restart: %v", err)
	}
	if followUp.TargetOrdinal == runningJob.TargetOrdinal {
		t.Fatalf("follow-up reused the running job's identity: %d", followUp.TargetOrdinal)
	}
	if followUp.TargetOrdinal < domain.SummaryDiscoveryTargetFloor {
		t.Fatalf("follow-up target %d fell below the discovery floor %d", followUp.TargetOrdinal, domain.SummaryDiscoveryTargetFloor)
	}
}

// TestSummaryStoreDiscoveryMarkerRequestsCoalesceToOneRow is FIND-134
// lifecycle gate 3: many ScheduleConversation-shaped calls in a row, with
// nothing claiming them, must coalesce to a bounded number of durable rows,
// not one row per call.
func TestSummaryStoreDiscoveryMarkerRequestsCoalesceToOneRow(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	const session = "discovery-coalesce"

	const requests = 50
	for i := 0; i < requests; i++ {
		if _, err := store.ScheduleSummaryJob(ctx, session, domain.SummaryDiscoveryTargetFloor, time.Now().Add(-time.Second)); err != nil {
			t.Fatalf("schedule request %d: %v", i, err)
		}
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM adk_context_summary_jobs WHERE session_identity = ?`, session).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("durable row count after %d unclaimed requests = %d, want 1", requests, rows)
	}
}

// TestSummaryStoreDiscoveryMarkerExhaustionFailsExplicitlyAndPreservesRows is
// FIND-135's boundary gate: if a session's job history already retains a row
// at math.MaxInt64 (corrupt or manually seeded data), no incremented marker
// exists. ScheduleSummaryJob must return the explicit
// domain.ErrSummaryDiscoveryMarkersExhausted error instead of letting
// maxUsed+1 wrap to a negative value, and the failed attempt must not lose
// any row that existed before it ran: the coalescing delete inside the same
// transaction must roll back along with the failed insert.
func TestSummaryStoreDiscoveryMarkerExhaustionFailsExplicitlyAndPreservesRows(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	const session = "discovery-exhausted"
	now := time.Now().UTC().UnixMicro()

	// A prior pending row that a naive fix would lose to an uncommitted
	// coalescing delete.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO adk_context_summary_jobs
		(session_identity, target_ordinal, status, attempts, next_attempt, created_at, updated_at)
		VALUES (?, 1, 'pending', 0, ?, ?, ?)`, session, now, now, now); err != nil {
		t.Fatalf("seed prior pending row: %v", err)
	}
	// A completed discovery row at the exact floor marker, so the plain
	// insert collides and enters the marker-allocation path.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO adk_context_summary_jobs
		(session_identity, target_ordinal, status, attempts, next_attempt, created_at, updated_at)
		VALUES (?, ?, 'done', 1, ?, ?, ?)`, session, domain.SummaryDiscoveryTargetFloor, now, now, now); err != nil {
		t.Fatalf("seed done row at the floor: %v", err)
	}
	// A row already sitting at the top of the int64 range, so no marker
	// above it exists.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO adk_context_summary_jobs
		(session_identity, target_ordinal, status, attempts, next_attempt, created_at, updated_at)
		VALUES (?, ?, 'done', 1, ?, ?, ?)`, session, int64(math.MaxInt64), now, now, now); err != nil {
		t.Fatalf("seed done row at math.MaxInt64: %v", err)
	}

	_, err := store.ScheduleSummaryJob(ctx, session, domain.SummaryDiscoveryTargetFloor, time.Now().Add(-time.Second))
	if !errors.Is(err, domain.ErrSummaryDiscoveryMarkersExhausted) {
		t.Fatalf("schedule at exhausted markers error = %v, want domain.ErrSummaryDiscoveryMarkersExhausted", err)
	}

	var pendingStillThere int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM adk_context_summary_jobs
		WHERE session_identity = ? AND target_ordinal = 1 AND status = 'pending'`, session).Scan(&pendingStillThere); err != nil {
		t.Fatal(err)
	}
	if pendingStillThere != 1 {
		t.Fatal("the failed schedule attempt did not roll back its coalescing delete: the prior pending row is gone")
	}
	var totalRows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM adk_context_summary_jobs WHERE session_identity = ?`, session).Scan(&totalRows); err != nil {
		t.Fatal(err)
	}
	if totalRows != 3 {
		t.Fatalf("row count after failed schedule = %d, want 3 (all seeded rows preserved, nothing added)", totalRows)
	}
}
