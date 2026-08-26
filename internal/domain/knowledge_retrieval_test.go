package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func testRetrievalBinding() domain.KnowledgeWriteBinding {
	return domain.KnowledgeWriteBinding{
		Team:         "T00000001",
		Actor:        "U00000001",
		Conversation: domain.ConversationKey("slack:T00000001:dm:D00000001"),
	}
}

func testWorkstreamSnapshot() *domain.WorkstreamSnapshot {
	return &domain.WorkstreamSnapshot{
		ID:              "ws_0001",
		ConversationKey: domain.ConversationKey("slack:T00000001:dm:D00000001"),
		OwnerActor:      "U00000001",
		Project:         "local-agent",
		Status:          domain.WorkstreamActive,
	}
}

func testRetrievalRequest() domain.KnowledgeRetrievalRequest {
	return domain.KnowledgeRetrievalRequest{
		Binding:        testRetrievalBinding(),
		ExchangeTS:     "1723543200.123456",
		CurrentMessage: "postgresql",
		Now:            time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
	}
}

func TestKnowledgeRetrievalLimitsDefaultsAndHardMaxima(t *testing.T) {
	if err := domain.DefaultKnowledgeRetrievalLimits().Validate(); err != nil {
		t.Fatalf("default retrieval limits rejected: %v", err)
	}
	if err := (domain.KnowledgeRetrievalLimits{}).Validate(); err != nil {
		t.Fatalf("zero retrieval limits must fall back to defaults: %v", err)
	}
	if got := (domain.KnowledgeRetrievalLimits{}).WithDefaults(); got != domain.DefaultKnowledgeRetrievalLimits() {
		t.Fatalf("zero limits with defaults = %+v", got)
	}
	rejected := []struct {
		name   string
		mutate func(*domain.KnowledgeRetrievalLimits)
	}{
		{"zero timeout", func(l *domain.KnowledgeRetrievalLimits) { l.TimeoutSeconds = 31 }},
		{"timeout over hard max", func(l *domain.KnowledgeRetrievalLimits) {
			l.TimeoutSeconds = domain.HardMaxKnowledgeRetrievalTimeoutSeconds + 1
		}},
		{"query runes over hard max", func(l *domain.KnowledgeRetrievalLimits) {
			l.MaxQueryRunes = domain.HardMaxKnowledgeRetrievalMaxQueryRunes + 1
		}},
		{"candidates over hard max", func(l *domain.KnowledgeRetrievalLimits) {
			l.MaxCandidatesPerChannel = domain.HardMaxKnowledgeRetrievalMaxCandidatesPerChannel + 1
		}},
		{"cards over hard max", func(l *domain.KnowledgeRetrievalLimits) { l.MaxCards = domain.HardMaxKnowledgeRetrievalMaxCards + 1 }},
		{"card tokens over hard max", func(l *domain.KnowledgeRetrievalLimits) { l.MaxCardTokens = domain.HardMaxKnowledgeCardBudget + 1 }},
		{"document bytes over hard max", func(l *domain.KnowledgeRetrievalLimits) {
			l.MaxDocumentBytes = domain.HardMaxKnowledgeRetrievalMaxDocumentBytes + 1
		}},
		{"worker interval over hard max", func(l *domain.KnowledgeRetrievalLimits) {
			l.WorkerIntervalSeconds = domain.HardMaxKnowledgeRetrievalWorkerIntervalSeconds + 1
		}},
		{"worker retries over hard max", func(l *domain.KnowledgeRetrievalLimits) {
			l.WorkerMaxRetries = domain.HardMaxKnowledgeRetrievalWorkerMaxRetries + 1
		}},
		{"worker batch over hard max", func(l *domain.KnowledgeRetrievalLimits) {
			l.WorkerBatchSize = domain.HardMaxKnowledgeRetrievalWorkerBatchSize + 1
		}},
		{"embedding timeout over hard max", func(l *domain.KnowledgeRetrievalLimits) {
			l.EmbeddingTimeoutSeconds = domain.HardMaxKnowledgeEmbeddingTimeoutSeconds + 1
		}},
		{"embedding dimensions over hard max", func(l *domain.KnowledgeRetrievalLimits) {
			l.EmbeddingDimensions = domain.HardMaxKnowledgeEmbeddingDimensions + 1
		}},
		{"negative embedding dimensions", func(l *domain.KnowledgeRetrievalLimits) { l.EmbeddingDimensions = -1 }},
		{"similarity over hard max", func(l *domain.KnowledgeRetrievalLimits) {
			l.MinSimilarityBasisPoints = domain.HardMaxKnowledgeMinSimilarityBasisPoints + 1
		}},
		{"negative similarity", func(l *domain.KnowledgeRetrievalLimits) { l.MinSimilarityBasisPoints = -1 }},
	}
	for _, candidate := range rejected {
		limits := domain.DefaultKnowledgeRetrievalLimits()
		candidate.mutate(&limits)
		if err := limits.Validate(); !errors.Is(err, domain.ErrKnowledgeLimitExceeded) {
			t.Errorf("%s error = %v, want ErrKnowledgeLimitExceeded", candidate.name, err)
		}
	}
}

func TestKnowledgeRetrievalRequestRequiresTrustedBindingAndTurnSelector(t *testing.T) {
	valid := testRetrievalRequest()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid retrieval request rejected: %v", err)
	}
	noTeam := valid
	noTeam.Binding.Team = ""
	if err := noTeam.Validate(); err == nil || !strings.Contains(err.Error(), "team") {
		t.Fatalf("missing team error = %v", err)
	}
	noActor := valid
	noActor.Binding.Actor = "human-name"
	if err := noActor.Validate(); err == nil || !strings.Contains(err.Error(), "actor") {
		t.Fatalf("implausible actor error = %v", err)
	}
	noConversation := valid
	noConversation.Binding.Conversation = ""
	if err := noConversation.Validate(); err == nil || !strings.Contains(err.Error(), "conversation") {
		t.Fatalf("missing conversation error = %v", err)
	}
	noExchange := valid
	noExchange.ExchangeTS = ""
	if err := noExchange.Validate(); err == nil || !strings.Contains(err.Error(), "timestamp") {
		t.Fatalf("missing turn selector error = %v", err)
	}
	malformedExchange := valid
	malformedExchange.ExchangeTS = "not-a-timestamp"
	if err := malformedExchange.Validate(); err == nil || !strings.Contains(err.Error(), "timestamp") {
		t.Fatalf("malformed turn selector error = %v", err)
	}
	noClock := valid
	noClock.Now = time.Time{}
	if err := noClock.Validate(); err == nil || !strings.Contains(err.Error(), "clock") {
		t.Fatalf("missing clock error = %v", err)
	}
	badLimits := valid
	badLimits.Limits = domain.KnowledgeRetrievalLimits{MaxCards: domain.HardMaxKnowledgeRetrievalMaxCards + 1}
	if err := badLimits.Validate(); !errors.Is(err, domain.ErrKnowledgeLimitExceeded) {
		t.Fatalf("invalid limits error = %v", err)
	}
}

func TestKnowledgeRetrievalRequestRejectsCrossTeamConversation(t *testing.T) {
	request := testRetrievalRequest()
	request.Binding.Conversation = domain.ConversationKey("slack:T99999999:dm:D00000001")
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "team") {
		t.Fatalf("cross-team conversation error = %v", err)
	}
	request = testRetrievalRequest()
	request.Binding.Conversation = domain.ConversationKey("slack:T00000001:channel:C00000001:thread:1723543200.123456")
	if err := request.Validate(); err != nil {
		t.Fatalf("canonical thread conversation rejected: %v", err)
	}
}

func TestKnowledgeRetrievalRequestRequiresSnapshotCoherence(t *testing.T) {
	// A nil snapshot must not carry project or workstream scope.
	nilSnapshot := testRetrievalRequest()
	nilSnapshot.Binding.Project = "local-agent"
	if err := nilSnapshot.Validate(); err == nil || !strings.Contains(err.Error(), "nil workstream") {
		t.Fatalf("nil snapshot with project error = %v", err)
	}
	nilSnapshot = testRetrievalRequest()
	nilSnapshot.Binding.WorkstreamID = "ws_0001"
	if err := nilSnapshot.Validate(); err == nil || !strings.Contains(err.Error(), "nil workstream") {
		t.Fatalf("nil snapshot with workstream error = %v", err)
	}

	// A coherent snapshot must validate.
	bound := testRetrievalRequest()
	snapshot := testWorkstreamSnapshot()
	bound.Workstream = snapshot
	bound.Binding.Project = snapshot.Project
	bound.Binding.WorkstreamID = snapshot.ID
	if err := bound.Validate(); err != nil {
		t.Fatalf("coherent snapshot request rejected: %v", err)
	}

	hostile := []struct {
		name   string
		mutate func(*domain.KnowledgeRetrievalRequest)
	}{
		{"cross actor", func(r *domain.KnowledgeRetrievalRequest) { r.Workstream.OwnerActor = "U99999999" }},
		{"cross conversation", func(r *domain.KnowledgeRetrievalRequest) {
			r.Workstream.ConversationKey = domain.ConversationKey("slack:T00000001:dm:D99999999")
		}},
		{"cross project", func(r *domain.KnowledgeRetrievalRequest) { r.Workstream.Project = "other-project" }},
		{"cross workstream", func(r *domain.KnowledgeRetrievalRequest) { r.Workstream.ID = "ws_other" }},
		{"binding project mismatch", func(r *domain.KnowledgeRetrievalRequest) { r.Binding.Project = "other-project" }},
		{"binding workstream mismatch", func(r *domain.KnowledgeRetrievalRequest) { r.Binding.WorkstreamID = "ws_other" }},
		{"empty snapshot identity", func(r *domain.KnowledgeRetrievalRequest) { r.Workstream.ID = "" }},
		{"empty snapshot project", func(r *domain.KnowledgeRetrievalRequest) { r.Workstream.Project = "" }},
		{"paused snapshot", func(r *domain.KnowledgeRetrievalRequest) { r.Workstream.Status = domain.WorkstreamPaused }},
		{"blocked snapshot", func(r *domain.KnowledgeRetrievalRequest) { r.Workstream.Status = domain.WorkstreamBlocked }},
		{"completed snapshot", func(r *domain.KnowledgeRetrievalRequest) { r.Workstream.Status = domain.WorkstreamCompleted }},
		{"cancelled snapshot", func(r *domain.KnowledgeRetrievalRequest) { r.Workstream.Status = domain.WorkstreamCancelled }},
		{"proposed snapshot", func(r *domain.KnowledgeRetrievalRequest) { r.Workstream.Status = domain.WorkstreamProposed }},
	}
	for _, candidate := range hostile {
		request := testRetrievalRequest()
		snap := testWorkstreamSnapshot()
		request.Workstream = snap
		request.Binding.Project = snap.Project
		request.Binding.WorkstreamID = snap.ID
		candidate.mutate(&request)
		if err := request.Validate(); err == nil {
			t.Errorf("%s: incoherent snapshot request accepted", candidate.name)
		}
	}
}

func rank(t *testing.T, candidates ...domain.KnowledgeRankCandidate) []domain.KnowledgeRankedCandidate {
	t.Helper()
	ranked, err := domain.RankKnowledgeCandidates(candidates)
	if err != nil {
		t.Fatalf("ranking error = %v", err)
	}
	return ranked
}

func TestKnowledgeRankingTiersOrderBeforeFusion(t *testing.T) {
	ranked := rank(
		t,
		domain.KnowledgeRankCandidate{
			Kind:         domain.KnowledgeRetrievalDocument,
			ID:           "doc-2",
			Tier:         domain.KnowledgeRankTierFused,
			Reasons:      []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonSemantic},
			SemanticRank: 1,
		},
		domain.KnowledgeRankCandidate{
			Kind:        domain.KnowledgeRetrievalClaim,
			ID:          "claim-1",
			Tier:        domain.KnowledgeRankTierFused,
			Reasons:     []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonLexical},
			LexicalRank: 1,
		},
		domain.KnowledgeRankCandidate{
			Kind:    domain.KnowledgeRetrievalClaim,
			ID:      "claim-rel",
			Tier:    domain.KnowledgeRankTierRelation,
			Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonRelation},
		},
		domain.KnowledgeRankCandidate{
			Kind:    domain.KnowledgeRetrievalClaim,
			ID:      "claim-exact",
			Tier:    domain.KnowledgeRankTierExact,
			Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonExactIdentifier},
		},
	)
	want := []string{"claim-exact", "claim-rel", "claim-1", "doc-2"}
	if len(ranked) != len(want) {
		t.Fatalf("ranked count = %d, want %d", len(ranked), len(want))
	}
	for i, id := range want {
		if ranked[i].ID != id {
			t.Errorf("ranked[%d] = %s/%s, want %s", i, ranked[i].Kind, ranked[i].ID, id)
		}
	}
}

func TestKnowledgeRankingIntegerRRFIsDeterministic(t *testing.T) {
	// rank 1 contributes 1_000_000/61 and rank 2 contributes 1_000_000/62
	// with integer division.
	const wantRank1 = 1_000_000 / 61
	const wantRank2 = 1_000_000 / 62
	ranked := rank(
		t,
		domain.KnowledgeRankCandidate{
			Kind:        domain.KnowledgeRetrievalClaim,
			ID:          "c1",
			Tier:        domain.KnowledgeRankTierFused,
			Reasons:     []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonLexical},
			LexicalRank: 2,
		},
		domain.KnowledgeRankCandidate{
			Kind:        domain.KnowledgeRetrievalClaim,
			ID:          "c2",
			Tier:        domain.KnowledgeRankTierFused,
			Reasons:     []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonLexical},
			LexicalRank: 1,
		},
	)
	if len(ranked) != 2 || ranked[0].ID != "c2" || ranked[1].ID != "c1" {
		t.Fatalf("fused order = %+v", ranked)
	}
	if ranked[0].FusedScore != wantRank1 || ranked[1].FusedScore != wantRank2 {
		t.Fatalf("fused scores = %d/%d, want %d/%d", ranked[0].FusedScore, ranked[1].FusedScore, wantRank1, wantRank2)
	}
	// Repeating the identical input must reproduce the identical order.
	again := rank(
		t,
		domain.KnowledgeRankCandidate{
			Kind:        domain.KnowledgeRetrievalClaim,
			ID:          "c1",
			Tier:        domain.KnowledgeRankTierFused,
			Reasons:     []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonLexical},
			LexicalRank: 2,
		},
		domain.KnowledgeRankCandidate{
			Kind:        domain.KnowledgeRetrievalClaim,
			ID:          "c2",
			Tier:        domain.KnowledgeRankTierFused,
			Reasons:     []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonLexical},
			LexicalRank: 1,
		},
	)
	if len(again) != 2 || again[0].ID != "c2" || again[0].FusedScore != wantRank1 || again[1].FusedScore != wantRank2 {
		t.Fatalf("repeated fusion order diverged = %+v", again)
	}
}

func TestKnowledgeRankingDedupesByIdentityAndMergesReasons(t *testing.T) {
	ranked := rank(
		t,
		domain.KnowledgeRankCandidate{
			Kind:    domain.KnowledgeRetrievalClaim,
			ID:      "c1",
			Tier:    domain.KnowledgeRankTierExact,
			Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonExactSubject},
		},
		domain.KnowledgeRankCandidate{
			Kind:         domain.KnowledgeRetrievalClaim,
			ID:           "c1",
			Tier:         domain.KnowledgeRankTierFused,
			Reasons:      []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonSemantic, domain.KnowledgeRetrievalReasonLexical},
			SemanticRank: 3,
			LexicalRank:  5,
		},
		domain.KnowledgeRankCandidate{
			Kind:    domain.KnowledgeRetrievalClaim,
			ID:      "c2",
			Tier:    domain.KnowledgeRankTierExact,
			Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonExactIdentifier},
		},
	)
	if len(ranked) != 2 {
		t.Fatalf("deduped count = %d, want 2", len(ranked))
	}
	if ranked[0].ID != "c1" || ranked[0].Tier != domain.KnowledgeRankTierExact {
		t.Fatalf("deduped identity = %+v", ranked[0])
	}
	want := []string{"exact_subject", "lexical", "semantic"}
	if len(ranked[0].Reasons) != len(want) {
		t.Fatalf("merged reasons = %v, want sorted closed set %v", ranked[0].Reasons, want)
	}
	for i := range want {
		if string(ranked[0].Reasons[i]) != want[i] {
			t.Fatalf("merged reasons = %v, want sorted closed set %v", ranked[0].Reasons, want)
		}
	}
	// Duplicate reasons must not repeat.
	dup := rank(
		t,
		domain.KnowledgeRankCandidate{
			Kind:    domain.KnowledgeRetrievalClaim,
			ID:      "c1",
			Tier:    domain.KnowledgeRankTierExact,
			Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonExactSubject},
		},
		domain.KnowledgeRankCandidate{
			Kind:    domain.KnowledgeRetrievalClaim,
			ID:      "c1",
			Tier:    domain.KnowledgeRankTierExact,
			Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonExactSubject},
		},
	)
	if len(dup) != 1 || len(dup[0].Reasons) != 1 {
		t.Fatalf("duplicate reason merge = %+v", dup)
	}
}

func TestKnowledgeRankingTiesUseKindThenIdentity(t *testing.T) {
	ranked := rank(
		t,
		domain.KnowledgeRankCandidate{
			Kind:    domain.KnowledgeRetrievalDocument,
			ID:      "b",
			Tier:    domain.KnowledgeRankTierExact,
			Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonExactSubject},
		},
		domain.KnowledgeRankCandidate{
			Kind:    domain.KnowledgeRetrievalClaim,
			ID:      "z",
			Tier:    domain.KnowledgeRankTierExact,
			Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonExactSubject},
		},
		domain.KnowledgeRankCandidate{
			Kind:    domain.KnowledgeRetrievalClaim,
			ID:      "a",
			Tier:    domain.KnowledgeRankTierExact,
			Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonExactSubject},
		},
		domain.KnowledgeRankCandidate{
			Kind:    domain.KnowledgeRetrievalPreference,
			ID:      "m",
			Tier:    domain.KnowledgeRankTierExact,
			Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonExactSubject},
		},
	)
	if len(ranked) != 4 {
		t.Fatalf("tie count = %d", len(ranked))
	}
	want := []struct {
		kind domain.KnowledgeRetrievalItemKind
		id   string
	}{
		{domain.KnowledgeRetrievalClaim, "a"},
		{domain.KnowledgeRetrievalClaim, "z"},
		{domain.KnowledgeRetrievalPreference, "m"},
		{domain.KnowledgeRetrievalDocument, "b"},
	}
	for i, w := range want {
		if ranked[i].Kind != w.kind || ranked[i].ID != w.id {
			t.Errorf("tie order[%d] = %s/%s, want %s/%s", i, ranked[i].Kind, ranked[i].ID, w.kind, w.id)
		}
	}
	if capped := domain.CapRankedKnowledgeCandidates(ranked, 2); len(capped) != 2 || capped[0].ID != "a" {
		t.Fatalf("capped list = %+v", capped)
	}
	// A non-positive cap selects nothing instead of admitting the full set.
	if capped := domain.CapRankedKnowledgeCandidates(ranked, 0); capped != nil {
		t.Fatalf("zero cap admitted %d candidates", len(capped))
	}
	if capped := domain.CapRankedKnowledgeCandidates(ranked, -1); capped != nil {
		t.Fatalf("negative cap admitted %d candidates", len(capped))
	}
}

func TestKnowledgeRankingUsesBestChannelRankPerIdentity(t *testing.T) {
	ranked := rank(
		t,
		domain.KnowledgeRankCandidate{
			Kind:        domain.KnowledgeRetrievalClaim,
			ID:          "c1",
			Tier:        domain.KnowledgeRankTierFused,
			Reasons:     []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonLexical},
			LexicalRank: 5,
		},
		domain.KnowledgeRankCandidate{
			Kind:        domain.KnowledgeRetrievalClaim,
			ID:          "c1",
			Tier:        domain.KnowledgeRankTierFused,
			Reasons:     []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonLexical},
			LexicalRank: 2,
		},
	)
	if len(ranked) != 1 || ranked[0].FusedScore != 1_000_000/62 {
		t.Fatalf("best-rank fusion = %+v, want score %d", ranked, 1_000_000/62)
	}
}

func TestKnowledgeRankingRejectsMalformedCandidates(t *testing.T) {
	malformed := []struct {
		name      string
		candidate domain.KnowledgeRankCandidate
	}{
		{"unknown kind", domain.KnowledgeRankCandidate{Kind: "invented", ID: "x", Tier: domain.KnowledgeRankTierExact}},
		{"empty identity", domain.KnowledgeRankCandidate{Kind: domain.KnowledgeRetrievalClaim, ID: "", Tier: domain.KnowledgeRankTierExact}},
		{"unbounded identity", domain.KnowledgeRankCandidate{Kind: domain.KnowledgeRetrievalClaim, ID: strings.Repeat("x", 300), Tier: domain.KnowledgeRankTierExact}},
		{"unknown tier", domain.KnowledgeRankCandidate{Kind: domain.KnowledgeRetrievalClaim, ID: "x", Tier: domain.KnowledgeRankTier(7)}},
		{"unknown reason", domain.KnowledgeRankCandidate{Kind: domain.KnowledgeRetrievalClaim, ID: "x", Tier: domain.KnowledgeRankTierExact, Reasons: []domain.KnowledgeRetrievalReason{"invented"}}},
		{"negative lexical rank", domain.KnowledgeRankCandidate{Kind: domain.KnowledgeRetrievalClaim, ID: "x", Tier: domain.KnowledgeRankTierFused, LexicalRank: -1}},
		{"negative semantic rank", domain.KnowledgeRankCandidate{Kind: domain.KnowledgeRetrievalClaim, ID: "x", Tier: domain.KnowledgeRankTierFused, SemanticRank: -1}},
		{
			"rank over hard max",
			domain.KnowledgeRankCandidate{Kind: domain.KnowledgeRetrievalClaim, ID: "x", Tier: domain.KnowledgeRankTierFused, LexicalRank: domain.HardMaxKnowledgeRetrievalRank + 1},
		},
		{"exact with channel rank", domain.KnowledgeRankCandidate{Kind: domain.KnowledgeRetrievalClaim, ID: "x", Tier: domain.KnowledgeRankTierExact, LexicalRank: 1}},
		{
			"exact with lexical reason",
			domain.KnowledgeRankCandidate{
				Kind:    domain.KnowledgeRetrievalClaim,
				ID:      "x",
				Tier:    domain.KnowledgeRankTierExact,
				Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonLexical},
			},
		},
		{"relation with channel rank", domain.KnowledgeRankCandidate{Kind: domain.KnowledgeRetrievalClaim, ID: "x", Tier: domain.KnowledgeRankTierRelation, SemanticRank: 2}},
		{
			"relation with exact reason",
			domain.KnowledgeRankCandidate{
				Kind:    domain.KnowledgeRetrievalClaim,
				ID:      "x",
				Tier:    domain.KnowledgeRankTierRelation,
				Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonExactSubject},
			},
		},
		{
			"fused without ranks",
			domain.KnowledgeRankCandidate{
				Kind:    domain.KnowledgeRetrievalClaim,
				ID:      "x",
				Tier:    domain.KnowledgeRankTierFused,
				Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonLexical},
			},
		},
		{
			"fused with relation reason",
			domain.KnowledgeRankCandidate{
				Kind:        domain.KnowledgeRetrievalClaim,
				ID:          "x",
				Tier:        domain.KnowledgeRankTierFused,
				Reasons:     []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonRelation},
				LexicalRank: 1,
			},
		},
		{"exact without reasons", domain.KnowledgeRankCandidate{Kind: domain.KnowledgeRetrievalClaim, ID: "x", Tier: domain.KnowledgeRankTierExact}},
		{"relation without reasons", domain.KnowledgeRankCandidate{Kind: domain.KnowledgeRetrievalClaim, ID: "x", Tier: domain.KnowledgeRankTierRelation}},
		{"fused without reasons", domain.KnowledgeRankCandidate{Kind: domain.KnowledgeRetrievalClaim, ID: "x", Tier: domain.KnowledgeRankTierFused, LexicalRank: 1}},
		{
			"fused lexical rank without lexical reason",
			domain.KnowledgeRankCandidate{
				Kind:         domain.KnowledgeRetrievalClaim,
				ID:           "x",
				Tier:         domain.KnowledgeRankTierFused,
				Reasons:      []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonSemantic},
				LexicalRank:  1,
				SemanticRank: 2,
			},
		},
		{
			"fused semantic rank without semantic reason",
			domain.KnowledgeRankCandidate{
				Kind:         domain.KnowledgeRetrievalClaim,
				ID:           "x",
				Tier:         domain.KnowledgeRankTierFused,
				Reasons:      []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonLexical},
				LexicalRank:  1,
				SemanticRank: 2,
			},
		},
		{
			"fused lexical reason without rank",
			domain.KnowledgeRankCandidate{
				Kind:         domain.KnowledgeRetrievalClaim,
				ID:           "x",
				Tier:         domain.KnowledgeRankTierFused,
				Reasons:      []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonLexical},
				SemanticRank: 2,
			},
		},
		{
			"fused semantic reason without rank",
			domain.KnowledgeRankCandidate{
				Kind:        domain.KnowledgeRetrievalClaim,
				ID:          "x",
				Tier:        domain.KnowledgeRankTierFused,
				Reasons:     []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonSemantic},
				LexicalRank: 2,
			},
		},
	}
	for _, candidate := range malformed {
		if ranked, err := domain.RankKnowledgeCandidates([]domain.KnowledgeRankCandidate{candidate.candidate}); err == nil || ranked != nil {
			t.Errorf("%s: malformed candidate entered the ranking (%v)", candidate.name, err)
		}
	}
}

func TestKnowledgeRankingIsPermutationIndependent(t *testing.T) {
	base := []domain.KnowledgeRankCandidate{
		{Kind: domain.KnowledgeRetrievalClaim, ID: "b", Tier: domain.KnowledgeRankTierFused, Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonLexical}, LexicalRank: 2},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "a", Tier: domain.KnowledgeRankTierExact, Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonExactSubject}},
		{Kind: domain.KnowledgeRetrievalClaim, ID: "a", Tier: domain.KnowledgeRankTierFused, Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonSemantic}, SemanticRank: 1},
		{Kind: domain.KnowledgeRetrievalPreference, ID: "p", Tier: domain.KnowledgeRankTierRelation, Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonRelation}},
	}
	permuted := []domain.KnowledgeRankCandidate{base[3], base[1], base[0], base[2]}
	first := rank(t, base...)
	second := rank(t, permuted...)
	if len(first) != len(second) {
		t.Fatalf("permutation changed cardinality: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Kind != second[i].Kind || first[i].ID != second[i].ID ||
			first[i].FusedScore != second[i].FusedScore ||
			strings.Join(reasonStrings(first[i].Reasons), ",") != strings.Join(reasonStrings(second[i].Reasons), ",") {
			t.Fatalf("permutation diverged at %d: %+v vs %+v", i, first[i], second[i])
		}
	}
}

func reasonStrings(reasons []domain.KnowledgeRetrievalReason) []string {
	out := make([]string, len(reasons))
	for i, reason := range reasons {
		out[i] = string(reason)
	}
	return out
}

func TestKnowledgeRetrievalDiagnosticsAreContentFreeBoundedAndClosed(t *testing.T) {
	valid := domain.KnowledgeRetrievalDiagnostics{
		RankingPolicy:           domain.KnowledgeRankingPolicyID,
		IndexFingerprintVersion: domain.KnowledgeVectorEncodingVersion,
		EnabledChannels:         []domain.KnowledgeRetrievalChannel{domain.KnowledgeRetrievalChannelExact, domain.KnowledgeRetrievalChannelLexical},
		SelectedIdentities:      []string{"claim:a", "document:b"},
		CandidateCount:          3,
		SelectedCount:           2,
		OmittedCount:            1,
		Failures:                []domain.KnowledgeRetrievalFailure{domain.KnowledgeRetrievalLexicalUnavailable},
		Omissions:               []domain.KnowledgeRetrievalOmission{domain.KnowledgeRetrievalOmissionDocumentOverLimit},
		Elapsed:                 time.Millisecond,
	}
	if err := domain.ValidateKnowledgeRetrievalDiagnostics(valid); err != nil {
		t.Fatalf("valid diagnostics rejected: %v", err)
	}
	rejected := []struct {
		name   string
		mutate func(*domain.KnowledgeRetrievalDiagnostics)
	}{
		{"empty policy", func(d *domain.KnowledgeRetrievalDiagnostics) { d.RankingPolicy = "" }},
		{"foreign policy", func(d *domain.KnowledgeRetrievalDiagnostics) { d.RankingPolicy = "rank-v99" }},
		{"foreign fingerprint", func(d *domain.KnowledgeRetrievalDiagnostics) { d.IndexFingerprintVersion = "l2-f32le-v2" }},
		{"unknown failure", func(d *domain.KnowledgeRetrievalDiagnostics) {
			d.Failures = []domain.KnowledgeRetrievalFailure{"invented"}
		}},
		{"duplicate failures", func(d *domain.KnowledgeRetrievalDiagnostics) {
			d.Failures = []domain.KnowledgeRetrievalFailure{domain.KnowledgeRetrievalLexicalUnavailable, domain.KnowledgeRetrievalLexicalUnavailable}
		}},
		{"unsorted failures", func(d *domain.KnowledgeRetrievalDiagnostics) {
			d.Failures = []domain.KnowledgeRetrievalFailure{domain.KnowledgeRetrievalSemanticUnavailable, domain.KnowledgeRetrievalLexicalUnavailable}
		}},
		{"unknown omission", func(d *domain.KnowledgeRetrievalDiagnostics) {
			d.Omissions = []domain.KnowledgeRetrievalOmission{"invented"}
		}},
		{"unknown channel", func(d *domain.KnowledgeRetrievalDiagnostics) {
			d.EnabledChannels = []domain.KnowledgeRetrievalChannel{"invented"}
		}},
		{"unsorted channels", func(d *domain.KnowledgeRetrievalDiagnostics) {
			d.EnabledChannels = []domain.KnowledgeRetrievalChannel{domain.KnowledgeRetrievalChannelLexical, domain.KnowledgeRetrievalChannelExact}
		}},
		{"negative count", func(d *domain.KnowledgeRetrievalDiagnostics) { d.SelectedCount = -1 }},
		{"selection exceeds candidates", func(d *domain.KnowledgeRetrievalDiagnostics) { d.SelectedCount = 4 }},
		{"selected count differs from identities", func(d *domain.KnowledgeRetrievalDiagnostics) { d.SelectedCount = 1 }},
		{"selected count over card cap", func(d *domain.KnowledgeRetrievalDiagnostics) {
			d.SelectedIdentities = make([]string, domain.HardMaxKnowledgeRetrievalMaxCards+1)
			for i := range d.SelectedIdentities {
				d.SelectedIdentities[i] = strings.Repeat("a", i+1)
			}
			d.SelectedCount = len(d.SelectedIdentities)
			d.CandidateCount = len(d.SelectedIdentities)
		}},
		{"candidate count over channel cap", func(d *domain.KnowledgeRetrievalDiagnostics) {
			d.CandidateCount = domain.HardMaxKnowledgeRetrievalCandidates + 1
		}},
		{"negative elapsed", func(d *domain.KnowledgeRetrievalDiagnostics) { d.Elapsed = -time.Second }},
		{"unsorted identities", func(d *domain.KnowledgeRetrievalDiagnostics) { d.SelectedIdentities = []string{"b", "a"} }},
		{"duplicate identities", func(d *domain.KnowledgeRetrievalDiagnostics) { d.SelectedIdentities = []string{"a", "a"} }},
		{"empty identity", func(d *domain.KnowledgeRetrievalDiagnostics) { d.SelectedIdentities = []string{""} }},
		{"identity overflow", func(d *domain.KnowledgeRetrievalDiagnostics) {
			d.SelectedIdentities = make([]string, domain.HardMaxKnowledgeRetrievalMaxCards+1)
			for i := range d.SelectedIdentities {
				d.SelectedIdentities[i] = "i" + strings.Repeat("a", i)
			}
		}},
	}
	for _, candidate := range rejected {
		diagnostics := valid
		candidate.mutate(&diagnostics)
		if err := domain.ValidateKnowledgeRetrievalDiagnostics(diagnostics); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
			t.Errorf("%s error = %v, want ErrKnowledgeInvalidValue", candidate.name, err)
		}
	}
}

func TestKnowledgeRetrievalReasonSetIsClosed(t *testing.T) {
	for _, reason := range []string{"exact_subject", "exact_identifier", "relation", "lexical", "semantic"} {
		if err := domain.ValidateKnowledgeRetrievalReason(reason); err != nil {
			t.Errorf("reason %s rejected: %v", reason, err)
		}
	}
	if err := domain.ValidateKnowledgeRetrievalReason("invented"); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
		t.Fatalf("unknown reason error = %v", err)
	}
}

func TestKnowledgeQueueItemAndClaimContracts(t *testing.T) {
	now := time.Now().UTC()
	valid := domain.KnowledgeQueueItem{
		Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_001", Status: domain.KnowledgeQueuePending,
		Generation: 3, Attempts: 1, NextAttempt: now.Add(time.Minute), LeaseUntil: now.Add(time.Minute),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid queue item rejected: %v", err)
	}
	rejected := []struct {
		name   string
		mutate func(*domain.KnowledgeQueueItem)
	}{
		{"unknown kind", func(i *domain.KnowledgeQueueItem) { i.Kind = "invented" }},
		{"empty identity", func(i *domain.KnowledgeQueueItem) { i.ID = "" }},
		{"unbounded identity", func(i *domain.KnowledgeQueueItem) { i.ID = strings.Repeat("x", 300) }},
		{"unknown status", func(i *domain.KnowledgeQueueItem) { i.Status = "invented" }},
		{"negative generation", func(i *domain.KnowledgeQueueItem) { i.Generation = -1 }},
		{"negative attempts", func(i *domain.KnowledgeQueueItem) { i.Attempts = -1 }},
		{"missing created", func(i *domain.KnowledgeQueueItem) { i.CreatedAt = time.Time{} }},
		{"missing updated", func(i *domain.KnowledgeQueueItem) { i.UpdatedAt = time.Time{} }},
	}
	for _, candidate := range rejected {
		item := valid
		candidate.mutate(&item)
		if err := item.Validate(); !errors.Is(err, domain.ErrKnowledgeInvalidValue) {
			t.Errorf("%s error = %v, want ErrKnowledgeInvalidValue", candidate.name, err)
		}
	}
	for _, code := range []domain.KnowledgeQueueFailureCode{"source_invalid", "provider_invalid", "attempts_exhausted"} {
		if !domain.ValidKnowledgeQueueFailureCode(code) {
			t.Errorf("queue failure code %s rejected", code)
		}
	}
	if domain.ValidKnowledgeQueueFailureCode("invented") {
		t.Error("unknown queue failure code accepted")
	}
	claim := domain.KnowledgeQueueClaim{Kind: domain.KnowledgeRetrievalClaim, ID: "kclaim_001", Generation: 3, LeaseUntil: now.Add(time.Minute)}
	if claim == (domain.KnowledgeQueueClaim{}) {
		t.Fatal("claim token cannot be empty")
	}
}

func TestKnowledgeMetricLabelContractsAreClosed(t *testing.T) {
	for _, channel := range []domain.KnowledgeRetrievalChannel{"exact", "relation", "lexical", "semantic"} {
		if !domain.ValidKnowledgeRetrievalChannel(channel) {
			t.Errorf("channel %s rejected", channel)
		}
	}
	if domain.ValidKnowledgeRetrievalChannel("invented") {
		t.Error("unknown channel accepted")
	}
	for _, outcome := range []string{"success", "empty", "validation_rejected", "unavailable"} {
		if !domain.ValidKnowledgeRetrievalOutcome(outcome) {
			t.Errorf("outcome %s rejected", outcome)
		}
	}
	if domain.ValidKnowledgeRetrievalOutcome("invented") {
		t.Error("unknown outcome accepted")
	}
	for _, reason := range []string{"relation_unavailable", "lexical_unavailable", "semantic_unavailable", "counter_unavailable", "stale_index", "oversized"} {
		if !domain.ValidKnowledgeRetrievalReasonLabel(reason) {
			t.Errorf("reason %s rejected", reason)
		}
	}
	if domain.ValidKnowledgeRetrievalReasonLabel("invented") {
		t.Error("unknown reason accepted")
	}
}

func TestKnowledgeMetricLabelCombinationsAreFrozen(t *testing.T) {
	valid := []struct {
		metric string
		labels map[string]string
	}{
		{domain.MetricKnowledgeRetrievalTotal, map[string]string{domain.MetricLabelOutcome: "success"}},
		{domain.MetricKnowledgeRetrievalDuration, map[string]string{domain.MetricLabelOutcome: "empty"}},
		{domain.MetricKnowledgeRetrievalCandidates, map[string]string{domain.MetricLabelChannel: "exact"}},
		{domain.MetricKnowledgeRetrievalSelected, map[string]string{domain.MetricLabelChannel: "relation"}},
		{domain.MetricKnowledgeRetrievalEmptyTotal, map[string]string{domain.MetricLabelOutcome: "empty"}},
		{domain.MetricKnowledgeRetrievalChannelFailure, map[string]string{domain.MetricLabelChannel: "lexical", domain.MetricLabelReason: "lexical_unavailable"}},
		{domain.MetricKnowledgeRetrievalChannelFailure, map[string]string{domain.MetricLabelChannel: "relation", domain.MetricLabelReason: "relation_unavailable"}},
		{domain.MetricKnowledgeRetrievalChannelFailure, map[string]string{domain.MetricLabelChannel: "semantic", domain.MetricLabelReason: "semantic_unavailable"}},
		{domain.MetricKnowledgeRetrievalStaleIndex, map[string]string{domain.MetricLabelChannel: "lexical", domain.MetricLabelReason: "stale_index"}},
		{domain.MetricKnowledgeRetrievalStaleIndex, map[string]string{domain.MetricLabelChannel: "semantic", domain.MetricLabelReason: "stale_index"}},
		{domain.MetricKnowledgeRetrievalCardTokens, map[string]string{}},
		{domain.MetricKnowledgeLexicalQueueDepth, map[string]string{}},
		{domain.MetricKnowledgeEmbeddingQueueDepth, map[string]string{}},
		{domain.MetricKnowledgeEmbeddingRequestDuration, map[string]string{domain.MetricLabelOutcome: "success"}},
	}
	for _, candidate := range valid {
		if err := domain.ValidateKnowledgeMetricLabels(candidate.metric, candidate.labels); err != nil {
			t.Errorf("metric %s labels %v rejected: %v", candidate.metric, candidate.labels, err)
		}
	}
	rejected := []struct {
		name   string
		metric string
		labels map[string]string
	}{
		{"unknown metric", "knowledge_invented_total", nil},
		{"foreign label key", domain.MetricKnowledgeRetrievalTotal, map[string]string{"actor": "U1"}},
		{"unknown channel value", domain.MetricKnowledgeRetrievalCandidates, map[string]string{domain.MetricLabelChannel: "invented"}},
		{"unknown outcome value", domain.MetricKnowledgeRetrievalTotal, map[string]string{domain.MetricLabelOutcome: "invented"}},
		{"unknown reason value", domain.MetricKnowledgeRetrievalChannelFailure, map[string]string{domain.MetricLabelChannel: "lexical", domain.MetricLabelReason: "invented"}},
		{"outcome on channel metric", domain.MetricKnowledgeRetrievalCandidates, map[string]string{domain.MetricLabelOutcome: "success"}},
		{"channel on unlabeled metric", domain.MetricKnowledgeRetrievalCardTokens, map[string]string{domain.MetricLabelChannel: "exact"}},
		{"total without outcome", domain.MetricKnowledgeRetrievalTotal, map[string]string{}},
		{"candidates without channel", domain.MetricKnowledgeRetrievalCandidates, map[string]string{}},
		{"failure without reason", domain.MetricKnowledgeRetrievalChannelFailure, map[string]string{domain.MetricLabelChannel: "lexical"}},
		{"failure with stale reason", domain.MetricKnowledgeRetrievalChannelFailure, map[string]string{domain.MetricLabelChannel: "lexical", domain.MetricLabelReason: "stale_index"}},
		{
			"failure with wrong channel for reason",
			domain.MetricKnowledgeRetrievalChannelFailure,
			map[string]string{domain.MetricLabelChannel: "lexical", domain.MetricLabelReason: "semantic_unavailable"},
		},
		{
			"failure relation channel with lexical reason",
			domain.MetricKnowledgeRetrievalChannelFailure,
			map[string]string{domain.MetricLabelChannel: "relation", domain.MetricLabelReason: "lexical_unavailable"},
		},
		{
			"failure semantic channel with relation reason",
			domain.MetricKnowledgeRetrievalChannelFailure,
			map[string]string{domain.MetricLabelChannel: "semantic", domain.MetricLabelReason: "relation_unavailable"},
		},
		{
			"failure relation channel with counter reason",
			domain.MetricKnowledgeRetrievalChannelFailure,
			map[string]string{domain.MetricLabelChannel: "relation", domain.MetricLabelReason: "counter_unavailable"},
		},
		{
			"failure lexical channel with counter reason",
			domain.MetricKnowledgeRetrievalChannelFailure,
			map[string]string{domain.MetricLabelChannel: "lexical", domain.MetricLabelReason: "counter_unavailable"},
		},
		{
			"failure semantic channel with counter reason",
			domain.MetricKnowledgeRetrievalChannelFailure,
			map[string]string{domain.MetricLabelChannel: "semantic", domain.MetricLabelReason: "counter_unavailable"},
		},
		{"stale index without stale reason", domain.MetricKnowledgeRetrievalStaleIndex, map[string]string{domain.MetricLabelChannel: "lexical", domain.MetricLabelReason: "lexical_unavailable"}},
		{"stale index without channel", domain.MetricKnowledgeRetrievalStaleIndex, map[string]string{domain.MetricLabelReason: "stale_index"}},
		{"stale index on exact channel", domain.MetricKnowledgeRetrievalStaleIndex, map[string]string{domain.MetricLabelChannel: "exact", domain.MetricLabelReason: "stale_index"}},
		{"stale index on relation channel", domain.MetricKnowledgeRetrievalStaleIndex, map[string]string{domain.MetricLabelChannel: "relation", domain.MetricLabelReason: "stale_index"}},
		{"empty total without outcome", domain.MetricKnowledgeRetrievalEmptyTotal, map[string]string{}},
		{"empty total with success outcome", domain.MetricKnowledgeRetrievalEmptyTotal, map[string]string{domain.MetricLabelOutcome: "success"}},
		{"empty total with unavailable outcome", domain.MetricKnowledgeRetrievalEmptyTotal, map[string]string{domain.MetricLabelOutcome: "unavailable"}},
	}
	for _, candidate := range rejected {
		if err := domain.ValidateKnowledgeMetricLabels(candidate.metric, candidate.labels); err == nil {
			t.Errorf("%s: invalid label combination accepted", candidate.name)
		}
	}
}
