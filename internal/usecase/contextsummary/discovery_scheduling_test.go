package contextsummary

import (
	"context"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// scaledSummaryStore counts ScheduleSummaryJob calls (writes) without ever
// reading turn history, mirroring the real sqlite store's O(1) job upsert.
type scaledSummaryStore struct {
	summaryStoreFake
	scheduleCalls int
}

func (s *scaledSummaryStore) ScheduleSummaryJob(ctx context.Context, sessionIdentity string, target int64, at time.Time) (bool, error) {
	s.scheduleCalls++
	return s.summaryStoreFake.ScheduleSummaryJob(ctx, sessionIdentity, target, at)
}

// scaledTurnSource simulates a session with turnCount closed turns. Every
// ClosedTurns call is recorded and its cost is charged against readCost, so
// the test can prove ScheduleConversation's foreground cost does not grow
// with session size, exactly the property FIND-131 requires.
type scaledTurnSource struct {
	turnCount int
	calls     *int
	readCost  *int
}

func (s scaledTurnSource) ClosedTurns(_ context.Context, _ string, after, through int64) ([]domain.ConversationTurn, error) {
	*s.calls++
	*s.readCost += s.turnCount
	turns := make([]domain.ConversationTurn, 0, s.turnCount)
	for i := 1; i <= s.turnCount; i++ {
		ordinal := int64(i)
		if ordinal > after && ordinal <= through {
			turns = append(turns, domain.ConversationTurn{Ordinal: ordinal, Closed: true})
		}
	}
	return turns, nil
}

// TestScheduleConversationForegroundCostIsBoundedBySessionSize is Gate 1 for
// FIND-131: a short session and a 10,000-event-equivalent session must
// perform the same constant, bounded event-read work before foreground
// scheduling (bot.Service's real call path, ScheduleConversation) returns.
// Restoring the pre-fix synchronous ClosedTurns call in ScheduleConversation
// makes this fail on the scaling assertion itself (readCost grows with
// turnCount), not merely because a mock expected an unexpected call.
func TestScheduleConversationForegroundCostIsBoundedBySessionSize(t *testing.T) {
	run := func(t *testing.T, turnCount int) (scheduleCalls, turnSourceCalls, readCost int) {
		t.Helper()
		store := &scaledSummaryStore{}
		calls, cost := 0, 0
		service, err := New(Config{MaxChars: 100, RecentTurns: 2, WorkerInterval: time.Hour}, Dependencies{
			Store: store, Summarizer: summarizerFake{}, ScheduleWake: func() {},
			TurnSource: scaledTurnSource{turnCount: turnCount, calls: &calls, readCost: &cost},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.ScheduleConversation(t.Context(), "session-scaled"); err != nil {
			t.Fatal(err)
		}
		return store.scheduleCalls, calls, cost
	}

	shortSchedules, shortTurnCalls, shortReadCost := run(t, 5)
	largeSchedules, largeTurnCalls, largeReadCost := run(t, 10_000)

	if shortTurnCalls != 0 || largeTurnCalls != 0 {
		t.Fatalf("ScheduleConversation called ClosedTurns short=%d large=%d, want 0 for both (turn discovery must move to the background worker)", shortTurnCalls, largeTurnCalls)
	}
	if shortReadCost != 0 || largeReadCost != 0 {
		t.Fatalf("ScheduleConversation event-read cost short=%d large=%d, want 0 for both", shortReadCost, largeReadCost)
	}
	if shortSchedules != largeSchedules {
		t.Fatalf("ScheduleConversation store write count short=%d large=%d, want equal (bounded, independent of session size)", shortSchedules, largeSchedules)
	}
}

// TestDiscoveryWorkerResolvesCoveredRangeAcrossRestartWithNoLocalWake is Gate
// 2 for FIND-131: after ScheduleConversation records a discovery job, a
// brand-new Service instance (a distinct scheduler and Wake, standing in for
// a process restart) must still resolve the same covered turn range through
// its own initial poll, never through a wake retained by the process that
// scheduled the job.
func TestDiscoveryWorkerResolvesCoveredRangeAcrossRestartWithNoLocalWake(t *testing.T) {
	turns := make([]domain.ConversationTurn, 12)
	for i := range turns {
		turns[i] = domain.ConversationTurn{Ordinal: int64(i + 1), Closed: true}
	}
	store := &wakeSummaryStore{}

	producerScheduler, _ := newWakeScheduler(t)
	producer, err := New(Config{MaxChars: 1000, RecentTurns: 3, WorkerInterval: time.Hour}, Dependencies{
		Store: store, Summarizer: summarizerFake{}, TurnSource: wakeTurnSource{turns: turns}, Scheduler: producerScheduler,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.ScheduleConversation(t.Context(), "session-restart"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	pending := store.pending
	store.mu.Unlock()
	if pending == nil || pending.TargetOrdinal != domain.SummaryDiscoveryTargetFloor {
		t.Fatalf("scheduled job = %#v, want a pending discovery job", pending)
	}

	restartScheduler, restartTimers := newWakeScheduler(t)
	consumer, err := New(Config{MaxChars: 1000, RecentTurns: 3, WorkerInterval: time.Hour}, Dependencies{
		Store: store, Summarizer: summarizerFake{}, TurnSource: wakeTurnSource{turns: turns}, Scheduler: restartScheduler,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go consumer.Run(ctx)
	waitPoll(t, restartTimers) // initial poll must resolve the pre-existing discovery job.

	store.mu.Lock()
	done := store.hasRecord
	covered := store.record.CoveredThroughOrdinal
	store.mu.Unlock()
	// 12 closed turns, RecentTurns=3: the newest contiguous prefix outside
	// retention covers ordinals 1..9.
	const wantCovered = int64(9)
	if !done || covered != wantCovered {
		t.Fatalf("post-restart summary state = done=%v covered=%d, want done=true covered=%d", done, covered, wantCovered)
	}
}

var _ port.SummaryStore = (*scaledSummaryStore)(nil)
