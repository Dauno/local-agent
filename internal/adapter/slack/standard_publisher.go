package slack

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const (
	progressMetadataEventType    = "local_agent_progress"
	promptMetadataEventType      = "local_agent_suggested_prompts"
	onboardingMetadataEventType  = "local_agent_onboarding"
	incrementalMetadataEventType = "local_agent_incremental"
	progressRecoveryLimit        = 100
	standardIncrementalRenderer  = "standard_incremental_v1"
)

type standardMessageClient interface {
	PostStandard(context.Context, string, string, string, slackapi.SlackMetadata) (string, error)
	UpdateStandard(context.Context, string, string, string, slackapi.SlackMetadata) error
	StandardMessages(context.Context, string, string, int) ([]slackapi.Message, bool, error)
}

type sdkStandardMessageClient struct {
	client *slackapi.Client
}

func (c sdkStandardMessageClient) PostStandard(ctx context.Context, channelID, threadTS, markdown string, metadata slackapi.SlackMetadata) (string, error) {
	options := []slackapi.MsgOption{
		slackapi.MsgOptionMarkdownText(markdown),
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

func (c sdkStandardMessageClient) UpdateStandard(ctx context.Context, channelID, messageTS, markdown string, metadata slackapi.SlackMetadata) error {
	_, _, _, err := c.client.UpdateMessageContext(ctx, channelID, messageTS,
		slackapi.MsgOptionMarkdownText(markdown), slackapi.MsgOptionMetadata(metadata))
	return err
}

func (c sdkStandardMessageClient) StandardMessages(ctx context.Context, channelID, threadTS string, limit int) ([]slackapi.Message, bool, error) {
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

type StandardPublisher struct {
	client         standardMessageClient
	blockClient    blockPostClient
	botUserID      string
	timeout        time.Duration
	renderer       *TemplateRenderer
	renderErr      error
	progressLabels map[domain.ProgressState]string
}

func NewStandardPublisher(client *slackapi.Client, botUserID string, timeout time.Duration, progressLabels map[domain.ProgressState]string) *StandardPublisher {
	var standard standardMessageClient
	var blocks blockPostClient
	if client != nil {
		standard = sdkStandardMessageClient{client: client}
		blocks = sdkPostClient{client: client}
	}
	renderer, renderErr := NewEmbeddedTemplateRenderer()
	return &StandardPublisher{client: standard, blockClient: blocks, botUserID: botUserID, timeout: timeout, renderer: renderer, renderErr: renderErr, progressLabels: progressLabels}
}

func (p *StandardPublisher) PublishProgress(ctx context.Context, target domain.ReplyTarget, operation domain.ProgressOperation) (port.PublishedResponse, error) {
	if err := p.validateProgress(operation); err != nil {
		return port.PublishedResponse{}, err
	}
	markdown, err := p.progressMarkdown(operation.State)
	if err != nil {
		return port.PublishedResponse{}, err
	}
	callCtx, cancel := slackTimeout(ctx, p.timeout)
	defer cancel()
	timestamp, err := p.client.PostStandard(callCtx, target.ChannelID, target.ThreadTS, markdown, progressMetadata(operation))
	if err != nil {
		return port.PublishedResponse{}, fmt.Errorf("publish Slack progress: %w", err)
	}
	return port.PublishedResponse{LastMessageTS: timestamp}, nil
}

func (p *StandardPublisher) UpdateProgress(ctx context.Context, operation domain.ProgressOperation) error {
	if err := p.validateProgress(operation); err != nil {
		return err
	}
	if operation.MessageTS == "" {
		return errors.New("Slack progress message timestamp is required")
	}
	markdown, err := p.progressMarkdown(operation.State)
	if err != nil {
		return err
	}
	callCtx, cancel := slackTimeout(ctx, p.timeout)
	defer cancel()
	if err := p.client.UpdateStandard(callCtx, operation.ChannelID, operation.MessageTS, markdown, progressMetadata(operation)); err != nil {
		return fmt.Errorf("update Slack progress: %w", err)
	}
	return nil
}

func (p *StandardPublisher) RecoverProgress(ctx context.Context, operation domain.ProgressOperation) (port.PublishedResponse, bool, error) {
	if err := p.validateProgress(operation); err != nil {
		return port.PublishedResponse{}, false, err
	}
	callCtx, cancel := slackTimeout(ctx, p.timeout)
	defer cancel()
	messages, hasMore, err := p.client.StandardMessages(callCtx, operation.ChannelID, operation.ThreadTS, progressRecoveryLimit)
	if err != nil {
		return port.PublishedResponse{}, false, fmt.Errorf("recover Slack progress: %w", err)
	}
	if hasMore {
		return port.PublishedResponse{}, false, errors.New("recover Slack progress: bounded history is incomplete")
	}
	var match string
	for _, message := range messages {
		if message.User != p.botUserID || message.Metadata.EventType != progressMetadataEventType {
			continue
		}
		operationID, _ := message.Metadata.EventPayload["operation_id"].(string)
		if operationID != operation.ID {
			continue
		}
		if match != "" {
			return port.PublishedResponse{}, false, errors.New("recover Slack progress: duplicate operation metadata")
		}
		match = message.Timestamp
	}
	return port.PublishedResponse{LastMessageTS: match}, match != "", nil
}

func (p *StandardPublisher) PublishSuggestedPrompts(ctx context.Context, target domain.ReplyTarget, deliveryID string, prompts []string) (port.PublishedResponse, error) {
	if p == nil || p.client == nil {
		return port.PublishedResponse{}, errors.New("Slack standard publisher is required")
	}
	if target.ChannelID == "" || target.ThreadTS == "" || deliveryID == "" || len(prompts) == 0 {
		return port.PublishedResponse{}, errors.New("Slack suggested prompt identity and content are required")
	}
	var text strings.Builder
	text.WriteString("**Prueba con una de estas solicitudes:**")
	for _, prompt := range prompts {
		text.WriteString("\n- ")
		text.WriteString(prompt)
	}
	markdown := neutralizeUnsafeControls(text.String())
	if len([]rune(markdown)) > SlackMarkdownChunkRunes {
		return port.PublishedResponse{}, errors.New("Slack suggested prompts exceed one message")
	}
	callCtx, cancel := slackTimeout(ctx, p.timeout)
	defer cancel()
	metadata := slackapi.SlackMetadata{EventType: promptMetadataEventType, EventPayload: map[string]any{"delivery_id": deliveryID}}
	timestamp, err := p.client.PostStandard(callCtx, target.ChannelID, target.ThreadTS, markdown, metadata)
	if err != nil {
		return port.PublishedResponse{}, fmt.Errorf("publish Slack suggested prompts: %w", err)
	}
	return port.PublishedResponse{LastMessageTS: timestamp}, nil
}

func (p *StandardPublisher) PublishOnboarding(ctx context.Context, target domain.ReplyTarget, request port.OnboardingPublishRequest) (port.PublishedResponse, error) {
	if p == nil || p.blockClient == nil {
		return port.PublishedResponse{}, errors.New("Slack onboarding publisher is required")
	}
	if target.ChannelID == "" || request.DeliveryID == "" || !domain.PlausibleUserID(request.Actor) {
		return port.PublishedResponse{}, errors.New("Slack onboarding identity is required")
	}
	if err := validateOnboardingPrompts(request.SuggestedPrompts); err != nil {
		return port.PublishedResponse{}, err
	}
	if p.renderErr != nil || p.renderer == nil {
		if p.renderErr != nil {
			return port.PublishedResponse{}, fmt.Errorf("initialize onboarding template renderer: %w", p.renderErr)
		}
		return port.PublishedResponse{}, errors.New("onboarding template renderer is required")
	}
	builderContext, err := encodeBuilderInteractionContext(request.Actor, request.ConversationKey)
	if err != nil {
		return port.PublishedResponse{}, fmt.Errorf("encode onboarding interaction context: %w", err)
	}
	prompts := make([]string, len(request.SuggestedPrompts))
	for index, prompt := range request.SuggestedPrompts {
		prompts[index] = neutralizeUnsafeControls(prompt)
	}
	fallback, blocks, err := compileOnboardingMessage(p.renderer, builderContext, prompts)
	if err != nil {
		return port.PublishedResponse{}, fmt.Errorf("render onboarding message: %w", err)
	}
	callCtx, cancel := slackTimeout(ctx, p.timeout)
	defer cancel()
	timestamp, err := p.blockClient.PostBlocks(callCtx, target.ChannelID, fallback, blocks, onboardingMetadata(request.DeliveryID), target.ThreadTS)
	if err != nil {
		return port.PublishedResponse{}, fmt.Errorf("publish Slack onboarding: %w", err)
	}
	if timestamp == "" {
		return port.PublishedResponse{}, errors.New("Slack onboarding publisher returned no timestamp")
	}
	return port.PublishedResponse{LastMessageTS: timestamp}, nil
}

func (p *StandardPublisher) RecoverOnboarding(ctx context.Context, target domain.ReplyTarget, deliveryID string) (port.PublishedResponse, bool, error) {
	if p == nil || p.client == nil {
		return port.PublishedResponse{}, false, errors.New("Slack onboarding recovery client is required")
	}
	if target.ChannelID == "" || deliveryID == "" {
		return port.PublishedResponse{}, false, errors.New("Slack onboarding recovery identity is required")
	}
	callCtx, cancel := slackTimeout(ctx, p.timeout)
	defer cancel()
	messages, hasMore, err := p.client.StandardMessages(callCtx, target.ChannelID, target.ThreadTS, progressRecoveryLimit)
	if err != nil {
		return port.PublishedResponse{}, false, fmt.Errorf("recover Slack onboarding: %w", err)
	}
	if hasMore {
		return port.PublishedResponse{}, false, errors.New("recover Slack onboarding: bounded history is incomplete")
	}
	metadata := onboardingMetadata(deliveryID)
	var match string
	for _, message := range messages {
		if message.User != p.botUserID || message.Metadata.EventType != metadata.EventType {
			continue
		}
		candidate, _ := message.Metadata.EventPayload["delivery_id"].(string)
		if candidate != deliveryID {
			continue
		}
		if match != "" {
			return port.PublishedResponse{}, false, errors.New("recover Slack onboarding: duplicate delivery metadata")
		}
		match = message.Timestamp
	}
	return port.PublishedResponse{LastMessageTS: match}, match != "", nil
}

func onboardingMetadata(deliveryID string) slackapi.SlackMetadata {
	return slackapi.SlackMetadata{EventType: onboardingMetadataEventType, EventPayload: map[string]any{"delivery_id": deliveryID}}
}

func validateOnboardingPrompts(prompts []string) error {
	if len(prompts) > 5 {
		return errors.New("Slack onboarding supports at most five suggested prompts")
	}
	for index, prompt := range prompts {
		if strings.TrimSpace(prompt) == "" || strings.ContainsAny(prompt, "\r\n\x00") || len([]rune(prompt)) > 200 {
			return fmt.Errorf("Slack onboarding prompt %d is invalid", index)
		}
	}
	return nil
}

func (p *StandardPublisher) validateProgress(operation domain.ProgressOperation) error {
	if p == nil || p.client == nil {
		return errors.New("Slack standard publisher is required")
	}
	if p.botUserID == "" || operation.ID == "" || operation.ChannelID == "" || operation.ThreadTS == "" {
		return errors.New("Slack progress identity is required")
	}
	if p.progressLabel(operation.State) == "" {
		return fmt.Errorf("unsupported Slack progress state %q", operation.State)
	}
	return nil
}

func progressMetadata(operation domain.ProgressOperation) slackapi.SlackMetadata {
	return slackapi.SlackMetadata{EventType: progressMetadataEventType, EventPayload: map[string]any{
		"operation_id": operation.ID,
		"state":        string(operation.State),
	}}
}

// defaultProgressLabels are the built-in Slack progress labels; configured
// labels overlay them per state.
var defaultProgressLabels = map[domain.ProgressState]string{
	domain.ProgressWorking:             "Working",
	domain.ProgressWaitingConfirmation: "Waiting for approval",
	domain.ProgressFinalizing:          "Finalizing",
	domain.ProgressCleared:             "Completed",
	domain.ProgressFailed:              "Interrupted",
	domain.ProgressInterrupted:         "Interrupted",
}

// ResolveProgressLabels overlays configured labels onto the built-in defaults.
// A missing or empty configured value keeps the default label for that state,
// so an absent or partial map behaves exactly like the current hardcoded
// behavior.
func ResolveProgressLabels(configured map[domain.ProgressState]string) map[domain.ProgressState]string {
	resolved := maps.Clone(defaultProgressLabels)
	for state, label := range configured {
		if strings.TrimSpace(label) == "" {
			continue
		}
		resolved[state] = label
	}
	return resolved
}

func (p *StandardPublisher) progressLabel(state domain.ProgressState) string {
	if p != nil && p.progressLabels != nil {
		if label := p.progressLabels[state]; strings.TrimSpace(label) != "" {
			return label
		}
	}
	return defaultProgressLabels[state]
}

// progressMarkdown resolves the label for a state to publishable Slack
// markdown: Slack control sequences are neutralized and the result is bounded
// to domain.ProgressLabelMaxRunes Unicode code points, measured after
// neutralization (which can grow the text). An oversized label is rejected
// rather than truncated so a misconfigured label never silently degrades.
func (p *StandardPublisher) progressMarkdown(state domain.ProgressState) (string, error) {
	label := p.progressLabel(state)
	if label == "" {
		return "", fmt.Errorf("unsupported Slack progress state %q", state)
	}
	markdown := neutralizeUnsafeControls(label)
	if len([]rune(markdown)) > domain.ProgressLabelMaxRunes {
		return "", fmt.Errorf("Slack progress label exceeds %d Unicode code points", domain.ProgressLabelMaxRunes)
	}
	return markdown, nil
}

func slackTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

var _ port.ProgressPublisher = (*StandardPublisher)(nil)
var _ port.SuggestedPromptPublisher = (*StandardPublisher)(nil)
var _ port.OnboardingPublisher = (*StandardPublisher)(nil)
var _ port.IncrementalPublisher = (*StandardPublisher)(nil)

func (*StandardPublisher) ValidateIncrementalText(text string) error {
	_, err := incrementalMarkdown(text)
	return err
}

func (p *StandardPublisher) CreateIncremental(ctx context.Context, target domain.ReplyTarget, operation domain.IncrementalOperation, text string) (port.PublishedResponse, error) {
	markdown, err := incrementalMarkdown(text)
	if err != nil {
		return port.PublishedResponse{}, err
	}
	if err := p.validateIncremental(operation, false); err != nil {
		return port.PublishedResponse{}, err
	}
	callCtx, cancel := slackTimeout(ctx, p.timeout)
	defer cancel()
	timestamp, err := p.client.PostStandard(callCtx, target.ChannelID, target.ThreadTS, markdown, incrementalMetadata(operation))
	if err != nil {
		return port.PublishedResponse{}, fmt.Errorf("create Slack incremental message: %w", err)
	}
	return port.PublishedResponse{LastMessageTS: timestamp}, nil
}

func (p *StandardPublisher) UpdateIncremental(ctx context.Context, operation domain.IncrementalOperation, text string) error {
	markdown, err := incrementalMarkdown(text)
	if err != nil {
		return err
	}
	if err := p.validateIncremental(operation, true); err != nil {
		return err
	}
	callCtx, cancel := slackTimeout(ctx, p.timeout)
	defer cancel()
	if err := p.client.UpdateStandard(callCtx, operation.ChannelID, operation.MessageTS, markdown, incrementalMetadata(operation)); err != nil {
		return fmt.Errorf("update Slack incremental message: %w", err)
	}
	return nil
}

func (p *StandardPublisher) FinalizeIncremental(ctx context.Context, operation domain.IncrementalOperation, text, assistantCorrelationID string) error {
	markdown, err := incrementalMarkdown(text)
	if err != nil {
		return err
	}
	if err := p.validateIncremental(operation, true); err != nil {
		return err
	}
	if assistantCorrelationID == "" {
		return errors.New("assistant correlation ID is required to finalize Slack incremental delivery")
	}
	metadata := slackapi.SlackMetadata{EventType: assistantMetadataEventType, EventPayload: map[string]any{
		"correlation_id": assistantCorrelationID, "render_mode": markdownRenderMode,
		"part_index": 1, "part_count": 1, "content_sha256": contentSHA256(markdown),
	}}
	callCtx, cancel := slackTimeout(ctx, p.timeout)
	defer cancel()
	if err := p.client.UpdateStandard(callCtx, operation.ChannelID, operation.MessageTS, markdown, metadata); err != nil {
		return fmt.Errorf("finalize Slack incremental message: %w", err)
	}
	return nil
}

func (p *StandardPublisher) InterruptIncremental(ctx context.Context, operation domain.IncrementalOperation, text string) error {
	if strings.TrimSpace(text) == "" {
		text = "Interrupted"
	}
	return p.UpdateIncremental(ctx, operation, text)
}

func (p *StandardPublisher) RecoverIncremental(ctx context.Context, operation domain.IncrementalOperation) (port.PublishedResponse, bool, error) {
	if err := p.validateIncremental(operation, false); err != nil {
		return port.PublishedResponse{}, false, err
	}
	callCtx, cancel := slackTimeout(ctx, p.timeout)
	defer cancel()
	messages, hasMore, err := p.client.StandardMessages(callCtx, operation.ChannelID, operation.ThreadTS, progressRecoveryLimit)
	if err != nil {
		return port.PublishedResponse{}, false, fmt.Errorf("recover Slack incremental message: %w", err)
	}
	if hasMore {
		return port.PublishedResponse{}, false, errors.New("recover Slack incremental message: bounded history is incomplete")
	}
	var match string
	for _, message := range messages {
		if message.User != p.botUserID || message.Metadata.EventType != incrementalMetadataEventType {
			continue
		}
		operationID, _ := message.Metadata.EventPayload["operation_id"].(string)
		if operationID != operation.ID {
			continue
		}
		if match != "" {
			return port.PublishedResponse{}, false, errors.New("recover Slack incremental message: duplicate operation metadata")
		}
		match = message.Timestamp
	}
	return port.PublishedResponse{LastMessageTS: match}, match != "", nil
}

func (p *StandardPublisher) validateIncremental(operation domain.IncrementalOperation, requireMessage bool) error {
	if p == nil || p.client == nil || p.botUserID == "" {
		return errors.New("Slack standard publisher is required")
	}
	if operation.ID == "" || operation.ChannelID == "" || operation.ThreadTS == "" || operation.RendererVersion != standardIncrementalRenderer {
		return errors.New("Slack incremental delivery identity is invalid")
	}
	if requireMessage && operation.MessageTS == "" {
		return errors.New("Slack incremental message timestamp is required")
	}
	return nil
}

func incrementalMetadata(operation domain.IncrementalOperation) slackapi.SlackMetadata {
	return slackapi.SlackMetadata{EventType: incrementalMetadataEventType, EventPayload: map[string]any{
		"operation_id": operation.ID, "renderer_version": operation.RendererVersion,
		"sequence": operation.Sequence, "prefix_digest": operation.PrefixDigest,
	}}
}

func incrementalMarkdown(text string) (string, error) {
	markdown := neutralizeUnsafeControls(text)
	if strings.TrimSpace(markdown) == "" {
		return "", errors.New("Slack incremental text is required")
	}
	if len([]rune(markdown)) > SlackMarkdownChunkRunes {
		return "", fmt.Errorf("%w: Slack incremental text exceeds %d Unicode code points", port.ErrIncrementalTextTooLong, SlackMarkdownChunkRunes)
	}
	return markdown, nil
}
