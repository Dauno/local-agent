package knowledge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// workerFakeQueue is a scripted generation-CAS queue for worker tests.
type workerFakeQueue struct {
	mu       sync.Mutex
	items    map[string]domain.KnowledgeQueueItem
	claims   int
	complete []domain.KnowledgeQueueClaim
	fails    []struct {
		claim domain.KnowledgeQueueClaim
		code  domain.KnowledgeQueueFailureCode
	}
	retries []struct {
		claim domain.KnowledgeQueueClaim
		next  time.Time
	}
	completeErr error
	enqueued    [][2]string
}

func newWorkerFakeQueue(items []domain.KnowledgeQueueItem) *workerFakeQueue {
	queue := &workerFakeQueue{items: make(map[string]domain.KnowledgeQueueItem)}
	for _, item := range items {
		queue.items[string(item.Kind)+"\x00"+item.ID] = item
	}
	return queue
}

func (f *workerFakeQueue) key(kind domain.KnowledgeRetrievalItemKind, id string) string {
	return string(kind) + "\x00" + id
}

func (f *workerFakeQueue) ClaimNext(_ context.Context, kind domain.KnowledgeRetrievalItemKind, now time.Time, lease time.Duration) (domain.KnowledgeQueueItem, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best string
	var bestID string
	for key, item := range f.items {
		if item.Kind != kind {
			continue
		}
		ready := item.Status == domain.KnowledgeQueuePending && (item.NextAttempt.IsZero() || !item.NextAttempt.After(now))
		expired := item.Status == domain.KnowledgeQueueProcessing && !item.LeaseUntil.IsZero() && !item.LeaseUntil.After(now)
		if !ready && !expired {
			continue
		}
		if best == "" || item.ID < bestID {
			best, bestID = key, item.ID
		}
	}
	if best == "" {
		return domain.KnowledgeQueueItem{}, false, nil
	}
	item := f.items[best]
	item.Status = domain.KnowledgeQueueProcessing
	item.Attempts++
	item.LeaseUntil = now.Add(lease)
	item.UpdatedAt = now
	f.items[best] = item
	f.claims++
	return item, true, nil
}

func (f *workerFakeQueue) Complete(_ context.Context, claim domain.KnowledgeQueueClaim) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.complete = append(f.complete, claim)
	if f.completeErr != nil {
		return f.completeErr
	}
	item, ok := f.items[f.key(claim.Kind, claim.ID)]
	if !ok || item.Status != domain.KnowledgeQueueProcessing || item.Generation != claim.Generation || item.LeaseUntil != claim.LeaseUntil {
		return port.ErrKnowledgeCASConflict
	}
	item.Status = domain.KnowledgeQueueDone
	f.items[f.key(claim.Kind, claim.ID)] = item
	return nil
}

func (f *workerFakeQueue) Retry(_ context.Context, claim domain.KnowledgeQueueClaim, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retries = append(f.retries, struct {
		claim domain.KnowledgeQueueClaim
		next  time.Time
	}{claim, next})
	item, ok := f.items[f.key(claim.Kind, claim.ID)]
	if !ok || item.Status != domain.KnowledgeQueueProcessing || item.Generation != claim.Generation || item.LeaseUntil != claim.LeaseUntil {
		return port.ErrKnowledgeCASConflict
	}
	item.Status = domain.KnowledgeQueuePending
	item.NextAttempt = next
	item.LeaseUntil = time.Time{}
	f.items[f.key(claim.Kind, claim.ID)] = item
	return nil
}

func (f *workerFakeQueue) Fail(_ context.Context, claim domain.KnowledgeQueueClaim, code domain.KnowledgeQueueFailureCode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails = append(f.fails, struct {
		claim domain.KnowledgeQueueClaim
		code  domain.KnowledgeQueueFailureCode
	}{claim, code})
	item, ok := f.items[f.key(claim.Kind, claim.ID)]
	if !ok || item.Status != domain.KnowledgeQueueProcessing || item.Generation != claim.Generation || item.LeaseUntil != claim.LeaseUntil {
		return port.ErrKnowledgeCASConflict
	}
	item.Status = domain.KnowledgeQueueFailed
	item.LeaseUntil = time.Time{}
	f.items[f.key(claim.Kind, claim.ID)] = item
	return nil
}

func (f *workerFakeQueue) List(_ context.Context, kind domain.KnowledgeRetrievalItemKind, _ string, _ int) ([]domain.KnowledgeQueueItem, error) {
	var items []domain.KnowledgeQueueItem
	for _, item := range f.items {
		if item.Kind == kind {
			items = append(items, item)
		}
	}
	return items, nil
}

func (f *workerFakeQueue) Enqueue(_ context.Context, kind domain.KnowledgeRetrievalItemKind, itemID string) (domain.KnowledgeQueueItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, [2]string{string(kind), itemID})
	key := f.key(kind, itemID)
	item := f.items[key]
	if item.ID == "" {
		item = domain.KnowledgeQueueItem{Kind: kind, ID: itemID, Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	} else {
		item.Generation++
		item.Status = domain.KnowledgeQueuePending
		item.Attempts = 0
		item.NextAttempt = time.Time{}
		item.LeaseUntil = time.Time{}
		item.UpdatedAt = time.Now()
	}
	f.items[key] = item
	return item, nil
}

// workerFakeSource scripts authoritative reads and records not-found.
type workerFakeSource struct {
	items   map[string]port.KnowledgeAuthoritativeItem
	err     map[string]error
	generic error
}

func newWorkerFakeSource() *workerFakeSource {
	return &workerFakeSource{items: make(map[string]port.KnowledgeAuthoritativeItem), err: make(map[string]error)}
}

func (f *workerFakeSource) ReadIndexSource(_ context.Context, kind domain.KnowledgeRetrievalItemKind, id string) (port.KnowledgeAuthoritativeItem, error) {
	key := string(kind) + "\x00" + id
	if err := f.err[key]; err != nil {
		return port.KnowledgeAuthoritativeItem{}, err
	}
	if f.generic != nil {
		return port.KnowledgeAuthoritativeItem{}, f.generic
	}
	item, ok := f.items[key]
	if !ok {
		return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: missing", port.ErrKnowledgeNotFound)
	}
	return item, nil
}

// workerFakeIndex records replacements and deletions.
type workerFakeIndex struct {
	mu         sync.Mutex
	rows       map[string]port.KnowledgeLexicalIndexRow
	deleted    [][2]string
	cleared    bool
	replaceErr error
}

func newWorkerFakeIndex(rows []port.KnowledgeLexicalIndexRow) *workerFakeIndex {
	index := &workerFakeIndex{rows: make(map[string]port.KnowledgeLexicalIndexRow)}
	for _, row := range rows {
		index.rows[string(row.Kind)+"\x00"+row.ID] = row
	}
	return index
}

func (f *workerFakeIndex) ReplaceLexical(_ context.Context, kind domain.KnowledgeRetrievalItemKind, id string, revision int, sourceDigest, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replaceErr != nil {
		return f.replaceErr
	}
	f.rows[string(kind)+"\x00"+id] = port.KnowledgeLexicalIndexRow{Kind: kind, ID: id, Revision: revision, SourceDigest: sourceDigest}
	return nil
}

func (f *workerFakeIndex) DeleteLexical(_ context.Context, kind domain.KnowledgeRetrievalItemKind, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, [2]string{string(kind), id})
	delete(f.rows, string(kind)+"\x00"+id)
	return nil
}

func (f *workerFakeIndex) ListLexical(_ context.Context, kind domain.KnowledgeRetrievalItemKind, _ string, _ int) ([]port.KnowledgeLexicalIndexRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rows []port.KnowledgeLexicalIndexRow
	for _, row := range f.rows {
		if row.Kind == kind {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (f *workerFakeIndex) ClearLexical(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = true
	clear(f.rows)
	return nil
}

// workerFakeResolver resolves fixed document content.
type workerFakeResolver struct {
	content map[string]string
	err     map[string]error
}

func (f *workerFakeResolver) Resolve(_ context.Context, document domain.KnowledgeDocument, _ domain.KnowledgeRetrievalLimits) ([]byte, error) {
	if err := f.err[string(document.ID)]; err != nil {
		return nil, err
	}
	return []byte(f.content[string(document.ID)]), nil
}

// workerFakeLister pages truth identities.
type workerFakeLister struct {
	identities map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity
}

func (f *workerFakeLister) ListTruthIdentities(_ context.Context, kind domain.KnowledgeRetrievalItemKind, _ string, _ int) ([]port.KnowledgeTruthIdentity, error) {
	return append([]port.KnowledgeTruthIdentity(nil), f.identities[kind]...), nil
}

func workerTestConfig() LexicalWorkerConfig {
	return LexicalWorkerConfig{
		Interval:   time.Hour,
		MaxRetries: 3,
		BatchSize:  8,
		Limits:     domain.DefaultKnowledgeRetrievalLimits(),
	}
}

func workerTestDependencies(queue *workerFakeQueue, source *workerFakeSource, index *workerFakeIndex, lister *workerFakeLister, resolver *workerFakeResolver) LexicalWorkerDependencies {
	return LexicalWorkerDependencies{
		Queue: queue, Source: source, Index: index, Resolver: resolver,
		Lister: lister, Clock: &workerTestClock{now: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)},
		Logger: workerTestLogger{},
		Redact: func(value string) string {
			if value == "SECRET" {
				return ""
			}
			return value
		},
	}
}

func workerTestClaim(item domain.KnowledgeQueueItem) domain.KnowledgeQueueClaim {
	return domain.KnowledgeQueueClaim{Kind: item.Kind, ID: item.ID, Generation: item.Generation, LeaseUntil: item.LeaseUntil}
}

func workerTestProjectClaim(id, subject, valueText string, status domain.KnowledgeClaimStatus, revision int) port.KnowledgeAuthoritativeItem {
	return port.KnowledgeAuthoritativeItem{
		Kind: domain.KnowledgeRetrievalClaim, ID: id,
		Claim: &domain.KnowledgeClaim{
			ID: domain.KnowledgeClaimID(id), Subject: subject, Predicate: domain.KnowledgePredicateIs,
			Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: valueText},
			ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project",
			SourceClass: domain.KnowledgeSourceHuman, SourceRef: "src:" + id,
			Status: status, Revision: revision,
		},
	}
}

func TestLexicalWorkerIndexesAndCompletes(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_w1", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	source := newWorkerFakeSource()
	source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_w1"] = workerTestProjectClaim("kclaim_w1", "subject one", "value one", domain.KnowledgeClaimAsserted, 2)
	index := newWorkerFakeIndex(nil)
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	worker, err := NewLexicalWorker(workerTestConfig(), workerTestDependencies(queue, source, index, lister, resolver))
	if err != nil {
		t.Fatalf("NewLexicalWorker() error = %v", err)
	}
	worker.tick(t.Context())

	rows, _ := index.ListLexical(t.Context(), domain.KnowledgeRetrievalClaim, "", 10)
	if len(rows) != 1 || rows[0].Revision != 2 || rows[0].SourceDigest == "" {
		t.Fatalf("index rows after tick = %+v, want the redacted canonical row for revision 2", rows)
	}
	if len(queue.complete) != 1 {
		t.Fatalf("completions = %d, want 1", len(queue.complete))
	}
	if len(index.deleted) != 0 {
		t.Fatalf("deletions = %v, want none for a live indexable claim", index.deleted)
	}
}

func TestLexicalWorkerMissingAndIneligibleDeleteAndComplete(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_gone", Generation: 3, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_archived", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	source := newWorkerFakeSource()
	source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_archived"] = workerTestProjectClaim("kclaim_archived", "archived subject", "value", domain.KnowledgeClaimArchived, 1)
	index := newWorkerFakeIndex([]port.KnowledgeLexicalIndexRow{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_gone", Revision: 2, SourceDigest: "aa"},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_archived", Revision: 1, SourceDigest: "bb"},
	})
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	worker, err := NewLexicalWorker(workerTestConfig(), workerTestDependencies(queue, source, index, lister, resolver))
	if err != nil {
		t.Fatalf("NewLexicalWorker() error = %v", err)
	}
	worker.tick(t.Context())

	if len(queue.complete) != 2 || len(queue.fails) != 0 || len(queue.retries) != 0 {
		t.Fatalf("transitions = complete %d fails %d retries %d, want 2 completes", len(queue.complete), len(queue.fails), len(queue.retries))
	}
	if len(index.deleted) != 2 {
		t.Fatalf("deletions = %v, want both stale rows removed", index.deleted)
	}
}

func TestLexicalWorkerRedactedEmptyDeletesAndCompletes(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_secret", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	source := newWorkerFakeSource()
	source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_secret"] = workerTestProjectClaim("kclaim_secret", "SECRET", "SECRET", domain.KnowledgeClaimAsserted, 1)
	index := newWorkerFakeIndex([]port.KnowledgeLexicalIndexRow{{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_secret", Revision: 1, SourceDigest: "aa"}})
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	worker, err := NewLexicalWorker(workerTestConfig(), workerTestDependencies(queue, source, index, lister, resolver))
	if err != nil {
		t.Fatalf("NewLexicalWorker() error = %v", err)
	}
	worker.tick(t.Context())
	if len(queue.complete) != 1 || len(index.deleted) != 1 {
		t.Fatalf("redacted-empty claim: completes %d deletions %d, want 1/1", len(queue.complete), len(index.deleted))
	}
}

func TestLexicalWorkerUnverifiableDocumentFailsSourceInvalid(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_bad", Generation: 1, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	source := newWorkerFakeSource()
	source.items[string(domain.KnowledgeRetrievalDocument)+"\x00kdoc_bad"] = port.KnowledgeAuthoritativeItem{
		Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_bad",
		Document: &domain.KnowledgeDocument{
			ID: "kdoc_bad", Subject: "bad doc", ScopeKind: domain.KnowledgeScopeGlobal,
			ContentDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ContentHandle: "result:doc-1",
			SourceID:      "t", SourceRev: 1,
			Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive, Revision: 1,
		},
	}
	index := newWorkerFakeIndex([]port.KnowledgeLexicalIndexRow{{Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_bad", Revision: 1, SourceDigest: "aa"}})
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{"kdoc_bad": port.ErrKnowledgeUnavailable}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	worker, err := NewLexicalWorker(workerTestConfig(), workerTestDependencies(queue, source, index, lister, resolver))
	if err != nil {
		t.Fatalf("NewLexicalWorker() error = %v", err)
	}
	worker.tick(t.Context())
	if len(queue.fails) != 1 || queue.fails[0].code != domain.KnowledgeQueueFailureSourceInvalid {
		t.Fatalf("fails = %+v, want one source_invalid", queue.fails)
	}
	if len(index.deleted) != 1 || len(queue.complete) != 0 {
		t.Fatalf("deletions %d completes %d, want 1 deletion and no completion", len(index.deleted), len(queue.complete))
	}
}

func TestLexicalWorkerRetriesAndExhausts(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	// Attempts 1: below budget -> retry. Attempts 3: at budget -> exhaust.
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_retry", Generation: 1, Status: domain.KnowledgeQueuePending, Attempts: 1, CreatedAt: now, UpdatedAt: now},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_exhaust", Generation: 1, Status: domain.KnowledgeQueuePending, Attempts: 3, CreatedAt: now, UpdatedAt: now},
	})
	source := newWorkerFakeSource()
	source.generic = errors.New("transient sqlite failure")
	index := newWorkerFakeIndex(nil)
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	worker, err := NewLexicalWorker(workerTestConfig(), workerTestDependencies(queue, source, index, lister, resolver))
	if err != nil {
		t.Fatalf("NewLexicalWorker() error = %v", err)
	}
	worker.tick(t.Context())
	if len(queue.retries) != 1 || queue.retries[0].claim.ID != "kclaim_retry" {
		t.Fatalf("retries = %+v, want one retry for kclaim_retry", queue.retries)
	}
	if !queue.retries[0].next.After(now) {
		t.Fatalf("retry next attempt %v is not in the future", queue.retries[0].next)
	}
	if len(queue.fails) != 1 || queue.fails[0].code != domain.KnowledgeQueueFailureAttemptsExhausted || queue.fails[0].claim.ID != "kclaim_exhaust" {
		t.Fatalf("fails = %+v, want attempts_exhausted for kclaim_exhaust", queue.fails)
	}
}

func TestLexicalWorkerConcurrentMutationSurvivesStaleCompletion(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_race", Generation: 5, Status: domain.KnowledgeQueuePending, CreatedAt: now, UpdatedAt: now},
	})
	source := newWorkerFakeSource()
	source.items[string(domain.KnowledgeRetrievalClaim)+"\x00kclaim_race"] = workerTestProjectClaim("kclaim_race", "race subject", "value", domain.KnowledgeClaimAsserted, 5)
	index := newWorkerFakeIndex(nil)
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	// The truth mutation bumps the queue generation while the stale worker
	// is building: complete must conflict and the fresh work must survive.
	queue.completeErr = port.ErrKnowledgeCASConflict
	worker, err := NewLexicalWorker(workerTestConfig(), workerTestDependencies(queue, source, index, lister, resolver))
	if err != nil {
		t.Fatalf("NewLexicalWorker() error = %v", err)
	}
	worker.tick(t.Context())
	if len(queue.complete) != 1 {
		t.Fatalf("completions = %d, want the conflicting attempt recorded", len(queue.complete))
	}
	if len(queue.fails) != 0 || len(queue.retries) != 0 {
		t.Fatalf("conflicted completion must not fail or retry the row: fails %d retries %d", len(queue.fails), len(queue.retries))
	}
}

func TestLexicalWorkerReconcileEnqueuesMissingStaleAndOrphans(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	// Truth: claim A revision 3, claim B revision 1, claim C revision 1.
	// Queue: A generation 2 (stale), B done generation 1 (apparently
	// current), C missing entirely.
	// Index: A revision 3 (ok), B revision 1 (apparently current), D
	// orphan.
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_a", Generation: 2, Status: domain.KnowledgeQueueDone, CreatedAt: now, UpdatedAt: now},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_b", Generation: 1, Status: domain.KnowledgeQueueDone, CreatedAt: now, UpdatedAt: now},
	})
	index := newWorkerFakeIndex([]port.KnowledgeLexicalIndexRow{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_a", Revision: 3, SourceDigest: "aa"},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_b", Revision: 1, SourceDigest: "stale-digest-before-redactor-rotation"},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_d", Revision: 1, SourceDigest: "dd"},
	})
	source := newWorkerFakeSource()
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{
		domain.KnowledgeRetrievalClaim: {
			{ID: "kclaim_a", Revision: 3},
			{ID: "kclaim_b", Revision: 1},
			{ID: "kclaim_c", Revision: 1},
		},
	}}
	worker, err := NewLexicalWorker(workerTestConfig(), workerTestDependencies(queue, source, index, lister, resolver))
	if err != nil {
		t.Fatalf("NewLexicalWorker() error = %v", err)
	}
	if err := worker.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	enqueued := map[string]int{}
	for _, pair := range queue.enqueued {
		enqueued[pair[1]]++
	}
	// FIND-090: every current truth identity is re-enqueued even when its
	// queue generation matches its revision and its FTS row revision
	// matches, because a redactor rotation changes the redacted digest
	// without advancing the truth revision; the idempotent worker rebuild
	// repairs the stale text. The orphan identity is enqueued for cleanup.
	if enqueued["kclaim_a"] != 1 || enqueued["kclaim_b"] != 1 || enqueued["kclaim_c"] != 1 || enqueued["kclaim_d"] != 1 {
		t.Fatalf("reconcile enqueues = %v, want A, B, C (all truth) and D (orphan)", queue.enqueued)
	}
}

func TestLexicalWorkerRebuildClearsAndReenqueuesTruth(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	queue := newWorkerFakeQueue([]domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_d", Generation: 1, Status: domain.KnowledgeQueueDone, CreatedAt: now, UpdatedAt: now},
	})
	index := newWorkerFakeIndex([]port.KnowledgeLexicalIndexRow{{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_d", Revision: 1, SourceDigest: "aa"}})
	source := newWorkerFakeSource()
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{
		domain.KnowledgeRetrievalClaim: {{ID: "kclaim_d", Revision: 1}},
	}}
	worker, err := NewLexicalWorker(workerTestConfig(), workerTestDependencies(queue, source, index, lister, resolver))
	if err != nil {
		t.Fatalf("NewLexicalWorker() error = %v", err)
	}
	if err := worker.Rebuild(t.Context()); err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if !index.cleared {
		t.Fatal("Rebuild() did not clear the lexical index")
	}
	found := false
	for _, pair := range queue.enqueued {
		if pair[1] == "kclaim_d" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Rebuild() enqueues = %v, want kclaim_d", queue.enqueued)
	}
}

func TestLexicalWorkerRunCancellationIsAwaitable(t *testing.T) {
	queue := newWorkerFakeQueue(nil)
	source := newWorkerFakeSource()
	index := newWorkerFakeIndex(nil)
	resolver := &workerFakeResolver{content: map[string]string{}, err: map[string]error{}}
	lister := &workerFakeLister{identities: map[domain.KnowledgeRetrievalItemKind][]port.KnowledgeTruthIdentity{}}
	worker, err := NewLexicalWorker(workerTestConfig(), workerTestDependencies(queue, source, index, lister, resolver))
	if err != nil {
		t.Fatalf("NewLexicalWorker() error = %v", err)
	}
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

func TestLexicalWorkerRejectsInvalidConfiguration(t *testing.T) {
	deps := workerTestDependencies(newWorkerFakeQueue(nil), newWorkerFakeSource(), newWorkerFakeIndex(nil), &workerFakeLister{}, &workerFakeResolver{})
	cfg := workerTestConfig()
	cfg.Interval = 0
	if _, err := NewLexicalWorker(cfg, deps); err == nil {
		t.Fatal("NewLexicalWorker(zero interval) succeeded")
	}
	cfg = workerTestConfig()
	cfg.BatchSize = 0
	if _, err := NewLexicalWorker(cfg, deps); err == nil {
		t.Fatal("NewLexicalWorker(zero batch) succeeded")
	}
	cfg = workerTestConfig()
	cfg.BatchSize = domain.HardMaxKnowledgeRetrievalWorkerBatchSize + 1
	if _, err := NewLexicalWorker(cfg, deps); err == nil {
		t.Fatal("NewLexicalWorker(oversized batch) succeeded")
	}
	cfg = workerTestConfig()
	bad := deps
	bad.Queue = nil
	if _, err := NewLexicalWorker(cfg, bad); err == nil {
		t.Fatal("NewLexicalWorker(missing queue) succeeded")
	}
}

type depthFakeQueue struct {
	workerFakeQueue
	lexicalDepth   int
	embeddingDepth int
	lexicalErr     error
	embeddingErr   error
}

func (q *depthFakeQueue) LexicalDepth(context.Context) (int, error) {
	return q.lexicalDepth, q.lexicalErr
}

func (q *depthFakeQueue) EmbeddingDepth(context.Context) (int, error) {
	return q.embeddingDepth, q.embeddingErr
}

// TestLexicalWorkerEmitsQueueDepthGauges pins the closed queue depth
// gauges: pending plus processing only, never terminal done or failed rows,
// sampled through the consumer-owned depth surface.
func TestLexicalWorkerEmitsQueueDepthGauges(t *testing.T) {
	queue := &depthFakeQueue{workerFakeQueue: *newWorkerFakeQueue(nil), lexicalDepth: 3, embeddingDepth: 1}
	metrics := &retrievalMetricCapture{}
	worker, err := NewLexicalWorker(LexicalWorkerConfig{
		Interval: time.Hour, MaxRetries: 2, BatchSize: 2, Limits: domain.DefaultKnowledgeRetrievalLimits(),
	}, LexicalWorkerDependencies{
		Queue: queue, Source: &workerFakeSource{}, Index: &workerFakeIndex{},
		Resolver: &workerFakeResolver{}, Lister: &workerFakeLister{},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("NewLexicalWorker() error = %v", err)
	}
	worker.tick(t.Context())
	lexical, ok := metrics.findWithLabels(domain.MetricKnowledgeLexicalQueueDepth, nil)
	if !ok || lexical.Kind != port.MetricKindGauge || lexical.Value != 3 {
		t.Fatalf("lexical depth gauge = %#v found=%t", lexical, ok)
	}
	embedding, ok := metrics.findWithLabels(domain.MetricKnowledgeEmbeddingQueueDepth, nil)
	if !ok || embedding.Kind != port.MetricKindGauge || embedding.Value != 1 {
		t.Fatalf("embedding depth gauge = %#v found=%t", embedding, ok)
	}
}
