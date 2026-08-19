package resultanalysis

import (
	"context"
	"crypto/sha256"
	"fmt"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// EvidenceRejection is one selector the host refused to resolve into an
// excerpt, with a short, non-sensitive reason. It never carries excerpt
// text: a rejected selector's bytes are never persisted.
type EvidenceRejection struct {
	Index  int
	Reason string
}

// EvidenceResolution is the host's verdict over one leaf's declared
// evidence selectors: the accepted, digest-verified excerpts, and the
// selectors that were rejected and why. Version 1 policy rejects only the
// individual selector, never the whole leaf; a leaf whose surviving
// findings and evidence become inconsistent after rejection is caught by
// domain.AnalysisLeaf.Validate at the call site, not here.
type EvidenceResolution struct {
	Accepted []port.AnalysisEvidenceExcerpt
	Rejected []EvidenceRejection
}

// ResolveEvidence re-reads segment's exact byte range fresh through
// port.TrustedResultStore, verifies it still matches the segment's own
// recorded digest, then resolves every selector against that freshly read
// and verified text. A selector that falls outside the segment, crosses a
// non-rune boundary, or exceeds limits.EvidenceExcerptBytes is rejected
// with a reason and dropped; it never aborts resolution of the remaining
// selectors.
func ResolveEvidence(ctx context.Context, store port.TrustedResultStore, resultID string, scope domain.ResultScope, segment domain.AnalysisSegment, selectors []domain.AnalysisByteRange, limits domain.AnalysisLimits) (EvidenceResolution, error) {
	if store == nil {
		return EvidenceResolution{}, fmt.Errorf("%w: evidence resolution requires a source store", domain.ErrAnalysisUnavailable)
	}
	if err := segment.Validate(); err != nil {
		return EvidenceResolution{}, err
	}
	if err := limits.Validate(); err != nil {
		return EvidenceResolution{}, err
	}
	if len(selectors) == 0 {
		return EvidenceResolution{}, nil
	}
	if len(selectors) > limits.EvidenceSelectorsPerLeaf {
		return EvidenceResolution{}, fmt.Errorf("%w: leaf declares more evidence selectors than the configured maximum", domain.ErrAnalysisValidation)
	}

	segmentText, err := readFullRange(ctx, store, resultID, scope, "", segment.OffsetBytes, segment.LengthBytes)
	if err != nil {
		return EvidenceResolution{}, err
	}
	segmentBytes := []byte(segmentText)
	if int64(len(segmentBytes)) != segment.LengthBytes {
		return EvidenceResolution{}, fmt.Errorf("%w: re-read segment length does not match the manifest", domain.ErrAnalysisUnavailable)
	}
	if hexSHA256(segmentBytes) != segment.SHA256 {
		return EvidenceResolution{}, fmt.Errorf("%w: segment digest changed since segmentation", domain.ErrAnalysisValidation)
	}

	var resolution EvidenceResolution
	for i, selector := range selectors {
		excerpt, err := resolveOneSelector(segment, segmentBytes, selector, limits)
		if err != "" {
			resolution.Rejected = append(resolution.Rejected, EvidenceRejection{Index: i, Reason: err})
			continue
		}
		resolution.Accepted = append(resolution.Accepted, excerpt)
	}
	return resolution, nil
}

// resolveOneSelector returns either a resolved excerpt or a non-empty
// rejection reason, never both.
func resolveOneSelector(segment domain.AnalysisSegment, segmentBytes []byte, selector domain.AnalysisByteRange, limits domain.AnalysisLimits) (port.AnalysisEvidenceExcerpt, string) {
	if err := selector.Validate(); err != nil {
		return port.AnalysisEvidenceExcerpt{}, "selector is not a valid byte range"
	}
	if selector.LengthBytes > int64(limits.EvidenceExcerptBytes) {
		return port.AnalysisEvidenceExcerpt{}, "selector exceeds the configured excerpt byte maximum"
	}
	start, end := selector.OffsetBytes, selector.OffsetBytes+selector.LengthBytes
	if start < 0 || end > int64(len(segmentBytes)) {
		return port.AnalysisEvidenceExcerpt{}, "selector falls outside the leaf's own segment"
	}
	if !utf8.RuneStart(segmentBytes[start]) {
		return port.AnalysisEvidenceExcerpt{}, "selector start crosses a non-rune boundary"
	}
	if end < int64(len(segmentBytes)) && !utf8.RuneStart(segmentBytes[end]) {
		return port.AnalysisEvidenceExcerpt{}, "selector end crosses a non-rune boundary"
	}
	excerpt := segmentBytes[start:end]
	if !utf8.Valid(excerpt) {
		return port.AnalysisEvidenceExcerpt{}, "selector excerpt is not valid UTF-8"
	}
	digest := hexSHA256(excerpt)
	return port.AnalysisEvidenceExcerpt{
		EvidenceID:     evidenceID(segment.SHA256, selector),
		SegmentOrdinal: segment.Ordinal,
		Range:          selector,
		SHA256:         digest,
		Excerpt:        string(excerpt),
	}, ""
}

// evidenceID derives a deterministic, opaque, 64-hex identifier from the
// segment's own digest and the selector's byte range, netstring-encoded
// like domain.AnalysisIdentity.SemanticDigest so a retried resolution of
// the same selector against the same segment always produces the same
// evidence id.
func evidenceID(segmentSHA256 string, selector domain.AnalysisByteRange) string {
	h := sha256.New()
	writeNetstring(h, segmentSHA256)
	writeNetstring(h, fmt.Sprintf("%d", selector.OffsetBytes))
	writeNetstring(h, fmt.Sprintf("%d", selector.LengthBytes))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func writeNetstring(h interface{ Write([]byte) (int, error) }, value string) {
	_, _ = fmt.Fprintf(h, "%d:%s", len(value), value)
}
