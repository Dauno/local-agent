package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/blockkit"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

const jobAcceptedStatusSentence = "The host accepted the job. It is queued or running."

var _ port.JobAcceptancePublisher = (*ConfirmationPublisher)(nil)

// PublishJobAccepted publishes a new host-owned receipt for a delegated job.
// It never updates the confirmation prompt.
func (p *ConfirmationPublisher) PublishJobAccepted(ctx context.Context, job domain.ExternalAgentJob) error {
	if p == nil || p.client == nil {
		return errors.New("slack posting client is required for job acceptance publishing")
	}
	target, err := domain.ConversationReplyTarget(job.ConversationKey)
	if err != nil {
		return fmt.Errorf("resolve job acceptance target: %w", err)
	}
	fallbackText, blocks, err := compileJobAcceptedMessage(p.engine, job)
	if err != nil {
		if p.renderErr != nil {
			err = p.renderErr
		}
		return fmt.Errorf("render job acceptance template: %w", err)
	}

	callCtx := ctx
	cancel := func() {}
	if p.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, p.timeout)
	}
	defer cancel()

	timestamp, err := p.client.PostBlocks(callCtx, target.ChannelID, fallbackText, blocks, slackapi.SlackMetadata{}, target.ThreadTS)
	if err != nil {
		return fmt.Errorf("publish job acceptance blocks: %w", secure.NewRedactor().Error(err))
	}
	if timestamp == "" {
		return errors.New("slack published job acceptance without a message timestamp")
	}
	return nil
}

func compileJobAcceptedMessage(engine *blockkit.Engine, job domain.ExternalAgentJob) (string, []slackapi.Block, error) {
	if strings.TrimSpace(job.ID) == "" {
		return "", nil, errors.New("job acceptance requires a job ID")
	}
	status := jobStatusLabel(job.Status)
	if status == "unknown" {
		return "", nil, errors.New("job acceptance requires a known job status")
	}
	if job.CreatedAt.IsZero() || job.UpdatedAt.IsZero() {
		return "", nil, errors.New("job acceptance requires created and updated timestamps")
	}

	jobIDLimit := jobAcceptedSubtitleLimit - utf8.RuneCountInString("*Job ID:* `") -
		utf8.RuneCountInString("` · *Status:* `"+status+"`")
	message, err := engine.Message(jobAcceptedView{
		JobID: truncateConfirmationText(job.ID, jobIDLimit), Status: status,
		CreatedAt: job.CreatedAt.UTC(), UpdatedAt: job.UpdatedAt.UTC(),
		StatusSentence: jobAcceptedStatusSentence,
	})
	if err != nil {
		return "", nil, err
	}
	return message.FallbackText, message.Blocks, nil
}

func validateJobAcceptedMessageLimits(fallback string, blocks []slackapi.Block) error {
	if utf8.RuneCountInString(fallback) > maxFallbackText {
		return fmt.Errorf("job acceptance fallback exceeds %d character limit", maxFallbackText)
	}
	if len(blocks) > maxBlocksPerMessage {
		return fmt.Errorf("job acceptance exceeds %d block limit", maxBlocksPerMessage)
	}
	return nil
}
