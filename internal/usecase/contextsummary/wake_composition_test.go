package contextsummary

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

// wakeTimer is a workpoll.Timer whose channel never fires on its own.
type wakeTimer struct{ c chan time.Time }

func (t *wakeTimer) C() <-chan time.Time { return t.c }
func (t *wakeTimer) Stop() bool          { return true }

type wakeTimers struct{ created chan *wakeTimer }

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

// waitPoll blocks on the fake timer's creation channel only: it carries no
// real duration of its own, so a hang here surfaces as the go test binary's
// own -timeout, never as a flaky arbitrary wait inside the test.
func waitPoll(t *testing.T, timers *wakeTimers) {
	t.Helper()
	<-timers.created
}

// wakeSummaryStore is a single-slot durable summary job store: empty until
// ScheduleSummaryJob is called, so a test can drive both restart fixtures
// without a real clock.
type wakeSummaryStore struct {
	mu        sync.Mutex
	hasRecord bool
	record    port.SummaryRecord
	pending   *port.SummaryJob
	claimed   bool
}

func (s *wakeSummaryStore) LatestSummary(context.Context, string) (port.SummaryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasRecord {
		return port.SummaryRecord{}, port.ErrSummaryNotFound
	}
	return s.record, nil
}

func (s *wakeSummaryStore) ScheduleSummaryJob(_ context.Context, sessionIdentity string, target int64, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = &port.SummaryJob{SessionIdentity: sessionIdentity, TargetOrdinal: target}
	s.claimed = false
	return true, nil
}

func (s *wakeSummaryStore) ClaimSummaryJob(context.Context, time.Time) (port.SummaryJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil || s.claimed {
		return port.SummaryJob{}, port.ErrSummaryNotFound
	}
	s.claimed = true
	return *s.pending, nil
}

func (s *wakeSummaryStore) CommitSummary(_ context.Context, commit port.SummaryCommit) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record = port.SummaryRecord{SessionIdentity: commit.SessionIdentity, CoveredThroughOrdinal: commit.Summary.CoveredThroughOrdinal, SourceDigest: commit.Summary.SourceDigest, SanitizedText: commit.Summary.Text}
	s.hasRecord = true
	return true, nil
}

func (s *wakeSummaryStore) CompleteSummaryJob(context.Context, port.SummaryJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = nil
	return nil
}

func (s *wakeSummaryStore) FailSummaryJob(context.Context, port.SummaryJob, time.Time) error {
	return nil
}

// wakeTurnSource returns closed turns whose ordinal falls in (from, to], the
// same range semantics the real turn source uses.
type wakeTurnSource struct{ turns []domain.ConversationTurn }

func (s wakeTurnSource) ClosedTurns(_ context.Context, _ string, from, to int64) ([]domain.ConversationTurn, error) {
	var out []domain.ConversationTurn
	for _, turn := range s.turns {
		if turn.Ordinal > from && turn.Ordinal <= to {
			out = append(out, turn)
		}
	}
	return out, nil
}

// TestSummarySchedulerConsumesWithoutRecoveryTimer pins FIND-122 for the
// summary job class: the worker (Service.Run) only ever advances through the
// initial durable poll or a real wake from ScheduleConversation, never
// through the recovery timer.
func TestSummarySchedulerConsumesWithoutRecoveryTimer(t *testing.T) {
	newService := func(t *testing.T, store *wakeSummaryStore, scheduler *workpoll.Scheduler) *Service {
		t.Helper()
		service, err := New(Config{MaxChars: 1000, RecentTurns: 1, WorkerInterval: time.Hour}, Dependencies{
			Store: store, Summarizer: summarizerFake{}, TurnSource: wakeTurnSource{turns: []domain.ConversationTurn{{Ordinal: 1, Closed: true}, {Ordinal: 2, Closed: true}}},
			Scheduler: scheduler,
		})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}

	t.Run("restart empty then produced", func(t *testing.T) {
		store := &wakeSummaryStore{}
		scheduler, timers := newWakeScheduler(t)
		service := newService(t, store, scheduler)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go service.Run(ctx)
		waitPoll(t, timers) // initial poll: no summary job is pending.

		if err := service.ScheduleConversation(t.Context(), "session-wake"); err != nil {
			t.Fatal(err)
		}
		waitPoll(t, timers) // poll triggered by the producer's wake, not the timer.
		store.mu.Lock()
		done := store.hasRecord && store.record.CoveredThroughOrdinal == 1
		store.mu.Unlock()
		if !done {
			t.Fatal("summary job was not consumed after wake-driven poll")
		}
	})

	t.Run("pending before restart", func(t *testing.T) {
		store := &wakeSummaryStore{}

		// The producer runs against scheduler A, which no consumer ever
		// drives: ScheduleConversation's internal wake is inert, exactly
		// like a process that writes durable state and then exits before
		// it ever polls.
		producerScheduler, _ := newWakeScheduler(t)
		producer := newService(t, store, producerScheduler)
		if err := producer.ScheduleConversation(t.Context(), "session-wake"); err != nil {
			t.Fatal(err)
		}

		// Restart: a brand-new scheduler and a brand-new Service consume
		// the same durable store. Nothing ever calls this scheduler's
		// Wake(), so only its own initial poll can discover the
		// pre-existing durable job; no wake retained from the producer
		// process can substitute for that property.
		restartScheduler, restartTimers := newWakeScheduler(t)
		consumer := newService(t, store, restartScheduler)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go consumer.Run(ctx)
		waitPoll(t, restartTimers) // initial poll must consume the pre-existing durable job.
		store.mu.Lock()
		done := store.hasRecord && store.record.CoveredThroughOrdinal == 1
		store.mu.Unlock()
		if !done {
			t.Fatal("pre-existing summary job was not consumed by the initial poll")
		}
	})
}
