package slack

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/slack-go/slack"
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

type interactiveResponseAcker interface {
	AckResponse(context.Context, socketmode.Request, any) error
}

type viewOpener interface {
	OpenViewContext(context.Context, string, slack.ModalViewRequest) (*slack.ViewResponse, error)
}

type viewUpdater interface {
	UpdateViewContext(context.Context, slack.ModalViewRequest, string, string, string) (*slack.ViewResponse, error)
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

func (c sdkSocketClient) AckResponse(ctx context.Context, request socketmode.Request, payload any) error {
	return c.client.AckCtx(ctx, request.EnvelopeID, payload)
}

func (c sdkSocketClient) OpenViewContext(ctx context.Context, triggerID string, view slack.ModalViewRequest) (*slack.ViewResponse, error) {
	return c.client.OpenViewContext(ctx, triggerID, view)
}

func (c sdkSocketClient) UpdateViewContext(ctx context.Context, view slack.ModalViewRequest, externalID, hash, viewID string) (*slack.ViewResponse, error) {
	return c.client.UpdateViewContext(ctx, view, externalID, hash, viewID)
}

// Listener owns the Socket Mode lifecycle and its acknowledge-before-dispatch
// boundary. Handler work is launched asynchronously with the listener context.
type Listener struct {
	client             socketClient
	router             Router
	logger             port.Logger
	allowedUserIDs     []string
	interactiveHandler func(context.Context, domain.ConfirmationInteractiveAction) error
	builderPresenter   *BuilderModalPresenter
	builderHandler     *BuilderSubmissionHandler
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

func (l *Listener) SetInteractiveHandler(handler func(context.Context, domain.ConfirmationInteractiveAction) error) {
	if l == nil {
		return
	}
	l.interactiveHandler = handler
}

// WithAllowedUserIDs configures the users allowed to open the builder modal.
func (l *Listener) WithAllowedUserIDs(ids []string) *Listener {
	if l != nil {
		l.allowedUserIDs = append([]string(nil), ids...)
	}
	return l
}

// WithBuilderPresenter configures the modal opened by the builder action.
func (l *Listener) WithBuilderPresenter(p *BuilderModalPresenter) *Listener {
	if l != nil {
		l.builderPresenter = p
	}
	return l
}

// WithBuilderHandler configures the asynchronous builder submission handler.
func (l *Listener) WithBuilderHandler(h *BuilderSubmissionHandler) *Listener {
	if l != nil {
		l.builderHandler = h
	}
	return l
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

			switch event.Type {
			case socketmode.EventTypeInteractive:
				l.handleInteractive(runCtx, event, &handlers)
			case socketmode.EventTypeEventsAPI:
				l.handleEventsAPI(runCtx, event, &handlers, handler)
			}
		}
	}
}

func (l *Listener) handleInteractive(ctx context.Context, event socketmode.Event, handlers *sync.WaitGroup) {
	if event.Request == nil {
		l.logger.Warn("Slack interactive event ignored because its Socket Mode request is missing")
		return
	}

	callback, ok := event.Data.(slack.InteractionCallback)
	if !ok {
		if err := l.ackInteractive(ctx, *event.Request, nil); err != nil {
			l.logger.Error("Slack Socket Mode interactive acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
		}
		l.logger.Debug("unsupported Slack interactive payload ignored")
		return
	}

	if callback.Type == slack.InteractionTypeViewSubmission && callback.View.CallbackID == builderSubmitCallbackID {
		if l.builderHandler == nil {
			l.logger.Warn("builder submission handler not configured, ignoring builder submission")
			if err := l.ackInteractive(ctx, *event.Request, nil); err != nil {
				l.logger.Error("Slack Socket Mode builder acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
			}
			return
		}
		response := l.builderHandler.HandleSubmission(ctx, callback)
		if err := l.ackInteractive(ctx, *event.Request, response); err != nil {
			l.logger.Error("Slack Socket Mode builder acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
			if ctx.Err() != nil {
				return
			}
		}
		if response != nil {
			return
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			if err := l.builderHandler.PreviewAndPublish(ctx, callback); err != nil {
				l.logger.Warn("builder preview processing failed", "error", err)
			}
		}()
		return
	}

	if callback.Type == slack.InteractionTypeBlockActions {
		if callback.View.CallbackID == builderSubmitCallbackID && l.builderPresenter != nil {
			for _, action := range callback.ActionCallback.BlockActions {
				if action != nil && action.ActionID == "agent_type" {
					if err := l.ackInteractive(ctx, *event.Request, nil); err != nil {
						l.logger.Error("Slack Socket Mode builder acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
						if ctx.Err() != nil {
							return
						}
					}
					updater, ok := l.client.(viewUpdater)
					if !ok {
						l.logger.Error("Slack Socket Mode builder modal update failed", "error", "Slack view updater is not configured")
						return
					}
					view := l.builderPresenter.BuildViewForCallback(callback)
					if callback.View.ID == "" || callback.View.Hash == "" {
						l.logger.Error("Slack Socket Mode builder modal update failed", "error", "view ID and hash are required")
						return
					}
					if _, err := updater.UpdateViewContext(ctx, view, "", callback.View.Hash, callback.View.ID); err != nil {
						l.logger.Error("Slack Socket Mode builder modal update failed", "envelope_id", event.Request.EnvelopeID, "error", err)
					}
					return
				}
			}
		}
		for _, action := range callback.ActionCallback.BlockActions {
			if action != nil && action.ActionID == builderInstallActionID {
				if !domain.PlausibleUserID(callback.User.ID) || !domain.PlausibleTeamID(callback.Team.ID) || action.Value == "" {
					l.logger.Warn("builder install action rejected because authorization context is incomplete")
					if err := l.ackInteractive(ctx, *event.Request, nil); err != nil {
						l.logger.Error("Slack Socket Mode builder acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
					}
					return
				}
				if err := l.ackInteractive(ctx, *event.Request, nil); err != nil {
					l.logger.Error("Slack Socket Mode builder acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
					if ctx.Err() != nil {
						return
					}
				}
				if l.builderHandler == nil {
					l.logger.Warn("builder submission handler not configured, ignoring builder install action")
					return
				}
				draftID := action.Value
				handlers.Add(1)
				go func() {
					defer handlers.Done()
					if err := l.builderHandler.HandleInstallRequest(ctx, callback, draftID); err != nil {
						l.logger.Warn("builder install request processing failed", "error", err)
					}
				}()
				return
			}
			if action == nil || action.ActionID != "local_agent.builder.open" {
				continue
			}
			if callback.TriggerID == "" || !domain.PlausibleUserID(callback.User.ID) || !domain.PlausibleTeamID(callback.Team.ID) {
				l.logger.Warn("builder modal action rejected because authorization context is incomplete")
				if err := l.ackInteractive(ctx, *event.Request, nil); err != nil {
					l.logger.Error("Slack Socket Mode builder acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
				}
				return
			}
			if callback.User.ID != action.Value {
				l.logger.Warn("builder modal action rejected because the clicking user does not match the launcher actor", "actor", action.Value, "user", callback.User.ID)
				if err := l.ackInteractive(ctx, *event.Request, nil); err != nil {
					l.logger.Error("Slack Socket Mode builder acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
				}
				return
			}
			if !l.isAllowedBuilderUser(callback.User.ID) {
				l.logger.Warn("builder modal action rejected because the user is not an allowed administrator", "user", callback.User.ID)
				if err := l.ackInteractive(ctx, *event.Request, nil); err != nil {
					l.logger.Error("Slack Socket Mode builder acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
				}
				return
			}
			if err := l.ackInteractive(ctx, *event.Request, nil); err != nil {
				l.logger.Error("Slack Socket Mode builder acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
				if ctx.Err() != nil {
					return
				}
			}
			if l.builderPresenter == nil {
				l.logger.Warn("builder modal presenter not configured, ignoring builder action")
				return
			}
			opener, ok := l.client.(viewOpener)
			if !ok {
				l.logger.Error("open builder modal failed", "error", "Slack view opener is not configured")
				l.publishBuilderModalFallback(ctx, callback, handlers)
				return
			}
			if _, err := opener.OpenViewContext(ctx, callback.TriggerID, l.builderPresenter.BuildView()); err != nil {
				l.logger.Error("open builder modal", "error", err)
				l.publishBuilderModalFallback(ctx, callback, handlers)
			}
			return
		}
	}

	if err := l.ackInteractive(ctx, *event.Request, nil); err != nil {
		l.logger.Error("Slack Socket Mode interactive acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
		if ctx.Err() != nil {
			return
		}
	}

	action, ok := normalizeInteractiveAction(&callback)
	if !ok {
		l.logger.Debug("non-confirmation interactive action ignored", "action_id", callback.ActionID)
		return
	}

	if l.interactiveHandler == nil {
		l.logger.Warn("interactive handler not configured, ignoring confirmation action")
		return
	}

	handlers.Add(1)
	go func() {
		defer handlers.Done()
		if err := l.interactiveHandler(ctx, action); err != nil {
			l.logger.Warn("interactive handler returned error", "error", err)
		}
	}()
}

func (l *Listener) isAllowedBuilderUser(userID string) bool {
	if len(l.allowedUserIDs) == 0 {
		return true
	}
	for _, allowed := range l.allowedUserIDs {
		if allowed == userID {
			return true
		}
	}
	return false
}

func (l *Listener) ackInteractive(ctx context.Context, request socketmode.Request, payload any) error {
	if payload != nil {
		if acker, ok := l.client.(interactiveResponseAcker); ok {
			return acker.AckResponse(ctx, request, payload)
		}
	}
	return l.client.Ack(ctx, request)
}

func (l *Listener) publishBuilderModalFallback(ctx context.Context, callback slack.InteractionCallback, handlers *sync.WaitGroup) {
	if l.builderHandler == nil {
		return
	}
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		if err := l.builderHandler.publishModalFallback(ctx, callback); err != nil {
			l.logger.Warn("builder modal fallback failed", "error", err)
		}
	}()
}

func (l *Listener) handleEventsAPI(ctx context.Context, event socketmode.Event, handlers *sync.WaitGroup, handler func(context.Context, domain.Invocation)) {
	if event.Request == nil {
		l.logger.Warn("Slack event ignored because its Socket Mode request is missing")
		return
	}

	if err := l.client.Ack(ctx, *event.Request); err != nil {
		l.logger.Error("Slack Socket Mode acknowledgement failed", "envelope_id", event.Request.EnvelopeID, "error", err)
		if ctx.Err() != nil {
			return
		}
	}

	apiEvent, ok := event.Data.(slackevents.EventsAPIEvent)
	if !ok {
		l.logger.Debug("unsupported Slack Events API payload ignored")
		return
	}
	invocation, ok := l.router.Route(apiEvent)
	if !ok {
		l.logger.Debug("unsupported Slack event ignored", "event_type", apiEvent.InnerEvent.Type)
		return
	}

	handlers.Add(1)
	go func() {
		defer handlers.Done()
		handler(ctx, invocation)
	}()
}
