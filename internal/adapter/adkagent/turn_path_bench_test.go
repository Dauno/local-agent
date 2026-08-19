package adkagent

// TRD 08 checkpoint 2, Gate A numeric form
// (docs/root-orchestrator-v2/hallazgos/worker-prompt-trd08-checkpoint2.md):
// over the turn path's benchmark, allocs/op at 10,000 events divided by
// allocs/op at 1,000 events must not exceed 1.5. Before this checkpoint the
// ratio was 9.97, because updateContinuity loaded the whole session to
// compute two integers. After it, updateContinuity uses LatestEventOrdinal
// (an indexed head read), so the turn path's allocation cost should no
// longer scale with session size at all.
//
// The architecture test excludes _test.go files, so this benchmark may
// import internal/adapter/sqlite to seed a real corpus, matching the pattern
// in internal/adapter/sqlite/session_read_bench_test.go (checkpoint 1).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
)

// turnPathBenchApp and turnPathBenchUser must be the same constants
// continuityHead queries with (applicationName, ephemeralUserID), not
// independent literals: a literal here can silently drift from the query and
// make the seeded corpus invisible to the operation under benchmark (FIND-112).
const (
	turnPathBenchApp  = applicationName
	turnPathBenchUser = ephemeralUserID
	// turnPathBenchContentBytes matches the 2.67 KiB mean content size
	// recorded against the deployed database in the worker prompt.
	turnPathBenchContentBytes = 2700
)

var turnPathBenchSizes = []int{1_000, 10_000}

type turnPathBenchCorpus struct {
	store     *adaptersqlite.Store
	service   *adaptersqlite.AdkSessionService
	sessionID map[int]string
}

var (
	turnPathBenchOnce  sync.Once
	turnPathBenchState turnPathBenchCorpus
	turnPathBenchErr   error
)

func turnPathBench(tb testing.TB) *turnPathBenchCorpus {
	tb.Helper()
	turnPathBenchOnce.Do(func() {
		turnPathBenchState, turnPathBenchErr = seedTurnPathBenchCorpus()
	})
	if turnPathBenchErr != nil {
		tb.Fatalf("seed turn path benchmark corpus: %v", turnPathBenchErr)
	}
	return &turnPathBenchState
}

func seedTurnPathBenchCorpus() (turnPathBenchCorpus, error) {
	fail := func(err error) (turnPathBenchCorpus, error) {
		return turnPathBenchCorpus{}, fmt.Errorf("seed turn path benchmark corpus: %w", err)
	}
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "turn-path-bench-")
	if err != nil {
		return fail(err)
	}
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(dir, "bench.db"))
	if err != nil {
		return fail(err)
	}

	now := time.Now().UnixMicro()
	filler := strings.Repeat("x", turnPathBenchContentBytes-40)
	sessionIDs := make(map[int]string, len(turnPathBenchSizes))

	db := store.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO adk_app_state (app_name, state, update_time) VALUES (?, '{}', ?)`,
		turnPathBenchApp, now); err != nil {
		store.Close()
		return fail(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO adk_user_state (app_name, user_id, state, update_time) VALUES (?, ?, '{}', ?)`,
		turnPathBenchApp, turnPathBenchUser, now); err != nil {
		store.Close()
		return fail(err)
	}
	for _, n := range turnPathBenchSizes {
		sessionID := fmt.Sprintf("sess-%d", n)
		sessionIDs[n] = sessionID

		if _, err := db.ExecContext(ctx, `INSERT INTO adk_sessions
			(app_name, user_id, session_id, state, revision, create_time, update_time)
			VALUES (?, ?, ?, '{}', ?, ?, ?)`,
			turnPathBenchApp, turnPathBenchUser, sessionID, n, now, now); err != nil {
			store.Close()
			return fail(fmt.Errorf("insert session %s: %w", sessionID, err))
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			store.Close()
			return fail(err)
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO adk_events
			(id, app_name, user_id, session_id, ordinal, invocation_id, author, actions, timestamp, content, partial, turn_complete, interrupted)
			VALUES (?, ?, ?, ?, ?, ?, ?, '{}', ?, ?, 0, 1, 0)`)
		if err != nil {
			_ = tx.Rollback()
			store.Close()
			return fail(err)
		}
		for i := 0; i < n; i++ {
			content := fmt.Sprintf(`{"role":"model","parts":[{"text":"event %d %s"}]}`, i, filler)
			if _, err := stmt.ExecContext(ctx, fmt.Sprintf("%s-evt-%d", sessionID, i),
				turnPathBenchApp, turnPathBenchUser, sessionID, i, "bench-invocation", "model",
				now+int64(i), content); err != nil {
				_ = stmt.Close()
				_ = tx.Rollback()
				store.Close()
				return fail(fmt.Errorf("insert event %d for session %s: %w", i, sessionID, err))
			}
		}
		if err := stmt.Close(); err != nil {
			_ = tx.Rollback()
			store.Close()
			return fail(err)
		}
		if err := tx.Commit(); err != nil {
			store.Close()
			return fail(fmt.Errorf("commit session %s corpus: %w", sessionID, err))
		}
	}

	return turnPathBenchCorpus{store: store, service: adaptersqlite.NewAdkSessionService(store), sessionID: sessionIDs}, nil
}

// BenchmarkTurnPathUpdateContinuity measures updateContinuity's allocation
// cost in isolation: this is the exact call every normal turn makes
// (runtime.go Run/Stream), and it is the highest-cost unbounded caller the
// TRD identified. Gate A requires allocs/op at 10,000 events divided by
// allocs/op at 1,000 events to be at most 1.5.
func BenchmarkTurnPathUpdateContinuity(b *testing.B) {
	corpus := turnPathBench(b)
	for _, n := range turnPathBenchSizes {
		sessionID := corpus.sessionID[n]
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			ctx := context.Background()
			// recordingContinuityStore (continuity_runtime_test.go) is a minimal
			// in-memory port.ContinuityStore, reused here so the benchmark
			// isolates updateContinuity's own allocation cost.
			store := &recordingContinuityStore{}
			runtime := &Runtime{
				sessionService:  corpus.service,
				continuityStore: store,
			}
			for b.Loop() {
				runtime.updateContinuity(ctx, sessionID, "current turn text", "final response text", true)
			}
			// FIND-112: assert on what the measured operation actually
			// observed, not on what the benchmark seeded. A misnamed corpus
			// (wrong app/user) makes continuityHead return ok=false and
			// updateContinuity exit through recordContinuityFallback before
			// it ever reaches the store, and the allocation ratio would then
			// pass by accident (constant-cost no-op at every size).
			b.StopTimer()
			if store.commits == 0 {
				b.Fatalf("continuity store recorded no commit: updateContinuity never reached the head read")
			}
			if got, want := store.capsule.CoveredThrough, int64(n-1); got != want {
				b.Fatalf("covered through ordinal = %d, want %d", got, want)
			}
			b.ReportMetric(float64(n), "events")
		})
	}
}
