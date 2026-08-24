package resultanalysis_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

// TestFIND100MarkdownScanCostReference is the reviewer's FIND-100
// measurement (docs/root-orchestrator-v2/hallazgos/tests-regresion-ronda1/perf-markdown.go.txt),
// incorporated verbatim. It is reference-only: FIND-100 (nextFenceOpen scans
// to EOF instead of stopping at limit, making segmentMarkdownBounds
// quadratic on a fence-free source) is accepted debt owned by TRD 07,
// removal condition checkpoint 3. This test asserts nothing about the
// measured ratio; it exists only to keep the reviewer's measurement runnable
// against future changes.
func TestFIND100MarkdownScanCostReference(t *testing.T) {
	line := []byte("una linea de prosa de largo razonable sin ningun fence\n")
	for _, size := range []int{256 << 10, 1 << 20, 4 << 20} {
		source := bytes.Repeat(line, size/len(line))
		limits := reviewLimits(24576, 1000, 4096, 512)

		start := time.Now()
		mdManifest, err := resultanalysis.Segment(resultanalysis.SegmenterMarkdownV1, source, limits)
		mdElapsed := time.Since(start)
		if err != nil {
			t.Fatalf("markdown %d: %v", size, err)
		}
		start = time.Now()
		txtManifest, err := resultanalysis.Segment(resultanalysis.SegmenterTextV1, source, limits)
		txtElapsed := time.Since(start)
		if err != nil {
			t.Fatalf("text %d: %v", size, err)
		}
		t.Logf("source=%7d bytes segs=%3d  markdown=%9s  text=%9s  ratio=%.0fx",
			len(source), len(mdManifest.Segments), mdElapsed, txtElapsed,
			float64(mdElapsed)/float64(txtElapsed))
		_ = txtManifest
	}
}
