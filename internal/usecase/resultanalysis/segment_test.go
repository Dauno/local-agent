package resultanalysis_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

func errIsAnalysisValidation(err error) bool {
	return errors.Is(err, domain.ErrAnalysisValidation)
}

func timeAfter() <-chan time.Time {
	return time.After(2 * time.Second)
}

func testLimits() domain.AnalysisLimits {
	return domain.AnalysisLimits{
		MaxSegmentBytes:             120,
		OverlapBasisPoints:          2000,
		OverlapMaxBytes:             40,
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

func hexSHA(data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest)
}

// 1. Determinism for both segmenter versions.
func TestSegmentDeterministic(t *testing.T) {
	source := []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 40))
	for _, version := range []string{resultanalysis.SegmenterTextV1, resultanalysis.SegmenterMarkdownV1} {
		t.Run(version, func(t *testing.T) {
			first, err := resultanalysis.Segment(version, source, testLimits())
			if err != nil {
				t.Fatalf("first segmentation failed: %v", err)
			}
			second, err := resultanalysis.Segment(version, source, testLimits())
			if err != nil {
				t.Fatalf("second segmentation failed: %v", err)
			}
			if len(first.Segments) != len(second.Segments) {
				t.Fatalf("segment counts differ: %d vs %d", len(first.Segments), len(second.Segments))
			}
			for i := range first.Segments {
				if first.Segments[i] != second.Segments[i] {
					t.Fatalf("segment %d differs: %+v vs %+v", i, first.Segments[i], second.Segments[i])
				}
			}
			if first.SourceSHA256 != second.SourceSHA256 {
				t.Fatal("source digest differs across runs")
			}
		})
	}
}

// 2. Rune boundaries with multibyte source, including a 4-byte character
// landing exactly at a would-be cut point.
func TestSegmentRuneBoundariesWithMultibyteSource(t *testing.T) {
	// U+1F600 (grinning face) is a 4-byte UTF-8 rune. Repeat it around the
	// nominal segment boundary so a naive byte cut would split it.
	emoji := "\U0001F600"
	source := []byte(strings.Repeat("a", 118) + strings.Repeat(emoji, 10) + strings.Repeat("b", 40))
	limits := testLimits()
	for _, version := range []string{resultanalysis.SegmenterTextV1, resultanalysis.SegmenterMarkdownV1} {
		t.Run(version, func(t *testing.T) {
			manifest, err := resultanalysis.Segment(version, source, limits)
			if err != nil {
				t.Fatalf("segmentation failed: %v", err)
			}
			for _, segment := range manifest.Segments {
				if !utf8.RuneStart(source[segment.OffsetBytes]) {
					t.Fatalf("segment %d starts mid-rune at offset %d", segment.Ordinal, segment.OffsetBytes)
				}
				end := segment.OffsetBytes + segment.LengthBytes
				if end < int64(len(source)) && !utf8.RuneStart(source[end]) {
					t.Fatalf("segment %d ends mid-rune at offset %d", segment.Ordinal, end)
				}
				if !utf8.Valid(source[segment.OffsetBytes:end]) {
					t.Fatalf("segment %d bytes are not valid UTF-8", segment.Ordinal)
				}
			}
		})
	}
}

// 3. Complete coverage without double counting, with overlap greater than
// zero.
func TestSegmentCoverageCompleteWithoutDoubleCounting(t *testing.T) {
	source := []byte(strings.Repeat("Paragraph one line.\nParagraph one line two.\n\n", 20))
	limits := testLimits()
	for _, version := range []string{resultanalysis.SegmenterTextV1, resultanalysis.SegmenterMarkdownV1} {
		t.Run(version, func(t *testing.T) {
			manifest, err := resultanalysis.Segment(version, source, limits)
			if err != nil {
				t.Fatalf("segmentation failed: %v", err)
			}
			coverage := manifest.Coverage()
			if !coverage.Complete {
				t.Fatalf("expected complete coverage, gaps=%+v", coverage.Gaps)
			}
			if coverage.CoveredBytes != int64(len(source)) {
				t.Fatalf("expected covered bytes to equal source size %d, got %d", len(source), coverage.CoveredBytes)
			}
			hasOverlap := false
			for _, segment := range manifest.Segments {
				if segment.OverlapPrevBytes > 0 {
					hasOverlap = true
				}
			}
			if !hasOverlap {
				t.Fatal("expected at least one segment to carry overlap in this fixture")
			}
		})
	}
}

// 4. A semantic unit (one sentence) that crosses the nominal chunk boundary
// survives intact in at least one segment because of overlap.
func TestSegmentOverlapPreservesUnitCrossingBoundary(t *testing.T) {
	// Construct a source where a distinctive sentence straddles the nominal
	// 120-byte boundary.
	filler := strings.Repeat("x", 100)
	sentence := "THIS SENTENCE MUST SURVIVE INTACT IN ONE SEGMENT."
	source := []byte(filler + " " + sentence + " " + strings.Repeat("y", 100))
	limits := testLimits()
	manifest, err := resultanalysis.Segment(resultanalysis.SegmenterTextV1, source, limits)
	if err != nil {
		t.Fatalf("segmentation failed: %v", err)
	}
	found := false
	for _, segment := range manifest.Segments {
		content := string(source[segment.OffsetBytes : segment.OffsetBytes+segment.LengthBytes])
		if strings.Contains(content, sentence) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected the sentence crossing the nominal boundary to survive intact in some segment")
	}
}

// 5. A fenced block that fits inside a segment is never split.
func TestSegmentMarkdownDoesNotSplitFittingFence(t *testing.T) {
	fence := "```go\nfunc main() {}\n```\n"
	source := []byte(strings.Repeat("intro text. ", 8) + "\n\n" + fence + strings.Repeat("outro text. ", 8))
	manifest, err := resultanalysis.Segment(resultanalysis.SegmenterMarkdownV1, source, testLimits())
	if err != nil {
		t.Fatalf("segmentation failed: %v", err)
	}
	fenceStart := strings.Index(string(source), fence)
	fenceEnd := fenceStart + len(fence)
	found := false
	for _, segment := range manifest.Segments {
		start, end := int(segment.OffsetBytes), int(segment.OffsetBytes+segment.LengthBytes)
		if start <= fenceStart && end >= fenceEnd {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected the fitting fenced block to remain inside one segment")
	}
}

// FIND-098 regression, acceptance criterion 5: an indented fence (1, 2, or
// 3 spaces, the CommonMark allowance) that fits inside a segment must not
// be split, one subtest per indentation level.
func TestSegmentMarkdownIndentedFenceNotSplit(t *testing.T) {
	for indent := 1; indent <= 3; indent++ {
		prefix := strings.Repeat(" ", indent)
		t.Run(fmt.Sprintf("indent_%d", indent), func(t *testing.T) {
			fence := prefix + "```go\n" + prefix + "func main() {}\n" + prefix + "```\n"
			source := []byte(strings.Repeat("intro text. ", 8) + "\n\n" + fence + strings.Repeat("outro text. ", 8))
			manifest, err := resultanalysis.Segment(resultanalysis.SegmenterMarkdownV1, source, testLimits())
			if err != nil {
				t.Fatalf("segmentation failed: %v", err)
			}
			fenceStart := strings.Index(string(source), fence)
			fenceEnd := fenceStart + len(fence)
			found := false
			for _, segment := range manifest.Segments {
				start, end := int(segment.OffsetBytes), int(segment.OffsetBytes+segment.LengthBytes)
				if start <= fenceStart && end >= fenceEnd {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected the %d-space indented fitting fenced block to remain inside one segment", indent)
			}
		})
	}
}

// 6. A fenced block bigger than one segment is split, and the parts tile it
// exactly and contiguously (the only way, absent an explicit block-ordinal
// field, that a caller can recover "the same enclosing block").
func TestSegmentMarkdownSplitsOversizedFenceContiguously(t *testing.T) {
	body := strings.Repeat("code line here\n", 30)
	fence := "```text\n" + body + "```\n"
	source := []byte("intro\n\n" + fence + "\n\noutro\n")
	limits := testLimits()
	manifest, err := resultanalysis.Segment(resultanalysis.SegmenterMarkdownV1, source, limits)
	if err != nil {
		t.Fatalf("segmentation failed: %v", err)
	}
	fenceStart := strings.Index(string(source), fence)
	fenceEnd := fenceStart + len(fence)
	if int64(fenceEnd-fenceStart) <= limits.MaxSegmentBytes {
		t.Fatalf("fixture fence must be larger than one segment: %d bytes", fenceEnd-fenceStart)
	}
	var covering []domain.AnalysisSegment
	for _, segment := range manifest.Segments {
		start, end := int(segment.OffsetBytes), int(segment.OffsetBytes+segment.LengthBytes)
		if end > fenceStart && start < fenceEnd {
			covering = append(covering, segment)
		}
	}
	if len(covering) < 2 {
		t.Fatalf("expected the oversized fence to span multiple segments, got %d", len(covering))
	}
	for i := 1; i < len(covering); i++ {
		if covering[i].Ordinal != covering[i-1].Ordinal+1 {
			t.Fatalf("expected contiguous ordinals across the split fence, got %d then %d", covering[i-1].Ordinal, covering[i].Ordinal)
		}
	}
	coverage := manifest.Coverage()
	if !coverage.Complete {
		t.Fatalf("expected complete coverage across the split fence, gaps=%+v", coverage.Gaps)
	}
}

// 7. An unclosed fence at EOF must not hang and must not produce a segment
// that violates the configured leaf or segment-size bounds.
func TestSegmentMarkdownUnterminatedFenceDoesNotHang(t *testing.T) {
	body := strings.Repeat("unterminated code line\n", 50)
	source := []byte("intro\n\n```text\n" + body)
	limits := testLimits()
	limits.MaxLeaves = 32

	done := make(chan struct{})
	var manifest domain.AnalysisSegmentManifest
	var err error
	go func() {
		manifest, err = resultanalysis.Segment(resultanalysis.SegmenterMarkdownV1, source, limits)
		close(done)
	}()
	select {
	case <-done:
	case <-timeAfter():
		t.Fatal("segmentation of an unterminated fence did not return in time")
	}
	if err != nil {
		t.Fatalf("segmentation failed: %v", err)
	}
	if len(manifest.Segments) > limits.MaxLeaves {
		t.Fatalf("expected at most %d segments, got %d", limits.MaxLeaves, len(manifest.Segments))
	}
	for _, segment := range manifest.Segments {
		if segment.LengthBytes > limits.MaxSegmentBytes+limits.OverlapMaxBytes {
			t.Fatalf("segment %d length %d exceeds nominal+overlap bound", segment.Ordinal, segment.LengthBytes)
		}
	}
	coverage := manifest.Coverage()
	if !coverage.Complete {
		t.Fatalf("expected complete coverage even with an unterminated fence, gaps=%+v", coverage.Gaps)
	}
}

// 8. A source larger than max_leaves * max_segment_bytes fails typed before
// any manifest is produced.
func TestSegmentOversizedSourceFailsTypedBeforeManifest(t *testing.T) {
	limits := testLimits()
	limits.MaxLeaves = 2
	source := []byte(strings.Repeat("a", int(limits.MaxSegmentBytes)*int(limits.MaxLeaves)+1))
	for _, version := range []string{resultanalysis.SegmenterTextV1, resultanalysis.SegmenterMarkdownV1} {
		t.Run(version, func(t *testing.T) {
			manifest, err := resultanalysis.Segment(version, source, limits)
			if err == nil {
				t.Fatal("expected an oversized source to fail")
			}
			if !errIsAnalysisValidation(err) {
				t.Fatalf("expected ErrAnalysisValidation, got %v", err)
			}
			if len(manifest.Segments) != 0 {
				t.Fatal("expected no partial manifest on oversized-source failure")
			}
		})
	}
}

func TestSegmentRejectsEmptySource(t *testing.T) {
	_, err := resultanalysis.Segment(resultanalysis.SegmenterTextV1, nil, testLimits())
	if !errIsAnalysisValidation(err) {
		t.Fatalf("expected ErrAnalysisValidation for empty source, got %v", err)
	}
}

func TestSegmentRejectsUnknownVersion(t *testing.T) {
	_, err := resultanalysis.Segment("unknown_v1", []byte("hello"), testLimits())
	if !errIsAnalysisValidation(err) {
		t.Fatalf("expected ErrAnalysisValidation for unknown version, got %v", err)
	}
}

// FIND-096 regression, acceptance criterion 2: no manifest Segment produces
// is ever rejected by its own AnalysisSegmentManifest.Validate, across
// several extreme limit combinations (overlap at the hard ceiling, overlap
// as a large basis-point fraction, tiny nominal segments) with a multibyte
// rune placed at every byte offset near a would-be cut point.
func TestSegmentNeverProducesASelfRejectingManifest(t *testing.T) {
	limitCases := map[string]domain.AnalysisLimits{
		"overlap at hard byte ceiling": {
			MaxSegmentBytes: 512, OverlapBasisPoints: 2000, OverlapMaxBytes: domain.HardMaxAnalysisOverlapBytes,
			MaxLeaves: 64, MaxReductionFanIn: 8, MaxReductionDepth: 4, MaxConcurrentLeaves: 2,
			MaxAttemptsPerStep: 2, CallTimeoutSeconds: 120, WallTimeSeconds: 900,
			EvidenceExcerptBytes: 2048, EvidenceSelectorsPerLeaf: 8, EvidenceReferencesPerPacket: 32, BundleBytes: 32768,
		},
		"overlap basis points at hard max": {
			MaxSegmentBytes: 300, OverlapBasisPoints: domain.HardMaxAnalysisOverlapBasisPoints, OverlapMaxBytes: 4096,
			MaxLeaves: 64, MaxReductionFanIn: 8, MaxReductionDepth: 4, MaxConcurrentLeaves: 2,
			MaxAttemptsPerStep: 2, CallTimeoutSeconds: 120, WallTimeSeconds: 900,
			EvidenceExcerptBytes: 2048, EvidenceSelectorsPerLeaf: 8, EvidenceReferencesPerPacket: 32, BundleBytes: 32768,
		},
		"tiny nominal segment with overlap near its size": {
			MaxSegmentBytes: 32, OverlapBasisPoints: domain.HardMaxAnalysisOverlapBasisPoints, OverlapMaxBytes: 30,
			MaxLeaves: 256, MaxReductionFanIn: 8, MaxReductionDepth: 4, MaxConcurrentLeaves: 2,
			MaxAttemptsPerStep: 2, CallTimeoutSeconds: 120, WallTimeSeconds: 900,
			EvidenceExcerptBytes: 2048, EvidenceSelectorsPerLeaf: 8, EvidenceReferencesPerPacket: 32, BundleBytes: 32768,
		},
	}
	for name, limits := range limitCases {
		t.Run(name, func(t *testing.T) {
			nominal := int(limits.MaxSegmentBytes)
			// Place a 3-byte multibyte rune at every offset in a window
			// around each nominal boundary, for the first few boundaries.
			for boundary := 1; boundary <= 3; boundary++ {
				for delta := -4; delta <= 4; delta++ {
					pos := boundary*nominal + delta
					if pos < 1 {
						continue
					}
					var b bytes.Buffer
					b.Write(bytes.Repeat([]byte("a"), pos))
					b.WriteString("€")
					b.Write(bytes.Repeat([]byte("a"), nominal*5-b.Len()))
					source := b.Bytes()

					for _, version := range []string{resultanalysis.SegmenterTextV1, resultanalysis.SegmenterMarkdownV1} {
						manifest, err := resultanalysis.Segment(version, source, limits)
						if err != nil {
							t.Fatalf("%s: boundary=%d delta=%d version=%s: %v", name, boundary, delta, version, err)
						}
						if err := manifest.Validate(); err != nil {
							t.Fatalf("%s: boundary=%d delta=%d version=%s: manifest failed its own Validate: %v", name, boundary, delta, version, err)
						}
						for _, segment := range manifest.Segments {
							if segment.OverlapPrevBytes > limits.OverlapMaxBytes {
								t.Fatalf("%s: boundary=%d delta=%d version=%s: segment %d overlap %d exceeds ceiling %d",
									name, boundary, delta, version, segment.Ordinal, segment.OverlapPrevBytes, limits.OverlapMaxBytes)
							}
						}
					}
				}
			}
		})
	}
}

func TestSegmentSourceDigestMatchesInput(t *testing.T) {
	source := []byte("hello world, this is the analysis source text.")
	manifest, err := resultanalysis.Segment(resultanalysis.SegmenterTextV1, source, testLimits())
	if err != nil {
		t.Fatalf("segmentation failed: %v", err)
	}
	if manifest.SourceSHA256 != hexSHA(source) {
		t.Fatalf("expected source digest %s, got %s", hexSHA(source), manifest.SourceSHA256)
	}
}
