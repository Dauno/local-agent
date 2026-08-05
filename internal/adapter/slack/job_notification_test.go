package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestJobNotificationPublisherUsesDeterministicMetadata(t *testing.T) {
	job := domain.ExternalAgentJob{ID: "job-1", Mode: domain.JobDetached, Status: domain.JobCompleted, StatusRevision: 3, ResultSummary: "safe", ConversationKey: "slack:T12345678:dm:D12345678", UpdatedAt: time.Now().UTC()}
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
	if req.extraMetadata["notification_sha256"] != notification.NotificationSHA256 || req.extraMetadata["notification_bytes"] != int64(len([]byte(notification.CanonicalMarkdown))) {
		t.Fatalf("notification identity metadata = %#v", req.extraMetadata)
	}
	if req.extraMetadata["result_sha256"] != notification.ResultSHA256 || req.extraMetadata["result_bytes"] != notification.ResultBytes {
		t.Fatalf("result identity metadata = %#v", req.extraMetadata)
	}
	if req.extraMetadata["notification_sha256"] == req.extraMetadata["result_sha256"] && req.extraMetadata["result_sha256"] != "" {
		t.Fatal("notification and result identity metadata collide")
	}
}

func TestJobNotificationPublisherEmitsDistinctIdentityPairsForDeliveredResult(t *testing.T) {
	text := "safe delivered result"
	job := domain.ExternalAgentJob{ID: "job-1", Mode: domain.JobDetached, Status: domain.JobCompleted, StatusRevision: 3, ConversationKey: "slack:T12345678:dm:D12345678"}
	notification, err := domain.NewExternalAgentJobDelivery(job, domain.AcpInvocationResult{
		Text: text, ResultSHA256: contentSHA256(text), ResultBytes: int64(len([]byte(text))),
		DeliveryMode: domain.JobResultDeliveryMarkdown, DeliveryPolicyVersion: domain.JobDeliveryPolicyV1,
		DeliveryContentSHA256: contentSHA256(text), DeliveryContentBytes: int64(len([]byte(text))),
		DeliveryCanonicalMarkdown: "OpenCode job `job-1` completed.\n\n" + text, DeliveryMaxMarkdownParts: 1,
	})
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
	metadata := recorder.requests[0].extraMetadata
	notificationDigest, _ := metadata["notification_sha256"].(string)
	resultDigest, _ := metadata["result_sha256"].(string)
	if notificationDigest != notification.NotificationSHA256 || resultDigest != notification.ResultSHA256 {
		t.Fatalf("identity pairs = notification %q / result %q", notificationDigest, resultDigest)
	}
	if notificationDigest == resultDigest {
		t.Fatal("delivered notification and result identities are not distinct")
	}
	if notificationDigest == contentSHA256(text) {
		t.Fatal("notification identity leaked the result digest")
	}
	if resultDigest != contentSHA256(text) {
		t.Fatal("result identity does not match the sanitized result digest")
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

func TestFileNotificationPublishesCompleteExternalUpload(t *testing.T) {
	content := "complete sanitized result"
	notification := fileTestNotification(content, domain.JobResultUploadPending, "")
	uploader := &jobNotificationUploader{}
	deliveryStore := &jobNotificationDeliveryStore{}
	recorder := &jobNotificationPostRecorder{}
	publisher := newPublisher(recorder, 0, nil, false)
	publisher.pace = 0

	response, err := NewDurableJobNotificationPublisher(
		publisher, nil, uploader, &jobNotificationArtifacts{}, deliveryStore, nil,
	).Publish(t.Context(), notification)
	if err != nil {
		t.Fatal(err)
	}
	if response.LastMessageTS == "" || uploader.requestedFilename != "opencode-job-1.md" || uploader.requestedBytes != len(content) {
		t.Fatalf("response=%#v requested=%q/%d", response, uploader.requestedFilename, uploader.requestedBytes)
	}
	if string(uploader.uploaded) != content || uploader.completedFileID != "F123" || uploader.completedChannel != "D12345678" || uploader.completedThread != "" {
		t.Fatalf("uploaded=%q completed=%q/%q/%q", uploader.uploaded, uploader.completedFileID, uploader.completedChannel, uploader.completedThread)
	}
	if len(deliveryStore.fileIDs) != 1 || deliveryStore.fileIDs[0] != "F123" || len(deliveryStore.states) != 2 || deliveryStore.states[0] != domain.JobResultUploadBytesUploaded || deliveryStore.states[1] != domain.JobResultUploadCompleted {
		t.Fatalf("persisted file IDs=%v states=%v", deliveryStore.fileIDs, deliveryStore.states)
	}
	if len(recorder.requests) != 1 || recorder.requests[0].extraMetadata["file_id"] != "F123" {
		t.Fatalf("status requests=%#v", recorder.requests)
	}
}

func TestFileNotificationRestartContinuesPersistedUploadStages(t *testing.T) {
	content := "complete sanitized result"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/files.info" {
			http.NotFound(w, request)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"file":{"id":"F123","name":"opencode-job-1.md","size":%d,"user":"BOT"}}`, len(content))
	}))
	t.Cleanup(server.Close)
	fileClient := slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.URL+"/"))

	for _, state := range []domain.JobResultUploadState{domain.JobResultUploadURLRequested, domain.JobResultUploadBytesUploaded} {
		t.Run(string(state), func(t *testing.T) {
			notification := fileTestNotification(content, state, "F123")
			uploader := &jobNotificationUploader{}
			deliveryStore := &jobNotificationDeliveryStore{}
			recorder := &jobNotificationPostRecorder{}
			publisher := newPublisher(recorder, 0, nil, false)
			publisher.pace = 0

			response, err := NewDurableJobNotificationPublisher(
				publisher, nil, uploader, &jobNotificationArtifacts{}, deliveryStore, fileClient,
			).Publish(t.Context(), notification)
			if err != nil {
				t.Fatal(err)
			}
			if response.LastMessageTS == "" || uploader.requestedFilename != "" || len(uploader.uploaded) != 0 || uploader.completedFileID != "F123" {
				t.Fatalf("response=%#v uploader=%+v", response, uploader)
			}
			if len(deliveryStore.states) != 1 || deliveryStore.states[0] != domain.JobResultUploadCompleted || len(recorder.requests) != 1 {
				t.Fatalf("persisted states=%v requests=%d", deliveryStore.states, len(recorder.requests))
			}
		})
	}
}

func TestFileNotificationRestartAfterCompletionPublishesOnlyStatus(t *testing.T) {
	content := "complete sanitized result"
	notification := fileTestNotification(content, domain.JobResultUploadCompleted, "F123")
	uploader := &jobNotificationUploader{}
	deliveryStore := &jobNotificationDeliveryStore{}
	recorder := &jobNotificationPostRecorder{}
	publisher := newPublisher(recorder, 0, nil, false)
	publisher.pace = 0

	response, err := NewDurableJobNotificationPublisher(
		publisher, nil, uploader, &jobNotificationArtifacts{}, deliveryStore, nil,
	).Publish(t.Context(), notification)
	if err != nil {
		t.Fatal(err)
	}
	if response.LastMessageTS == "" || uploader.requestedFilename != "" || len(uploader.uploaded) != 0 || uploader.completedFileID != "" || len(deliveryStore.states) != 0 || len(recorder.requests) != 1 {
		t.Fatalf("response=%#v uploader=%+v states=%v requests=%d", response, uploader, deliveryStore.states, len(recorder.requests))
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
			"renderer_version": notification.RendererVersion, "notification_sha256": notification.NotificationSHA256,
			"part_sha256": contentSHA256(part), "part_index": index, "part_count": count,
		}},
	}}
}

func fileTestNotification(content string, state domain.JobResultUploadState, fileID string) domain.ExternalAgentJobNotification {
	digest := contentSHA256(content)
	return domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 3, Kind: domain.JobNotificationTerminal,
		Actor: "U12345678", ConversationKey: "slack:T12345678:dm:D12345678", HostResultText: content,
		CanonicalMarkdown: fmt.Sprintf("OpenCode job `job-1` completed. The complete result was attached as `opencode-job-1.md` (%d bytes, SHA-256 `%s`).", len(content), digest),
		ContentSHA256:     digest, ContentBytes: int64(len(content)), RendererVersion: domain.JobNotificationRenderer,
		Target:       domain.ReplyTarget{ChannelID: "D12345678", CorrelationID: "job:job-1:3:terminal"},
		DeliveryMode: domain.JobResultDeliveryFile, PolicyVersion: domain.JobDeliveryPolicyV1,
		ArtifactRef: "job-1-delivery.result", MaxMarkdownParts: 6, UploadState: state, SlackFileID: fileID,
	}
}

type jobNotificationUploader struct {
	requestedFilename string
	requestedBytes    int
	uploaded          []byte
	completedFileID   string
	completedChannel  string
	completedThread   string
}

func (u *jobNotificationUploader) RequestUploadURL(ctx context.Context, filename string, sizeBytes int) (port.GeneratedFileUploadTarget, error) {
	return u.RequestMarkdownUploadURL(ctx, filename, sizeBytes)
}

func (u *jobNotificationUploader) RequestMarkdownUploadURL(_ context.Context, filename string, sizeBytes int) (port.GeneratedFileUploadTarget, error) {
	u.requestedFilename, u.requestedBytes = filename, sizeBytes
	return port.GeneratedFileUploadTarget{FileID: "F123", UploadURL: "https://upload.invalid/F123"}, nil
}

func (u *jobNotificationUploader) UploadBytes(_ context.Context, _ port.GeneratedFileUploadTarget, content []byte) error {
	u.uploaded = append([]byte(nil), content...)
	return nil
}

func (u *jobNotificationUploader) CompleteUpload(_ context.Context, fileID, channelID, threadTS, _ string) error {
	u.completedFileID, u.completedChannel, u.completedThread = fileID, channelID, threadTS
	return nil
}

type jobNotificationArtifacts struct{}

func (*jobNotificationArtifacts) Put(context.Context, string, string) (domain.ResultArtifact, error) {
	return domain.ResultArtifact{}, errors.New("unexpected artifact write")
}

func (*jobNotificationArtifacts) Get(context.Context, string, string, string, int64) ([]byte, error) {
	return nil, errors.New("unexpected artifact read")
}

type jobNotificationDeliveryStore struct {
	fileIDs []string
	states  []domain.JobResultUploadState
}

func (s *jobNotificationDeliveryStore) MarkNotificationFileID(_ context.Context, _ *domain.ExternalAgentJobNotification, fileID string, _ time.Time) error {
	s.fileIDs = append(s.fileIDs, fileID)
	return nil
}

func (s *jobNotificationDeliveryStore) MarkNotificationUploadState(_ context.Context, _ *domain.ExternalAgentJobNotification, state domain.JobResultUploadState, _ time.Time) error {
	s.states = append(s.states, state)
	return nil
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
