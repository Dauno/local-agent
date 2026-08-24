package sqlite

import (
	"context"

	"github.com/Dauno/slack-local-agent/internal/port"
)

type exchangeFinder struct {
	intentID      string
	correlationID string
	content       string
	timestamp     string
}

func (f exchangeFinder) FindPublishedAssistantExchange(_ context.Context, intent port.AssistantExchangeIntent) (string, bool, error) {
	if f.intentID != "" && f.intentID != intent.ID {
		return "", false, nil
	}
	if f.correlationID != "" && f.correlationID != intent.CorrelationID {
		return "", false, nil
	}
	if f.content != "" && intent.Content != f.content {
		return "", false, nil
	}
	return f.timestamp, f.timestamp != "", nil
}
