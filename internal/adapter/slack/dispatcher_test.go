package slack

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestInteractiveDispatcherRejectsInvalidRegistrations(t *testing.T) {
	actionHandler := func(context.Context, slackapi.InteractionCallback) error { return nil }
	viewHandler := func(context.Context, slackapi.InteractionCallback) (ViewDispatchResult, error) {
		return ViewDispatchResult{}, nil
	}
	tests := []struct {
		name string
		regs []InteractiveRegistration
	}{
		{
			name: "empty ID",
			regs: []InteractiveRegistration{{EventType: InteractiveEventBlockActions, ActionHandler: actionHandler}},
		},
		{
			name: "duplicate actions",
			regs: []InteractiveRegistration{
				{ID: "same", EventType: InteractiveEventBlockActions, ActionHandler: actionHandler},
				{ID: "same", EventType: InteractiveEventBlockActions, ActionHandler: actionHandler},
			},
		},
		{
			name: "duplicate across tables",
			regs: []InteractiveRegistration{
				{ID: "same", EventType: InteractiveEventBlockActions, ActionHandler: actionHandler},
				{ID: "same", EventType: InteractiveEventViewSubmission, ViewHandler: viewHandler},
			},
		},
		{
			name: "missing action handler",
			regs: []InteractiveRegistration{{ID: "action", EventType: InteractiveEventBlockActions}},
		},
		{
			name: "missing view handler",
			regs: []InteractiveRegistration{{ID: "view", EventType: InteractiveEventViewSubmission}},
		},
		{
			name: "wrong handler for event",
			regs: []InteractiveRegistration{{ID: "view", EventType: InteractiveEventViewSubmission, ActionHandler: actionHandler}},
		},
		{
			name: "unsupported event type",
			regs: []InteractiveRegistration{{ID: "closed", EventType: InteractiveEventType("view_closed"), ViewHandler: viewHandler}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewInteractiveDispatcher(test.regs); err == nil {
				t.Fatal("NewInteractiveDispatcher accepted invalid registrations")
			}
		})
	}
}

func TestInteractiveDispatcherRejectsMalformedAndUnknownPayloads(t *testing.T) {
	var actionCalls atomic.Int32
	var viewCalls atomic.Int32
	dispatcher, err := NewInteractiveDispatcher([]InteractiveRegistration{
		{
			ID:        "action",
			EventType: InteractiveEventBlockActions,
			ActionHandler: func(context.Context, slackapi.InteractionCallback) error {
				actionCalls.Add(1)
				return nil
			},
		},
		{
			ID:        "view",
			EventType: InteractiveEventViewSubmission,
			ViewHandler: func(context.Context, slackapi.InteractionCallback) (ViewDispatchResult, error) {
				viewCalls.Add(1)
				return ViewDispatchResult{}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewInteractiveDispatcher() error = %v", err)
	}

	validAction := slackapi.InteractionCallback{
		Type:           slackapi.InteractionTypeBlockActions,
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{{ActionID: "action"}}},
	}
	if err := dispatcher.HandleAction(context.Background(), "action", validAction); err != nil {
		t.Fatalf("valid action returned error: %v", err)
	}
	if err := dispatcher.HandleAction(context.Background(), "missing", validAction); !errors.Is(err, ErrUnsupportedInteractive) {
		t.Fatalf("unknown action error = %v, want ErrUnsupportedInteractive", err)
	}
	if err := dispatcher.HandleAction(context.Background(), "action", slackapi.InteractionCallback{Type: slackapi.InteractionTypeViewSubmission}); !errors.Is(err, ErrMalformedInteractive) {
		t.Fatalf("wrong event type error = %v, want ErrMalformedInteractive", err)
	}
	if err := dispatcher.HandleAction(context.Background(), "action", slackapi.InteractionCallback{Type: slackapi.InteractionTypeBlockActions}); !errors.Is(err, ErrMalformedInteractive) {
		t.Fatalf("missing action payload error = %v, want ErrMalformedInteractive", err)
	}
	if err := dispatcher.HandleAction(context.Background(), "action", slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{
			{ActionID: "action"}, {ActionID: "another"},
		}},
	}); !errors.Is(err, ErrMalformedInteractive) {
		t.Fatalf("multiple action payload error = %v, want ErrMalformedInteractive", err)
	}
	if actionCalls.Load() != 1 {
		t.Fatalf("action handler calls = %d, want 1", actionCalls.Load())
	}

	validView := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeViewSubmission,
		View: slackapi.View{CallbackID: "view"},
	}
	if _, err := dispatcher.HandleView(context.Background(), "view", validView); err != nil {
		t.Fatalf("valid view returned error: %v", err)
	}
	if _, err := dispatcher.HandleView(context.Background(), "missing", validView); !errors.Is(err, ErrUnsupportedInteractive) {
		t.Fatalf("unknown callback error = %v, want ErrUnsupportedInteractive", err)
	}
	if _, err := dispatcher.HandleView(context.Background(), "view", slackapi.InteractionCallback{Type: slackapi.InteractionTypeViewSubmission}); !errors.Is(err, ErrMalformedInteractive) {
		t.Fatalf("missing callback payload error = %v, want ErrMalformedInteractive", err)
	}
	if viewCalls.Load() != 1 {
		t.Fatalf("view handler calls = %d, want 1", viewCalls.Load())
	}
}

func TestTemplateCatalogExtractsIDsAndValidatesDispatcherCoverage(t *testing.T) {
	catalog, err := LoadTemplateCatalog()
	if err != nil {
		t.Fatalf("LoadTemplateCatalog() error = %v", err)
	}
	ids := catalog.InteractiveIDs()
	if !reflect.DeepEqual(ids.ModalCallbacks, []string{builderSubmitCallbackID}) {
		t.Fatalf("modal callbacks = %v", ids.ModalCallbacks)
	}
	if !reflect.DeepEqual(ids.Actions, []string{"agent_type", "local_agent.builder.open", builderInstallActionID, "local_agent.onboarding.describe"}) {
		t.Fatalf("actions = %v", ids.Actions)
	}
	if !reflect.DeepEqual(ids.BuilderBlocks, []string{"agent_type", "description", "execution_mode", "instruction", "model", "name", "timeout_seconds"}) {
		t.Fatalf("builder blocks = %v", ids.BuilderBlocks)
	}
	if !reflect.DeepEqual(ids.MessageBlocks, []string{"builder_preview_actions", "onboarding_actions"}) {
		t.Fatalf("message blocks = %v", ids.MessageBlocks)
	}

	listener := newListener(nil, NewRouter(testBot), nil)
	if err := listener.ValidateTemplateCatalog(catalog, approveActionID, rejectActionID, statusActionID); err != nil {
		t.Fatalf("default listener dispatcher failed catalog validation: %v", err)
	}

	catalogRegistrations := []InteractiveRegistration{{
		ID:        builderSubmitCallbackID,
		EventType: InteractiveEventViewSubmission,
		ViewHandler: func(context.Context, slackapi.InteractionCallback) (ViewDispatchResult, error) {
			return ViewDispatchResult{}, nil
		},
	}}
	for _, actionID := range ids.Actions {
		catalogRegistrations = append(catalogRegistrations, InteractiveRegistration{
			ID: actionID, EventType: InteractiveEventBlockActions,
			ActionHandler: func(context.Context, slackapi.InteractionCallback) error { return nil },
		})
	}
	catalogDispatcher, err := NewInteractiveDispatcher(catalogRegistrations)
	if err != nil {
		t.Fatalf("construct catalog-only dispatcher: %v", err)
	}
	if err := catalog.ValidateDispatcher(catalogDispatcher); err != nil {
		t.Fatalf("catalog-only dispatcher failed validation: %v", err)
	}
	if err := catalog.ValidateDispatcher(catalogDispatcher, approveActionID); err == nil {
		t.Fatal("dispatcher validation ignored an action declared only by the block kit engine")
	}

	missingView, err := NewInteractiveDispatcher([]InteractiveRegistration{
		{ID: "agent_type", EventType: InteractiveEventBlockActions, ActionHandler: func(context.Context, slackapi.InteractionCallback) error { return nil }},
	})
	if err != nil {
		t.Fatalf("construct partial dispatcher: %v", err)
	}
	if err := catalog.ValidateDispatcher(missingView); err == nil {
		t.Fatal("catalog accepted dispatcher without builder view handler")
	}
}

func TestInteractiveDispatcherConcurrentReadOnlyDispatch(t *testing.T) {
	const calls = 128
	var actionCalls atomic.Int32
	var viewCalls atomic.Int32
	dispatcher, err := NewInteractiveDispatcher([]InteractiveRegistration{
		{
			ID:        "action",
			EventType: InteractiveEventBlockActions,
			ActionHandler: func(context.Context, slackapi.InteractionCallback) error {
				actionCalls.Add(1)
				return nil
			},
		},
		{
			ID:        "view",
			EventType: InteractiveEventViewSubmission,
			ViewHandler: func(context.Context, slackapi.InteractionCallback) (ViewDispatchResult, error) {
				viewCalls.Add(1)
				return ViewDispatchResult{}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("construct dispatcher: %v", err)
	}
	action := slackapi.InteractionCallback{
		Type:           slackapi.InteractionTypeBlockActions,
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{{ActionID: "action"}}},
	}
	view := slackapi.InteractionCallback{Type: slackapi.InteractionTypeViewSubmission, View: slackapi.View{CallbackID: "view"}}
	var wait sync.WaitGroup
	for range calls {
		wait.Add(2)
		go func() {
			defer wait.Done()
			if err := dispatcher.HandleAction(context.Background(), "action", action); err != nil {
				t.Errorf("concurrent action error: %v", err)
			}
		}()
		go func() {
			defer wait.Done()
			if _, err := dispatcher.HandleView(context.Background(), "view", view); err != nil {
				t.Errorf("concurrent view error: %v", err)
			}
		}()
	}
	wait.Wait()
	if actionCalls.Load() != calls || viewCalls.Load() != calls {
		t.Fatalf("handler calls = action %d, view %d; want %d each", actionCalls.Load(), viewCalls.Load(), calls)
	}
}

func TestListenerACKsBeforeDispatcherEffectAndDoesNotNormalizeUnknowns(t *testing.T) {
	client := newFakeSocketClient()
	var effectCalls atomic.Int32
	ackedBeforeEffect := make(chan bool, 1)
	dispatcher, err := NewInteractiveDispatcher([]InteractiveRegistration{{
		ID:        "test.action",
		EventType: InteractiveEventBlockActions,
		ActionHandler: func(_ context.Context, _ slackapi.InteractionCallback) error {
			effectCalls.Add(1)
			ackedBeforeEffect <- client.wasAcked("known-action")
			return nil
		},
	}})
	if err != nil {
		t.Fatalf("construct test dispatcher: %v", err)
	}
	listener := newListener(client, NewRouter(testBot), nil).WithDispatcher(dispatcher)
	var confirmationCalls atomic.Int32
	listener.SetInteractiveHandler(func(context.Context, domain.ConfirmationInteractiveAction) error {
		confirmationCalls.Add(1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx, func(context.Context, domain.Invocation) {}) }()

	client.events <- socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slackapi.InteractionCallback{
			Type:           slackapi.InteractionTypeBlockActions,
			ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{{ActionID: "test.action"}}},
		},
		Request: &socketmode.Request{Type: socketmode.RequestTypeInteractive, EnvelopeID: "known-action"},
	}
	select {
	case acked := <-ackedBeforeEffect:
		if !acked {
			t.Fatal("dispatcher effect ran before Socket Mode ACK")
		}
	case <-time.After(time.Second):
		t.Fatal("known action was not dispatched")
	}

	client.events <- socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Data:    newTestCallback("unknown.action", "wrapper", "T12345678", "U12345678", "C12345678", "1720000001.000001", ""),
		Request: &socketmode.Request{Type: socketmode.RequestTypeInteractive, EnvelopeID: "unknown-action"},
	}
	client.events <- socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slackapi.InteractionCallback{
			Type: slackapi.InteractionTypeViewSubmission,
			View: slackapi.View{CallbackID: "unknown.callback"},
		},
		Request: &socketmode.Request{Type: socketmode.RequestTypeInteractive, EnvelopeID: "unknown-callback"},
	}
	client.events <- socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Data:    slackapi.InteractionCallback{Type: slackapi.InteractionTypeViewClosed, View: slackapi.View{CallbackID: builderSubmitCallbackID}},
		Request: &socketmode.Request{Type: socketmode.RequestTypeInteractive, EnvelopeID: "closed-view"},
	}
	for _, envelopeID := range []string{"unknown-action", "unknown-callback", "closed-view"} {
		waitForAck(t, client, envelopeID)
	}
	if effectCalls.Load() != 1 {
		t.Fatalf("effect calls = %d, want 1", effectCalls.Load())
	}
	if confirmationCalls.Load() != 0 {
		t.Fatalf("confirmation handler calls = %d, want 0 for unknown interactions", confirmationCalls.Load())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("listener shutdown error: %v", err)
	}
}

func TestListenerACKsMalformedActionWithoutEffect(t *testing.T) {
	client := newFakeSocketClient()
	listener := newListener(client, NewRouter(testBot), nil)
	var calls atomic.Int32
	listener.SetInteractiveHandler(func(context.Context, domain.ConfirmationInteractiveAction) error {
		calls.Add(1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx, func(context.Context, domain.Invocation) {}) }()
	client.events <- socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slackapi.InteractionCallback{
			Type:           slackapi.InteractionTypeBlockActions,
			ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{nil}},
		},
		Request: &socketmode.Request{Type: socketmode.RequestTypeInteractive, EnvelopeID: "malformed-action"},
	}
	waitForAck(t, client, "malformed-action")
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("malformed action reached confirmation handler")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("listener shutdown error: %v", err)
	}
}

func waitForAck(t *testing.T, client *fakeSocketClient, envelopeID string) {
	t.Helper()
	deadline := time.After(time.Second)
	for !client.wasAcked(envelopeID) {
		select {
		case <-deadline:
			t.Fatalf("envelope %q was not acknowledged", envelopeID)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
