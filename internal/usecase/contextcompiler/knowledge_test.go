package contextcompiler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// findWithLabels locates a sample with the exact metric name and labels.
func (m *compilerMetricCapture) findWithLabels(name string, labels port.MetricLabels) (port.MetricSample, bool) {
	for _, sample := range m.samples {
		if sample.Name != name || !equalMetricLabels(sample.Labels, labels) {
			continue
		}
		return sample, true
	}
	return port.MetricSample{}, false
}

func equalMetricLabels(left, right port.MetricLabels) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

// knowledgeDeltaFrameCounter counts only the rendered knowledge block: every
// part whose text is the [KNOWLEDGE DATA] block contributes its code points
// and everything else contributes zero, so the source delta equals the
// exact rendered block cost including the preamble and separators. When
// failKnowledge is set the source counter errors on frames carrying the
// block while final admission frames (without it) keep counting.
type knowledgeDeltaFrameCounter struct {
	calls         int
	err           error
	failKnowledge bool
}

func (c *knowledgeDeltaFrameCounter) CountContextFrame(_ context.Context, contents []domain.Content) (port.TokenCount, error) {
	c.calls++
	hasBlock := false
	tokens := 0
	for _, content := range contents {
		for _, part := range content.Parts {
			if strings.HasPrefix(part.Text, "[KNOWLEDGE DATA]") {
				hasBlock = true
				tokens += utf8.RuneCountInString(part.Text)
			}
		}
	}
	if hasBlock && c.failKnowledge {
		return port.TokenCount{}, c.err
	}
	if !c.failKnowledge && c.err != nil {
		return port.TokenCount{}, c.err
	}
	return port.TokenCount{Tokens: tokens, Strategy: "provider"}, nil
}

func knowledgeFrameCard(id, subject, reason string, content string) domain.KnowledgeFrameCard {
	return domain.KnowledgeFrameCard{
		Kind: domain.KnowledgeRetrievalClaim,
		Claim: &domain.KnowledgeCard{
			ClaimID:         domain.KnowledgeClaimID(id),
			Subject:         subject,
			Predicate:       "is",
			Value:           domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "value"},
			ScopeKind:       domain.KnowledgeScopeProject,
			ScopeID:         "workspace",
			SourceClass:     domain.KnowledgeSourceHuman,
			SourceRef:       "slack:T12345678:dm:D12345678:1710000000.000001",
			Status:          domain.KnowledgeClaimAsserted,
			ValidFrom:       testValidFrom(),
			RetrievalReason: reason,
		},
	}
}

func testValidFrom() (t time.Time) { return }

func knowledgeContentsFixture() []domain.Content {
	return []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "old request"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "old answer"}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "current request"}}},
	}
}

func TestKnowledgeFitsWholeCardsCumulativelyWithPreambleAndSeparators(t *testing.T) {
	cardOne := knowledgeFrameCard("kclaim_000000000000000000000001", "subject one", "exact_subject", "")
	cardTwo := knowledgeFrameCard("kclaim_000000000000000000000002", "subject two", "relation", "")
	cards := []domain.KnowledgeFrameCard{cardOne, cardTwo}
	// The budget is the exact rendered cost of both cards including the
	// shared preamble and the separator: an equal budget selects both and a
	// budget one code point lower selects only the first.
	full := domain.RenderKnowledgeFrameCards(cards)
	exactBudget := utf8.RuneCountInString(full)
	contents := knowledgeContentsFixture()
	counter := &knowledgeDeltaFrameCounter{}
	compiler := New(newFakeResultStore(), fakeTokenCounter{})
	result, err := compiler.CompileFrame(t.Context(), domain.CompileRequest{
		Contents: contents, ModelBudget: domain.RequestBudget{HardTokens: 10_000, TargetTokens: 10_000},
		Knowledge: cards, KnowledgeBudgetTokens: exactBudget,
	}, counter)
	if err != nil {
		t.Fatal(err)
	}
	if result.KnowledgeSelectedCount != 2 || result.KnowledgeOmittedCount != 0 {
		t.Fatalf("selected=%d omitted=%d, want 2/0", result.KnowledgeSelectedCount, result.KnowledgeOmittedCount)
	}
	wantIdentities := []string{"claim:kclaim_000000000000000000000001", "claim:kclaim_000000000000000000000002"}
	if len(result.KnowledgeIdentities) != 2 || result.KnowledgeIdentities[0] != wantIdentities[0] || result.KnowledgeIdentities[1] != wantIdentities[1] {
		t.Fatalf("identities = %#v", result.KnowledgeIdentities)
	}
	text, found := knowledgeBlockText(result.Contents)
	if !found || text != full {
		t.Fatalf("rendered block mismatch: found=%t", found)
	}
	role, blockIndex, _ := knowledgeBlockRole(result.Contents)
	if role != domain.ContentRoleUser {
		t.Fatalf("knowledge block role = %q, want user (never a model or assistant message)", role)
	}
	// The block is exactly one additional part of the current user content:
	// the same content carries the current user message and the block, and
	// no extra content was created.
	content := result.Contents[blockIndex]
	if len(content.Parts) != 2 {
		t.Fatalf("current user content parts = %d, want the message plus exactly one knowledge part", len(content.Parts))
	}
	if content.Parts[0].Text != "current request" {
		t.Fatalf("current user content = %#v, want the original message preserved", content.Parts[0])
	}
	if content.Parts[1].Text != full {
		t.Fatalf("knowledge part = %q, want the exact rendered block", content.Parts[1].Text)
	}
	// The active turn still contains exactly the original contents: the
	// block added no new content.
	if len(result.Contents) != len(knowledgeContentsFixture()) {
		t.Fatalf("contents = %d, want the original count with the block as a part", len(result.Contents))
	}

	lower := New(newFakeResultStore(), fakeTokenCounter{})
	reduced, err := lower.CompileFrame(t.Context(), domain.CompileRequest{
		Contents: contents, ModelBudget: domain.RequestBudget{HardTokens: 10_000, TargetTokens: 10_000},
		Knowledge: cards, KnowledgeBudgetTokens: exactBudget - 1,
	}, &knowledgeDeltaFrameCounter{})
	if err != nil {
		t.Fatal(err)
	}
	if reduced.KnowledgeSelectedCount != 1 || reduced.KnowledgeOmittedCount != 1 {
		t.Fatalf("reduced selected=%d omitted=%d, want 1/1", reduced.KnowledgeSelectedCount, reduced.KnowledgeOmittedCount)
	}
	if len(reduced.KnowledgeIdentities) != 1 || reduced.KnowledgeIdentities[0] != wantIdentities[0] {
		t.Fatalf("reduced identities = %#v", reduced.KnowledgeIdentities)
	}
}

func TestKnowledgeOversizedCardIsOmittedWholeAndLaterSmallerCardFits(t *testing.T) {
	huge := knowledgeFrameCard("kclaim_00000000000000000000000a", "huge subject", "exact_subject", "")
	small := knowledgeFrameCard("kclaim_00000000000000000000000b", "small subject", "relation", "")
	smallCost := utf8.RuneCountInString(domain.RenderKnowledgeFrameCards([]domain.KnowledgeFrameCard{small}))
	hugeCost := utf8.RuneCountInString(domain.RenderKnowledgeFrameCards([]domain.KnowledgeFrameCard{huge}))
	if hugeCost <= smallCost {
		t.Fatal("fixture requires the first card to cost more than the second")
	}
	cards := []domain.KnowledgeFrameCard{huge, small}
	result, err := New(newFakeResultStore(), fakeTokenCounter{}).CompileFrame(t.Context(), domain.CompileRequest{
		Contents: knowledgeContentsFixture(), ModelBudget: domain.RequestBudget{HardTokens: 10_000, TargetTokens: 10_000},
		Knowledge: cards, KnowledgeBudgetTokens: smallCost,
	}, &knowledgeDeltaFrameCounter{})
	if err != nil {
		t.Fatal(err)
	}
	if result.KnowledgeSelectedCount != 1 || result.KnowledgeOmittedCount != 1 {
		t.Fatalf("selected=%d omitted=%d, want the huge card omitted whole and the small card selected", result.KnowledgeSelectedCount, result.KnowledgeOmittedCount)
	}
	if len(result.KnowledgeIdentities) != 1 || result.KnowledgeIdentities[0] != "claim:kclaim_00000000000000000000000b" {
		t.Fatalf("identities = %#v", result.KnowledgeIdentities)
	}
	text, found := knowledgeBlockText(result.Contents)
	if !found || !strings.Contains(text, "small subject") || strings.Contains(text, "huge subject") {
		t.Fatalf("selected block = %q, want only the small card", text)
	}
}

func TestKnowledgeCounterFailureOmitsAllCardsAndContinues(t *testing.T) {
	cards := []domain.KnowledgeFrameCard{knowledgeFrameCard("kclaim_00000000000000000000000c", "subject", "exact_subject", "")}
	// The source counter fails only for candidate frames carrying the
	// block; final admission frames (without the omitted block) keep
	// counting, which is what keeps the root request authoritative.
	counter := &knowledgeDeltaFrameCounter{failKnowledge: true, err: errors.New("counter exploded")}
	result, err := New(newFakeResultStore(), fakeTokenCounter{}).CompileFrame(t.Context(), domain.CompileRequest{
		Contents: knowledgeContentsFixture(), ModelBudget: domain.RequestBudget{HardTokens: 10_000, TargetTokens: 10_000},
		Knowledge: cards, KnowledgeBudgetTokens: 100,
	}, counter)
	if err != nil {
		t.Fatalf("CompileFrame() error = %v, want the root request to continue", err)
	}
	if result.KnowledgeSelectedCount != 0 || result.KnowledgeOmittedCount != 1 {
		t.Fatalf("selected=%d omitted=%d, want every card omitted", result.KnowledgeSelectedCount, result.KnowledgeOmittedCount)
	}
	if _, found := knowledgeBlockText(result.Contents); found {
		t.Fatal("knowledge block present after counter failure")
	}
}

func TestKnowledgeUsesConservativeCounterWithoutFrameCounter(t *testing.T) {
	card := knowledgeFrameCard("kclaim_00000000000000000000000d", "subject", "exact_subject", "")
	contents := knowledgeContentsFixture()
	block := knowledgeBlockContents([]domain.KnowledgeFrameCard{card})
	baseJSON, err := domain.CanonicalJSON(contents)
	if err != nil {
		t.Fatal(err)
	}
	withBlock := append([]domain.Content{contents[0]}, append(block, contents[1:]...)...)
	candidateJSON, err := domain.CanonicalJSON(withBlock)
	if err != nil {
		t.Fatal(err)
	}
	budget := len(candidateJSON) - len(baseJSON)
	compiler := New(newFakeResultStore(), serializedByteCounter{})
	result, err := compiler.Compile(t.Context(), domain.CompileRequest{
		Contents: contents, ModelBudget: domain.RequestBudget{HardTokens: 10_000, TargetTokens: 10_000},
		Knowledge: []domain.KnowledgeFrameCard{card}, KnowledgeBudgetTokens: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.KnowledgeSelectedCount != 1 || result.KnowledgeOmittedCount != 0 {
		t.Fatalf("selected=%d omitted=%d, want the conservative delta to select the card", result.KnowledgeSelectedCount, result.KnowledgeOmittedCount)
	}
}

func TestKnowledgeEvictionOrderPutsKnowledgeBeforeContinuityAndProtected(t *testing.T) {
	capsule := domain.ContinuityCapsule{Objective: &domain.ContinuityItem{ID: "objective", Kind: domain.ContinuityKindObjective, Text: "keep bounded", Status: domain.ContinuityStatusCurrent}}
	cards := []domain.KnowledgeFrameCard{knowledgeFrameCard("kclaim_00000000000000000000000e", "subject", "exact_subject", "")}
	base := domain.CompileRequest{
		Contents: knowledgeContentsFixture(), ExistingSummary: "summary",
		ModelBudget: domain.RequestBudget{HardTokens: 4000, TargetTokens: 4000},
		Continuity:  capsule, Knowledge: cards, KnowledgeBudgetTokens: 1000,
	}

	// First sequence: the conservative knowledge fitting consumes two counts
	// (base and one candidate, both zero), then summary, old completed
	// turns, and knowledge are evicted in that exact order while continuity
	// survives: knowledge goes before continuity under total pressure.
	t.Run("knowledge before continuity", func(t *testing.T) {
		counter := &sequenceTokenCounter{counts: []int{1, 1, 4001, 4001, 4001, 100}}
		result, err := New(newFakeResultStore(), counter).Compile(t.Context(), base)
		if err != nil {
			t.Fatal(err)
		}
		if result.KnowledgeSelectedCount != 0 || result.KnowledgeOmittedCount != 1 {
			t.Fatalf("selected=%d omitted=%d, want the block evicted whole", result.KnowledgeSelectedCount, result.KnowledgeOmittedCount)
		}
		if _, found := knowledgeBlockText(result.Contents); found {
			t.Fatal("knowledge block survived eviction")
		}
		if !continuityPresent(result.Contents) {
			t.Fatal("continuity was evicted before knowledge")
		}
		if len(result.KnowledgeIdentities) != 0 {
			t.Fatalf("identities = %#v, want empty after eviction", result.KnowledgeIdentities)
		}
		// Whole-block eviction restores the original user content: the
		// current user message keeps its single original part.
		for _, content := range result.Contents {
			for _, part := range content.Parts {
				if part.Text == "current request" && len(content.Parts) != 1 {
					t.Fatalf("eviction did not restore the original user content: %#v", content)
				}
			}
		}
	})

	// Second sequence: the summary alone reaches the target, so recent
	// turns, knowledge, and continuity all survive.
	t.Run("summary evicted first", func(t *testing.T) {
		counter := &sequenceTokenCounter{counts: []int{1, 1, 4001, 100}}
		result, err := New(newFakeResultStore(), counter).Compile(t.Context(), base)
		if err != nil {
			t.Fatal(err)
		}
		if _, found := knowledgeBlockText(result.Contents); !found {
			t.Fatal("knowledge block was evicted before old completed turns")
		}
		if result.KnowledgeSelectedCount != 1 {
			t.Fatalf("selected=%d, want the knowledge block retained", result.KnowledgeSelectedCount)
		}
		if !continuityPresent(result.Contents) {
			t.Fatal("continuity was evicted in the summary phase")
		}
	})

	// Third sequence: pressure survives every optional phase and the final
	// count exceeds the hard limit; the irreducible error must arrive with
	// knowledge already gone, never because of it.
	t.Run("knowledge never makes request irreducible", func(t *testing.T) {
		counter := &sequenceTokenCounter{counts: []int{1, 1, 4001, 4001, 4001, 4001, 4001, 4001}}
		result, err := New(newFakeResultStore(), counter).Compile(t.Context(), base)
		if !errors.Is(err, domain.ErrIrreducibleContext) {
			t.Fatalf("error = %v, want irreducible", err)
		}
		if result.KnowledgeSelectedCount != 0 || result.KnowledgeOmittedCount != 1 {
			t.Fatalf("selected=%d omitted=%d, want knowledge evicted before the irreducible conclusion", result.KnowledgeSelectedCount, result.KnowledgeOmittedCount)
		}
		if _, found := knowledgeBlockText(result.Contents); found {
			t.Fatal("knowledge block present in the irreducible result")
		}
	})
}

func TestKnowledgeInvalidCardIsOmittedBeforeSelection(t *testing.T) {
	valid := knowledgeFrameCard("kclaim_00000000000000000000000f", "valid subject", "exact_subject", "")
	invalid := valid
	invalid.Claim = &domain.KnowledgeCard{} // no payload fields: invalid
	cards := []domain.KnowledgeFrameCard{valid, invalid}
	result, err := New(newFakeResultStore(), fakeTokenCounter{}).CompileFrame(t.Context(), domain.CompileRequest{
		Contents: knowledgeContentsFixture(), ModelBudget: domain.RequestBudget{HardTokens: 10_000, TargetTokens: 10_000},
		Knowledge: cards, KnowledgeBudgetTokens: 10_000,
	}, &knowledgeDeltaFrameCounter{})
	if err != nil {
		t.Fatal(err)
	}
	if result.KnowledgeSelectedCount != 1 || result.KnowledgeOmittedCount != 1 {
		t.Fatalf("selected=%d omitted=%d, want the invalid card omitted whole", result.KnowledgeSelectedCount, result.KnowledgeOmittedCount)
	}
	if len(result.KnowledgeIdentities) != 1 || result.KnowledgeIdentities[0] != "claim:kclaim_00000000000000000000000f" {
		t.Fatalf("identities = %#v", result.KnowledgeIdentities)
	}
}

func TestKnowledgeNegativeBudgetRejectsCompile(t *testing.T) {
	cards := []domain.KnowledgeFrameCard{knowledgeFrameCard("kclaim_000000000000000000000010", "subject", "exact_subject", "")}
	_, err := New(newFakeResultStore(), fakeTokenCounter{}).Compile(t.Context(), domain.CompileRequest{
		Contents: knowledgeContentsFixture(), ModelBudget: domain.RequestBudget{HardTokens: 1000, TargetTokens: 1000},
		Knowledge: cards, KnowledgeBudgetTokens: -1,
	})
	if err == nil {
		t.Fatal("Compile() with a negative knowledge budget succeeded")
	}
}

func TestKnowledgeEmptyRequestOmitsEveryCandidate(t *testing.T) {
	cards := []domain.KnowledgeFrameCard{knowledgeFrameCard("kclaim_000000000000000000000011", "subject", "exact_subject", "")}
	result, err := New(newFakeResultStore(), fakeTokenCounter{}).Compile(t.Context(), domain.CompileRequest{
		Contents: nil, ModelBudget: domain.RequestBudget{HardTokens: 1000, TargetTokens: 1000},
		Knowledge: cards, KnowledgeBudgetTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.KnowledgeSelectedCount != 0 || result.KnowledgeOmittedCount != 1 {
		t.Fatalf("selected=%d omitted=%d, want no turn to attach the block", result.KnowledgeSelectedCount, result.KnowledgeOmittedCount)
	}
}

func TestKnowledgeSelectionMetricsUseFinalSelection(t *testing.T) {
	cards := []domain.KnowledgeFrameCard{
		knowledgeFrameCard("kclaim_000000000000000000000012", "subject exact", "exact_subject", ""),
		knowledgeFrameCard("kclaim_000000000000000000000013", "subject relation", "relation", ""),
	}
	fullCost := utf8.RuneCountInString(domain.RenderKnowledgeFrameCards(cards))
	recorder := &compilerMetricCapture{}
	result, err := New(newFakeResultStore(), fakeTokenCounter{}, recorder).CompileFrame(t.Context(), domain.CompileRequest{
		Contents: knowledgeContentsFixture(), ModelBudget: domain.RequestBudget{HardTokens: 10_000, TargetTokens: 10_000},
		Knowledge: cards, KnowledgeBudgetTokens: fullCost,
	}, &knowledgeDeltaFrameCounter{})
	if err != nil {
		t.Fatal(err)
	}
	if result.KnowledgeSelectedCount != 2 {
		t.Fatalf("selected=%d", result.KnowledgeSelectedCount)
	}
	exact, ok := recorder.findWithLabels(domain.MetricKnowledgeRetrievalSelected, port.MetricLabels{domain.MetricLabelChannel: string(domain.KnowledgeRetrievalChannelExact)})
	if !ok || exact.Value != 1 {
		t.Fatalf("exact selected metric = %#v found=%t", exact, ok)
	}
	relation, ok := recorder.findWithLabels(domain.MetricKnowledgeRetrievalSelected, port.MetricLabels{domain.MetricLabelChannel: string(domain.KnowledgeRetrievalChannelRelation)})
	if !ok || relation.Value != 1 {
		t.Fatalf("relation selected metric = %#v found=%t", relation, ok)
	}
	tokens, ok := recorder.find(domain.MetricKnowledgeRetrievalCardTokens)
	if !ok || tokens.Value != float64(fullCost) {
		t.Fatalf("card tokens metric = %#v found=%t want %d", tokens, ok, fullCost)
	}
}

// weightedKnowledgeFrameCounter assigns a fixed authoritative weight to
// each candidate document by its identity, independent of its rendered
// rune size. This decouples the local rune-based planning model inside
// FitKnowledgeFrameCards from the authoritative cost, which is what forces
// the correction phase to test more than one candidate of the same final
// selection length: exactly the condition FIND-120 needs to expose a
// length-keyed cost map.
type weightedKnowledgeFrameCounter struct {
	weights map[string]int
}

func (c *weightedKnowledgeFrameCounter) CountContextFrame(_ context.Context, contents []domain.Content) (port.TokenCount, error) {
	total := 0
	for _, content := range contents {
		for _, part := range content.Parts {
			if !strings.HasPrefix(part.Text, "[KNOWLEDGE DATA]") {
				continue
			}
			for id, weight := range c.weights {
				if strings.Contains(part.Text, id) {
					total += weight
				}
			}
		}
	}
	return port.TokenCount{Tokens: total, Strategy: "provider"}, nil
}

// weightedDocumentCard builds a valid document card of the given word count,
// identified by id, for weightedKnowledgeFrameCounter to key its weight on.
func weightedDocumentCard(id string, words int) domain.KnowledgeFrameCard {
	var b strings.Builder
	for w := 0; w < words; w++ {
		fmt.Fprintf(&b, "word%d ", w)
	}
	doc := domain.KnowledgeDocumentCard{
		ID: id, Subject: "subject " + id,
		ScopeKind: domain.KnowledgeScopeGlobal, Provenance: domain.KnowledgeProvenanceCurated,
		Status: domain.KnowledgeDocumentActive, Content: strings.TrimSpace(b.String()), RetrievalReason: "lexical",
	}
	return domain.KnowledgeFrameCard{Kind: domain.KnowledgeRetrievalDocument, Document: &doc}
}

// TestKnowledgeFinalMetricMatchesExactSelectionNotLength covers FIND-120: a
// length-keyed cost map records whichever selection of a given card count
// was costed last, which is not necessarily the selection FitKnowledgeFrameCards
// finally returns. With three candidate documents, a budget of 16, and
// weights 20/8/39 keyed by identity, correction tests a lone card of weight
// 8 (fits) and then a lone card of weight 20 (does not fit) before settling
// on the weight-8 card: both attempts have length 1, so a map keyed by
// length alone would report 20, not the real 8, for the final selection.
func TestKnowledgeFinalMetricMatchesExactSelectionNotLength(t *testing.T) {
	fits := "kdoc_" + strings.Repeat("1", 24)
	rejectedSameLength := "kdoc_" + strings.Repeat("0", 24)
	tooExpensive := "kdoc_" + strings.Repeat("2", 24)
	cards := []domain.KnowledgeFrameCard{
		weightedDocumentCard(rejectedSameLength, 111),
		weightedDocumentCard(fits, 79),
		weightedDocumentCard(tooExpensive, 211),
	}
	counter := &weightedKnowledgeFrameCounter{weights: map[string]int{rejectedSameLength: 20, fits: 8, tooExpensive: 39}}
	recorder := &compilerMetricCapture{}
	result, err := New(newFakeResultStore(), fakeTokenCounter{}, recorder).CompileFrame(t.Context(), domain.CompileRequest{
		Contents: knowledgeContentsFixture(), ModelBudget: domain.RequestBudget{HardTokens: 10_000, TargetTokens: 10_000},
		Knowledge: cards, KnowledgeBudgetTokens: 16,
	}, counter)
	if err != nil {
		t.Fatal(err)
	}
	if result.KnowledgeSelectedCount != 1 {
		t.Fatalf("selected=%d, want exactly the weight-8 document", result.KnowledgeSelectedCount)
	}
	wantIdentity := "document:" + fits
	if len(result.KnowledgeIdentities) != 1 || result.KnowledgeIdentities[0] != wantIdentity {
		t.Fatalf("identities = %#v, want [%q]", result.KnowledgeIdentities, wantIdentity)
	}
	tokens, ok := recorder.find(domain.MetricKnowledgeRetrievalCardTokens)
	if !ok {
		t.Fatal("card tokens metric not recorded")
	}
	if tokens.Value != 8 {
		t.Fatalf("card tokens metric = %v, want 8 (the exact returned selection's cost, not 20 from the rejected same-length candidate)", tokens.Value)
	}
}

// knowledgeBlockText extracts the single attributed [KNOWLEDGE DATA] block
// from the model-facing contents along with its role and position, so tests
// pin that the block is user role inside the current user turn, never a
// model or assistant message.
func knowledgeBlockText(contents []domain.Content) (string, bool) {
	for _, content := range contents {
		for _, part := range content.Parts {
			if strings.HasPrefix(part.Text, "[KNOWLEDGE DATA]") {
				return part.Text, true
			}
		}
	}
	return "", false
}

// knowledgeBlockPosition returns the role of the block content and the index
// of the current user message content it must follow.
func knowledgeBlockRole(contents []domain.Content) (domain.ContentRole, int, bool) {
	for index, content := range contents {
		for _, part := range content.Parts {
			if strings.HasPrefix(part.Text, "[KNOWLEDGE DATA]") {
				return content.Role, index, true
			}
		}
	}
	return "", -1, false
}

func continuityPresent(contents []domain.Content) bool {
	for _, content := range contents {
		for _, part := range content.Parts {
			if strings.Contains(part.Text, "[UNTRUSTED CONTINUITY REFERENCE]") {
				return true
			}
		}
	}
	return false
}
