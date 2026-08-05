package slack

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const jobNotificationMetadataEventType = "local_agent_external_agent_job"

type JobNotificationPublisher struct {
	publisher     *Publisher
	history       *HistoryReader
	uploader      port.GeneratedFileUploader
	artifacts     port.VerifiedResultArtifactStore
	deliveryStore port.ExternalAgentJobDeliveryStore
	fileClient    *slackapi.Client
	partLabels    bool
}

func NewJobNotificationPublisher(publisher *Publisher, history *HistoryReader) *JobNotificationPublisher {
	return &JobNotificationPublisher{publisher: publisher, history: history}
}

func NewDurableJobNotificationPublisher(publisher *Publisher, history *HistoryReader, uploader port.GeneratedFileUploader, artifacts port.VerifiedResultArtifactStore, deliveryStore port.ExternalAgentJobDeliveryStore, fileClient *slackapi.Client, partLabels ...bool) *JobNotificationPublisher {
	labels := false
	if len(partLabels) > 0 {
		labels = partLabels[0]
	}
	return &JobNotificationPublisher{publisher: publisher, history: history, uploader: uploader, artifacts: artifacts, deliveryStore: deliveryStore, fileClient: fileClient, partLabels: labels}
}

func (p *JobNotificationPublisher) Publish(ctx context.Context, notification domain.ExternalAgentJobNotification) (port.PublishedResponse, error) {
	parts, err := validateJobNotification(notification)
	if err != nil {
		return port.PublishedResponse{}, invalidNotificationError(err)
	}
	if p == nil || p.publisher == nil || p.publisher.client == nil {
		return port.PublishedResponse{}, invalidNotificationError(errors.New("Slack job notification publisher is required"))
	}
	if notification.DeliveryMode == domain.JobResultDeliveryFile {
		return p.publishFile(ctx, notification)
	}
	parts = renderMarkdownV1(notification.CanonicalMarkdown, p.partLabels)
	if notification.MaxMarkdownParts > 0 && len(parts) > notification.MaxMarkdownParts {
		return port.PublishedResponse{}, invalidNotificationError(errors.New("job notification exceeds its persisted Markdown part policy"))
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
				"notification_sha256": notification.NotificationSHA256, "content_sha256": notification.ContentSHA256,
				"notification_bytes": notification.NotificationBytes,
				"delivery_mode":      string(notification.DeliveryMode),
				"policy_version":     notification.PolicyVersion,
				"result_sha256":      notification.ResultSHA256,
				"result_bytes":       notification.ResultBytes,
				"max_markdown_parts": notification.MaxMarkdownParts,
				"upload_state":       string(notification.UploadState),
				"part_sha256":        contentSHA256(part),
			},
		}
		ts, err := p.publisher.postWithRetry(ctx, req)
		channel.lastAttempt = p.publisher.now()
		if err != nil {
			return result, classifyMarkdownPostError(err)
		}
		result.LastMessageTS = ts
	}
	return result, nil
}

func (p *JobNotificationPublisher) Reconcile(ctx context.Context, notification domain.ExternalAgentJobNotification) (string, bool, error) {
	parts, err := validateJobNotification(notification)
	if err != nil {
		return "", false, invalidNotificationError(err)
	}
	if p == nil || p.history == nil || p.history.client == nil {
		return "", false, invalidNotificationError(errors.New("Slack job notification history is required"))
	}
	if notification.DeliveryMode == domain.JobResultDeliveryFile && notification.SlackFileID == "" {
		// No durable Slack identity exists yet, so there is nothing to
		// reconcile. Let the worker retry the upload URL request.
		return "", false, nil
	}
	if notification.DeliveryMode == domain.JobResultDeliveryMarkdown {
		parts = renderMarkdownV1(notification.CanonicalMarkdown, p.partLabels)
		if notification.MaxMarkdownParts > 0 && len(parts) > notification.MaxMarkdownParts {
			return "", false, invalidNotificationError(errors.New("job notification exceeds its persisted Markdown part policy"))
		}
	}
	callCtx := ctx
	cancel := func() {}
	if p.history.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, p.history.timeout)
	}
	defer cancel()
	if notification.DeliveryMode == domain.JobResultDeliveryFile {
		return p.reconcileFile(ctx, notification, callCtx)
	}
	var messages []slackapi.Message
	if notification.Target.ThreadTS != "" {
		messages, err = p.history.client.ConversationReplies(callCtx, notification.Target.ChannelID, notification.Target.ThreadTS, "", 100)
	} else {
		messages, err = p.history.client.ConversationHistory(callCtx, notification.Target.ChannelID, "", 100)
	}
	if err != nil {
		return "", false, classifyHistoryError(err)
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
		mode, _ := payload["delivery_mode"].(string)
		policy, _ := payload["policy_version"].(string)
		partDigest, _ := payload["part_sha256"].(string)
		revision, _ := metadataInt(payload["status_revision"])
		index, _ := metadataInt(payload["part_index"])
		count, _ := metadataInt(payload["part_count"])
		if jobID != notification.JobID || kind != notification.Kind || renderer != notification.RendererVersion || !notificationEvidenceDigestMatch(payload, notification) || (mode != "" && mode != string(notification.DeliveryMode)) || (policy != "" && policy != notification.PolicyVersion) {
			if jobID == notification.JobID {
				return "", false, invalidNotificationError(errors.New("Slack job notification metadata is inconsistent"))
			}
			continue
		}
		if message.User != p.history.botUserID || message.Edited != nil || message.Hidden || len(message.Files) != 0 || revision != notification.StatusRevision || count != len(parts) || index < 1 || index > len(parts) || partDigest != contentSHA256(parts[index-1]) || message.Timestamp == "" || seen[index] {
			return "", false, invalidNotificationError(errors.New("Slack job notification delivery evidence is inconsistent"))
		}
		seen[index] = true
		matched[index-1] = message
	}
	if len(seen) != len(parts) {
		if len(seen) > 0 {
			return "", false, invalidNotificationError(errors.New("Slack job notification delivery is incomplete"))
		}
		return "", false, nil
	}
	for index := 1; index < len(matched); index++ {
		if !parseSlackTimestamp(matched[index-1].Timestamp).Before(parseSlackTimestamp(matched[index].Timestamp)) {
			return "", false, invalidNotificationError(errors.New("Slack job notification delivery order is inconsistent"))
		}
	}
	return matched[len(matched)-1].Timestamp, true, nil
}

func validateJobNotification(notification domain.ExternalAgentJobNotification) ([]string, error) {
	if notification.JobID == "" || notification.StatusRevision < 0 || notification.Kind == "" || notification.RendererVersion != domain.JobNotificationRenderer || notification.Target.ChannelID == "" || notification.CanonicalMarkdown == "" {
		return nil, errors.New("job notification identity is invalid")
	}
	if notification.PolicyVersion == domain.JobDeliveryPolicyV1 {
		expected, err := domain.ConversationReplyTarget(notification.ConversationKey)
		if err != nil || notification.Target.ChannelID != expected.ChannelID || notification.Target.ThreadTS != expected.ThreadTS {
			return nil, errors.New("result_destination_mismatch")
		}
	}
	notificationDigest, notificationBytes := notificationIdentity(notification)
	if notification.DeliveryMode == domain.JobResultDeliveryFile {
		resultDigest, resultBytes := resultIdentity(notification)
		if notification.ArtifactRef == "" || resultBytes <= 0 || resultDigest == "" {
			return nil, errors.New("file job notification delivery identity is invalid")
		}
		parts := renderMarkdownV1(notification.CanonicalMarkdown, false)
		if len(parts) != 1 {
			return nil, errors.New("file job notification status exceeds one Markdown part")
		}
		return parts, nil
	}
	if notification.PolicyVersion == "legacy_v1" {
		digest := sha256.Sum256([]byte(notification.CanonicalMarkdown))
		if fmt.Sprintf("%x", digest) != notificationDigest {
			return nil, errors.New("job notification digest does not match canonical Markdown")
		}
	} else {
		if len(notificationDigest) != sha256.Size*2 || notificationBytes <= 0 {
			return nil, errors.New("job notification identity is invalid")
		}
		resultDigest, resultBytes := resultIdentity(notification)
		if len(resultDigest) != sha256.Size*2 || resultBytes <= 0 {
			return nil, errors.New("job notification result identity is invalid")
		}
	}
	parts := renderMarkdownV1(notification.CanonicalMarkdown, false)
	if len(parts) == 0 || strings.TrimSpace(strings.Join(parts, "")) == "" {
		return nil, errors.New("job notification Markdown is empty")
	}
	if notification.MaxMarkdownParts > 0 && len(parts) > notification.MaxMarkdownParts {
		return nil, errors.New("job notification exceeds its persisted Markdown part policy")
	}
	return parts, nil
}

// notificationIdentity returns the notification identity over the canonical
// Markdown, falling back to the legacy content storage columns for rows that
// predate the v32 identity split. It never mixes the two identities.
func notificationIdentity(notification domain.ExternalAgentJobNotification) (string, int64) {
	digest := notification.NotificationSHA256
	bytes := notification.NotificationBytes
	if digest == "" || bytes <= 0 {
		digest = notification.ContentSHA256
		bytes = int64(len([]byte(notification.CanonicalMarkdown)))
	}
	return digest, bytes
}

// notificationEvidenceDigestMatch decides whether the Slack payload digest
// matches the durable notification identity. v32 rows are verified against the
// canonical-Markdown identity (notification_sha256) and fail closed on any
// mismatch. Deliveries published before v32 wrote the result digest into both
// notification_sha256 and content_sha256 and never carried the v32 metadata
// fields (notification_bytes, result_sha256); such legacy evidence is accepted
// only when those fields are absent and the digest equals the legacy content
// identity. Rows that never received a v32 identity keep reconciling against
// their legacy content identity alone.
func notificationEvidenceDigestMatch(payload map[string]any, notification domain.ExternalAgentJobNotification) bool {
	digest, _ := payload["notification_sha256"].(string)
	if digest == "" {
		digest, _ = payload["content_sha256"].(string)
	}
	expected := notification.NotificationSHA256
	if expected == "" {
		return digest != "" && notification.ContentSHA256 != "" && digest == notification.ContentSHA256
	}
	if digest != "" && digest == expected {
		return true
	}
	_, hasNotificationBytes := payload["notification_bytes"]
	_, hasResultSHA256 := payload["result_sha256"]
	if hasNotificationBytes || hasResultSHA256 || digest == "" || notification.ContentSHA256 == "" {
		return false
	}
	return digest == notification.ContentSHA256
}

// resultIdentity returns the complete sanitized result identity, falling back
// to the legacy content storage columns for rows written before v32.
func resultIdentity(notification domain.ExternalAgentJobNotification) (string, int64) {
	digest := notification.ResultSHA256
	bytes := notification.ResultBytes
	if digest == "" || bytes <= 0 {
		digest = notification.ContentSHA256
		bytes = notification.ContentBytes
		if bytes <= 0 {
			bytes = notification.ResultBytes
		}
	}
	return digest, bytes
}

func (p *JobNotificationPublisher) publishFile(ctx context.Context, notification domain.ExternalAgentJobNotification) (port.PublishedResponse, error) {
	if p.uploader == nil || p.artifacts == nil {
		return port.PublishedResponse{}, invalidNotificationError(errors.New("durable file delivery dependencies are required"))
	}
	resultDigest, resultBytes := resultIdentity(notification)
	var content []byte
	var err error
	if notification.HostResultText != "" {
		content = []byte(notification.HostResultText)
	} else {
		content, err = p.artifacts.Get(ctx, notification.JobID+"-delivery", notification.ArtifactRef, resultDigest, resultBytes)
		if err != nil {
			return port.PublishedResponse{}, permanentUploadError(string(domain.ResultErrorCodeOf(err)))
		}
	}
	if int64(len(content)) != resultBytes {
		return port.PublishedResponse{}, permanentUploadError(string(domain.ResultErrorArtifactBytesMismatch))
	}
	if notification.HostResultText != "" {
		digest := sha256.Sum256(content)
		if !strings.EqualFold(resultDigest, fmt.Sprintf("%x", digest)) {
			return port.PublishedResponse{}, permanentUploadError("result_delivery_failed")
		}
	}
	if notification.SlackFileID == "" {
		if notification.UploadState != domain.JobResultUploadPending {
			return port.PublishedResponse{}, permanentUploadError("result_file_upload_unknown")
		}
		filename := "opencode-" + notification.JobID + ".md"
		var target port.GeneratedFileUploadTarget
		var requestErr error
		if markdownUploader, ok := p.uploader.(port.MarkdownResultUploader); ok {
			target, requestErr = markdownUploader.RequestMarkdownUploadURL(ctx, filename, len(content))
		} else {
			target, requestErr = p.uploader.RequestUploadURL(ctx, filename, len(content))
		}
		if requestErr != nil || target.FileID == "" || target.UploadURL == "" {
			if requestErr != nil {
				var uploadErr *port.GeneratedFileUploadError
				if errors.As(requestErr, &uploadErr) && uploadErr.Ambiguous {
					return port.PublishedResponse{}, permanentUploadError("result_file_upload_unknown")
				}
				return port.PublishedResponse{}, classifyUploadError("result_file_upload_failed", requestErr, true)
			}
			return port.PublishedResponse{}, permanentUploadError("result_file_upload_unknown")
		}
		if p.deliveryStore != nil {
			if err := p.deliveryStore.MarkNotificationFileID(ctx, &notification, target.FileID, time.Now().UTC()); err != nil {
				return port.PublishedResponse{}, permanentUploadError("result_file_upload_unknown")
			}
		}
		notification.SlackFileID = target.FileID
		if err := p.uploader.UploadBytes(ctx, target, content); err != nil {
			return port.PublishedResponse{}, classifyUploadError("result_file_upload_unknown", err, true)
		}
		if p.deliveryStore != nil {
			if err := p.deliveryStore.MarkNotificationUploadState(ctx, &notification, domain.JobResultUploadBytesUploaded, time.Now().UTC()); err != nil {
				return port.PublishedResponse{}, port.NewNotificationPublishError("result_file_upload_unknown", true, true, errors.New("result_file_upload_unknown"))
			}
		}
		notification.UploadState = domain.JobResultUploadBytesUploaded
	} else if notification.UploadState == domain.JobResultUploadUnknown {
		// An ambiguous upload is recoverable only when Slack already exposes the
		// exact file in the target. Never create a second upload or complete an
		// unproven file.
		if _, inspectErr := p.inspectFile(ctx, notification, true); inspectErr != nil {
			return port.PublishedResponse{}, classifyFileEvidenceError(inspectErr, "result_file_upload_unknown")
		}
		if notification.Target.ThreadTS != "" {
			shared, inspectErr := p.fileSharedInThread(ctx, notification)
			if inspectErr != nil {
				return port.PublishedResponse{}, classifyHistoryError(inspectErr)
			}
			if !shared {
				return port.PublishedResponse{}, permanentUploadError("result_file_upload_unknown")
			}
		}
		notification.UploadState = domain.JobResultUploadCompleted
		if p.deliveryStore != nil {
			if err := p.deliveryStore.MarkNotificationUploadState(ctx, &notification, domain.JobResultUploadCompleted, time.Now().UTC()); err != nil {
				return port.PublishedResponse{}, port.NewNotificationPublishError("result_file_upload_unknown", true, true, errors.New("result_file_upload_unknown"))
			}
		}
	} else if notification.UploadState == domain.JobResultUploadURLRequested || notification.UploadState == domain.JobResultUploadBytesUploaded {
		// Upload URLs are intentionally not persisted. After restart, exact file
		// evidence is the only safe way to continue to CompleteUpload.
		file, inspectErr := p.inspectFile(ctx, notification, false)
		if inspectErr != nil {
			return port.PublishedResponse{}, classifyFileEvidenceError(inspectErr, "result_file_upload_unknown")
		}
		alreadyShared := fileVisibleInChannel(file, notification.Target.ChannelID)
		if notification.Target.ThreadTS != "" {
			shared, shareErr := p.fileSharedInThread(ctx, notification)
			if shareErr != nil {
				return port.PublishedResponse{}, classifyHistoryError(shareErr)
			}
			alreadyShared = shared
		}
		if alreadyShared {
			notification.UploadState = domain.JobResultUploadCompleted
			if p.deliveryStore != nil {
				if err := p.deliveryStore.MarkNotificationUploadState(ctx, &notification, domain.JobResultUploadCompleted, time.Now().UTC()); err != nil {
					return port.PublishedResponse{}, port.NewNotificationPublishError("result_file_upload_unknown", true, true, errors.New("result_file_upload_unknown"))
				}
			}
		}
	}
	if notification.UploadState != domain.JobResultUploadCompleted {
		if err := p.uploader.CompleteUpload(ctx, notification.SlackFileID, notification.Target.ChannelID, notification.Target.ThreadTS, "OpenCode result "+notification.JobID); err != nil {
			return port.PublishedResponse{}, classifyUploadError("result_file_completion_failed", err, true)
		}
		if p.deliveryStore != nil {
			if err := p.deliveryStore.MarkNotificationUploadState(ctx, &notification, domain.JobResultUploadCompleted, time.Now().UTC()); err != nil {
				return port.PublishedResponse{}, port.NewNotificationPublishError("result_file_upload_unknown", true, true, errors.New("result_file_upload_unknown"))
			}
		}
		notification.UploadState = domain.JobResultUploadCompleted
	}
	return p.publishParts(ctx, notification, renderMarkdownV1(notification.CanonicalMarkdown, false), notification.SlackFileID)
}

func (p *JobNotificationPublisher) publishParts(ctx context.Context, notification domain.ExternalAgentJobNotification, parts []string, fileID string) (port.PublishedResponse, error) {
	channel := p.publisher.channelPace(notification.Target.ChannelID)
	channel.mu.Lock()
	defer channel.mu.Unlock()
	result := port.PublishedResponse{}
	for index, part := range parts {
		if err := p.publisher.waitForChannel(ctx, channel); err != nil {
			return result, classifyMarkdownPostError(err)
		}
		extra := map[string]any{
			"job_id": notification.JobID, "status_revision": notification.StatusRevision,
			"kind": notification.Kind, "renderer_version": notification.RendererVersion,
			"delivery_mode": string(notification.DeliveryMode), "policy_version": notification.PolicyVersion,
			"notification_sha256": notification.NotificationSHA256, "content_sha256": notification.ContentSHA256,
			"notification_bytes": notification.NotificationBytes,
			"result_sha256":      notification.ResultSHA256,
			"result_bytes":       notification.ResultBytes,
			"max_markdown_parts": notification.MaxMarkdownParts,
			"upload_state":       string(notification.UploadState),
			"part_sha256":        contentSHA256(part),
		}
		if fileID != "" {
			extra["file_id"] = fileID
		}
		ts, err := p.publisher.postWithRetry(ctx, postRequest{channelID: notification.Target.ChannelID, threadTS: notification.Target.ThreadTS,
			markdown: part, correlationID: notification.Target.CorrelationID, renderMode: notification.RendererVersion,
			partIndex: index + 1, partCount: len(parts), contentSHA256: contentSHA256(part), eventType: jobNotificationMetadataEventType, extraMetadata: extra})
		channel.lastAttempt = p.publisher.now()
		if err != nil {
			return result, classifyMarkdownPostError(err)
		}
		result.LastMessageTS = ts
	}
	return result, nil
}

func classifyUploadError(code string, err error, retryDefinitive bool) error {
	var uploadErr *port.GeneratedFileUploadError
	if errors.As(err, &uploadErr) {
		if uploadErr.Ambiguous {
			return port.NewNotificationPublishError(code, true, retryDefinitive, errors.New(code))
		}
		return port.NewNotificationPublishError(code, false, retryDefinitive, errors.New(code))
	}
	return port.NewNotificationPublishError(code, true, retryDefinitive, errors.New(code))
}

func invalidNotificationError(err error) error {
	if err == nil {
		err = errors.New("invalid durable notification")
	}
	code := "notification_delivery_invalid"
	if strings.Contains(err.Error(), "result_destination_mismatch") {
		code = "result_destination_mismatch"
	}
	return port.NewNotificationPublishError(code, false, false, errors.New(code))
}

func permanentUploadError(code string) error {
	return port.NewNotificationPublishError(code, false, false, errors.New(code))
}

func classifyFileEvidenceError(err error, code string) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "ambiguous") || strings.Contains(err.Error(), "unavailable") {
		return port.NewNotificationPublishError(code, true, true, errors.New(code))
	}
	return permanentUploadError(code)
}

func classifyHistoryError(err error) error {
	if err == nil {
		return nil
	}
	if slackPermanentError(err) {
		return invalidNotificationError(err)
	}
	return port.NewNotificationPublishError("notification_publish_ambiguous", true, true, errors.New("Slack notification history lookup is ambiguous"))
}

func classifyMarkdownPostError(err error) error {
	if err == nil {
		return nil
	}
	if slackPermanentError(err) {
		return invalidNotificationError(err)
	}
	return port.NewNotificationPublishError("notification_publish_ambiguous", true, true, errors.New("Slack job notification publication outcome is ambiguous"))
}

func slackPermanentError(err error) bool {
	var slackErr slackapi.SlackErrorResponse
	if errors.As(err, &slackErr) {
		switch strings.ToLower(slackErr.Err) {
		case "channel_not_found", "conversation_not_found", "not_in_channel", "is_archived", "invalid_auth", "account_inactive", "token_revoked", "missing_scope", "restricted_action", "not_allowed_token_type", "msg_too_long", "invalid_arguments", "thread_not_found", "metadata_too_long":
			return true
		}
	}
	var statusErr slackapi.StatusCodeError
	if errors.As(err, &statusErr) {
		return statusErr.Code >= 400 && statusErr.Code < 500 && statusErr.Code != 429
	}
	return false
}

func (p *JobNotificationPublisher) reconcileFile(ctx context.Context, notification domain.ExternalAgentJobNotification, callCtx context.Context) (string, bool, error) {
	if p.fileClient == nil {
		return "", false, invalidNotificationError(errors.New("file delivery evidence is unavailable"))
	}
	switch notification.UploadState {
	case domain.JobResultUploadURLRequested, domain.JobResultUploadBytesUploaded:
		// URL credentials are not persisted. An exact file that exists but is
		// not yet visible can still converge through CompleteUpload.
		if _, err := p.inspectFile(callCtx, notification, false); err != nil {
			return "", false, classifyFileEvidenceError(err, "result_file_upload_unknown")
		}
	case domain.JobResultUploadUnknown:
		// Unknown upload outcomes are fail-closed: an existing file ID is
		// useful only when Slack proves that the file was already shared.
		if _, err := p.inspectFile(callCtx, notification, true); err != nil {
			return "", false, classifyFileEvidenceError(err, "result_file_upload_unknown")
		}
		if notification.Target.ThreadTS != "" {
			shared, err := p.fileSharedInThread(callCtx, notification)
			if err != nil {
				return "", false, classifyHistoryError(err)
			}
			if !shared {
				return "", false, permanentUploadError("result_file_upload_unknown")
			}
		}
	case domain.JobResultUploadCompleted:
		if _, err := p.inspectFile(callCtx, notification, true); err != nil {
			return "", false, classifyFileEvidenceError(err, "result_destination_mismatch")
		}
		if notification.Target.ThreadTS != "" {
			shared, err := p.fileSharedInThread(callCtx, notification)
			if err != nil {
				return "", false, classifyHistoryError(err)
			}
			if !shared {
				return "", false, permanentUploadError("result_destination_mismatch")
			}
		}
	default:
		return "", false, invalidNotificationError(errors.New("file upload stage is invalid"))
	}
	if notification.UploadState == domain.JobResultUploadURLRequested || notification.UploadState == domain.JobResultUploadBytesUploaded {
		return "", false, nil
	}
	var err error
	var messages []slackapi.Message
	if notification.Target.ThreadTS != "" {
		messages, err = p.history.client.ConversationReplies(callCtx, notification.Target.ChannelID, notification.Target.ThreadTS, "", 100)
	} else {
		messages, err = p.history.client.ConversationHistory(callCtx, notification.Target.ChannelID, "", 100)
	}
	if err != nil {
		return "", false, classifyHistoryError(err)
	}
	fileShared := notification.Target.ThreadTS == ""
	statusTS := ""
	for _, message := range messages {
		if containsSlackFile(message.Files, notification.SlackFileID) {
			if fileShared || message.Edited != nil || message.Hidden || message.User != p.history.botUserID || message.Timestamp == "" {
				return "", false, invalidNotificationError(errors.New("file delivery share evidence is inconsistent"))
			}
			fileShared = true
		}
		if message.Metadata.EventType != jobNotificationMetadataEventType || message.Edited != nil || message.Hidden || len(message.Files) != 0 || message.User != p.history.botUserID {
			continue
		}
		payload := message.Metadata.EventPayload
		jobID, _ := payload["job_id"].(string)
		mode, _ := payload["delivery_mode"].(string)
		policy, _ := payload["policy_version"].(string)
		fileID, _ := payload["file_id"].(string)
		kind, _ := payload["kind"].(string)
		renderer, _ := payload["renderer_version"].(string)
		partDigest, _ := payload["part_sha256"].(string)
		revision, _ := metadataInt(payload["status_revision"])
		index, _ := metadataInt(payload["part_index"])
		count, _ := metadataInt(payload["part_count"])
		parts := renderMarkdownV1(notification.CanonicalMarkdown, false)
		if jobID == notification.JobID && (mode != string(domain.JobResultDeliveryFile) || policy != notification.PolicyVersion || fileID != notification.SlackFileID || kind != notification.Kind || renderer != notification.RendererVersion || !notificationEvidenceDigestMatch(payload, notification) || index != 1 || count != 1 || len(parts) != 1 || partDigest != contentSHA256(parts[0]) || revision != notification.StatusRevision || message.Timestamp == "") {
			if jobID == notification.JobID {
				return "", false, invalidNotificationError(errors.New("file delivery status evidence is inconsistent"))
			}
			continue
		}
		if jobID == notification.JobID {
			if statusTS != "" {
				return "", false, invalidNotificationError(errors.New("file delivery status evidence is duplicated"))
			}
			statusTS = message.Timestamp
		}
	}
	if !fileShared {
		return "", false, permanentUploadError("result_destination_mismatch")
	}
	if statusTS != "" {
		return statusTS, true, nil
	}
	return "", false, nil
}

func (p *JobNotificationPublisher) fileSharedInThread(ctx context.Context, notification domain.ExternalAgentJobNotification) (bool, error) {
	if p == nil || p.history == nil || p.history.client == nil || notification.Target.ThreadTS == "" {
		return false, errors.New("file delivery thread evidence is unavailable")
	}
	messages, err := p.history.client.ConversationReplies(ctx, notification.Target.ChannelID, notification.Target.ThreadTS, "", 100)
	if err != nil {
		return false, err
	}
	found := false
	for _, message := range messages {
		if !containsSlackFile(message.Files, notification.SlackFileID) {
			continue
		}
		if found || message.Edited != nil || message.Hidden || message.User != p.history.botUserID || message.Timestamp == "" {
			return false, errors.New("file delivery share evidence is inconsistent")
		}
		found = true
	}
	return found, nil
}

func containsSlackFile(files []slackapi.File, fileID string) bool {
	for _, file := range files {
		if file.ID == fileID && fileID != "" {
			return true
		}
	}
	return false
}

func (p *JobNotificationPublisher) inspectFile(ctx context.Context, notification domain.ExternalAgentJobNotification, requireVisible bool) (*slackapi.File, error) {
	if p == nil || p.fileClient == nil || notification.SlackFileID == "" {
		return nil, errors.New("file delivery evidence is unavailable")
	}
	file, _, _, err := p.fileClient.GetFileInfoContext(ctx, notification.SlackFileID, 1, 1)
	if err != nil || file == nil {
		return nil, errors.New("file delivery evidence is ambiguous")
	}
	contentBytes := notification.ResultBytes
	if contentBytes <= 0 {
		contentBytes = notification.ContentBytes
	}
	botUserID := ""
	if p.history != nil {
		botUserID = p.history.botUserID
	}
	if file.ID != notification.SlackFileID || file.Name != "opencode-"+notification.JobID+".md" || file.Size != int(contentBytes) || (requireVisible && !fileVisibleInChannel(file, notification.Target.ChannelID)) || (file.User != "" && botUserID != "" && file.User != botUserID) {
		return nil, errors.New("file delivery evidence is inconsistent")
	}
	return file, nil
}

func fileVisibleInChannel(file *slackapi.File, channelID string) bool {
	if file == nil || channelID == "" {
		return false
	}
	for _, candidate := range append(append(append([]string{}, file.Channels...), file.Groups...), file.IMs...) {
		if candidate == channelID {
			return true
		}
	}
	return false
}

var _ port.JobNotificationPublisher = (*JobNotificationPublisher)(nil)
