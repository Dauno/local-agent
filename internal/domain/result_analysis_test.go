package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func validAnalysisLimits() AnalysisLimits {
	return AnalysisLimits{
		MaxSegmentBytes:             24576,
		OverlapBasisPoints:          1000,
		OverlapMaxBytes:             4096,
		MaxLeaves:                   64,
		MaxReductionFanIn:           8,
		MaxReductionDepth:           4,
		MaxConcurrentLeaves:         2,
		MaxAttemptsPerStep:          2,
		CallTimeoutSeconds:          120,
		WallTimeSeconds:             900,
		EvidenceExcerptBytes:        2048,
		EvidenceSelectorsPerLeaf:    8,
		EvidenceReferencesPerPacket: 32,
		BundleBytes:                 32768,
	}
}

func TestAnalysisLimitsValidateAcceptsDefaultShape(t *testing.T) {
	if err := validAnalysisLimits().Validate(); err != nil {
		t.Fatalf("expected valid limits, got %v", err)
	}
}

func TestAnalysisLimitsValidateRejectsFieldsOutOfRange(t *testing.T) {
	base := validAnalysisLimits()
	cases := map[string]func(*AnalysisLimits){
		"max segment bytes zero":        func(l *AnalysisLimits) { l.MaxSegmentBytes = 0 },
		"max segment bytes over hard":   func(l *AnalysisLimits) { l.MaxSegmentBytes = HardMaxAnalysisSegmentBytes + 1 },
		"overlap basis points over":     func(l *AnalysisLimits) { l.OverlapBasisPoints = HardMaxAnalysisOverlapBasisPoints + 1 },
		"overlap max bytes zero":        func(l *AnalysisLimits) { l.OverlapMaxBytes = 0 },
		"max leaves over hard":          func(l *AnalysisLimits) { l.MaxLeaves = HardMaxAnalysisLeaves + 1 },
		"max fan-in zero":               func(l *AnalysisLimits) { l.MaxReductionFanIn = 0 },
		"max depth over hard":           func(l *AnalysisLimits) { l.MaxReductionDepth = HardMaxAnalysisReductionDepth + 1 },
		"max concurrent leaves zero":    func(l *AnalysisLimits) { l.MaxConcurrentLeaves = 0 },
		"max attempts over hard":        func(l *AnalysisLimits) { l.MaxAttemptsPerStep = HardMaxAnalysisAttemptsPerStep + 1 },
		"call timeout zero":             func(l *AnalysisLimits) { l.CallTimeoutSeconds = 0 },
		"wall time over hard":           func(l *AnalysisLimits) { l.WallTimeSeconds = HardMaxAnalysisWallTimeSeconds + 1 },
		"evidence excerpt bytes zero":   func(l *AnalysisLimits) { l.EvidenceExcerptBytes = 0 },
		"evidence per leaf over hard":   func(l *AnalysisLimits) { l.EvidenceSelectorsPerLeaf = HardMaxAnalysisEvidencePerLeaf + 1 },
		"evidence per packet over hard": func(l *AnalysisLimits) { l.EvidenceReferencesPerPacket = HardMaxAnalysisEvidencePerPacket + 1 },
		"bundle bytes zero":             func(l *AnalysisLimits) { l.BundleBytes = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			limits := base
			mutate(&limits)
			err := limits.Validate()
			if err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
			if !errors.Is(err, ErrAnalysisValidation) {
				t.Fatalf("expected ErrAnalysisValidation for %s, got %v", name, err)
			}
		})
	}
}

// TestAnalysisLimitsValidateRejectsUnsatisfiableReductionTree proves the
// criterion with concrete numbers: fan-in 2 to depth 2 covers at most 4
// leaves, so 5 configured leaves is an unsatisfiable combination.
func TestAnalysisLimitsValidateRejectsUnsatisfiableReductionTree(t *testing.T) {
	limits := validAnalysisLimits()
	limits.MaxReductionFanIn = 2
	limits.MaxReductionDepth = 2
	limits.MaxLeaves = 5
	err := limits.Validate()
	if err == nil {
		t.Fatal("expected an unsatisfiable reduction tree to fail validation")
	}
	if !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation, got %v", err)
	}

	limits.MaxLeaves = 4
	if err := limits.Validate(); err != nil {
		t.Fatalf("expected exactly-satisfiable tree (2^2=4) to validate, got %v", err)
	}
}

// TestAnalysisReductionTreeSatisfiableIsTheOneSharedRule proves FIND-097's
// acceptance criterion 4: AnalysisReductionTreeSatisfiable is exported
// precisely so internal/config's static config validation can call this
// exact function instead of reimplementing the fan-in/depth/leaves
// arithmetic. This test pins its behavior directly, and
// TestAnalysisLimitsValidateRejectsUnsatisfiableReductionTree above already
// proves AnalysisLimits.Validate rejects through this same function; the
// config-side regression is TestFIND097UnsatisfiableTreeRejectedAtConfigValidation
// in internal/config.
func TestAnalysisReductionTreeSatisfiableIsTheOneSharedRule(t *testing.T) {
	cases := []struct {
		fanIn, depth, leaves int
		want                 bool
	}{
		{fanIn: 2, depth: 2, leaves: 4, want: true},
		{fanIn: 2, depth: 2, leaves: 5, want: false},
		{fanIn: 2, depth: 2, leaves: 512, want: false},
		{fanIn: 16, depth: 4, leaves: 512, want: true},
	}
	for _, c := range cases {
		if got := AnalysisReductionTreeSatisfiable(c.fanIn, c.depth, c.leaves); got != c.want {
			t.Errorf("AnalysisReductionTreeSatisfiable(%d, %d, %d) = %v, want %v", c.fanIn, c.depth, c.leaves, got, c.want)
		}
	}
}

func TestAnalysisLimitsDigestDeterministic(t *testing.T) {
	limits := validAnalysisLimits()
	if limits.Digest() != limits.Digest() {
		t.Fatal("digest must be stable across repeated calls")
	}
	other := limits
	other.MaxLeaves++
	if limits.Digest() == other.Digest() {
		t.Fatal("digest must change when a field changes")
	}
}

func validAnalysisIdentity() AnalysisIdentity {
	return AnalysisIdentity{
		SourceResultID:      strings.Repeat("a", 64),
		SourceSHA256:        strings.Repeat("b", 64),
		ObjectiveClass:      AnalysisObjectiveBoundedQuestionV1,
		ObjectiveDigest:     strings.Repeat("c", 64),
		SegmentationVersion: "text_v1",
		PromptVersion:       "prompt_v1",
		ModelFingerprint:    "model:fingerprint:v1",
		LimitsDigest:        strings.Repeat("d", 64),
	}
}

func TestAnalysisIdentityValidate(t *testing.T) {
	if err := validAnalysisIdentity().Validate(); err != nil {
		t.Fatalf("expected valid identity, got %v", err)
	}
	bad := validAnalysisIdentity()
	bad.ObjectiveClass = "unknown_v1"
	if err := bad.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for unknown class, got %v", err)
	}
}

// TestAnalysisIdentitySemanticDigestChangesPerComponent proves criterion 2:
// changing any one of the eight semantic components changes the digest.
func TestAnalysisIdentitySemanticDigestChangesPerComponent(t *testing.T) {
	base := validAnalysisIdentity()
	baseDigest := base.SemanticDigest()

	mutators := map[string]func(*AnalysisIdentity){
		"source result id":     func(a *AnalysisIdentity) { a.SourceResultID = strings.Repeat("1", 64) },
		"source sha256":        func(a *AnalysisIdentity) { a.SourceSHA256 = strings.Repeat("2", 64) },
		"objective class":      func(a *AnalysisIdentity) { a.ObjectiveClass = AnalysisObjectiveClass("other_v1") },
		"objective digest":     func(a *AnalysisIdentity) { a.ObjectiveDigest = strings.Repeat("3", 64) },
		"segmentation version": func(a *AnalysisIdentity) { a.SegmentationVersion = "markdown_v1" },
		"prompt version":       func(a *AnalysisIdentity) { a.PromptVersion = "prompt_v2" },
		"model fingerprint":    func(a *AnalysisIdentity) { a.ModelFingerprint = "model:fingerprint:v2" },
		"limits digest":        func(a *AnalysisIdentity) { a.LimitsDigest = strings.Repeat("4", 64) },
	}
	for name, mutate := range mutators {
		t.Run(name, func(t *testing.T) {
			mutated := base
			mutate(&mutated)
			if mutated.SemanticDigest() == baseDigest {
				t.Fatalf("expected digest to change when %s changes", name)
			}
		})
	}

	// Two identities with equal components produce the same digest.
	again := validAnalysisIdentity()
	if again.SemanticDigest() != baseDigest {
		t.Fatal("expected equal identities to produce equal digests")
	}
}

func TestNormalizeAnalysisObjectivePreservesAngleBrackets(t *testing.T) {
	objective := "which configs use value < 10?"
	normalized, err := NormalizeAnalysisObjective(objective)
	if err != nil {
		t.Fatalf("expected valid objective, got %v", err)
	}
	if normalized != objective {
		t.Fatalf("expected the objective text unchanged, got %q", normalized)
	}
	digest1 := AnalysisObjectiveDigest(AnalysisObjectiveBoundedQuestionV1, normalized)
	digest2 := AnalysisObjectiveDigest(AnalysisObjectiveBoundedQuestionV1, objective)
	if digest1 != digest2 {
		t.Fatal("expected the digest of the sanitized text to match the digest of the original text")
	}
}

func TestNormalizeAnalysisObjectiveDiscardsControlsAndCanonicalizesWhitespace(t *testing.T) {
	objective := "which   configs\x00\x07 use\tvalue > 10 ?  "
	normalized, err := NormalizeAnalysisObjective(objective)
	if err != nil {
		t.Fatalf("expected valid objective, got %v", err)
	}
	if strings.ContainsAny(normalized, "\x00\x07") {
		t.Fatalf("expected control characters to be discarded, got %q", normalized)
	}
	if normalized != "which configs use value > 10 ?" {
		t.Fatalf("expected canonicalized whitespace, got %q", normalized)
	}
}

func TestNormalizeAnalysisObjectiveRejectsEmptyAndOversized(t *testing.T) {
	if _, err := NormalizeAnalysisObjective("   \n\t  "); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for empty objective, got %v", err)
	}
	if _, err := NormalizeAnalysisObjective(strings.Repeat("a", HardMaxAnalysisObjectiveRunes+1)); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for oversized objective, got %v", err)
	}
	if _, err := NormalizeAnalysisObjective(string([]byte{0xff, 0xfe})); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for invalid UTF-8, got %v", err)
	}
}

func hexDigest(fill byte) string {
	return strings.Repeat(string(fill), 64)
}

func TestAnalysisSegmentManifestValidateOrdinalsAndOffsets(t *testing.T) {
	manifest := AnalysisSegmentManifest{
		SourceSHA256: strings.Repeat("a", 64),
		SourceBytes:  300,
		Segments: []AnalysisSegment{
			{Ordinal: 0, OffsetBytes: 0, LengthBytes: 100, SHA256: strings.Repeat("b", 64), SegmenterVersion: "text_v1", OverlapPrevBytes: 0},
			{Ordinal: 1, OffsetBytes: 90, LengthBytes: 110, SHA256: strings.Repeat("c", 64), SegmenterVersion: "text_v1", OverlapPrevBytes: 10},
			{Ordinal: 2, OffsetBytes: 190, LengthBytes: 110, SHA256: strings.Repeat("d", 64), SegmenterVersion: "text_v1", OverlapPrevBytes: 10},
		},
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("expected valid manifest, got %v", err)
	}
}

func TestAnalysisSegmentManifestValidateRejectsBadOrdinals(t *testing.T) {
	manifest := AnalysisSegmentManifest{
		SourceSHA256: strings.Repeat("a", 64),
		SourceBytes:  200,
		Segments: []AnalysisSegment{
			{Ordinal: 1, OffsetBytes: 0, LengthBytes: 100, SHA256: strings.Repeat("b", 64), SegmenterVersion: "text_v1"},
		},
	}
	if err := manifest.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for bad ordinal, got %v", err)
	}
}

func TestAnalysisSegmentManifestValidateRejectsOutOfBoundsSegment(t *testing.T) {
	manifest := AnalysisSegmentManifest{
		SourceSHA256: strings.Repeat("a", 64),
		SourceBytes:  100,
		Segments: []AnalysisSegment{
			{Ordinal: 0, OffsetBytes: 0, LengthBytes: 150, SHA256: strings.Repeat("b", 64), SegmenterVersion: "text_v1"},
		},
	}
	if err := manifest.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for out-of-bounds segment, got %v", err)
	}
}

func TestAnalysisSegmentManifestValidateRejectsIncoherentOverlap(t *testing.T) {
	manifest := AnalysisSegmentManifest{
		SourceSHA256: strings.Repeat("a", 64),
		SourceBytes:  300,
		Segments: []AnalysisSegment{
			{Ordinal: 0, OffsetBytes: 0, LengthBytes: 100, SHA256: strings.Repeat("b", 64), SegmenterVersion: "text_v1"},
			{Ordinal: 1, OffsetBytes: 95, LengthBytes: 100, SHA256: strings.Repeat("c", 64), SegmenterVersion: "text_v1", OverlapPrevBytes: 10},
		},
	}
	if err := manifest.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for incoherent overlap, got %v", err)
	}
}

// TestAnalysisCoverageOverlapNeverDoubleCounted proves criterion 3: three
// 100-byte-nominal segments with 100 bytes of overlap between each pair
// cover the 300-byte source exactly once, not 500 bytes.
func TestAnalysisCoverageOverlapNeverDoubleCounted(t *testing.T) {
	manifest := AnalysisSegmentManifest{
		SourceSHA256: strings.Repeat("a", 64),
		SourceBytes:  300,
		Segments: []AnalysisSegment{
			{Ordinal: 0, OffsetBytes: 0, LengthBytes: 150, SHA256: strings.Repeat("b", 64), SegmenterVersion: "text_v1", OverlapPrevBytes: 0},
			{Ordinal: 1, OffsetBytes: 50, LengthBytes: 150, SHA256: strings.Repeat("c", 64), SegmenterVersion: "text_v1", OverlapPrevBytes: 100},
			{Ordinal: 2, OffsetBytes: 100, LengthBytes: 200, SHA256: strings.Repeat("d", 64), SegmenterVersion: "text_v1", OverlapPrevBytes: 100},
		},
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("expected valid manifest, got %v", err)
	}
	coverage := manifest.Coverage()
	if coverage.CoveredBytes != 300 {
		t.Fatalf("expected 300 covered bytes, got %d", coverage.CoveredBytes)
	}
	if !coverage.Complete {
		t.Fatalf("expected complete coverage, got gaps %+v", coverage.Gaps)
	}
	if len(coverage.Gaps) != 0 {
		t.Fatalf("expected no gaps, got %+v", coverage.Gaps)
	}
}

func TestAnalysisCoverageReportsGap(t *testing.T) {
	manifest := AnalysisSegmentManifest{
		SourceSHA256: strings.Repeat("a", 64),
		SourceBytes:  300,
		Segments: []AnalysisSegment{
			{Ordinal: 0, OffsetBytes: 0, LengthBytes: 100, SHA256: strings.Repeat("b", 64), SegmenterVersion: "text_v1", OverlapPrevBytes: 0},
			{Ordinal: 1, OffsetBytes: 200, LengthBytes: 100, SHA256: strings.Repeat("c", 64), SegmenterVersion: "text_v1", OverlapPrevBytes: 0},
		},
	}
	coverage := manifest.Coverage()
	if coverage.Complete {
		t.Fatal("expected incomplete coverage")
	}
	if coverage.CoveredBytes != 200 {
		t.Fatalf("expected 200 covered bytes, got %d", coverage.CoveredBytes)
	}
	if len(coverage.Gaps) != 1 || coverage.Gaps[0] != (AnalysisByteRange{OffsetBytes: 100, LengthBytes: 100}) {
		t.Fatalf("expected one 100-byte gap at offset 100, got %+v", coverage.Gaps)
	}
}

func validAnalysisLeaf() AnalysisLeaf {
	return AnalysisLeaf{
		ObjectiveClass:  AnalysisObjectiveBoundedQuestionV1,
		ObjectiveDigest: hexDigest('a'),
		SegmentOrdinal:  0,
		Findings:        []AnalysisStatement{{Text: "the config sets a timeout of 30s"}},
		EvidenceSelectors: []AnalysisByteRange{
			{OffsetBytes: 0, LengthBytes: 10},
		},
	}
}

func TestAnalysisLeafValidate(t *testing.T) {
	if err := validAnalysisLeaf().Validate(); err != nil {
		t.Fatalf("expected valid leaf, got %v", err)
	}
}

func TestAnalysisLeafValidateRejectsEmptyOutput(t *testing.T) {
	leaf := validAnalysisLeaf()
	leaf.Findings = nil
	if err := leaf.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for empty leaf output, got %v", err)
	}
}

func TestAnalysisLeafValidateRejectsUnknownObjectiveClass(t *testing.T) {
	leaf := validAnalysisLeaf()
	leaf.ObjectiveClass = "unknown_v1"
	if err := leaf.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for unknown class, got %v", err)
	}
}

func TestAnalysisLeafValidateRejectsOversizedStatement(t *testing.T) {
	leaf := validAnalysisLeaf()
	leaf.Findings = []AnalysisStatement{{Text: strings.Repeat("a", HardMaxAnalysisStatementRunes+1)}}
	if err := leaf.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for oversized statement, got %v", err)
	}
}

func TestAnalysisLeafValidateRejectsTooManyFindings(t *testing.T) {
	leaf := validAnalysisLeaf()
	findings := make([]AnalysisStatement, HardMaxAnalysisFindings+1)
	for i := range findings {
		findings[i] = AnalysisStatement{Text: "finding"}
	}
	leaf.Findings = findings
	if err := leaf.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for too many findings, got %v", err)
	}
}

func TestAnalysisLeafValidateRejectsInvalidSelector(t *testing.T) {
	leaf := validAnalysisLeaf()
	leaf.EvidenceSelectors = []AnalysisByteRange{{OffsetBytes: 0, LengthBytes: 0}}
	if err := leaf.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for zero-length selector, got %v", err)
	}
	leaf.EvidenceSelectors = []AnalysisByteRange{{OffsetBytes: -1, LengthBytes: 10}}
	if err := leaf.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for negative offset selector, got %v", err)
	}
}

func validAnalysisPacket() AnalysisPacket {
	return AnalysisPacket{
		ObjectiveClass:  AnalysisObjectiveBoundedQuestionV1,
		ObjectiveDigest: hexDigest('a'),
		Findings:        []AnalysisStatement{{Text: "the config sets a timeout of 30s"}},
		EvidenceRefs:    []string{"evidence-1"},
		SourceSHA256:    hexDigest('b'),
		Coverage:        AnalysisCoverage{CoveredBytes: 100, Complete: true},
		Lineage:         []string{"step-1", "step-2"},
	}
}

func TestAnalysisPacketValidate(t *testing.T) {
	if err := validAnalysisPacket().Validate(); err != nil {
		t.Fatalf("expected valid packet, got %v", err)
	}
}

func TestAnalysisPacketValidateRejectsEmptyOutput(t *testing.T) {
	packet := validAnalysisPacket()
	packet.Findings = nil
	if err := packet.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for empty packet output, got %v", err)
	}
}

func TestAnalysisPacketValidateRejectsUnknownClass(t *testing.T) {
	packet := validAnalysisPacket()
	packet.ObjectiveClass = "unknown_v1"
	if err := packet.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for unknown class, got %v", err)
	}
}

func TestAnalysisPacketValidateRejectsInvalidSourceDigest(t *testing.T) {
	packet := validAnalysisPacket()
	packet.SourceSHA256 = "not-a-digest"
	if err := packet.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for invalid source digest, got %v", err)
	}
}

func TestValidAnalysisFailureCode(t *testing.T) {
	codes := []AnalysisFailureCode{
		AnalysisFailureSourceTooLarge, AnalysisFailureSegmentInvalid, AnalysisFailureLeafSchemaInvalid,
		AnalysisFailureEvidenceSelectorInvalid, AnalysisFailureReductionIrreducible, AnalysisFailureCoverageIncomplete,
		AnalysisFailureBundleTooLarge, AnalysisFailureWallTimeExceeded, AnalysisFailureAttemptsExhausted,
		AnalysisFailureIdentityStale,
	}
	for _, code := range codes {
		if !ValidAnalysisFailureCode(string(code)) {
			t.Fatalf("expected %q to be a valid failure code", code)
		}
	}
	if ValidAnalysisFailureCode("not_a_real_code") {
		t.Fatal("expected an unknown failure code to be rejected")
	}
}

func TestAnalysisStepClaimValidate(t *testing.T) {
	claim := AnalysisStepClaim{
		AnalysisID: strings.Repeat("a", 64),
		StepID:     "step-1",
		Generation: 1,
		LeaseUntil: time.Now().Add(time.Minute),
	}
	if err := claim.Validate(); err != nil {
		t.Fatalf("expected valid claim, got %v", err)
	}
	claim.AnalysisID = "not-an-id"
	if err := claim.Validate(); !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for invalid analysis id, got %v", err)
	}
}

func TestAnalysisObjectiveClassValidation(t *testing.T) {
	if !ValidAnalysisObjectiveClass(string(AnalysisObjectiveBoundedQuestionV1)) {
		t.Fatal("expected bounded_question_v1 to be valid")
	}
	if ValidAnalysisObjectiveClass("bounded_question_v2") {
		t.Fatal("expected an unsupported class to be rejected")
	}
}

// TestAnalysisCoverageStatusTextDistinguishesCoverageFromComprehension is
// criterion 9: user-facing coverage text always reports verified byte
// ranges and never claims review, comprehension, or approval. This fixes
// the exact wording for both the complete and incomplete cases.
func TestAnalysisCoverageStatusTextDistinguishesCoverageFromComprehension(t *testing.T) {
	complete := AnalysisCoverage{CoveredBytes: 4096, Complete: true}
	wantComplete := "Source coverage: 4096 verified bytes, complete. This reports which byte ranges were read and digest-verified, not that the content was reviewed, understood, or approved."
	if got := complete.StatusText(); got != wantComplete {
		t.Fatalf("complete coverage status text = %q, want %q", got, wantComplete)
	}

	incomplete := AnalysisCoverage{CoveredBytes: 2048, Complete: false, Gaps: []AnalysisByteRange{{OffsetBytes: 2048, LengthBytes: 1024}}}
	wantIncomplete := "Source coverage: 2048 verified bytes, incomplete (1 gap range(s) remain). This reports which byte ranges were read and digest-verified, not that the content was reviewed, understood, or approved."
	if got := incomplete.StatusText(); got != wantIncomplete {
		t.Fatalf("incomplete coverage status text = %q, want %q", got, wantIncomplete)
	}

	for _, text := range []string{complete.StatusText(), incomplete.StatusText()} {
		if !strings.Contains(text, "verified byte") && !strings.Contains(text, "byte ranges") {
			t.Fatalf("coverage status text does not report verified byte ranges: %q", text)
		}
		if !strings.Contains(text, "not that the content was reviewed, understood, or approved") {
			t.Fatalf("coverage status text does not disclaim review, comprehension, and approval: %q", text)
		}
	}
}

func TestAnalysisSentinelErrorsWrapExactlyOne(t *testing.T) {
	err := (AnalysisLimits{}).Validate()
	if !errors.Is(err, ErrAnalysisValidation) {
		t.Fatalf("expected limits validation error to wrap ErrAnalysisValidation, got %v", err)
	}
	if errors.Is(err, ErrAnalysisUnavailable) || errors.Is(err, ErrAnalysisCASConflict) {
		t.Fatalf("expected limits validation error to wrap only ErrAnalysisValidation, got %v", err)
	}
}
