package port

import (
	"context"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// CodeReader reads a line range from a text file within a registered project root.
type CodeReader interface {
	ReadRange(ctx context.Context, req domain.SourceRangeRequest) (domain.SourceRange, error)
}
