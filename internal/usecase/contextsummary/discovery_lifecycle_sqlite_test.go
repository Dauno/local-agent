package contextsummary

// FIND-134 end-to-end lifecycle gate: internal/usecase/contextsummary always
// schedules the same fixed domain.SummaryDiscoveryTargetFloor marker, and the durable
// jobs table's primary key (session_identity, target_ordinal) retains
// completed rows. This exercises the real sqlite store, not a fake, through
// two full discovery rounds for the same session: schedule, claim, resolve,
// commit, then schedule again after completion and prove the second round
// resolves against newly eligible turns instead of failing on the reused
// marker. The architecture test excludes _test.go files, so this file may
// import internal/adapter/sqlite even though production contextsummary code
// must not.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// mutableTurnSource lets a test grow the closed-turn set between discovery
// rounds, standing in for new turns landing between two summary cycles.
type mutableTurnSource struct{ turns *[]domain.ConversationTurn }

func (m mutableTurnSource) ClosedTurns(_ context.Context, _ string, after, through int64) ([]domain.ConversationTurn, error) {
	var out []domain.ConversationTurn
	for _, turn := range *m.turns {
		if turn.Ordinal > after && turn.Ordinal <= through {
			out = append(out, turn)
		}
	}
	return out, nil
}

func closedTurnsThrough(n int) []domain.ConversationTurn {
	turns := make([]domain.ConversationTurn, n)
	for i := range turns {
		turns[i] = domain.ConversationTurn{Ordinal: int64(i + 1), Closed: true}
	}
	return turns
}

func TestDiscoverySchedulesAgainAfterCompletionAndCoversNewTurnsWithRealStore(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "discovery-lifecycle.db")
	store, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatalf("initialize store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	turns := closedTurnsThrough(20)
	source := mutableTurnSource{turns: &turns}
	const session = "adk:session-lifecycle"

	service, err := New(Config{MaxChars: 1000, RecentTurns: 3, WorkerInterval: time.Hour}, Dependencies{
		Store: store, Summarizer: summarizerFake{}, TurnSource: source,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Round 1: 20 closed turns, RecentTurns=3, so discovery must cover
	// through ordinal 17 (20 - 3 held back).
	if err := service.ScheduleConversation(ctx, session); err != nil {
		t.Fatalf("round 1 schedule: %v", err)
	}
	if err := service.RunOnce(ctx, time.Now()); err != nil {
		t.Fatalf("round 1 run: %v", err)
	}
	summary, err := store.LatestSummary(ctx, session)
	if err != nil || summary.CoveredThroughOrdinal != 17 {
		t.Fatalf("round 1 summary = %#v, %v, want covered=17", summary, err)
	}

	// Ten more turns land. A later successful turn schedules discovery again
	// with the exact same literal marker ScheduleConversation always uses.
	turns = append(turns, closedTurnsThrough(30)[20:]...)
	if err := service.ScheduleConversation(ctx, session); err != nil {
		t.Fatalf("round 2 schedule after completion (must not hit a unique-constraint error): %v", err)
	}
	if err := service.RunOnce(ctx, time.Now()); err != nil {
		t.Fatalf("round 2 run: %v", err)
	}
	summary, err = store.LatestSummary(ctx, session)
	if err != nil || summary.CoveredThroughOrdinal != 27 {
		t.Fatalf("round 2 summary = %#v, %v, want covered=27 (newly eligible turns 18-27)", summary, err)
	}
}

// TestDiscoveryWorkerRecognizesBumpedMarkerAsDiscoveryAfterRestart is
// FIND-135's restart gate: internal/adapter/sqlite hands out a marker above
// domain.SummaryDiscoveryTargetFloor when the fixed floor value collides
// (FIND-134), and a fresh Service instance (a distinct scheduler, no shared
// wake, standing in for a process restart) must still recognize that bumped
// marker as a discovery job rather than a concrete turn target, and resolve
// it against real closed-turn history.
func TestDiscoveryWorkerRecognizesBumpedMarkerAsDiscoveryAfterRestart(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "discovery-bumped-restart.db")
	store, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatalf("initialize store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	turns := closedTurnsThrough(12)
	source := mutableTurnSource{turns: &turns}
	const session = "adk:session-bumped-restart"

	producer, err := New(Config{MaxChars: 1000, RecentTurns: 3, WorkerInterval: time.Hour}, Dependencies{
		Store: store, Summarizer: summarizerFake{}, TurnSource: source,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Schedule the first discovery, then claim it directly (bypassing
	// RunOnce) so it sits at 'running' while a second turn schedules again.
	if err := producer.ScheduleConversation(ctx, session); err != nil {
		t.Fatalf("first schedule: %v", err)
	}
	firstJob, err := store.ClaimSummaryJob(ctx, time.Now())
	if err != nil {
		t.Fatalf("claim first discovery: %v", err)
	}
	if firstJob.TargetOrdinal != domain.SummaryDiscoveryTargetFloor {
		t.Fatalf("first discovery target = %d, want the floor marker %d", firstJob.TargetOrdinal, domain.SummaryDiscoveryTargetFloor)
	}
	if err := producer.ScheduleConversation(ctx, session); err != nil {
		t.Fatalf("schedule while first is running: %v", err)
	}

	// The first discovery finishes, covering an arbitrary early range so the
	// follow-up has real history to resolve against.
	committed, err := store.CommitSummary(ctx, portSummaryCommit(session, 5))
	if err != nil || !committed {
		t.Fatalf("commit first discovery result = %v, %v", committed, err)
	}
	if err := store.CompleteSummaryJob(ctx, firstJob); err != nil {
		t.Fatalf("complete first discovery: %v", err)
	}

	// Restart: a brand-new Service, distinct scheduler, no wake carried over.
	// Its RunOnce must itself claim the bumped follow-up row and recognize
	// it as a discovery job. If it instead treated the bumped target_ordinal
	// as a concrete turn ordinal, validateContiguousTurns would reject the
	// real turns (they cannot reach a value near math.MaxInt64/4) and the
	// covered ordinal below would stay at 5, not advance to 9.
	consumer, err := New(Config{MaxChars: 1000, RecentTurns: 3, WorkerInterval: time.Hour}, Dependencies{
		Store: store, Summarizer: summarizerFake{}, TurnSource: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.RunOnce(ctx, time.Now()); err != nil {
		t.Fatalf("consumer run: %v", err)
	}
	summary, err := store.LatestSummary(ctx, session)
	// 12 closed turns, previous covered=5, RecentTurns=3: the newest
	// contiguous prefix outside retention among turns 6-12 covers through 9.
	const wantCovered = int64(9)
	if err != nil || summary.CoveredThroughOrdinal != wantCovered {
		t.Fatalf("post-restart summary = %#v, %v, want covered=%d", summary, err, wantCovered)
	}
}

func portSummaryCommit(sessionIdentity string, coveredThroughOrdinal int64) port.SummaryCommit {
	return port.SummaryCommit{
		SessionIdentity: sessionIdentity,
		Summary: port.ConversationSummary{
			Text: "The user stated a goal.", CoveredThroughOrdinal: coveredThroughOrdinal,
			SourceDigest: "digest-1", PromptVersion: "v1",
		},
	}
}
