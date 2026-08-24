package integration_test

// Checkpoint 6 sub-round 6b: the embedding evaluation gate. This file proves
// the TRD enablement condition against the frozen hard-paraphrase subset:
// an FTS-only baseline measurably below 1.0, an embedding-assisted run at
// least +0.10 absolute recall@8 above it, and zero regression on every 6a
// mandatory gate when embeddings are enabled. The only embedding path
// exercised is the deterministic in-memory
// internal/testutil.FakeEmbeddingProvider — no live credential, endpoint, or
// real OpenAI-compatible provider exists anywhere in this round.

import (
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/testutil"
)

const knowledgeEvalHardParaphraseFixture = "testdata/knowledge-retrieval/hard-paraphrase-v1.json"

func hardParaphraseCases(dataset evalDataset) []evalCase {
	var cases []evalCase
	for _, testCase := range dataset.Cases {
		if testCase.Subset == "hard-paraphrase" {
			cases = append(cases, testCase)
		}
	}
	return cases
}

// hardParaphraseRecall runs the hard-paraphrase subset and computes the
// recall@8 arithmetic over every expected identity.
func hardParaphraseRecall(t *testing.T, harness *knowledgeEvalHarness, dataset evalDataset, retriever port.KnowledgeRetriever) (found, expected int) {
	t.Helper()
	for _, testCase := range hardParaphraseCases(dataset) {
		identities, _, _ := harness.runCaseOn(t, testCase, retriever)
		caseFound, caseExpected := recallAt8(identities, testCase.Want)
		found += caseFound
		expected += caseExpected
	}
	return found, expected
}

// TestKnowledgeRetrievalHardParaphraseFTSOnlyBaseline pins the lexical-only
// baseline on the hard-paraphrase subset: the queries are semantically
// equivalent but lexically disjoint, so FTS alone must find (almost) none of
// the targets. The baseline number is the denominator of the 6b delta.
func TestKnowledgeRetrievalHardParaphraseFTSOnlyBaseline(t *testing.T) {
	dataset := loadKnowledgeEvalDatasetFile(t, knowledgeEvalHardParaphraseFixture)
	harness := newKnowledgeEvalHarness(t, dataset)
	found, expected := hardParaphraseRecall(t, harness, dataset, harness.retriever)
	if expected == 0 {
		t.Fatal("no hard-paraphrase cases ran")
	}
	baseline := float64(found) / float64(expected)
	if baseline >= 0.10 {
		t.Fatalf("hard-paraphrase FTS-only baseline recall@8 = %.4f, want below 0.10 (lexically disjoint fixtures)", baseline)
	}
	t.Logf("hard-paraphrase FTS-only baseline recall@8 = %d/%d (%.4f)", found, expected, baseline)
}

// TestKnowledgeRetrievalEmbeddingParaphraseRecallGate pins the TRD
// enablement condition: semantic-assisted recall@8 on the same frozen
// subset must beat the FTS-only baseline by at least +0.10 absolute. The
// fake provider returns hand-constructed unit vectors per case (aligned
// with the target row, orthogonal to everything else), so the lift is a
// real similarity measurement, never an input-hash artifact.
func TestKnowledgeRetrievalEmbeddingParaphraseRecallGate(t *testing.T) {
	dataset := loadKnowledgeEvalDatasetFile(t, knowledgeEvalHardParaphraseFixture)
	lexical := newKnowledgeEvalHarness(t, dataset)
	baselineFound, expected := hardParaphraseRecall(t, lexical, dataset, lexical.retriever)
	if expected == 0 {
		t.Fatal("no hard-paraphrase cases ran")
	}
	baseline := float64(baselineFound) / float64(expected)

	embedding := newKnowledgeEvalHarnessWithFingerprint(t, dataset, knowledgeEvalFingerprint(), nil)
	found := 0
	for _, testCase := range hardParaphraseCases(dataset) {
		// One fake provider per case: SetVectors pins the query vector the
		// retriever's semantic channel receives, aligned with the target
		// row seeded under the same fingerprint.
		provider := testutil.NewFakeEmbeddingProvider(knowledgeEvalEmbeddingDims)
		provider.SetVectors([][]float32{testCase.QueryVector})
		retriever := embedding.withProvider(provider)
		identities, reasons, cards := embedding.runCaseOn(t, testCase, retriever)
		caseFound, _ := recallAt8(identities, testCase.Want)
		found += caseFound
		if caseFound == 0 {
			t.Fatalf("case %s: semantic channel found no expected identity (cards %v reasons %v)", testCase.Name, identities, reasons)
		}
		if len(cards) > 0 && reasons[0] != "semantic" {
			t.Fatalf("case %s: first reason = %q, want semantic", testCase.Name, reasons[0])
		}
	}
	assisted := float64(found) / float64(expected)
	delta := assisted - baseline
	if delta < 0.10 {
		t.Fatalf("embedding lift = %.4f (%.4f - %.4f), want at least +0.10 absolute paraphrase recall@8", delta, assisted, baseline)
	}
	t.Logf("hard-paraphrase semantic-assisted recall@8 = %d/%d (%.4f)", found, expected, assisted)
	t.Logf("embedding lift over FTS-only baseline = +%.4f absolute (>= 0.10 required)", delta)
}

// TestKnowledgeRetrievalEmbeddingNoRegressionOnMandatoryGates reruns every
// 6a mandatory-gate case through the embedding-enabled configuration (fake
// provider with a constant vector, fingerprint-bound index, no seeded
// vector rows) and requires byte-identical identity and reason output to
// the lexical-only baseline: enabling embeddings must never change an
// already-passing lexical outcome.
func TestKnowledgeRetrievalEmbeddingNoRegressionOnMandatoryGates(t *testing.T) {
	dataset := loadKnowledgeEvalDataset(t)
	lexical := newKnowledgeEvalHarness(t, dataset)
	embedding := newKnowledgeEvalHarnessWithFingerprint(t, dataset, knowledgeEvalFingerprint(), nil)
	// The 6a dataset carries no vector rows, so the fake provider's output
	// can never match anything: the semantic channel contributes nothing
	// and the lexical results must stay identical.
	provider := testutil.NewFakeEmbeddingProvider(knowledgeEvalEmbeddingDims)
	provider.SetVectors([][]float32{{1, 0, 0, 0, 0, 0, 0, 0}})
	retriever := embedding.withProvider(provider)
	for _, testCase := range dataset.Cases {
		baseIDs, baseReasons, _ := lexical.runCase(t, testCase)
		embIDs, embReasons, _ := embedding.runCaseOn(t, testCase, retriever)
		if !equalStringSlice(baseIDs, embIDs) || !equalStringSlice(baseReasons, embReasons) {
			t.Fatalf("case %s: embedding-enabled run differs from lexical baseline\nbaseline: %v / %v\nembedded: %v / %v",
				testCase.Name, baseIDs, baseReasons, embIDs, embReasons)
		}
	}
	// The frozen 6a gate arithmetic is reproduced through the
	// embedding-enabled configuration for the exact and lexical subsets.
	found, expected := 0, 0
	for _, testCase := range dataset.Cases {
		if (testCase.Gate == "exact" || testCase.Gate == "lexical-subset") && len(testCase.Want) > 0 {
			identities, _, _ := embedding.runCaseOn(t, testCase, retriever)
			caseFound, caseExpected := recallAt8(identities, testCase.Want)
			found += caseFound
			expected += caseExpected
		}
	}
	if expected == 0 || found != expected {
		t.Fatalf("embedding-enabled recall on mandatory subsets = %d/%d, want no regression (1.00)", found, expected)
	}
	t.Logf("embedding-enabled recall on mandatory subsets = %d/%d (no regression)", found, expected)
}

// TestKnowledgeRetrievalEmbeddingFingerprintBindsTheSemanticSurface proves
// the fingerprint used by the gate is the frozen ModelFingerprint of the
// pinned fake configuration, so the seeded rows and the bound store can
// never diverge accidentally.
func TestKnowledgeRetrievalEmbeddingFingerprintBindsTheSemanticSurface(t *testing.T) {
	fingerprint := knowledgeEvalFingerprint()
	if fingerprint != domain.ModelFingerprint(knowledgeEvalEmbeddingProvider, knowledgeEvalEmbeddingModel, knowledgeEvalEmbeddingDims) {
		t.Fatalf("eval fingerprint %q is not the frozen provider fingerprint", fingerprint)
	}
	if fingerprint == "" {
		t.Fatal("eval fingerprint is empty; semantic search would fail closed")
	}
}
