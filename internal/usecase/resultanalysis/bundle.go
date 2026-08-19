package resultanalysis

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// BundleBuilder implements port.AnalysisBundleBuilder. It never truncates: a
// bundle whose evidence count, one excerpt's size, one segment's evidence
// count, or serialized size exceeds any of the four static bounds in TRD
// 07's Evidence and Downstream Bundle section is a typed rejection wrapping
// domain.ErrAnalysisValidation. Silently truncating evidence to satisfy a
// downstream request is a named TRD Non-Goal; the caller must instead
// narrow the objective or create explicit, independently reviewable tasks.
type BundleBuilder struct{}

var _ port.AnalysisBundleBuilder = BundleBuilder{}

// Build assembles one bounded downstream bundle. evidence's SegmentOrdinal
// is used as the per-leaf grouping key for the evidence-selectors-per-leaf
// bound, because one segment is produced by exactly one leaf step.
func (BundleBuilder) Build(_ context.Context, analysisID string, packet domain.AnalysisPacket, evidence []port.AnalysisEvidenceExcerpt, limits domain.AnalysisLimits) (port.AnalysisBundle, error) {
	if err := limits.Validate(); err != nil {
		return port.AnalysisBundle{}, err
	}
	if err := packet.Validate(); err != nil {
		return port.AnalysisBundle{}, err
	}
	if len(evidence) > limits.EvidenceReferencesPerPacket {
		return port.AnalysisBundle{}, fmt.Errorf("%w: %s: bundle cites %d evidence excerpts, more than the configured maximum %d",
			domain.ErrAnalysisValidation, domain.AnalysisFailureBundleTooLarge, len(evidence), limits.EvidenceReferencesPerPacket)
	}

	perSegment := make(map[int]int, len(evidence))
	for _, excerpt := range evidence {
		if len(excerpt.Excerpt) > limits.EvidenceExcerptBytes {
			return port.AnalysisBundle{}, fmt.Errorf("%w: %s: evidence excerpt is %d bytes, more than the configured maximum %d",
				domain.ErrAnalysisValidation, domain.AnalysisFailureBundleTooLarge, len(excerpt.Excerpt), limits.EvidenceExcerptBytes)
		}
		perSegment[excerpt.SegmentOrdinal]++
		if perSegment[excerpt.SegmentOrdinal] > limits.EvidenceSelectorsPerLeaf {
			return port.AnalysisBundle{}, fmt.Errorf("%w: %s: segment %d cites more than the configured maximum %d evidence selectors",
				domain.ErrAnalysisValidation, domain.AnalysisFailureBundleTooLarge, excerpt.SegmentOrdinal, limits.EvidenceSelectorsPerLeaf)
		}
	}

	packetPayload, err := EncodePacketPayload(packet)
	if err != nil {
		return port.AnalysisBundle{}, fmt.Errorf("%w: encode bundle packet: %v", domain.ErrAnalysisValidation, err)
	}

	totalBytes := int64(len(packetPayload))
	for _, excerpt := range evidence {
		totalBytes += int64(len(excerpt.Excerpt))
	}
	if totalBytes > int64(limits.BundleBytes) {
		return port.AnalysisBundle{}, fmt.Errorf("%w: %s: bundle is %d bytes, more than the configured maximum %d",
			domain.ErrAnalysisValidation, domain.AnalysisFailureBundleTooLarge, totalBytes, limits.BundleBytes)
	}

	h := sha256.New()
	writeNetstring(h, analysisID)
	h.Write(packetPayload)
	for _, excerpt := range evidence {
		writeNetstring(h, excerpt.EvidenceID)
	}
	contentSHA256 := fmt.Sprintf("%x", h.Sum(nil))

	return port.AnalysisBundle{
		BundleID:   bundleIDFor(analysisID, contentSHA256),
		AnalysisID: analysisID,
		Packet:     packet,
		Evidence:   append([]port.AnalysisEvidenceExcerpt(nil), evidence...),
		Bytes:      totalBytes,
		SHA256:     contentSHA256,
	}, nil
}

// bundleIDFor derives a deterministic, opaque bundle selector from the
// owning analysis and the bundle's own content digest, netstring-encoded so
// no in-band separator choice can create a collision. It carries no storage
// path, credential, or raw provider error: it is a selector, not a
// capability.
func bundleIDFor(analysisID, contentSHA256 string) string {
	h := sha256.New()
	writeNetstring(h, "analysis_bundle")
	writeNetstring(h, analysisID)
	writeNetstring(h, contentSHA256)
	return fmt.Sprintf("%x", h.Sum(nil))
}
