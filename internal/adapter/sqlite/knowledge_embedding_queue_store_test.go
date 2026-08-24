package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestKnowledgeEmbeddingQueueClaimCompleteAndCASParity(t *testing.T) {
	store, _ := newTestStore(t)
	queue := NewKnowledgeEmbeddingQueueStore(store)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('claim', 'kclaim_emb_q', 4, 'pending', 0, 0, 0, '', 100, 100)`); err != nil {
		t.Fatal(err)
	}
	item, ok, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalClaim, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("ClaimNext() = %+v, %v, %v", item, ok, err)
	}
	if item.ID != "kclaim_emb_q" || item.Generation != 4 || item.Status != domain.KnowledgeQueueProcessing || item.Attempts != 1 {
		t.Fatalf("ClaimNext() item = %+v", item)
	}
	claim := domain.KnowledgeQueueClaim{Kind: domain.KnowledgeRetrievalClaim, ID: item.ID, Generation: item.Generation, LeaseUntil: item.LeaseUntil}
	if err := queue.Complete(t.Context(), claim); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if _, ok, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalClaim, now.Add(2*time.Minute), time.Minute); err != nil || ok {
		t.Fatalf("ClaimNext(after complete) = ok %v, err %v, want exhausted", ok, err)
	}
	if err := queue.Complete(t.Context(), claim); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("Complete(replayed) error = %v, want ErrKnowledgeCASConflict", err)
	}
}

func TestKnowledgeEmbeddingQueueLeaseExpiryAndReclaimParity(t *testing.T) {
	store, _ := newTestStore(t)
	queue := NewKnowledgeEmbeddingQueueStore(store)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('document', 'kdoc_emb_q', 1, 'processing', 2, 0, ?, '', 100, 100)`, now.Add(30*time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalDocument, now, time.Minute); err != nil || ok {
		t.Fatalf("ClaimNext(unexpired lease) = ok %v, err %v, want no claim", ok, err)
	}
	item, ok, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalDocument, now.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("ClaimNext(expired) = %+v, %v, %v", item, ok, err)
	}
	if item.Attempts != 3 || item.Generation != 1 {
		t.Fatalf("reclaimed item = %+v, want attempts 3 generation 1", item)
	}
	stale := domain.KnowledgeQueueClaim{Kind: domain.KnowledgeRetrievalDocument, ID: item.ID, Generation: 1, LeaseUntil: now.Add(30 * time.Second)}
	if err := queue.Complete(t.Context(), stale); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("Complete(expired claimant) error = %v, want ErrKnowledgeCASConflict", err)
	}
}

func TestKnowledgeEmbeddingQueueRetryFailAndEnqueueParity(t *testing.T) {
	store, _ := newTestStore(t)
	queue := NewKnowledgeEmbeddingQueueStore(store)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_embedding_queue (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('preference', 'preference:9', 2, 'pending', 0, 0, 0, '', 100, 100)`); err != nil {
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
	if _, ok, err := queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalPreference, now, time.Minute); err != nil || ok {
		t.Fatalf("ClaimNext(before next_attempt) = ok %v, err %v", ok, err)
	}
	item, ok, err = queue.ClaimNext(t.Context(), domain.KnowledgeRetrievalPreference, next, time.Minute)
	if err != nil || !ok {
		t.Fatalf("ClaimNext(after next_attempt) = %+v, %v, %v", item, ok, err)
	}
	claim = domain.KnowledgeQueueClaim{Kind: item.Kind, ID: item.ID, Generation: item.Generation, LeaseUntil: item.LeaseUntil}
	if err := queue.Fail(t.Context(), claim, domain.KnowledgeQueueFailureProviderInvalid); err != nil {
		t.Fatalf("Fail(provider_invalid) error = %v", err)
	}
	if err := queue.Fail(t.Context(), claim, "made-up-code"); err == nil {
		t.Fatal("Fail(unknown code) succeeded")
	}
	enqueued, err := queue.Enqueue(t.Context(), domain.KnowledgeRetrievalPreference, "preference:9")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if enqueued.Generation != 3 || enqueued.Status != domain.KnowledgeQueuePending || enqueued.Attempts != 0 {
		t.Fatalf("Enqueue() = %+v, want generation 3 pending attempts 0", enqueued)
	}
}

func TestKnowledgeEmbeddingQueueIsolationFromLexicalQueue(t *testing.T) {
	store, _ := newTestStore(t)
	embedding := NewKnowledgeEmbeddingQueueStore(store)
	lexical := NewKnowledgeLexicalQueueStore(store)

	if _, err := embedding.Enqueue(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_isolated"); err != nil {
		t.Fatalf("embedding Enqueue() error = %v", err)
	}
	if _, err := lexical.Enqueue(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_lex_only"); err != nil {
		t.Fatalf("lexical Enqueue() error = %v", err)
	}
	// Each store sees only its own table's rows.
	embeddingRows, err := embedding.List(t.Context(), domain.KnowledgeRetrievalClaim, "", 16)
	if err != nil || len(embeddingRows) != 1 || embeddingRows[0].ID != "kclaim_isolated" {
		t.Fatalf("embedding List() = %+v, %v", embeddingRows, err)
	}
	lexicalRows, err := lexical.List(t.Context(), domain.KnowledgeRetrievalClaim, "", 16)
	if err != nil || len(lexicalRows) != 1 || lexicalRows[0].ID != "kclaim_lex_only" {
		t.Fatalf("lexical List() = %+v, %v", lexicalRows, err)
	}
	// Claims never cross tables.
	if _, ok, err := embedding.ClaimNext(t.Context(), domain.KnowledgeRetrievalClaim, time.Now().UTC(), time.Minute); err != nil || !ok {
		t.Fatalf("embedding ClaimNext() = ok %v, err %v", ok, err)
	}
	if _, ok, err := lexical.ClaimNext(t.Context(), domain.KnowledgeRetrievalClaim, time.Now().UTC(), time.Minute); err != nil || !ok {
		t.Fatalf("lexical ClaimNext() = ok %v, err %v", ok, err)
	}
	// Depths are table-scoped.
	embeddingDepth, err := embedding.EmbeddingDepth(t.Context())
	if err != nil || embeddingDepth != 1 {
		t.Fatalf("embedding EmbeddingDepth() = %d, %v, want 1 processing row", embeddingDepth, err)
	}
	lexicalDepth, err := lexical.LexicalDepth(t.Context())
	if err != nil || lexicalDepth != 1 {
		t.Fatalf("lexical LexicalDepth() = %d, %v, want 1 processing row", lexicalDepth, err)
	}
}
