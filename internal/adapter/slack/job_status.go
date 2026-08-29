package slack

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

const (
	jobStatusUnauthorizedMessage = "No se encontró un trabajo autorizado para esta confirmación."
	jobStatusUnavailableMessage  = "No se pudo consultar el estado del trabajo. Intenta de nuevo."
)

// jobStatusEphemeralClient is the small Slack API surface required by the
// status action. It cannot update the original confirmation message.
type jobStatusEphemeralClient interface {
	PostEphemeral(ctx context.Context, channelID, userID, threadTS, fallbackText string, blocks []slackapi.Block) (string, error)
}

type sdkJobStatusEphemeralClient struct {
	client *slackapi.Client
}

func (c sdkJobStatusEphemeralClient) PostEphemeral(ctx context.Context, channelID, userID, threadTS, fallbackText string, blocks []slackapi.Block) (string, error) {
	options := []slackapi.MsgOption{
		slackapi.MsgOptionText(fallbackText, false),
		slackapi.MsgOptionBlocks(blocks...),
	}
	if threadTS != "" {
		options = append(options, slackapi.MsgOptionTS(threadTS))
	}
	return c.client.PostEphemeralContext(ctx, channelID, userID, options...)
}

// JobStatusHandler handles the v2 confirmation status action. It performs all
// authorization checks before it reads or renders any job status.
type JobStatusHandler struct {
	client        jobStatusEphemeralClient
	confirmations port.ConfirmationDeliveryStore
	jobs          port.ExternalAgentJobWrapperReader
	timeout       time.Duration
}

// NewJobStatusHandler creates the Slack handler for authorized job status
// responses.
func NewJobStatusHandler(
	client *slackapi.Client,
	timeout time.Duration,
	confirmations port.ConfirmationDeliveryStore,
	jobs port.ExternalAgentJobWrapperReader,
) *JobStatusHandler {
	var ephemeral jobStatusEphemeralClient
	if client != nil {
		ephemeral = sdkJobStatusEphemeralClient{client: client}
	}
	return newJobStatusHandler(ephemeral, timeout, confirmations, jobs)
}

func newJobStatusHandler(
	client jobStatusEphemeralClient,
	timeout time.Duration,
	confirmations port.ConfirmationDeliveryStore,
	jobs port.ExternalAgentJobWrapperReader,
) *JobStatusHandler {
	return &JobStatusHandler{client: client, confirmations: confirmations, jobs: jobs, timeout: timeout}
}

// Handle validates the Slack callback, confirmation identity, and durable job
// binding before it sends one ephemeral response to the acting user.
func (h *JobStatusHandler) Handle(ctx context.Context, callback slackapi.InteractionCallback) error {
	if h == nil {
		return errors.New("job status handler is not configured")
	}
	action, ok := normalizeJobStatusAction(&callback)
	if !ok {
		return ErrMalformedInteractive
	}
	if h.confirmations == nil || h.jobs == nil {
		return h.publishError(ctx, action, jobStatusUnavailableMessage)
	}

	delivery, err := h.confirmations.GetByWrapperCallID(ctx, action.WrapperCallID)
	if err != nil || !statusConfirmationMatches(delivery, action) {
		return h.publishError(ctx, action, jobStatusUnauthorizedMessage)
	}
	job, err := h.jobs.StatusByWrapperCallID(ctx, action.WrapperCallID, action.Actor, action.ConversationKey)
	if err != nil || !statusJobMatches(job, action) {
		return h.publishError(ctx, action, jobStatusUnauthorizedMessage)
	}

	fallbackText, blocks, err := compileJobStatusResponse(*job)
	if err != nil {
		return h.publishError(ctx, action, jobStatusUnavailableMessage)
	}
	return h.publish(ctx, action, fallbackText, blocks)
}

func statusConfirmationMatches(delivery *port.ConfirmationDelivery, action domain.ConfirmationInteractiveAction) bool {
	if delivery == nil || delivery.RendererMode != confirmationRenderModeV2 ||
		delivery.WrapperCallID != action.WrapperCallID || delivery.Actor != action.Actor ||
		delivery.TeamID != action.TeamID || delivery.ChannelID != action.ChannelID ||
		delivery.ThreadTS != action.ThreadTS || delivery.ConversationKey != action.ConversationKey ||
		delivery.SlackMessageTS != action.MessageTS || !domain.ValidKnowledgeConversationKey(delivery.ConversationKey) {
		return false
	}
	if action.RendererMode == "" && action.CorrelationID == "" && action.ContentSHA256 == "" {
		return true
	}
	return action.RendererMode == delivery.RendererMode && action.CorrelationID == delivery.CorrelationID &&
		action.ContentSHA256 == confirmationContentDigest(*delivery)
}

func statusJobMatches(job *domain.ExternalAgentJob, action domain.ConfirmationInteractiveAction) bool {
	return job != nil && job.ID != "" && job.WrapperCallID == action.WrapperCallID &&
		job.Actor == action.Actor && job.TeamID == action.TeamID &&
		job.ConversationKey == action.ConversationKey
}

func (h *JobStatusHandler) publishError(ctx context.Context, action domain.ConfirmationInteractiveAction, message string) error {
	fallbackText, blocks, err := compileJobStatusErrorResponse(message)
	if err != nil {
		return err
	}
	return h.publish(ctx, action, fallbackText, blocks)
}

func (h *JobStatusHandler) publish(ctx context.Context, action domain.ConfirmationInteractiveAction, fallbackText string, blocks []slackapi.Block) error {
	if h.client == nil {
		return errors.New("job status ephemeral client is not configured")
	}
	if utf8.RuneCountInString(fallbackText) > maxFallbackText || len(blocks) > maxBlocksPerMessage {
		return errors.New("job status response exceeds Slack limits")
	}
	callCtx, cancel := slackTimeout(ctx, h.timeout)
	defer cancel()
	if _, err := h.client.PostEphemeral(callCtx, action.ChannelID, action.Actor, action.ThreadTS, fallbackText, blocks); err != nil {
		return fmt.Errorf("publish ephemeral job status: %w", secure.NewRedactor().Error(err))
	}
	return nil
}

func compileJobStatusErrorResponse(message string) (string, []slackapi.Block, error) {
	engine, err := newJobEngine()
	if err != nil {
		return "", nil, err
	}
	safe := truncateConfirmationText(message, maxContextText)
	view, err := engine.Message(jobStatusErrorView{Message: safe})
	if err != nil {
		return "", nil, err
	}
	return view.FallbackText, view.Blocks, nil
}

func compileJobStatusResponse(job domain.ExternalAgentJob) (string, []slackapi.Block, error) {
	engine, err := newJobEngine()
	if err != nil {
		return "", nil, err
	}
	state := jobStatusLabel(job.Status)
	view, err := engine.Message(jobStatusView{
		JobID: truncateConfirmationText(job.ID, 255), Status: state,
		CreatedAt: formatJobStatusTime(job.CreatedAt), UpdatedAt: formatJobStatusTime(job.UpdatedAt),
		HostStatus: jobHostStatusText(job.Status),
	})
	if err != nil {
		return "", nil, err
	}
	return view.FallbackText, view.Blocks, nil
}

func formatJobStatusTime(value time.Time) string {
	if value.IsZero() {
		return "no disponible"
	}
	return value.UTC().Format(time.RFC3339)
}

func jobStatusLabel(status domain.ExternalAgentJobStatus) string {
	switch status {
	case domain.JobQueued, domain.JobRunning, domain.JobCancelRequested,
		domain.JobInterruptedSafe, domain.JobCompletionUnknown, domain.JobReconciling,
		domain.JobCompleted, domain.JobFailed, domain.JobCancelled, domain.JobAbandoned:
		return string(status)
	default:
		return "unknown"
	}
}

func jobHostStatusText(status domain.ExternalAgentJobStatus) string {
	switch status {
	case domain.JobQueued:
		return "The host accepted the job. It is waiting to run."
	case domain.JobRunning:
		return "The host is running the job."
	case domain.JobCancelRequested:
		return "The host requested cancellation."
	case domain.JobInterruptedSafe:
		return "The host stopped the job in a safe state."
	case domain.JobCompletionUnknown:
		return "The host cannot confirm the final external state. Do not retry automatically."
	case domain.JobReconciling:
		return "The host is reconciling the external state."
	case domain.JobCompleted:
		return "The host recorded that the job completed."
	case domain.JobFailed:
		return "The host recorded that the job failed."
	case domain.JobCancelled:
		return "The host recorded that the job was cancelled."
	case domain.JobAbandoned:
		return "The host recorded that the job was abandoned."
	default:
		return "The host recorded an unknown job state."
	}
}
