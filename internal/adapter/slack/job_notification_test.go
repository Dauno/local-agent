package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestJobNotificationPublisherUsesDeterministicMetadata(t *testing.T) {
	job := domain.ExternalAgentJob{ID: "job-1", Status: domain.JobCompleted, StatusRevision: 3, ResultSummary: "safe", ConversationKey: "slack:T12345678:dm:D12345678", UpdatedAt: time.Now().UTC()}
	notification, err := domain.NewExternalAgentJobNotification(job)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &jobNotificationPostRecorder{}
	publisher := newPublisher(recorder, 0, nil, false)
	publisher.pace = 0
	if _, err := NewJobNotificationPublisher(publisher, nil).Publish(t.Context(), notification); err != nil {
		t.Fatal(err)
	}
	if len(recorder.requests) != 1 {
		t.Fatalf("requests = %d", len(recorder.requests))
	}
	req := recorder.requests[0]
	if req.eventType != jobNotificationMetadataEventType || req.extraMetadata["job_id"] != "job-1" || req.extraMetadata["status_revision"] != 3 || req.extraMetadata["content_sha256"] != notification.ContentSHA256 {
		t.Fatalf("metadata = %#v", req)
	}
}

func TestJobNotificationHistoryRejectsPartialEvidenceBeforeRetry(t *testing.T) {
	job := domain.ExternalAgentJob{ID: "job-1", Status: domain.JobCompleted, StatusRevision: 3, ResultSummary: "safe", ConversationKey: "slack:T12345678:dm:D12345678", UpdatedAt: time.Now().UTC()}
	notification, err := domain.NewExternalAgentJobNotification(job)
	if err != nil {
		t.Fatal(err)
	}
	history := newHistoryReader(&jobNotificationHistoryRecorder{messages: []slackapi.Message{{Msg: slackapi.Msg{
		User: "BOT", Timestamp: "1710000000.000001", Metadata: slackapi.SlackMetadata{EventType: jobNotificationMetadataEventType, EventPayload: map[string]any{
			"job_id": notification.JobID, "status_revision": notification.StatusRevision, "kind": notification.Kind,
			"renderer_version": notification.RendererVersion, "content_sha256": notification.ContentSHA256,
			"part_sha256": contentSHA256(notification.CanonicalMarkdown), "part_index": 1, "part_count": 2,
		}},
	}}}}, "BOT", 0, nil, false)
	_, found, err := NewJobNotificationPublisher(nil, history).Reconcile(t.Context(), notification)
	if err == nil || found {
		t.Fatalf("partial evidence found=%v err=%v", found, err)
	}
}

func TestFileNotificationWithoutSlackIdentityRetriesInsteadOfReconciling(t *testing.T) {
	notification := domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 3, Kind: domain.JobNotificationTerminal,
		CanonicalMarkdown: "OpenCode job `job-1` completed. The complete result was attached.",
		ContentSHA256:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ContentBytes:      4, RendererVersion: domain.JobNotificationRenderer,
		Target: domain.ReplyTarget{ChannelID: "D12345678"}, ConversationKey: "slack:T12345678:dm:D12345678", DeliveryMode: domain.JobResultDeliveryFile,
		PolicyVersion: domain.JobDeliveryPolicyV1, ArtifactRef: "job-1-delivery.result",
		MaxMarkdownParts: 6, UploadState: domain.JobResultUploadUnknown,
	}
	history := newHistoryReader(&jobNotificationHistoryRecorder{}, "BOT", 0, nil, false)
	_, found, err := NewDurableJobNotificationPublisher(nil, history, nil, nil, nil, nil).Reconcile(t.Context(), notification)
	if err != nil || found {
		t.Fatalf("file delivery without Slack identity found=%v err=%v", found, err)
	}
}

func TestFileShareEvidenceRequiresOriginalThread(t *testing.T) {
	notification := domain.ExternalAgentJobNotification{
		SlackFileID: "F123", Target: domain.ReplyTarget{ChannelID: "C123", ThreadTS: "1710000000.000001"},
	}
	history := newHistoryReader(&jobNotificationHistoryRecorder{messages: []slackapi.Message{{Msg: slackapi.Msg{
		User: "BOT", Timestamp: "1710000000.000002", Files: []slackapi.File{{ID: "F999"}},
	}}}}, "BOT", 0, nil, false)
	shared, err := NewDurableJobNotificationPublisher(nil, history, nil, nil, nil, nil).fileSharedInThread(t.Context(), notification)
	if err != nil || shared {
		t.Fatalf("foreign file share shared=%v err=%v", shared, err)
	}
	history.client = &jobNotificationHistoryRecorder{messages: []slackapi.Message{{Msg: slackapi.Msg{
		User: "BOT", Timestamp: "1710000000.000002", Files: []slackapi.File{{ID: "F123"}},
	}}}}
	shared, err = NewDurableJobNotificationPublisher(nil, history, nil, nil, nil, nil).fileSharedInThread(t.Context(), notification)
	if err != nil || !shared {
		t.Fatalf("matching file share shared=%v err=%v", shared, err)
	}
}

func TestJobNotificationRejectsDestinationMismatch(t *testing.T) {
	content := "safe result"
	notification := domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 3, Kind: domain.JobNotificationTerminal,
		CanonicalMarkdown: "OpenCode job `job-1` completed.\n\n" + content,
		ContentSHA256:     contentSHA256(content), ContentBytes: int64(len(content)),
		RendererVersion: domain.JobNotificationRenderer,
		Target:          domain.ReplyTarget{ChannelID: "D99999999"}, ConversationKey: "slack:T12345678:dm:D12345678",
		DeliveryMode: domain.JobResultDeliveryMarkdown, PolicyVersion: domain.JobDeliveryPolicyV1, MaxMarkdownParts: 1,
	}
	_, err := NewJobNotificationPublisher(nil, nil).Publish(t.Context(), notification)
	var classified *port.NotificationPublishError
	if !errors.As(err, &classified) || classified.Code != "result_destination_mismatch" || classified.Retryable {
		t.Fatalf("mismatch error = %#v", err)
	}
}

func TestJobNotificationMarkdownRecoveryBoundariesAndEvidence(t *testing.T) {
	for _, wantParts := range []int{2, 6} {
		t.Run(fmt.Sprintf("%d_parts", wantParts), func(t *testing.T) {
			notification := markdownTestNotification(t, wantParts)
			parts := renderMarkdownV1(notification.CanonicalMarkdown, false)
			messages := make([]slackapi.Message, 0, len(parts))
			for index, part := range parts {
				messages = append(messages, jobEvidenceMessage(notification, index+1, len(parts), part, fmt.Sprintf("171000000%d.%06d", index+1, index+1)))
			}
			history := newHistoryReader(&jobNotificationHistoryRecorder{messages: messages}, "BOT", 0, nil, false)
			got, found, err := NewJobNotificationPublisher(nil, history).Reconcile(t.Context(), notification)
			if err != nil || !found || got == "" {
				t.Fatalf("recovery = %q, found=%v, err=%v", got, found, err)
			}
		})
	}

	base := markdownTestNotification(t, 2)
	parts := renderMarkdownV1(base.CanonicalMarkdown, false)
	duplicate := []slackapi.Message{
		jobEvidenceMessage(base, 1, len(parts), parts[0], "1710000001.000001"),
		jobEvidenceMessage(base, 1, len(parts), parts[0], "1710000002.000002"),
	}
	if _, found, err := NewJobNotificationPublisher(nil, newHistoryReader(&jobNotificationHistoryRecorder{messages: duplicate}, "BOT", 0, nil, false)).Reconcile(t.Context(), base); err == nil || found {
		t.Fatalf("duplicate evidence found=%v err=%v", found, err)
	}
	edited := jobEvidenceMessage(base, 1, len(parts), parts[0], "1710000001.000001")
	edited.Edited = &slackapi.Edited{}
	if _, found, err := NewJobNotificationPublisher(nil, newHistoryReader(&jobNotificationHistoryRecorder{messages: []slackapi.Message{edited}}, "BOT", 0, nil, false)).Reconcile(t.Context(), base); err == nil || found {
		t.Fatalf("edited evidence found=%v err=%v", found, err)
	}
	reordered := []slackapi.Message{
		jobEvidenceMessage(base, 1, len(parts), parts[0], "1710000002.000002"),
		jobEvidenceMessage(base, 2, len(parts), parts[1], "1710000001.000001"),
	}
	if _, found, err := NewJobNotificationPublisher(nil, newHistoryReader(&jobNotificationHistoryRecorder{messages: reordered}, "BOT", 0, nil, false)).Reconcile(t.Context(), base); err == nil || found {
		t.Fatalf("reordered evidence found=%v err=%v", found, err)
	}
	foreign := jobEvidenceMessage(base, 1, len(parts), parts[0], "1710000001.000001")
	foreign.Metadata.EventPayload["job_id"] = "foreign-job"
	if _, found, err := NewJobNotificationPublisher(nil, newHistoryReader(&jobNotificationHistoryRecorder{messages: []slackapi.Message{foreign}}, "BOT", 0, nil, false)).Reconcile(t.Context(), base); err != nil || found {
		t.Fatalf("foreign evidence found=%v err=%v", found, err)
	}
}

func markdownTestNotification(t *testing.T, wantParts int) domain.ExternalAgentJobNotification {
	t.Helper()
	text := strings.Repeat("界", domain.SlackMarkdownChunkRunes*(wantParts-1)-20)
	job := domain.ExternalAgentJob{ID: fmt.Sprintf("job-%d", wantParts), Status: domain.JobCompleted, StatusRevision: 3, Actor: "U12345678", ConversationKey: "slack:T12345678:dm:D12345678"}
	digest := contentSHA256(text)
	notification, err := domain.NewExternalAgentJobDelivery(job, domain.AcpInvocationResult{
		Text: text, ResultSHA256: digest, ResultBytes: int64(len([]byte(text))), DeliveryMode: domain.JobResultDeliveryMarkdown,
		DeliveryPolicyVersion: domain.JobDeliveryPolicyV1, DeliveryContentSHA256: digest, DeliveryContentBytes: int64(len([]byte(text))),
		DeliveryMaxMarkdownParts: 6, DeliveryCanonicalMarkdown: fmt.Sprintf("OpenCode job `%s` completed.\n\n%s", job.ID, text),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(renderMarkdownV1(notification.CanonicalMarkdown, false)); got != wantParts {
		t.Fatalf("rendered parts = %d, want %d", got, wantParts)
	}
	return notification
}

func jobEvidenceMessage(notification domain.ExternalAgentJobNotification, index, count int, part, timestamp string) slackapi.Message {
	return slackapi.Message{Msg: slackapi.Msg{
		User: "BOT", Timestamp: timestamp,
		Metadata: slackapi.SlackMetadata{EventType: jobNotificationMetadataEventType, EventPayload: map[string]any{
			"job_id": notification.JobID, "status_revision": notification.StatusRevision, "kind": notification.Kind,
			"renderer_version": notification.RendererVersion, "notification_sha256": notification.ContentSHA256,
			"part_sha256": contentSHA256(part), "part_index": index, "part_count": count,
		}},
	}}
}

type jobNotificationPostRecorder struct{ requests []postRequest }

func (r *jobNotificationPostRecorder) PostMessage(_ context.Context, request postRequest) (string, error) {
	r.requests = append(r.requests, request)
	return "1710000000.000001", nil
}

type jobNotificationHistoryRecorder struct{ messages []slackapi.Message }

func (r *jobNotificationHistoryRecorder) ConversationReplies(context.Context, string, string, string, int) ([]slackapi.Message, error) {
	return r.messages, nil
}
func (r *jobNotificationHistoryRecorder) ConversationHistory(context.Context, string, string, int) ([]slackapi.Message, error) {
	return r.messages, nil
}
