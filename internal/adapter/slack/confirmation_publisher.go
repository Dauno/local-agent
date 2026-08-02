package slack

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

const (
	confirmationRenderMode        = "confirmation_v1"
	approveActionID               = "local_agent.confirm.approve"
	rejectActionID                = "local_agent.confirm.reject"
	confirmationMetadataEventType = "local_agent_confirmation_prompt"
)

var slackMessageTimestampPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

var _ port.ConfirmationPublisher = (*ConfirmationPublisher)(nil)

type confirmationBlockClient interface {
	PostBlocks(ctx context.Context, channelID, fallbackText string, blocks []slackapi.Block, metadata slackapi.SlackMetadata, threadTS string) (string, error)
	UpdateBlocks(ctx context.Context, channelID, messageTS string, blocks []slackapi.Block, text string) error
	ConfirmationMessages(ctx context.Context, channelID, threadTS string, limit int) ([]slackapi.Message, bool, error)
}

type sdkConfirmationBlockClient struct {
	client *slackapi.Client
}

func (c sdkConfirmationBlockClient) PostBlocks(ctx context.Context, channelID, fallbackText string, blocks []slackapi.Block, metadata slackapi.SlackMetadata, threadTS string) (string, error) {
	options := []slackapi.MsgOption{
		slackapi.MsgOptionText(fallbackText, false),
		slackapi.MsgOptionBlocks(blocks...),
		slackapi.MsgOptionDisableLinkUnfurl(),
		slackapi.MsgOptionDisableMediaUnfurl(),
		slackapi.MsgOptionMetadata(metadata),
	}
	if threadTS != "" {
		options = append(options, slackapi.MsgOptionTS(threadTS))
	}
	_, timestamp, err := c.client.PostMessageContext(ctx, channelID, options...)
	return timestamp, err
}

func (c sdkConfirmationBlockClient) ConfirmationMessages(ctx context.Context, channelID, threadTS string, limit int) ([]slackapi.Message, bool, error) {
	if threadTS != "" {
		messages, hasMore, _, err := c.client.GetConversationRepliesContext(ctx, &slackapi.GetConversationRepliesParameters{
			ChannelID: channelID, Timestamp: threadTS, Limit: limit, IncludeAllMetadata: true,
		})
		return messages, hasMore, err
	}
	response, err := c.client.GetConversationHistoryContext(ctx, &slackapi.GetConversationHistoryParameters{
		ChannelID: channelID, Limit: limit, IncludeAllMetadata: true,
	})
	if err != nil {
		return nil, false, err
	}
	return response.Messages, response.HasMore, nil
}

func (c sdkConfirmationBlockClient) UpdateBlocks(ctx context.Context, channelID, messageTS string, blocks []slackapi.Block, text string) error {
	options := []slackapi.MsgOption{
		slackapi.MsgOptionBlocks(blocks...),
		slackapi.MsgOptionText(text, false),
	}
	_, _, _, err := c.client.UpdateMessageContext(ctx, channelID, messageTS, options...)
	return err
}

type ConfirmationPublisher struct {
	client    confirmationBlockClient
	botUserID string
	timeout   time.Duration
	logger    port.Logger
	renderer  *TemplateRenderer
	renderErr error
}

func NewConfirmationPublisher(client *slackapi.Client, botUserID string, timeout time.Duration, logger port.Logger) *ConfirmationPublisher {
	var poster confirmationBlockClient
	if client != nil {
		poster = sdkConfirmationBlockClient{client: client}
	}
	return newConfirmationPublisher(poster, botUserID, timeout, logger)
}

func newConfirmationPublisher(client confirmationBlockClient, botUserID string, timeout time.Duration, logger port.Logger) *ConfirmationPublisher {
	renderer, renderErr := NewEmbeddedTemplateRenderer()
	return &ConfirmationPublisher{
		client: client, botUserID: botUserID, timeout: timeout, logger: loggerOrDiscard(logger),
		renderer: renderer, renderErr: renderErr,
	}
}

func (p *ConfirmationPublisher) PublishConfirmation(ctx context.Context, delivery port.ConfirmationDelivery) (port.ConfirmationPublishedResult, error) {
	if p == nil || p.client == nil {
		return port.ConfirmationPublishedResult{}, errors.New("Slack posting client is required for confirmation publishing")
	}
	if delivery.ChannelID == "" {
		return port.ConfirmationPublishedResult{}, errors.New("Slack channel is required for confirmation publishing")
	}

	fallbackText, blocks, err := compileConfirmationMessage(p.renderer, delivery)
	if err != nil {
		if p.renderErr != nil {
			err = p.renderErr
		}
		return port.ConfirmationPublishedResult{}, fmt.Errorf("render confirmation template: %w", err)
	}
	metadata := confirmationMetadata(delivery)

	callCtx := ctx
	cancel := func() {}
	if p.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, p.timeout)
	}
	defer cancel()

	timestamp, err := p.client.PostBlocks(callCtx, delivery.ChannelID, fallbackText, blocks, metadata, delivery.ThreadTS)
	if err != nil {
		safeErr := secure.NewRedactor().Error(err)
		return port.ConfirmationPublishedResult{}, fmt.Errorf("publish confirmation blocks: %w", safeErr)
	}
	if timestamp == "" {
		return port.ConfirmationPublishedResult{}, errors.New("Slack published confirmation without a message timestamp")
	}
	return port.ConfirmationPublishedResult{SlackMessageTS: timestamp}, nil
}

func (p *ConfirmationPublisher) RecoverConfirmation(ctx context.Context, delivery port.ConfirmationDelivery) (port.ConfirmationPublishedResult, bool, error) {
	if p == nil || p.client == nil {
		return port.ConfirmationPublishedResult{}, false, errors.New("Slack client is required for confirmation recovery")
	}
	if p.botUserID == "" || delivery.ChannelID == "" || delivery.CorrelationID == "" {
		return port.ConfirmationPublishedResult{}, false, errors.New("invalid confirmation recovery input")
	}
	callCtx := ctx
	cancel := func() {}
	if p.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, p.timeout)
	}
	defer cancel()

	const recoveryLimit = 100
	messages, hasMore, err := p.client.ConfirmationMessages(callCtx, delivery.ChannelID, delivery.ThreadTS, recoveryLimit)
	if err != nil {
		return port.ConfirmationPublishedResult{}, false, fmt.Errorf("read Slack confirmation history: %w", secure.NewRedactor().Error(err))
	}
	expectedDigest := port.ConfirmationContentDigest(delivery)
	var timestamp string
	for _, message := range messages {
		if message.Metadata.EventType != confirmationMetadataEventType {
			continue
		}
		correlationID, _ := message.Metadata.EventPayload["correlation_id"].(string)
		if correlationID != delivery.CorrelationID {
			continue
		}
		renderMode, _ := message.Metadata.EventPayload["render_mode"].(string)
		contentDigest, _ := message.Metadata.EventPayload["content_sha256"].(string)
		if timestamp != "" || message.User != p.botUserID || message.Hidden || message.Edited != nil || len(message.Files) != 0 ||
			message.Timestamp == "" || renderMode != confirmationRenderMode || contentDigest != expectedDigest {
			return port.ConfirmationPublishedResult{}, false, errors.New("Slack confirmation recovery evidence is ambiguous or invalid")
		}
		timestamp = message.Timestamp
	}
	if hasMore {
		return port.ConfirmationPublishedResult{}, false, errors.New("Slack confirmation recovery history is incomplete")
	}
	if timestamp == "" {
		return port.ConfirmationPublishedResult{}, false, nil
	}
	return port.ConfirmationPublishedResult{SlackMessageTS: timestamp}, true, nil
}

func (p *ConfirmationPublisher) UpdateConfirmation(ctx context.Context, delivery port.ConfirmationDelivery, terminalText string) error {
	if p == nil || p.client == nil {
		return errors.New("Slack update client is required for confirmation update")
	}
	if delivery.SlackMessageTS == "" {
		return errors.New("Slack message timestamp is required for confirmation update")
	}
	if delivery.ChannelID == "" {
		return errors.New("Slack channel is required for confirmation update")
	}

	summary := delivery.Summary
	originalCallID := delivery.OriginalCallID

	var headerText string
	var footerText string
	now := time.Now().UTC()
	switch delivery.Status {
	case port.ConfirmationConsumed, port.ConfirmationApproved:
		headerText = fmt.Sprintf(":white_check_mark: %s", summary)
		footerText = terminalConfirmationFooter(true, now)
	case port.ConfirmationRejected:
		headerText = fmt.Sprintf(":x: %s", summary)
		footerText = terminalConfirmationFooter(false, now)
	case port.ConfirmationExpired:
		headerText = fmt.Sprintf(":hourglass: %s", summary)
		footerText = expiredConfirmationFooter(now)
	case port.ConfirmationFailed:
		headerText = fmt.Sprintf(":warning: %s", summary)
		footerText = fmt.Sprintf("_Failed %s_", now.Format("15:04"))
	default:
		return fmt.Errorf("confirmation delivery status %s is not terminal", delivery.Status)
	}

	headerBlock := slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject("mrkdwn", headerText, false, false),
		nil, nil,
	)

	detailFields := []*slackapi.TextBlockObject{
		slackapi.NewTextBlockObject("mrkdwn", fmt.Sprintf("*Call ID:*\n`%s`", originalCallID), false, false),
		slackapi.NewTextBlockObject("mrkdwn", footerText, false, false),
	}
	detailBlock := slackapi.NewSectionBlock(nil, detailFields, nil)

	blocks := []slackapi.Block{headerBlock, detailBlock}

	callCtx := ctx
	cancel := func() {}
	if p.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, p.timeout)
	}
	defer cancel()

	plainText := strings.Join([]string{headerText, footerText, originalCallID}, "\n")

	if err := p.client.UpdateBlocks(callCtx, delivery.ChannelID, delivery.SlackMessageTS, blocks, plainText); err != nil {
		safeErr := secure.NewRedactor().Error(err)
		return fmt.Errorf("update confirmation blocks: %w", safeErr)
	}
	return nil
}

func renderConfirmationBlocks(delivery port.ConfirmationDelivery) []slackapi.Block {
	renderer, err := NewEmbeddedTemplateRenderer()
	if err != nil {
		return nil
	}
	_, blocks, err := compileConfirmationMessage(renderer, delivery)
	if err != nil {
		return nil
	}
	return blocks
}

func compileConfirmationMessage(renderer *TemplateRenderer, delivery port.ConfirmationDelivery) (string, []slackapi.Block, error) {
	return renderer.CompileMessageWithFallback("confirmation_message", TemplateContext{Values: map[string]string{
		"summary":          fmt.Sprintf(":lock: %s", neutralizeUnsafeControls(delivery.Summary)),
		"original_call_id": fmt.Sprintf("*Call ID:*\n`%s`", delivery.OriginalCallID),
		"expires_at":       fmt.Sprintf("*Expires:*\n%s UTC", delivery.Expiry.UTC().Format("15:04")),
		"wrapper_call_id":  delivery.WrapperCallID,
		"fallback_text":    confirmationFallbackText(delivery),
	}})
}

func confirmationContentDigest(delivery port.ConfirmationDelivery) string {
	return port.ConfirmationContentDigest(delivery)
}

func confirmationFallbackText(delivery port.ConfirmationDelivery) string {
	return fmt.Sprintf("Confirmation required: %s\nCall ID: %s\nExpires: %s UTC",
		neutralizeUnsafeControls(delivery.Summary), delivery.OriginalCallID, delivery.Expiry.UTC().Format("15:04"))
}

func confirmationMetadata(delivery port.ConfirmationDelivery) slackapi.SlackMetadata {
	return slackapi.SlackMetadata{
		EventType: confirmationMetadataEventType,
		EventPayload: map[string]any{
			"correlation_id": delivery.CorrelationID,
			"render_mode":    confirmationRenderMode,
			"content_sha256": confirmationContentDigest(delivery),
		},
	}
}

func normalizeInteractiveAction(callback *slackapi.InteractionCallback) (domain.ConfirmationInteractiveAction, bool) {
	if callback == nil {
		return domain.ConfirmationInteractiveAction{}, false
	}
	if callback.Type != slackapi.InteractionTypeBlockActions {
		return domain.ConfirmationInteractiveAction{}, false
	}

	blockActions := callback.ActionCallback.BlockActions
	if len(blockActions) != 1 {
		return domain.ConfirmationInteractiveAction{}, false
	}

	action := blockActions[0]
	var approved bool
	switch action.ActionID {
	case approveActionID:
		approved = true
	case rejectActionID:
		approved = false
	default:
		return domain.ConfirmationInteractiveAction{}, false
	}

	wrapperCallID := action.Value
	if wrapperCallID == "" || len(wrapperCallID) > 2048 {
		return domain.ConfirmationInteractiveAction{}, false
	}

	var key domain.ConversationKey
	teamID := callback.Team.ID
	channelID := callback.Channel.ID
	messageTS := callback.Message.Timestamp
	threadTS := callback.Message.ThreadTimestamp
	if callback.Container.MessageTs != "" {
		if messageTS != "" && messageTS != callback.Container.MessageTs {
			return domain.ConfirmationInteractiveAction{}, false
		}
		messageTS = callback.Container.MessageTs
	}
	if callback.Container.ThreadTs != "" {
		if threadTS != "" && threadTS != callback.Container.ThreadTs {
			return domain.ConfirmationInteractiveAction{}, false
		}
		threadTS = callback.Container.ThreadTs
	}
	if callback.Container.ChannelID != "" {
		if channelID != "" && channelID != callback.Container.ChannelID {
			return domain.ConfirmationInteractiveAction{}, false
		}
		channelID = callback.Container.ChannelID
	}
	if !domain.PlausibleTeamID(teamID) || !domain.PlausibleChannelID(channelID) ||
		!domain.PlausibleUserID(callback.User.ID) || !slackMessageTimestampPattern.MatchString(messageTS) ||
		(threadTS != "" && !slackMessageTimestampPattern.MatchString(threadTS)) {
		return domain.ConfirmationInteractiveAction{}, false
	}
	var correlationID, rendererMode, contentDigest string
	if callback.Message.Metadata.EventType != "" {
		if callback.Message.Metadata.EventType != confirmationMetadataEventType {
			return domain.ConfirmationInteractiveAction{}, false
		}
		var correlationOK, rendererOK, digestOK bool
		correlationID, correlationOK = callback.Message.Metadata.EventPayload["correlation_id"].(string)
		rendererMode, rendererOK = callback.Message.Metadata.EventPayload["render_mode"].(string)
		contentDigest, digestOK = callback.Message.Metadata.EventPayload["content_sha256"].(string)
		if !correlationOK || correlationID == "" || !rendererOK || rendererMode != confirmationRenderMode || !digestOK || len(contentDigest) != 64 {
			return domain.ConfirmationInteractiveAction{}, false
		}
	}

	if channelID[0] == 'D' {
		if threadTS == "" {
			key = domain.ConversationKey(fmt.Sprintf("slack:%s:dm:%s", teamID, channelID))
		} else {
			key = domain.ConversationKey(fmt.Sprintf("slack:%s:dm:%s:thread:%s", teamID, channelID, threadTS))
		}
	} else {
		key = domain.ConversationKey(fmt.Sprintf("slack:%s:channel:%s:thread:%s", teamID, channelID, threadTS))
	}

	return domain.ConfirmationInteractiveAction{
		WrapperCallID:   wrapperCallID,
		ConversationKey: key,
		Actor:           callback.User.ID,
		TeamID:          teamID,
		ChannelID:       channelID,
		MessageTS:       messageTS,
		ThreadTS:        threadTS,
		CorrelationID:   correlationID,
		RendererMode:    rendererMode,
		ContentSHA256:   contentDigest,
		Approved:        approved,
	}, true
}

func terminalConfirmationFooter(approved bool, now time.Time) string {
	if approved {
		return fmt.Sprintf("_Approved %s_", now.Format("15:04"))
	}
	return fmt.Sprintf("_Rejected %s_", now.Format("15:04"))
}

func expiredConfirmationFooter(now time.Time) string {
	return fmt.Sprintf("_Expired %s_", now.Format("15:04"))
}
