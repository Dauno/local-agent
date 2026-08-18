package resultanalysis_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

// The tests in this file are the reviewer's round-1 regression fixtures for
// FIND-096 and FIND-098 (docs/root-orchestrator-v2/hallazgos/tests-regresion-ronda1/regresion-h1-h2.go.txt),
// incorporated verbatim in fixture construction and given definitive,
// asserting names. Do not weaken these assertions to make a future change
// pass; if a future change requires that, stop and say so instead.

func reviewLimits(segBytes int64, bp int, overlapMax int64, leaves int) domain.AnalysisLimits {
	return domain.AnalysisLimits{
		MaxSegmentBytes: segBytes, OverlapBasisPoints: bp, OverlapMaxBytes: overlapMax,
		MaxLeaves: leaves, MaxReductionFanIn: 16, MaxReductionDepth: 4,
		MaxConcurrentLeaves: 2, MaxAttemptsPerStep: 2, CallTimeoutSeconds: 120,
		WallTimeSeconds: 900, EvidenceExcerptBytes: 2048, EvidenceSelectorsPerLeaf: 8,
		EvidenceReferencesPerPacket: 32, BundleBytes: 32768,
	}
}

// TestFIND096OverlapAtCeilingWithMultibyteBoundary is the FIND-096
// regression: overlap configured exactly at the hard ceiling, and the byte
// proposed as the next segment's start falls in the middle of a rune. The
// old backward rune snap pushed the overlap past 4096 and the manifest
// rejected itself.
func TestFIND096OverlapAtCeilingWithMultibyteBoundary(t *testing.T) {
	// 65536 * 1000 / 10000 = 6553, capped to 4096 -> overlap = 4096 exact.
	limits := reviewLimits(65536, 1000, 4096, 8)

	var b bytes.Buffer
	b.Write(bytes.Repeat([]byte("a"), 61439))
	b.WriteString("€") // 3 bytes: 61439, 61440, 61441
	b.Write(bytes.Repeat([]byte("a"), 70000-b.Len()))
	source := b.Bytes()

	if source[61440] < 0x80 || source[61440] >= 0xC0 {
		t.Fatalf("fixture malformed: source[61440]=%#x is not a UTF-8 continuation byte", source[61440])
	}

	manifest, err := resultanalysis.Segment(resultanalysis.SegmenterTextV1, source, limits)
	if err != nil {
		t.Fatalf("FIND-096: legitimate multibyte source rejected: %v", err)
	}
	for _, s := range manifest.Segments {
		if s.OverlapPrevBytes > limits.OverlapMaxBytes {
			t.Fatalf("segment %d overlap %d exceeds the configured ceiling %d", s.Ordinal, s.OverlapPrevBytes, limits.OverlapMaxBytes)
		}
		t.Logf("segment %d offset=%d len=%d overlap=%d", s.Ordinal, s.OffsetBytes, s.LengthBytes, s.OverlapPrevBytes)
	}
}

// TestFIND098IndentedFenceNotSplit is the FIND-098 regression: a fence
// indented two spaces (valid CommonMark) was not recognized, so the
// segmenter split inside a block that fit one segment.
func TestFIND098IndentedFenceNotSplit(t *testing.T) {
	limits := reviewLimits(200, 1000, 4096, 16)

	prose := strings.Repeat("linea de prosa corta\n", 7) // 147 bytes
	fenceBody := strings.Repeat("codigo\n", 12)          // 84 bytes

	build := func(indent string) []byte {
		return []byte(prose + indent + "```\n" + fenceBody + indent + "```\n")
	}

	for _, tc := range []struct{ name, indent string }{
		{"column 0", ""},
		{"indented 2 spaces", "  "},
	} {
		source := build(tc.indent)
		manifest, err := resultanalysis.Segment(resultanalysis.SegmenterMarkdownV1, source, limits)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		openIdx := bytes.Index(source, []byte(tc.indent+"```\n"))
		closeIdx := bytes.LastIndex(source, []byte(tc.indent+"```\n")) + len(tc.indent) + 4
		split := false
		for _, s := range manifest.Segments {
			if s.OffsetBytes > int64(openIdx) && s.OffsetBytes < int64(closeIdx) {
				split = true
			}
		}
		if split {
			t.Errorf("%s: fence [%d,%d) was split, segments=%+v", tc.name, openIdx, closeIdx, manifest.Segments)
		}
	}
}
