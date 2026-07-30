package slack

import (
	"context"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
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
		Target: domain.ReplyTarget{ChannelID: "D12345678"}, DeliveryMode: domain.JobResultDeliveryFile,
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
