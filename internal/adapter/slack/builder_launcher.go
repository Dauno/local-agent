package slack

import (
	"context"
	"errors"
	"fmt"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// builderLauncherPublisher implements port.BuilderLauncherPublisher.
type builderLauncherPublisher struct {
	client         blockPostClient
	recoveryClient standardMessageClient
	store          port.BuilderLauncherDeliveryStore
	botUserID      string
	publisher      port.ResponsePublisher
	logger         port.Logger
	renderer       *TemplateRenderer
	renderErr      error
}

func newBuilderLauncherPublisher(client blockPostClient, publisher port.ResponsePublisher, logger port.Logger) *builderLauncherPublisher {
	return newBuilderLauncherPublisherWithDependencies(client, nil, nil, "", publisher, logger)
}

func NewBuilderLauncherPublisherWithStore(client *slackapi.Client, publisher port.ResponsePublisher, logger port.Logger, store port.BuilderLauncherDeliveryStore, botUserID string) port.BuilderLauncherPublisher {
	var poster blockPostClient
	var recovery standardMessageClient
	if client != nil {
		poster = sdkBlockPostClient{client: client}
		recovery = sdkStandardMessageClient{client: client}
	}
	return newBuilderLauncherPublisherWithDependencies(poster, recovery, store, botUserID, publisher, logger)
}

func newBuilderLauncherPublisherWithDependencies(client blockPostClient, recovery standardMessageClient, store port.BuilderLauncherDeliveryStore, botUserID string, publisher port.ResponsePublisher, logger port.Logger) *builderLauncherPublisher {
	renderer, renderErr := NewEmbeddedTemplateRenderer()
	return &builderLauncherPublisher{
		client: client, recoveryClient: recovery, store: store, botUserID: botUserID,
		publisher: publisher, logger: loggerOrDiscard(logger), renderer: renderer, renderErr: renderErr,
	}
}

func (p *builderLauncherPublisher) PublishBuilderLauncher(ctx context.Context, req port.BuilderLauncherRequest) error {
	if !domain.PlausibleUserID(req.Actor) {
		return fmt.Errorf("builder actor is invalid")
	}
	metadata, err := encodeBuilderInteractionContext(req.Actor, req.ConversationKey)
	if err != nil {
		return fmt.Errorf("encode builder launcher context: %w", err)
	}
	target, err := domain.ConversationReplyTarget(req.ConversationKey)
	if err != nil {
		return fmt.Errorf("resolve builder launcher target: %w", err)
	}
	if p == nil || p.client == nil {
		return fmt.Errorf("Slack client is required")
	}
	var claim port.BuilderLauncherDeliveryClaim
	if p.store != nil {
		if req.IdempotencyKey == "" {
			return errors.New("builder launcher idempotency key is required")
		}
		claim, state, claimErr := p.store.ClaimBuilderLauncher(ctx, req.IdempotencyKey, req.ConversationKey, time.Now().UTC())
		if claimErr != nil {
			return claimErr
		}
		switch state {
		case port.BuilderLauncherAlreadyPublished, port.BuilderLauncherInFlight:
			return nil
		case port.BuilderLauncherClaimed:
		default:
			return errors.New("builder launcher claim state is unsupported")
		}
		if p.recoveryClient == nil {
			return errors.New("builder launcher recovery client is required")
		}
		recovered, found, recoverErr := p.recover(ctx, target, req.IdempotencyKey)
		if recoverErr != nil {
			return recoverErr
		}
		if found {
			return p.store.MarkBuilderLauncherPublished(ctx, claim, recovered.LastMessageTS, time.Now().UTC())
		}
	}
	fallbackText, blocks, err := compileOnboardingMessage(p.renderer, metadata, nil)
	if err != nil {
		if p.renderErr != nil {
			err = p.renderErr
		}
		return fmt.Errorf("render onboarding template: %w", err)
	}
	messageMetadata := slackapi.SlackMetadata{}
	if p.store != nil {
		messageMetadata = builderLauncherMetadata(req.IdempotencyKey)
	}
	timestamp, err := p.client.PostBlocks(ctx, target.ChannelID, fallbackText, blocks, messageMetadata, target.ThreadTS)
	if err != nil {
		return err
	}
	if timestamp == "" {
		return errors.New("Slack published builder launcher without a message timestamp")
	}
	if p.store != nil {
		if err := p.store.MarkBuilderLauncherPublished(ctx, claim, timestamp, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

const builderLauncherMetadataEventType = "local_agent_builder_launcher"

func builderLauncherMetadata(deliveryID string) slackapi.SlackMetadata {
	return slackapi.SlackMetadata{EventType: builderLauncherMetadataEventType, EventPayload: map[string]any{"delivery_id": deliveryID}}
}

func (p *builderLauncherPublisher) recover(ctx context.Context, target domain.ReplyTarget, deliveryID string) (port.PublishedResponse, bool, error) {
	messages, hasMore, err := p.recoveryClient.StandardMessages(ctx, target.ChannelID, target.ThreadTS, progressRecoveryLimit)
	if err != nil {
		return port.PublishedResponse{}, false, fmt.Errorf("recover Slack builder launcher: %w", err)
	}
	if hasMore {
		return port.PublishedResponse{}, false, errors.New("recover Slack builder launcher: bounded history is incomplete")
	}
	var match string
	for _, message := range messages {
		if p.botUserID != "" && message.User != p.botUserID {
			continue
		}
		if message.Metadata.EventType != builderLauncherMetadataEventType {
			continue
		}
		candidate, _ := message.Metadata.EventPayload["delivery_id"].(string)
		if candidate != deliveryID {
			continue
		}
		if match != "" {
			return port.PublishedResponse{}, false, errors.New("recover Slack builder launcher: duplicate delivery metadata")
		}
		match = message.Timestamp
	}
	return port.PublishedResponse{LastMessageTS: match}, match != "", nil
}

const (
	onboardingIntroText      = "Puedo analizar proyectos, revisar errores, resumir contexto y ayudarte a crear agentes."
	onboardingDescribePrompt = "Describe lo que necesitas en un mensaje y trabajamos sobre ello."
)

func compileOnboardingMessage(renderer *TemplateRenderer, builderContext string, prompts []string) (string, []slackapi.Block, error) {
	return renderer.CompileMessageWithFallback("onboarding_message", TemplateContext{
		Values: map[string]string{
			"builder_context": builderContext,
			"intro":           onboardingIntroText,
			"describe_prompt": onboardingDescribePrompt,
		},
		SuggestedPrompts: prompts,
	})
}

var _ port.BuilderLauncherPublisher = (*builderLauncherPublisher)(nil)
