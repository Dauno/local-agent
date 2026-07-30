package port

import (
	"context"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// ServerCandidate describes a language server to probe for availability.
type ServerCandidate struct {
	ID        string
	Command   string
	Args      []string
	Languages []string
}

// LanguageServerDiscovery discovers which language server binaries are
// available on the current system.
type LanguageServerDiscovery interface {
	Discover(ctx context.Context, candidates []ServerCandidate) ([]domain.LanguageServerDescriptor, error)
}

// CodeIntelligence provides LSP-backed code navigation queries.
type CodeIntelligence interface {
	Symbols(ctx context.Context, req domain.SymbolRequest) (domain.SymbolResult, error)
	Definition(ctx context.Context, req domain.LocationRequest) (domain.LocationResult, error)
	References(ctx context.Context, req domain.LocationRequest) (domain.LocationResult, error)
}
