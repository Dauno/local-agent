package resultanalysis_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

func testSegment(offset, length int64, text []byte) domain.AnalysisSegment {
	digest := sha256.Sum256(text[offset : offset+length])
	return domain.AnalysisSegment{
		Ordinal: 0, OffsetBytes: offset, LengthBytes: length,
		SHA256: fmt.Sprintf("%x", digest), SegmenterVersion: "text_v1",
	}
}

func TestResolveEvidenceAcceptsAValidSelectorAndVerifiesItsDigest(t *testing.T) {
	source := []byte("retry_limit = 5\ntimeout = 30\n")
	store := newFakeTrustedResultStore(source, 4096)
	segment := testSegment(0, int64(len(source)), source)
	limits := reviewLimits(120, 1000, 40, 64)

	selector := domain.AnalysisByteRange{OffsetBytes: 0, LengthBytes: 16} // "retry_limit = 5\n"
	resolution, err := resultanalysis.ResolveEvidence(context.Background(), store, store.identity.ResultID, testScope(), segment, []domain.AnalysisByteRange{selector}, limits)
	if err != nil {
		t.Fatalf("resolve evidence: %v", err)
	}
	if len(resolution.Rejected) != 0 {
		t.Fatalf("expected no rejections, got %+v", resolution.Rejected)
	}
	if len(resolution.Accepted) != 1 {
		t.Fatalf("expected one accepted excerpt, got %d", len(resolution.Accepted))
	}
	excerpt := resolution.Accepted[0]
	if excerpt.Excerpt != "retry_limit = 5\n" {
		t.Fatalf("excerpt = %q", excerpt.Excerpt)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(excerpt.Excerpt)))
	if excerpt.SHA256 != wantDigest {
		t.Fatalf("excerpt digest = %q, want %q", excerpt.SHA256, wantDigest)
	}
	if !domain.ValidAnalysisID(excerpt.EvidenceID) {
		t.Fatalf("evidence id %q is not a well-formed opaque id", excerpt.EvidenceID)
	}
}

func TestResolveEvidenceRejectsSelectorOutsideSegment(t *testing.T) {
	source := []byte("retry_limit = 5\ntimeout = 30\n")
	store := newFakeTrustedResultStore(source, 4096)
	// The segment covers only the first 16 bytes ("retry_limit = 5\n").
	segment := testSegment(0, 16, source)
	limits := reviewLimits(120, 1000, 40, 64)

	// This selector reaches past the segment's own 16-byte length, into
	// the next segment's territory, even though it is a valid range
	// within the full source.
	selector := domain.AnalysisByteRange{OffsetBytes: 10, LengthBytes: 12}
	resolution, err := resultanalysis.ResolveEvidence(context.Background(), store, store.identity.ResultID, testScope(), segment, []domain.AnalysisByteRange{selector}, limits)
	if err != nil {
		t.Fatalf("resolve evidence: %v", err)
	}
	if len(resolution.Accepted) != 0 {
		t.Fatalf("expected the out-of-segment selector to be rejected, got %+v", resolution.Accepted)
	}
	if len(resolution.Rejected) != 1 || resolution.Rejected[0].Index != 0 {
		t.Fatalf("resolution.Rejected = %+v", resolution.Rejected)
	}
}

func TestResolveEvidenceRejectsSelectorCrossingRuneBoundary(t *testing.T) {
	// "a" + 3-byte euro sign + "a": the euro sign occupies bytes [1,4).
	source := []byte("a€a")
	store := newFakeTrustedResultStore(source, 4096)
	segment := testSegment(0, int64(len(source)), source)
	limits := reviewLimits(120, 1000, 40, 64)

	selector := domain.AnalysisByteRange{OffsetBytes: 2, LengthBytes: 2} // starts mid-rune
	resolution, err := resultanalysis.ResolveEvidence(context.Background(), store, store.identity.ResultID, testScope(), segment, []domain.AnalysisByteRange{selector}, limits)
	if err != nil {
		t.Fatalf("resolve evidence: %v", err)
	}
	if len(resolution.Accepted) != 0 || len(resolution.Rejected) != 1 {
		t.Fatalf("expected the mid-rune selector to be rejected: accepted=%+v rejected=%+v", resolution.Accepted, resolution.Rejected)
	}
}

func TestResolveEvidenceRejectsSelectorOverExcerptBound(t *testing.T) {
	source := []byte("The quick brown fox jumps over the lazy dog and keeps running far away.")
	store := newFakeTrustedResultStore(source, 4096)
	segment := testSegment(0, int64(len(source)), source)
	limits := reviewLimits(120, 1000, 40, 64)
	limits.EvidenceExcerptBytes = 8

	selector := domain.AnalysisByteRange{OffsetBytes: 0, LengthBytes: 20} // over the 8-byte bound
	resolution, err := resultanalysis.ResolveEvidence(context.Background(), store, store.identity.ResultID, testScope(), segment, []domain.AnalysisByteRange{selector}, limits)
	if err != nil {
		t.Fatalf("resolve evidence: %v", err)
	}
	if len(resolution.Accepted) != 0 || len(resolution.Rejected) != 1 {
		t.Fatalf("expected the oversized selector to be rejected: accepted=%+v rejected=%+v", resolution.Accepted, resolution.Rejected)
	}
}

// TestResolveEvidenceRejectsSegmentWhoseContentChangedSinceSegmentation is
// FIND-104: the source result changed underneath an in-flight analysis
// between segmentation and evidence resolution. ResolveEvidence re-reads
// the segment's exact byte range fresh and must reject the whole call, not
// silently resolve selectors against stale or mismatched bytes, because a
// leaf's evidence selectors are byte offsets into a segment whose recorded
// digest no longer matches what is actually stored.
func TestResolveEvidenceRejectsSegmentWhoseContentChangedSinceSegmentation(t *testing.T) {
	original := []byte("retry_limit = 5\ntimeout = 30\n")
	store := newFakeTrustedResultStore(original, 4096)
	segment := testSegment(0, int64(len(original)), original)
	limits := reviewLimits(120, 1000, 40, 64)

	// The source changed after segmentation: same length, different bytes,
	// so the segment's recorded digest (computed from `original`) no
	// longer matches what a fresh ReadRange returns.
	store.content = []byte("retry_limit = 9\ntimeout = 99\n")

	selector := domain.AnalysisByteRange{OffsetBytes: 0, LengthBytes: 16}
	_, err := resultanalysis.ResolveEvidence(context.Background(), store, store.identity.ResultID, testScope(), segment, []domain.AnalysisByteRange{selector}, limits)
	if err == nil {
		t.Fatal("expected a changed segment to be rejected, got nil error")
	}
	if !errors.Is(err, domain.ErrAnalysisValidation) {
		t.Fatalf("changed-segment error = %v, want ErrAnalysisValidation", err)
	}
}

func TestResolveEvidenceOneRejectionDoesNotBlockOthers(t *testing.T) {
	source := []byte("retry_limit = 5\ntimeout = 30\n")
	store := newFakeTrustedResultStore(source, 4096)
	segment := testSegment(0, int64(len(source)), source)
	limits := reviewLimits(120, 1000, 40, 64)

	valid := domain.AnalysisByteRange{OffsetBytes: 0, LengthBytes: 16}
	outOfBounds := domain.AnalysisByteRange{OffsetBytes: 1000, LengthBytes: 5}
	resolution, err := resultanalysis.ResolveEvidence(context.Background(), store, store.identity.ResultID, testScope(), segment,
		[]domain.AnalysisByteRange{outOfBounds, valid}, limits)
	if err != nil {
		t.Fatalf("resolve evidence: %v", err)
	}
	if len(resolution.Accepted) != 1 || resolution.Accepted[0].Excerpt != "retry_limit = 5\n" {
		t.Fatalf("expected the valid selector to still resolve despite the other's rejection: %+v", resolution)
	}
	if len(resolution.Rejected) != 1 || resolution.Rejected[0].Index != 0 {
		t.Fatalf("expected the out-of-bounds selector rejected at index 0: %+v", resolution.Rejected)
	}
}
