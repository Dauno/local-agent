package slack

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	slackapi "github.com/slack-go/slack"
)

// InteractiveEventType identifies the two interactive event kinds handled by
// this dispatcher. Other Socket Mode event types are deliberately not
// representable as registrations.
type InteractiveEventType string

const (
	InteractiveEventBlockActions   InteractiveEventType = "block_actions"
	InteractiveEventViewSubmission InteractiveEventType = "view_submission"
)

var (
	// ErrUnsupportedInteractive is intentionally bounded so an untrusted Slack
	// ID is never copied into an internal error or sent to another layer.
	ErrUnsupportedInteractive = errors.New("unsupported Slack interactive interaction")
	ErrMalformedInteractive   = errors.New("malformed Slack interactive interaction")
)

// BlockActionHandler handles one registered block action after the listener
// has acknowledged the Socket Mode request.
type BlockActionHandler func(context.Context, slackapi.InteractionCallback) error

// ViewSubmissionHandler returns the synchronous Slack response and, when
// needed, work that the listener may start after acknowledgement.
type ViewSubmissionHandler func(context.Context, slackapi.InteractionCallback) (ViewDispatchResult, error)

// ViewDispatchResult keeps the view_submission acknowledgement separate from
// its post-acknowledgement effects.
type ViewDispatchResult struct {
	Response *slackapi.ViewSubmissionResponse
	Effect   func(context.Context) error
}

// InteractiveRegistration is one immutable dispatcher registration. The
// event type selects exactly one of ActionHandler or ViewHandler.
type InteractiveRegistration struct {
	ID            string
	EventType     InteractiveEventType
	ActionHandler BlockActionHandler
	ViewHandler   ViewSubmissionHandler
}

// InteractiveDispatcher routes known block actions and view submissions. The
// maps are copied during construction and are never mutated afterward, so
// lookups are safe for concurrent listener dispatch.
type InteractiveDispatcher struct {
	actions map[string]BlockActionHandler
	views   map[string]ViewSubmissionHandler
}

// Dispatcher is a short alias for callers that do not need the transport
// qualifier in the type name.
type Dispatcher = InteractiveDispatcher

// NewInteractiveDispatcher validates and copies registrations into immutable
// action and view tables.
func NewInteractiveDispatcher(registrations []InteractiveRegistration) (*InteractiveDispatcher, error) {
	dispatcher := &InteractiveDispatcher{
		actions: make(map[string]BlockActionHandler),
		views:   make(map[string]ViewSubmissionHandler),
	}
	for _, registration := range registrations {
		if err := validateInteractiveRegistration(registration); err != nil {
			return nil, err
		}
		if _, exists := dispatcher.actions[registration.ID]; exists {
			return nil, fmt.Errorf("duplicate interactive registration %q", registration.ID)
		}
		if _, exists := dispatcher.views[registration.ID]; exists {
			return nil, fmt.Errorf("duplicate interactive registration %q", registration.ID)
		}
		switch registration.EventType {
		case InteractiveEventBlockActions:
			dispatcher.actions[registration.ID] = registration.ActionHandler
		case InteractiveEventViewSubmission:
			dispatcher.views[registration.ID] = registration.ViewHandler
		}
	}
	return dispatcher, nil
}

// NewDispatcher is an explicit short constructor alias.
func NewDispatcher(registrations []InteractiveRegistration) (*InteractiveDispatcher, error) {
	return NewInteractiveDispatcher(registrations)
}

func validateInteractiveRegistration(registration InteractiveRegistration) error {
	if strings.TrimSpace(registration.ID) == "" {
		return errors.New("interactive registration ID is required")
	}
	if len(registration.ID) > maxRendererIDLength {
		return errors.New("interactive registration ID exceeds Slack limit")
	}
	for _, r := range registration.ID {
		if r < 0x21 || r > 0x7e {
			return errors.New("interactive registration ID must contain printable ASCII only")
		}
	}

	switch registration.EventType {
	case InteractiveEventBlockActions:
		if registration.ActionHandler == nil || registration.ViewHandler != nil {
			return fmt.Errorf("block action registration %q must provide only an action handler", registration.ID)
		}
	case InteractiveEventViewSubmission:
		if registration.ViewHandler == nil || registration.ActionHandler != nil {
			return fmt.Errorf("view submission registration %q must provide only a view handler", registration.ID)
		}
	default:
		return fmt.Errorf("interactive registration %q uses unsupported event type", registration.ID)
	}
	return nil
}

// HasAction reports whether actionID has a registered block-action handler.
func (d *InteractiveDispatcher) HasAction(actionID string) bool {
	if d == nil {
		return false
	}
	_, ok := d.actions[actionID]
	return ok
}

// HasView reports whether callbackID has a registered view-submission handler.
func (d *InteractiveDispatcher) HasView(callbackID string) bool {
	if d == nil {
		return false
	}
	_, ok := d.views[callbackID]
	return ok
}

// RegisteredActionIDs returns a copy of registered action IDs for startup
// validation and diagnostics. The dispatcher retains ownership of its tables.
func (d *InteractiveDispatcher) RegisteredActionIDs() []string {
	if d == nil {
		return nil
	}
	ids := make([]string, 0, len(d.actions))
	for id := range d.actions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// RegisteredViewIDs returns a copy of registered callback IDs for startup
// validation and diagnostics. The dispatcher retains ownership of its tables.
func (d *InteractiveDispatcher) RegisteredViewIDs() []string {
	if d == nil {
		return nil
	}
	ids := make([]string, 0, len(d.views))
	for id := range d.views {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// HandleAction looks up actionID and invokes its handler only for a well-formed
// block_actions payload containing that action ID.
func (d *InteractiveDispatcher) HandleAction(ctx context.Context, actionID string, interaction slackapi.InteractionCallback) error {
	if d == nil || actionID == "" {
		return ErrUnsupportedInteractive
	}
	handler, ok := d.actions[actionID]
	if !ok {
		return ErrUnsupportedInteractive
	}
	if interaction.Type != slackapi.InteractionTypeBlockActions || !hasSingleBlockAction(interaction, actionID) {
		return ErrMalformedInteractive
	}
	return handler(ctx, interaction)
}

// HandleView looks up callbackID and invokes its handler only for a matching
// view_submission payload.
func (d *InteractiveDispatcher) HandleView(ctx context.Context, callbackID string, interaction slackapi.InteractionCallback) (ViewDispatchResult, error) {
	if d == nil || callbackID == "" {
		return ViewDispatchResult{}, ErrUnsupportedInteractive
	}
	handler, ok := d.views[callbackID]
	if !ok {
		return ViewDispatchResult{}, ErrUnsupportedInteractive
	}
	if interaction.Type != slackapi.InteractionTypeViewSubmission || interaction.View.CallbackID != callbackID {
		return ViewDispatchResult{}, ErrMalformedInteractive
	}
	return handler(ctx, interaction)
}

func hasSingleBlockAction(interaction slackapi.InteractionCallback, actionID string) bool {
	if len(interaction.ActionCallback.BlockActions) != 1 || interaction.ActionCallback.BlockActions[0] == nil {
		return false
	}
	return interaction.ActionCallback.BlockActions[0].ActionID == actionID && actionID != ""
}

// newListenerDispatcher contains all listener-owned handlers. It is built
// once and its registration tables remain immutable for the listener lifetime.
func newListenerDispatcher(listener *Listener) (*InteractiveDispatcher, error) {
	return NewInteractiveDispatcher([]InteractiveRegistration{
		{
			ID:        builderSubmitCallbackID,
			EventType: InteractiveEventViewSubmission,
			ViewHandler: func(ctx context.Context, callback slackapi.InteractionCallback) (ViewDispatchResult, error) {
				return listener.handleBuilderSubmission(ctx, callback)
			},
		},
		{
			ID:            "agent_type",
			EventType:     InteractiveEventBlockActions,
			ActionHandler: listener.handleBuilderTypeAction,
		},
		{
			ID:            "local_agent.builder.open",
			EventType:     InteractiveEventBlockActions,
			ActionHandler: listener.handleBuilderOpenAction,
		},
		{
			ID:            builderInstallActionID,
			EventType:     InteractiveEventBlockActions,
			ActionHandler: listener.handleBuilderInstallAction,
		},
		{
			ID:            approveActionID,
			EventType:     InteractiveEventBlockActions,
			ActionHandler: listener.handleConfirmationAction,
		},
		{
			ID:            rejectActionID,
			EventType:     InteractiveEventBlockActions,
			ActionHandler: listener.handleConfirmationAction,
		},
		{
			ID:            "local_agent.onboarding.describe",
			EventType:     InteractiveEventBlockActions,
			ActionHandler: listener.handleOnboardingDescribeAction,
		},
	})
}
