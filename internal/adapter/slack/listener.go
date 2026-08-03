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
	responsePublisher  port.ResponsePublisher
	builderPresenter   *BuilderModalPresenter
	builderHandler     *BuilderSubmissionHandler
	dispatcher         *InteractiveDispatcher
	dispatcherErr      error
}

func NewListener(client *socketmode.Client, router Router, logger port.Logger) *Listener {
	var socket socketClient
	if client != nil {
		socket = sdkSocketClient{client: client}
	}
	return newListener(socket, router, logger)
}

func newListener(client socketClient, router Router, logger port.Logger) *Listener {
	listener := &Listener{client: client, router: router, logger: loggerOrDiscard(logger)}
	listener.dispatcher, listener.dispatcherErr = listener.BuildInteractiveDispatcher()
	return listener
}

func (l *Listener) SetInteractiveHandler(handler func(context.Context, domain.ConfirmationInteractiveAction) error) {
	if l == nil {
		return
	}
	l.interactiveHandler = handler
}

// WithResponsePublisher configures deterministic responses for app-owned
// interactive actions that must not enter the model flow.
func (l *Listener) WithResponsePublisher(publisher port.ResponsePublisher) *Listener {
	if l != nil {
		l.responsePublisher = publisher
	}
	return l
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

// WithDispatcher replaces the listener's immutable dispatcher. It is useful
// for composition and hermetic routing tests; the dispatcher itself cannot be
// modified after construction.
func (l *Listener) WithDispatcher(dispatcher *InteractiveDispatcher) *Listener {
	if l != nil {
		l.dispatcher = dispatcher
		l.dispatcherErr = nil
	}
	return l
}

// BuildInteractiveDispatcher constructs the listener-owned registration tables
// after composition has supplied its handlers and dependencies.
func (l *Listener) BuildInteractiveDispatcher() (*InteractiveDispatcher, error) {
	if l == nil {
		return nil, errors.New("Slack listener is required for dispatcher construction")
	}
	return newListenerDispatcher(l)
}

// ValidateTemplateCatalog checks startup coverage before Listener.Run starts
// receiving Socket Mode events.
func (l *Listener) ValidateTemplateCatalog(catalog *TemplateCatalog) error {
	if l == nil {
		return errors.New("Slack listener is required for template validation")
	}
	if l.dispatcherErr != nil {
		return fmt.Errorf("initialize Slack interactive dispatcher: %w", l.dispatcherErr)
	}
	if l.dispatcher == nil {
		return errors.New("Slack interactive dispatcher is required")
	}
	return catalog.ValidateDispatcher(l.dispatcher)
}

// Run blocks until the context is canceled or the Socket Mode client stops.
// Context cancellation is a normal shutdown and returns nil.
func (l *Listener) Run(ctx context.Context, handler func(context.Context, domain.Invocation)) error {
	return l.RunWithHandlerContext(ctx, ctx, handler)
}

// RunWithHandlerContext stops Socket Mode intake with intakeCtx while allowing
// already admitted handlers to remain alive under handlerCtx during drain.
func (l *Listener) RunWithHandlerContext(intakeCtx, handlerCtx context.Context, handler func(context.Context, domain.Invocation)) error {
	if l == nil || l.client == nil {
		return errors.New("Socket Mode client is required")
	}
	if handler == nil {
		return errors.New("Slack invocation handler is required")
	}
	if intakeCtx == nil || handlerCtx == nil {
		return errors.New("Slack intake and handler contexts are required")
	}

	runCtx, cancel := context.WithCancel(intakeCtx)
	defer cancel()

	runResult := make(chan error, 1)
	go func() {
		runResult <- l.client.Run(runCtx)
	}()

	var handlers sync.WaitGroup
	waitHandlers := func() { handlers.Wait() }

	for {
		select {
		case <-intakeCtx.Done():
			cancel()
			waitHandlers()
			err := <-runResult
			if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, intakeCtx.Err()) {
				return nil
			}
			return fmt.Errorf("run Slack Socket Mode client: %w", err)

		case err := <-runResult:
			cancel()
			waitHandlers()
			if err == nil || (intakeCtx.Err() != nil && errors.Is(err, context.Canceled)) {
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
				l.handleInteractive(runCtx, handlerCtx, event, &handlers)
			case socketmode.EventTypeEventsAPI:
				l.handleEventsAPI(runCtx, handlerCtx, event, &handlers, handler)
			}
		}
	}
}

func (l *Listener) handleInteractive(ctx, handlerCtx context.Context, event socketmode.Event, handlers *sync.WaitGroup) {
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

	switch callback.Type {
	case slack.InteractionTypeViewSubmission:
		l.handleViewSubmission(ctx, handlerCtx, *event.Request, callback, handlers)
	case slack.InteractionTypeBlockActions:
		l.handleBlockActions(ctx, handlerCtx, *event.Request, callback, handlers)
	default:
		l.ackUnsupportedInteractive(ctx, *event.Request, "event", string(callback.Type))
	}
}

func (l *Listener) handleViewSubmission(ctx, handlerCtx context.Context, request socketmode.Request, callback slack.InteractionCallback, handlers *sync.WaitGroup) {
	if l.dispatcherErr != nil || l.dispatcher == nil {
		l.ackUnsupportedInteractive(ctx, request, "view", callback.View.CallbackID)
		return
	}
	result, err := l.dispatcher.HandleView(handlerCtx, callback.View.CallbackID, callback)
	if err != nil {
		l.ackUnsupportedInteractive(ctx, request, "view", callback.View.CallbackID)
		return
	}
	if err := l.ackInteractive(ctx, request, result.Response); err != nil {
		l.logger.Error("Slack Socket Mode builder acknowledgement failed", "envelope_id", request.EnvelopeID, "error", err)
		if ctx.Err() != nil {
			return
		}
	}
	if result.Response != nil || result.Effect == nil {
		return
	}
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		if err := result.Effect(handlerCtx); err != nil {
			l.logger.Warn("builder preview processing failed", "error", err)
		}
	}()
}

func (l *Listener) handleBlockActions(ctx, handlerCtx context.Context, request socketmode.Request, callback slack.InteractionCallback, handlers *sync.WaitGroup) {
	if l.dispatcherErr != nil || l.dispatcher == nil {
		l.ackUnsupportedInteractive(ctx, request, "action", "")
		return
	}
	if callback.View.CallbackID != "" && !l.dispatcher.HasView(callback.View.CallbackID) {
		l.ackUnsupportedInteractive(ctx, request, "callback", callback.View.CallbackID)
		return
	}
	actionID, found := l.dispatchActionID(callback)
	if !found {
		l.ackUnsupportedInteractive(ctx, request, "action", actionID)
		return
	}
	if err := l.ackInteractive(ctx, request, nil); err != nil {
		l.logger.Error("Slack Socket Mode interactive acknowledgement failed", "envelope_id", request.EnvelopeID, "error", err)
		if ctx.Err() != nil {
			return
		}
	}
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		if err := l.dispatcher.HandleAction(handlerCtx, actionID, callback); err != nil {
			if errors.Is(err, ErrUnsupportedInteractive) {
				l.logger.Warn("unsupported Slack interactive action ignored")
				return
			}
			l.logger.Warn("Slack interactive action handler failed", "error", err)
		}
	}()
}

func (l *Listener) dispatchActionID(callback slack.InteractionCallback) (string, bool) {
	if len(callback.ActionCallback.BlockActions) != 1 || callback.ActionCallback.BlockActions[0] == nil {
		return "", false
	}
	actionID := callback.ActionCallback.BlockActions[0].ActionID
	if actionID == "" || !l.dispatcher.HasAction(actionID) {
		return actionID, false
	}
	return actionID, true
}

func (l *Listener) handleBuilderSubmission(ctx context.Context, callback slack.InteractionCallback) (ViewDispatchResult, error) {
	if l.builderHandler == nil {
		l.logger.Warn("builder submission handler not configured, ignoring builder submission")
		return ViewDispatchResult{}, nil
	}
	response := l.builderHandler.HandleSubmission(ctx, callback)
	if response != nil {
		return ViewDispatchResult{Response: response}, nil
	}
	return ViewDispatchResult{
		Effect: func(effectCtx context.Context) error {
			return l.builderHandler.PreviewAndPublish(effectCtx, callback)
		},
	}, nil
}

func (l *Listener) handleBuilderTypeAction(ctx context.Context, callback slack.InteractionCallback) error {
	if callback.View.CallbackID != builderSubmitCallbackID || l.builderPresenter == nil {
		return nil
	}
	updater, ok := l.client.(viewUpdater)
	if !ok {
		l.logger.Error("Slack Socket Mode builder modal update failed", "error", "Slack view updater is not configured")
		return nil
	}
	view, err := l.builderPresenter.BuildViewForCallbackResult(callback)
	if err != nil {
		return fmt.Errorf("render builder modal update: %w", err)
	}
	if callback.View.ID == "" || callback.View.Hash == "" {
		l.logger.Error("Slack Socket Mode builder modal update failed", "error", "view ID and hash are required")
		return nil
	}
	if _, err := updater.UpdateViewContext(ctx, view, "", callback.View.Hash, callback.View.ID); err != nil {
		l.logger.Error("Slack Socket Mode builder modal update failed", "error", err)
	}
	return nil
}

func (l *Listener) handleBuilderInstallAction(ctx context.Context, callback slack.InteractionCallback) error {
	draftID, _ := blockActionValue(callback, builderInstallActionID)
	if !domain.PlausibleUserID(callback.User.ID) || !domain.PlausibleTeamID(callback.Team.ID) || draftID == "" {
		l.logger.Warn("builder install action rejected because authorization context is incomplete")
		return nil
	}
	if l.builderHandler == nil {
		l.logger.Warn("builder submission handler not configured, ignoring builder install action")
		return nil
	}
	if err := l.builderHandler.HandleInstallRequest(ctx, callback, draftID); err != nil {
		l.logger.Warn("builder install request processing failed", "error", err)
	}
	return nil
}

func (l *Listener) handleBuilderOpenAction(ctx context.Context, callback slack.InteractionCallback) error {
	if callback.TriggerID == "" || !domain.PlausibleUserID(callback.User.ID) || !domain.PlausibleTeamID(callback.Team.ID) {
		l.logger.Warn("builder modal action rejected because authorization context is incomplete")
		return nil
	}
	metadata, _, contextErr := builderActionContext(callback, blockActionValueOrEmpty(callback, "local_agent.builder.open"))
	if contextErr != nil {
		l.logger.Warn("builder modal action rejected because its conversation context is invalid", "error", contextErr)
		return nil
	}
	if !l.isAllowedBuilderUser(callback.User.ID) {
		l.logger.Warn("builder modal action rejected because the user is not an allowed administrator", "user", callback.User.ID)
		return nil
	}
	if l.builderPresenter == nil {
		l.logger.Warn("builder modal presenter not configured, ignoring builder action")
		return nil
	}
	opener, ok := l.client.(viewOpener)
	if !ok {
		l.logger.Error("open builder modal failed", "error", "Slack view opener is not configured")
		l.publishBuilderModalFallback(ctx, callback, nil)
		return nil
	}
	view, err := l.builderPresenter.BuildViewResult()
	if err != nil {
		l.logger.Error("render builder modal", "error", err)
		l.publishBuilderModalFallback(ctx, callback, nil)
		return nil
	}
	view.PrivateMetadata = metadata
	if _, err := opener.OpenViewContext(ctx, callback.TriggerID, view); err != nil {
		l.logger.Error("open builder modal", "error", err)
		l.publishBuilderModalFallback(ctx, callback, nil)
	}
	return nil
}

func (l *Listener) handleConfirmationAction(ctx context.Context, callback slack.InteractionCallback) error {
	if len(callback.ActionCallback.BlockActions) != 1 || callback.ActionCallback.BlockActions[0] == nil {
		return ErrMalformedInteractive
	}
	action, ok := normalizeInteractiveAction(&callback)
	if !ok {
		return ErrMalformedInteractive
	}
	if l.interactiveHandler == nil {
		l.logger.Warn("interactive handler not configured, ignoring confirmation action")
		return nil
	}
	if err := l.interactiveHandler(ctx, action); err != nil {
		l.logger.Warn("interactive handler returned error", "error", err)
	}
	return nil
}

func (l *Listener) handleOnboardingDescribeAction(ctx context.Context, callback slack.InteractionCallback) error {
	if !domain.PlausibleUserID(callback.User.ID) || !domain.PlausibleTeamID(callback.Team.ID) {
		l.logger.Warn("onboarding describe action rejected because authorization context is incomplete")
		return nil
	}
	if l.responsePublisher == nil {
		l.logger.Warn("onboarding describe action ignored because response publisher is not configured")
		return nil
	}
	_, target, err := builderActionContext(callback, blockActionValueOrEmpty(callback, "local_agent.onboarding.describe"))
	if err != nil {
		l.logger.Warn("onboarding describe action rejected because its conversation context is invalid", "error", err)
		return nil
	}
	if _, err := l.responsePublisher.Publish(ctx, target, onboardingDescribePrompt); err != nil {
		return fmt.Errorf("publish onboarding describe guidance: %w", err)
	}
	return nil
}

func blockActionValue(callback slack.InteractionCallback, actionID string) (string, bool) {
	for _, action := range callback.ActionCallback.BlockActions {
		if action != nil && action.ActionID == actionID {
			return action.Value, true
		}
	}
	return "", false
}

func blockActionValueOrEmpty(callback slack.InteractionCallback, actionID string) string {
	value, _ := blockActionValue(callback, actionID)
	return value
}

func (l *Listener) ackUnsupportedInteractive(ctx context.Context, request socketmode.Request, kind, id string) {
	if err := l.ackInteractive(ctx, request, nil); err != nil {
		l.logger.Error("Slack Socket Mode interactive acknowledgement failed", "envelope_id", request.EnvelopeID, "error", err)
	}
	l.logger.Warn("unsupported Slack interactive interaction ignored", "kind", kind, "id", boundedInteractiveID(id))
}

func boundedInteractiveID(id string) string {
	if id == "" {
		return "<empty>"
	}
	if len(id) > maxRendererIDLength {
		return "<oversized>"
	}
	for _, r := range id {
		if r < 0x21 || r > 0x7e {
			return "<invalid>"
		}
	}
	return id
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
	if handlers == nil {
		if err := l.builderHandler.publishModalFallback(ctx, callback); err != nil {
			l.logger.Warn("builder modal fallback failed", "error", err)
		}
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

func (l *Listener) handleEventsAPI(ctx, handlerCtx context.Context, event socketmode.Event, handlers *sync.WaitGroup, handler func(context.Context, domain.Invocation)) {
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
		handler(handlerCtx, invocation)
	}()
}
