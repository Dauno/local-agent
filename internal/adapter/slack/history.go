package slack

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

type historyClient interface {
	ConversationReplies(context.Context, string, string, string, int) ([]slackapi.Message, error)
	ConversationHistory(context.Context, string, string, int) ([]slackapi.Message, error)
}

type sdkHistoryClient struct {
	client *slackapi.Client
}

func (c sdkHistoryClient) ConversationReplies(ctx context.Context, channelID, rootTS, latest string, limit int) ([]slackapi.Message, error) {
	messages, _, _, err := c.client.GetConversationRepliesContext(ctx, &slackapi.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: rootTS,
		Latest:    latest,
		Inclusive: true,
		Limit:     limit,
	})
	return messages, err
}

func (c sdkHistoryClient) ConversationHistory(ctx context.Context, channelID, latest string, limit int) ([]slackapi.Message, error) {
	response, err := c.client.GetConversationHistoryContext(ctx, &slackapi.GetConversationHistoryParameters{
		ChannelID: channelID,
		Latest:    latest,
		Inclusive: true,
		Limit:     limit,
	})
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, errors.New("Slack conversations.history returned no response")
	}
	return response.Messages, nil
}

// HistoryReader recovers bounded Slack context without persisting it.
type HistoryReader struct {
	client    historyClient
	botUserID string
	timeout   time.Duration
	logger    port.Logger
}

func NewHistoryReader(client *slackapi.Client, botUserID string, timeout time.Duration, logger port.Logger) *HistoryReader {
	var history historyClient
	if client != nil {
		history = sdkHistoryClient{client: client}
	}
	return newHistoryReader(history, botUserID, timeout, logger)
}

func newHistoryReader(client historyClient, botUserID string, timeout time.Duration, logger port.Logger) *HistoryReader {
	return &HistoryReader{
		client: client, botUserID: botUserID, timeout: timeout,
		logger: loggerOrDiscard(logger),
	}
}

func (r *HistoryReader) RecentHistory(ctx context.Context, invocation domain.Invocation, limits domain.ContextLimits) (port.History, error) {
	if r == nil || r.client == nil {
		return port.History{}, errors.New("Slack history client is required")
	}
	if r.botUserID == "" {
		return port.History{}, errors.New("Slack bot user ID is required")
	}
	if limits.MaxMessages <= 0 || limits.MaxChars <= 0 {
		return port.History{}, errors.New("Slack history limits must be positive")
	}
	if err := invocation.Validate(); err != nil {
		return port.History{}, fmt.Errorf("invalid Slack history invocation: %w", err)
	}

	callCtx := ctx
	cancel := func() {}
	if r.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, r.timeout)
	}
	defer cancel()

	var (
		messages []slackapi.Message
		err      error
	)
	if invocation.ChannelKind == domain.ChannelDM {
		messages, err = r.client.ConversationHistory(callCtx, invocation.ChannelID, invocation.EventTS, limits.MaxMessages)
		if len(messages) > limits.MaxMessages {
			messages = messages[:limits.MaxMessages]
		}
		slices.Reverse(messages) // conversations.history is newest first.
	} else {
		rootTS := invocation.ThreadTS
		if rootTS == "" {
			rootTS = invocation.EventTS
		}
		messages, err = r.client.ConversationReplies(callCtx, invocation.ChannelID, rootTS, invocation.EventTS, limits.MaxMessages)
		if len(messages) > limits.MaxMessages {
			messages = messages[len(messages)-limits.MaxMessages:]
		}
	}
	if err != nil {
		safeErr := secure.NewRedactor().Error(err)
		r.logger.Warn("Slack history read failed", "channel_id", invocation.ChannelID, "error", safeErr)
		return port.History{}, fmt.Errorf("read Slack conversation history: %w", safeErr)
	}

	history := mapHistory(messages, r.botUserID)
	history.Messages = domain.LimitMessages(history.Messages, limits)
	return history, nil
}

func mapHistory(messages []slackapi.Message, botUserID string) port.History {
	history := port.History{Messages: make([]domain.Message, 0, len(messages))}
	for _, message := range messages {
		if strings.TrimSpace(message.Text) == "" || message.User == "" || message.Hidden || message.Edited != nil || len(message.Files) != 0 {
			continue
		}

		role := domain.RoleUser
		if message.User == botUserID {
			role = domain.RoleAssistant
			history.BotParticipated = true
		} else if message.SubType != "" {
			// Bot-authored messages from this app are kept above; unsupported
			// user/system subtypes do not become model context.
			continue
		}
		history.Messages = append(history.Messages, domain.Message{
			Role:       role,
			Content:    message.Text,
			UserID:     message.User,
			ExternalTS: message.Timestamp,
			CreatedAt:  parseSlackTimestamp(message.Timestamp),
		})
	}
	return history
}

func parseSlackTimestamp(timestamp string) time.Time {
	secondsText, fractionText, found := strings.Cut(timestamp, ".")
	if !found {
		return time.Time{}
	}
	seconds, err := strconv.ParseInt(secondsText, 10, 64)
	if err != nil {
		return time.Time{}
	}
	if len(fractionText) > 9 {
		fractionText = fractionText[:9]
	}
	fractionText += strings.Repeat("0", 9-len(fractionText))
	nanoseconds, err := strconv.ParseInt(fractionText, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(seconds, nanoseconds).UTC()
}

var _ port.HistoryReader = (*HistoryReader)(nil)
