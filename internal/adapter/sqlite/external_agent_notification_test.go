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

func TestNotificationClaimCASConflictReturnsTypedError(t *testing.T) {
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
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobFailed, nil, "acp_process_exit", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `CREATE TRIGGER ignore_notification_claim_update
		BEFORE UPDATE OF publish_state ON external_agent_job_notifications
		WHEN NEW.publish_state = 'publishing'
		BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.ClaimNextNotification(t.Context(), now.Add(2*time.Second), "publisher-1", time.Minute); !errors.Is(err, ErrNotificationClaimConflict) {
		t.Fatalf("claim CAS error = %v", err)
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
		"result_identity_invalid",
		"result_artifact_missing",
		"result_artifact_owner_ref_mismatch",
		"result_artifact_bytes_mismatch",
		"result_artifact_digest_mismatch",
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

func TestNotificationHealthAndAdminInspectionAreContentFree(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	healthNow := base.Add(20 * time.Minute)
	createTerminal := func(id string) domain.ExternalAgentJob {
		job := testExternalAgentJob(base)
		job.ID = id
		job.OriginalCallID = id + "-call"
		job.Task = "secret task text"
		created, _, createErr := jobs.CreateIfAbsent(t.Context(), job)
		if createErr != nil || !created {
			t.Fatalf("create %s = %v, err=%v", id, created, createErr)
		}
		claimed, claimErr := jobs.ClaimNext(t.Context(), base, "worker-"+id, time.Minute)
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		if transitionErr := jobs.Transition(t.Context(), id, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &domain.AcpInvocationResult{Text: "secret result text"}, "", base.Add(time.Second)); transitionErr != nil {
			t.Fatal(transitionErr)
		}
		return job
	}

	createTerminal("job_health_pending")
	createTerminal("job_health_publishing")
	createTerminal("job_health_permanent")
	createTerminal("job_health_overdue")
	createTerminal("job_health_published")
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE external_agent_job_notifications
		SET publish_state = ?, lease_expiry = ?, next_attempt_at = ? WHERE job_id = ?`,
		domain.NotificationPublishing, healthNow.Add(-time.Minute).UnixNano(), healthNow.UnixNano(), "job_health_publishing"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE external_agent_job_notifications
		SET publish_state = ?, lease_expiry = 0, next_attempt_at = ?, last_error_code = ? WHERE job_id = ?`,
		domain.NotificationUnknown, healthNow.Add(-6*time.Minute).UnixNano(), "result_delivery_failed", "job_health_permanent"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE external_agent_job_notifications
		SET publish_state = ?, lease_expiry = 0, next_attempt_at = ?, last_error_code = '' WHERE job_id = ?`,
		domain.NotificationUnknown, healthNow.Add(-6*time.Minute).UnixNano(), "job_health_overdue"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE external_agent_job_notifications
		SET next_attempt_at = ? WHERE job_id = ?`, healthNow.UnixNano(), "job_health_pending"); err != nil {
		t.Fatal(err)
	}
	published, err := jobs.ClaimNextNotification(t.Context(), base.Add(2*time.Second), "publisher", time.Minute)
	if err != nil || published == nil || published.JobID != "job_health_published" {
		t.Fatalf("published claim = %#v, err=%v", published, err)
	}
	if err := jobs.MarkNotificationPublished(t.Context(), published, "1710000000.000001", base.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}

	health, err := jobs.NotificationHealth(t.Context(), healthNow, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if health.Pending != 1 || health.Publishing != 1 || health.Unknown != 2 || health.Published != 1 || health.PermanentFailures != 1 || health.Stuck != 2 {
		t.Fatalf("health = %#v", health)
	}

	if _, err := store.DB().ExecContext(t.Context(), `UPDATE external_agent_job_notifications
		SET delivery_mode = 'file', upload_state = 'completed', slack_file_id = 'FSECRET', last_error_code = 'raw provider body', lease_owner = 'worker-secret', lease_expiry = ?
		WHERE job_id = ?`, healthNow.Add(time.Minute).UnixNano(), "job_health_pending"); err != nil {
		t.Fatal(err)
	}
	inspection, err := jobs.InspectJob(t.Context(), "job_health_pending")
	if err != nil || inspection == nil || len(inspection.Deliveries) != 1 {
		t.Fatalf("inspection = %#v, err=%v", inspection, err)
	}
	if inspection.Deliveries[0].DeliveryMode != domain.JobResultDeliveryFile || !inspection.Deliveries[0].SlackFileIDPresent || inspection.Deliveries[0].UploadState != domain.JobResultUploadCompleted || inspection.Deliveries[0].LeaseOwner != "worker-secret" || !inspection.Deliveries[0].LeaseOwnerPresent || !inspection.Deliveries[0].LeaseExpiry.Equal(healthNow.Add(time.Minute)) {
		t.Fatalf("file inspection = %#v", inspection.Deliveries[0])
	}
	if inspection.Deliveries[0].LastErrorCode != "notification_publish_ambiguous" {
		t.Fatalf("raw error was not bounded: %#v", inspection.Deliveries[0])
	}
	if inspection.Status != domain.JobCompleted || inspection.FinishedAt.IsZero() {
		t.Fatalf("job inspection = %#v", inspection)
	}
	if inspection, err := jobs.InspectJob(t.Context(), "does-not-exist"); err != nil || inspection != nil {
		t.Fatalf("missing inspection = %#v, err=%v", inspection, err)
	}
}

func TestOpenReadOnlyDoesNotMigrateOrWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	store, err := Initialize(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenReadOnly(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	var version int
	if err := readOnly.DB().QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("read-only schema version=%d err=%v", version, err)
	}
	if _, err := readOnly.DB().ExecContext(t.Context(), `UPDATE external_agent_jobs SET task = 'should fail'`); err == nil {
		t.Fatal("read-only database accepted a write")
	}
}

// assertTerminalNotificationRows checks the row-level activation disposition of
// every terminal notification row of the job: one row per status revision in
// order, matching terminal statuses, pending publication, and a
// root_activation_required that follows the job mode.
func assertTerminalNotificationRows(t *testing.T, store *Store, jobs *ExternalAgentJobStore, jobID string, mode domain.ExternalAgentJobMode, wantStatuses ...domain.ExternalAgentJobStatus) {
	t.Helper()
	rows := terminalNotificationRowsForJob(t, store, jobID)
	if len(rows) != len(wantStatuses) {
		t.Fatalf("job %s has %d terminal notification rows, want %d", jobID, len(rows), len(wantStatuses))
	}
	current, err := jobs.GetJob(t.Context(), jobID)
	if err != nil || current == nil {
		t.Fatalf("load job %s: %v", jobID, err)
	}
	seen := map[int]bool{}
	for index, row := range rows {
		if row.TerminalStatus != wantStatuses[index] {
			t.Fatalf("row %d terminal status = %s, want %s", index, row.TerminalStatus, wantStatuses[index])
		}
		if row.PublishState != domain.NotificationPending {
			t.Fatalf("row %d publish state = %s, want %s", index, row.PublishState, domain.NotificationPending)
		}
		if row.RootActivationRequired != (mode == domain.JobDetached) {
			t.Fatalf("row %d root activation = %t, mode = %s", index, row.RootActivationRequired, mode)
		}
		if seen[row.StatusRevision] {
			t.Fatalf("duplicate terminal notification revision %d", row.StatusRevision)
		}
		seen[row.StatusRevision] = true
	}
	if rows[len(rows)-1].StatusRevision != current.StatusRevision {
		t.Fatalf("last terminal row revision %d != job revision %d", rows[len(rows)-1].StatusRevision, current.StatusRevision)
	}
}

func TestTerminalPathNotificationRowDisposition(t *testing.T) {
	statuses := []domain.ExternalAgentJobStatus{
		domain.JobCompleted, domain.JobFailed, domain.JobCancelled,
		domain.JobCompletionUnknown, domain.JobAbandoned,
	}
	for _, mode := range []domain.ExternalAgentJobMode{domain.JobForeground, domain.JobDetached} {
		for _, status := range statuses {
			t.Run(fmt.Sprintf("transition/%s/%s", status, mode), func(t *testing.T) {
				store, jobs, now := newActivationTestStore(t)
				job := activationTestJob("notification-transition-"+string(status)+"-"+string(mode), now)
				job.Mode = mode
				terminalizeActivationTestJobByStatus(t, jobs, job, now, status)
				wantStatuses := []domain.ExternalAgentJobStatus{status}
				if status == domain.JobAbandoned {
					wantStatuses = []domain.ExternalAgentJobStatus{domain.JobCompletionUnknown, domain.JobAbandoned}
				}
				assertTerminalNotificationRows(t, store, jobs, job.ID, mode, wantStatuses...)
			})
		}
		t.Run(fmt.Sprintf("queued-cancellation/%s", mode), func(t *testing.T) {
			store, jobs, now := newActivationTestStore(t)
			job := activationTestJob("notification-cancellation-"+string(mode), now)
			job.Mode = mode
			if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
				t.Fatalf("create = %v, err = %v", created, err)
			}
			cancelled, err := jobs.RequestCancellation(t.Context(), job.ID, job.Actor)
			if err != nil || cancelled == nil || cancelled.Status != domain.JobCancelled {
				t.Fatalf("cancellation = %#v, err = %v", cancelled, err)
			}
			assertTerminalNotificationRows(t, store, jobs, job.ID, mode, domain.JobCancelled)
		})
		for _, outcome := range []struct {
			name        string
			status      domain.ExternalAgentJobStatus
			errorCode   string
			sideEffects bool
		}{
			{"failed", domain.JobFailed, "job_lease_lost", false},
			{"completion_unknown", domain.JobCompletionUnknown, "completion_unknown", true},
		} {
			t.Run(fmt.Sprintf("expired-recovery/%s/%s", outcome.name, mode), func(t *testing.T) {
				store, jobs, now := newActivationTestStore(t)
				job := activationTestJob("notification-recovered-"+outcome.name+"-"+string(mode), now)
				job.Mode = mode
				if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
					t.Fatalf("create = %v, err = %v", created, err)
				}
				claimed, err := jobs.ClaimNext(t.Context(), now, "job-worker-"+job.ID, time.Minute)
				if err != nil || claimed == nil {
					t.Fatalf("claim = %#v, err = %v", claimed, err)
				}
				if outcome.sideEffects {
					if err := jobs.MarkSideEffectsPossible(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt); err != nil {
						t.Fatal(err)
					}
				}
				if err := jobs.RecoverExpired(t.Context(), job.ID, claimed.Attempt, claimed.StatusRevision, now.Add(2*time.Minute), outcome.status, outcome.errorCode); err != nil {
					t.Fatal(err)
				}
				assertTerminalNotificationRows(t, store, jobs, job.ID, mode, outcome.status)
			})
		}
	}
}
