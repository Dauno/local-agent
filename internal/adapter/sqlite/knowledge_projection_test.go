package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestKnowledgeProjectionBatchClaimCoalescesPendingTriggers(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	ctx := t.Context()
	for range 3 {
		if err := store.EnqueueProjection(ctx); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(items) != 3 {
		t.Fatalf("batch claim = %d items, %v; want 3", len(items), err)
	}
	for _, item := range items {
		if item.Status != domain.KnowledgeProjectionProcessing || item.Attempts != 1 || item.LeaseUntil.IsZero() {
			t.Fatalf("claimed item = %#v", item)
		}
	}
	if again, err := store.ClaimProjectionBatch(ctx); err != nil || len(again) != 0 {
		t.Fatalf("claim while leased = %v, %v; want none", again, err)
	}
}

func TestKnowledgeProjectionCompleteMarksOnlyClaimedBatch(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	ctx := t.Context()
	for range 4 {
		if err := store.EnqueueProjection(ctx); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(first) != 4 {
		t.Fatalf("first batch = %d items, %v", len(first), err)
	}
	ids := make([]int, len(first))
	for i, item := range first {
		ids[i] = item.ID
	}
	if err := store.CompleteProjectionBatch(ctx, ids, first[0].LeaseUntil); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueProjection(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(second) != 1 {
		t.Fatalf("second batch after enqueue = %d items, %v; want 1 new trigger", len(second), err)
	}
}

func TestKnowledgeProjectionCompleteCASConflictKeepsRowsForRecovery(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	ctx := t.Context()
	if err := store.EnqueueProjection(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim = %v, %v", items, err)
	}
	item := items[0]
	if err := store.CompleteProjectionBatch(ctx, []int{item.ID}, item.LeaseUntil.Add(-time.Hour)); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("stale complete error = %v, want ErrKnowledgeCASConflict", err)
	}
	// The row must still be processing: a completion conflict never loses
	// the trigger.
	var status string
	if err := store.db.QueryRowContext(ctx, `
		SELECT status FROM knowledge_projection_outbox WHERE id = ?`, item.ID).Scan(&status); err != nil || status != string(domain.KnowledgeProjectionProcessing) {
		t.Fatalf("row after conflict = %q, %v; want processing", status, err)
	}
	if err := store.CompleteProjectionBatch(ctx, []int{item.ID}, item.LeaseUntil); err != nil {
		t.Fatalf("recover complete = %v", err)
	}
}

func TestKnowledgeProjectionRetryPreservesAttempts(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	ctx := t.Context()
	if err := store.EnqueueProjection(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim = %v, %v", items, err)
	}
	item := items[0]
	if item.Attempts != 1 {
		t.Fatalf("attempts after first claim = %d, want 1", item.Attempts)
	}
	next := time.Now().UTC().Add(time.Minute)
	if err := store.RetryProjectionBatch(ctx, []int{item.ID}, item.LeaseUntil, next); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var nextAttempt int64
	var status string
	if err := store.db.QueryRowContext(ctx, `
		SELECT attempts, next_attempt, status FROM knowledge_projection_outbox WHERE id = ?`, item.ID).Scan(&attempts, &nextAttempt, &status); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("attempts after retry = %d, want 1 preserved", attempts)
	}
	if status != string(domain.KnowledgeProjectionPending) || nextAttempt != next.UnixNano() {
		t.Fatalf("row after retry = status %q next_attempt %d; want pending at %d", status, nextAttempt, next.UnixNano())
	}
	if _, err := store.db.ExecContext(ctx, `
		UPDATE knowledge_projection_outbox SET next_attempt = 0 WHERE id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].Attempts != 2 {
		t.Fatalf("reclaim = %v, %v; want one item with attempts 2", reclaimed, err)
	}
}

func TestKnowledgeProjectionCleanupDeferralPreservesBudget(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	ctx := t.Context()
	if err := store.EnqueueProjection(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim = %v, %v", items, err)
	}
	item := items[0]
	next := time.Now().UTC().Add(time.Minute)
	if err := store.DeferProjectionCleanupBatch(ctx, []int{item.ID}, item.LeaseUntil, next); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var nextAttempt int64
	var status string
	if err := store.db.QueryRowContext(ctx, `
		SELECT attempts, next_attempt, status FROM knowledge_projection_outbox WHERE id = ?`, item.ID).Scan(&attempts, &nextAttempt, &status); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("attempts after cleanup deferral = %d, want 0 (pre-claim budget restored)", attempts)
	}
	if status != string(domain.KnowledgeProjectionPending) || nextAttempt != next.UnixNano() {
		t.Fatalf("row after deferral = status %q next_attempt %d; want pending at %d", status, nextAttempt, next.UnixNano())
	}
	// The next claim consumes the same budget value as the first: cleanup
	// retries never advance the projection attempt counter.
	if _, err := store.db.ExecContext(ctx, `
		UPDATE knowledge_projection_outbox SET next_attempt = 0 WHERE id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].Attempts != 1 {
		t.Fatalf("reclaim after deferral = %v, %v; want one item with attempts 1 again", reclaimed, err)
	}
	if err := store.DeferProjectionCleanupBatch(ctx, []int{item.ID}, item.LeaseUntil.Add(-time.Hour), next); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("stale deferral error = %v, want ErrKnowledgeCASConflict", err)
	}
}

func TestKnowledgeProjectionFailPersistsBoundedCode(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	ctx := t.Context()
	if err := store.EnqueueProjection(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim = %v, %v", items, err)
	}
	item := items[0]
	if err := store.FailProjectionBatch(ctx, []int{item.ID}, item.LeaseUntil, port.KnowledgeProjectionExhaustedCode); err != nil {
		t.Fatal(err)
	}
	var status, lastError string
	if err := store.db.QueryRowContext(ctx, `
		SELECT status, last_error FROM knowledge_projection_outbox WHERE id = ?`, item.ID).Scan(&status, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.KnowledgeProjectionFailed) || lastError != port.KnowledgeProjectionExhaustedCode {
		t.Fatalf("failed row = %q %q", status, lastError)
	}
	if err := store.FailProjectionBatch(ctx, []int{item.ID}, item.LeaseUntil, "a code that is far too long for the bounded terminal failure column and would leak detail"); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("oversized code error = %v, want ErrKnowledgeValidation", err)
	}
}

func TestKnowledgeProjectionLeaseExpiryRecoversBatchAcrossRestart(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	ctx := t.Context()
	for range 2 {
		if err := store.EnqueueProjection(ctx); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(items) != 2 {
		t.Fatalf("claim = %v, %v", items, err)
	}
	// Simulate a crash after claim: the lease expires while rows stay
	// processing.
	if _, err := store.db.ExecContext(ctx, `
		UPDATE knowledge_projection_outbox SET lease_until = 0 WHERE status = 'processing'`); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(recovered) != 2 {
		t.Fatalf("recovery claim = %v, %v; want both rows", recovered, err)
	}
	for _, item := range recovered {
		if item.Attempts != 2 {
			t.Fatalf("recovered attempts = %d, want 2", item.Attempts)
		}
	}
	if err := store.CompleteProjectionBatch(ctx, []int{recovered[0].ID, recovered[1].ID}, recovered[0].LeaseUntil); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeProjectionRetryDoesNotReturnRowsBeforeNextAttempt(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	ctx := t.Context()
	if err := store.EnqueueProjection(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim = %v, %v", items, err)
	}
	item := items[0]
	if err := store.RetryProjectionBatch(ctx, []int{item.ID}, item.LeaseUntil, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if again, err := store.ClaimProjectionBatch(ctx); err != nil || len(again) != 0 {
		t.Fatalf("claim before next_attempt = %v, %v; want none", again, err)
	}
}

func TestKnowledgeProjectionCleanupKeepsPendingAndProcessing(t *testing.T) {
	store, _ := newKnowledgeTestStore(t)
	ctx := t.Context()
	if err := store.EnqueueProjection(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim = %v, %v", items, err)
	}
	if err := store.CompleteProjectionBatch(ctx, []int{items[0].ID}, items[0].LeaseUntil); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueProjection(ctx); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Hour)
	// The second pending trigger is not done and must survive retention
	// cleanup of terminal rows.
	if err := store.CleanupProjection(ctx, future); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.ClaimProjectionBatch(ctx)
	if err != nil || len(remaining) != 1 {
		t.Fatalf("pending after cleanup = %v, %v; want one", remaining, err)
	}
}
