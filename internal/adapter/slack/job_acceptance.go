package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

const (
	jobAcceptedTemplate       = "job_accepted_message"
	jobAcceptedStatusSentence = "The host accepted the job. It is queued or running."
)

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
	fallbackText, blocks, err := compileJobAcceptedMessage(p.renderer, job)
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

func compileJobAcceptedMessage(renderer *TemplateRenderer, job domain.ExternalAgentJob) (string, []slackapi.Block, error) {
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

	jobID := truncateConfirmationText(neutralizeUnsafeControls(job.ID), maxRendererIDLength)
	createdAt := job.CreatedAt.UTC().Format(time.RFC3339)
	updatedAt := job.UpdatedAt.UTC().Format(time.RFC3339)
	statusSentence := neutralizeUnsafeControls(jobAcceptedStatusSentence)
	fallback := strings.Join([]string{
		"Job accepted / running",
		"Job ID: " + jobID,
		"Status: " + status,
		"Created: " + createdAt,
		"Updated: " + updatedAt,
		statusSentence,
	}, "\n")
	fallback = truncateConfirmationText(neutralizeUnsafeControls(fallback), maxFallbackText)

	compiledFallback, blocks, err := renderer.CompileMessageWithFallback(jobAcceptedTemplate, TemplateContext{Values: map[string]string{
		"job_id":          "*Job ID:*\n`" + escapeSlackMrkdwn(jobID) + "`",
		"status":          "*Status:*\n`" + escapeSlackMrkdwn(status) + "`",
		"created_at":      "*Created:*\n" + escapeSlackMrkdwn(createdAt),
		"updated_at":      "*Updated:*\n" + escapeSlackMrkdwn(updatedAt),
		"status_sentence": statusSentence,
		"fallback_text":   fallback,
	}})
	if err != nil {
		return compiledFallback, blocks, err
	}
	if err := validateJobAcceptedMessageLimits(compiledFallback, blocks); err != nil {
		return "", nil, err
	}
	return compiledFallback, blocks, nil
}

func validateJobAcceptedMessageLimits(fallback string, blocks []slackapi.Block) error {
	if utf8.RuneCountInString(fallback) > maxFallbackText {
		return fmt.Errorf("job acceptance fallback exceeds %d character limit", maxFallbackText)
	}
	if len(blocks) > maxBlocksPerMessage {
		return fmt.Errorf("job acceptance exceeds %d block limit", maxBlocksPerMessage)
	}
	return validateCompiledBlocks(jobAcceptedTemplate, blocks, false)
}
