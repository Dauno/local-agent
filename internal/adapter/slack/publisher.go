package slack

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

const (
	// SlackChunkRunes is deliberately below Slack's 3,500-character safety
	// boundary and includes any multipart prefix.
	SlackChunkRunes = 3499
	defaultPace     = time.Second
)

type postClient interface {
	PostMessage(context.Context, string, string, string, string) (string, error)
}

type sdkPostClient struct {
	client *slackapi.Client
}

func (c sdkPostClient) PostMessage(ctx context.Context, channelID, text, threadTS, correlationID string) (string, error) {
	options := []slackapi.MsgOption{
		slackapi.MsgOptionText(text, false),
		slackapi.MsgOptionDisableLinkUnfurl(),
		slackapi.MsgOptionDisableMediaUnfurl(),
	}
	if threadTS != "" {
		options = append(options, slackapi.MsgOptionTS(threadTS))
	}
	if correlationID != "" {
		options = append(options, slackapi.MsgOptionMetadata(slackapi.SlackMetadata{
			EventType:    "local_agent.assistant_exchange",
			EventPayload: map[string]any{"correlation_id": correlationID},
		}))
	}
	_, timestamp, err := c.client.PostMessageContext(ctx, channelID, options...)
	return timestamp, err
}

type sleepFunc func(context.Context, time.Duration) error

// Publisher implements port.ResponsePublisher using Slack chat.postMessage.
type Publisher struct {
	client   postClient
	timeout  time.Duration
	pace     time.Duration
	sleep    sleepFunc
	now      func() time.Time
	logger   port.Logger
	channels sync.Map
}

type channelPace struct {
	mu          sync.Mutex
	lastAttempt time.Time
}

func NewPublisher(client *slackapi.Client, timeout time.Duration, logger port.Logger) *Publisher {
	var poster postClient
	if client != nil {
		poster = sdkPostClient{client: client}
	}
	return newPublisher(poster, timeout, logger)
}

func newPublisher(client postClient, timeout time.Duration, logger port.Logger) *Publisher {
	return &Publisher{
		client: client, timeout: timeout, pace: defaultPace,
		sleep: sleepContext, now: time.Now, logger: loggerOrDiscard(logger),
	}
}

func (p *Publisher) Publish(ctx context.Context, target domain.ReplyTarget, text string) (port.PublishedResponse, error) {
	if p == nil || p.client == nil {
		return port.PublishedResponse{}, errors.New("Slack posting client is required")
	}
	if target.ChannelID == "" {
		return port.PublishedResponse{}, errors.New("Slack response channel is required")
	}
	if strings.TrimSpace(text) == "" {
		return port.PublishedResponse{}, errors.New("Slack response text is required")
	}

	chunks := SplitResponse(text)
	result := port.PublishedResponse{}
	channel := p.channelPace(target.ChannelID)
	channel.mu.Lock()
	defer channel.mu.Unlock()
	for index, chunk := range chunks {
		if err := p.waitForChannel(ctx, channel); err != nil {
			return result, fmt.Errorf("pace Slack channel %s: %w", target.ChannelID, err)
		}
		timestamp, err := p.postWithRetry(ctx, target, chunk, target.CorrelationID)
		channel.lastAttempt = p.now()
		if err != nil {
			safeErr := secure.NewRedactor().Error(err)
			p.logger.Error("Slack response posting failed", "channel_id", target.ChannelID, "chunk", index+1, "chunks", len(chunks), "error", safeErr)
			return result, fmt.Errorf("post Slack response chunk %d of %d: %w", index+1, len(chunks), safeErr)
		}
		result.LastMessageTS = timestamp
	}
	return result, nil
}

func (p *Publisher) channelPace(channelID string) *channelPace {
	value, _ := p.channels.LoadOrStore(channelID, &channelPace{})
	return value.(*channelPace)
}

func (p *Publisher) waitForChannel(ctx context.Context, channel *channelPace) error {
	if p.pace <= 0 || channel.lastAttempt.IsZero() {
		return nil
	}
	wait := p.pace - p.now().Sub(channel.lastAttempt)
	if wait <= 0 {
		return nil
	}
	return p.sleep(ctx, wait)
}

func (p *Publisher) postWithRetry(ctx context.Context, target domain.ReplyTarget, text, correlationID string) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		callCtx := ctx
		cancel := func() {}
		if p.timeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, p.timeout)
		}
		timestamp, err := p.client.PostMessage(callCtx, target.ChannelID, text, target.ThreadTS, correlationID)
		cancel()
		if err == nil {
			return timestamp, nil
		}

		var rateLimited *slackapi.RateLimitedError
		if attempt != 0 || !errors.As(err, &rateLimited) {
			return "", err
		}
		p.logger.Warn("Slack response rate limited; retrying once", "channel_id", target.ChannelID, "retry_after", rateLimited.RetryAfter)
		if err := p.sleep(ctx, max(rateLimited.RetryAfter, 0)); err != nil {
			return "", err
		}
	}
	return "", errors.New("Slack response retry exhausted")
}

// SplitResponse returns Slack-safe chunks. Character limits count Unicode code
// points, and multipart prefixes are included in the limit.
func SplitResponse(text string) []string {
	return splitResponse(text, SlackChunkRunes)
}

func splitResponse(text string, limit int) []string {
	if text == "" {
		return nil
	}
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}

	runeCount := utf8.RuneCountInString(text)
	maxDigits := len(strconv.Itoa(runeCount))
	var chunks []string
	for digits := 1; digits <= maxDigits; digits++ {
		// "(" + n + "/" + N + ") "
		contentLimit := limit - (2*digits + 4)
		if contentLimit <= 0 {
			continue
		}
		candidate := splitContent(text, contentLimit)
		if len(candidate) <= maxChunkCount(digits) {
			chunks = candidate
			break
		}
	}
	if len(chunks) == 0 {
		// This can only occur with an impractically small custom test limit.
		return []string{text}
	}

	total := len(chunks)
	for index := range chunks {
		chunks[index] = fmt.Sprintf("(%d/%d) %s", index+1, total, chunks[index])
	}
	return chunks
}

func splitContent(text string, limit int) []string {
	remaining := []rune(text)
	chunks := make([]string, 0, len(remaining)/limit+1)
	for len(remaining) > limit {
		cut := limit
		advance := limit
		for index := limit - 1; index > 1; index-- {
			if remaining[index-1] == '\n' && remaining[index] == '\n' {
				cut = index + 1
				advance = index + 1
				break
			}
		}
		chunks = append(chunks, string(remaining[:cut]))
		remaining = remaining[advance:]
	}
	if len(remaining) != 0 {
		chunks = append(chunks, string(remaining))
	}
	return chunks
}

func maxChunkCount(digits int) int {
	value := 1
	for range digits {
		value *= 10
	}
	return value - 1
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ port.ResponsePublisher = (*Publisher)(nil)
