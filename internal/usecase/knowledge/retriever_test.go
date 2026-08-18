package knowledge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// retrievalFakeReader is a scripted candidate reader for retriever tests.
type retrievalFakeReader struct {
	mu         sync.Mutex
	exact      []port.KnowledgeEligibleCandidate
	related    []port.KnowledgeEligibleCandidate
	items      map[string]port.KnowledgeAuthoritativeItem
	exactErr   error
	relatedErr error
	itemErr    map[string]error
	reads      int
	block      bool
}

func newRetrievalFakeReader() *retrievalFakeReader {
	return &retrievalFakeReader{items: make(map[string]port.KnowledgeAuthoritativeItem), itemErr: make(map[string]error)}
}

func (f *retrievalFakeReader) key(kind domain.KnowledgeRetrievalItemKind, id string) string {
	return string(kind) + "\x00" + id
}

func (f *retrievalFakeReader) ReadExact(ctx context.Context, _ domain.KnowledgeWriteBinding, _ time.Time, _ domain.KnowledgeRetrievalLimits, _ string, _ []string) ([]port.KnowledgeEligibleCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return append([]port.KnowledgeEligibleCandidate(nil), f.exact...), f.exactErr
}

func (f *retrievalFakeReader) ReadRelated(_ context.Context, _ domain.KnowledgeWriteBinding, _ time.Time, _ domain.KnowledgeRetrievalLimits, _ []port.KnowledgeEligibleCandidate) ([]port.KnowledgeEligibleCandidate, error) {
	return append([]port.KnowledgeEligibleCandidate(nil), f.related...), f.relatedErr
}

func (f *retrievalFakeReader) ReadItem(_ context.Context, _ domain.KnowledgeWriteBinding, _ time.Time, _ domain.KnowledgeRetrievalLimits, kind domain.KnowledgeRetrievalItemKind, id string) (port.KnowledgeAuthoritativeItem, error) {
	if err := f.itemErr[f.key(kind, id)]; err != nil {
		return port.KnowledgeAuthoritativeItem{}, err
	}
	item, ok := f.items[f.key(kind, id)]
	if !ok {
		return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: fake item missing", port.ErrKnowledgeNotFound)
	}
	return item, nil
}

// retrievalFakeIndex is a scripted lexical and semantic index for retriever
// tests.
type retrievalFakeIndex struct {
	hits          []port.KnowledgeIndexHit
	err           error
	calls         int
	semanticHits  []port.KnowledgeIndexHit
	semanticErr   error
	semanticCalls int
}

func (f *retrievalFakeIndex) SearchLexical(_ context.Context, _ []domain.KnowledgeScopeRef, _, _ string, _ int) ([]port.KnowledgeIndexHit, error) {
	f.calls++
	return append([]port.KnowledgeIndexHit(nil), f.hits...), f.err
}

func (f *retrievalFakeIndex) SearchSemantic(_ context.Context, _ []domain.KnowledgeScopeRef, _ string, _ []float32, _, _ int) ([]port.KnowledgeIndexHit, error) {
	f.semanticCalls++
	return append([]port.KnowledgeIndexHit(nil), f.semanticHits...), f.semanticErr
}

// retrievalFakeResolver resolves fixed content for documents.
type retrievalFakeResolver struct {
	content map[string]string
	err     map[string]error
}

func (f *retrievalFakeResolver) Resolve(_ context.Context, document domain.KnowledgeDocument, _ domain.KnowledgeRetrievalLimits) ([]byte, error) {
	if err := f.err[string(document.ID)]; err != nil {
		return nil, err
	}
	return []byte(f.content[string(document.ID)]), nil
}

// retrievalFakeQueue records stale-repair enqueues.
type retrievalFakeQueue struct {
	mu       sync.Mutex
	enqueued [][2]string
}

func (f *retrievalFakeQueue) Enqueue(_ context.Context, kind domain.KnowledgeRetrievalItemKind, itemID string) (domain.KnowledgeQueueItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, [2]string{string(kind), itemID})
	return domain.KnowledgeQueueItem{Kind: kind, ID: itemID, Generation: 1}, nil
}

func (f *retrievalFakeQueue) ClaimNext(_ context.Context, _ domain.KnowledgeRetrievalItemKind, _ time.Time, _ time.Duration) (domain.KnowledgeQueueItem, bool, error) {
	return domain.KnowledgeQueueItem{}, false, nil
}

func (f *retrievalFakeQueue) Complete(_ context.Context, _ domain.KnowledgeQueueClaim) error {
	return nil
}

func (f *retrievalFakeQueue) Retry(_ context.Context, _ domain.KnowledgeQueueClaim, _ time.Time) error {
	return nil
}

func (f *retrievalFakeQueue) Fail(_ context.Context, _ domain.KnowledgeQueueClaim, _ domain.KnowledgeQueueFailureCode) error {
	return nil
}

func (f *retrievalFakeQueue) List(_ context.Context, _ domain.KnowledgeRetrievalItemKind, _ string, _ int) ([]domain.KnowledgeQueueItem, error) {
	return nil, nil
}

// retrievalTestClock is a fixed clock for deterministic elapsed durations.
type retrievalTestClock struct{ now time.Time }

func (c retrievalTestClock) Now() time.Time { return c.now }

func retrievalTestRequest(message string, workstream *domain.WorkstreamSnapshot, limits domain.KnowledgeRetrievalLimits) domain.KnowledgeRetrievalRequest {
	project, workstreamID := "", ""
	if workstream != nil {
		project, workstreamID = workstream.Project, workstream.ID
	}
	return domain.KnowledgeRetrievalRequest{
		Binding: domain.KnowledgeWriteBinding{
			Team: "T00000001", Actor: "U00000001",
			Conversation: domain.ConversationKey("slack:T00000001:dm:C00000001"),
			Project:      project, WorkstreamID: workstreamID,
		},
		Workstream:     workstream,
		ExchangeTS:     "1700000000.000000",
		CurrentMessage: message,
		Now:            time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		Limits:         limits,
	}
}

func retrievalTestProjectClaim(id, subject, valueText string) port.KnowledgeAuthoritativeItem {
	return port.KnowledgeAuthoritativeItem{
		Kind: domain.KnowledgeRetrievalClaim, ID: id,
		Claim: &domain.KnowledgeClaim{
			ID: domain.KnowledgeClaimID(id), Subject: subject, Predicate: domain.KnowledgePredicateIs,
			Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: valueText},
			ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project",
			SourceClass: domain.KnowledgeSourceHuman, SourceRef: "src:" + id,
			Status: domain.KnowledgeClaimAsserted, Revision: 1,
		},
	}
}

func TestRetrieverRanksAndBuildsCardsWithReasonPrecedence(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	limits := domain.DefaultKnowledgeRetrievalLimits()
	limits.MaxCards = 8
	reader := newRetrievalFakeReader()
	reader.exact = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_000000000000000000000001", Subject: "db host", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project", Revision: 1},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_000000000000000000000002", Subject: "unrelated", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project", Revision: 1},
	}
	reader.related = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_000000000000000000000003", Subject: "related subject", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project", Revision: 1},
	}
	index := &retrievalFakeIndex{hits: []port.KnowledgeIndexHit{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_000000000000000000000004", Rank: 1, Revision: 1, SourceDigest: ""},
	}}
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, "kclaim_000000000000000000000001")] = retrievalTestProjectClaim("kclaim_000000000000000000000001", "db host", "db.internal")
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, "kclaim_000000000000000000000002")] = retrievalTestProjectClaim("kclaim_000000000000000000000002", "unrelated", "host")
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, "kclaim_000000000000000000000003")] = retrievalTestProjectClaim("kclaim_000000000000000000000003", "related subject", "value")
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, "kclaim_000000000000000000000004")] = retrievalTestProjectClaim("kclaim_000000000000000000000004", "lexical subject", "value")

	// The lexical hit must match its authoritative digest to survive.
	text, _ := BuildKnowledgeIndexText(domain.KnowledgeRetrievalClaim, retrievalTestProjectClaim("kclaim_000000000000000000000004", "lexical subject", "value"), "", nil)
	index.hits[0].SourceDigest = text.SourceDigest

	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{
		Reader: reader, Index: index, Resolver: resolver, Queue: queue,
		Clock: retrievalTestClock{now: now},
	})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}

	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("db host", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 4 {
		t.Fatalf("Retrieve() cards = %d, want 4", len(result.Cards))
	}
	// Deterministic order: tier order first, then identity within the
	// exact tier (both exact claims share tier 0 and order by ID).
	wantOrder := []string{"kclaim_000000000000000000000001", "kclaim_000000000000000000000002", "kclaim_000000000000000000000003", "kclaim_000000000000000000000004"}
	for i, card := range result.Cards {
		if card.Identity() != "claim:"+wantOrder[i] {
			t.Fatalf("card %d identity = %q, want %q", i, card.Identity(), "claim:"+wantOrder[i])
		}
	}
	// Card attribution reasons follow the frozen precedence: the subject
	// match carries exact_subject even though it sorts after the
	// identifier match within the tier.
	reasons := map[string]string{}
	for _, card := range result.Cards {
		reasons[string(card.Claim.ClaimID)] = card.Claim.RetrievalReason
	}
	wantReasons := map[string]string{
		"kclaim_000000000000000000000001": "exact_subject",
		"kclaim_000000000000000000000002": "exact_identifier",
		"kclaim_000000000000000000000003": "relation",
		"kclaim_000000000000000000000004": "lexical",
	}
	for id, want := range wantReasons {
		if reasons[id] != want {
			t.Fatalf("card %s reason = %q, want %q", id, reasons[id], want)
		}
	}
	if err := domain.ValidateKnowledgeRetrievalDiagnostics(result.Diagnostics); err != nil {
		t.Fatalf("diagnostics invalid: %v", err)
	}
	if result.Diagnostics.CandidateCount != 4 || result.Diagnostics.SelectedCount != 4 {
		t.Fatalf("diagnostics counts = %d/%d, want 4/4", result.Diagnostics.CandidateCount, result.Diagnostics.SelectedCount)
	}
}

func TestRetrieverEmptyQueryTouchesNothing(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	reader := newRetrievalFakeReader()
	reader.exact = []port.KnowledgeEligibleCandidate{{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_00000000000000000000000b", Subject: "s", Revision: 1}}
	index := &retrievalFakeIndex{}
	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{Reader: reader, Index: index, Resolver: resolver, Queue: queue, Clock: retrievalTestClock{now: time.Now()}})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("   ", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 0 {
		t.Fatalf("Retrieve() cards = %d, want 0", len(result.Cards))
	}
	if reader.reads != 0 || index.calls != 0 || len(queue.enqueued) != 0 {
		t.Fatalf("empty query touched reader %d index %d queue %d times", reader.reads, index.calls, len(queue.enqueued))
	}
	if err := domain.ValidateKnowledgeRetrievalDiagnostics(result.Diagnostics); err != nil {
		t.Fatalf("diagnostics invalid: %v", err)
	}
}

func TestRetrieverOptionalChannelFailuresPreserveSafeChannels(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	reader := newRetrievalFakeReader()
	reader.exact = []port.KnowledgeEligibleCandidate{{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_000000000000000000000005", Subject: "safe subject", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project", Revision: 1}}
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, "kclaim_000000000000000000000005")] = retrievalTestProjectClaim("kclaim_000000000000000000000005", "safe subject", "value")
	reader.relatedErr = errors.New("relation exploded")
	index := &retrievalFakeIndex{err: errors.New("fts exploded")}
	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{Reader: reader, Index: index, Resolver: resolver, Queue: queue, Clock: retrievalTestClock{now: time.Now()}})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("safe subject", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 1 || result.Cards[0].Identity() != "claim:kclaim_000000000000000000000005" {
		t.Fatalf("Retrieve() cards = %v, want the exact card preserved", result.Cards)
	}
	wantFailures := []domain.KnowledgeRetrievalFailure{
		domain.KnowledgeRetrievalLexicalUnavailable,
		domain.KnowledgeRetrievalRelationUnavailable,
	}
	if !equalFailures(result.Diagnostics.Failures, wantFailures) {
		t.Fatalf("failures = %v, want %v", result.Diagnostics.Failures, wantFailures)
	}
}

func TestRetrieverStaleLexicalHitsExcludedAndReenqueued(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	reader := newRetrievalFakeReader()
	index := &retrievalFakeIndex{hits: []port.KnowledgeIndexHit{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_000000000000000000000006", Rank: 1, Revision: 1, SourceDigest: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_000000000000000000000007", Rank: 2, Revision: 1, SourceDigest: ""},
	}}
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, "kclaim_000000000000000000000006")] = retrievalTestProjectClaim("kclaim_000000000000000000000006", "stale subject", "value")
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, "kclaim_000000000000000000000007")] = retrievalTestProjectClaim("kclaim_000000000000000000000007", "fresh subject", "value")
	text, _ := BuildKnowledgeIndexText(domain.KnowledgeRetrievalClaim, retrievalTestProjectClaim("kclaim_000000000000000000000007", "fresh subject", "value"), "", nil)
	index.hits[1].SourceDigest = text.SourceDigest
	// A hit whose authoritative row vanished is stale too.
	index.hits = append(index.hits, port.KnowledgeIndexHit{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_000000000000000000000008", Rank: 3, Revision: 1, SourceDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})

	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{Reader: reader, Index: index, Resolver: resolver, Queue: queue, Clock: retrievalTestClock{now: time.Now()}})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("any query", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 1 || result.Cards[0].Identity() != "claim:kclaim_000000000000000000000007" {
		t.Fatalf("Retrieve() cards = %v, want only the fresh card", result.Cards)
	}
	// Stale hits were enqueued for repair: digest-mismatched and missing
	// identities, never the fresh one.
	enqueued := queue.enqueued
	if len(enqueued) != 2 {
		t.Fatalf("repair enqueues = %v, want 2", enqueued)
	}
	seen := map[string]bool{}
	for _, pair := range enqueued {
		seen[pair[1]] = true
	}
	if !seen["kclaim_000000000000000000000006"] || !seen["kclaim_000000000000000000000008"] {
		t.Fatalf("repair enqueues = %v, want kclaim_000000000000000000000006 and kclaim_000000000000000000000008", enqueued)
	}
}

func TestRetrieverDocumentFailuresNeverFallBack(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	limits.MaxDocumentBytes = 1024
	reader := newRetrievalFakeReader()
	reader.exact = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_000000000000000000000001", Subject: "bad doc", ScopeKind: domain.KnowledgeScopeGlobal, Revision: 1},
		{Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_000000000000000000000002", Subject: "oversized doc", ScopeKind: domain.KnowledgeScopeGlobal, Revision: 1},
		{Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_000000000000000000000003", Subject: "good doc", ScopeKind: domain.KnowledgeScopeGlobal, Revision: 1},
	}
	document := func(id, subject string) port.KnowledgeAuthoritativeItem {
		return port.KnowledgeAuthoritativeItem{Kind: domain.KnowledgeRetrievalDocument, ID: id, Document: &domain.KnowledgeDocument{
			ID: domain.KnowledgeDocumentID(id), Subject: subject, ScopeKind: domain.KnowledgeScopeGlobal,
			Provenance: domain.KnowledgeProvenanceLegacyCurated, Status: domain.KnowledgeDocumentActive,
			ContentDigest: strings.Repeat("a", 64), ContentHandle: "memory_topics:t:revision:1",
			SourceID: "t", SourceRev: 1, Revision: 1,
		}}
	}
	reader.items[reader.key(domain.KnowledgeRetrievalDocument, "kdoc_000000000000000000000001")] = document("kdoc_000000000000000000000001", "bad doc")
	reader.items[reader.key(domain.KnowledgeRetrievalDocument, "kdoc_000000000000000000000002")] = document("kdoc_000000000000000000000002", "oversized doc")
	reader.items[reader.key(domain.KnowledgeRetrievalDocument, "kdoc_000000000000000000000003")] = document("kdoc_000000000000000000000003", "good doc")

	resolver := &retrievalFakeResolver{
		content: map[string]string{
			"kdoc_000000000000000000000001": "some content",
			"kdoc_000000000000000000000002": strings.Repeat("x", 2048),
			"kdoc_000000000000000000000003": "verified good content",
		},
		err: map[string]error{"kdoc_000000000000000000000001": port.ErrKnowledgeUnavailable},
	}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{Reader: reader, Index: &retrievalFakeIndex{}, Resolver: resolver, Queue: queue, Clock: retrievalTestClock{now: time.Now()}})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("good doc", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 1 || result.Cards[0].Identity() != "document:kdoc_000000000000000000000003" {
		t.Fatalf("Retrieve() cards = %v, want only the good document", result.Cards)
	}
	if result.Cards[0].Document.Content != "verified good content" {
		t.Fatalf("document content = %q, want verified content", result.Cards[0].Document.Content)
	}
	if len(result.Diagnostics.Omissions) != 1 || result.Diagnostics.Omissions[0] != domain.KnowledgeRetrievalOmissionDocumentOverLimit {
		t.Fatalf("omissions = %v, want document_over_limit", result.Diagnostics.Omissions)
	}
	// The failing document was enqueued for cleanup, never served.
	enqueued := false
	for _, pair := range queue.enqueued {
		if pair[1] == "kdoc_000000000000000000000001" {
			enqueued = true
		}
	}
	if !enqueued {
		t.Fatalf("cleanup enqueues = %v, want kdoc_000000000000000000000001", queue.enqueued)
	}
}

func TestRetrieverAuthoritativeFailureFailsClosed(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	reader := newRetrievalFakeReader()
	reader.exact = []port.KnowledgeEligibleCandidate{{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_000000000000000000000009", Subject: "subject", Revision: 1}}
	index := &retrievalFakeIndex{}
	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{Reader: reader, Index: index, Resolver: resolver, Queue: queue, Clock: retrievalTestClock{now: time.Now()}})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	_, err = retriever.Retrieve(t.Context(), retrievalTestRequest("subject", nil, limits))
	if !errors.Is(err, port.ErrKnowledgeUnavailable) {
		t.Fatalf("Retrieve() error = %v, want ErrKnowledgeUnavailable", err)
	}
}

func TestRetrieverRejectsInvalidRequestsAndTimesOut(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	reader := newRetrievalFakeReader()
	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{Reader: reader, Index: &retrievalFakeIndex{}, Resolver: resolver, Queue: queue, Clock: retrievalTestClock{now: time.Now()}})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}

	invalid := retrievalTestRequest("query", nil, limits)
	invalid.Binding.Team = "nope"
	if _, err := retriever.Retrieve(t.Context(), invalid); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("Retrieve(bad team) error = %v, want ErrKnowledgeValidation", err)
	}
	invalid = retrievalTestRequest("query", nil, limits)
	invalid.ExchangeTS = "not-a-ts"
	if _, err := retriever.Retrieve(t.Context(), invalid); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("Retrieve(bad exchange ts) error = %v, want ErrKnowledgeValidation", err)
	}
	invalid = retrievalTestRequest("query", nil, limits)
	invalid.Limits.MaxDocumentBytes = domain.HardMaxKnowledgeRetrievalMaxDocumentBytes + 1
	if _, err := retriever.Retrieve(t.Context(), invalid); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("Retrieve(bad limits) error = %v, want ErrKnowledgeValidation", err)
	}

	// Timeout: the reader blocks until the request context expires.
	blocked := newRetrievalFakeReader()
	blocked.block = true
	blockedRetriever, err := NewRetriever(RetrieverDependencies{Reader: blocked, Index: &retrievalFakeIndex{}, Resolver: resolver, Queue: queue, Clock: retrievalTestClock{now: time.Now()}})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	fastLimits := limits
	fastLimits.TimeoutSeconds = 1
	started := time.Now()
	if _, err := blockedRetriever.Retrieve(t.Context(), retrievalTestRequest("query", nil, fastLimits)); !errors.Is(err, port.ErrKnowledgeUnavailable) {
		t.Fatalf("Retrieve(timeout) error = %v, want ErrKnowledgeUnavailable", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("timeout took %v, want bounded by timeout_seconds", elapsed)
	}
}

func TestRetrieverRedactsQueryAndDocumentContent(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	reader := newRetrievalFakeReader()
	reader.exact = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_000000000000000000000004", Subject: "secret doc", ScopeKind: domain.KnowledgeScopeGlobal, Revision: 1},
	}
	reader.items[reader.key(domain.KnowledgeRetrievalDocument, "kdoc_000000000000000000000004")] = port.KnowledgeAuthoritativeItem{
		Kind: domain.KnowledgeRetrievalDocument, ID: "kdoc_000000000000000000000004",
		Document: &domain.KnowledgeDocument{
			ID: "kdoc_000000000000000000000004", Subject: "secret doc", ScopeKind: domain.KnowledgeScopeGlobal,
			Provenance: domain.KnowledgeProvenanceLegacyCurated, Status: domain.KnowledgeDocumentActive,
			ContentDigest: strings.Repeat("b", 64), ContentHandle: "memory_topics:t:revision:1",
			SourceID: "t", SourceRev: 1, Revision: 1,
		},
	}
	resolver := &retrievalFakeResolver{
		content: map[string]string{"kdoc_000000000000000000000004": "token AKIA_SECRET_KEY leaks here"},
		err:     map[string]error{},
	}
	queue := &retrievalFakeQueue{}
	redacted := false
	retriever, err := NewRetriever(RetrieverDependencies{
		Reader: reader, Index: &retrievalFakeIndex{}, Resolver: resolver, Queue: queue,
		Clock: retrievalTestClock{now: time.Now()},
		Redact: func(value string) string {
			if strings.Contains(value, "AKIA_SECRET_KEY") {
				redacted = true
				return strings.ReplaceAll(value, "AKIA_SECRET_KEY", "REDACTED")
			}
			return value
		},
	})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("secret doc", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if !redacted {
		t.Fatal("redactor never saw the credential-bearing content")
	}
	if len(result.Cards) != 1 || strings.Contains(result.Cards[0].Document.Content, "AKIA_SECRET_KEY") {
		t.Fatalf("card content leaks credentials: %+v", result.Cards)
	}
	if err := domain.ValidateKnowledgeRetrievalDiagnostics(result.Diagnostics); err != nil {
		t.Fatalf("diagnostics invalid: %v", err)
	}
}

func TestRetrieverWorkstreamGroundingFeedsQuery(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	limits.MaxQueryRunes = 256
	reader := newRetrievalFakeReader()
	reader.exact = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_00000000000000000000000a", Subject: "ship payment flow", ScopeKind: domain.KnowledgeScopeWorkstream, ScopeID: "ws-mine", Revision: 1},
	}
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, "kclaim_00000000000000000000000a")] = port.KnowledgeAuthoritativeItem{
		Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_00000000000000000000000a",
		Claim: &domain.KnowledgeClaim{
			ID: "kclaim_00000000000000000000000a", Subject: "ship payment flow", Predicate: domain.KnowledgePredicateIs,
			Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "value"},
			ScopeKind: domain.KnowledgeScopeWorkstream, ScopeID: "ws-mine",
			SourceClass: domain.KnowledgeSourceDecision, SourceRef: "src:kclaim_00000000000000000000000a",
			Status: domain.KnowledgeClaimAsserted, Revision: 1,
		},
	}
	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{Reader: reader, Index: &retrievalFakeIndex{}, Resolver: resolver, Queue: queue, Clock: retrievalTestClock{now: time.Now()}})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	workstream := &domain.WorkstreamSnapshot{
		ID: "ws-mine", Project: "my-project", OwnerActor: "U00000001",
		ConversationKey: domain.ConversationKey("slack:T00000001:dm:C00000001"),
		Status:          domain.WorkstreamActive,
		Objective:       "ship payment flow",
	}
	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("deictic follow-up", workstream, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 1 || result.Cards[0].Identity() != "claim:kclaim_00000000000000000000000a" {
		t.Fatalf("Retrieve() cards = %v, want the workstream-grounded card", result.Cards)
	}
}

func equalFailures(a, b []domain.KnowledgeRetrievalFailure) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRetrieverFusesCrossChannelReasons pins FIND-089: per-channel
// deduplication only; the ranker merges an identity matched by multiple
// channels and retains the complete reason set with the exact tier and the
// exact primary attribution reason.
func TestRetrieverFusesCrossChannelReasons(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	reader := newRetrievalFakeReader()
	identity := "kclaim_00000000000000000000000c"
	reader.exact = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Subject: "shared subject", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project", Revision: 1},
	}
	reader.related = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Subject: "shared subject", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project", Revision: 1},
	}
	index := &retrievalFakeIndex{hits: []port.KnowledgeIndexHit{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Rank: 1, Revision: 1, SourceDigest: ""},
	}}
	item := retrievalTestProjectClaim(identity, "shared subject", "value")
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, identity)] = item
	text, _ := BuildKnowledgeIndexText(domain.KnowledgeRetrievalClaim, item, "", nil)
	index.hits[0].SourceDigest = text.SourceDigest

	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{Reader: reader, Index: index, Resolver: resolver, Queue: queue, Clock: retrievalTestClock{now: time.Now()}})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("shared subject", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 1 {
		t.Fatalf("Retrieve() cards = %d, want the single fused identity", len(result.Cards))
	}
	if result.Cards[0].Identity() != "claim:"+identity {
		t.Fatalf("card identity = %q, want %q", result.Cards[0].Identity(), "claim:"+identity)
	}
	if result.Cards[0].Claim.RetrievalReason != "exact_subject" {
		t.Fatalf("card reason = %q, want exact_subject (exact tier wins attribution)", result.Cards[0].Claim.RetrievalReason)
	}
	if result.Diagnostics.CandidateCount != 3 {
		t.Fatalf("candidate count = %d, want 3 (exact + relation + lexical contributions)", result.Diagnostics.CandidateCount)
	}
	if len(result.Diagnostics.Failures) != 0 {
		t.Fatalf("failures = %v, want none", result.Diagnostics.Failures)
	}
}

// TestRetrieverOmitsInvalidCardsBeforeSelection pins FIND-091: a document
// whose verified content redacts to empty fails frame-card validation and
// is omitted whole with an index-cleanup enqueue, never returned.
func TestRetrieverOmitsInvalidCardsBeforeSelection(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	reader := newRetrievalFakeReader()
	identity := "kdoc_000000000000000000000005"
	reader.exact = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalDocument, ID: identity, Subject: "redacts empty", ScopeKind: domain.KnowledgeScopeGlobal, Revision: 1},
	}
	reader.items[reader.key(domain.KnowledgeRetrievalDocument, identity)] = port.KnowledgeAuthoritativeItem{
		Kind: domain.KnowledgeRetrievalDocument, ID: identity,
		Document: &domain.KnowledgeDocument{
			ID: domain.KnowledgeDocumentID(identity), Subject: "redacts empty", ScopeKind: domain.KnowledgeScopeGlobal,
			ContentDigest: strings.Repeat("c", 64), ContentHandle: "memory_topics:t:revision:1",
			SourceID: "t", SourceRev: 1,
			Provenance: domain.KnowledgeProvenanceLegacyCurated, Status: domain.KnowledgeDocumentActive, Revision: 1,
		},
	}
	resolver := &retrievalFakeResolver{
		content: map[string]string{identity: "TOP-SECRET-TOKEN"},
		err:     map[string]error{},
	}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{
		Reader: reader, Index: &retrievalFakeIndex{}, Resolver: resolver, Queue: queue,
		Clock: retrievalTestClock{now: time.Now()},
		Redact: func(value string) string {
			return strings.ReplaceAll(value, "TOP-SECRET-TOKEN", "")
		},
	})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("redacts empty", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 0 {
		t.Fatalf("Retrieve() cards = %v, want the redacted-empty document omitted", result.Cards)
	}
	if result.Diagnostics.OmittedCount != 1 {
		t.Fatalf("omitted count = %d, want 1", result.Diagnostics.OmittedCount)
	}
	if err := domain.ValidateKnowledgeRetrievalDiagnostics(result.Diagnostics); err != nil {
		t.Fatalf("diagnostics invalid: %v", err)
	}
	enqueued := false
	for _, pair := range queue.enqueued {
		if pair[1] == identity {
			enqueued = true
		}
	}
	if !enqueued {
		t.Fatalf("cleanup enqueues = %v, want the redacted-empty document", queue.enqueued)
	}
}

// TestBuildRankContributionsObservesFusedTierReasonsAndRank pins FIND-092
// through the pure contribution seam: an identity matched by exact,
// relation, lexical, and semantic produces four contributions and the
// ranker fuses them into one candidate with the exact tier, the complete
// sorted reason set, and the channel ranks preserved on the fused
// contributions. These assertions fail under the former cross-channel
// `seen` suppression.
func TestBuildRankContributionsObservesFusedTierReasonsAndRank(t *testing.T) {
	identity := "kclaim_00000000000000000000000d"
	exact := []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Subject: "shared subject", Revision: 1},
	}
	related := []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Subject: "shared subject", Revision: 1},
	}
	lexical := []port.KnowledgeIndexHit{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Rank: 7, Revision: 1},
	}
	semantic := []port.KnowledgeIndexHit{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Rank: 3, Revision: 1},
	}
	contributions := buildRankContributions(exact, related, lexical, semantic, "shared subject")
	if len(contributions) != 4 {
		t.Fatalf("contributions = %d, want 4 (exact + relation + lexical + semantic)", len(contributions))
	}
	tiers := map[domain.KnowledgeRankTier]bool{}
	lexicalRank, semanticRank := 0, 0
	for _, contribution := range contributions {
		tiers[contribution.Tier] = true
		if contribution.Tier == domain.KnowledgeRankTierFused {
			if contribution.LexicalRank > 0 {
				lexicalRank = contribution.LexicalRank
			}
			if contribution.SemanticRank > 0 {
				semanticRank = contribution.SemanticRank
			}
		}
	}
	if !tiers[domain.KnowledgeRankTierExact] || !tiers[domain.KnowledgeRankTierRelation] || !tiers[domain.KnowledgeRankTierFused] {
		t.Fatalf("contribution tiers = %v, want all three tiers present", tiers)
	}
	if lexicalRank != 7 {
		t.Fatalf("fused lexical rank = %d, want 7", lexicalRank)
	}
	if semanticRank != 3 {
		t.Fatalf("fused semantic rank = %d, want 3", semanticRank)
	}
	ranked, err := domain.RankKnowledgeCandidates(contributions)
	if err != nil {
		t.Fatalf("RankKnowledgeCandidates() error = %v", err)
	}
	if len(ranked) != 1 {
		t.Fatalf("ranked identities = %d, want the single fused identity", len(ranked))
	}
	if ranked[0].Tier != domain.KnowledgeRankTierExact {
		t.Fatalf("fused tier = %d, want exact", ranked[0].Tier)
	}
	wantReasons := []domain.KnowledgeRetrievalReason{
		domain.KnowledgeRetrievalReasonExactSubject,
		domain.KnowledgeRetrievalReasonLexical,
		domain.KnowledgeRetrievalReasonRelation,
		domain.KnowledgeRetrievalReasonSemantic,
	}
	if len(ranked[0].Reasons) != 4 {
		t.Fatalf("fused reasons = %v, want the complete merged set", ranked[0].Reasons)
	}
	for i, reason := range wantReasons {
		if ranked[0].Reasons[i] != reason {
			t.Fatalf("fused reason %d = %q, want %q", i, ranked[0].Reasons[i], reason)
		}
	}
}

// TestRetrieverStaleLexicalHitOnExactIdentityRepairEnqueues pins FIND-092:
// a lexical hit for an identity already matched exactly is still verified,
// and a stale digest enqueues repair even though the exact card survives.
func TestRetrieverStaleLexicalHitOnExactIdentityRepairEnqueues(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	identity := "kclaim_00000000000000000000000e"
	reader := newRetrievalFakeReader()
	reader.exact = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Subject: "shared subject", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project", Revision: 1},
	}
	index := &retrievalFakeIndex{hits: []port.KnowledgeIndexHit{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Rank: 1, Revision: 1,
			SourceDigest: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
	}}
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, identity)] = retrievalTestProjectClaim(identity, "shared subject", "value")

	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{Reader: reader, Index: index, Resolver: resolver, Queue: queue, Clock: retrievalTestClock{now: time.Now()}})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("shared subject", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 1 || result.Cards[0].Identity() != "claim:"+identity {
		t.Fatalf("Retrieve() cards = %v, want the exact card to survive the stale lexical hit", result.Cards)
	}
	enqueued := false
	for _, pair := range queue.enqueued {
		if pair[1] == identity {
			enqueued = true
		}
	}
	if !enqueued {
		t.Fatalf("repair enqueues = %v, want the stale lexical identity even though exact matched", queue.enqueued)
	}
	if result.Diagnostics.CandidateCount != 2 {
		t.Fatalf("candidate count = %d, want 2 (exact hit + stale lexical hit)", result.Diagnostics.CandidateCount)
	}
}

type retrievalMetricCapture struct {
	samples []port.MetricSample
}

func (m *retrievalMetricCapture) AddCounter(name string, delta int64, labels port.MetricLabels) {
	m.samples = append(m.samples, port.MetricSample{Name: name, Kind: port.MetricKindCounter, Value: float64(delta), Labels: labels})
}
func (m *retrievalMetricCapture) SetGauge(name string, value int64, labels port.MetricLabels) {
	m.samples = append(m.samples, port.MetricSample{Name: name, Kind: port.MetricKindGauge, Value: float64(value), Labels: labels})
}
func (m *retrievalMetricCapture) Observe(name string, value float64, labels port.MetricLabels) {
	m.samples = append(m.samples, port.MetricSample{Name: name, Kind: port.MetricKindObservation, Value: value, Labels: labels})
}
func (m *retrievalMetricCapture) Snapshot() []port.MetricSample {
	return append([]port.MetricSample(nil), m.samples...)
}

func (m *retrievalMetricCapture) findWithLabels(name string, labels port.MetricLabels) (port.MetricSample, bool) {
	for _, sample := range m.samples {
		if sample.Name != name || len(sample.Labels) != len(labels) {
			continue
		}
		match := true
		for key, value := range labels {
			if sample.Labels[key] != value {
				match = false
				break
			}
		}
		if match {
			return sample, true
		}
	}
	return port.MetricSample{}, false
}

// TestRetrieverEmitsClosedMetrics pins the retrieval metric boundaries: one
// total and one duration per attempt with the frozen outcome, per-channel
// candidate counts, per-channel failure counters with compatible reasons,
// stale-index counters, and the empty total for empty outcomes.
func TestRetrieverEmitsClosedMetrics(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	identity := "kclaim_00000000000000000000000f"
	reader := newRetrievalFakeReader()
	reader.exact = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Subject: "shared subject", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project", Revision: 1},
	}
	reader.related = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Subject: "shared subject", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project", Revision: 1},
	}
	index := &retrievalFakeIndex{hits: []port.KnowledgeIndexHit{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Rank: 1, Revision: 1, SourceDigest: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
	}}
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, identity)] = retrievalTestProjectClaim(identity, "shared subject", "value")
	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	metrics := &retrievalMetricCapture{}
	retriever, err := NewRetriever(RetrieverDependencies{Reader: reader, Index: index, Resolver: resolver, Queue: &retrievalFakeQueue{}, Clock: retrievalTestClock{now: time.Now()}, Metrics: metrics})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("shared subject", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(result.Cards))
	}
	success, ok := metrics.findWithLabels(domain.MetricKnowledgeRetrievalTotal, port.MetricLabels{domain.MetricLabelOutcome: string(domain.KnowledgeRetrievalOutcomeSuccess)})
	if !ok || success.Value != 1 {
		t.Fatalf("success total = %#v found=%t", success, ok)
	}
	if _, ok := metrics.findWithLabels(domain.MetricKnowledgeRetrievalDuration, port.MetricLabels{domain.MetricLabelOutcome: string(domain.KnowledgeRetrievalOutcomeSuccess)}); !ok {
		t.Fatal("duration metric with success outcome was not emitted")
	}
	if _, ok := metrics.findWithLabels(domain.MetricKnowledgeRetrievalCandidates, port.MetricLabels{domain.MetricLabelChannel: string(domain.KnowledgeRetrievalChannelExact)}); !ok {
		t.Fatal("exact candidate metric was not emitted")
	}
	if _, ok := metrics.findWithLabels(domain.MetricKnowledgeRetrievalCandidates, port.MetricLabels{domain.MetricLabelChannel: string(domain.KnowledgeRetrievalChannelRelation)}); !ok {
		t.Fatal("relation candidate metric was not emitted")
	}
	if _, ok := metrics.findWithLabels(domain.MetricKnowledgeRetrievalCandidates, port.MetricLabels{domain.MetricLabelChannel: string(domain.KnowledgeRetrievalChannelLexical)}); !ok {
		t.Fatal("lexical candidate metric was not emitted")
	}
	if _, ok := metrics.findWithLabels(domain.MetricKnowledgeRetrievalEmptyTotal, port.MetricLabels{domain.MetricLabelOutcome: string(domain.KnowledgeRetrievalOutcomeEmpty)}); ok {
		t.Fatal("empty total emitted for a non-empty retrieval")
	}
	for _, sample := range metrics.samples {
		for key := range sample.Labels {
			if key != domain.MetricLabelChannel && key != domain.MetricLabelOutcome && key != domain.MetricLabelReason {
				t.Fatalf("sample %s carries unadmitted label %q", sample.Name, key)
			}
		}
	}
}

func TestRetrieverEmitsChannelFailureAndStaleMetrics(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	identity := "kclaim_00000000000000000000000f"
	reader := newRetrievalFakeReader()
	reader.exact = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Subject: "shared subject", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project", Revision: 1},
	}
	reader.relatedErr = errors.New("relation channel exploded")
	index := &retrievalFakeIndex{hits: []port.KnowledgeIndexHit{
		// The index hit cites a stale revision that no authoritative item
		// can match: it is excluded and counted as a stale-index sample.
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Rank: 1, Revision: 999, SourceDigest: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
	}}
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, identity)] = retrievalTestProjectClaim(identity, "shared subject", "value")
	metrics := &retrievalMetricCapture{}
	retriever, err := NewRetriever(RetrieverDependencies{Reader: reader, Index: index, Resolver: &retrievalFakeResolver{}, Queue: &retrievalFakeQueue{}, Clock: retrievalTestClock{now: time.Now()}, Metrics: metrics})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	if _, err := retriever.Retrieve(t.Context(), retrievalTestRequest("shared subject", nil, limits)); err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if _, ok := metrics.findWithLabels(domain.MetricKnowledgeRetrievalChannelFailure, port.MetricLabels{domain.MetricLabelChannel: string(domain.KnowledgeRetrievalChannelRelation), domain.MetricLabelReason: string(domain.KnowledgeRetrievalRelationUnavailable)}); !ok {
		t.Fatal("relation channel failure metric was not emitted with the compatible channel/reason pair")
	}
	if _, ok := metrics.findWithLabels(domain.MetricKnowledgeRetrievalStaleIndex, port.MetricLabels{domain.MetricLabelChannel: string(domain.KnowledgeRetrievalChannelLexical), domain.MetricLabelReason: string(domain.KnowledgeRetrievalReasonLabelStaleIndex)}); !ok {
		t.Fatal("stale index metric was not emitted with the lexical channel and stale_index reason")
	}
}

func TestRetrieverEmitsValidationRejectedOutcome(t *testing.T) {
	metrics := &retrievalMetricCapture{}
	retriever, err := NewRetriever(RetrieverDependencies{Reader: newRetrievalFakeReader(), Index: &retrievalFakeIndex{}, Resolver: &retrievalFakeResolver{}, Queue: &retrievalFakeQueue{}, Clock: retrievalTestClock{now: time.Now()}, Metrics: metrics})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	_, err = retriever.Retrieve(t.Context(), domain.KnowledgeRetrievalRequest{})
	if !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("Retrieve() error = %v, want validation rejection", err)
	}
	if _, ok := metrics.findWithLabels(domain.MetricKnowledgeRetrievalTotal, port.MetricLabels{domain.MetricLabelOutcome: string(domain.KnowledgeRetrievalOutcomeValidationRejected)}); !ok {
		t.Fatal("validation-rejected total was not emitted")
	}
}

// TestRetrieverLateSuccessAfterDeadlineFailsClosed pins FIND-094: channels
// that ignore cancellation can still produce candidates, but a retrieval
// whose context expired or was cancelled never returns cards — the final
// deadline verification fails closed with the wrapped context error.
func TestRetrieverLateSuccessAfterDeadlineFailsClosed(t *testing.T) {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	identity := "kclaim_00000000000000000000000f"
	reader := newRetrievalFakeReader()
	reader.exact = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Subject: "shared subject", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project", Revision: 1},
	}
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, identity)] = retrievalTestProjectClaim(identity, "shared subject", "value")
	retriever, err := NewRetriever(RetrieverDependencies{Reader: reader, Index: &retrievalFakeIndex{}, Resolver: &retrievalFakeResolver{}, Queue: &retrievalFakeQueue{}, Clock: retrievalTestClock{now: time.Now()}})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := retriever.Retrieve(cancelled, retrievalTestRequest("shared subject", nil, limits))
	if !errors.Is(err, port.ErrKnowledgeUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Retrieve() error = %v, want unavailable wrapping the cancellation", err)
	}
	if len(result.Cards) != 0 {
		t.Fatalf("Retrieve() returned %d cards past the deadline", len(result.Cards))
	}
}
