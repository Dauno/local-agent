package slack

import (
	"context"
	"fmt"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/port"
)

type CanvasCreator struct {
	client  *slackapi.Client
	timeout time.Duration
}

func NewCanvasCreator(client *slackapi.Client, timeout time.Duration) *CanvasCreator {
	return &CanvasCreator{client: client, timeout: timeout}
}

func (c *CanvasCreator) CreateCanvas(ctx context.Context, title string, documentContent string) (port.CanvasCreateResult, error) {
	if c == nil || c.client == nil {
		return port.CanvasCreateResult{}, fmt.Errorf("canvas client is not configured")
	}
	ctx, cancel := slackTimeout(ctx, c.timeout)
	defer cancel()
	canvasID, err := c.client.CreateCanvasContext(ctx, title, slackapi.DocumentContent{
		Type:     "markdown",
		Markdown: documentContent,
	})
	if err != nil {
		ambiguous := true
		if v, matched := ambiguousSlackError(err); matched {
			ambiguous = v
		}
		return port.CanvasCreateResult{}, &port.CanvasCreateError{
			Err: fmt.Errorf("create Slack canvas: %w", err), Ambiguous: ambiguous,
		}
	}
	return port.CanvasCreateResult{CanvasID: canvasID}, nil
}

var _ port.CanvasCreator = (*CanvasCreator)(nil)
