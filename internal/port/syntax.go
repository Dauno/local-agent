package port

import (
	"context"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// SyntaxEngine executes structure-aware queries over source files.
type SyntaxEngine interface {
	Query(ctx context.Context, req domain.SyntaxQueryRequest) (domain.SyntaxQueryResult, error)
}
