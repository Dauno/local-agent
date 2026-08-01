package sqlite

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestTerminalTransitionEnqueuesOneDurableNotification(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create = %v, err = %v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.AcpInvocationResult{Text: "safe summary"}, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	claimedNotification, err := jobs.ClaimNextNotification(t.Context(), now.Add(2*time.Second), "publisher-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimedNotification == nil || claimedNotification.JobID != job.ID || claimedNotification.PublishState != domain.NotificationPublishing {
		t.Fatalf("notification = %#v", claimedNotification)
	}
	if claimedNotification.CanonicalMarkdown == "" || claimedNotification.ContentSHA256 == "" || claimedNotification.Target.ChannelID == "" {
		t.Fatalf("notification lacks canonical delivery data: %#v", claimedNotification)
	}
	if duplicate, err := jobs.ClaimNextNotification(t.Context(), now.Add(2*time.Second), "publisher-2", time.Minute); err != nil || duplicate != nil {
		t.Fatalf("duplicate claim = %#v, err = %v", duplicate, err)
	}
}

func TestNotificationRestartAndAmbiguousPublishAreReconciledBeforeRetry(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	if _, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobFailed, nil, "acp_process_exit", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	first, err := jobs.ClaimNextNotification(t.Context(), now.Add(2*time.Second), "publisher-1", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first claim = %#v, err = %v", first, err)
	}
	markStartedAt := time.Now().UTC()
	if err := jobs.MarkNotificationUnknown(t.Context(), first, "ambiguous"); err != nil {
		t.Fatal(err)
	}
	var nextAttemptNanos int64
	if err := store.db.QueryRowContext(t.Context(), `SELECT next_attempt_at FROM external_agent_job_notifications
		WHERE job_id = ? AND status_revision = ? AND kind = ?`, first.JobID, first.StatusRevision, first.Kind).Scan(&nextAttemptNanos); err != nil {
		t.Fatal(err)
	}
	nextAttemptAt := time.Unix(0, nextAttemptNanos)
	minimumDelay := notificationRetryDelay(first.Attempts, -1)
	if nextAttemptAt.Before(markStartedAt.Add(minimumDelay)) {
		t.Fatalf("next attempt was not delayed: %v", nextAttemptAt)
	}
	if early, err := jobs.ClaimNextNotification(t.Context(), nextAttemptAt.Add(-time.Nanosecond), "publisher-2", time.Minute); err != nil || early != nil {
		t.Fatalf("notification claimed before next attempt: %#v, err = %v", early, err)
	}
	recovered, err := jobs.ClaimNextNotification(t.Context(), nextAttemptAt, "publisher-2", time.Minute)
	if err != nil || recovered == nil || !recovered.NeedsReconciliation {
		t.Fatalf("recovered claim = %#v, err = %v", recovered, err)
	}
	if recovered.Attempts != 2 {
		t.Fatalf("recovered attempts = %d, want 2", recovered.Attempts)
	}
	if err := jobs.MarkNotificationPublished(t.Context(), recovered, "1710000000.000001", now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if again, err := jobs.ClaimNextNotification(t.Context(), now.Add(5*time.Second), "publisher-3", time.Minute); err != nil || again != nil {
		t.Fatalf("published notification was claimable: %#v, err = %v", again, err)
	}
	if err := jobs.MarkNotificationPublished(t.Context(), recovered, "1710000000.000002", now.Add(5*time.Second)); !errors.Is(err, ErrNotificationStateConflict) {
		t.Fatalf("duplicate publish err = %v", err)
	}
}

func TestNotificationRetryDelayGrowsMonotonically(t *testing.T) {
	for attempt := 2; attempt <= 12; attempt++ {
		previousMax := notificationRetryDelay(attempt-1, 1)
		currentMin := notificationRetryDelay(attempt, -1)
		if currentMin < previousMax {
			t.Fatalf("attempt %d minimum delay %v is below previous maximum %v", attempt, currentMin, previousMax)
		}
	}
}

func TestNotificationRetryDelayCapsAtSixtySeconds(t *testing.T) {
	for _, attempt := range []int{7, 8, 100} {
		for _, jitter := range []float64{-1, 0, 1} {
			if got := notificationRetryDelay(attempt, jitter); got != notificationRetryMaxDelay {
				t.Fatalf("attempt %d jitter %v delay = %v, want %v", attempt, jitter, got, notificationRetryMaxDelay)
			}
		}
	}
}

func TestNotificationRetryDelayJitterIsBounded(t *testing.T) {
	base := notificationRetryDelay(4, 0)
	minimum := time.Duration(float64(base) * (1 - notificationRetryJitter))
	maximum := time.Duration(float64(base) * (1 + notificationRetryJitter))
	for _, jitter := range []float64{-2, -1, -0.5, 0, 0.5, 1, 2} {
		got := notificationRetryDelay(4, jitter)
		if got < minimum || got > maximum {
			t.Fatalf("jitter %v delay = %v, want within [%v, %v]", jitter, got, minimum, maximum)
		}
	}
	if got := notificationRetryDelay(4, -1); got != minimum {
		t.Fatalf("minimum jitter delay = %v, want %v", got, minimum)
	}
	if got := notificationRetryDelay(4, 1); got != maximum {
		t.Fatalf("maximum jitter delay = %v, want %v", got, maximum)
	}
}

func TestPermanentNotificationErrorsAreNotRescheduledOrRetried(t *testing.T) {
	for _, code := range []string{
		"result_artifact_invalid",
		"result_delivery_failed",
		"result_destination_mismatch",
		"notification_delivery_invalid",
	} {
		t.Run(code, func(t *testing.T) {
			store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			jobs := NewExternalAgentJobStore(store)
			now := time.Now().UTC()
			job := testExternalAgentJob(now)
			if _, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil {
				t.Fatal(err)
			}
			claimed, err := jobs.ClaimNext(t.Context(), now, "worker-1", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobFailed, nil, "acp_process_exit", now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			notification, err := jobs.ClaimNextNotification(t.Context(), now.Add(2*time.Second), "publisher-1", time.Minute)
			if err != nil || notification == nil {
				t.Fatalf("claim = %#v, err = %v", notification, err)
			}
			var scheduledBefore int64
			if err := store.db.QueryRowContext(t.Context(), `SELECT next_attempt_at FROM external_agent_job_notifications
				WHERE job_id = ? AND status_revision = ? AND kind = ?`, notification.JobID, notification.StatusRevision, notification.Kind).Scan(&scheduledBefore); err != nil {
				t.Fatal(err)
			}
			if err := jobs.MarkNotificationUnknown(t.Context(), notification, code); err != nil {
				t.Fatal(err)
			}
			var scheduledAfter int64
			if err := store.db.QueryRowContext(t.Context(), `SELECT next_attempt_at FROM external_agent_job_notifications
				WHERE job_id = ? AND status_revision = ? AND kind = ?`, notification.JobID, notification.StatusRevision, notification.Kind).Scan(&scheduledAfter); err != nil {
				t.Fatal(err)
			}
			if scheduledAfter != scheduledBefore {
				t.Fatalf("next attempt changed from %d to %d", scheduledBefore, scheduledAfter)
			}
			diagnostic, err := jobs.ClaimNextNotification(t.Context(), now.Add(24*time.Hour), "publisher-2", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if diagnostic == nil || diagnostic.Kind != domain.JobNotificationFailure {
				t.Fatalf("claim after permanent error = %#v", diagnostic)
			}
			if retried, err := jobs.ClaimNextNotification(t.Context(), now.Add(24*time.Hour), "publisher-3", time.Minute); err != nil || retried != nil {
				t.Fatalf("permanent notification retried: %#v, err = %v", retried, err)
			}
		})
	}
}

func TestTerminalTransitionRollsBackWhenNotificationCannotBeBuilt(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	job.ConversationKey = "not-a-slack-key"
	if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create = %v, err = %v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.AcpInvocationResult{Text: "done"}, "", now.Add(time.Second)); err == nil {
		t.Fatal("terminal transition succeeded without a valid notification target")
	}
	current, err := jobs.GetJob(t.Context(), job.ID)
	if err != nil || current == nil || current.Status != domain.JobRunning || current.StatusRevision != claimed.StatusRevision {
		t.Fatalf("transition was not rolled back: job=%#v err=%v", current, err)
	}
}

func TestLegacyNotificationCannotBeConvertedIntoNewDelivery(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	if _, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.AcpInvocationResult{Text: "done"}, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = store.db.ExecContext(t.Context(), `UPDATE external_agent_job_notifications SET
		policy_version = 'delivery_v1', content_sha256 = ?, result_bytes = 1,
		delivery_mode = 'markdown', max_markdown_parts = 1, upload_state = 'not_applicable'
		WHERE job_id = ?`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", job.ID)
	if err == nil {
		t.Fatal("legacy notification was converted into a new delivery")
	}
}

func TestPermanentDeliveryFailureEnqueuesHostDiagnostic(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	job.Mode = domain.JobDetached
	if _, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	result := &domain.AcpInvocationResult{
		DeliveryMode: domain.JobResultDeliveryFile, DeliveryPolicyVersion: domain.JobDeliveryPolicyV1,
		DeliveryArtifactRef: "job_1-delivery.result", DeliveryContentSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeliveryContentBytes: 4, DeliveryMaxMarkdownParts: 6, ArtifactRef: "job_1-delivery.result", ResultSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ResultBytes: 4,
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, result, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	delivery, err := jobs.ClaimNextNotification(t.Context(), now.Add(2*time.Second), "publisher-1", time.Minute)
	if err != nil || delivery == nil {
		t.Fatalf("delivery = %#v, err = %v", delivery, err)
	}
	if err := jobs.MarkNotificationFileID(t.Context(), delivery, "F123", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := jobs.MarkNotificationUnknown(t.Context(), delivery, "result_file_upload_unknown"); err != nil {
		t.Fatal(err)
	}
	referenced, err := jobs.IsArtifactReferenced(t.Context(), result.DeliveryArtifactRef)
	if err != nil || referenced {
		t.Fatalf("permanently failed artifact referenced=%v err=%v", referenced, err)
	}
	diagnostic, err := jobs.ClaimNextNotification(t.Context(), now.Add(3*time.Second), "publisher-2", time.Minute)
	if err != nil || diagnostic == nil {
		t.Fatalf("diagnostic = %#v, err = %v", diagnostic, err)
	}
	if diagnostic.Kind != domain.JobNotificationFailure || diagnostic.PolicyVersion != "legacy_v1" || diagnostic.ArtifactRef != "" || diagnostic.CanonicalMarkdown == "" {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
	if err := jobs.MarkNotificationPublished(t.Context(), diagnostic, "1710000000.000001", now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if original, err := jobs.ClaimNextNotification(t.Context(), now.Add(24*time.Hour), "publisher-3", time.Minute); err != nil || original != nil {
		t.Fatalf("terminal file delivery was reclaimed: %#v, err = %v", original, err)
	}
}

func TestFileNotificationCannotPublishWithoutUploadEvidence(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	job.Mode = domain.JobDetached
	if _, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	content := "file result"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	result := &domain.AcpInvocationResult{
		Text: "", DeliveryMode: domain.JobResultDeliveryFile, DeliveryPolicyVersion: domain.JobDeliveryPolicyV1,
		DeliveryArtifactRef: job.ID + "-delivery.result", DeliveryContentSHA256: digest, DeliveryContentBytes: int64(len(content)),
		ArtifactRef: job.ID + "-delivery.result", ResultSHA256: digest, ResultBytes: int64(len(content)), DeliveryMaxMarkdownParts: 6,
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, result, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	delivery, err := jobs.ClaimNextNotification(t.Context(), now.Add(2*time.Second), "publisher-1", time.Minute)
	if err != nil || delivery == nil {
		t.Fatalf("delivery = %#v, err = %v", delivery, err)
	}
	if err := jobs.MarkNotificationPublished(t.Context(), delivery, "1710000000.000001", now.Add(3*time.Second)); !errors.Is(err, ErrNotificationStateConflict) {
		t.Fatalf("publish without evidence err = %v", err)
	}
	if err := jobs.MarkNotificationFileID(t.Context(), delivery, "F123", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := jobs.MarkNotificationFileID(t.Context(), delivery, "F999", now.Add(3*time.Second)); !errors.Is(err, ErrNotificationStateConflict) {
		t.Fatalf("file identity changed err = %v", err)
	}
	if err := jobs.MarkNotificationUploadState(t.Context(), delivery, domain.JobResultUploadCompleted, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := jobs.MarkNotificationPublished(t.Context(), delivery, "1710000000.000001", now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
}
