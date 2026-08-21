package knowledge

import (
	"fmt"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/testutil"
)

// TestLexicalWorkerClaimsExactlyBatchSizePerKindPerTick pins FIND-123: with
// more claimable rows than BatchSize, one tick must claim exactly BatchSize
// rows for that kind and leave the rest pending. Sabotaging
// `claimed < w.cfg.BatchSize` in lexical_worker.go's drainKind to
// `claimed < w.cfg.BatchSize+1` must fail this test.
func TestLexicalWorkerClaimsExactlyBatchSizePerKindPerTick(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	const batchSize = 2
	const extra = 3
	source := newWorkerFakeSource()
	items := make([]domain.KnowledgeQueueItem, 0, batchSize+extra)
	for i := range batchSize + extra {
		id := fmt.Sprintf("kclaim_batch_%d", i)
		items = append(items, domain.KnowledgeQueueItem{Kind: domain.KnowledgeRetrievalClaim, ID: id, Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now})
		source.items[string(domain.KnowledgeRetrievalClaim)+"\x00"+id] = workerTestProjectClaim(id, "subject", "value", domain.KnowledgeClaimAsserted, 1)
	}
	queue := newWorkerFakeQueue(items)
	index := newWorkerFakeIndex(nil)
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	cfg := workerTestConfig()
	cfg.BatchSize = batchSize
	worker, err := NewLexicalWorker(cfg, workerTestDependencies(queue, source, index, lister, resolver))
	if err != nil {
		t.Fatalf("NewLexicalWorker() error = %v", err)
	}
	worker.tick(t.Context())

	if len(queue.complete) != batchSize {
		t.Fatalf("completed claims = %d, want exactly BatchSize = %d", len(queue.complete), batchSize)
	}
	pending := 0
	for _, item := range queue.items {
		if item.Kind == domain.KnowledgeRetrievalClaim && item.Status == domain.KnowledgeQueuePending {
			pending++
		}
	}
	if pending != extra {
		t.Fatalf("pending claims after one tick = %d, want %d left for the next tick", pending, extra)
	}
}

// TestEmbeddingWorkerClaimsExactlyBatchSizePerKindPerTick pins FIND-123 for
// the embedding worker's own BatchSize gate. Sabotaging
// `len(claims) < w.cfg.BatchSize` in embedding_worker.go's drainKind to
// `len(claims) < w.cfg.BatchSize+1` must fail this test.
func TestEmbeddingWorkerClaimsExactlyBatchSizePerKindPerTick(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	const batchSize = 2
	const extra = 3
	source := newWorkerFakeSource()
	items := make([]domain.KnowledgeQueueItem, 0, batchSize+extra)
	for i := range batchSize + extra {
		id := fmt.Sprintf("kclaim_ebatch_%d", i)
		items = append(items, domain.KnowledgeQueueItem{Kind: domain.KnowledgeRetrievalClaim, ID: id, Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now})
		source.items[string(domain.KnowledgeRetrievalClaim)+"\x00"+id] = workerTestProjectClaim(id, "subject", "value", domain.KnowledgeClaimAsserted, 1)
	}
	queue := newWorkerFakeQueue(items)
	index := newWorkerFakeVectorIndex(nil)
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	provider := testutil.NewFakeEmbeddingProvider(3)
	cfg := embeddingWorkerTestConfig()
	cfg.BatchSize = batchSize
	worker, err := NewEmbeddingWorker(cfg, embeddingWorkerTestDependencies(queue, source, index, lister, resolver, provider))
	if err != nil {
		t.Fatalf("NewEmbeddingWorker() error = %v", err)
	}
	worker.tick(t.Context())

	if len(queue.complete) != batchSize {
		t.Fatalf("completed claims = %d, want exactly BatchSize = %d", len(queue.complete), batchSize)
	}
	pending := 0
	for _, item := range queue.items {
		if item.Kind == domain.KnowledgeRetrievalClaim && item.Status == domain.KnowledgeQueuePending {
			pending++
		}
	}
	if pending != extra {
		t.Fatalf("pending claims after one tick = %d, want %d left for the next tick", pending, extra)
	}
}
