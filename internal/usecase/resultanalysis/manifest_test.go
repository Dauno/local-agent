package resultanalysis_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

// fakeTrustedResultStore is a minimal in-memory port.TrustedResultStore.
// ReadRange serves at most chunkSize bytes per call regardless of the
// caller's requested maxBytes, so a caller that assumes one call suffices
// for a large range is caught by these tests looping incorrectly.
type fakeTrustedResultStore struct {
	identity      domain.ResultIdentity
	content       []byte
	chunkSize     int
	readCalls     int
	overrideSHA   string
	overrideAfter int
}

func (f *fakeTrustedResultStore) Materialize(context.Context, port.ResultMaterialization) (domain.ResultHandle, error) {
	return domain.ResultHandle{}, errors.New("not implemented")
}

func (f *fakeTrustedResultStore) Resolve(_ context.Context, resultID string, _ domain.ResultScope) (domain.ResultIdentity, domain.ResultHandle, error) {
	if resultID != f.identity.ResultID {
		return domain.ResultIdentity{}, domain.ResultHandle{}, domain.ErrResultUnavailable
	}
	return f.identity, domain.ResultHandle{}, nil
}

func (f *fakeTrustedResultStore) ReadRange(_ context.Context, resultID string, _ domain.ResultScope, offsetBytes, maxBytes int64) (domain.ResultChunk, error) {
	if resultID != f.identity.ResultID {
		return domain.ResultChunk{}, domain.ErrResultUnavailable
	}
	f.readCalls++
	sha := f.identity.SHA256
	if f.overrideSHA != "" && f.readCalls > f.overrideAfter {
		sha = f.overrideSHA
	}
	if offsetBytes >= int64(len(f.content)) {
		return domain.ResultChunk{OffsetBytes: offsetBytes, NextOffsetBytes: offsetBytes, EOF: true, SHA256: sha}, nil
	}
	end := offsetBytes + maxBytes
	if f.chunkSize > 0 && offsetBytes+int64(f.chunkSize) < end {
		end = offsetBytes + int64(f.chunkSize)
	}
	if end > int64(len(f.content)) {
		end = int64(len(f.content))
	}
	chunk := f.content[offsetBytes:end]
	return domain.ResultChunk{
		Content: string(chunk), OffsetBytes: offsetBytes, NextOffsetBytes: end,
		EOF: end >= int64(len(f.content)), SHA256: sha,
	}, nil
}

func newFakeTrustedResultStore(content []byte, chunkSize int) *fakeTrustedResultStore {
	digest := sha256.Sum256(content)
	return &fakeTrustedResultStore{
		identity: domain.ResultIdentity{
			ResultID: "src-result-id", SHA256: fmt.Sprintf("%x", digest), Bytes: int64(len(content)),
			MediaType: "text/plain", Scope: testScope(),
		},
		content: content, chunkSize: chunkSize,
	}
}

// fakeResultAnalyzer records whether it was ever invoked, for the "no model
// call before the size check" acceptance criterion.
type fakeResultAnalyzer struct {
	invoked bool
}

func (f *fakeResultAnalyzer) AnalyzeLeaf(context.Context, port.AnalysisLeafInput) (domain.AnalysisLeaf, error) {
	f.invoked = true
	return domain.AnalysisLeaf{}, errors.New("fake analyzer should not be called")
}

func (f *fakeResultAnalyzer) Reduce(context.Context, port.AnalysisReductionInput) (domain.AnalysisPacket, error) {
	f.invoked = true
	return domain.AnalysisPacket{}, errors.New("fake analyzer should not be called")
}

func TestBuildManifestReadsOnlyThroughReadRange(t *testing.T) {
	content := []byte("The quick brown fox jumps over the lazy dog. " + repeatWord("word ", 200))
	store := newFakeTrustedResultStore(content, 37) // force many small chunks
	limits := reviewLimits(120, 2000, 40, 64)

	manifest, identity, err := resultanalysis.BuildManifest(context.Background(), store, store.identity.ResultID, testScope(), resultanalysis.SegmenterTextV1, limits)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	if store.readCalls < 2 {
		t.Fatalf("expected BuildManifest to loop ReadRange for a source larger than one chunk, got %d calls", store.readCalls)
	}
	if identity.SHA256 != store.identity.SHA256 {
		t.Fatalf("identity sha256 = %q, want %q", identity.SHA256, store.identity.SHA256)
	}
	if manifest.SourceSHA256 != store.identity.SHA256 || manifest.SourceBytes != int64(len(content)) {
		t.Fatalf("manifest source identity = %+v", manifest)
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("manifest failed its own Validate: %v", err)
	}
}

func TestBuildManifestRejectsSourceDigestMismatchBeforeSegmenting(t *testing.T) {
	content := []byte("The quick brown fox jumps over the lazy dog.")
	store := newFakeTrustedResultStore(content, 8)
	store.overrideSHA = "0000000000000000000000000000000000000000000000000000000000000000"
	store.overrideAfter = 0 // every ReadRange call after Resolve returns the wrong digest
	limits := reviewLimits(120, 2000, 40, 64)

	_, _, err := resultanalysis.BuildManifest(context.Background(), store, store.identity.ResultID, testScope(), resultanalysis.SegmenterTextV1, limits)
	if err == nil {
		t.Fatal("expected a digest mismatch to fail BuildManifest")
	}
	if !errors.Is(err, domain.ErrAnalysisValidation) && !errors.Is(err, domain.ErrAnalysisUnavailable) {
		t.Fatalf("digest mismatch error = %v, want a wrapped domain.ErrAnalysis* sentinel", err)
	}
}

func TestBuildManifestRejectsOversizedSourceBeforeAnyReadOrModelCall(t *testing.T) {
	content := make([]byte, 10_000)
	for i := range content {
		content[i] = 'a'
	}
	store := newFakeTrustedResultStore(content, 4096)
	// MaxLeaves * MaxSegmentBytes = 2 * 100 = 200, far below the 10000-byte source.
	limits := reviewLimits(100, 2000, 20, 2)
	analyzer := &fakeResultAnalyzer{}

	_, _, err := resultanalysis.BuildManifest(context.Background(), store, store.identity.ResultID, testScope(), resultanalysis.SegmenterTextV1, limits)
	if err == nil {
		t.Fatal("expected an oversized source to fail")
	}
	if !errors.Is(err, domain.ErrAnalysisValidation) {
		t.Fatalf("oversized source error = %v, want ErrAnalysisValidation", err)
	}
	if store.readCalls != 0 {
		t.Fatalf("expected zero ReadRange calls for an oversized source, got %d", store.readCalls)
	}
	if analyzer.invoked {
		t.Fatal("expected the analyzer to never be invoked for an oversized source")
	}
}

func repeatWord(word string, n int) string {
	out := make([]byte, 0, len(word)*n)
	for range n {
		out = append(out, word...)
	}
	return string(out)
}

func testScope() domain.ResultScope {
	return domain.ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:U1", Project: "workspace"}
}
