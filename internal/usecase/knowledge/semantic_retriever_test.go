package knowledge

import (
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/testutil"
)

func semanticTestLimits() domain.KnowledgeRetrievalLimits {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	limits.EmbeddingDimensions = 3
	return limits
}

func semanticTestProvider() *testutil.FakeEmbeddingProvider {
	return testutil.NewFakeEmbeddingProvider(3).SetVectors([][]float32{{1, 0, 0}})
}

func TestRetrieverSemanticHitReachesCardWithSemanticReason(t *testing.T) {
	limits := semanticTestLimits()
	identity := "kclaim_000000000000000000000010"
	reader := newRetrievalFakeReader()
	index := &retrievalFakeIndex{semanticHits: []port.KnowledgeIndexHit{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Rank: 1, Revision: 1, SourceDigest: ""},
	}}
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, identity)] = retrievalTestProjectClaim(identity, "semantic subject", "value")
	text, _ := BuildKnowledgeIndexText(domain.KnowledgeRetrievalClaim, retrievalTestProjectClaim(identity, "semantic subject", "value"), "", nil)
	index.semanticHits[0].SourceDigest = text.SourceDigest

	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{
		Reader: reader, Index: index, Resolver: resolver, Queue: queue,
		Provider: semanticTestProvider(),
		Clock:    retrievalTestClock{now: time.Now()},
	})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("any query", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 1 || result.Cards[0].Identity() != "claim:"+identity {
		t.Fatalf("Retrieve() cards = %v, want the semantic card", result.Cards)
	}
	if result.Cards[0].Claim.RetrievalReason != string(domain.KnowledgeRetrievalReasonSemantic) {
		t.Fatalf("card reason = %q, want semantic", result.Cards[0].Claim.RetrievalReason)
	}
	if len(result.Diagnostics.Failures) != 0 {
		t.Fatalf("failures = %v, want none", result.Diagnostics.Failures)
	}
	channels := result.Diagnostics.EnabledChannels
	found := false
	for _, channel := range channels {
		if channel == domain.KnowledgeRetrievalChannelSemantic {
			found = true
		}
	}
	if !found {
		t.Fatalf("enabled channels = %v, want semantic present with a provider wired", channels)
	}
}

func TestRetrieverSemanticFailuresDegradeOnlySemanticChannel(t *testing.T) {
	limits := semanticTestLimits()
	cases := []struct {
		name     string
		provider *testutil.FakeEmbeddingProvider
		index    *retrievalFakeIndex
	}{
		{
			name:     "provider error",
			provider: testutil.NewFakeEmbeddingProvider(3).SetErr(port.ErrKnowledgeUnavailable),
			index:    &retrievalFakeIndex{},
		},
		{
			name:     "zero norm output",
			provider: testutil.NewFakeEmbeddingProvider(3).SetVectors([][]float32{{0, 0, 0}}),
			index:    &retrievalFakeIndex{},
		},
		{
			name:     "search failure",
			provider: semanticTestProvider(),
			index:    &retrievalFakeIndex{semanticErr: port.ErrKnowledgeUnavailable},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			identity := "kclaim_000000000000000000000011"
			reader := newRetrievalFakeReader()
			reader.exact = []port.KnowledgeEligibleCandidate{
				{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Subject: "safe subject", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project", Revision: 1},
			}
			reader.items[reader.key(domain.KnowledgeRetrievalClaim, identity)] = retrievalTestProjectClaim(identity, "safe subject", "value")
			resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
			queue := &retrievalFakeQueue{}
			retriever, err := NewRetriever(RetrieverDependencies{
				Reader: reader, Index: tc.index, Resolver: resolver, Queue: queue,
				Provider: tc.provider,
				Clock:    retrievalTestClock{now: time.Now()},
			})
			if err != nil {
				t.Fatalf("NewRetriever() error = %v", err)
			}
			result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("safe subject", nil, limits))
			if err != nil {
				t.Fatalf("Retrieve() error = %v", err)
			}
			if len(result.Cards) != 1 || result.Cards[0].Identity() != "claim:"+identity {
				t.Fatalf("Retrieve() cards = %v, want the exact card to survive the semantic failure", result.Cards)
			}
			wantFailures := []domain.KnowledgeRetrievalFailure{domain.KnowledgeRetrievalSemanticUnavailable}
			if !equalFailures(result.Diagnostics.Failures, wantFailures) {
				t.Fatalf("failures = %v, want %v", result.Diagnostics.Failures, wantFailures)
			}
		})
	}
}

func TestRetrieverStaleSemanticHitExcludedAndReenqueued(t *testing.T) {
	limits := semanticTestLimits()
	fresh := "kclaim_000000000000000000000012"
	stale := "kclaim_000000000000000000000013"
	reader := newRetrievalFakeReader()
	index := &retrievalFakeIndex{semanticHits: []port.KnowledgeIndexHit{
		{Kind: domain.KnowledgeRetrievalClaim, ID: stale, Rank: 1, Revision: 1, SourceDigest: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
		{Kind: domain.KnowledgeRetrievalClaim, ID: fresh, Rank: 2, Revision: 1, SourceDigest: ""},
	}}
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, stale)] = retrievalTestProjectClaim(stale, "stale semantic subject", "value")
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, fresh)] = retrievalTestProjectClaim(fresh, "fresh semantic subject", "value")
	text, _ := BuildKnowledgeIndexText(domain.KnowledgeRetrievalClaim, retrievalTestProjectClaim(fresh, "fresh semantic subject", "value"), "", nil)
	index.semanticHits[1].SourceDigest = text.SourceDigest

	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{
		Reader: reader, Index: index, Resolver: resolver, Queue: queue,
		Provider: semanticTestProvider(),
		Clock:    retrievalTestClock{now: time.Now()},
	})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("any query", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 1 || result.Cards[0].Identity() != "claim:"+fresh {
		t.Fatalf("Retrieve() cards = %v, want only the fresh semantic card", result.Cards)
	}
	enqueued := false
	for _, pair := range queue.enqueued {
		if pair[1] == stale {
			enqueued = true
		}
	}
	if !enqueued {
		t.Fatalf("repair enqueues = %v, want the stale semantic identity", queue.enqueued)
	}
}

func TestRetrieverExactAndSemanticCrossChannelReasonsFuse(t *testing.T) {
	limits := semanticTestLimits()
	identity := "kclaim_000000000000000000000014"
	reader := newRetrievalFakeReader()
	reader.exact = []port.KnowledgeEligibleCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Subject: "shared subject", ScopeKind: domain.KnowledgeScopeProject, ScopeID: "my-project", Revision: 1},
	}
	index := &retrievalFakeIndex{semanticHits: []port.KnowledgeIndexHit{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Rank: 2, Revision: 1, SourceDigest: ""},
	}}
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, identity)] = retrievalTestProjectClaim(identity, "shared subject", "value")
	text, _ := BuildKnowledgeIndexText(domain.KnowledgeRetrievalClaim, retrievalTestProjectClaim(identity, "shared subject", "value"), "", nil)
	index.semanticHits[0].SourceDigest = text.SourceDigest

	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}
	retriever, err := NewRetriever(RetrieverDependencies{
		Reader: reader, Index: index, Resolver: resolver, Queue: queue,
		Provider: semanticTestProvider(),
		Clock:    retrievalTestClock{now: time.Now()},
	})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err := retriever.Retrieve(t.Context(), retrievalTestRequest("shared subject", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 1 || result.Cards[0].Identity() != "claim:"+identity {
		t.Fatalf("Retrieve() cards = %v, want the fused identity once", result.Cards)
	}
	// Exact attribution wins the frozen precedence even though the
	// semantic contribution fused into the same candidate.
	if result.Cards[0].Claim.RetrievalReason != string(domain.KnowledgeRetrievalReasonExactSubject) {
		t.Fatalf("card reason = %q, want exact_subject", result.Cards[0].Claim.RetrievalReason)
	}
	if result.Diagnostics.CandidateCount != 2 {
		t.Fatalf("candidate count = %d, want 2 (exact + semantic)", result.Diagnostics.CandidateCount)
	}
}

func TestRetrieverEnabledChannelsReflectsProviderPresence(t *testing.T) {
	limits := semanticTestLimits()
	reader := newRetrievalFakeReader()
	index := &retrievalFakeIndex{}
	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}

	withoutProvider, err := NewRetriever(RetrieverDependencies{
		Reader: reader, Index: index, Resolver: resolver, Queue: queue,
		Clock: retrievalTestClock{now: time.Now()},
	})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err := withoutProvider.Retrieve(t.Context(), retrievalTestRequest("   ", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	for _, channel := range result.Diagnostics.EnabledChannels {
		if channel == domain.KnowledgeRetrievalChannelSemantic {
			t.Fatal("enabled channels contain semantic without a provider")
		}
	}

	withProvider, err := NewRetriever(RetrieverDependencies{
		Reader: reader, Index: index, Resolver: resolver, Queue: queue,
		Provider: semanticTestProvider(),
		Clock:    retrievalTestClock{now: time.Now()},
	})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	result, err = withProvider.Retrieve(t.Context(), retrievalTestRequest("   ", nil, limits))
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	found := false
	for _, channel := range result.Diagnostics.EnabledChannels {
		if channel == domain.KnowledgeRetrievalChannelSemantic {
			found = true
		}
	}
	if !found {
		t.Fatalf("enabled channels = %v, want semantic with a provider wired", result.Diagnostics.EnabledChannels)
	}
}

func TestRetrieverObservesEmbeddingRequestDurationOutcomes(t *testing.T) {
	limits := semanticTestLimits()
	identity := "kclaim_000000000000000000000015"
	successIndex := &retrievalFakeIndex{semanticHits: []port.KnowledgeIndexHit{
		{Kind: domain.KnowledgeRetrievalClaim, ID: identity, Rank: 1, Revision: 1, SourceDigest: ""},
	}}
	item := retrievalTestProjectClaim(identity, "metric subject", "value")
	text, _ := BuildKnowledgeIndexText(domain.KnowledgeRetrievalClaim, item, "", nil)
	successIndex.semanticHits[0].SourceDigest = text.SourceDigest

	reader := newRetrievalFakeReader()
	reader.items[reader.key(domain.KnowledgeRetrievalClaim, identity)] = item
	resolver := &retrievalFakeResolver{content: map[string]string{}, err: map[string]error{}}
	queue := &retrievalFakeQueue{}
	metrics := &retrievalMetricCapture{}
	retriever, err := NewRetriever(RetrieverDependencies{
		Reader: reader, Index: successIndex, Resolver: resolver, Queue: queue,
		Provider: semanticTestProvider(),
		Clock:    retrievalTestClock{now: time.Now()},
		Metrics:  metrics,
	})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	if _, err := retriever.Retrieve(t.Context(), retrievalTestRequest("metric subject", nil, limits)); err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	success, ok := metrics.findWithLabels(domain.MetricKnowledgeEmbeddingRequestDuration, port.MetricLabels{domain.MetricLabelOutcome: string(domain.KnowledgeRetrievalOutcomeSuccess)})
	if !ok || success.Kind != port.MetricKindObservation {
		t.Fatalf("success embedding duration sample = %#v found=%t", success, ok)
	}

	failing := &retrievalFakeIndex{}
	failMetrics := &retrievalMetricCapture{}
	failingRetriever, err := NewRetriever(RetrieverDependencies{
		Reader: newRetrievalFakeReader(), Index: failing, Resolver: resolver, Queue: queue,
		Provider: testutil.NewFakeEmbeddingProvider(3).SetErr(port.ErrKnowledgeUnavailable),
		Clock:    retrievalTestClock{now: time.Now()},
		Metrics:  failMetrics,
	})
	if err != nil {
		t.Fatalf("NewRetriever() error = %v", err)
	}
	if _, err := failingRetriever.Retrieve(t.Context(), retrievalTestRequest("metric subject", nil, limits)); err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	unavailable, ok := failMetrics.findWithLabels(domain.MetricKnowledgeEmbeddingRequestDuration, port.MetricLabels{domain.MetricLabelOutcome: string(domain.KnowledgeRetrievalOutcomeUnavailable)})
	if !ok || unavailable.Kind != port.MetricKindObservation {
		t.Fatalf("unavailable embedding duration sample = %#v found=%t", unavailable, ok)
	}
}
