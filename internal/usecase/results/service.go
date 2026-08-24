package results

import (
	"context"
	"errors"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// Service is the trusted producer boundary. It redacts and control-sanitizes
// text before a catalog can calculate its bytes or digest.
type Service struct {
	store  port.TrustedResultStore
	redact func(string) string
}

func New(store port.TrustedResultStore, redact func(string) string) (*Service, error) {
	if store == nil || redact == nil {
		return nil, errors.New("result materializer requires a store and redactor")
	}
	return &Service{store: store, redact: redact}, nil
}

func (s *Service) Materialize(ctx context.Context, request port.ResultMaterialization) (domain.ResultHandle, error) {
	if s == nil || s.store == nil || s.redact == nil {
		return domain.ResultHandle{}, domain.ErrResultUnavailable
	}
	sanitized, err := domain.SanitizeResultText(s.redact(request.Payload))
	if err != nil {
		return domain.ResultHandle{}, domain.ErrResultInvalid
	}
	request.Payload = sanitized
	return s.store.Materialize(ctx, request)
}
