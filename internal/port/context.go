package port

import (
	"context"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// ContextProjector creates a bounded model-facing projection without changing
// the durable session or its event ledger.
type ContextProjector interface {
	Project(context.Context, domain.CompactionRequest) (domain.CompactionResult, error)
}
