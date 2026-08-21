package app

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

// activationCompletionHandler durably completes whatever activation it is
// handed, using the real store, so the activation worker's poll under test
// observes a real durable state transition without depending on a real ADK
// turn.
type activationCompletionHandler struct {
	store port.ExternalAgentJobActivationStore
}

func (h activationCompletionHandler) HandleJobCompletion(ctx context.Context, activation domain.ExternalAgentJobActivation) error {
	return h.store.MarkActivationModelStarted(ctx, &activation, time.Now().UTC())
}

func (h activationCompletionHandler) ReconcileJobCompletion(ctx context.Context, activation domain.ExternalAgentJobActivation) error {
	return h.store.MarkActivationCompletionUnknown(ctx, &activation, "test-composition", time.Now().UTC())
}

var _ port.ExternalAgentJobCompletionHandler = activationCompletionHandler{}

// distinctWakeCompositionJobRequest varies the wrapper/original call IDs so
// each call produces a distinct RequestSHA256: Service.Start is idempotent
// on that digest, so two calls with wakeCompositionJobRequest's fixed IDs
// return the same already-completed job and never wake anything a second
// time.
func distinctWakeCompositionJobRequest(suffix string) domain.ExternalAgentJobRequest {
	request := wakeCompositionJobRequest()
	request.WrapperCallID = "wrapper-" + suffix
	request.OriginalCallID = "original-" + suffix
	return request
}

// seedActivation inserts one durable, claimable activation row directly,
// referencing a real job row for its foreign key, exactly like production's
// own conditional INSERT inside MarkNotificationPublished would when a
// completion binding is present. This fixture does not reconstruct the
// workstream/task admission that conditional INSERT requires, so the row is
// seeded directly instead.
func seedActivation(t *testing.T, store *adaptersqlite.Store, jobID string) string {
	t.Helper()
	activationID := jobID + "-activation-seed"
	now := time.Now().UTC().UnixNano()
	_, err := store.DB().ExecContext(t.Context(), `INSERT INTO external_agent_job_activations (
		job_id, status_revision, kind, activation_id, terminal_status, notification_sha256,
		actor, team_id, conversation_key, original_call_id, delivery_mode,
		slack_message_ts, published_at, state, next_attempt_at, created_at, updated_at)
		VALUES (?, 99, 'terminal', ?, 'completed', ?, 'U12345678', 'T12345678', 'slack:T12345678:dm:D12345678', 'original-1', 'markdown', '1.0', ?, 'pending', ?, ?, ?)`,
		jobID, activationID, strings.Repeat("a", 64), now, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	return activationID
}

// TestExternalAgentActivationCompositionWakesAndConsumesThroughItsOwnScheduler
// is FIND-122's repair for the root-activation class. It exercises two real
// production links: newExternalAgentJobService wires
// NotificationDependencies.ActivationWake to schedules.activations.Wake
// (external_agent_jobs.go), and newExternalAgentActivationWorker wires the
// activation worker's Scheduler to the same instance (also
// external_agent_jobs.go, called from composeRuntime). A regression that
// points either at the jobs scheduler instead makes this test hang until
// the go test binary's own -timeout: the notification publish would never
// wake the activations scheduler, or the activation worker would never
// consume through it.
func TestExternalAgentActivationCompositionWakesAndConsumesThroughItsOwnScheduler(t *testing.T) {
	logger := logging.New(io.Discard, "error", secure.NewRedactor())
	models := newWakeCompositionModels(t, logger)
	infra := newWakeCompositionInfrastructure(t, logger)
	cfg := config.Default()

	jobsScheduler, jobsTimers := newWakeCompositionScheduler(t)
	notificationsScheduler, notificationsTimers := newWakeCompositionScheduler(t)
	activationsScheduler, activationsTimers := newWakeCompositionScheduler(t)
	schedules := externalAgentSchedules{jobs: jobsScheduler, notifications: notificationsScheduler, activations: activationsScheduler}

	service, notificationWorker, err := newExternalAgentJobService(cfg, models, infra, schedules)
	if err != nil {
		t.Fatal(err)
	}

	activationStore := adaptersqlite.NewExternalAgentJobStore(infra.store)
	activationWorker, err := newExternalAgentActivationWorker(activationStore, activationCompletionHandler{store: activationStore}, models.logger, models.metrics, schedules)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go service.Run(ctx)
	go notificationWorker.Run(ctx)
	go activationWorker.Run(ctx)
	waitCompositionPoll(t, jobsTimers)
	waitCompositionPoll(t, notificationsTimers)
	waitCompositionPoll(t, activationsTimers)

	// A first job supplies a real foreign-key target for the seeded
	// activation row below, and its own terminal notification publish
	// (which fires a real wakeActivations() call, finding nothing to
	// claim) is drained here so it cannot race the seeded row inserted
	// afterward.
	seedJob, err := service.Start(t.Context(), distinctWakeCompositionJobRequest("seed"))
	if err != nil {
		t.Fatal(err)
	}
	waitCompositionPoll(t, jobsTimers)
	waitCompositionPoll(t, notificationsTimers)
	waitCompositionPoll(t, activationsTimers)
	activationID := seedActivation(t, infra.store, seedJob.ID)

	// The second job's terminal notification publish is the real wake
	// under test: production's NotificationWorker.wakeActivations, not
	// this test, drives the activations scheduler from here on.
	job, err := service.Start(t.Context(), distinctWakeCompositionJobRequest("primary"))
	if err != nil {
		t.Fatal(err)
	}
	waitCompositionPoll(t, jobsTimers)
	waitCompositionPoll(t, notificationsTimers)
	current, err := activationStore.GetJob(t.Context(), job.ID)
	if err != nil || current == nil || current.Status != domain.JobCompleted {
		t.Fatalf("job after notification-scheduler poll = %#v, err = %v", current, err)
	}

	// This is the link under test: the notification publish above must
	// have called wakeActivations() through schedules.activations.Wake,
	// driving one more full poll cycle on the activations scheduler,
	// which must consume the seeded row through externalSchedules.activations.
	waitCompositionPoll(t, activationsTimers)

	activation, err := activationStore.GetActivation(t.Context(), activationID)
	if err != nil || activation == nil || activation.State != domain.ActivationCompletionUnknown {
		t.Fatalf("seeded activation after wake-driven poll = %#v, err = %v", activation, err)
	}
}
