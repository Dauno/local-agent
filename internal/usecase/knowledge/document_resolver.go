package knowledge

import (
	"context"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// UnavailableDocumentResolver is a test seam for workers that process only
// claim or preference items. Production composition uses the SQLite resolver.
type UnavailableDocumentResolver struct{}

func (UnavailableDocumentResolver) Resolve(context.Context, domain.KnowledgeDocument, domain.KnowledgeRetrievalLimits) ([]byte, error) {
	return nil, port.ErrKnowledgeUnavailable
}

var _ port.KnowledgeDocumentResolver = UnavailableDocumentResolver{}
