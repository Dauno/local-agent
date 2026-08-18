package knowledge

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/testutil"
)

// workerFakeVectorIndex is a scripted worker-owned vector index for
// embedding worker tests. ListVector honors fingerprint, cursor, ordering,
// and limit so the worker's paged state reads terminate exactly like the
// SQLite store's.
type workerFakeVectorIndex struct {
	mu         sync.Mutex
	rows       map[string]port.KnowledgeVectorIndexRow
	replaced   []port.KnowledgeVectorIndexRow
	deleted    [][2]string
	cleared    bool
	replaceErr error
	listErr    error
}

func newWorkerFakeVectorIndex(rows []port.KnowledgeVectorIndexRow) *workerFakeVectorIndex {
	index := &workerFakeVectorIndex{rows: make(map[string]port.KnowledgeVectorIndexRow)}
	for _, row := range rows {
		index.rows[string(row.Kind)+"\x00"+row.ID+"\x00"+row.Fingerprint] = row
	}
	return index
}

func (f *workerFakeVectorIndex) ReplaceVector(_ context.Context, kind domain.KnowledgeRetrievalItemKind, id string, revision int, sourceDigest, fingerprint string, vector []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replaceErr != nil {
		return f.replaceErr
	}
	row := port.KnowledgeVectorIndexRow{Kind: kind, ID: id, Revision: revision, SourceDigest: sourceDigest, Fingerprint: fingerprint}
	f.rows[string(kind)+"\x00"+id+"\x00"+fingerprint] = row
	f.replaced = append(f.replaced, row)
	return nil
}

func (f *workerFakeVectorIndex) DeleteVector(_ context.Context, kind domain.KnowledgeRetrievalItemKind, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, [2]string{string(kind), id})
	for key := range f.rows {
		if f.rows[key].Kind == kind && f.rows[key].ID == id {
			delete(f.rows, key)
		}
	}
	return nil
}

func (f *workerFakeVectorIndex) ListVector(_ context.Context, kind domain.KnowledgeRetrievalItemKind, fingerprint, afterID string, limit int) ([]port.KnowledgeVectorIndexRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var sorted []port.KnowledgeVectorIndexRow
	for _, row := range f.rows {
		if row.Kind == kind && row.Fingerprint == fingerprint && row.ID > afterID {
			sorted = append(sorted, row)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	if len(sorted) > limit {
		sorted = sorted[:limit]
	}
	return sorted, nil
}

func (f *workerFakeVectorIndex) ClearVector(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = true
	clear(f.rows)
	return nil
}

// validDigest is a placeholder 64-character lowercase hex source digest
// for seeded vector rows whose text content is irrelevant to the test.
const validDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func embeddingWorkerTestConfig() EmbeddingWorkerConfig {
	return EmbeddingWorkerConfig{
		Interval:   time.Hour,
		MaxRetries: 3,
		BatchSize:  8,
		Limits:     domain.DefaultKnowledgeRetrievalLimits(),
		ProviderID: "acme",
		Model:      "m-3",
		Dimensions: 3,
	}
}

func embeddingWorkerFingerprint() string {
	return domain.ModelFingerprint("acme", "m-3", 3)
}

func embeddingWorkerTestDependencies(queue port.KnowledgeQueueStore, source *workerFakeSource, index *workerFakeVectorIndex, lister *workerFakeLister, resolver *workerFakeResolver, provider port.EmbeddingProvider) EmbeddingWorkerDependencies {
	return EmbeddingWorkerDependencies{
		Queue: queue, Source: source, Index: index, Provider: provider,
		Resolver: resolver, Lister: lister,
		Clock:  &workerTestClock{now: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)},
		Logger: workerTestLogger{},
		Redact: func(value string) string {
			if value == "SECRET" {
				return ""
			}
			return value
		},
	}
}

func newEmbeddingWorker(t *testing.T, queue *workerFakeQueue, source *workerFakeSource, index *workerFakeVectorIndex, lister *workerFakeLister, resolver *workerFakeResolver, provider port.EmbeddingProvider) *EmbeddingWorker {
	t.Helper()
	worker, err := NewEmbeddingWorker(embeddingWorkerTestConfig(), embeddingWorkerTestDependencies(queue, source, index, lister, resolver, provider))
	if err != nil {
		t.Fatalf("NewEmbeddingWorker() error = %v", err)
	}
	return worker
}

func claimDigest(t *testing.T, item port.KnowledgeAuthoritativeItem, content string) string {
	t.Helper()
	text, err := BuildKnowledgeIndexText(item.Kind, item, content, func(value string) string {
		if value == "SECRET" {
			return ""
		}
		return value
	})
	if err != nil {
		t.Fatalf("BuildKnowledgeIndexText() error = %v", err)
	}
	return text.SourceDigest
}

func TestEmbeddingWorkerBatchesEmbedsAndSkipsUnchanged(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_e_a", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_e_b", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_e_c", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	source := newWorkerFakeSource()
	claimA := workerTestProjectClaim("kclaim_e_a", "subject A", "value A", domain.KnowledgeClaimAsserted, 2)
	claimB := workerTestProjectClaim("kclaim_e_b", "subject B", "value B", domain.KnowledgeClaimAsserted, 3)
	claimC := workerTestProjectClaim("kclaim_e_c", "subject C", "value C", domain.KnowledgeClaimAsserted, 1)
	source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_e_a"] = claimA
	source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_e_b"] = claimB
	source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_e_c"] = claimC
	// B is unchanged (matching revision, digest, and fingerprint) and must
	// never reach the provider. C has the same revision but a stale digest
	// (redactor rotation) and must be re-embedded.
	fp := embeddingWorkerFingerprint()
	index := newWorkerFakeVectorIndex([]port.KnowledgeVectorIndexRow{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_e_b", Revision: 3, SourceDigest: claimDigest(t, claimB, ""), Fingerprint: fp},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_e_c", Revision: 1, SourceDigest: "stale-digest", Fingerprint: fp},
	})
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	provider := testutil.NewFakeEmbeddingProvider(3)
	worker := newEmbeddingWorker(t, queue, source, index, lister, resolver, provider)
	worker.tick(t.Context())

	recorded := provider.RecordedInputs()
	if len(recorded) != 2 {
		t.Fatalf("provider inputs = %q, want exactly the two texts needing embedding", recorded)
	}
	if recorded[0] != "subject A\nvalue A" || recorded[1] != "subject C\nvalue C" {
		t.Fatalf("provider inputs = %q, want A then C in claim order", recorded)
	}
	if len(queue.complete) != 3 || len(queue.fails) != 0 || len(queue.retries) != 0 {
		t.Fatalf("transitions = complete %d fails %d retries %d, want 3 completes", len(queue.complete), len(queue.fails), len(queue.retries))
	}
	if len(index.replaced) != 2 || index.replaced[0].ID != "kclaim_e_a" || index.replaced[1].ID != "kclaim_e_c" {
		t.Fatalf("replacements = %+v, want A and C only", index.replaced)
	}
	for _, replaced := range index.replaced {
		if replaced.Fingerprint != fp {
			t.Fatalf("replacement fingerprint = %q, want the resolved fingerprint %q", replaced.Fingerprint, fp)
		}
	}
}

func TestEmbeddingWorkerSkipWhenUnchangedNeverCallsEmbed(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalPreference, ID: "preference:11", Generation: 2, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	source := newWorkerFakeSource()
	preference := port.KnowledgeAuthoritativeItem{
		Kind: domain.KnowledgeRetrievalPreference, ID: "preference:11",
		Preference: &domain.KnowledgePreference{
			ID: 11, Key: "pref key", Value: domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "pref value"},
			Status: domain.KnowledgePreferenceActive, Revision: 4,
		},
	}
	source.items[string(domain.KnowledgeRetrievalPreference)+"\x00preference:11"] = preference
	index := newWorkerFakeVectorIndex([]port.KnowledgeVectorIndexRow{
		{Kind: domain.KnowledgeRetrievalPreference, ID: "preference:11", Revision: 4, SourceDigest: claimDigest(t, preference, ""), Fingerprint: embeddingWorkerFingerprint()},
	})
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	provider := testutil.NewFakeEmbeddingProvider(3)
	worker := newEmbeddingWorker(t, queue, source, index, lister, resolver, provider)
	worker.tick(t.Context())

	if provider.CallCount() != 0 {
		t.Fatalf("provider calls = %d, want zero for an unchanged item", provider.CallCount())
	}
	if len(queue.complete) != 1 {
		t.Fatalf("completions = %d, want 1", len(queue.complete))
	}
	if len(index.replaced) != 0 || len(index.deleted) != 0 {
		t.Fatalf("unchanged item must not replace or delete rows: replaced %d deleted %d", len(index.replaced), len(index.deleted))
	}
}

func TestEmbeddingWorkerChangedDigestSameRevisionStillEmbeds(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_rotated", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	source := newWorkerFakeSource()
	claim := workerTestProjectClaim("kclaim_rotated", "subject", "value", domain.KnowledgeClaimAsserted, 7)
	source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_rotated"] = claim
	// Same revision 7, but the stored digest reflects the pre-rotation
	// redacted text.
	index := newWorkerFakeVectorIndex([]port.KnowledgeVectorIndexRow{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_rotated", Revision: 7, SourceDigest: "stale-redactor-digest", Fingerprint: embeddingWorkerFingerprint()},
	})
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	provider := testutil.NewFakeEmbeddingProvider(3)
	worker := newEmbeddingWorker(t, queue, source, index, lister, resolver, provider)
	worker.tick(t.Context())

	if provider.CallCount() != 1 {
		t.Fatalf("provider calls = %d, want 1 for a redactor-rotated digest", provider.CallCount())
	}
	if len(index.replaced) != 1 || index.replaced[0].Revision != 7 {
		t.Fatalf("replacements = %+v, want the same revision re-embedded", index.replaced)
	}
	if len(queue.complete) != 1 {
		t.Fatalf("completions = %d, want 1", len(queue.complete))
	}
}

func TestEmbeddingWorkerMalformedOutputFailsProviderInvalid(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		vectors [][]float32
	}{
		{"wrong dimension count", [][]float32{{0.5, 0.5}}},
		{"non-finite value", [][]float32{{1, float32(math.NaN()), 0}}},
		{"zero norm", [][]float32{{0, 0, 0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
				{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_malformed", Generation: 1, Status: domain.KnowledgeQueuePending, Attempts: 1, CreatedAt: now, UpdatedAt: now},
			})
			source := newWorkerFakeSource()
			source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_malformed"] = workerTestProjectClaim("kclaim_malformed", "subject", "value", domain.KnowledgeClaimAsserted, 1)
			index := newWorkerFakeVectorIndex(nil)
			resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
			lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
			provider := testutil.NewFakeEmbeddingProvider(3).SetVectors(tc.vectors)
			worker := newEmbeddingWorker(t, queue, source, index, lister, resolver, provider)
			worker.tick(t.Context())

			if len(queue.fails) != 1 || queue.fails[0].code != domain.KnowledgeQueueFailureProviderInvalid {
				t.Fatalf("fails = %+v, want one terminal provider_invalid", queue.fails)
			}
			if len(queue.retries) != 0 || len(queue.complete) != 0 {
				t.Fatalf("malformed output must never retry or complete: retries %d completes %d", len(queue.retries), len(queue.complete))
			}
			if len(index.replaced) != 0 {
				t.Fatalf("malformed output must never reach the index: %+v", index.replaced)
			}
		})
	}
}

func TestEmbeddingWorkerTransientErrorRetriesThenExhausts(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_retry_e", Generation: 1, Status: domain.KnowledgeQueuePending, Attempts: 1, CreatedAt: now, UpdatedAt: now},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_exhaust_e", Generation: 1, Status: domain.KnowledgeQueuePending, Attempts: 3, CreatedAt: now, UpdatedAt: now},
	})
	source := newWorkerFakeSource()
	source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_retry_e"] = workerTestProjectClaim("kclaim_retry_e", "subject", "value", domain.KnowledgeClaimAsserted, 1)
	source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_exhaust_e"] = workerTestProjectClaim("kclaim_exhaust_e", "subject", "value", domain.KnowledgeClaimAsserted, 1)
	index := newWorkerFakeVectorIndex(nil)
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	provider := testutil.NewFakeEmbeddingProvider(3).SetErr(errors.New("provider down"))
	worker := newEmbeddingWorker(t, queue, source, index, lister, resolver, provider)
	worker.tick(t.Context())

	if len(queue.retries) != 1 || queue.retries[0].claim.ID != "kclaim_retry_e" {
		t.Fatalf("retries = %+v, want one retry for kclaim_retry_e", queue.retries)
	}
	if !queue.retries[0].next.After(now) {
		t.Fatalf("retry next attempt %v is not in the future", queue.retries[0].next)
	}
	if len(queue.fails) != 1 || queue.fails[0].code != domain.KnowledgeQueueFailureAttemptsExhausted || queue.fails[0].claim.ID != "kclaim_exhaust_e" {
		t.Fatalf("fails = %+v, want attempts_exhausted for kclaim_exhaust_e", queue.fails)
	}
}

func TestEmbeddingWorkerMissingAndRedactedEmptyDeleteAndComplete(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_gone_e", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_secret_e", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	source := newWorkerFakeSource()
	source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_secret_e"] = workerTestProjectClaim("kclaim_secret_e", "SECRET", "SECRET", domain.KnowledgeClaimAsserted, 1)
	fp := embeddingWorkerFingerprint()
	index := newWorkerFakeVectorIndex([]port.KnowledgeVectorIndexRow{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_gone_e", Revision: 2, SourceDigest: validDigest, Fingerprint: fp},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_secret_e", Revision: 1, SourceDigest: validDigest, Fingerprint: fp},
	})
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	provider := testutil.NewFakeEmbeddingProvider(3)
	worker := newEmbeddingWorker(t, queue, source, index, lister, resolver, provider)
	worker.tick(t.Context())

	if provider.CallCount() != 0 {
		t.Fatalf("provider calls = %d, want zero", provider.CallCount())
	}
	if len(queue.complete) != 2 || len(queue.fails) != 0 || len(queue.retries) != 0 {
		t.Fatalf("transitions = complete %d fails %d retries %d, want 2 completes", len(queue.complete), len(queue.fails), len(queue.retries))
	}
	if len(index.deleted) != 2 {
		t.Fatalf("deletions = %v, want both stale rows removed", index.deleted)
	}
}

func TestEmbeddingWorkerUnverifiableDocumentFailsSourceInvalid(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_bad_e", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	source := newWorkerFakeSource()
	source.items[string(domain.KnowledgeRetrievalDocument)+"\x00kdoc_bad_e"] = port.KnowledgeAuthoritativeItem{
		Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_bad_e",
		Document: &domain.KnowledgeDocument{
			ID: "kdoc_bad_e", Subject: "bad doc", ScopeKind: domain.KnowledgeScopeGlobal,
			ContentDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ContentHandle: "memory_topics:t:revision:1",
			SourceID:      "t", SourceRev: 1,
			Provenance: domain.KnowledgeProvenanceLegacyCurated, Status: domain.KnowledgeDocumentActive, Revision: 1,
		},
	}
	index := newWorkerFakeVectorIndex([]port.KnowledgeVectorIndexRow{{Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_bad_e", Revision: 1, SourceDigest: validDigest, Fingerprint: embeddingWorkerFingerprint()}})
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{"kdoc_bad_e": port.ErrKnowledgeUnavailable}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	provider := testutil.NewFakeEmbeddingProvider(3)
	worker := newEmbeddingWorker(t, queue, source, index, lister, resolver, provider)
	worker.tick(t.Context())

	if len(queue.fails) != 1 || queue.fails[0].code != domain.KnowledgeQueueFailureSourceInvalid {
		t.Fatalf("fails = %+v, want one source_invalid", queue.fails)
	}
	if len(index.deleted) != 1 || len(queue.complete) != 0 {
		t.Fatalf("deletions %d completes %d, want 1 deletion and no completion", len(index.deleted), len(queue.complete))
	}
	if provider.CallCount() != 0 {
		t.Fatalf("provider calls = %d, want zero for an unverifiable document", provider.CallCount())
	}
}

func TestEmbeddingWorkerConcurrentMutationSurvivesStaleCompletion(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_race_e", Generation: 5, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	source := newWorkerFakeSource()
	source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_race_e"] = workerTestProjectClaim("kclaim_race_e", "race subject", "value", domain.KnowledgeClaimAsserted, 5)
	index := newWorkerFakeVectorIndex(nil)
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	queue.completeErr = port.ErrKnowledgeCASConflict
	provider := testutil.NewFakeEmbeddingProvider(3)
	worker := newEmbeddingWorker(t, queue, source, index, lister, resolver, provider)
	worker.tick(t.Context())

	if len(queue.complete) != 1 {
		t.Fatalf("completions = %d, want the conflicting attempt recorded", len(queue.complete))
	}
	if len(queue.fails) != 0 || len(queue.retries) != 0 {
		t.Fatalf("conflicted completion must not fail or retry the row: fails %d retries %d", len(queue.fails), len(queue.retries))
	}
	if len(index.replaced) != 1 {
		t.Fatalf("replacements = %+v, want the vector row written before the CAS conflict", index.replaced)
	}
}

func TestEmbeddingWorkerReconcileEnqueuesTruthAndOrphans(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_recon", Generation: 1, Status: domain.KnowledgeQueueDone, CreatedAt: now, UpdatedAt: now},
	})
	fp := embeddingWorkerFingerprint()
	index := newWorkerFakeVectorIndex([]port.KnowledgeVectorIndexRow{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_a", Revision: 3, SourceDigest: validDigest, Fingerprint: fp},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_orphan", Revision: 1, SourceDigest: validDigest, Fingerprint: fp},
	})
	source := newWorkerFakeSource()
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{
		domain.KnowledgeRetrievalClaim: {
			{ID: "kclaim_a", Revision: 4},
			{ID: "kclaim_b", Revision: 1},
		},
	}}
	provider := testutil.NewFakeEmbeddingProvider(3)
	worker := newEmbeddingWorker(t, queue, source, index, lister, resolver, provider)
	if err := worker.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	enqueued := map[string]int{}
	for _, pair := range queue.enqueued {
		enqueued[pair[1]]++
	}
	// Every truth identity is re-enqueued unconditionally (redactor
	// rotations and provider configuration changes must rebuild vectors)
	// and the orphan vector identity is enqueued for cleanup.
	if enqueued["kclaim_a"] != 1 || enqueued["kclaim_b"] != 1 || enqueued["kclaim_orphan"] != 1 {
		t.Fatalf("reconcile enqueues = %v, want A, B (truth) and the orphan", queue.enqueued)
	}
	if provider.CallCount() != 0 {
		t.Fatalf("Reconcile() must never call the provider: %d calls", provider.CallCount())
	}
}

func TestEmbeddingWorkerRebuildClearsAndReenqueuesTruth(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_rebuild_e", Generation: 1, Status: domain.KnowledgeQueueDone, CreatedAt: now, UpdatedAt: now},
	})
	index := newWorkerFakeVectorIndex([]port.KnowledgeVectorIndexRow{
		{Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_rebuild_e", Revision: 1, SourceDigest: validDigest, Fingerprint: embeddingWorkerFingerprint()},
	})
	source := newWorkerFakeSource()
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{
		domain.KnowledgeRetrievalDocument: {{ID: "kdoc_rebuild_e", Revision: 1}},
	}}
	provider := testutil.NewFakeEmbeddingProvider(3)
	worker := newEmbeddingWorker(t, queue, source, index, lister, resolver, provider)
	if err := worker.Rebuild(t.Context()); err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if !index.cleared {
		t.Fatal("Rebuild() did not clear the vector index")
	}
	found := false
	for _, pair := range queue.enqueued {
		if pair[1] == "kdoc_rebuild_e" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Rebuild() enqueues = %v, want kdoc_rebuild_e", queue.enqueued)
	}
}

func TestEmbeddingWorkerRunCancellationIsAwaitable(t *testing.T) {
	queue := newWorkerFakeQueue(nil)
	source := newWorkerFakeSource()
	index := newWorkerFakeVectorIndex(nil)
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	provider := testutil.NewFakeEmbeddingProvider(3)
	worker := newEmbeddingWorker(t, queue, source, index, lister, resolver, provider)
	ctx, cancel := context.WithCancel(t.Context())
	runDone := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(runDone)
	}()
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
	if err := worker.WaitStopped(t.Context()); err != nil {
		t.Fatalf("WaitStopped() error = %v", err)
	}
}

func TestEmbeddingWorkerRejectsInvalidConfiguration(t *testing.T) {
	deps := embeddingWorkerTestDependencies(newWorkerFakeQueue(nil), newWorkerFakeSource(), newWorkerFakeVectorIndex(nil), &workerFakeLister{}, &workerFakeResolver{}, testutil.NewFakeEmbeddingProvider(3))
	cfg := embeddingWorkerTestConfig()
	cfg.Interval = 0
	if _, err := NewEmbeddingWorker(cfg, deps); err == nil {
		t.Fatal("NewEmbeddingWorker(zero interval) succeeded")
	}
	cfg = embeddingWorkerTestConfig()
	cfg.BatchSize = 0
	if _, err := NewEmbeddingWorker(cfg, deps); err == nil {
		t.Fatal("NewEmbeddingWorker(zero batch) succeeded")
	}
	cfg = embeddingWorkerTestConfig()
	cfg.BatchSize = domain.HardMaxKnowledgeRetrievalWorkerBatchSize + 1
	if _, err := NewEmbeddingWorker(cfg, deps); err == nil {
		t.Fatal("NewEmbeddingWorker(oversized batch) succeeded")
	}
	cfg = embeddingWorkerTestConfig()
	cfg.Dimensions = 0
	if _, err := NewEmbeddingWorker(cfg, deps); err == nil {
		t.Fatal("NewEmbeddingWorker(zero dimensions) succeeded")
	}
	cfg = embeddingWorkerTestConfig()
	cfg.Dimensions = domain.HardMaxKnowledgeEmbeddingDimensions + 1
	if _, err := NewEmbeddingWorker(cfg, deps); err == nil {
		t.Fatal("NewEmbeddingWorker(oversized dimensions) succeeded")
	}
	cfg = embeddingWorkerTestConfig()
	cfg.ProviderID = ""
	if _, err := NewEmbeddingWorker(cfg, deps); err == nil {
		t.Fatal("NewEmbeddingWorker(empty provider id) succeeded")
	}
	cfg = embeddingWorkerTestConfig()
	cfg.Model = ""
	if _, err := NewEmbeddingWorker(cfg, deps); err == nil {
		t.Fatal("NewEmbeddingWorker(empty model) succeeded")
	}
	bad := deps
	bad.Provider = nil
	if _, err := NewEmbeddingWorker(embeddingWorkerTestConfig(), bad); err == nil {
		t.Fatal("NewEmbeddingWorker(missing provider) succeeded")
	}
	bad = deps
	bad.Index = nil
	if _, err := NewEmbeddingWorker(embeddingWorkerTestConfig(), bad); err == nil {
		t.Fatal("NewEmbeddingWorker(missing index) succeeded")
	}
}

func TestEmbeddingWorkerEmitsEmbeddingQueueDepthGauge(t *testing.T) {
	queue := &depthFakeQueue{workerFakeQueue: *newWorkerFakeQueue(nil), embeddingDepth: 2}
	metrics := &retrievalMetricCapture{}
	deps := embeddingWorkerTestDependencies(queue, newWorkerFakeSource(), newWorkerFakeVectorIndex(nil), &workerFakeLister{}, &workerFakeResolver{}, testutil.NewFakeEmbeddingProvider(3))
	deps.Metrics = metrics
	worker, err := NewEmbeddingWorker(embeddingWorkerTestConfig(), deps)
	if err != nil {
		t.Fatalf("NewEmbeddingWorker() error = %v", err)
	}
	worker.tick(t.Context())
	embedding, ok := metrics.findWithLabels(domain.MetricKnowledgeEmbeddingQueueDepth, nil)
	if !ok || embedding.Kind != port.MetricKindGauge || embedding.Value != 2 {
		t.Fatalf("embedding depth gauge = %#v found=%t", embedding, ok)
	}
}

func TestEmbeddingWorkerObservesEmbeddingRequestDurationOutcomes(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	successQueue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_metric_ok", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	successSource := newWorkerFakeSource()
	successSource.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_metric_ok"] = workerTestProjectClaim("kclaim_metric_ok", "subject", "value", domain.KnowledgeClaimAsserted, 1)
	successMetrics := &retrievalMetricCapture{}
	deps := embeddingWorkerTestDependencies(successQueue, successSource, newWorkerFakeVectorIndex(nil), &workerFakeLister{}, &workerFakeResolver{}, testutil.NewFakeEmbeddingProvider(3))
	deps.Metrics = successMetrics
	worker, err := NewEmbeddingWorker(embeddingWorkerTestConfig(), deps)
	if err != nil {
		t.Fatalf("NewEmbeddingWorker() error = %v", err)
	}
	worker.tick(t.Context())
	success, ok := successMetrics.findWithLabels(domain.MetricKnowledgeEmbeddingRequestDuration, port.MetricLabels{domain.MetricLabelOutcome: string(domain.KnowledgeRetrievalOutcomeSuccess)})
	if !ok || success.Kind != port.MetricKindObservation {
		t.Fatalf("success embedding duration sample = %#v found=%t", success, ok)
	}

	failureQueue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_metric_err", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	failureSource := newWorkerFakeSource()
	failureSource.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_metric_err"] = workerTestProjectClaim("kclaim_metric_err", "subject", "value", domain.KnowledgeClaimAsserted, 1)
	failureMetrics := &retrievalMetricCapture{}
	failureDeps := embeddingWorkerTestDependencies(failureQueue, failureSource, newWorkerFakeVectorIndex(nil), &workerFakeLister{}, &workerFakeResolver{}, testutil.NewFakeEmbeddingProvider(3).SetErr(errors.New("provider down")))
	failureDeps.Metrics = failureMetrics
	failingWorker, err := NewEmbeddingWorker(embeddingWorkerTestConfig(), failureDeps)
	if err != nil {
		t.Fatalf("NewEmbeddingWorker() error = %v", err)
	}
	failingWorker.tick(t.Context())
	unavailable, ok := failureMetrics.findWithLabels(domain.MetricKnowledgeEmbeddingRequestDuration, port.MetricLabels{domain.MetricLabelOutcome: string(domain.KnowledgeRetrievalOutcomeUnavailable)})
	if !ok || unavailable.Kind != port.MetricKindObservation {
		t.Fatalf("unavailable embedding duration sample = %#v found=%t", unavailable, ok)
	}
}
