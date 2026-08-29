package slack

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/blockkit"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

const (
	confirmationRenderModeV2      = "confirmation_v2"
	confirmationRenderMode        = confirmationRenderModeV2
	approveActionID               = "local_agent.confirm.approve"
	rejectActionID                = "local_agent.confirm.reject"
	statusActionID                = "local_agent.job.status"
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
	engine    *blockkit.Engine
	renderErr error
}

func NewConfirmationPublisher(client *slackapi.Client, botUserID string, timeout time.Duration, logger port.Logger) *ConfirmationPublisher {
	var poster confirmationBlockClient
	if client != nil {
		poster = sdkConfirmationBlockClient{client: client}
	}
	return newConfirmationPublisher(poster, botUserID, timeout, logger)
}

func newConfirmationViewEngine() (*blockkit.Engine, error) {
	return newViewEngine()
}

func newConfirmationPublisher(client confirmationBlockClient, botUserID string, timeout time.Duration, logger port.Logger) *ConfirmationPublisher {
	engine, engineErr := newConfirmationViewEngine()
	if engineErr == nil {
		engineErr = engine.Register(confirmationPromptView{}, confirmationResolvedView{}, jobAcceptedView{})
	}
	return &ConfirmationPublisher{
		client: client, botUserID: botUserID, timeout: timeout, logger: loggerOrDiscard(logger),
		engine: engine, renderErr: engineErr,
	}
}

// InitializationError exposes template setup failures to the composition root.
func (p *ConfirmationPublisher) InitializationError() error {
	if p == nil {
		return errors.New("confirmation publisher is required")
	}
	return p.renderErr
}

// ActionIDs returns the action IDs declared by the confirmation view engine.
func (p *ConfirmationPublisher) ActionIDs() []string {
	if p == nil || p.engine == nil {
		return nil
	}
	return p.engine.ActionIDs()
}

func (p *ConfirmationPublisher) PublishConfirmation(ctx context.Context, delivery port.ConfirmationDelivery) (port.ConfirmationPublishedResult, error) {
	if p == nil || p.client == nil {
		return port.ConfirmationPublishedResult{}, errors.New("slack posting client is required for confirmation publishing")
	}
	if delivery.ChannelID == "" {
		return port.ConfirmationPublishedResult{}, errors.New("slack channel is required for confirmation publishing")
	}

	renderMode := confirmationRenderModeForDelivery(delivery)
	if renderMode == "" {
		return port.ConfirmationPublishedResult{}, errors.New("unsupported confirmation renderer mode")
	}
	fallbackText, blocks, err := compileConfirmationMessageV2(p.engine, delivery)
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
		return port.ConfirmationPublishedResult{}, errors.New("slack published confirmation without a message timestamp")
	}
	return port.ConfirmationPublishedResult{SlackMessageTS: timestamp}, nil
}

func (p *ConfirmationPublisher) RecoverConfirmation(ctx context.Context, delivery port.ConfirmationDelivery) (port.ConfirmationPublishedResult, bool, error) {
	if p == nil || p.client == nil {
		return port.ConfirmationPublishedResult{}, false, errors.New("slack client is required for confirmation recovery")
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
	renderMode := confirmationRenderModeForDelivery(delivery)
	if renderMode == "" {
		return port.ConfirmationPublishedResult{}, false, errors.New("unsupported confirmation renderer mode")
	}
	expectedDigest := confirmationContentDigest(delivery)
	var timestamp string
	for _, message := range messages {
		if message.Metadata.EventType != confirmationMetadataEventType {
			continue
		}
		correlationID, _ := message.Metadata.EventPayload["correlation_id"].(string)
		if correlationID != delivery.CorrelationID {
			continue
		}
		messageRenderMode, _ := message.Metadata.EventPayload["render_mode"].(string)
		contentDigest, _ := message.Metadata.EventPayload["content_sha256"].(string)
		if timestamp != "" || message.User != p.botUserID || message.Hidden || message.Edited != nil || len(message.Files) != 0 ||
			message.Timestamp == "" || messageRenderMode != renderMode || contentDigest != expectedDigest {
			return port.ConfirmationPublishedResult{}, false, errors.New("slack confirmation recovery evidence is ambiguous or invalid")
		}
		timestamp = message.Timestamp
	}
	if hasMore {
		return port.ConfirmationPublishedResult{}, false, errors.New("slack confirmation recovery history is incomplete")
	}
	if timestamp == "" {
		return port.ConfirmationPublishedResult{}, false, nil
	}
	return port.ConfirmationPublishedResult{SlackMessageTS: timestamp}, true, nil
}

func (p *ConfirmationPublisher) UpdateConfirmation(ctx context.Context, delivery port.ConfirmationDelivery, terminalText string) error {
	if p == nil || p.client == nil {
		return errors.New("slack update client is required for confirmation update")
	}
	if delivery.SlackMessageTS == "" {
		return errors.New("slack message timestamp is required for confirmation update")
	}
	if delivery.ChannelID == "" {
		return errors.New("slack channel is required for confirmation update")
	}

	now := time.Now().UTC()
	fallbackText, blocks, err := compileConfirmationResolvedMessage(p.engine, delivery, now, terminalText)
	if err != nil {
		if p.renderErr != nil {
			err = p.renderErr
		}
		return fmt.Errorf("render resolved confirmation template: %w", err)
	}

	callCtx := ctx
	cancel := func() {}
	if p.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, p.timeout)
	}
	defer cancel()

	if err := p.client.UpdateBlocks(callCtx, delivery.ChannelID, delivery.SlackMessageTS, blocks, fallbackText); err != nil {
		safeErr := secure.NewRedactor().Error(err)
		return fmt.Errorf("update confirmation blocks: %w", safeErr)
	}
	return nil
}

type confirmationPromptView struct {
	Summary       string          `bk:"summary"`
	CallID        string          `bk:"call_id"`
	WrapperCallID string          `bk:"wrapper_call_id"`
	ExpiresAt     time.Time       `bk:"expires_at"`
	Project       string          `bk:"project,omitempty"`
	Task          string          `bk:"task,omitempty"`
	Workstream    []blockkit.Pair `bk:"workstream,omitempty"`
	Payload       string          `bk:"payload,omitempty"`
}

func (confirmationPromptView) Template() string { return "confirmation.prompt" }

type confirmationResolvedView struct {
	Status    string    `bk:"status"`
	Summary   string    `bk:"summary"`
	CallID    string    `bk:"call_id"`
	UpdatedAt time.Time `bk:"updated_at"`
	Result    string    `bk:"result,omitempty"`
}

func (confirmationResolvedView) Template() string { return "confirmation.resolved" }

func compileConfirmationMessageV2(engine *blockkit.Engine, delivery port.ConfirmationDelivery) (string, []slackapi.Block, error) {
	display := buildConfirmationDisplay(delivery)
	message, err := engine.Message(confirmationPromptView{
		Summary: delivery.Summary, CallID: delivery.OriginalCallID, WrapperCallID: delivery.WrapperCallID,
		ExpiresAt: delivery.Expiry, Project: display.Project, Task: display.ProposedTask,
		Workstream: display.WorkstreamData, Payload: display.Payload,
	})
	if err != nil {
		return "", nil, err
	}
	return message.FallbackText, message.Blocks, nil
}

func compileConfirmationResolvedMessage(engine *blockkit.Engine, delivery port.ConfirmationDelivery, updatedAt time.Time, terminalText string) (string, []slackapi.Block, error) {
	if strings.TrimSpace(terminalText) == "" {
		terminalText = ""
	}
	message, err := engine.Message(confirmationResolvedView{
		Status: confirmationResolvedStatus(delivery.Status), Summary: delivery.Summary, CallID: delivery.OriginalCallID,
		UpdatedAt: updatedAt, Result: terminalText,
	})
	if err != nil {
		return "", nil, err
	}
	return message.FallbackText, message.Blocks, nil
}

func confirmationResolvedStatus(status port.ConfirmationDeliveryStatus) string {
	if status == port.ConfirmationConsumed {
		return string(port.ConfirmationApproved)
	}
	return string(status)
}

type confirmationDisplay struct {
	Project        string
	ProposedTask   string
	WorkstreamData []blockkit.Pair
	Payload        string
}

func buildConfirmationDisplay(delivery port.ConfirmationDelivery) confirmationDisplay {
	if strings.TrimSpace(delivery.Payload) == "" {
		return confirmationDisplay{}
	}

	var payload map[string]jsontext.Value
	if err := json.Unmarshal([]byte(delivery.Payload), &payload); err != nil {
		return confirmationDisplay{Payload: delivery.Payload}
	}
	display := confirmationDisplay{
		Project:        confirmationPayloadString(payload, "project"),
		WorkstreamData: confirmationWorkstreamData(payload),
	}
	display.ProposedTask = confirmationPayloadString(payload, "task")
	if display.ProposedTask == "" {
		if raw := payload["task"]; len(raw) > 0 {
			display.ProposedTask = confirmationTaskObjectText(raw)
		}
	}
	if display.ProposedTask == "" {
		display.ProposedTask = confirmationPayloadString(payload, "objective")
	}
	display.ProposedTask = truncateConfirmationText(
		display.ProposedTask, maxRendererCompositionTextLength-utf8.RuneCountInString("Proposed task:\n"),
	)
	return display
}

func confirmationPayloadValue(payload map[string]jsontext.Value, key string) (jsontext.Value, bool) {
	if value, ok := payload[key]; ok {
		return value, true
	}
	for candidate, value := range payload {
		if strings.EqualFold(candidate, key) {
			return value, true
		}
	}
	return nil, false
}

func confirmationPayloadString(payload map[string]jsontext.Value, key string) string {
	raw, ok := confirmationPayloadValue(payload, key)
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return ""
	}
	return value
}

func confirmationTaskObjectText(raw jsontext.Value) string {
	var task map[string]jsontext.Value
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		return string(raw)
	}
	if description := confirmationPayloadString(task, "description"); description != "" {
		return description
	}
	return string(raw)
}

func confirmationWorkstreamData(payload map[string]jsontext.Value) []blockkit.Pair {
	keys := []struct {
		key   string
		label string
	}{
		{"workstream_id", "Workstream ID"},
		{"expected_revision", "Expected revision"},
		{"action", "Action"},
		{"task_id", "Task ID"},
		{"current_phase", "Current phase"},
		{"payload_digest", "Payload digest"},
		{"source_result_identities", "Source result identities"},
	}
	var pairs []blockkit.Pair
	for _, item := range keys {
		raw, ok := confirmationPayloadValue(payload, item.key)
		if !ok || string(raw) == "null" {
			continue
		}
		value := confirmationPayloadString(payload, item.key)
		if value == "" {
			value = string(raw)
		}
		pairs = append(pairs, blockkit.Pair{Label: item.label, Value: value})
	}
	return pairs
}

func truncateConfirmationText(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	marker := "..."
	if limit <= utf8.RuneCountInString(marker) {
		return string([]rune(value)[:limit])
	}
	runes := []rune(value)
	return string(runes[:limit-utf8.RuneCountInString(marker)]) + marker
}

func confirmationContentDigest(delivery port.ConfirmationDelivery) string {
	return port.ConfirmationContentDigest(delivery)
}

func confirmationRenderModeForDelivery(delivery port.ConfirmationDelivery) string {
	switch delivery.RendererMode {
	case "", confirmationRenderModeV2:
		return confirmationRenderModeV2
	default:
		return ""
	}
}

func isConfirmationRendererMode(renderMode string) bool {
	return renderMode == confirmationRenderModeV2
}

func confirmationMetadata(delivery port.ConfirmationDelivery) slackapi.SlackMetadata {
	renderMode := confirmationRenderModeForDelivery(delivery)
	return slackapi.SlackMetadata{
		EventType: confirmationMetadataEventType,
		EventPayload: map[string]any{
			"correlation_id": delivery.CorrelationID,
			"render_mode":    renderMode,
			"content_sha256": confirmationContentDigest(delivery),
		},
	}
}

func normalizeInteractiveAction(callback *slackapi.InteractionCallback) (domain.ConfirmationInteractiveAction, bool) {
	if callback == nil || callback.Type != slackapi.InteractionTypeBlockActions || len(callback.ActionCallback.BlockActions) != 1 {
		return domain.ConfirmationInteractiveAction{}, false
	}
	action := callback.ActionCallback.BlockActions[0]
	if action == nil {
		return domain.ConfirmationInteractiveAction{}, false
	}
	var approved bool
	switch action.ActionID {
	case approveActionID:
		approved = true
	case rejectActionID:
		approved = false
	default:
		return domain.ConfirmationInteractiveAction{}, false
	}
	normalized, ok := normalizeConfirmationActionContext(callback, action.Value)
	if !ok {
		return domain.ConfirmationInteractiveAction{}, false
	}
	normalized.Approved = approved
	return normalized, true
}

func normalizeJobStatusAction(callback *slackapi.InteractionCallback) (domain.ConfirmationInteractiveAction, bool) {
	if callback == nil || callback.Type != slackapi.InteractionTypeBlockActions || len(callback.ActionCallback.BlockActions) != 1 {
		return domain.ConfirmationInteractiveAction{}, false
	}
	action := callback.ActionCallback.BlockActions[0]
	if action == nil || action.ActionID != statusActionID {
		return domain.ConfirmationInteractiveAction{}, false
	}
	return normalizeConfirmationActionContext(callback, action.Value)
}

func normalizeConfirmationActionContext(callback *slackapi.InteractionCallback, wrapperCallID string) (domain.ConfirmationInteractiveAction, bool) {
	if wrapperCallID == "" || utf8.RuneCountInString(wrapperCallID) > maxRendererOptionValueLength {
		return domain.ConfirmationInteractiveAction{}, false
	}

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
		if !correlationOK || correlationID == "" || !rendererOK || !isConfirmationRendererMode(rendererMode) || !digestOK || len(contentDigest) != 64 {
			return domain.ConfirmationInteractiveAction{}, false
		}
	}

	var key domain.ConversationKey
	switch channelID[0] {
	case 'D':
		if threadTS == "" {
			key = domain.ConversationKey(fmt.Sprintf("slack:%s:dm:%s", teamID, channelID))
		} else {
			key = domain.ConversationKey(fmt.Sprintf("slack:%s:dm:%s:thread:%s", teamID, channelID, threadTS))
		}
	case 'C':
		key = domain.ConversationKey(fmt.Sprintf("slack:%s:channel:%s:thread:%s", teamID, channelID, threadTS))
	case 'G':
		key = domain.ConversationKey(fmt.Sprintf("slack:%s:group:%s:thread:%s", teamID, channelID, threadTS))
	default:
		return domain.ConfirmationInteractiveAction{}, false
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
	}, true
}
