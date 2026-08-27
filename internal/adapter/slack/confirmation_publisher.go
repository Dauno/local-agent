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

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

const (
	confirmationRenderModeV1      = "confirmation_v1"
	confirmationRenderModeV2      = "confirmation_v2"
	confirmationRenderMode        = confirmationRenderModeV2
	confirmationTemplateV1        = "confirmation_message"
	confirmationTemplateV2        = "confirmation_message_v2"
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
		return port.ConfirmationPublishedResult{}, errors.New("slack posting client is required for confirmation publishing")
	}
	if delivery.ChannelID == "" {
		return port.ConfirmationPublishedResult{}, errors.New("slack channel is required for confirmation publishing")
	}

	renderMode := confirmationRenderModeForDelivery(delivery)
	if renderMode == "" {
		return port.ConfirmationPublishedResult{}, errors.New("unsupported confirmation renderer mode")
	}
	fallbackText, blocks, err := compileConfirmationMessageForMode(p.renderer, delivery, renderMode)
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
	expectedDigest := confirmationContentDigestForMode(delivery, renderMode)
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

	var statusText string
	now := time.Now().UTC()
	switch delivery.Status {
	case port.ConfirmationConsumed, port.ConfirmationApproved:
		statusText = "Confirmation approved"
	case port.ConfirmationRejected:
		statusText = "Confirmation rejected"
	case port.ConfirmationExpired:
		statusText = "Confirmation expired"
	case port.ConfirmationFailed:
		statusText = "Confirmation failed"
	default:
		return fmt.Errorf("confirmation delivery status %s is not terminal", delivery.Status)
	}

	headerText := fmt.Sprintf("*%s*\n%s", statusText, escapeSlackMrkdwn(delivery.Summary))
	headerBlock := slackapi.NewSectionBlock(slackapi.NewTextBlockObject("mrkdwn", headerText, false, false), nil, nil)

	detailFields := []*slackapi.TextBlockObject{
		slackapi.NewTextBlockObject("mrkdwn", fmt.Sprintf("*Call ID:*\n`%s`", escapeSlackMrkdwn(delivery.OriginalCallID)), false, false),
		slackapi.NewTextBlockObject("mrkdwn", fmt.Sprintf("*Status:*\n%s", statusText), false, false),
		slackapi.NewTextBlockObject("mrkdwn", fmt.Sprintf("*Updated:*\n%s UTC", now.Format("15:04")), false, false),
	}
	detailBlock := slackapi.NewSectionBlock(nil, detailFields, nil)

	blocks := []slackapi.Block{headerBlock, detailBlock}
	if text := strings.TrimSpace(terminalText); text != "" {
		terminal := "Confirmation result:\n" + neutralizeUnsafeControls(text)
		if utf8.RuneCountInString(terminal) > maxRendererCompositionTextLength {
			return fmt.Errorf("confirmation result exceeds %d character limit", maxRendererCompositionTextLength)
		}
		blocks = append(blocks, slackapi.NewSectionBlock(slackapi.NewTextBlockObject("plain_text", terminal, false, false), nil, nil))
	}
	if err := validateCompiledBlocks(confirmationTemplateV2, blocks, false); err != nil {
		return fmt.Errorf("validate confirmation update blocks: %w", err)
	}

	callCtx := ctx
	cancel := func() {}
	if p.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, p.timeout)
	}
	defer cancel()

	plainText := confirmationUpdateFallback(statusText, delivery, terminalText)

	if err := p.client.UpdateBlocks(callCtx, delivery.ChannelID, delivery.SlackMessageTS, blocks, plainText); err != nil {
		safeErr := secure.NewRedactor().Error(err)
		return fmt.Errorf("update confirmation blocks: %w", safeErr)
	}
	return nil
}

func confirmationUpdateFallback(statusText string, delivery port.ConfirmationDelivery, terminalText string) string {
	parts := []string{
		statusText,
		"Summary: " + neutralizeUnsafeControls(delivery.Summary),
		"Call ID: " + neutralizeUnsafeControls(delivery.OriginalCallID),
	}
	if text := strings.TrimSpace(terminalText); text != "" {
		parts = append(parts, "Result: "+neutralizeUnsafeControls(text))
	}
	return truncateConfirmationText(strings.Join(parts, "\n"), maxFallbackText)
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
	return compileConfirmationMessageV2(renderer, delivery)
}

func compileConfirmationMessageForMode(renderer *TemplateRenderer, delivery port.ConfirmationDelivery, renderMode string) (string, []slackapi.Block, error) {
	switch renderMode {
	case confirmationRenderModeV1:
		return compileConfirmationMessageV1(renderer, delivery)
	case confirmationRenderModeV2:
		return compileConfirmationMessageV2(renderer, delivery)
	default:
		return "", nil, errors.New("unsupported confirmation renderer mode")
	}
}

func compileConfirmationMessageV1(renderer *TemplateRenderer, delivery port.ConfirmationDelivery) (string, []slackapi.Block, error) {
	fallback, blocks, err := renderer.CompileMessageWithFallback(confirmationTemplateV1, TemplateContext{Values: map[string]string{
		"summary":          fmt.Sprintf(":lock: %s", neutralizeUnsafeControls(delivery.Summary)),
		"original_call_id": fmt.Sprintf("*Call ID:*\n`%s`", delivery.OriginalCallID),
		"expires_at":       fmt.Sprintf("*Expires:*\n%s UTC", delivery.Expiry.UTC().Format("15:04")),
		"wrapper_call_id":  delivery.WrapperCallID,
		"fallback_text":    confirmationFallbackTextV1(delivery),
	}})
	if err != nil || strings.TrimSpace(delivery.Payload) == "" {
		return fallback, blocks, err
	}
	payloadBlocks := make([]slackapi.Block, 0)
	for _, chunk := range confirmationPayloadChunks(neutralizeUnsafeControls(delivery.Payload), 2800) {
		payloadBlocks = append(payloadBlocks, slackapi.NewSectionBlock(slackapi.NewTextBlockObject("plain_text", "Proposed payload:\n"+chunk, false, false), nil, nil))
	}
	blocks = append(payloadBlocks, blocks...)
	return fallback, blocks, nil
}

func compileConfirmationMessageV2(renderer *TemplateRenderer, delivery port.ConfirmationDelivery) (string, []slackapi.Block, error) {
	display := buildConfirmationDisplay(delivery)
	fallback, blocks, err := renderer.CompileMessageWithFallback(confirmationTemplateV2, TemplateContext{Values: map[string]string{
		"title_summary":    confirmationTitleSummary(delivery.Summary),
		"original_call_id": fmt.Sprintf("*Call ID:*\n`%s`", escapeSlackMrkdwn(delivery.OriginalCallID)),
		"expires_at":       fmt.Sprintf("*Expires:*\n%s UTC", delivery.Expiry.UTC().Format("15:04")),
		"project":          display.Project,
		"proposed_task":    display.ProposedTask,
		"wrapper_call_id":  delivery.WrapperCallID,
		"fallback_text":    confirmationFallbackTextV2(delivery, display),
	}})
	if err != nil {
		return fallback, blocks, err
	}
	if display.WorkstreamData != "" {
		workstreamBlocks := confirmationWorkstreamBlocks(display.WorkstreamData)
		if len(blocks)+len(workstreamBlocks) > maxBlocksPerMessage {
			return "", nil, fmt.Errorf("confirmation exceeds %d block limit", maxBlocksPerMessage)
		}
		for index, block := range blocks {
			if _, ok := block.(*slackapi.ActionBlock); !ok {
				continue
			}
			withWorkstream := make([]slackapi.Block, 0, len(blocks)+len(workstreamBlocks))
			withWorkstream = append(withWorkstream, blocks[:index]...)
			withWorkstream = append(withWorkstream, workstreamBlocks...)
			withWorkstream = append(withWorkstream, blocks[index:]...)
			blocks = withWorkstream
			break
		}
	}
	if err := validateCompiledBlocks(confirmationTemplateV2, blocks, false); err != nil {
		return "", nil, fmt.Errorf("validate confirmation blocks: %w", err)
	}
	return fallback, blocks, nil
}

type confirmationDisplay struct {
	Project        string
	ProposedTask   string
	WorkstreamData string
}

func buildConfirmationDisplay(delivery port.ConfirmationDelivery) confirmationDisplay {
	display := confirmationDisplay{
		Project:      "Project: not provided",
		ProposedTask: "Proposed task: not provided",
	}
	if strings.TrimSpace(delivery.Payload) == "" {
		return display
	}

	var payload map[string]jsontext.Value
	if err := json.Unmarshal([]byte(delivery.Payload), &payload); err != nil {
		display.ProposedTask = "Proposed task:\n" + neutralizeUnsafeControls(delivery.Payload)
		return display
	}
	if project := confirmationPayloadString(payload, "project"); project != "" {
		display.Project = "Project: " + neutralizeUnsafeControls(project)
	}
	task := confirmationPayloadString(payload, "task")
	if task == "" {
		if raw := payload["task"]; len(raw) > 0 {
			task = confirmationTaskObjectText(raw)
		}
	}
	if task == "" {
		task = confirmationPayloadString(payload, "objective")
	}
	if task != "" {
		display.ProposedTask = "Proposed task:\n" + neutralizeUnsafeControls(task)
	}
	display.WorkstreamData = confirmationWorkstreamData(payload)
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

func confirmationWorkstreamData(payload map[string]jsontext.Value) string {
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
	var lines []string
	for _, item := range keys {
		raw, ok := confirmationPayloadValue(payload, item.key)
		if !ok || string(raw) == "null" {
			continue
		}
		value := confirmationPayloadString(payload, item.key)
		if value == "" {
			value = string(raw)
		}
		lines = append(lines, item.label+": "+neutralizeUnsafeControls(value))
	}
	return strings.Join(lines, "\n")
}

func confirmationWorkstreamBlocks(data string) []slackapi.Block {
	const prefix = "Workstream data:\n"
	chunks := confirmationPayloadChunks(data, maxRendererCompositionTextLength-utf8.RuneCountInString(prefix))
	blocks := make([]slackapi.Block, 0, len(chunks))
	for _, chunk := range chunks {
		blocks = append(blocks, slackapi.NewSectionBlock(slackapi.NewTextBlockObject("plain_text", prefix+chunk, false, false), nil, nil))
	}
	return blocks
}

func confirmationTitleSummary(summary string) string {
	return "*Confirmation required*\n" + escapeSlackMrkdwn(summary)
}

func confirmationFallbackTextV2(delivery port.ConfirmationDelivery, display confirmationDisplay) string {
	parts := []string{
		"Confirmation required: " + neutralizeUnsafeControls(delivery.Summary),
		"Call ID: " + neutralizeUnsafeControls(delivery.OriginalCallID),
		"Expires: " + delivery.Expiry.UTC().Format("15:04") + " UTC",
		display.Project,
		display.ProposedTask,
	}
	if display.WorkstreamData != "" {
		parts = append(parts, "Workstream data:\n"+display.WorkstreamData)
	}
	return truncateConfirmationText(strings.Join(parts, "\n"), maxFallbackText)
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

func escapeSlackMrkdwn(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
}

func confirmationPayloadChunks(value string, maxRunes int) []string {
	runes := []rune(value)
	if len(runes) == 0 || maxRunes <= 0 {
		return nil
	}
	chunks := make([]string, 0, (len(runes)+maxRunes-1)/maxRunes)
	for len(runes) > 0 {
		end := min(maxRunes, len(runes))
		chunks = append(chunks, string(runes[:end]))
		runes = runes[end:]
	}
	return chunks
}

func confirmationContentDigest(delivery port.ConfirmationDelivery) string {
	return confirmationContentDigestForMode(delivery, confirmationRenderModeForDelivery(delivery))
}

func confirmationContentDigestForMode(delivery port.ConfirmationDelivery, renderMode string) string {
	switch renderMode {
	case confirmationRenderModeV2:
		return port.ConfirmationContentDigestV2(delivery)
	case confirmationRenderModeV1:
		return port.ConfirmationContentDigest(delivery)
	default:
		return ""
	}
}

func confirmationRenderModeForDelivery(delivery port.ConfirmationDelivery) string {
	if delivery.RendererMode == "" {
		return confirmationRenderModeV2
	}
	if delivery.RendererMode == confirmationRenderModeV1 || delivery.RendererMode == confirmationRenderModeV2 {
		return delivery.RendererMode
	}
	return ""
}

func isConfirmationRendererMode(renderMode string) bool {
	return renderMode == confirmationRenderModeV1 || renderMode == confirmationRenderModeV2
}

func confirmationFallbackTextV1(delivery port.ConfirmationDelivery) string {
	text := fmt.Sprintf("Confirmation required: %s\nCall ID: %s\nExpires: %s UTC",
		neutralizeUnsafeControls(delivery.Summary), delivery.OriginalCallID, delivery.Expiry.UTC().Format("15:04"))
	if delivery.Payload != "" {
		withPayload := text + "\nProposed payload: " + neutralizeUnsafeControls(delivery.Payload)
		if utf8.RuneCountInString(withPayload) <= maxFallbackText {
			return withPayload
		}
		text += "\nThe complete proposed payload is shown in the message blocks before the approval buttons."
	}
	return text
}

func confirmationFallbackText(delivery port.ConfirmationDelivery) string {
	display := buildConfirmationDisplay(delivery)
	return confirmationFallbackTextV2(delivery, display)
}

func confirmationMetadata(delivery port.ConfirmationDelivery) slackapi.SlackMetadata {
	renderMode := confirmationRenderModeForDelivery(delivery)
	return slackapi.SlackMetadata{
		EventType: confirmationMetadataEventType,
		EventPayload: map[string]any{
			"correlation_id": delivery.CorrelationID,
			"render_mode":    renderMode,
			"content_sha256": confirmationContentDigestForMode(delivery, renderMode),
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
