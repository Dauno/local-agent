// Package tokencounter provides token counting strategies for model request
// budget enforcement. The byte_bound strategy treats each byte of the
// serialized request as a token approximation.
package tokencounter

import (
	"context"
	"errors"

	"github.com/Dauno/slack-local-agent/internal/port"
)

var ErrUnsupportedStrategy = errors.New("unsupported token counter strategy")

var _ port.RequestTokenCounter = (*byteBoundCounter)(nil)

// byteBoundCounter implements port.RequestTokenCounter by treating each byte
// of the serialized request as one token.
type byteBoundCounter struct{}

func (c *byteBoundCounter) CountRequest(ctx context.Context, envelope port.ModelRequestEnvelope) (port.TokenCount, error) {
	if err := ctx.Err(); err != nil {
		return port.TokenCount{}, err
	}

	tokens := len(envelope.Serialized)
	return port.TokenCount{
		Tokens:   tokens,
		Strategy: "byte_bound",
		Exact:    false,
	}, nil
}

// New returns a port.RequestTokenCounter for the given strategy.
// Supported strategies: "byte_bound".
func New(strategy string) (port.RequestTokenCounter, error) {
	switch strategy {
	case "byte_bound":
		return &byteBoundCounter{}, nil
	default:
		return nil, ErrUnsupportedStrategy
	}
}
