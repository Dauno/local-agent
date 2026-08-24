// Package resultanalysis holds the pure, deterministic building blocks of
// TRD 07 objective-bound result analysis: the segmenters that turn one
// verified source into a bounded, deterministic segment manifest. No
// function in this package performs IO, reads the clock, generates random
// values, or starts a goroutine.
package resultanalysis

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// Segmenter version identifiers. text_v1 is the fallback for every media
// type without a dedicated segmenter, including JSON, code, and logs.
const (
	SegmenterTextV1     = "text_v1"
	SegmenterMarkdownV1 = "markdown_v1"
)

// Segment builds a deterministic segment manifest for source using the
// named segmenter version and limits. The same source, version, and limits
// always produce the same manifest, byte for byte, including every digest.
// A source that would need more than limits.MaxLeaves segments fails typed
// with domain.AnalysisFailureSourceTooLarge before any segment is produced.
func Segment(version string, source []byte, limits domain.AnalysisLimits) (domain.AnalysisSegmentManifest, error) {
	if err := limits.Validate(); err != nil {
		return domain.AnalysisSegmentManifest{}, err
	}
	if len(source) == 0 {
		return domain.AnalysisSegmentManifest{}, fmt.Errorf("%w: analysis source must not be empty", domain.ErrAnalysisValidation)
	}
	if !utf8.Valid(source) {
		return domain.AnalysisSegmentManifest{}, fmt.Errorf("%w: analysis source is not valid UTF-8", domain.ErrAnalysisValidation)
	}

	var bounds []int
	switch version {
	case SegmenterTextV1:
		bounds = segmentTextBounds(source, limits.MaxSegmentBytes)
	case SegmenterMarkdownV1:
		bounds = segmentMarkdownBounds(source, limits.MaxSegmentBytes)
	default:
		return domain.AnalysisSegmentManifest{}, fmt.Errorf("%w: unknown segmenter version %q", domain.ErrAnalysisValidation, version)
	}
	if len(bounds) > limits.MaxLeaves {
		return domain.AnalysisSegmentManifest{}, fmt.Errorf("%w: %s: source needs %d segments, more than the configured maximum %d",
			domain.ErrAnalysisValidation, domain.AnalysisFailureSourceTooLarge, len(bounds), limits.MaxLeaves)
	}
	return buildManifest(source, version, bounds, limits)
}

// overlapBytesFor returns the fixed overlap, in bytes, for one nominal
// segment size: a basis-point fraction of the nominal size, capped by the
// configured absolute maximum. It never depends on any individual segment's
// actual length, so it is the same value for every boundary in one
// manifest, as the TRD requires.
func overlapBytesFor(nominal int64, limits domain.AnalysisLimits) int64 {
	overlap := max(min(nominal*int64(limits.OverlapBasisPoints)/10000, limits.OverlapMaxBytes), 0)
	return overlap
}

// buildManifest turns a list of non-overlapping base cut points into the
// final overlapping manifest. bounds[i] is the exclusive end offset of base
// chunk i; base chunk 0 starts at 0 and base chunk i (i>0) starts at
// bounds[i-1]. Every bounds[i] is guaranteed by the callers to be a valid
// UTF-8 rune boundary.
func buildManifest(source []byte, version string, bounds []int, limits domain.AnalysisLimits) (domain.AnalysisSegmentManifest, error) {
	overlap := overlapBytesFor(limits.MaxSegmentBytes, limits)
	segments := make([]domain.AnalysisSegment, 0, len(bounds))
	baseStart := 0
	for i, end := range bounds {
		finalStart := baseStart
		var overlapPrev int64
		if i > 0 {
			floor := int(segments[i-1].OffsetBytes) + 1
			proposed := max(max(baseStart-int(overlap), floor), 0)
			finalStart = advanceToRuneBoundary(source, proposed, baseStart)
			overlapPrev = int64(baseStart - finalStart)
		}
		segment := domain.AnalysisSegment{
			Ordinal:          i,
			OffsetBytes:      int64(finalStart),
			LengthBytes:      int64(end - finalStart),
			SHA256:           hexSHA256(source[finalStart:end]),
			SegmenterVersion: version,
			OverlapPrevBytes: overlapPrev,
		}
		segments = append(segments, segment)
		baseStart = end
	}
	manifest := domain.AnalysisSegmentManifest{
		SourceSHA256: hexSHA256(source),
		SourceBytes:  int64(len(source)),
		Segments:     segments,
	}
	if err := manifest.Validate(); err != nil {
		return domain.AnalysisSegmentManifest{}, err
	}
	return manifest, nil
}

// advanceToRuneBoundary returns the closest UTF-8 rune-start position at or
// after proposed, searching only forward up to ceiling. ceiling is always a
// valid boundary itself (it is either bounds[i-1], produced by a cut
// function that only returns boundaries, or len(source)), so this always
// terminates with a valid boundary without needing to search backward.
//
// FIND-096: an earlier version snapped backward first when proposed itself
// was mid-rune. proposed is baseStart minus the requested overlap, already
// at the configured overlap ceiling in the worst case (overlap capped by
// limits.OverlapMaxBytes); snapping backward moves the start even earlier,
// which can only enlarge the overlap past its own cap, so
// AnalysisSegmentManifest.Validate then rejected the manifest the
// segmenter had just built. Searching only forward instead can only move
// finalStart closer to baseStart, which can only shrink OverlapPrevBytes
// below the requested overlap, never grow it past the cap.
func advanceToRuneBoundary(source []byte, proposed, ceiling int) int {
	pos := max(proposed, 0)
	for pos < ceiling && !utf8.RuneStart(source[pos]) {
		pos++
	}
	return pos
}

// safeRuneCut returns a cut position in (start, limit] that lands on a
// UTF-8 rune boundary, guaranteeing forward progress (the result is always
// greater than start) whenever the source contains at least one more rune
// after start. limit is clamped to len(source).
func safeRuneCut(source []byte, start, limit int) int {
	if limit > len(source) {
		limit = len(source)
	}
	pos := limit
	for pos > start && !utf8.RuneStart(source[pos]) {
		pos--
	}
	if pos > start {
		return pos
	}
	pos = start + 1
	for pos < len(source) && !utf8.RuneStart(source[pos]) {
		pos++
	}
	return pos
}

// lastParagraphBreak returns the offset right after the last blank-line
// paragraph break ("\n\n") found within source[start:limit], or -1 when
// none exists.
func lastParagraphBreak(source []byte, start, limit int) int {
	idx := bytes.LastIndex(source[start:limit], []byte("\n\n"))
	if idx < 0 {
		return -1
	}
	return start + idx + 2
}

// lastLineBreak returns the offset right after the last '\n' found within
// source[start:limit], or -1 when none exists.
func lastLineBreak(source []byte, start, limit int) int {
	idx := bytes.LastIndexByte(source[start:limit], '\n')
	if idx < 0 {
		return -1
	}
	return start + idx + 1
}

func hexSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest)
}
