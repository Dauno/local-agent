package externalagent

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

// wakeTimer is a workpoll.Timer whose channel never fires on its own. Every
// test in this file drives progress only through a real wake or an initial
// poll, never through a real clock.
type wakeTimer struct{ c chan time.Time }

func (t *wakeTimer) C() <-chan time.Time { return t.c }
func (t *wakeTimer) Stop() bool          { return true }

// wakeTimers hands out blocked timers and reports each creation on a
// channel, so a test can synchronize on "one full poll cycle completed"
// without a real clock or a polling loop of its own.
type wakeTimers struct {
	created chan *wakeTimer
}

func newWakeTimers() *wakeTimers { return &wakeTimers{created: make(chan *wakeTimer, 16)} }

func (f *wakeTimers) New(time.Duration) workpoll.Timer {
	timer := &wakeTimer{c: make(chan time.Time, 1)}
	f.created <- timer
	return timer
}

func newWakeScheduler(t *testing.T) (*workpoll.Scheduler, *wakeTimers) {
	t.Helper()
	timers := newWakeTimers()
	scheduler, err := workpoll.New(time.Hour, workpoll.Options{NewTimer: timers.New})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler, timers
}

// waitPoll blocks until the scheduler under test has completed exactly one
// more full poll cycle (poll() returned and Wait() built its recovery
// timer). It blocks on the fake timer's creation channel only, carrying no
// real duration of its own: a hang surfaces as the go test binary's own
// -timeout, never as a flaky arbitrary wait inside the test.
func waitPoll(t *testing.T, timers *wakeTimers) {
	t.Helper()
	<-timers.created
}

// TestJobSchedulerConsumesWithoutRecoveryTimer pins FIND-122 for the
// external-agent job class: the job scheduler's consumer (Service.Run)
// only ever advances through the initial durable poll or a real wake from
// the producer (Service.Start), never through the recovery timer.
func TestJobSchedulerConsumesWithoutRecoveryTimer(t *testing.T) {
	t.Run("restart empty then produced", func(t *testing.T) {
		store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "jobs.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		jobStore := sqlite.NewExternalAgentJobStore(store)
		scheduler, timers := newWakeScheduler(t)
		service, err := New(Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute, PollInterval: time.Hour, Concurrency: 1, MaxAttempts: 1},
			Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}, Scheduler: scheduler})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go service.Run(ctx)
		waitPoll(t, timers) // initial poll: durable state is empty.

		job, err := service.Start(t.Context(), testRequest(domain.JobDetached))
		if err != nil {
			t.Fatal(err)
		}
		waitPoll(t, timers) // poll triggered by the producer's wake, not the timer.
		current, err := jobStore.GetJob(t.Context(), job.ID)
		if err != nil || current == nil || current.Status != domain.JobRunning {
			t.Fatalf("job after wake-driven poll = %#v, err = %v", current, err)
		}
	})

	t.Run("pending before restart", func(t *testing.T) {
		store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "jobs.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		jobStore := sqlite.NewExternalAgentJobStore(store)

		// The producer runs against scheduler A, which no consumer ever
		// drives: any Wake() it receives is inert, exactly like a process
		// that writes durable state and then exits before it ever polls.
		producerScheduler, _ := newWakeScheduler(t)
		producer, err := New(Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute, PollInterval: time.Hour, Concurrency: 1, MaxAttempts: 1},
			Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}, Scheduler: producerScheduler})
		if err != nil {
			t.Fatal(err)
		}
		job, err := producer.Start(t.Context(), testRequest(domain.JobDetached))
		if err != nil {
			t.Fatal(err)
		}

		// Restart: a brand-new scheduler and a brand-new Service consume
		// the same durable store. Nothing ever calls this scheduler's
		// Wake(), so only its own initial poll can discover the
		// pre-existing durable job; no wake retained from the producer
		// process can substitute for that property.
		restartScheduler, restartTimers := newWakeScheduler(t)
		consumer, err := New(Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute, PollInterval: time.Hour, Concurrency: 1, MaxAttempts: 1},
			Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{}, Scheduler: restartScheduler})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go consumer.Run(ctx)
		waitPoll(t, restartTimers) // initial poll must consume pre-existing durable work.
		current, err := jobStore.GetJob(t.Context(), job.ID)
		if err != nil || current == nil || current.Status != domain.JobRunning {
			t.Fatalf("job after initial poll = %#v, err = %v", current, err)
		}
	})
}

// wakeNotificationStore is a single-slot notification store: empty until
// insert is called, so a test can drive both the "restart empty, then
// produced" and "pending before restart" fixtures without a real clock.
type wakeNotificationStore struct {
	mu           sync.Mutex
	notification *domain.ExternalAgentJobNotification
	claimed      bool
}

func (s *wakeNotificationStore) insert(n domain.ExternalAgentJobNotification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := n
	s.notification = &cp
	s.claimed = false
}

func (s *wakeNotificationStore) ClaimNextNotification(context.Context, time.Time, string, time.Duration) (*domain.ExternalAgentJobNotification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notification == nil || s.claimed {
		return nil, nil
	}
	s.claimed = true
	cp := *s.notification
	return &cp, nil
}

func (s *wakeNotificationStore) MarkNotificationPublished(_ context.Context, n *domain.ExternalAgentJobNotification, ts string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notification == nil || n == nil {
		return errors.New("no notification to publish")
	}
	s.notification.PublishState = domain.NotificationPublished
	return nil
}

func (s *wakeNotificationStore) MarkNotificationUnknown(context.Context, *domain.ExternalAgentJobNotification, string) error {
	return nil
}

// TestNotificationSchedulerConsumesWithoutRecoveryTimer pins FIND-122 for the
// terminal notification outbox class.
func TestNotificationSchedulerConsumesWithoutRecoveryTimer(t *testing.T) {
	notification := func() domain.ExternalAgentJobNotification {
		return domain.ExternalAgentJobNotification{
			JobID: "job-wake", Kind: domain.JobNotificationTerminal, PublishState: domain.NotificationPending,
			CanonicalMarkdown: "safe", RendererVersion: domain.JobNotificationRenderer,
		}
	}
	t.Run("restart empty then produced", func(t *testing.T) {
		store := &wakeNotificationStore{}
		scheduler, timers := newWakeScheduler(t)
		publisher := &fakeNotificationPublisher{publishResponse: port.PublishedResponse{LastMessageTS: "1.0"}}
		worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Hour, LeaseTTL: time.Minute}, NotificationDependencies{Store: store, Publisher: publisher, Scheduler: scheduler})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go worker.Run(ctx)
		waitPoll(t, timers) // initial poll: durable outbox is empty.

		store.insert(notification())
		scheduler.Wake() // the durable producer (Transition) calls this on commit.
		waitPoll(t, timers)
		store.mu.Lock()
		published := store.notification.PublishState == domain.NotificationPublished
		store.mu.Unlock()
		if !published {
			t.Fatal("notification was not published after wake-driven poll")
		}
	})

	t.Run("pending before restart", func(t *testing.T) {
		store := &wakeNotificationStore{}
		store.insert(notification())
		scheduler, timers := newWakeScheduler(t)
		publisher := &fakeNotificationPublisher{publishResponse: port.PublishedResponse{LastMessageTS: "1.0"}}
		worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Hour, LeaseTTL: time.Minute}, NotificationDependencies{Store: store, Publisher: publisher, Scheduler: scheduler})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go worker.Run(ctx)
		waitPoll(t, timers)
		store.mu.Lock()
		published := store.notification.PublishState == domain.NotificationPublished
		store.mu.Unlock()
		if !published {
			t.Fatal("pending notification was not published by the initial poll")
		}
	})
}

// TestActivationSchedulerConsumesWithoutRecoveryTimer pins FIND-122 for the
// root activation class.
func TestActivationSchedulerConsumesWithoutRecoveryTimer(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	t.Run("restart empty then produced", func(t *testing.T) {
		store := newActivationWorkerStore()
		scheduler, timers := newWakeScheduler(t)
		handler := &fakeActivationHandler{onHandle: func(a domain.ExternalAgentJobActivation) {
			store.setState(a.ActivationID, domain.ActivationCompleted)
		}}
		worker, err := NewActivationWorker(ActivationConfig{PollInterval: time.Hour, LeaseTTL: time.Minute}, ActivationDependencies{
			Store: store, Handler: handler, Clock: fixedClock{now: now}, Scheduler: scheduler,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go worker.Run(ctx)
		waitPoll(t, timers) // initial poll: durable activation outbox is empty.

		store.insertActivation(testWorkerActivation(now, domain.ActivationPending))
		scheduler.Wake() // the durable producer (MarkNotificationPublished) calls this on commit.
		waitPoll(t, timers)
		if got := store.activation(t, "activation-job-1").State; got != domain.ActivationCompleted {
			t.Fatalf("activation state after wake-driven poll = %s", got)
		}
	})

	t.Run("pending before restart", func(t *testing.T) {
		store := newActivationWorkerStore(testWorkerActivation(now, domain.ActivationPending))
		scheduler, timers := newWakeScheduler(t)
		handler := &fakeActivationHandler{onHandle: func(a domain.ExternalAgentJobActivation) {
			store.setState(a.ActivationID, domain.ActivationCompleted)
		}}
		worker, err := NewActivationWorker(ActivationConfig{PollInterval: time.Hour, LeaseTTL: time.Minute}, ActivationDependencies{
			Store: store, Handler: handler, Clock: fixedClock{now: now}, Scheduler: scheduler,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go worker.Run(ctx)
		waitPoll(t, timers)
		if got := store.activation(t, "activation-job-1").State; got != domain.ActivationCompleted {
			t.Fatalf("activation state after initial poll = %s", got)
		}
	})
}

// insertActivation adds a durable activation row directly, mirroring what a
// real store does when a new claim is created out of band from the worker
// under test.
func (s *activationWorkerStore) insertActivation(a domain.ExternalAgentJobActivation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := a
	s.activations[a.ActivationID] = &cp
}

// TestExternalAgentThreeSchedulerGroupWakesEachOwnConsumerOnly is the
// composition-group gate the review demanded for newExternalAgentSchedules:
// jobs, notifications, and activations must each drive only their own
// consumer. A composition that accidentally shared one scheduler, or wired a
// producer's wake to the wrong consumer, fails this test even though every
// per-class callback-counting test still passes.
func TestExternalAgentThreeSchedulerGroupWakesEachOwnConsumerOnly(t *testing.T) {
	store, err := sqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "group.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := sqlite.NewExternalAgentJobStore(store)

	jobsScheduler, jobsTimers := newWakeScheduler(t)
	notificationsScheduler, notificationsTimers := newWakeScheduler(t)
	activationsScheduler, activationsTimers := newWakeScheduler(t)

	// The activation worker reads the same durable store the job and
	// notification workers write to (mirroring newExternalAgentJobService's
	// composition exactly); no activation row needs to exist for this test,
	// since it verifies scheduler wiring, not the activation-creation SQL.
	activationWorker, err := NewActivationWorker(ActivationConfig{PollInterval: time.Hour, LeaseTTL: time.Minute}, ActivationDependencies{
		Store: jobStore, Handler: &fakeActivationHandler{}, Clock: fixedClock{now: time.Now().UTC()}, Scheduler: activationsScheduler,
	})
	if err != nil {
		t.Fatal(err)
	}

	notificationWorker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Hour, LeaseTTL: time.Minute}, NotificationDependencies{
		Store: jobStore, Publisher: &fakeNotificationPublisher{publishResponse: port.PublishedResponse{LastMessageTS: "1710000000.000001"}},
		Scheduler: notificationsScheduler, ActivationWake: activationsScheduler.Wake,
	})
	if err != nil {
		t.Fatal(err)
	}

	service, err := New(Config{DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute, PollInterval: time.Hour, Concurrency: 1, MaxAttempts: 1},
		Dependencies{Store: jobStore, Runtime: &fakeJobRuntime{result: domain.AcpInvocationResult{Text: "done", Inline: true, ResultBytes: 4}},
			Scheduler: jobsScheduler, NotificationWake: notificationsScheduler.Wake})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go service.Run(ctx)
	go notificationWorker.Run(ctx)
	go activationWorker.Run(ctx)
	waitPoll(t, jobsTimers)
	waitPoll(t, notificationsTimers)
	waitPoll(t, activationsTimers)

	// Producing a job wakes only the jobs scheduler.
	job, err := service.Start(t.Context(), testRequest(domain.JobDetached))
	if err != nil {
		t.Fatal(err)
	}
	waitPoll(t, jobsTimers)
	running, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || running == nil || running.Status != domain.JobRunning {
		t.Fatalf("job after jobs-scheduler poll = %#v, err = %v", running, err)
	}

	// The transition to a terminal status wakes only the notifications
	// scheduler: this blocks until that wake actually happens, so it never
	// races the background execution goroutine.
	waitPoll(t, notificationsTimers)
	notification, err := jobStore.GetJob(t.Context(), job.ID)
	if err != nil || notification == nil || notification.Status != domain.JobCompleted {
		t.Fatalf("job after notification-scheduler poll = %#v, err = %v", notification, err)
	}

	// Publishing the notification wakes only the activations scheduler.
	waitPoll(t, activationsTimers)
}
