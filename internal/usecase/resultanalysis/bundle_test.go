package resultanalysis_test

import (
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

func bundlePacketFixture() domain.AnalysisPacket {
	return domain.AnalysisPacket{
		ObjectiveClass:  domain.AnalysisObjectiveBoundedQuestionV1,
		ObjectiveDigest: domain.AnalysisObjectiveDigest(domain.AnalysisObjectiveBoundedQuestionV1, "objective"),
		Findings:        []domain.AnalysisStatement{{Text: "finding one"}},
		SourceSHA256:    strings.Repeat("a", 64),
	}
}

func bundleExcerptFixture(ordinal int, evidenceID string, bytes int) port.AnalysisEvidenceExcerpt {
	return port.AnalysisEvidenceExcerpt{
		EvidenceID:     evidenceID,
		SegmentOrdinal: ordinal,
		Range:          domain.AnalysisByteRange{OffsetBytes: 0, LengthBytes: int64(bytes)},
		SHA256:         strings.Repeat("b", 64),
		Excerpt:        strings.Repeat("x", bytes),
	}
}

// TestBundleBuilderBuildsWithinBounds is the baseline: a bundle within every
// configured bound builds successfully with a deterministic, non-empty
// selector.
func TestBundleBuilderBuildsWithinBounds(t *testing.T) {
	limits := testLimits()
	evidence := []port.AnalysisEvidenceExcerpt{bundleExcerptFixture(0, strings.Repeat("1", 64), 100)}
	bundle, err := (resultanalysis.BundleBuilder{}).Build(t.Context(), strings.Repeat("c", 64), bundlePacketFixture(), evidence, limits)
	if err != nil {
		t.Fatalf("build within bounds: %v", err)
	}
	if bundle.BundleID == "" || bundle.SHA256 == "" {
		t.Fatalf("expected a non-empty bundle id and digest, got %+v", bundle)
	}
	if bundle.Bytes <= 0 {
		t.Fatalf("expected positive bundle bytes, got %d", bundle.Bytes)
	}

	// Determinism: the same inputs produce the same selector and digest.
	bundle2, err := (resultanalysis.BundleBuilder{}).Build(t.Context(), strings.Repeat("c", 64), bundlePacketFixture(), evidence, limits)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.BundleID != bundle2.BundleID || bundle.SHA256 != bundle2.SHA256 {
		t.Fatalf("expected deterministic bundle id and digest, got %q/%q vs %q/%q", bundle.BundleID, bundle.SHA256, bundle2.BundleID, bundle2.SHA256)
	}
}

// TestBundleBuilderRejectsExcerptOverExcerptBound is criterion 6, bound 1:
// one evidence excerpt over the configured excerpt-bytes bound is a typed
// rejection, never a silent truncation of the excerpt.
func TestBundleBuilderRejectsExcerptOverExcerptBound(t *testing.T) {
	limits := testLimits()
	oversized := bundleExcerptFixture(0, strings.Repeat("1", 64), limits.EvidenceExcerptBytes+1)
	bundle, err := (resultanalysis.BundleBuilder{}).Build(t.Context(), strings.Repeat("c", 64), bundlePacketFixture(), []port.AnalysisEvidenceExcerpt{oversized}, limits)
	if !errIsAnalysisValidation(err) {
		t.Fatalf("expected an oversized excerpt to be rejected typed, got bundle=%+v err=%v", bundle, err)
	}
	if len(bundle.Evidence) != 0 {
		t.Fatal("expected no bundle content on rejection, never a truncated one")
	}
}

// TestBundleBuilderRejectsTooManySelectorsPerLeaf is criterion 6, bound 2:
// more evidence excerpts than the configured per-leaf selector bound, all
// citing the same segment (leaf), is a typed rejection.
func TestBundleBuilderRejectsTooManySelectorsPerLeaf(t *testing.T) {
	limits := testLimits()
	var evidence []port.AnalysisEvidenceExcerpt
	for i := 0; i < limits.EvidenceSelectorsPerLeaf+1; i++ {
		evidence = append(evidence, bundleExcerptFixture(0, strings.Repeat(string(rune('a'+i)), 64), 10))
	}
	_, err := (resultanalysis.BundleBuilder{}).Build(t.Context(), strings.Repeat("c", 64), bundlePacketFixture(), evidence, limits)
	if !errIsAnalysisValidation(err) {
		t.Fatalf("expected too many per-leaf selectors to be rejected typed, got %v", err)
	}
}

// TestBundleBuilderRejectsTooManyEvidenceReferences is criterion 6, bound 3:
// more evidence excerpts than the configured per-packet reference bound,
// spread across distinct segments so the per-leaf bound is never the
// trigger, is a typed rejection.
func TestBundleBuilderRejectsTooManyEvidenceReferences(t *testing.T) {
	limits := testLimits()
	var evidence []port.AnalysisEvidenceExcerpt
	for i := 0; i < limits.EvidenceReferencesPerPacket+1; i++ {
		evidence = append(evidence, bundleExcerptFixture(i, strings.Repeat(string(rune('a'+(i%26))), 64), 10))
	}
	_, err := (resultanalysis.BundleBuilder{}).Build(t.Context(), strings.Repeat("c", 64), bundlePacketFixture(), evidence, limits)
	if !errIsAnalysisValidation(err) {
		t.Fatalf("expected too many evidence references to be rejected typed, got %v", err)
	}
}

// TestBundleBuilderRejectsOverBundleBytes is criterion 6, bound 4: a bundle
// whose serialized size exceeds the configured byte bound is a typed
// rejection, never a truncated payload.
func TestBundleBuilderRejectsOverBundleBytes(t *testing.T) {
	limits := testLimits()
	limits.BundleBytes = 200
	limits.EvidenceExcerptBytes = 500
	evidence := []port.AnalysisEvidenceExcerpt{bundleExcerptFixture(0, strings.Repeat("1", 64), 400)}
	_, err := (resultanalysis.BundleBuilder{}).Build(t.Context(), strings.Repeat("c", 64), bundlePacketFixture(), evidence, limits)
	if !errIsAnalysisValidation(err) {
		t.Fatalf("expected an oversized bundle to be rejected typed, got %v", err)
	}
}
