package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	metricsadapter "github.com/Dauno/slack-local-agent/internal/adapter/metrics"
	slackadapter "github.com/Dauno/slack-local-agent/internal/adapter/slack"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/secure"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

// wakeCompositionTimer is a workpoll.Timer whose channel never fires on its
// own. Every test in this file drives progress only through a real wake or
// the initial poll, never through a real clock.
type wakeCompositionTimer struct{ c chan time.Time }

func (t *wakeCompositionTimer) C() <-chan time.Time { return t.c }
func (t *wakeCompositionTimer) Stop() bool          { return true }

type wakeCompositionTimers struct{ created chan *wakeCompositionTimer }

func newWakeCompositionTimers() *wakeCompositionTimers {
	return &wakeCompositionTimers{created: make(chan *wakeCompositionTimer, 16)}
}

func (f *wakeCompositionTimers) New(time.Duration) workpoll.Timer {
	timer := &wakeCompositionTimer{c: make(chan time.Time, 1)}
	f.created <- timer
	return timer
}

func newWakeCompositionScheduler(t *testing.T) (*workpoll.Scheduler, *wakeCompositionTimers) {
	t.Helper()
	timers := newWakeCompositionTimers()
	scheduler, err := workpoll.New(time.Hour, workpoll.Options{NewTimer: timers.New})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler, timers
}

// waitCompositionPoll blocks on the fake timer's creation channel only: it
// carries no real duration of its own, so a hang here surfaces as the go
// test binary's own -timeout, never as a flaky arbitrary wait inside the
// test.
func waitCompositionPoll(t *testing.T, timers *wakeCompositionTimers) {
	t.Helper()
	<-timers.created
}

// newWakeCompositionSlackClient returns a *slackapi.Client backed by an
// httptest server that answers every Slack Web API call with a generic
// success envelope, so the production JobNotificationPublisher this test
// exercises through newExternalAgentJobService can actually post without a
// live workspace.
func newWakeCompositionSlackClient(t *testing.T) *slackapi.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "D12345678", "ts": "1.0"})
	}))
	t.Cleanup(server.Close)
	return slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.URL+"/"))
}

// newWakeCompositionInfrastructure builds the minimal-but-real
// *runtimeInfrastructure newExternalAgentJobService needs: a real temp
// SQLite store and a Slack publisher/history pair backed by a fake HTTP
// server, exactly the pieces composeRuntime itself wires.
func newWakeCompositionInfrastructure(t *testing.T, logger *logging.Logger) *runtimeInfrastructure {
	t.Helper()
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "wake-composition.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	client := newWakeCompositionSlackClient(t)
	return &runtimeInfrastructure{
		store: store, api: client, slackTimeout: 5 * time.Second,
		publisher:       slackadapter.NewPublisher(client, 5*time.Second, logger, false),
		history:         slackadapter.NewHistoryReader(client, "UBOTBOTBOT", 5*time.Second, logger, false),
		processRegistry: newInProcessRegistry(),
	}
}

// newWakeCompositionModels builds a runtimeModels with one durable-job ACP
// child wired to a fakeExternalRuntime, so newExternalAgentJobService takes
// its real, non-empty composition path instead of the len(children)==0
// early return.
func newWakeCompositionModels(t *testing.T, logger *logging.Logger) runtimeModels {
	t.Helper()
	workspace := t.TempDir()
	child := preparedAgentTool{
		definition:   agentdef.AgentDef{Name: "worker", Model: "codex/build", ExecutionMode: agentdef.ExecutionModeDurableJob},
		model:        &captureModel{text: "done"},
		cliResolved:  &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "codex", Type: agentdef.ProviderTypeAgentCLI}},
		projectRoots: map[string]string{"workspace": workspace}, registryRevision: "rev-1",
	}
	models := newRuntimeModels()
	models.preparedAgentTools = []preparedAgentTool{child}
	models.redactor = secure.NewRedactor()
	models.artifactStore = &recordingResultArtifacts{}
	models.logger = logger
	models.metrics = metricsadapter.NewRecorder()
	return models
}

func wakeCompositionJobRequest() domain.ExternalAgentJobRequest {
	return domain.ExternalAgentJobRequest{
		Provider: "codex", Profile: "codex/build", PrimaryProject: "workspace", RegistryRevision: "rev-1",
		Task: "task", Mode: domain.JobDetached, WrapperCallID: "wrapper-1", OriginalCallID: "original-1",
		Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678", Timeout: time.Minute,
	}
}

// TestNewExternalAgentJobServiceWakesOwnSchedulersOnly is FIND-122's repair
// for the external-agent job class: it drives the actual production
// composition function, newExternalAgentJobService, instead of a hand-built
// stand-in for its wiring. A regression that swaps NotificationWake for
// JobWake inside newExternalAgentJobService (external_agent_jobs.go) makes
// this test hang until the go test binary's own -timeout, because the
// notifications scheduler would then advance only through its own recovery
// timer, which this test never fires.
func TestNewExternalAgentJobServiceWakesOwnSchedulersOnly(t *testing.T) {
	logger := logging.New(io.Discard, "error", secure.NewRedactor())
	models := newWakeCompositionModels(t, logger)
	infra := newWakeCompositionInfrastructure(t, logger)
	cfg := config.Default()

	jobsScheduler, jobsTimers := newWakeCompositionScheduler(t)
	notificationsScheduler, notificationsTimers := newWakeCompositionScheduler(t)
	activationsScheduler, _ := newWakeCompositionScheduler(t)
	schedules := externalAgentSchedules{jobs: jobsScheduler, notifications: notificationsScheduler, activations: activationsScheduler}

	service, notificationWorker, err := newExternalAgentJobService(cfg, models, infra, schedules)
	if err != nil {
		t.Fatal(err)
	}
	if service == nil || notificationWorker == nil {
		t.Fatalf("newExternalAgentJobService() = %v, %v, want non-nil composition", service, notificationWorker)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go service.Run(ctx)
	go notificationWorker.Run(ctx)
	waitCompositionPoll(t, jobsTimers)
	waitCompositionPoll(t, notificationsTimers)

	job, err := service.Start(t.Context(), wakeCompositionJobRequest())
	if err != nil {
		t.Fatal(err)
	}
	// The jobs scheduler's own producer (Start) wakes it directly.
	waitCompositionPoll(t, jobsTimers)

	// Completing the job transitions it to a terminal status, which wakes
	// only the notifications scheduler: this blocks until that wake
	// actually happens through newExternalAgentJobService's own
	// NotificationWake wiring, never through a real recovery timer.
	waitCompositionPoll(t, notificationsTimers)

	store := adaptersqlite.NewExternalAgentJobStore(infra.store)
	completed, err := store.GetJob(t.Context(), job.ID)
	if err != nil || completed == nil || completed.Status != domain.JobCompleted {
		t.Fatalf("job after notification-scheduler poll = %#v, err = %v", completed, err)
	}
}
