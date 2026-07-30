package port

import (
	"context"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// ContextCompiler produces a bounded model-facing projection from a
// conversation ledger, externalizing oversized tool responses via a
// RecoverableResultStore without modifying the durable event ledger.
type ContextCompiler interface {
	Compile(ctx context.Context, req domain.CompileRequest) (domain.CompileResult, error)
}
