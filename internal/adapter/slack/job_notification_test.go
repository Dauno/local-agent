package slack

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
	job := domain.ExternalAgentJob{
		ID:              "job-1",
		Mode:            domain.JobDetached,
		Status:          domain.JobCompleted,
		StatusRevision:  3,
		ResultSummary:   "safe",
		ConversationKey: "slack:T12345678:dm:D12345678",
		UpdatedAt:       time.Now().UTC(),
	}
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
	if req.eventType != jobNotificationMetadataEventType || req.extraMetadata["job_id"] != "job-1" || req.extraMetadata["status_revision"] != 3 ||
		req.extraMetadata["content_sha256"] != notification.NotificationSHA256 {
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
	notification, err := domain.NewExternalAgentJobDelivery(job, domain.ExternalAgentInvocationResult{
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
	history := newHistoryReader(&jobNotificationHistoryRecorder{messages: []slackapi.Message{{
		User: "BOT", Timestamp: "1710000000.000001", Metadata: slackapi.SlackMetadata{EventType: jobNotificationMetadataEventType, EventPayload: map[string]any{
			"job_id": notification.JobID, "status_revision": notification.StatusRevision, "kind": notification.Kind,
			"renderer_version": notification.RendererVersion, "content_sha256": notification.NotificationSHA256,
			"part_sha256": contentSHA256(notification.CanonicalMarkdown), "part_index": 1, "part_count": 2,
		}},
	}}}, "BOT", 0, nil, false)
	_, found, err := NewJobNotificationPublisher(nil, history).Reconcile(t.Context(), notification)
	if err == nil || found {
		t.Fatalf("partial evidence found=%v err=%v", found, err)
	}
}

func TestFileNotificationWithoutSlackIdentityRetriesInsteadOfReconciling(t *testing.T) {
	notification := domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 3, Kind: domain.JobNotificationTerminal,
		CanonicalMarkdown: "OpenCode job `job-1` completed. The complete result was attached.",
		ResultSHA256:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ResultBytes:       4, RendererVersion: domain.JobNotificationRenderer,
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
	history := newHistoryReader(&jobNotificationHistoryRecorder{messages: []slackapi.Message{{
		User: "BOT", Timestamp: "1710000000.000002", Files: []slackapi.File{{ID: "F999"}},
	}}}, "BOT", 0, nil, false)
	shared, err := NewDurableJobNotificationPublisher(nil, history, nil, nil, nil, nil).fileSharedInThread(t.Context(), notification)
	if err != nil || shared {
		t.Fatalf("foreign file share shared=%v err=%v", shared, err)
	}
	history.client = &jobNotificationHistoryRecorder{messages: []slackapi.Message{{
		User: "BOT", Timestamp: "1710000000.000002", Files: []slackapi.File{{ID: "F123"}},
	}}}
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
		ResultSHA256:      contentSHA256(content), ResultBytes: int64(len(content)),
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
	if len(deliveryStore.fileIDs) != 1 || deliveryStore.fileIDs[0] != "F123" || len(deliveryStore.states) != 2 || deliveryStore.states[0] != domain.JobResultUploadBytesUploaded ||
		deliveryStore.states[1] != domain.JobResultUploadCompleted {
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
	if response.LastMessageTS == "" || uploader.requestedFilename != "" || len(uploader.uploaded) != 0 || uploader.completedFileID != "" || len(deliveryStore.states) != 0 ||
		len(recorder.requests) != 1 {
		t.Fatalf("response=%#v uploader=%+v states=%v requests=%d", response, uploader, deliveryStore.states, len(recorder.requests))
	}
}

func TestJobNotificationValidationVerifiesCanonicalMarkdownIdentity(t *testing.T) {
	markdown := "OpenCode job `job-1` completed.\n\nsafe result"
	fileMarkdown := "OpenCode job `job-1` completed. The complete result was attached."
	otherDigest := contentSHA256("some other result")
	build := func(mode domain.JobResultDeliveryMode, mutate func(*domain.ExternalAgentJobNotification)) domain.ExternalAgentJobNotification {
		t.Helper()
		var notification domain.ExternalAgentJobNotification
		if mode == domain.JobResultDeliveryFile {
			notification = preV32DeliveryNotification(mode, fileMarkdown, "file bytes", "job-1-delivery.result")
			notification.HostResultText = "file bytes"
		} else {
			notification = preV32DeliveryNotification(mode, markdown, "safe result", "")
			notification.UploadState = domain.JobResultUploadNotApplicable
			notification.SlackFileID = ""
		}
		if mutate != nil {
			mutate(&notification)
		}
		return notification
	}
	publish := func(notification domain.ExternalAgentJobNotification) error {
		recorder := &jobNotificationPostRecorder{}
		publisher := newPublisher(recorder, 0, nil, false)
		publisher.pace = 0
		_, err := NewDurableJobNotificationPublisher(publisher, nil, &jobNotificationUploader{}, &jobNotificationArtifacts{}, &jobNotificationDeliveryStore{}, nil).Publish(t.Context(), notification)
		if err == nil && len(recorder.requests) != 1 {
			t.Fatalf("published requests = %d, want 1", len(recorder.requests))
		}
		return err
	}
	cases := []struct {
		name      string
		mode      domain.JobResultDeliveryMode
		mutate    func(*domain.ExternalAgentJobNotification)
		wantError bool
	}{
		{name: "v32 exact match publishes", mode: domain.JobResultDeliveryMarkdown},
		{name: "v32 exact match publishes", mode: domain.JobResultDeliveryFile},
		{
			name:      "non-hex notification digest rejected",
			mode:      domain.JobResultDeliveryMarkdown,
			mutate:    func(n *domain.ExternalAgentJobNotification) { n.NotificationSHA256 = strings.Repeat("g", 64) },
			wantError: true,
		},
		{
			name:      "non-hex notification digest rejected",
			mode:      domain.JobResultDeliveryFile,
			mutate:    func(n *domain.ExternalAgentJobNotification) { n.NotificationSHA256 = strings.Repeat("g", 64) },
			wantError: true,
		},
		{
			name:      "uppercase notification digest rejected",
			mode:      domain.JobResultDeliveryMarkdown,
			mutate:    func(n *domain.ExternalAgentJobNotification) { n.NotificationSHA256 = strings.Repeat("A", 64) },
			wantError: true,
		},
		{
			name:      "mismatched notification digest rejected",
			mode:      domain.JobResultDeliveryMarkdown,
			mutate:    func(n *domain.ExternalAgentJobNotification) { n.NotificationSHA256 = otherDigest },
			wantError: true,
		},
		{
			name:      "mismatched notification digest rejected",
			mode:      domain.JobResultDeliveryFile,
			mutate:    func(n *domain.ExternalAgentJobNotification) { n.NotificationSHA256 = otherDigest },
			wantError: true,
		},
		{name: "mismatched notification bytes rejected", mode: domain.JobResultDeliveryMarkdown, mutate: func(n *domain.ExternalAgentJobNotification) { n.NotificationBytes++ }, wantError: true},
		{name: "mismatched notification bytes rejected", mode: domain.JobResultDeliveryFile, mutate: func(n *domain.ExternalAgentJobNotification) { n.NotificationBytes++ }, wantError: true},
		{name: "partial v32 digest only rejected", mode: domain.JobResultDeliveryMarkdown, mutate: func(n *domain.ExternalAgentJobNotification) { n.NotificationBytes = 0 }, wantError: true},
		{name: "partial v32 bytes only rejected", mode: domain.JobResultDeliveryMarkdown, mutate: func(n *domain.ExternalAgentJobNotification) { n.NotificationSHA256 = "" }, wantError: true},
		{name: "partial v32 digest only rejected", mode: domain.JobResultDeliveryFile, mutate: func(n *domain.ExternalAgentJobNotification) { n.NotificationBytes = 0 }, wantError: true},
		{name: "partial v32 bytes only rejected", mode: domain.JobResultDeliveryFile, mutate: func(n *domain.ExternalAgentJobNotification) { n.NotificationSHA256 = "" }, wantError: true},
		{
			name:      "negative notification bytes rejected",
			mode:      domain.JobResultDeliveryMarkdown,
			mutate:    func(n *domain.ExternalAgentJobNotification) { n.NotificationSHA256, n.NotificationBytes = "", -1 },
			wantError: true,
		},
		{
			name:      "negative notification bytes rejected",
			mode:      domain.JobResultDeliveryFile,
			mutate:    func(n *domain.ExternalAgentJobNotification) { n.NotificationSHA256, n.NotificationBytes = "", -1 },
			wantError: true,
		},
		{
			name:      "negative notification bytes rejected",
			mode:      domain.JobResultDeliveryMarkdown,
			mutate:    func(n *domain.ExternalAgentJobNotification) { n.NotificationSHA256, n.NotificationBytes = "", -5 },
			wantError: true,
		},
		{
			name:      "negative notification bytes rejected",
			mode:      domain.JobResultDeliveryFile,
			mutate:    func(n *domain.ExternalAgentJobNotification) { n.NotificationSHA256, n.NotificationBytes = "", -5 },
			wantError: true,
		},
		{name: "missing notification identity rejected", mode: domain.JobResultDeliveryMarkdown, mutate: func(n *domain.ExternalAgentJobNotification) {
			n.NotificationSHA256, n.NotificationBytes = "", 0
		}, wantError: true},
	}
	for _, scenario := range cases {
		t.Run(scenario.name+" "+string(scenario.mode), func(t *testing.T) {
			notification := build(scenario.mode, scenario.mutate)
			err := publish(notification)
			if (err != nil) != scenario.wantError {
				t.Fatalf("publish = %v, wantError %v", err, scenario.wantError)
			}
		})
	}

	// Legacy markdown rows use the canonical notification identity after v32
	// backfill; the old storage digest is not part of the domain object.
	markdownDigest := domain.NotificationIdentitySHA256(markdown)
	legacy := domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 1, Kind: domain.JobNotificationTerminal,
		CanonicalMarkdown: markdown, NotificationSHA256: markdownDigest, NotificationBytes: int64(len([]byte(markdown))),
		RendererVersion: domain.JobNotificationRenderer,
		Target:          domain.ReplyTarget{ChannelID: "D12345678"}, ConversationKey: "slack:T12345678:dm:D12345678",
		DeliveryMode: domain.JobResultDeliveryMarkdown, PolicyVersion: "legacy_v1",
		UploadState: domain.JobResultUploadNotApplicable, MaxMarkdownParts: 6,
	}
	if err := publish(legacy); err != nil {
		t.Fatalf("legacy markdown fallback publish = %v", err)
	}
	// Legacy file rows retain their result identity for upload verification.
	fileLegacy := build(domain.JobResultDeliveryFile, func(n *domain.ExternalAgentJobNotification) { n.NotificationSHA256, n.NotificationBytes = "", 0 })
	if err := publish(fileLegacy); err != nil {
		t.Fatalf("legacy file fallback publish = %v", err)
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
	if _, found, err := NewJobNotificationPublisher(
		nil,
		newHistoryReader(&jobNotificationHistoryRecorder{messages: duplicate}, "BOT", 0, nil, false),
	).Reconcile(
		t.Context(),
		base,
	); err == nil ||
		found {
		t.Fatalf("duplicate evidence found=%v err=%v", found, err)
	}
	edited := jobEvidenceMessage(base, 1, len(parts), parts[0], "1710000001.000001")
	edited.Edited = &slackapi.Edited{}
	if _, found, err := NewJobNotificationPublisher(
		nil,
		newHistoryReader(&jobNotificationHistoryRecorder{messages: []slackapi.Message{edited}}, "BOT", 0, nil, false),
	).Reconcile(
		t.Context(),
		base,
	); err == nil ||
		found {
		t.Fatalf("edited evidence found=%v err=%v", found, err)
	}
	reordered := []slackapi.Message{
		jobEvidenceMessage(base, 1, len(parts), parts[0], "1710000002.000002"),
		jobEvidenceMessage(base, 2, len(parts), parts[1], "1710000001.000001"),
	}
	if _, found, err := NewJobNotificationPublisher(
		nil,
		newHistoryReader(&jobNotificationHistoryRecorder{messages: reordered}, "BOT", 0, nil, false),
	).Reconcile(
		t.Context(),
		base,
	); err == nil ||
		found {
		t.Fatalf("reordered evidence found=%v err=%v", found, err)
	}
	foreign := jobEvidenceMessage(base, 1, len(parts), parts[0], "1710000001.000001")
	foreign.Metadata.EventPayload["job_id"] = "foreign-job"
	if _, found, err := NewJobNotificationPublisher(
		nil,
		newHistoryReader(&jobNotificationHistoryRecorder{messages: []slackapi.Message{foreign}}, "BOT", 0, nil, false),
	).Reconcile(
		t.Context(),
		base,
	); err != nil ||
		found {
		t.Fatalf("foreign evidence found=%v err=%v", found, err)
	}
}

// preV32DeliveryNotification returns a v32-identity row whose Slack delivery
// was published by the v30 binary: the payload carries the result digest in
// both notification_sha256 and content_sha256 and never carries the v32
// metadata fields (notification_bytes, result_sha256).
func preV32DeliveryNotification(mode domain.JobResultDeliveryMode, canonicalMarkdown, resultContent, artifactRef string) domain.ExternalAgentJobNotification {
	return domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 1, Kind: domain.JobNotificationTerminal,
		CanonicalMarkdown:  canonicalMarkdown,
		NotificationSHA256: domain.NotificationIdentitySHA256(canonicalMarkdown),
		NotificationBytes:  int64(len([]byte(canonicalMarkdown))),
		ResultSHA256:       contentSHA256(resultContent), ResultBytes: int64(len([]byte(resultContent))),
		RendererVersion: domain.JobNotificationRenderer,
		Target:          domain.ReplyTarget{ChannelID: "D12345678"},
		ConversationKey: "slack:T12345678:dm:D12345678",
		DeliveryMode:    mode, PolicyVersion: domain.JobDeliveryPolicyV1,
		ArtifactRef: artifactRef, MaxMarkdownParts: 6,
		UploadState: domain.JobResultUploadCompleted, SlackFileID: "F123",
	}
}

func preV32EvidencePayload(notification domain.ExternalAgentJobNotification) map[string]any {
	part := renderMarkdownV1(notification.CanonicalMarkdown, false)[0]
	payload := map[string]any{
		"job_id": notification.JobID, "status_revision": notification.StatusRevision, "kind": notification.Kind,
		"renderer_version": notification.RendererVersion, "delivery_mode": string(notification.DeliveryMode),
		"policy_version": notification.PolicyVersion, "notification_sha256": notification.ResultSHA256,
		"content_sha256": notification.ResultSHA256, "result_bytes": notification.ResultBytes,
		"max_markdown_parts": notification.MaxMarkdownParts, "upload_state": string(notification.UploadState),
		"part_sha256": contentSHA256(part), "part_index": 1, "part_count": 1,
	}
	if notification.SlackFileID != "" {
		payload["file_id"] = notification.SlackFileID
	}
	return payload
}

func TestJobNotificationReconcileAcceptsPreV32MarkdownEvidence(t *testing.T) {
	notification := preV32DeliveryNotification(domain.JobResultDeliveryMarkdown,
		"OpenCode job `job-1` completed.\n\nsafe result", "safe result", "")
	notification.UploadState = domain.JobResultUploadNotApplicable
	notification.SlackFileID = ""
	message := slackapi.Message{
		User: "BOT", Timestamp: "1710000000.000001",
		Metadata: slackapi.SlackMetadata{EventType: jobNotificationMetadataEventType, EventPayload: preV32EvidencePayload(notification)},
	}
	history := newHistoryReader(&jobNotificationHistoryRecorder{messages: []slackapi.Message{message}}, "BOT", 0, nil, false)
	got, found, err := NewJobNotificationPublisher(nil, history).Reconcile(t.Context(), notification)
	if err != nil || !found || got != "1710000000.000001" {
		t.Fatalf("pre-v32 markdown evidence = %q, found=%v, err=%v", got, found, err)
	}
}

func TestJobNotificationReconcileAcceptsPreV32FileEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/files.info" {
			http.NotFound(w, request)
			return
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"file":{"id":"F123","name":"opencode-job-1.md","size":10,"user":"BOT","channels":["D12345678"]}}`)
	}))
	t.Cleanup(server.Close)
	fileClient := slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.URL+"/"))
	notification := preV32DeliveryNotification(domain.JobResultDeliveryFile,
		"OpenCode job `job-1` completed. The complete result was attached.", "file bytes", "job-1-delivery.result")
	message := slackapi.Message{
		User: "BOT", Timestamp: "1710000000.000001",
		Metadata: slackapi.SlackMetadata{EventType: jobNotificationMetadataEventType, EventPayload: preV32EvidencePayload(notification)},
	}
	history := newHistoryReader(&jobNotificationHistoryRecorder{messages: []slackapi.Message{message}}, "BOT", 0, nil, false)
	got, found, err := NewDurableJobNotificationPublisher(nil, history, nil, nil, nil, fileClient).Reconcile(t.Context(), notification)
	if err != nil || !found || got != "1710000000.000001" {
		t.Fatalf("pre-v32 file evidence = %q, found=%v, err=%v", got, found, err)
	}
}

func TestJobNotificationReconcileFailsClosedOnEvidenceIdentityMismatch(t *testing.T) {
	notification := preV32DeliveryNotification(domain.JobResultDeliveryMarkdown,
		"OpenCode job `job-1` completed.\n\nsafe result", "safe result", "")
	notification.UploadState = domain.JobResultUploadNotApplicable
	notification.SlackFileID = ""
	part := renderMarkdownV1(notification.CanonicalMarkdown, false)[0]
	makeMessage := func(payload map[string]any) slackapi.Message {
		return slackapi.Message{
			User: "BOT", Timestamp: "1710000000.000001",
			Metadata: slackapi.SlackMetadata{EventType: jobNotificationMetadataEventType, EventPayload: payload},
		}
	}
	// A v32 payload whose notification_sha256 mismatches stays inconsistent
	// even when its content_sha256 matches the legacy content identity.
	v32Mismatch := preV32EvidencePayload(notification)
	v32Mismatch["notification_sha256"] = strings.Repeat("a", 64)
	v32Mismatch["notification_bytes"] = int64(len([]byte(notification.CanonicalMarkdown)))
	v32Mismatch["result_sha256"] = notification.ResultSHA256
	for name, payload := range map[string]map[string]any{
		"v32_digest_mismatch": v32Mismatch,
		"legacy_digest_mismatch": {
			"job_id": notification.JobID, "status_revision": notification.StatusRevision, "kind": notification.Kind,
			"renderer_version": notification.RendererVersion, "delivery_mode": "markdown", "policy_version": "delivery_v1",
			"notification_sha256": strings.Repeat("b", 64), "content_sha256": strings.Repeat("b", 64),
			"part_sha256": contentSHA256(part), "part_index": 1, "part_count": 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			history := newHistoryReader(&jobNotificationHistoryRecorder{messages: []slackapi.Message{makeMessage(payload)}}, "BOT", 0, nil, false)
			_, found, err := NewJobNotificationPublisher(nil, history).Reconcile(t.Context(), notification)
			if err == nil || found {
				t.Fatalf("mismatched evidence found=%v err=%v", found, err)
			}
		})
	}
}

// v32EvidencePayload reproduces the complete Slack metadata the v32 binary
// publishes for a single-part Markdown delivery: all four identity fields.
func v32EvidencePayload(notification domain.ExternalAgentJobNotification) map[string]any {
	part := renderMarkdownV1(notification.CanonicalMarkdown, false)[0]
	return map[string]any{
		"job_id": notification.JobID, "status_revision": notification.StatusRevision, "kind": notification.Kind,
		"renderer_version": notification.RendererVersion, "delivery_mode": string(notification.DeliveryMode),
		"policy_version": notification.PolicyVersion, "notification_sha256": notification.NotificationSHA256,
		"notification_bytes": notification.NotificationBytes, "result_sha256": notification.ResultSHA256,
		"result_bytes": notification.ResultBytes, "max_markdown_parts": notification.MaxMarkdownParts,
		"upload_state": string(notification.UploadState), "part_sha256": contentSHA256(part),
		"part_index": 1, "part_count": 1,
	}
}

// tamperEvidencePayload returns a copy of the payload with one value replaced.
func tamperEvidencePayload(payload map[string]any, key string, value any) map[string]any {
	clone := make(map[string]any, len(payload)+1)
	maps.Copy(clone, payload)
	clone[key] = value
	return clone
}

// onlyIdentityField returns a payload declaring just one of the identity
// fields (notification_sha256, notification_bytes, result_sha256,
// result_bytes); the other identity fields are removed.
func onlyIdentityField(payload map[string]any, keep string) map[string]any {
	clone := make(map[string]any, len(payload))
	maps.Copy(clone, payload)
	for _, field := range []string{"notification_sha256", "notification_bytes", "result_sha256", "result_bytes", "content_sha256"} {
		if field != keep {
			delete(clone, field)
		}
	}
	return clone
}

// TestJobNotificationReconcileRequiresAllIdentityFields proves the CR5
// correction: v32 evidence is published only when all four identity fields
// match with exact types, tampering any single field fails closed, a payload
// declaring a partial v32 identity is rejected instead of treated as legacy,
// and legacy evidence must match its complete legacy identity (both digest
// slots and the byte count) rather than a single digest.
func TestJobNotificationReconcileRequiresAllIdentityFields(t *testing.T) {
	notification := preV32DeliveryNotification(domain.JobResultDeliveryMarkdown,
		"OpenCode job `job-1` completed.\n\nsafe result", "safe result", "")
	notification.UploadState = domain.JobResultUploadNotApplicable
	notification.SlackFileID = ""
	makeMessage := func(payload map[string]any) slackapi.Message {
		return slackapi.Message{
			User: "BOT", Timestamp: "1710000000.000001",
			Metadata: slackapi.SlackMetadata{EventType: jobNotificationMetadataEventType, EventPayload: payload},
		}
	}
	v32 := v32EvidencePayload(notification)
	legacy := preV32EvidencePayload(notification)
	cases := map[string]struct {
		payload   map[string]any
		wantFound bool
		wantError bool
	}{
		"v32_all_four_fields_match":             {v32, true, false},
		"v32_tampered_notification_digest_only": {tamperEvidencePayload(v32, "notification_sha256", strings.Repeat("a", 64)), false, true},
		"v32_tampered_notification_bytes_only":  {tamperEvidencePayload(v32, "notification_bytes", notification.NotificationBytes+1), false, true},
		"v32_tampered_result_digest_only":       {tamperEvidencePayload(v32, "result_sha256", strings.Repeat("b", 64)), false, true},
		"v32_tampered_result_bytes_only":        {tamperEvidencePayload(v32, "result_bytes", notification.ResultBytes+1), false, true},
		"v32_partial_only_notification_digest":  {onlyIdentityField(v32, "notification_sha256"), false, true},
		"v32_partial_only_notification_bytes":   {onlyIdentityField(v32, "notification_bytes"), false, true},
		"v32_partial_only_result_digest":        {onlyIdentityField(v32, "result_sha256"), false, true},
		"v32_partial_only_result_bytes":         {onlyIdentityField(v32, "result_bytes"), false, true},
		"legacy_all_legacy_fields_match":        {legacy, true, false},
		"legacy_tampered_result_bytes_only":     {tamperEvidencePayload(legacy, "result_bytes", notification.ContentBytes+1), false, true},
		"legacy_tampered_content_digest_only":   {tamperEvidencePayload(legacy, "content_sha256", strings.Repeat("c", 64)), false, true},
	}
	for name, scenario := range cases {
		t.Run(name, func(t *testing.T) {
			history := newHistoryReader(&jobNotificationHistoryRecorder{messages: []slackapi.Message{makeMessage(scenario.payload)}}, "BOT", 0, nil, false)
			got, found, err := NewJobNotificationPublisher(nil, history).Reconcile(t.Context(), notification)
			if found != scenario.wantFound || (err != nil) != scenario.wantError || (scenario.wantFound && got == "") {
				t.Fatalf("reconcile = %q, found=%v, err=%v; want found=%v err=%v", got, found, err, scenario.wantFound, scenario.wantError)
			}
		})
	}
}

// TestJobNotificationReconcileRejectsNegativeNotificationBytes proves the
// CR12 correction reaches the reconciliation gate too: Reconcile validates
// through the same validateJobNotification as Publish, so a row declaring a
// malformed v32 identity (empty digest with negative notification bytes) is
// rejected in both delivery modes before any history lookup instead of being
// reconciled as legacy.
func TestJobNotificationReconcileRejectsNegativeNotificationBytes(t *testing.T) {
	markdown := "OpenCode job `job-1` completed.\n\nsafe result"
	fileMarkdown := "OpenCode job `job-1` completed. The complete result was attached."
	history := newHistoryReader(&jobNotificationHistoryRecorder{}, "BOT", 0, nil, false)
	for _, mode := range []domain.JobResultDeliveryMode{domain.JobResultDeliveryMarkdown, domain.JobResultDeliveryFile} {
		for _, negative := range []int64{-1, -5} {
			t.Run(fmt.Sprintf("%s_bytes=%d", mode, negative), func(t *testing.T) {
				var notification domain.ExternalAgentJobNotification
				if mode == domain.JobResultDeliveryFile {
					notification = preV32DeliveryNotification(mode, fileMarkdown, "file bytes", "job-1-delivery.result")
				} else {
					// A legacy-consistent markdown row would reconcile as
					// legacy when the negative bytes are ignored; the hardened
					// gate must reject it before any history lookup.
					notification = domain.ExternalAgentJobNotification{
						JobID: "job-1", StatusRevision: 1, Kind: domain.JobNotificationTerminal,
						CanonicalMarkdown: markdown, NotificationSHA256: domain.NotificationIdentitySHA256(markdown),
						NotificationBytes: int64(len([]byte(markdown))), RendererVersion: domain.JobNotificationRenderer,
						Target: domain.ReplyTarget{ChannelID: "D12345678"}, ConversationKey: "slack:T12345678:dm:D12345678",
						DeliveryMode: mode, PolicyVersion: domain.JobDeliveryPolicyV1,
						UploadState: domain.JobResultUploadNotApplicable, MaxMarkdownParts: 6,
					}
				}
				notification.NotificationSHA256 = ""
				notification.NotificationBytes = negative
				_, found, err := NewJobNotificationPublisher(nil, history).Reconcile(t.Context(), notification)
				if err == nil || found {
					t.Fatalf("malformed v32 identity found=%v err=%v", found, err)
				}
			})
		}
	}
}

func markdownTestNotification(t *testing.T, wantParts int) domain.ExternalAgentJobNotification {
	t.Helper()
	text := strings.Repeat("界", domain.SlackMarkdownChunkRunes*(wantParts-1)-20)
	job := domain.ExternalAgentJob{ID: fmt.Sprintf("job-%d", wantParts), Status: domain.JobCompleted, StatusRevision: 3, Actor: "U12345678", ConversationKey: "slack:T12345678:dm:D12345678"}
	digest := contentSHA256(text)
	notification, err := domain.NewExternalAgentJobDelivery(job, domain.ExternalAgentInvocationResult{
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
	return slackapi.Message{
		User: "BOT", Timestamp: timestamp,
		Metadata: slackapi.SlackMetadata{EventType: jobNotificationMetadataEventType, EventPayload: map[string]any{
			"job_id": notification.JobID, "status_revision": notification.StatusRevision, "kind": notification.Kind,
			"renderer_version": notification.RendererVersion, "notification_sha256": notification.NotificationSHA256,
			"notification_bytes": notification.NotificationBytes, "result_sha256": notification.ResultSHA256,
			"result_bytes": notification.ResultBytes,
			"part_sha256":  contentSHA256(part), "part_index": index, "part_count": count,
		}},
	}
}

func fileTestNotification(content string, state domain.JobResultUploadState, fileID string) domain.ExternalAgentJobNotification {
	digest := contentSHA256(content)
	return domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 3, Kind: domain.JobNotificationTerminal,
		Actor: "U12345678", ConversationKey: "slack:T12345678:dm:D12345678", HostResultText: content,
		CanonicalMarkdown: fmt.Sprintf("OpenCode job `job-1` completed. The complete result was attached as `opencode-job-1.md` (%d bytes, SHA-256 `%s`).", len(content), digest),
		ResultSHA256:      digest, ResultBytes: int64(len(content)), RendererVersion: domain.JobNotificationRenderer,
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
