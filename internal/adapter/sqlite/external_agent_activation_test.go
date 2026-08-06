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
	if activation.TerminalStatus != domain.JobCompleted || activation.NotificationSHA256 != notification.NotificationSHA256 ||
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

func TestPublicationActivationCountByModeAndPath(t *testing.T) {
	for _, tt := range []struct {
		name       string
		mode       domain.ExternalAgentJobMode
		reconciled bool
		want       int
	}{
		{"detached fresh publication", domain.JobDetached, false, 1},
		{"detached reconciled publication", domain.JobDetached, true, 1},
		{"foreground fresh publication", domain.JobForeground, false, 0},
		{"foreground reconciled publication", domain.JobForeground, true, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, jobs, now := newActivationTestStore(t)
			job := activationTestJob("activation-"+tt.name, now)
			job.Mode = tt.mode
			terminalizeActivationTestJob(t, jobs, job, now)
			notification := claimActivationTestNotification(t, jobs, now.Add(2*time.Second), "publisher")
			if tt.reconciled {
				if err := jobs.MarkNotificationUnknown(t.Context(), notification, "ambiguous"); err != nil {
					t.Fatal(err)
				}
				var nextAttemptNanos int64
				if err := store.DB().QueryRowContext(t.Context(), `SELECT next_attempt_at FROM external_agent_job_notifications WHERE job_id = ? AND status_revision = ? AND kind = ?`,
					notification.JobID, notification.StatusRevision, notification.Kind).Scan(&nextAttemptNanos); err != nil {
					t.Fatal(err)
				}
				var err error
				notification, err = jobs.ClaimNextNotification(t.Context(), time.Unix(0, nextAttemptNanos), "reconciler", time.Minute)
				if err != nil || notification == nil || !notification.NeedsReconciliation {
					t.Fatalf("reconciled notification claim = %#v, err=%v", notification, err)
				}
			}
			if err := jobs.MarkNotificationPublished(t.Context(), notification, "1710000000.000001", now.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM external_agent_job_activations WHERE job_id = ?`, job.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != tt.want {
				t.Fatalf("%s activation count = %d, want %d", tt.name, count, tt.want)
			}
			var state string
			if err := store.DB().QueryRowContext(t.Context(), `SELECT publish_state FROM external_agent_job_notifications WHERE job_id = ? AND status_revision = ? AND kind = ?`,
				job.ID, notification.StatusRevision, notification.Kind).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != string(domain.NotificationPublished) {
				t.Fatalf("%s notification state = %q, want published", tt.name, state)
			}
		})
	}
}

func TestPublicationActivationRequiresBothRouteAndDetachedMode(t *testing.T) {
	for _, tt := range []struct {
		name  string
		mode  domain.ExternalAgentJobMode
		route int
		want  int
	}{
		{"detached with route", domain.JobDetached, 1, 1},
		{"detached without route", domain.JobDetached, 0, 0},
		{"foreground with route", domain.JobForeground, 1, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, jobs, now := newActivationTestStore(t)
			job := activationTestJob("activation-dual-"+tt.name, now)
			job.Mode = tt.mode
			terminalizeActivationTestJob(t, jobs, job, now)
			markdown := "OpenCode job `" + job.ID + "` completed.\n\nresult"
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(markdown)))
			// The persisted route is immutable, so the matrix is exercised with
			// explicit storage rows rather than constructor fixtures.
			if _, err := store.DB().ExecContext(t.Context(), `DELETE FROM external_agent_job_notifications WHERE job_id = ?`, job.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().ExecContext(t.Context(), `INSERT INTO external_agent_job_notifications (
				job_id, status_revision, kind, terminal_status, canonical_markdown, content_sha256,
				renderer_version, channel_id, publish_state, next_attempt_at, created_at, updated_at,
				delivery_mode, policy_version, max_markdown_parts, upload_state,
				root_activation_required, notification_sha256, notification_bytes)
				VALUES (?, 2, 'terminal', 'completed', ?, ?, 'markdown_v1', 'D12345678', 'pending', ?, 1, 1,
					'markdown', 'legacy_v1', 1, 'not_applicable', ?, ?, ?)`,
				job.ID, markdown, digest, now.UnixNano(), tt.route, digest, int64(len([]byte(markdown)))); err != nil {
				t.Fatal(err)
			}
			notification, err := jobs.ClaimNextNotification(t.Context(), now, "publisher", time.Minute)
			if err != nil || notification == nil {
				t.Fatalf("notification claim = %#v, err=%v", notification, err)
			}
			if err := jobs.MarkNotificationPublished(t.Context(), notification, "1710000000.000001", now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM external_agent_job_activations WHERE job_id = ?`, job.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != tt.want {
				t.Fatalf("%s activation count = %d, want %d", tt.name, count, tt.want)
			}
			if count == 1 {
				activation, err := jobs.GetActivation(t.Context(), domain.ExternalAgentJobActivationID(job.ID, notification.StatusRevision, notification.Kind))
				if err != nil || activation == nil {
					t.Fatalf("activation = %#v, err = %v", activation, err)
				}
				if activation.NotificationSHA256 != digest {
					t.Fatalf("activation notification identity = %q, want %q", activation.NotificationSHA256, digest)
				}
			}
		})
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
	job.Mode = domain.JobDetached
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

// terminalizeActivationTestJobByStatus drives a claimed job to the requested
// terminal status through the legal store state machine (P0-05 normal
// transition path). JobCancelled and JobAbandoned need an intermediate state,
// and JobAbandoned emits one terminal notification per revision.
func terminalizeActivationTestJobByStatus(t *testing.T, jobs *ExternalAgentJobStore, job domain.ExternalAgentJob, now time.Time, status domain.ExternalAgentJobStatus) {
	t.Helper()
	if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create %s = %v, err=%v", job.ID, created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "job-worker-"+job.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim %s = %#v, err=%v", job.ID, claimed, err)
	}
	owner, attempt := claimed.LeaseOwner, claimed.Attempt
	switch status {
	case domain.JobCompleted:
		transitionTerminalTestJob(t, jobs, job.ID, owner, attempt, domain.JobCompleted, &domain.AcpInvocationResult{Text: "result"}, "", now.Add(time.Second))
	case domain.JobFailed:
		transitionTerminalTestJob(t, jobs, job.ID, owner, attempt, domain.JobFailed, nil, "acp_process_exit", now.Add(time.Second))
	case domain.JobCancelled:
		transitionTerminalTestJob(t, jobs, job.ID, owner, attempt, domain.JobCancelRequested, nil, "", now.Add(time.Second))
		transitionTerminalTestJob(t, jobs, job.ID, owner, attempt, domain.JobCancelled, nil, "", now.Add(2*time.Second))
	case domain.JobCompletionUnknown:
		transitionTerminalTestJob(t, jobs, job.ID, owner, attempt, domain.JobCompletionUnknown, nil, "completion_unknown", now.Add(time.Second))
	case domain.JobAbandoned:
		transitionTerminalTestJob(t, jobs, job.ID, owner, attempt, domain.JobCompletionUnknown, nil, "completion_unknown", now.Add(time.Second))
		transitionTerminalTestJob(t, jobs, job.ID, owner, attempt, domain.JobAbandoned, nil, "", now.Add(2*time.Second))
	default:
		t.Fatalf("unsupported terminal test status %q", status)
	}
}

func transitionTerminalTestJob(t *testing.T, jobs *ExternalAgentJobStore, jobID, owner string, attempt int, next domain.ExternalAgentJobStatus, result *domain.AcpInvocationResult, errorCode string, now time.Time) {
	t.Helper()
	if err := jobs.Transition(t.Context(), jobID, owner, attempt, next, result, errorCode, now); err != nil {
		t.Fatalf("transition %s to %s: %v", jobID, next, err)
	}
}

// publishAllTerminalNotifications claims and publishes every terminal
// notification row of the job. When reconcile is set, each claim goes through
// the ambiguous-publish reconciliation path before publication.
func publishAllTerminalNotifications(t *testing.T, store *Store, jobs *ExternalAgentJobStore, jobID string, start time.Time, reconcile bool) []*domain.ExternalAgentJobNotification {
	t.Helper()
	var published []*domain.ExternalAgentJobNotification
	for index := 0; ; index++ {
		if index >= 8 {
			t.Fatal("runaway terminal notification loop")
		}
		notification, err := jobs.ClaimNextNotification(t.Context(), start.Add(time.Duration(index)*time.Second), "publisher", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if notification == nil || notification.JobID != jobID {
			break
		}
		if notification.Kind != domain.JobNotificationTerminal {
			t.Fatalf("unexpected notification kind %q", notification.Kind)
		}
		if reconcile {
			notification = reconcileClaimedNotification(t, store, jobs, notification)
		}
		if err := jobs.MarkNotificationPublished(t.Context(), notification, "1710000000.000001", start); err != nil {
			t.Fatal(err)
		}
		published = append(published, notification)
	}
	if len(published) == 0 {
		t.Fatalf("no terminal notification was published for %s", jobID)
	}
	return published
}

// reconcileClaimedNotification marks the claim ambiguous, waits out the retry
// delay, and reclaims it through the NeedsReconciliation path.
func reconcileClaimedNotification(t *testing.T, store *Store, jobs *ExternalAgentJobStore, notification *domain.ExternalAgentJobNotification) *domain.ExternalAgentJobNotification {
	t.Helper()
	if err := jobs.MarkNotificationUnknown(t.Context(), notification, "ambiguous"); err != nil {
		t.Fatal(err)
	}
	var nextAttemptNanos int64
	if err := store.DB().QueryRowContext(t.Context(), `SELECT next_attempt_at FROM external_agent_job_notifications
		WHERE job_id = ? AND status_revision = ? AND kind = ?`, notification.JobID, notification.StatusRevision, notification.Kind).Scan(&nextAttemptNanos); err != nil {
		t.Fatal(err)
	}
	recovered, err := jobs.ClaimNextNotification(t.Context(), time.Unix(0, nextAttemptNanos), notification.LeaseOwner, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || !recovered.NeedsReconciliation {
		t.Fatalf("reconciled notification claim = %#v, err=%v", recovered, err)
	}
	return recovered
}

type terminalNotificationRow struct {
	StatusRevision         int
	TerminalStatus         domain.ExternalAgentJobStatus
	RootActivationRequired bool
	PublishState           domain.NotificationPublishState
}

func terminalNotificationRowsForJob(t *testing.T, store *Store, jobID string) []terminalNotificationRow {
	t.Helper()
	rows, err := store.DB().QueryContext(t.Context(), `SELECT status_revision, terminal_status, root_activation_required, publish_state
		FROM external_agent_job_notifications WHERE job_id = ? AND kind = ?
		ORDER BY status_revision`, jobID, domain.JobNotificationTerminal)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []terminalNotificationRow
	for rows.Next() {
		var row terminalNotificationRow
		if err := rows.Scan(&row.StatusRevision, &row.TerminalStatus, &row.RootActivationRequired, &row.PublishState); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func activationCountForJob(t *testing.T, store *Store, jobID string) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM external_agent_job_activations WHERE job_id = ?`, jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func duplicateActivationRevisionKindCount(t *testing.T, store *Store, jobID string) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM (
		SELECT 1 FROM external_agent_job_activations WHERE job_id = ?
		GROUP BY status_revision, kind HAVING COUNT(*) > 1)`, jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestNormalTransitionTerminalActivationByModeAndStatus(t *testing.T) {
	for _, status := range []domain.ExternalAgentJobStatus{
		domain.JobCompleted, domain.JobFailed, domain.JobCancelled,
		domain.JobCompletionUnknown, domain.JobAbandoned,
	} {
		for _, mode := range []domain.ExternalAgentJobMode{domain.JobForeground, domain.JobDetached} {
			for _, reconciled := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/reconciled=%t", status, mode, reconciled), func(t *testing.T) {
					store, jobs, now := newActivationTestStore(t)
					job := activationTestJob("activation-normal-"+string(status)+"-"+string(mode)+fmt.Sprint(reconciled), now)
					job.Mode = mode
					terminalizeActivationTestJobByStatus(t, jobs, job, now, status)
					published := publishAllTerminalNotifications(t, store, jobs, job.ID, now.Add(3*time.Second), reconciled)
					rows := terminalNotificationRowsForJob(t, store, job.ID)
					if len(published) != len(rows) || len(rows) == 0 {
						t.Fatalf("published %d notifications for %d terminal rows", len(published), len(rows))
					}
					if len(rows) > 1 && status != domain.JobAbandoned {
						t.Fatalf("unexpected %d terminal rows for %s", len(rows), status)
					}
					wantActivations := 0
					if mode == domain.JobDetached {
						wantActivations = len(rows)
					}
					if got := activationCountForJob(t, store, job.ID); got != wantActivations {
						t.Fatalf("%s activation count = %d, want %d", status, got, wantActivations)
					}
					if duplicates := duplicateActivationRevisionKindCount(t, store, job.ID); duplicates != 0 {
						t.Fatalf("duplicate activation revision/kind rows = %d", duplicates)
					}
					for _, notification := range published {
						activation, err := jobs.GetActivation(t.Context(), domain.ExternalAgentJobActivationID(notification.JobID, notification.StatusRevision, notification.Kind))
						if err != nil {
							t.Fatal(err)
						}
						if mode == domain.JobForeground {
							if activation != nil {
								t.Fatalf("foreground %s created activation %#v", status, activation)
							}
							if notification.RootActivationRequired {
								t.Fatalf("foreground %s notification revision %d requires root activation", status, notification.StatusRevision)
							}
						} else {
							if activation == nil {
								t.Fatalf("detached %s lost activation for revision %d", status, notification.StatusRevision)
							}
							if activation.TerminalStatus != notification.TerminalStatus {
								t.Fatalf("activation terminal status = %s, want %s", activation.TerminalStatus, notification.TerminalStatus)
							}
						}
					}
				})
			}
		}
	}
}

func TestQueuedCancellationTerminalActivationByMode(t *testing.T) {
	for _, mode := range []domain.ExternalAgentJobMode{domain.JobForeground, domain.JobDetached} {
		t.Run(string(mode), func(t *testing.T) {
			store, jobs, now := newActivationTestStore(t)
			job := activationTestJob("activation-cancelled-"+string(mode), now)
			job.Mode = mode
			if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
				t.Fatalf("create = %v, err = %v", created, err)
			}
			cancelled, err := jobs.RequestCancellation(t.Context(), job.ID, job.Actor)
			if err != nil || cancelled == nil || cancelled.Status != domain.JobCancelled {
				t.Fatalf("cancellation = %#v, err = %v", cancelled, err)
			}
			published := publishAllTerminalNotifications(t, store, jobs, job.ID, now.Add(2*time.Second), false)
			if len(published) != 1 {
				t.Fatalf("published %d notifications, want 1", len(published))
			}
			want := 0
			if mode == domain.JobDetached {
				want = 1
			}
			if got := activationCountForJob(t, store, job.ID); got != want {
				t.Fatalf("queued-cancelled %s activation count = %d, want %d", mode, got, want)
			}
			activation, err := jobs.GetActivation(t.Context(), domain.ExternalAgentJobActivationID(job.ID, published[0].StatusRevision, published[0].Kind))
			if err != nil {
				t.Fatal(err)
			}
			if mode == domain.JobForeground && activation != nil {
				t.Fatalf("foreground queued cancellation created activation %#v", activation)
			}
			if mode == domain.JobDetached && (activation == nil || activation.TerminalStatus != domain.JobCancelled) {
				t.Fatalf("detached queued cancellation activation = %#v", activation)
			}
		})
	}
}

func TestExpiredRecoveryTerminalActivationByMode(t *testing.T) {
	for _, outcome := range []struct {
		name        string
		status      domain.ExternalAgentJobStatus
		errorCode   string
		sideEffects bool
	}{
		{"failed", domain.JobFailed, "job_lease_lost", false},
		{"completion_unknown", domain.JobCompletionUnknown, "completion_unknown", true},
	} {
		for _, mode := range []domain.ExternalAgentJobMode{domain.JobForeground, domain.JobDetached} {
			t.Run(fmt.Sprintf("%s/%s", outcome.name, mode), func(t *testing.T) {
				store, jobs, now := newActivationTestStore(t)
				job := activationTestJob("activation-recovered-"+outcome.name+"-"+string(mode), now)
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
				published := publishAllTerminalNotifications(t, store, jobs, job.ID, now.Add(2*time.Minute+time.Second), false)
				if len(published) != 1 {
					t.Fatalf("published %d notifications, want 1", len(published))
				}
				want := 0
				if mode == domain.JobDetached {
					want = 1
				}
				if got := activationCountForJob(t, store, job.ID); got != want {
					t.Fatalf("recovered %s activation count = %d, want %d", mode, got, want)
				}
				activation, err := jobs.GetActivation(t.Context(), domain.ExternalAgentJobActivationID(job.ID, published[0].StatusRevision, published[0].Kind))
				if err != nil {
					t.Fatal(err)
				}
				if mode == domain.JobForeground && activation != nil {
					t.Fatalf("foreground recovery created activation %#v", activation)
				}
				if mode == domain.JobDetached && (activation == nil || activation.TerminalStatus != outcome.status) {
					t.Fatalf("detached recovery activation = %#v", activation)
				}
			})
		}
	}
}
