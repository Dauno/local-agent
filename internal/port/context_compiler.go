package port

import (
	"context"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// ContextCompiler produces a bounded, read-only model-facing projection from a
// conversation ledger without modifying durable events or result storage.
type ContextCompiler interface {
	Compile(ctx context.Context, req domain.CompileRequest) (domain.CompileResult, error)
}

// ContextFrameCounter counts a candidate frame after its owning adapter has
// applied the same provider request conversion used by its final guard.
type ContextFrameCounter interface {
	CountContextFrame(ctx context.Context, contents []domain.Content) (TokenCount, error)
}

// ContextFrameCompiler is an optional exact-counting extension. Callers that
// cannot construct a provider-shaped candidate retain ContextCompiler fallback
// behavior.
type ContextFrameCompiler interface {
	ContextCompiler
	CompileFrame(ctx context.Context, req domain.CompileRequest, counter ContextFrameCounter) (domain.CompileResult, error)
}
