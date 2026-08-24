package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestKnowledgeLexicalQueueClaimAndCompleteCAS(t *testing.T) {
	store, _ := newTestStore(t)
	queue := NewKnowledgeLexicalQueueStore(store)
	now := time.Now().UTC().Truncate(time.Second)

	// The truth-insert enqueue trigger is not involved here: seed a queue
	// row directly so the transition contract is exercised in isolation.
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', 'kclaim_q', 4, 'pending', 0, 0, 0, '', 100, 100)`); err != nil {
		t.Fatal(err)
	}

	item, ok, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalClaim, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("ClaimNext() = %+v, %v, %v", item, ok, err)
	}
	if item.ID != "kclaim_q" || item.Generation != 4 || item.Status != domain.KnowledgeQueueProcessing || item.Attempts != 1 {
		t.Fatalf("ClaimNext() item = %+v", item)
	}
	claim := domain.KnowledgeQueueClaim{Kind: domain.KnowledgeRetrievalClaim, ID: item.ID, Generation: item.Generation, LeaseUntil: item.LeaseUntil}
	if err := queue.Complete(t.Context(), claim); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	// The completed row is not claimable again.
	if _, ok, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalClaim, now.Add(2*time.Minute), time.Minute); err != nil || ok {
		t.Fatalf("ClaimNext(after complete) = ok %v, err %v, want exhausted", ok, err)
	}
	// A stale completion for the same identity fails CAS.
	if err := queue.Complete(t.Context(), claim); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("Complete(replayed) error = %v, want ErrKnowledgeCASConflict", err)
	}
}

func TestKnowledgeLexicalQueueLeaseExpiryAndReclaim(t *testing.T) {
	store, _ := newTestStore(t)
	queue := NewKnowledgeLexicalQueueStore(store)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('document', 'kdoc_q', 1, 'processing', 2, 0, ?, '', 100, 100)`, now.Add(30*time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	// An unexpired lease cannot be claimed.
	if _, ok, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalDocument, now, time.Minute); err != nil || ok {
		t.Fatalf("ClaimNext(unexpired lease) = ok %v, err %v, want no claim", ok, err)
	}
	// After expiry the row is reclaimed with a fresh lease and incremented
	// attempts.
	item, ok, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalDocument, now.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("ClaimNext(expired) = %+v, %v, %v", item, ok, err)
	}
	if item.Attempts != 3 || item.Generation != 1 {
		t.Fatalf("reclaimed item = %+v, want attempts 3 generation 1", item)
	}
	// The expired claimant's token can no longer complete the row.
	stale := domain.KnowledgeQueueClaim{Kind: domain.KnowledgeRetrievalDocument, ID: item.ID, Generation: 1, LeaseUntil: now.Add(30 * time.Second)}
	if err := queue.Complete(t.Context(), stale); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("Complete(expired claimant) error = %v, want ErrKnowledgeCASConflict", err)
	}
}

func TestKnowledgeLexicalQueueRetryAndFailClosedCodes(t *testing.T) {
	store, _ := newTestStore(t)
	queue := NewKnowledgeLexicalQueueStore(store)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('preference', 'preference:7', 2, 'pending', 0, 0, 0, '', 100, 100)`); err != nil {
		t.Fatal(err)
	}
	item, ok, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalPreference, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("ClaimNext() = %+v, %v, %v", item, ok, err)
	}
	claim := domain.KnowledgeQueueClaim{Kind: item.Kind, ID: item.ID, Generation: item.Generation, LeaseUntil: item.LeaseUntil}
	next := now.Add(time.Minute)
	if err := queue.Retry(t.Context(), claim, next); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	// Retried rows are pending and claimable only after next_attempt.
	if _, ok, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalPreference, now, time.Minute); err != nil || ok {
		t.Fatalf("ClaimNext(before next_attempt) = ok %v, err %v", ok, err)
	}
	item, ok, err = queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalPreference, next, time.Minute)
	if err != nil || !ok {
		t.Fatalf("ClaimNext(after next_attempt) = %+v, %v, %v", item, ok, err)
	}
	claim = domain.KnowledgeQueueClaim{Kind: item.Kind, ID: item.ID, Generation: item.Generation, LeaseUntil: item.LeaseUntil}
	if err := queue.Fail(t.Context(), claim, domain.KnowledgeQueueFailureAttemptsExhausted); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	// Invalid failure codes are rejected before SQL.
	if err := queue.Fail(t.Context(), claim, "made-up-code"); err == nil {
		t.Fatal("Fail(unknown code) succeeded")
	}
	// A new mutation re-enqueues and resets the failed row.
	enqueued, err := queue.Enqueue(t.Context(), domain.KnowledgeRetrievalPreference, "preference:7")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if enqueued.Generation != 3 || enqueued.Status != domain.KnowledgeQueuePending || enqueued.Attempts != 0 {
		t.Fatalf("Enqueue() = %+v, want generation 3 pending attempts 0", enqueued)
	}
}

func TestKnowledgeLexicalQueueListAndValidation(t *testing.T) {
	store, _ := newTestStore(t)
	queue := NewKnowledgeLexicalQueueStore(store)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', 'kclaim_b', 1, 'pending', 0, 0, 0, '', 100, 100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', 'kclaim_a', 1, 'done', 0, 0, 0, '', 100, 100)`); err != nil {
		t.Fatal(err)
	}
	first, err := queue.List(t.Context(), domain.KnowledgeRetrievalClaim, "", 1)
	if err != nil || len(first) != 1 || first[0].ID != "kclaim_a" {
		t.Fatalf("List(first page) = %+v, %v", first, err)
	}
	second, err := queue.List(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_a", 1)
	if err != nil || len(second) != 1 || second[0].ID != "kclaim_b" {
		t.Fatalf("List(second page) = %+v, %v", second, err)
	}

	// Validation negatives.
	if _, _, err := queue.ClaimNext(t.Context(), "unknown", now, time.Minute); err == nil {
		t.Fatal("ClaimNext(unknown kind) succeeded")
	}
	if _, _, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalClaim, time.Time{}, time.Minute); err == nil {
		t.Fatal("ClaimNext(zero clock) succeeded")
	}
	if _, _, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalClaim, now, 0); err == nil {
		t.Fatal("ClaimNext(zero lease) succeeded")
	}
	if _, _, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalClaim, now, 2*time.Hour); err == nil {
		t.Fatal("ClaimNext(over-long lease) succeeded")
	}
	if _, err := queue.List(t.Context(), domain.KnowledgeRetrievalClaim, "", 0); err == nil {
		t.Fatal("List(zero limit) succeeded")
	}
	if _, err := queue.List(t.Context(), domain.KnowledgeRetrievalClaim, "", domain.HardMaxKnowledgeQueueListLimit+1); err == nil {
		t.Fatal("List(over-limit) succeeded")
	}
	if _, err := queue.Enqueue(t.Context(), domain.KnowledgeRetrievalClaim, ""); err == nil {
		t.Fatal("Enqueue(empty id) succeeded")
	}
	if err := queue.Complete(t.Context(), domain.KnowledgeQueueClaim{}); err == nil {
		t.Fatal("Complete(empty claim) succeeded")
	}
}

func TestKnowledgeLexicalQueueEnqueueIncrementsGenerationAtomically(t *testing.T) {
	store, _ := newTestStore(t)
	queue := NewKnowledgeLexicalQueueStore(store)

	first, err := queue.Enqueue(t.Context(), domain.KnowledgeRetrievalDocument, "kdoc_enq")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if first.Generation != 1 || first.Status != domain.KnowledgeQueuePending {
		t.Fatalf("first Enqueue() = %+v", first)
	}
	second, err := queue.Enqueue(t.Context(), domain.KnowledgeRetrievalDocument, "kdoc_enq")
	if err != nil {
		t.Fatalf("second Enqueue() error = %v", err)
	}
	if second.Generation != 2 || second.Attempts != 0 {
		t.Fatalf("second Enqueue() = %+v, want generation 2 attempts 0", second)
	}
	// A conflict reset must not create a second row.
	var count int
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM knowledge_lexical_queue WHERE item_kind = 'document' AND item_id = 'kdoc_enq'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("queue rows after re-enqueue = %d, %v, want 1", count, err)
	}
}

func TestKnowledgeLexicalQueueSurvivesCrashAndReopen(t *testing.T) {
	store, path := newTestStore(t)
	queue := NewKnowledgeLexicalQueueStore(store)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_lexical_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', 'kclaim_crash', 6, 'pending', 0, 0, 0, '', 100, 100)`); err != nil {
		t.Fatal(err)
	}
	item, ok, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalClaim, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("ClaimNext() = %+v, %v, %v", item, ok, err)
	}
	if item.Generation != 6 || item.Attempts != 1 {
		t.Fatalf("claimed item = %+v, want generation 6 attempts 1", item)
	}
	// Simulate a crash before completion: close and reopen the database.
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reopenedQueue := NewKnowledgeLexicalQueueStore(reopened)

	// The in-flight claim survives with its attempts and lease intact.
	rows, err := reopenedQueue.List(t.Context(), domain.KnowledgeRetrievalClaim, "", 16)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List(after reopen) = %+v, %v", rows, err)
	}
	if rows[0].Status != domain.KnowledgeQueueProcessing || rows[0].Attempts != 1 || rows[0].Generation != 6 {
		t.Fatalf("row after reopen = %+v, want processing attempts 1 generation 6", rows[0])
	}
	// Lease expiry after reopen reclaims the row with a preserved
	// generation and incremented attempts.
	reclaimed, ok, err := reopenedQueue.ClaimNext(t.Context(), domain.KnowledgeRetrievalClaim, now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("ClaimNext(after reopen) = %+v, %v, %v", reclaimed, ok, err)
	}
	if reclaimed.Attempts != 2 || reclaimed.Generation != 6 {
		t.Fatalf("reclaimed item = %+v, want attempts 2 generation 6", reclaimed)
	}
}
