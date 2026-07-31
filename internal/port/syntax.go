package port

import (
	"context"
	"errors"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

var (
	ErrSyntaxUnsupportedLanguage = errors.New("syntax language is unsupported")
	ErrSyntaxUnsupportedQuery    = errors.New("syntax query is unsupported")
	ErrSyntaxSourceTooLarge      = errors.New("syntax source exceeds inspection limit")
	ErrSyntaxParseFailed         = errors.New("syntax source is malformed")
	ErrSyntaxProjectUnavailable  = errors.New("syntax project is unavailable")
)

// SyntaxEngine executes structure-aware queries over source files.
type SyntaxEngine interface {
	Query(ctx context.Context, req domain.SyntaxQueryRequest) (domain.SyntaxQueryResult, error)
}
