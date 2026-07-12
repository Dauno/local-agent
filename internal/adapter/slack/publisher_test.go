package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestSplitResponseCountsUnicodeAndStaysBelowSlackLimit(t *testing.T) {
	t.Parallel()
	short := strings.Repeat("界", SlackChunkRunes)
	if chunks := SplitResponse(short); len(chunks) != 1 || chunks[0] != short {
		t.Fatalf("SplitResponse(short) returned %d altered chunks", len(chunks))
	}

	long := strings.Repeat("界", SlackChunkRunes+1)
	chunks := SplitResponse(long)
	if len(chunks) != 2 {
		t.Fatalf("SplitResponse(long) returned %d chunks, want 2", len(chunks))
	}
	for index, chunk := range chunks {
		if got := utf8.RuneCountInString(chunk); got >= 3500 {
			t.Fatalf("chunk %d has %d Unicode characters", index+1, got)
		}
		wantPrefix := fmt.Sprintf("(%d/%d) ", index+1, len(chunks))
		if !strings.HasPrefix(chunk, wantPrefix) {
			t.Fatalf("chunk %d = %q..., want prefix %q", index+1, chunk[:6], wantPrefix)
		}
	}
	if got := joinChunkContent(chunks); got != long {
		t.Fatal("hard-split Unicode content was altered")
	}
}

func TestSplitResponsePrefersParagraphBoundaries(t *testing.T) {
	t.Parallel()
	text := "aaaa\n\nbbbb\n\ncccc"
	chunks := splitResponse(text, 12)
	want := []string{"(1/3) aaaa\n\n", "(2/3) bbbb\n\n", "(3/3) cccc"}
	if fmt.Sprint(chunks) != fmt.Sprint(want) {
		t.Fatalf("splitResponse() = %#v, want %#v", chunks, want)
	}
	if joinChunkContent(chunks) != text {
		t.Fatal("paragraph splitting altered response content")
	}
}

func TestSplitResponseHardSplitsSingleParagraphAndNumbersManyChunks(t *testing.T) {
	t.Parallel()
	chunks := splitResponse(strings.Repeat("x", 61), 12)
	if len(chunks) <= 9 {
		t.Fatalf("splitResponse() returned %d chunks, want more than 9", len(chunks))
	}
	for index, chunk := range chunks {
		if utf8.RuneCountInString(chunk) > 12 {
			t.Fatalf("chunk %d exceeds limit: %q", index+1, chunk)
		}
		if !strings.HasPrefix(chunk, fmt.Sprintf("(%d/%d) ", index+1, len(chunks))) {
			t.Fatalf("chunk %d has wrong prefix: %q", index+1, chunk)
		}
	}
	if joinChunkContent(chunks) != strings.Repeat("x", 61) {
		t.Fatal("hard-split content was altered")
	}
}

func TestPublisherPostsChunksInOrderToSameTargetsAndReturnsLastTimestamp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target domain.ReplyTarget
	}{
		{name: "direct message", target: domain.ReplyTarget{ChannelID: testDM}},
		{name: "channel thread", target: domain.ReplyTarget{ChannelID: testChannel, ThreadTS: testThread}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &fakePostClient{responses: []postResponse{{timestamp: "1.1"}, {timestamp: "1.2"}}}
			publisher := newPublisher(client, 2*time.Second, nil)
			publisher.pace = time.Second
			now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
			publisher.now = func() time.Time { return now }
			var sleeps []time.Duration
			publisher.sleep = func(_ context.Context, duration time.Duration) error {
				sleeps = append(sleeps, duration)
				now = now.Add(duration)
				return nil
			}

			text := strings.Repeat("界", SlackChunkRunes+1)
			tt.target.CorrelationID = "intent-correlation"
			got, err := publisher.Publish(context.Background(), tt.target, text)
			if err != nil {
				t.Fatalf("Publish() error = %v", err)
			}
			if got.LastMessageTS != "1.2" {
				t.Fatalf("last timestamp = %q, want 1.2", got.LastMessageTS)
			}
			calls := client.callsSnapshot()
			if len(calls) != 2 || len(sleeps) != 1 || sleeps[0] != time.Second {
				t.Fatalf("calls = %d, sleeps = %v", len(calls), sleeps)
			}
			for index, call := range calls {
				if call.channelID != tt.target.ChannelID || call.threadTS != tt.target.ThreadTS {
					t.Fatalf("call %d target = %q/%q, want %q/%q", index+1, call.channelID, call.threadTS, tt.target.ChannelID, tt.target.ThreadTS)
				}
				if !strings.HasPrefix(call.text, fmt.Sprintf("(%d/2) ", index+1)) {
					t.Fatalf("call %d text has wrong order prefix: %q", index+1, call.text[:6])
				}
				if !call.hadDeadline {
					t.Fatalf("call %d did not receive an API deadline", index+1)
				}
				if call.correlationID != tt.target.CorrelationID {
					t.Fatalf("call %d correlation = %q, want %q", index+1, call.correlationID, tt.target.CorrelationID)
				}
			}
		})
	}
}

func TestSDKPostClientAddsDurableCorrelationMetadata(t *testing.T) {
	t.Parallel()
	var metadata slackapi.SlackMetadata
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		if err := json.Unmarshal([]byte(r.Form.Get("metadata")), &metadata); err != nil {
			t.Errorf("metadata error = %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":"D12345678","ts":"1.1"}`))
	}))
	defer server.Close()

	client := slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.URL+"/"))
	timestamp, err := (sdkPostClient{client: client}).PostMessage(t.Context(), testDM, "reply", "", "intent-correlation")
	if err != nil || timestamp != "1.1" {
		t.Fatalf("PostMessage() = %q, %v", timestamp, err)
	}
	if metadata.EventType != "local_agent.assistant_exchange" || metadata.EventPayload["correlation_id"] != "intent-correlation" {
		t.Fatalf("Slack metadata = %#v", metadata)
	}
}

func TestPublisherPacesAcrossResponsesPerChannel(t *testing.T) {
	client := &fakePostClient{responses: []postResponse{{timestamp: "1.1"}, {timestamp: "1.2"}, {timestamp: "2.1"}}}
	publisher := newPublisher(client, time.Second, nil)
	publisher.pace = time.Second
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	publisher.now = func() time.Time { return now }
	var sleeps []time.Duration
	publisher.sleep = func(_ context.Context, duration time.Duration) error {
		sleeps = append(sleeps, duration)
		now = now.Add(duration)
		return nil
	}

	if _, err := publisher.Publish(t.Context(), domain.ReplyTarget{ChannelID: testChannel, ThreadTS: "1.0"}, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(t.Context(), domain.ReplyTarget{ChannelID: testChannel, ThreadTS: "2.0"}, "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(t.Context(), domain.ReplyTarget{ChannelID: testDM}, "other channel"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sleeps) != fmt.Sprint([]time.Duration{time.Second}) {
		t.Fatalf("channel pacing sleeps = %v", sleeps)
	}
}

func TestPublisherRetriesOneRateLimitUsingRetryAfter(t *testing.T) {
	t.Parallel()
	rateErr := &slackapi.RateLimitedError{RetryAfter: 37 * time.Millisecond}
	client := &fakePostClient{responses: []postResponse{{err: rateErr}, {timestamp: "2.2"}}}
	publisher := newPublisher(client, time.Second, nil)
	var sleeps []time.Duration
	publisher.sleep = func(_ context.Context, duration time.Duration) error {
		sleeps = append(sleeps, duration)
		return nil
	}

	got, err := publisher.Publish(context.Background(), domain.ReplyTarget{ChannelID: testDM}, "respuesta")
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got.LastMessageTS != "2.2" || client.callCount() != 2 {
		t.Fatalf("Publish() = %#v with %d calls", got, client.callCount())
	}
	if fmt.Sprint(sleeps) != fmt.Sprint([]time.Duration{37 * time.Millisecond}) {
		t.Fatalf("retry sleeps = %v", sleeps)
	}
	calls := client.callsSnapshot()
	if calls[0].text != calls[1].text || calls[0].channelID != calls[1].channelID || calls[0].threadTS != calls[1].threadTS {
		t.Fatal("rate-limit retry changed the message target or content")
	}
}

func TestPublisherRetriesRateLimitOnlyOnceAndStops(t *testing.T) {
	t.Parallel()
	rateErr := &slackapi.RateLimitedError{RetryAfter: time.Millisecond}
	client := &fakePostClient{responses: []postResponse{{err: rateErr}, {err: rateErr}, {timestamp: "unexpected"}}}
	publisher := newPublisher(client, time.Second, nil)
	publisher.sleep = func(context.Context, time.Duration) error { return nil }

	_, err := publisher.Publish(context.Background(), domain.ReplyTarget{ChannelID: testDM}, strings.Repeat("x", SlackChunkRunes+1))
	if !errors.Is(err, rateErr) {
		t.Fatalf("Publish() error = %v, want wrapped rate-limit error", err)
	}
	if client.callCount() != 2 {
		t.Fatalf("post calls = %d, want exactly 2", client.callCount())
	}
}

func TestPublisherStopsAfterChunkFailureAndReturnsLastPublishedTimestamp(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("Slack unavailable xoxb-123456789-secret")
	client := &fakePostClient{responses: []postResponse{{timestamp: "3.1"}, {err: wantErr}, {timestamp: "unexpected"}}}
	publisher := newPublisher(client, time.Second, nil)
	publisher.sleep = func(context.Context, time.Duration) error { return nil }

	text := strings.Repeat("x", 3*SlackChunkRunes)
	got, err := publisher.Publish(context.Background(), domain.ReplyTarget{ChannelID: testChannel, ThreadTS: testThread}, text)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Publish() error = %v, want wrapped post error", err)
	}
	if strings.Contains(err.Error(), "123456789-secret") {
		t.Fatalf("Publish() leaked token in error: %v", err)
	}
	if got.LastMessageTS != "3.1" {
		t.Fatalf("last timestamp = %q, want first successful chunk", got.LastMessageTS)
	}
	if client.callCount() != 2 {
		t.Fatalf("post calls = %d, want stop after second chunk", client.callCount())
	}
}

func TestPublisherCancellationDuringRetryWaitStopsRetry(t *testing.T) {
	t.Parallel()
	rateErr := &slackapi.RateLimitedError{RetryAfter: time.Hour}
	client := &fakePostClient{responses: []postResponse{{err: rateErr}, {timestamp: "unexpected"}}}
	publisher := newPublisher(client, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	publisher.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}

	_, err := publisher.Publish(ctx, domain.ReplyTarget{ChannelID: testDM}, "respuesta")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish() error = %v, want context canceled", err)
	}
	if client.callCount() != 1 {
		t.Fatalf("post calls = %d, want no retry", client.callCount())
	}
}

func TestPublisherStopsImmediatelyOnNonRateLimitFailure(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("post failed")
	client := &fakePostClient{responses: []postResponse{{err: wantErr}, {timestamp: "unexpected"}}}
	publisher := newPublisher(client, time.Second, nil)
	slept := false
	publisher.sleep = func(context.Context, time.Duration) error { slept = true; return nil }

	_, err := publisher.Publish(context.Background(), domain.ReplyTarget{ChannelID: testDM}, strings.Repeat("x", SlackChunkRunes+1))
	if !errors.Is(err, wantErr) || client.callCount() != 1 || slept {
		t.Fatalf("Publish() error = %v, calls = %d, slept = %v", err, client.callCount(), slept)
	}
}

func TestPublisherValidatesInput(t *testing.T) {
	t.Parallel()
	validClient := &fakePostClient{}
	tests := []struct {
		name      string
		publisher *Publisher
		target    domain.ReplyTarget
		text      string
	}{
		{name: "missing client", publisher: newPublisher(nil, time.Second, nil), target: domain.ReplyTarget{ChannelID: testDM}, text: "ok"},
		{name: "missing channel", publisher: newPublisher(validClient, time.Second, nil), text: "ok"},
		{name: "empty text", publisher: newPublisher(validClient, time.Second, nil), target: domain.ReplyTarget{ChannelID: testDM}, text: " \n "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tt.publisher.Publish(context.Background(), tt.target, tt.text); err == nil {
				t.Fatal("Publish() error = nil")
			}
		})
	}
}

func joinChunkContent(chunks []string) string {
	var joined strings.Builder
	for _, chunk := range chunks {
		separator := strings.Index(chunk, ") ")
		if separator < 0 {
			joined.WriteString(chunk)
			continue
		}
		joined.WriteString(chunk[separator+2:])
	}
	return joined.String()
}

type postCall struct {
	channelID     string
	text          string
	threadTS      string
	correlationID string
	hadDeadline   bool
}

type postResponse struct {
	timestamp string
	err       error
}

type fakePostClient struct {
	mu        sync.Mutex
	calls     []postCall
	responses []postResponse
}

func (c *fakePostClient) PostMessage(ctx context.Context, channelID, text, threadTS, correlationID string) (string, error) {
	_, hadDeadline := ctx.Deadline()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, postCall{channelID: channelID, text: text, threadTS: threadTS, correlationID: correlationID, hadDeadline: hadDeadline})
	index := len(c.calls) - 1
	if index >= len(c.responses) {
		return fmt.Sprintf("ts-%d", index+1), nil
	}
	return c.responses[index].timestamp, c.responses[index].err
}

func (c *fakePostClient) callsSnapshot() []postCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]postCall(nil), c.calls...)
}

func (c *fakePostClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}
