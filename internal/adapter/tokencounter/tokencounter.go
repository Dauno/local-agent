// Package tokencounter provides token counting strategies for model request
// budget enforcement. The byte_bound strategy treats each byte of the
// serialized request as a token approximation; the estimator strategy adds a
// versioned visual cost for multimodal requests.
package tokencounter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/port"
)

// Counter strategy and estimator identifiers. These are the only combinations
// this release implements; unknown or unimplemented combinations fail at
// startup and in doctor, never with a silent fallback (FR-15).
const (
	StrategyByteBound                 = "byte_bound"
	StrategyEstimator                 = "estimator"
	EstimatorVisualTileConservativeV1 = "visual-tile-conservative-v1"
)

var (
	ErrUnsupportedStrategy  = errors.New("unsupported token counter strategy")
	ErrUnsupportedCounterID = errors.New("unsupported token counter id")
	// ErrMediaNotCountable is returned by byte_bound when a request carries
	// media it cannot value (FR-16). It is never silently downgraded to a
	// base64 count or a zero-cost media estimate.
	ErrMediaNotCountable = errors.New("media requests require a visual estimator counter")
)

// visualTileBase and visualTileScale are the conservative per-image cost
// constants of visual-tile-conservative-v1 (FR-14). low=1024; auto/omitted/
// high = 1024 + 1024*ceil(width/512)*ceil(height/512). This is local
// admission policy, not a provider billing formula.
const (
	visualTileBase  = 1024
	visualTileScale = 1024
	visualTileEdge  = 512
)

var (
	_ port.RequestTokenCounter = (*byteBoundCounter)(nil)
	_ port.RequestTokenCounter = (*visualTileCounter)(nil)
)

// byteBoundCounter implements port.RequestTokenCounter by treating each byte
// of the serialized request as one token. It cannot value media.
type byteBoundCounter struct{}

func (c *byteBoundCounter) CountRequest(ctx context.Context, envelope port.ModelRequestEnvelope) (port.TokenCount, error) {
	if err := ctx.Err(); err != nil {
		return port.TokenCount{}, err
	}
	if len(envelope.Media) > 0 {
		return port.TokenCount{}, ErrMediaNotCountable
	}
	tokens := len(envelope.Serialized)
	return port.TokenCount{
		Tokens:   tokens,
		Strategy: StrategyByteBound,
		Exact:    false,
	}, nil
}

// visualTileCounter implements the versioned visual estimator: byte-bound of
// the countable payload plus the conservative tile cost of every image.
type visualTileCounter struct{}

func (c *visualTileCounter) CountRequest(ctx context.Context, envelope port.ModelRequestEnvelope) (port.TokenCount, error) {
	if err := ctx.Err(); err != nil {
		return port.TokenCount{}, err
	}
	switch envelope.SerializerID {
	case port.SerializerContextProjectionV1:
		if len(envelope.Media) > 0 {
			return port.TokenCount{}, fmt.Errorf("estimator %s cannot count media in serializer %q", EstimatorVisualTileConservativeV1, envelope.SerializerID)
		}
		return port.TokenCount{Tokens: len(envelope.Serialized), Strategy: StrategyByteBound, Exact: false}, nil
	case port.SerializerOpenAIChatCompletionsV1:
		// Text-only requests are counted exactly like byte_bound; media in a
		// v1 envelope means the conversion never produced the countable shape.
		if len(envelope.Media) > 0 {
			return port.TokenCount{}, fmt.Errorf("estimator %s cannot count media in serializer %q", EstimatorVisualTileConservativeV1, envelope.SerializerID)
		}
	case port.SerializerOpenAIChatCompletionsMultimodalV2:
	default:
		return port.TokenCount{}, fmt.Errorf("estimator %s cannot count serializer %q", EstimatorVisualTileConservativeV1, envelope.SerializerID)
	}
	total := int64(len(envelope.Serialized))
	for index, media := range envelope.Media {
		cost, err := visualTileTokens(media)
		if err != nil {
			return port.TokenCount{}, fmt.Errorf("media %d: %w", index, err)
		}
		if cost < 0 || total > int64(math.MaxInt)-int64(cost) {
			return port.TokenCount{}, errors.New("multimodal token estimate overflows")
		}
		total += int64(cost)
		if total < 0 {
			return port.TokenCount{}, errors.New("multimodal token estimate overflows")
		}
	}
	if total < 0 || total > int64(math.MaxInt) {
		return port.TokenCount{}, errors.New("multimodal token estimate overflows")
	}
	return port.TokenCount{
		Tokens:   int(total),
		Strategy: StrategyEstimator,
		Exact:    false,
	}, nil
}

// visualTileTokens computes the conservative cost of one image according to
// its effective detail. Width, height, and MIME are validated fail-closed;
// unknown detail values are rejected rather than guessed (FR-14, FR-16).
func visualTileTokens(media port.ModelRequestMedia) (int, error) {
	if media.Width <= 0 || media.Height <= 0 {
		return 0, fmt.Errorf("image dimensions must be positive, got %dx%d", media.Width, media.Height)
	}
	if !supportedImageMIME(media.MIMEType) {
		return 0, fmt.Errorf("image MIME type %q is not supported", media.MIMEType)
	}
	switch media.Detail {
	case "low":
		return visualTileBase, nil
	case "", "auto", "high":
		tilesX := ceilDiv(media.Width, visualTileEdge)
		tilesY := ceilDiv(media.Height, visualTileEdge)
		if tilesX > math.MaxInt/tilesY {
			return 0, errors.New("visual tile estimate overflows")
		}
		tiles := tilesX * tilesY
		if tiles > (math.MaxInt-visualTileBase)/visualTileScale {
			return 0, errors.New("visual tile estimate overflows")
		}
		scaledTiles := visualTileScale * tiles
		if scaledTiles > math.MaxInt-visualTileBase {
			return 0, errors.New("visual tile estimate overflows")
		}
		return visualTileBase + scaledTiles, nil
	default:
		return 0, fmt.Errorf("unknown image detail %q", media.Detail)
	}
}

func ceilDiv(value, divisor int) int {
	quotient := value / divisor
	if value%divisor != 0 {
		quotient++
	}
	return quotient
}

func supportedImageMIME(mimeType string) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

// New returns a port.RequestTokenCounter for the given strategy and estimator
// ID. byte_bound needs no ID; estimator requires the exact ID of an
// implemented formula. Any other combination fails without a fallback.
func New(strategy, id string) (port.RequestTokenCounter, error) {
	switch strategy {
	case StrategyByteBound:
		if strings.TrimSpace(id) != "" {
			return nil, fmt.Errorf("%w: byte_bound id must be empty", ErrUnsupportedCounterID)
		}
		return &byteBoundCounter{}, nil
	case StrategyEstimator:
		switch id {
		case EstimatorVisualTileConservativeV1:
			return &visualTileCounter{}, nil
		default:
			return nil, fmt.Errorf("%w: estimator id %q", ErrUnsupportedCounterID, id)
		}
	default:
		return nil, fmt.Errorf("%w: strategy %q", ErrUnsupportedStrategy, strategy)
	}
}
