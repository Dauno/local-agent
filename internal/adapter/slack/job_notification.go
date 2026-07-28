package slack

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const jobNotificationMetadataEventType = "local_agent_external_agent_job"

type JobNotificationPublisher struct {
	publisher *Publisher
	history   *HistoryReader
}

func NewJobNotificationPublisher(publisher *Publisher, history *HistoryReader) *JobNotificationPublisher {
	return &JobNotificationPublisher{publisher: publisher, history: history}
}

func (p *JobNotificationPublisher) Publish(ctx context.Context, notification domain.ExternalAgentJobNotification) (port.PublishedResponse, error) {
	parts, err := validateJobNotification(notification)
	if err != nil {
		return port.PublishedResponse{}, err
	}
	if p == nil || p.publisher == nil || p.publisher.client == nil {
		return port.PublishedResponse{}, errors.New("Slack job notification publisher is required")
	}
	if notification.Target.CorrelationID == "" {
		notification.Target.CorrelationID = fmt.Sprintf("job:%s:%d:%s", notification.JobID, notification.StatusRevision, notification.Kind)
	}
	channel := p.publisher.channelPace(notification.Target.ChannelID)
	channel.mu.Lock()
	defer channel.mu.Unlock()
	result := port.PublishedResponse{}
	for index, part := range parts {
		if err := p.publisher.waitForChannel(ctx, channel); err != nil {
			return result, err
		}
		req := postRequest{
			channelID: notification.Target.ChannelID, threadTS: notification.Target.ThreadTS,
			markdown: part, correlationID: notification.Target.CorrelationID,
			renderMode: notification.RendererVersion, partIndex: index + 1,
			partCount: len(parts), contentSHA256: contentSHA256(part),
			eventType: jobNotificationMetadataEventType,
			extraMetadata: map[string]any{
				"job_id": notification.JobID, "status_revision": notification.StatusRevision,
				"kind": notification.Kind, "renderer_version": notification.RendererVersion,
				"notification_sha256": notification.ContentSHA256, "content_sha256": notification.ContentSHA256,
				"part_sha256": contentSHA256(part),
			},
		}
		ts, err := p.publisher.postWithRetry(ctx, req)
		channel.lastAttempt = p.publisher.now()
		if err != nil {
			return result, err
		}
		result.LastMessageTS = ts
	}
	return result, nil
}

func (p *JobNotificationPublisher) Reconcile(ctx context.Context, notification domain.ExternalAgentJobNotification) (string, bool, error) {
	parts, err := validateJobNotification(notification)
	if err != nil {
		return "", false, err
	}
	if p == nil || p.history == nil || p.history.client == nil {
		return "", false, errors.New("Slack job notification history is required")
	}
	callCtx := ctx
	cancel := func() {}
	if p.history.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, p.history.timeout)
	}
	defer cancel()
	var messages []slackapi.Message
	if notification.Target.ThreadTS != "" {
		messages, err = p.history.client.ConversationReplies(callCtx, notification.Target.ChannelID, notification.Target.ThreadTS, "", 100)
	} else {
		messages, err = p.history.client.ConversationHistory(callCtx, notification.Target.ChannelID, "", 100)
	}
	if err != nil {
		return "", false, err
	}
	matched := make([]slackapi.Message, len(parts))
	seen := make(map[int]bool, len(parts))
	for _, message := range messages {
		if message.Metadata.EventType != jobNotificationMetadataEventType {
			continue
		}
		payload := message.Metadata.EventPayload
		jobID, _ := payload["job_id"].(string)
		kind, _ := payload["kind"].(string)
		renderer, _ := payload["renderer_version"].(string)
		digest, _ := payload["notification_sha256"].(string)
		if digest == "" {
			digest, _ = payload["content_sha256"].(string)
		}
		partDigest, _ := payload["part_sha256"].(string)
		revision, _ := metadataInt(payload["status_revision"])
		index, _ := metadataInt(payload["part_index"])
		count, _ := metadataInt(payload["part_count"])
		if jobID != notification.JobID || kind != notification.Kind || renderer != notification.RendererVersion || digest != notification.ContentSHA256 {
			if jobID == notification.JobID {
				return "", false, errors.New("Slack job notification metadata is inconsistent")
			}
			continue
		}
		if message.User != p.history.botUserID || message.Edited != nil || message.Hidden || len(message.Files) != 0 || revision != notification.StatusRevision || count != len(parts) || index < 1 || index > len(parts) || partDigest != contentSHA256(parts[index-1]) || message.Timestamp == "" || seen[index] {
			return "", false, errors.New("Slack job notification delivery evidence is inconsistent")
		}
		seen[index] = true
		matched[index-1] = message
	}
	if len(seen) != len(parts) {
		if len(seen) > 0 {
			return "", false, errors.New("Slack job notification delivery is incomplete")
		}
		return "", false, nil
	}
	for index := 1; index < len(matched); index++ {
		if !parseSlackTimestamp(matched[index-1].Timestamp).Before(parseSlackTimestamp(matched[index].Timestamp)) {
			return "", false, errors.New("Slack job notification delivery order is inconsistent")
		}
	}
	return matched[len(matched)-1].Timestamp, true, nil
}

func validateJobNotification(notification domain.ExternalAgentJobNotification) ([]string, error) {
	if notification.JobID == "" || notification.StatusRevision < 0 || notification.Kind == "" || notification.RendererVersion != domain.JobNotificationRenderer || notification.Target.ChannelID == "" || notification.CanonicalMarkdown == "" {
		return nil, errors.New("job notification identity is invalid")
	}
	digest := sha256.Sum256([]byte(notification.CanonicalMarkdown))
	if fmt.Sprintf("%x", digest) != notification.ContentSHA256 {
		return nil, errors.New("job notification digest does not match canonical Markdown")
	}
	parts := renderMarkdownV1(notification.CanonicalMarkdown, false)
	if len(parts) == 0 || strings.TrimSpace(strings.Join(parts, "")) == "" {
		return nil, errors.New("job notification Markdown is empty")
	}
	return parts, nil
}

var _ port.JobNotificationPublisher = (*JobNotificationPublisher)(nil)
