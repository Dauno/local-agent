package slack

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type socketClient interface {
	Run(context.Context) error
	Events() <-chan socketmode.Event
	Ack(context.Context, socketmode.Request) error
}

type sdkSocketClient struct {
	client *socketmode.Client
}

func (c sdkSocketClient) Run(ctx context.Context) error {
	return c.client.RunContext(ctx)
}

func (c sdkSocketClient) Events() <-chan socketmode.Event {
	return c.client.Events
}

func (c sdkSocketClient) Ack(ctx context.Context, request socketmode.Request) error {
	return c.client.AckCtx(ctx, request.EnvelopeID, nil)
}

// Listener owns the Socket Mode lifecycle and its acknowledge-before-dispatch
// boundary. Handler work is launched asynchronously with the listener context.
type Listener struct {
	client socketClient
	router Router
	logger port.Logger
}

func NewListener(client *socketmode.Client, router Router, logger port.Logger) *Listener {
	var socket socketClient
	if client != nil {
		socket = sdkSocketClient{client: client}
	}
	return newListener(socket, router, logger)
}

func newListener(client socketClient, router Router, logger port.Logger) *Listener {
	return &Listener{client: client, router: router, logger: loggerOrDiscard(logger)}
}

// Run blocks until the context is canceled or the Socket Mode client stops.
// Context cancellation is a normal shutdown and returns nil.
func (l *Listener) Run(ctx context.Context, handler func(context.Context, domain.Invocation)) error {
	if l == nil || l.client == nil {
		return errors.New("Socket Mode client is required")
	}
	if handler == nil {
		return errors.New("Slack invocation handler is required")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	runResult := make(chan error, 1)
	go func() {
		runResult <- l.client.Run(runCtx)
	}()

	var handlers sync.WaitGroup
	waitHandlers := func() { handlers.Wait() }

	for {
		select {
		case <-ctx.Done():
			cancel()
			waitHandlers()
			err := <-runResult
			if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ctx.Err()) {
				return nil
			}
			return fmt.Errorf("run Slack Socket Mode client: %w", err)

		case err := <-runResult:
			cancel()
			waitHandlers()
			if err == nil || (ctx.Err() != nil && errors.Is(err, context.Canceled)) {
				return nil
			}
			return fmt.Errorf("run Slack Socket Mode client: %w", err)

		case event, open := <-l.client.Events():
			if !open {
				cancel()
				waitHandlers()
				err := <-runResult
				if err == nil || errors.Is(err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("run Slack Socket Mode client: %w", err)
			}
			if event.Type != socketmode.EventTypeEventsAPI {
				continue
			}
			if event.Request == nil {
				l.logger.Warn("Slack event ignored because its Socket Mode request is missing")
				continue
			}

			// Acknowledge every Events API envelope before parsing or dispatching it.
			if err := l.client.Ack(runCtx, *event.Request); err != nil {
				l.logger.Error("Slack Socket Mode acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
				if runCtx.Err() != nil {
					continue
				}
			}

			apiEvent, ok := event.Data.(slackevents.EventsAPIEvent)
			if !ok {
				l.logger.Debug("unsupported Slack Events API payload ignored")
				continue
			}
			invocation, ok := l.router.Route(apiEvent)
			if !ok {
				l.logger.Debug("unsupported Slack event ignored", "event_type", apiEvent.InnerEvent.Type)
				continue
			}

			handlers.Add(1)
			go func() {
				defer handlers.Done()
				handler(runCtx, invocation)
			}()
		}
	}
}
