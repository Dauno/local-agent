package port

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// The fakes below are behavioral: they implement the frozen retrieval
// contracts with deterministic canned semantics so the interfaces are
// pinned by behavior, not compile-only assertions.

type fakeKnowledgeRetriever struct {
	result domain.KnowledgeRetrievalResult
	err    error
}

func (f fakeKnowledgeRetriever) Retrieve(_ context.Context, request domain.KnowledgeRetrievalRequest) (domain.KnowledgeRetrievalResult, error) {
	if err := request.Validate(); err != nil {
		return domain.KnowledgeRetrievalResult{}, err
	}
	return f.result, f.err
}

type fakeKnowledgeRetrievalBindingResolver struct {
	binding KnowledgeRetrievalBinding
}

func (f fakeKnowledgeRetrievalBindingResolver) ResolveRetrievalBinding(
	_ context.Context,
	team, actor string,
	conversation domain.ConversationKey,
	exchangeTS string,
) (KnowledgeRetrievalBinding, error) {
	if team == "" || actor == "" || conversation == "" || exchangeTS == "" {
		return KnowledgeRetrievalBinding{}, ErrKnowledgeValidation
	}
	return f.binding, nil
}

type fakeCandidate struct {
	Kind      domain.KnowledgeRetrievalItemKind
	ID        string
	Subject   string
	ScopeKind domain.KnowledgeScopeKind
	ScopeID   string
	Revision  int
	Token     string
}

type fakeKnowledgeCandidateReader struct {
	candidates []fakeCandidate
	items      map[string]KnowledgeAuthoritativeItem
}

func (f fakeKnowledgeCandidateReader) ReadExact(
	_ context.Context,
	_ domain.KnowledgeWriteBinding,
	_ time.Time,
	limits domain.KnowledgeRetrievalLimits,
	query string,
	tokens []string,
) ([]KnowledgeEligibleCandidate, error) {
	var matched []KnowledgeEligibleCandidate
	for _, candidate := range f.candidates {
		hit := candidate.Subject == query
		for _, token := range tokens {
			if candidate.Token == token {
				hit = true
			}
		}
		if hit {
			matched = append(matched, eligible(candidate))
		}
	}
	if len(matched) > limits.MaxCandidatesPerChannel {
		matched = matched[:limits.MaxCandidatesPerChannel]
	}
	return matched, nil
}

func (f fakeKnowledgeCandidateReader) ReadRelated(
	_ context.Context,
	_ domain.KnowledgeWriteBinding,
	_ time.Time,
	_ domain.KnowledgeRetrievalLimits,
	seeds []KnowledgeEligibleCandidate,
) ([]KnowledgeEligibleCandidate, error) {
	var related []KnowledgeEligibleCandidate
	for _, seed := range seeds {
		for _, candidate := range f.candidates {
			if candidate.Token == "rel:"+seed.ID {
				related = append(related, eligible(candidate))
			}
		}
	}
	return related, nil
}

func (f fakeKnowledgeCandidateReader) ReadItem(
	_ context.Context,
	_ domain.KnowledgeWriteBinding,
	_ time.Time,
	_ domain.KnowledgeRetrievalLimits,
	kind domain.KnowledgeRetrievalItemKind,
	id string,
) (KnowledgeAuthoritativeItem, error) {
	item, ok := f.items[string(kind)+":"+id]
	if !ok {
		return KnowledgeAuthoritativeItem{}, ErrKnowledgeNotFound
	}
	return item, nil
}

func eligible(candidate fakeCandidate) KnowledgeEligibleCandidate {
	return KnowledgeEligibleCandidate{
		Kind: candidate.Kind, ID: candidate.ID, Subject: candidate.Subject,
		ScopeKind: candidate.ScopeKind, ScopeID: candidate.ScopeID, Revision: candidate.Revision,
	}
}

type fakeKnowledgeDocumentResolver struct {
	content map[string][]byte
}

func (f fakeKnowledgeDocumentResolver) Resolve(_ context.Context, document domain.KnowledgeDocument, _ domain.KnowledgeRetrievalLimits) ([]byte, error) {
	content, ok := f.content[string(document.ID)]
	if !ok {
		return nil, ErrKnowledgeNotFound
	}
	return content, nil
}

type fakeKnowledgeIndex struct {
	lexical  []KnowledgeIndexHit
	semantic []KnowledgeIndexHit
}

func (f fakeKnowledgeIndex) SearchLexical(_ context.Context, _ []domain.KnowledgeScopeRef, _, _ string, limit int) ([]KnowledgeIndexHit, error) {
	if limit > 0 && len(f.lexical) > limit {
		return f.lexical[:limit], nil
	}
	return f.lexical, nil
}

func (f fakeKnowledgeIndex) SearchSemantic(_ context.Context, _ []domain.KnowledgeScopeRef, _ string, _ []float32, _, limit int) ([]KnowledgeIndexHit, error) {
	if limit > 0 && len(f.semantic) > limit {
		return f.semantic[:limit], nil
	}
	return f.semantic, nil
}

type fakeEmbeddingProvider struct{ vectors [][]float32 }

func (f fakeEmbeddingProvider) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return f.vectors, nil
}

// fakeKnowledgeQueueStore enforces generation and lease CAS in memory: a
// transition only applies when the presented claim matches the row's
// current generation and lease.
type fakeKnowledgeQueueStore struct {
	rows map[string]domain.KnowledgeQueueItem
	now  time.Time
}

func newFakeKnowledgeQueueStore(t *testing.T, items []domain.KnowledgeQueueItem) *fakeKnowledgeQueueStore {
	t.Helper()
	store := &fakeKnowledgeQueueStore{rows: make(map[string]domain.KnowledgeQueueItem), now: time.Now().UTC()}
	for _, item := range items {
		store.rows[string(item.Kind)+":"+item.ID] = item
	}
	return store
}

func (f *fakeKnowledgeQueueStore) key(kind domain.KnowledgeRetrievalItemKind, id string) string {
	return string(kind) + ":" + id
}

func (f *fakeKnowledgeQueueStore) ClaimNext(_ context.Context, kind domain.KnowledgeRetrievalItemKind, now time.Time, lease time.Duration) (domain.KnowledgeQueueItem, bool, error) {
	if !domain.ValidKnowledgeQueueLease(lease) {
		return domain.KnowledgeQueueItem{}, false, ErrKnowledgeValidation
	}
	for key, item := range f.rows {
		if item.Kind != kind || item.Status != domain.KnowledgeQueuePending || item.NextAttempt.After(now) {
			continue
		}
		item.Status = domain.KnowledgeQueueProcessing
		item.LeaseUntil = now.Add(lease)
		item.Attempts++
		item.UpdatedAt = now
		f.rows[key] = item
		return item, true, nil
	}
	return domain.KnowledgeQueueItem{}, false, nil
}

func (f *fakeKnowledgeQueueStore) Complete(_ context.Context, claim domain.KnowledgeQueueClaim) error {
	item, ok := f.rows[f.key(claim.Kind, claim.ID)]
	if !ok || item.Generation != claim.Generation || !item.LeaseUntil.Equal(claim.LeaseUntil) {
		return ErrKnowledgeCASConflict
	}
	item.Status = domain.KnowledgeQueueDone
	item.LeaseUntil = time.Time{}
	item.UpdatedAt = f.now
	f.rows[f.key(claim.Kind, claim.ID)] = item
	return nil
}

func (f *fakeKnowledgeQueueStore) Retry(_ context.Context, claim domain.KnowledgeQueueClaim, nextAttempt time.Time) error {
	item, ok := f.rows[f.key(claim.Kind, claim.ID)]
	if !ok || item.Generation != claim.Generation || !item.LeaseUntil.Equal(claim.LeaseUntil) {
		return ErrKnowledgeCASConflict
	}
	item.Status = domain.KnowledgeQueuePending
	item.NextAttempt = nextAttempt
	item.LeaseUntil = time.Time{}
	item.UpdatedAt = f.now
	f.rows[f.key(claim.Kind, claim.ID)] = item
	return nil
}

func (f *fakeKnowledgeQueueStore) Fail(_ context.Context, claim domain.KnowledgeQueueClaim, code domain.KnowledgeQueueFailureCode) error {
	if !domain.ValidKnowledgeQueueFailureCode(code) {
		return ErrKnowledgeValidation
	}
	item, ok := f.rows[f.key(claim.Kind, claim.ID)]
	if !ok || item.Generation != claim.Generation || !item.LeaseUntil.Equal(claim.LeaseUntil) {
		return ErrKnowledgeCASConflict
	}
	item.Status = domain.KnowledgeQueueFailed
	item.LeaseUntil = time.Time{}
	item.UpdatedAt = f.now
	f.rows[f.key(claim.Kind, claim.ID)] = item
	return nil
}

func (f *fakeKnowledgeQueueStore) List(_ context.Context, kind domain.KnowledgeRetrievalItemKind, afterID string, limit int) ([]domain.KnowledgeQueueItem, error) {
	if !domain.ValidKnowledgeQueueListLimit(limit) {
		return nil, ErrKnowledgeValidation
	}
	var items []domain.KnowledgeQueueItem
	for _, item := range f.rows {
		if item.Kind == kind && item.ID > afterID {
			items = append(items, item)
		}
	}
	slices.SortFunc(items, func(a, b domain.KnowledgeQueueItem) int { return cmp.Compare(a.ID, b.ID) })
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (f *fakeKnowledgeQueueStore) Enqueue(_ context.Context, kind domain.KnowledgeRetrievalItemKind, itemID string) (domain.KnowledgeQueueItem, error) {
	key := f.key(kind, itemID)
	item := f.rows[key]
	if item.ID == "" {
		item = domain.KnowledgeQueueItem{Kind: kind, ID: itemID, CreatedAt: f.now, UpdatedAt: f.now}
	} else {
		item.Generation++
		item.Attempts = 0
	}
	item.Status = domain.KnowledgeQueuePending
	item.UpdatedAt = f.now
	f.rows[key] = item
	return item, nil
}

func TestKnowledgeRetrievalBindingResolverPassesTurnSelectorsExplicitly(t *testing.T) {
	resolver := fakeKnowledgeRetrievalBindingResolver{binding: KnowledgeRetrievalBinding{
		Binding: domain.KnowledgeWriteBinding{
			Team: "T00000001", Actor: "U00000001",
			Conversation: domain.ConversationKey("slack:T00000001:dm:D00000001"),
		},
		ExchangeTS: "1723543200.123456",
	}}
	got, err := resolver.ResolveRetrievalBinding(context.Background(), "T00000001", "U00000001", "slack:T00000001:dm:D00000001", "1723543200.123456")
	if err != nil || got.ExchangeTS != "1723543200.123456" || got.Binding.Team != "T00000001" {
		t.Fatalf("resolution = %+v, %v", got, err)
	}
	if _, err := resolver.ResolveRetrievalBinding(context.Background(), "", "U00000001", "slack:T00000001:dm:D00000001", "1723543200.123456"); !errors.Is(err, ErrKnowledgeValidation) {
		t.Fatalf("missing selector error = %v", err)
	}
}

func TestKnowledgeCandidateReaderFakesBoundExactAndRelationReads(t *testing.T) {
	reader := fakeKnowledgeCandidateReader{
		candidates: []fakeCandidate{
			{Kind: domain.KnowledgeRetrievalClaim, ID: "c1", Subject: "api", Token: "postgresql", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "p", Revision: 2},
			{Kind: domain.KnowledgeRetrievalClaim, ID: "c2", Subject: "other", Token: "mysql", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "p", Revision: 1},
			{Kind: domain.KnowledgeRetrievalClaim, ID: "c3", Subject: "related", Token: "rel:c1", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "p", Revision: 4},
		},
		items: map[string]KnowledgeAuthoritativeItem{
			"claim:c1": {Kind: domain.KnowledgeRetrievalClaim, ID: "c1", Claim: &domain.KnowledgeClaim{ID: "c1"}},
		},
	}
	exact, err := reader.ReadExact(context.Background(), domain.KnowledgeWriteBinding{}, time.Time{}, domain.DefaultKnowledgeRetrievalLimits(), "api", []string{"postgresql"})
	if err != nil || len(exact) != 1 || exact[0].ID != "c1" || exact[0].Revision != 2 {
		t.Fatalf("exact read = %+v, %v", exact, err)
	}
	related, err := reader.ReadRelated(context.Background(), domain.KnowledgeWriteBinding{}, time.Time{}, domain.DefaultKnowledgeRetrievalLimits(), exact)
	if err != nil || len(related) != 1 || related[0].ID != "c3" {
		t.Fatalf("relation read = %+v, %v", related, err)
	}
	item, err := reader.ReadItem(context.Background(), domain.KnowledgeWriteBinding{}, time.Time{}, domain.DefaultKnowledgeRetrievalLimits(), domain.KnowledgeRetrievalClaim, "c1")
	if err != nil || item.Claim == nil || item.Claim.ID != "c1" {
		t.Fatalf("authoritative read = %+v, %v", item, err)
	}
	if _, err := reader.ReadItem(
		context.Background(),
		domain.KnowledgeWriteBinding{},
		time.Time{},
		domain.DefaultKnowledgeRetrievalLimits(),
		domain.KnowledgeRetrievalClaim,
		"missing",
	); !errors.Is(
		err,
		ErrKnowledgeNotFound,
	) {
		t.Fatalf("missing item error = %v", err)
	}
}

func TestKnowledgeIndexHitsCarryRevisionAndSourceDigest(t *testing.T) {
	index := fakeKnowledgeIndex{lexical: []KnowledgeIndexHit{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "c1", Rank: 1, Revision: 2, SourceDigest: strings.Repeat("a", 64)},
	}}
	hits, err := index.SearchLexical(context.Background(), nil, "", "api", 8)
	if err != nil || len(hits) != 1 || hits[0].Revision != 2 || hits[0].SourceDigest != strings.Repeat("a", 64) {
		t.Fatalf("lexical hits = %+v, %v", hits, err)
	}
}

func TestKnowledgeQueueStoreRejectsExpiredClaimantAfterReclaim(t *testing.T) {
	now := time.Now().UTC()
	store := newFakeKnowledgeQueueStore(t, []domain.KnowledgeQueueItem{{
		Kind: domain.KnowledgeRetrievalClaim, ID: "c1", Status: domain.KnowledgeQueuePending,
		Generation: 1, NextAttempt: now.Add(-time.Minute), CreatedAt: now, UpdatedAt: now,
	}})
	first, claimed, err := store.ClaimNext(context.Background(), domain.KnowledgeRetrievalClaim, now, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("first claim = %+v, %v, %v", first, claimed, err)
	}
	firstClaim := domain.KnowledgeQueueClaim{Kind: first.Kind, ID: first.ID, Generation: first.Generation, LeaseUntil: first.LeaseUntil}

	// The lease expires and the row is reclaimed by a new worker.
	now = now.Add(2 * time.Minute)
	store.rows[store.key(first.Kind, first.ID)] = domain.KnowledgeQueueItem{
		Kind: first.Kind, ID: first.ID, Status: domain.KnowledgeQueuePending,
		Generation: first.Generation, Attempts: 1, NextAttempt: now, CreatedAt: first.CreatedAt, UpdatedAt: now,
	}
	second, claimed, err := store.ClaimNext(context.Background(), domain.KnowledgeRetrievalClaim, now, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("second claim = %+v, %v, %v", second, claimed, err)
	}
	secondClaim := domain.KnowledgeQueueClaim{Kind: second.Kind, ID: second.ID, Generation: second.Generation, LeaseUntil: second.LeaseUntil}

	// The expired first claimant can never mutate the reclaimed claim.
	if err := store.Complete(context.Background(), firstClaim); !errors.Is(err, ErrKnowledgeCASConflict) {
		t.Fatalf("expired claimant completion error = %v, want ErrKnowledgeCASConflict", err)
	}
	if err := store.Retry(context.Background(), firstClaim, now.Add(time.Minute)); !errors.Is(err, ErrKnowledgeCASConflict) {
		t.Fatalf("expired claimant retry error = %v, want ErrKnowledgeCASConflict", err)
	}
	if err := store.Fail(context.Background(), firstClaim, domain.KnowledgeQueueFailureAttemptsExhausted); !errors.Is(err, ErrKnowledgeCASConflict) {
		t.Fatalf("expired claimant failure error = %v, want ErrKnowledgeCASConflict", err)
	}

	// The current owner completes normally and the row leaves the queue.
	if err := store.Complete(context.Background(), secondClaim); err != nil {
		t.Fatalf("current owner completion error = %v", err)
	}
	if row := store.rows[store.key(first.Kind, first.ID)]; row.Status != domain.KnowledgeQueueDone {
		t.Fatalf("row status after completion = %s", row.Status)
	}
}

func TestKnowledgeQueueStoreBoundsLeaseAndPagination(t *testing.T) {
	now := time.Now().UTC()
	store := newFakeKnowledgeQueueStore(t, []domain.KnowledgeQueueItem{{
		Kind: domain.KnowledgeRetrievalClaim, ID: "c1", Status: domain.KnowledgeQueuePending,
		Generation: 1, NextAttempt: now, CreatedAt: now, UpdatedAt: now,
	}})
	if _, _, err := store.ClaimNext(context.Background(), domain.KnowledgeRetrievalClaim, now, 0); !errors.Is(err, ErrKnowledgeValidation) {
		t.Fatalf("zero lease error = %v, want ErrKnowledgeValidation", err)
	}
	if _, _, err := store.ClaimNext(context.Background(), domain.KnowledgeRetrievalClaim, now, -time.Minute); !errors.Is(err, ErrKnowledgeValidation) {
		t.Fatalf("negative lease error = %v, want ErrKnowledgeValidation", err)
	}
	if _, _, err := store.ClaimNext(
		context.Background(),
		domain.KnowledgeRetrievalClaim,
		now,
		time.Duration(domain.HardMaxKnowledgeQueueLeaseSeconds+1)*time.Second,
	); !errors.Is(
		err,
		ErrKnowledgeValidation,
	) {
		t.Fatalf("over-long lease error = %v, want ErrKnowledgeValidation", err)
	}
	if _, err := store.List(context.Background(), domain.KnowledgeRetrievalClaim, "", 0); !errors.Is(err, ErrKnowledgeValidation) {
		t.Fatalf("zero page limit error = %v, want ErrKnowledgeValidation", err)
	}
	if _, err := store.List(context.Background(), domain.KnowledgeRetrievalClaim, "", domain.HardMaxKnowledgeQueueListLimit+1); !errors.Is(err, ErrKnowledgeValidation) {
		t.Fatalf("over-limit page error = %v, want ErrKnowledgeValidation", err)
	}
	// A valid bounded claim still works after the rejected ones.
	claimed, ok, err := store.ClaimNext(context.Background(), domain.KnowledgeRetrievalClaim, now, time.Minute)
	if err != nil || !ok || claimed.ID != "c1" {
		t.Fatalf("bounded claim = %+v, %v, %v", claimed, ok, err)
	}
}

func TestKnowledgeQueueStorePagesByIdentityOrder(t *testing.T) {
	now := time.Now().UTC()
	store := newFakeKnowledgeQueueStore(t, []domain.KnowledgeQueueItem{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "c1", Status: domain.KnowledgeQueuePending, Generation: 1, CreatedAt: now, UpdatedAt: now},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "c2", Status: domain.KnowledgeQueuePending, Generation: 1, CreatedAt: now, UpdatedAt: now},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "c3", Status: domain.KnowledgeQueuePending, Generation: 1, CreatedAt: now, UpdatedAt: now},
	})
	page, err := store.List(context.Background(), domain.KnowledgeRetrievalClaim, "", 2)
	if err != nil || len(page) != 2 || page[0].ID != "c1" || page[1].ID != "c2" {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	page, err = store.List(context.Background(), domain.KnowledgeRetrievalClaim, "c2", 2)
	if err != nil || len(page) != 1 || page[0].ID != "c3" {
		t.Fatalf("second page = %+v, %v", page, err)
	}
}

func TestKnowledgeAuthoritativeItemUnionValidates(t *testing.T) {
	claim := domain.KnowledgeClaim{
		ID: "claim_0001", Subject: "api", Predicate: domain.KnowledgePredicateRunsOn,
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "PostgreSQL 17"},
		ScopeKind: domain.KnowledgeScopeProject, ScopeID: "local-agent",
		SourceClass: domain.KnowledgeSourceHuman, SourceRef: "slack-human:evt-1",
		AuthorID: "U00000001", Status: domain.KnowledgeClaimAsserted, Revision: 1,
	}
	valid := KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalClaim, ID: "claim_0001", Claim: &claim}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid union rejected: %v", err)
	}
	rejected := []struct {
		name string
		item KnowledgeAuthoritativeItem
	}{
		{"zero payloads", KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalClaim, ID: "claim_0001"}},
		{"unknown kind", KnowledgeAuthoritativeItem{Kind: "invented", Claim: &claim}},
		{"mismatched identity", KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalClaim, ID: "other", Claim: &claim}},
		{"claim kind with document payload", KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalClaim, ID: "claim_0001", Document: &domain.KnowledgeDocument{ID: "claim_0001"}}},
		{"invalid claim payload", KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalClaim, ID: "claim_0001", Claim: &domain.KnowledgeClaim{ID: "claim_0001"}}},
	}
	for _, candidate := range rejected {
		if err := candidate.item.Validate(); !errors.Is(err, ErrKnowledgeValidation) {
			t.Errorf("%s error = %v, want ErrKnowledgeValidation", candidate.name, err)
		}
	}
}

func TestKnowledgeQueueStoreEnqueueResetsFailedGeneration(t *testing.T) {
	now := time.Now().UTC()
	store := newFakeKnowledgeQueueStore(t, []domain.KnowledgeQueueItem{{
		Kind: domain.KnowledgeRetrievalDocument, ID: "d1", Status: domain.KnowledgeQueueFailed,
		Generation: 5, Attempts: 3, CreatedAt: now, UpdatedAt: now,
	}})
	item, err := store.Enqueue(context.Background(), domain.KnowledgeRetrievalDocument, "d1")
	if err != nil || item.Status != domain.KnowledgeQueuePending || item.Generation != 6 || item.Attempts != 0 {
		t.Fatalf("re-enqueued item = %+v, %v", item, err)
	}
	listed, err := store.List(context.Background(), domain.KnowledgeRetrievalDocument, "", 8)
	if err != nil || len(listed) != 1 || listed[0].Generation != 6 {
		t.Fatalf("listed items = %+v, %v", listed, err)
	}
}

func TestKnowledgeRetrieverFakeValidatesRequestBeforeReturning(t *testing.T) {
	retriever := fakeKnowledgeRetriever{result: domain.KnowledgeRetrievalResult{Cards: []domain.KnowledgeFrameCard{{}}}}
	invalid := domain.KnowledgeRetrievalRequest{}
	if _, err := retriever.Retrieve(context.Background(), invalid); err == nil {
		t.Fatal("retriever served results for an invalid request")
	}
}

func TestEmbeddingProviderAndResolverFakesBehave(t *testing.T) {
	provider := fakeEmbeddingProvider{vectors: [][]float32{{0.5, 0.5}}}
	vectors, err := provider.Embed(context.Background(), []string{"api"})
	if err != nil || len(vectors) != 1 {
		t.Fatalf("embedding fake = %v, %v", vectors, err)
	}
	resolver := fakeKnowledgeDocumentResolver{content: map[string][]byte{"kdoc_1": []byte("verified bytes")}}
	content, err := resolver.Resolve(context.Background(), domain.KnowledgeDocument{ID: "kdoc_1"}, domain.DefaultKnowledgeRetrievalLimits())
	if err != nil || strings.TrimSpace(string(content)) != "verified bytes" {
		t.Fatalf("resolver fake = %q, %v", content, err)
	}
	if _, err := resolver.Resolve(context.Background(), domain.KnowledgeDocument{ID: "missing"}, domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, ErrKnowledgeNotFound) {
		t.Fatalf("missing document error = %v", err)
	}
}
