package slack

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestListenerAcknowledgesBeforeAsynchronousDispatchAndShutsDown(t *testing.T) {
	t.Parallel()
	client := newFakeSocketClient()
	listener := newListener(client, NewRouter(testBot), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	type handledInvocation struct {
		text  string
		acked bool
	}
	handled := make(chan handledInvocation, 2)

	go func() {
		done <- listener.Run(ctx, func(handlerCtx context.Context, invocation domain.Invocation) {
			envelopeID := "envelope-1"
			if invocation.Text == "second" {
				envelopeID = "envelope-2"
			}
			handled <- handledInvocation{text: invocation.Text, acked: client.wasAcked(envelopeID)}
			<-handlerCtx.Done()
		})
	}()

	client.events <- socketEvent("envelope-1", directMessageEvent("first"))
	client.events <- socketEvent("envelope-2", directMessageEvent("second"))

	for range 2 {
		select {
		case got := <-handled:
			if !got.acked {
				t.Fatalf("handler for %q started before its envelope was acknowledged", got.text)
			}
		case <-time.After(time.Second):
			t.Fatal("handler was not dispatched asynchronously")
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() shutdown error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop after context cancellation")
	}
}

func TestListenerAcknowledgesIgnoredEventsWithoutDispatching(t *testing.T) {
	t.Parallel()
	client := newFakeSocketClient()
	listener := newListener(client, NewRouter(testBot), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	handled := make(chan struct{}, 1)
	go func() {
		done <- listener.Run(ctx, func(context.Context, domain.Invocation) { handled <- struct{}{} })
	}()

	ignored := callbackEvent(slackevents.Message, &slackevents.MessageEvent{
		Type: "message", User: testUser, Text: "not a thread", TimeStamp: testTS,
		Channel: testChannel, ChannelType: slackevents.ChannelTypeChannel,
	})
	client.events <- socketEvent("ignored", ignored)

	deadline := time.After(time.Second)
	for !client.wasAcked("ignored") {
		select {
		case <-deadline:
			t.Fatal("ignored Events API envelope was not acknowledged")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case <-handled:
		t.Fatal("ignored event was dispatched")
	case <-time.After(20 * time.Millisecond):
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() shutdown error = %v", err)
	}
}

func TestListenerDoesNotAcknowledgeNonEventsAPIOrMissingRequest(t *testing.T) {
	t.Parallel()
	client := newFakeSocketClient()
	listener := newListener(client, NewRouter(testBot), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	handled := make(chan struct{}, 1)
	go func() {
		done <- listener.Run(ctx, func(context.Context, domain.Invocation) { handled <- struct{}{} })
	}()

	client.events <- socketmode.Event{Type: socketmode.EventTypeConnected}
	client.events <- socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: directMessageEvent("missing request")}
	time.Sleep(20 * time.Millisecond)
	if client.ackCount() != 0 {
		t.Fatalf("ack count = %d, want 0", client.ackCount())
	}
	select {
	case <-handled:
		t.Fatal("event without request was dispatched")
	default:
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() shutdown error = %v", err)
	}
}

func TestListenerDispatchesAfterAcknowledgementFailure(t *testing.T) {
	t.Parallel()
	client := newFakeSocketClient()
	client.ackErr = errors.New("socket write failed")
	listener := newListener(client, NewRouter(testBot), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	handled := make(chan struct{}, 1)
	go func() {
		done <- listener.Run(ctx, func(context.Context, domain.Invocation) { handled <- struct{}{} })
	}()

	client.events <- socketEvent("failed-ack", directMessageEvent("still handle"))
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("acknowledgement failure dropped a valid invocation")
	}
	if !client.wasAcked("failed-ack") {
		t.Fatal("Ack() was not attempted")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() shutdown error = %v", err)
	}
}

func TestListenerReturnsSocketClientFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("authentication failed")
	client := newFakeSocketClient()
	client.runErr = want
	listener := newListener(client, NewRouter(testBot), nil)

	err := listener.Run(context.Background(), func(context.Context, domain.Invocation) {})
	if !errors.Is(err, want) {
		t.Fatalf("Run() error = %v, want wrapped %v", err, want)
	}
}

func TestListenerRejectsMissingDependencies(t *testing.T) {
	t.Parallel()
	if err := newListener(nil, NewRouter(testBot), nil).Run(context.Background(), func(context.Context, domain.Invocation) {}); err == nil {
		t.Fatal("Run() with nil client returned nil")
	}
	client := newFakeSocketClient()
	if err := newListener(client, NewRouter(testBot), nil).Run(context.Background(), nil); err == nil {
		t.Fatal("Run() with nil handler returned nil")
	}
}

type fakeSocketClient struct {
	events chan socketmode.Event

	mu     sync.Mutex
	acked  map[string]bool
	ackErr error
	runErr error
}

func newFakeSocketClient() *fakeSocketClient {
	return &fakeSocketClient{events: make(chan socketmode.Event, 8), acked: make(map[string]bool)}
}

func (c *fakeSocketClient) Run(ctx context.Context) error {
	if c.runErr != nil {
		return c.runErr
	}
	<-ctx.Done()
	return ctx.Err()
}

func (c *fakeSocketClient) Events() <-chan socketmode.Event { return c.events }

func (c *fakeSocketClient) Ack(_ context.Context, request socketmode.Request) error {
	c.mu.Lock()
	c.acked[request.EnvelopeID] = true
	c.mu.Unlock()
	return c.ackErr
}

func (c *fakeSocketClient) wasAcked(envelopeID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acked[envelopeID]
}

func (c *fakeSocketClient) ackCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.acked)
}

func socketEvent(envelopeID string, event slackevents.EventsAPIEvent) socketmode.Event {
	return socketmode.Event{
		Type:    socketmode.EventTypeEventsAPI,
		Data:    event,
		Request: &socketmode.Request{Type: socketmode.RequestTypeEventsAPI, EnvelopeID: envelopeID},
	}
}

func directMessageEvent(text string) slackevents.EventsAPIEvent {
	return callbackEvent(slackevents.Message, &slackevents.MessageEvent{
		Type: "message", User: testUser, Text: text, TimeStamp: testTS,
		Channel: testDM, ChannelType: slackevents.ChannelTypeIM,
	})
}
