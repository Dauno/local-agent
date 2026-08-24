package sqlite

// TRD 08 item 3 (docs/root-orchestrator-v2/hallazgos/worker-prompt-trd08-matriz-benchmarks.md):
// contention over the single connection that decision 2 needs data for.
//
// This file never touches db.go and never opens the deployed database. Each
// subtest copies a small seeded database file into a fresh temp file and
// opens its own *sql.DB with pragmas set in the DSN, exactly like
// docs/root-orchestrator-v2/hallazgos/bench-trd08-baseline/write does.
// Measuring a configuration here is not adopting it in production.
//
// Grid: journal_mode in {delete, wal} x MaxOpenConns in {1, 2, 4, 8} x
// background poller count in {0, 4, 9}, all reads (no writer in the loop):
// DEC-08-1 already measured write/commit latency by journal and synchronous
// mode, so this item isolates the connection-pool question decision 2 asks:
// how much read pool helps, and does it help under both journal modes.
// Pollers run as fast as they can rather than on the real ~1s ticker
// cadence, which is a deliberate worst-case stress rather than a
// steady-state simulation; this is called out in the report.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"
)

const contentionBenchTurnEvents = 200

// buildContentionSeedDB creates one small seeded database, once per process,
// used as the read-only source copied into each subtest's scratch file.
func buildContentionSeedDB(tb testing.TB) string {
	tb.Helper()
	dir := tb.TempDir()
	path := filepath.Join(dir, "seed.db")
	ctx := context.Background()
	store, err := Initialize(ctx, path)
	if err != nil {
		tb.Fatalf("seed contention benchmark db: %v", err)
	}
	now := time.Now().UnixMicro()
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO adk_app_state (app_name, state, update_time) VALUES (?, '{}', ?)`,
		sessionReadBenchApp, now); err != nil {
		tb.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO adk_user_state (app_name, user_id, state, update_time) VALUES (?, ?, '{}', ?)`,
		sessionReadBenchApp, sessionReadBenchUser, now); err != nil {
		tb.Fatal(err)
	}
	sessionID := "turn-session"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO adk_sessions
		(app_name, user_id, session_id, state, revision, create_time, update_time)
		VALUES (?, ?, ?, '{}', ?, ?, ?)`,
		sessionReadBenchApp, sessionReadBenchUser, sessionID, contentionBenchTurnEvents, now, now); err != nil {
		tb.Fatal(err)
	}
	filler := strings.Repeat("x", sessionReadBenchContentBytes-40)
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO adk_events
		(id, app_name, user_id, session_id, ordinal, invocation_id, author, actions, timestamp, content, partial, turn_complete, interrupted)
		VALUES (?, ?, ?, ?, ?, ?, ?, '{}', ?, ?, 0, 1, 0)`)
	if err != nil {
		tb.Fatal(err)
	}
	for i := range contentionBenchTurnEvents {
		content := fmt.Sprintf(`{"role":"model","parts":[{"text":"event %d %s"}]}`, i, filler)
		if _, err := stmt.ExecContext(ctx, fmt.Sprintf("%s-evt-%d", sessionID, i),
			sessionReadBenchApp, sessionReadBenchUser, sessionID, i, "bench-invocation", "model",
			now+int64(i), content); err != nil {
			tb.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		tb.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	if err := store.Close(); err != nil {
		tb.Fatal(err)
	}
	return path
}

// openContentionDB copies seedPath into a fresh scratch file and opens it
// with the requested journal_mode and pool size. It never opens seedPath
// itself for writing.
func openContentionDB(tb testing.TB, seedPath, journal string, maxOpenConns int) *sql.DB {
	tb.Helper()
	scratch := filepath.Join(tb.TempDir(), "scratch.db")
	if out, err := exec.Command("cp", seedPath, scratch).CombinedOutput(); err != nil {
		tb.Fatalf("copy seed db: %v: %s", err, out)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(%s)&_pragma=synchronous(full)",
		scratch, journal)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		tb.Fatalf("open contention db: %v", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)
	tb.Cleanup(func() {
		_ = db.Close()
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = os.Remove(scratch + suffix)
		}
	})
	return db
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// BenchmarkReadContention answers decision 2's open question: how much read
// pool helps, and at what point it stops helping, under each journal mode.
func BenchmarkReadContention(b *testing.B) {
	seedPath := buildContentionSeedDB(b)
	const foregroundReps = 30

	for _, journal := range []string{"delete", "wal"} {
		for _, pool := range []int{1, 2, 4, 8} {
			for _, bgReaders := range []int{0, 4, 9} {
				name := fmt.Sprintf("journal=%s/pool=%d/bg=%d", journal, pool, bgReaders)
				b.Run(name, func(b *testing.B) {
					db := openContentionDB(b, seedPath, journal, pool)
					store := &Store{db: db}
					service := NewAdkSessionService(store)
					ctx := context.Background()

					var gotJournal string
					if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&gotJournal); err != nil {
						b.Fatal(err)
					}
					if !strings.EqualFold(gotJournal, journal) {
						b.Fatalf("journal_mode = %q, want %q (mode unsupported on this build?)", gotJournal, journal)
					}

					stop := make(chan struct{})
					var wg sync.WaitGroup
					var bgOps atomic.Int64
					for range bgReaders {
						wg.Go(func() {
							for {
								select {
								case <-stop:
									return
								default:
								}
								if _, err := service.LatestEventOrdinal(ctx, sessionReadBenchApp, sessionReadBenchUser, "turn-session"); err != nil {
									return
								}
								bgOps.Add(1)
							}
						})
					}

					// Warm the page cache before measuring, as the baseline
					// harness does, so this is the steady-state best case.
					if _, err := service.Get(ctx, &adksession.GetRequest{
						AppName: sessionReadBenchApp, UserID: sessionReadBenchUser, SessionID: "turn-session",
						NumRecentEvents: 128,
					}); err != nil {
						close(stop)
						wg.Wait()
						b.Fatal(err)
					}

					samples := make([]time.Duration, 0, foregroundReps)
					var lockWaitTotal time.Duration
					for range foregroundReps {
						start := time.Now()
						resp, err := service.Get(ctx, &adksession.GetRequest{
							AppName: sessionReadBenchApp, UserID: sessionReadBenchUser, SessionID: "turn-session",
							NumRecentEvents: 128,
						})
						elapsed := time.Since(start)
						if err != nil {
							close(stop)
							wg.Wait()
							b.Fatal(err)
						}
						if got := resp.Session.Events().Len(); got != 128 {
							close(stop)
							wg.Wait()
							b.Fatalf("loaded %d events, want 128: benchmark measured an empty or truncated read", got)
						}
						samples = append(samples, elapsed)
					}
					close(stop)
					wg.Wait()

					slices.Sort(samples)
					p50 := percentile(samples, 0.50)
					p95 := percentile(samples, 0.95)
					p99 := percentile(samples, 0.99)
					b.ReportMetric(float64(p50.Microseconds()), "p50-us")
					b.ReportMetric(float64(p95.Microseconds()), "p95-us")
					b.ReportMetric(float64(p99.Microseconds()), "p99-us")
					b.ReportMetric(float64(bgOps.Load()), "bg-ops")
					_ = lockWaitTotal // no per-statement lock-wait counter is exposed by database/sql; see report.
				})
			}
		}
	}
}
