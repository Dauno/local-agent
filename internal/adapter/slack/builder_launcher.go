package slack

import (
	"context"
	"fmt"
	"sync"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// builderLauncherPublisher implements port.BuilderLauncherPublisher.
type builderLauncherPublisher struct {
	client    *slackapi.Client
	publisher port.ResponsePublisher
	logger    port.Logger
	delivered sync.Map // map[string]bool for idempotency
}

func NewBuilderLauncherPublisher(client *slackapi.Client, publisher port.ResponsePublisher, logger port.Logger) port.BuilderLauncherPublisher {
	return &builderLauncherPublisher{client: client, publisher: publisher, logger: loggerOrDiscard(logger)}
}

func (p *builderLauncherPublisher) PublishBuilderLauncher(ctx context.Context, req port.BuilderLauncherRequest) error {
	if req.IdempotencyKey != "" {
		if _, loaded := p.delivered.LoadOrStore(req.IdempotencyKey, true); loaded {
			p.logger.Debug("builder launcher already published for this idempotency key")
			return nil
		}
	}

	target, err := domain.ConversationReplyTarget(req.ConversationKey)
	if err != nil {
		return fmt.Errorf("resolve builder launcher target: %w", err)
	}

	blocks := []slackapi.Block{
		slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn", "¿Quieres crear un nuevo agente? Completa el formulario a continuación.", false, false),
			nil, nil,
		),
		slackapi.NewActionBlock("builder_launcher",
			slackapi.NewButtonBlockElement(
				"local_agent.builder.open",
				req.Actor,
				slackapi.NewTextBlockObject("plain_text", "Abrir formulario", false, false),
			).WithStyle(slackapi.StylePrimary),
		),
	}

	_, _, err = p.client.PostMessageContext(ctx, target.ChannelID,
		slackapi.MsgOptionText("Abrir formulario para crear un nuevo agente", false),
		slackapi.MsgOptionBlocks(blocks...),
		slackapi.MsgOptionDisableLinkUnfurl(),
		slackapi.MsgOptionDisableMediaUnfurl(),
		slackapi.MsgOptionTS(target.ThreadTS),
	)
	return err
}

var _ port.BuilderLauncherPublisher = (*builderLauncherPublisher)(nil)
