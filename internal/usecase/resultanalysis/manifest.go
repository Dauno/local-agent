package resultanalysis

import (
	"context"
	"fmt"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// readChunkMaxBytes bounds one ReadRange call. It is well under every
// adapter's own per-call chunk cap, so BuildManifest and ResolveEvidence
// always loop to assemble a full range rather than assuming one call
// returns it: an adapter is always free to serve less than requested.
const readChunkMaxBytes = 1 << 20

// BuildManifest reads a verified source through port.TrustedResultStore
// only, never through direct storage or filesystem access, and produces its
// deterministic segment manifest. A source whose declared size already
// exceeds limits.MaxLeaves*limits.MaxSegmentBytes fails typed with
// domain.AnalysisFailureSourceTooLarge before a single byte is read, so an
// oversized source never reaches the model. The assembled source's digest
// is checked against the resolved identity before segmenting.
func BuildManifest(ctx context.Context, store port.TrustedResultStore, resultID string, scope domain.ResultScope, segmenterVersion string, limits domain.AnalysisLimits) (domain.AnalysisSegmentManifest, domain.ResultIdentity, error) {
	var zero domain.AnalysisSegmentManifest
	if store == nil {
		return zero, domain.ResultIdentity{}, fmt.Errorf("%w: manifest build requires a source store", domain.ErrAnalysisUnavailable)
	}
	if err := limits.Validate(); err != nil {
		return zero, domain.ResultIdentity{}, err
	}
	identity, _, err := store.Resolve(ctx, resultID, scope)
	if err != nil {
		return zero, domain.ResultIdentity{}, err
	}
	maxSourceBytes := limits.MaxSegmentBytes * int64(limits.MaxLeaves)
	if identity.Bytes > maxSourceBytes {
		return zero, identity, fmt.Errorf("%w: %s: source is %d bytes, more than the configured maximum %d",
			domain.ErrAnalysisValidation, domain.AnalysisFailureSourceTooLarge, identity.Bytes, maxSourceBytes)
	}

	source, err := readFullRange(ctx, store, resultID, scope, identity.SHA256, 0, identity.Bytes)
	if err != nil {
		return zero, identity, err
	}
	if int64(len(source)) != identity.Bytes {
		return zero, identity, fmt.Errorf("%w: assembled source length does not match the resolved identity", domain.ErrAnalysisUnavailable)
	}
	if hexSHA256([]byte(source)) != identity.SHA256 {
		return zero, identity, fmt.Errorf("%w: assembled source digest does not match the resolved identity", domain.ErrAnalysisUnavailable)
	}

	manifest, err := Segment(segmenterVersion, []byte(source), limits)
	if err != nil {
		return zero, identity, err
	}
	return manifest, identity, nil
}

// readFullRange assembles [offset, offset+length) from store by looping
// ReadRange until EOF or the requested length is reached, verifying the
// full-source digest that every chunk carries against expectedSHA256 on
// every call so a mid-read source change is caught immediately rather than
// only at the final digest check.
func readFullRange(ctx context.Context, store port.TrustedResultStore, resultID string, scope domain.ResultScope, expectedSHA256 string, offset, length int64) (string, error) {
	if length == 0 {
		return "", nil
	}
	var assembled []byte
	for int64(len(assembled)) < length {
		remaining := length - int64(len(assembled))
		chunkMax := min(remaining, readChunkMaxBytes)
		chunk, err := store.ReadRange(ctx, resultID, scope, offset+int64(len(assembled)), chunkMax)
		if err != nil {
			return "", err
		}
		if expectedSHA256 != "" && chunk.SHA256 != "" && chunk.SHA256 != expectedSHA256 {
			return "", fmt.Errorf("%w: source digest changed mid-read", domain.ErrAnalysisValidation)
		}
		if chunk.NextOffsetBytes <= offset+int64(len(assembled)) && chunk.Content == "" {
			if chunk.EOF {
				break
			}
			return "", fmt.Errorf("%w: source read made no forward progress", domain.ErrAnalysisUnavailable)
		}
		assembled = append(assembled, chunk.Content...)
		if chunk.EOF {
			break
		}
	}
	return string(assembled), nil
}
