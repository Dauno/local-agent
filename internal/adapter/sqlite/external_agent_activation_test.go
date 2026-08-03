package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestPublishedTerminalNotificationAtomicallyCreatesOneActivation(t *testing.T) {
	store, jobs, now := newActivationTestStore(t)
	job := activationTestJob("activation-atomic", now)
	terminalizeActivationTestJob(t, jobs, job, now)
	notification := claimActivationTestNotification(t, jobs, now.Add(2*time.Second), "publisher")

	publishedAt := now.Add(3 * time.Second)
	if err := jobs.MarkNotificationPublished(t.Context(), notification, "1710000000.000001", publishedAt); err != nil {
		t.Fatal(err)
	}
	activation, err := jobs.GetActivation(t.Context(), domain.ExternalAgentJobActivationID(job.ID, notification.StatusRevision, notification.Kind))
	if err != nil || activation == nil {
		t.Fatalf("activation = %#v, err = %v", activation, err)
	}
	if activation.TerminalStatus != domain.JobCompleted || activation.NotificationSHA256 != notification.ContentSHA256 ||
		activation.SlackMessageTS != "1710000000.000001" || !activation.PublishedAt.Equal(publishedAt) || activation.State != domain.ActivationPending {
		t.Fatalf("activation snapshot = %#v", activation)
	}
	var count int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM external_agent_job_activations WHERE job_id = ?`, job.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("activation count = %d, want 1", count)
	}
	if err := jobs.MarkNotificationPublished(t.Context(), notification, "1710000000.000002", now.Add(4*time.Second)); !errors.Is(err, ErrNotificationStateConflict) {
		t.Fatalf("duplicate publish error = %v", err)
	}
}

func TestPublishedNotificationRollsBackWhenActivationInsertFails(t *testing.T) {
	store, jobs, now := newActivationTestStore(t)
	job := activationTestJob("activation-rollback", now)
	terminalizeActivationTestJob(t, jobs, job, now)
	notification := claimActivationTestNotification(t, jobs, now.Add(2*time.Second), "publisher")
	if _, err := store.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_activation_insert
		BEFORE INSERT ON external_agent_job_activations
		BEGIN SELECT RAISE(ABORT, 'test activation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := jobs.MarkNotificationPublished(t.Context(), notification, "1710000000.000003", now.Add(3*time.Second)); err == nil {
		t.Fatal("publication succeeded after activation insert failure")
	}
	var state string
	var publishedAt int64
	if err := store.DB().QueryRowContext(t.Context(), `SELECT publish_state, published_at
		FROM external_agent_job_notifications WHERE job_id = ?`, job.ID).Scan(&state, &publishedAt); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.NotificationPublishing) || publishedAt != 0 {
		t.Fatalf("notification was not rolled back: state=%q published_at=%d", state, publishedAt)
	}
	var count int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM external_agent_job_activations WHERE job_id = ?`, job.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rolled-back activation count = %d", count)
	}
}

func TestMultipartNotificationActivatesOnce(t *testing.T) {
	store, jobs, now := newActivationTestStore(t)
	job := activationTestJob("activation-multipart", now)
	job.Mode = domain.JobDetached
	terminalizeActivationTestJobWithResult(t, jobs, job, now, &domain.AcpInvocationResult{
		Text: "multipart result", DeliveryMode: domain.JobResultDeliveryMarkdown,
		DeliveryPolicyVersion: domain.JobDeliveryPolicyV1, DeliveryMaxMarkdownParts: 4,
		DeliveryContentBytes: 16,
	})
	notification := claimActivationTestNotification(t, jobs, now.Add(2*time.Second), "publisher")
	if notification.MaxMarkdownParts != 4 {
		t.Fatalf("multipart notification parts = %d", notification.MaxMarkdownParts)
	}
	if err := jobs.MarkNotificationPublished(t.Context(), notification, "1710000000.000004", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM external_agent_job_activations WHERE job_id = ? AND status_revision = ? AND kind = ?`, job.ID, notification.StatusRevision, notification.Kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("multipart activation count = %d, want 1", count)
	}
}

func TestActivationClaimsUseConversationOrderAndCAS(t *testing.T) {
	_, jobs, now := newActivationTestStore(t)
	first := activationTestJob("activation-order-a", now)
	second := activationTestJob("activation-order-b", now)
	second.OriginalCallID = "activation-order-b-call"
	terminalizeActivationTestJob(t, jobs, first, now)
	terminalizeActivationTestJob(t, jobs, second, now.Add(time.Second))
	firstNotification := claimActivationTestNotification(t, jobs, now.Add(3*time.Second), "publisher-a")
	if err := jobs.MarkNotificationPublished(t.Context(), firstNotification, "1710000000.000005", now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	secondNotification := claimActivationTestNotification(t, jobs, now.Add(4*time.Second), "publisher-b")
	if err := jobs.MarkNotificationPublished(t.Context(), secondNotification, "1710000000.000006", now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}

	firstActivation, err := jobs.ClaimNextActivation(t.Context(), now.Add(6*time.Second), "activation-worker-1", time.Minute)
	if err != nil || firstActivation == nil || firstActivation.JobID != first.ID {
		t.Fatalf("first activation claim = %#v, err=%v", firstActivation, err)
	}
	if secondActivation, err := jobs.ClaimNextActivation(t.Context(), now.Add(6*time.Second), "activation-worker-2", time.Minute); err != nil || secondActivation != nil {
		t.Fatalf("later activation bypassed active earlier claim: %#v, err=%v", secondActivation, err)
	}
	wrongOwner := *firstActivation
	wrongOwner.LeaseOwner = "wrong-owner"
	if err := jobs.MarkActivationModelStarted(t.Context(), &wrongOwner, now.Add(7*time.Second)); !errors.Is(err, ErrActivationStateConflict) {
		t.Fatalf("wrong owner mutation error = %v", err)
	}
	if err := jobs.MarkActivationModelStarted(t.Context(), firstActivation, now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := jobs.PrepareActivationResponse(t.Context(), firstActivation, "first response", "", "intent-first", "corr-first", now.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := jobs.CompleteActivation(t.Context(), firstActivation, "1710000000.000007", now.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	secondActivation, err := jobs.ClaimNextActivation(t.Context(), now.Add(10*time.Second), "activation-worker-2", time.Minute)
	if err != nil || secondActivation == nil || secondActivation.JobID != second.ID {
		t.Fatalf("second activation claim = %#v, err=%v", secondActivation, err)
	}
}

func TestActivationReconciliationBindsActorTeamAndConversation(t *testing.T) {
	_, jobs, now := newActivationTestStore(t)
	job := activationTestJob("activation-binding", now)
	terminalizeActivationTestJob(t, jobs, job, now)
	notification := claimActivationTestNotification(t, jobs, now.Add(2*time.Second), "publisher")
	if err := jobs.MarkNotificationPublished(t.Context(), notification, "1710000000.000008", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	activationID := domain.ExternalAgentJobActivationID(job.ID, notification.StatusRevision, notification.Kind)
	for _, test := range []struct {
		name  string
		actor string
		team  string
		key   domain.ConversationKey
	}{
		{"actor", "wrong", job.TeamID, job.ConversationKey},
		{"team", job.Actor, "wrong", job.ConversationKey},
		{"conversation", job.Actor, job.TeamID, "slack:T12345678:dm:other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if activation, err := jobs.ReconcileActivation(t.Context(), activationID, test.actor, test.team, test.key, now.Add(4*time.Second), "reconciler", time.Minute); err == nil || activation != nil {
				t.Fatalf("unauthorized reconciliation = %#v, err=%v", activation, err)
			}
		})
	}
	claimed, err := jobs.ReconcileActivation(t.Context(), activationID, job.Actor, job.TeamID, job.ConversationKey, now.Add(4*time.Second), "reconciler", time.Minute)
	if err != nil || claimed == nil || claimed.State != domain.ActivationProcessing {
		t.Fatalf("authorized reconciliation = %#v, err=%v", claimed, err)
	}
}

func TestActivationRestartAfterModelStartedDoesNotReplay(t *testing.T) {
	_, jobs, now := newActivationTestStore(t)
	job := activationTestJob("activation-restart", now)
	terminalizeActivationTestJob(t, jobs, job, now)
	notification := claimActivationTestNotification(t, jobs, now.Add(2*time.Second), "publisher")
	if err := jobs.MarkNotificationPublished(t.Context(), notification, "1710000000.000009", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobs.ClaimNextActivation(t.Context(), now.Add(4*time.Second), "worker-before-crash", time.Second)
	if err != nil || claimed == nil || claimed.State != domain.ActivationProcessing {
		t.Fatalf("first claim = %#v, err=%v", claimed, err)
	}
	recoveredBeforeModel, err := jobs.ClaimNextActivation(t.Context(), now.Add(6*time.Second), "worker-after-crash", time.Minute)
	if err != nil || recoveredBeforeModel == nil || recoveredBeforeModel.State != domain.ActivationProcessing || recoveredBeforeModel.Attempt != 2 {
		t.Fatalf("pre-model restart claim = %#v, err=%v", recoveredBeforeModel, err)
	}
	if err := jobs.MarkActivationModelStarted(t.Context(), recoveredBeforeModel, now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	recoveredAfterModel, err := jobs.ClaimNextActivation(t.Context(), now.Add(68*time.Second), "worker-reconciler", time.Minute)
	if err != nil || recoveredAfterModel == nil || recoveredAfterModel.State != domain.ActivationModelStarted || recoveredAfterModel.Attempt != 3 {
		t.Fatalf("post-model restart claim = %#v, err=%v", recoveredAfterModel, err)
	}
	if err := jobs.MarkActivationCompletionUnknown(t.Context(), recoveredAfterModel, "final_event_missing", now.Add(69*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestActivationOutboxDoesNotWriteConversationMemory(t *testing.T) {
	store, jobs, now := newActivationTestStore(t)
	job := activationTestJob("activation-no-memory", now)
	terminalizeActivationTestJob(t, jobs, job, now)
	notification := claimActivationTestNotification(t, jobs, now.Add(2*time.Second), "publisher")
	if err := jobs.MarkNotificationPublished(t.Context(), notification, "1710000000.000010", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	var messages, memoryOutbox int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM memory_outbox`).Scan(&memoryOutbox); err != nil {
		t.Fatal(err)
	}
	if messages != 0 || memoryOutbox != 0 {
		t.Fatalf("activation changed memory tables: messages=%d outbox=%d", messages, memoryOutbox)
	}
}

func TestMessagesPersistExplicitProvenanceAtSQLiteBoundary(t *testing.T) {
	store, _, now := newActivationTestStore(t)
	metadata := domain.ConversationMetadata{
		Key: "slack:T12345678:dm:D12345678", TeamID: "T12345678", ChannelID: "D12345678",
		ChannelKind: domain.ChannelDM, LastTS: "1710000000.000011",
	}
	if err := store.AppendMessage(t.Context(), metadata, domain.Message{
		Role: domain.RoleUser, Source: domain.MessageSourceJobCompletion, Content: "completion envelope",
		CreatedAt: now, ExternalTS: "1710000000.000011",
	}, 10); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(t.Context(), metadata, domain.Message{
		Role: domain.RoleAssistant, Content: "assistant synthesis", CreatedAt: now.Add(time.Second),
		ExternalTS: "1710000000.000012",
	}, 10); err != nil {
		t.Fatal(err)
	}
	messages, err := store.RecentMessages(t.Context(), metadata.Key, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Source != domain.MessageSourceJobCompletion || messages[1].Source != domain.MessageSourceAssistant {
		t.Fatalf("persisted message provenance = %#v", messages)
	}
}

func newActivationTestStore(t *testing.T) (*Store, *ExternalAgentJobStore, time.Time) {
	t.Helper()
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "activations.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, NewExternalAgentJobStore(store), time.Now().UTC().Truncate(time.Nanosecond)
}

func activationTestJob(id string, now time.Time) domain.ExternalAgentJob {
	job := testExternalAgentJob(now)
	job.ID = id
	job.OriginalCallID = id + "-call"
	return job
}

func terminalizeActivationTestJob(t *testing.T, jobs *ExternalAgentJobStore, job domain.ExternalAgentJob, now time.Time) {
	terminalizeActivationTestJobWithResult(t, jobs, job, now, &domain.AcpInvocationResult{Text: "result"})
}

func terminalizeActivationTestJobWithResult(t *testing.T, jobs *ExternalAgentJobStore, job domain.ExternalAgentJob, now time.Time, result *domain.AcpInvocationResult) {
	t.Helper()
	if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create %s = %v, err=%v", job.ID, created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "job-worker-"+job.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim %s = %#v, err=%v", job.ID, claimed, err)
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, result, "", now.Add(time.Second)); err != nil {
		t.Fatalf("terminalize %s: %v", job.ID, err)
	}
}

func claimActivationTestNotification(t *testing.T, jobs *ExternalAgentJobStore, now time.Time, owner string) *domain.ExternalAgentJobNotification {
	t.Helper()
	notification, err := jobs.ClaimNextNotification(t.Context(), now, owner, time.Minute)
	if err != nil || notification == nil {
		t.Fatalf("notification claim = %#v, err=%v", notification, err)
	}
	return notification
}
