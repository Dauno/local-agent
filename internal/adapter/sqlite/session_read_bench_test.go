package sqlite

// TRD 08 items 1 and 2 (docs/root-orchestrator-v2/hallazgos/worker-prompt-trd08-matriz-benchmarks.md):
// the read-per-turn curve at 100, 1,000, and 10,000 ADK events, and the
// retention sweep cost as a function of event count and candidate count.
//
// Repo convention: this file has no Test functions, so `go test ./...` pays
// only the compile cost. Corpus is bulk-inserted with prepared statements in
// one transaction per size point, seeded once per process (sync.Once) and
// shared across benchmarks, matching internal/integration/knowledge_retrieval_bench_test.go.
// b.ResetTimer/b.Loop() run only after the corpus is ready; nothing reseeds
// inside the timed loop.
//
// Content size follows the deployed-corpus shape recorded in the worker
// prompt: 2.67 KiB mean content per event. A synthetic corpus of 200-byte
// events measures a different thing.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

const (
	sessionReadBenchApp  = "bench-app"
	sessionReadBenchUser = "bench-user"
	// sessionReadBenchContentBytes approximates the 2.67 KiB mean content
	// size measured against the deployed database on 2026-08-19.
	sessionReadBenchContentBytes = 2700
)

var sessionReadBenchSizes = []int{100, 1_000, 10_000}

type sessionReadBenchCorpus struct {
	store   *Store
	service *AdkSessionService
	// sessionID by event count.
	sessionID map[int]string
}

var (
	sessionReadBenchOnce  sync.Once
	sessionReadBenchState sessionReadBenchCorpus
	sessionReadBenchErr   error
)

func sessionReadBench(tb testing.TB) *sessionReadBenchCorpus {
	tb.Helper()
	sessionReadBenchOnce.Do(func() {
		sessionReadBenchState, sessionReadBenchErr = seedSessionReadBenchCorpus()
	})
	if sessionReadBenchErr != nil {
		tb.Fatalf("seed session read benchmark corpus: %v", sessionReadBenchErr)
	}
	return &sessionReadBenchState
}

func seedSessionReadBenchCorpus() (sessionReadBenchCorpus, error) {
	fail := func(err error) (sessionReadBenchCorpus, error) {
		return sessionReadBenchCorpus{}, fmt.Errorf("seed session read benchmark corpus: %w", err)
	}
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "session-read-bench-")
	if err != nil {
		return fail(err)
	}
	store, err := Initialize(ctx, filepath.Join(dir, "bench.db"))
	if err != nil {
		return fail(err)
	}

	now := time.Now().UnixMicro()
	filler := strings.Repeat("x", sessionReadBenchContentBytes-40)
	sessionIDs := make(map[int]string, len(sessionReadBenchSizes))

	db := store.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO adk_app_state (app_name, state, update_time) VALUES (?, '{}', ?)`,
		sessionReadBenchApp, now); err != nil {
		store.Close()
		return fail(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO adk_user_state (app_name, user_id, state, update_time) VALUES (?, ?, '{}', ?)`,
		sessionReadBenchApp, sessionReadBenchUser, now); err != nil {
		store.Close()
		return fail(err)
	}
	for _, n := range sessionReadBenchSizes {
		sessionID := fmt.Sprintf("sess-%d", n)
		sessionIDs[n] = sessionID

		if _, err := db.ExecContext(ctx, `INSERT INTO adk_sessions
			(app_name, user_id, session_id, state, revision, create_time, update_time)
			VALUES (?, ?, ?, '{}', ?, ?, ?)`,
			sessionReadBenchApp, sessionReadBenchUser, sessionID, n, now, now); err != nil {
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
				sessionReadBenchApp, sessionReadBenchUser, sessionID, i, "bench-invocation", "model",
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

	return sessionReadBenchCorpus{store: store, service: NewAdkSessionService(store), sessionID: sessionIDs}, nil
}

// BenchmarkSessionGetUnbounded measures the Get path used by
// updateContinuity, RecoverActivation, and the ensureSession fallback: no
// NumRecentEvents bound, so it loads and decodes the entire session.
func BenchmarkSessionGetUnbounded(b *testing.B) {
	corpus := sessionReadBench(b)
	for _, n := range sessionReadBenchSizes {
		sessionID := corpus.sessionID[n]
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			ctx := context.Background()
			b.ReportMetric(float64(n), "events")
			for b.Loop() {
				resp, err := corpus.service.Get(ctx, &adksession.GetRequest{
					AppName: sessionReadBenchApp, UserID: sessionReadBenchUser, SessionID: sessionID,
				})
				if err != nil {
					b.Fatal(err)
				}
				if got := resp.Session.Events().Len(); got != n {
					b.Fatalf("loaded %d events, corpus has %d: benchmark measured an empty or truncated read", got, n)
				}
			}
		})
	}
}

// BenchmarkSessionGetBounded measures the Get path the ADK runner uses:
// NumRecentEvents capped at domain.MaxContextEpochRange (128).
func BenchmarkSessionGetBounded(b *testing.B) {
	corpus := sessionReadBench(b)
	for _, n := range sessionReadBenchSizes {
		sessionID := corpus.sessionID[n]
		want := domain.MaxContextEpochRange
		if n < want {
			want = n
		}
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			ctx := context.Background()
			b.ReportMetric(float64(n), "events")
			for b.Loop() {
				resp, err := corpus.service.Get(ctx, &adksession.GetRequest{
					AppName: sessionReadBenchApp, UserID: sessionReadBenchUser, SessionID: sessionID,
					NumRecentEvents: domain.MaxContextEpochRange,
				})
				if err != nil {
					b.Fatal(err)
				}
				if got := resp.Session.Events().Len(); got != want {
					b.Fatalf("loaded %d events, want %d: benchmark measured an empty or truncated read", got, want)
				}
			}
		})
	}
}

// BenchmarkSessionLatestEventOrdinal measures the cheap session-head lookup
// that risk 2 in the TRD says already exists and goes unused on the
// updateContinuity path.
func BenchmarkSessionLatestEventOrdinal(b *testing.B) {
	corpus := sessionReadBench(b)
	for _, n := range sessionReadBenchSizes {
		sessionID := corpus.sessionID[n]
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			ctx := context.Background()
			b.ReportMetric(float64(n), "events")
			for b.Loop() {
				head, err := corpus.service.LatestEventOrdinal(ctx, sessionReadBenchApp, sessionReadBenchUser, sessionID)
				if err != nil {
					b.Fatal(err)
				}
				if head != int64(n-1) {
					b.Fatalf("head ordinal = %d, want %d: benchmark measured the wrong session", head, n-1)
				}
			}
		})
	}
}

// --- Item 2: the retention sweep, as a function of event count and
// candidate count. Store.IsRecoverableResultReferenced is called once per
// candidate by recoverableresult/store.go, so the sweep cost is candidates
// times the per-candidate instr-scan cost, which itself grows with the size
// of adk_events.content and continuity_capsules.capsule_json.
//
// The full product (10,000 events x thousands of candidates, matching the
// deployed 7,626 live candidates) is not run here: that is the documented
// cap from the worker prompt. This grid holds candidates <= 300, enough to
// show the two-variable shape without an hours-long run.

var retentionBenchEvents = []int{100, 1_000, 10_000}
var retentionBenchCandidates = []int{10, 100, 300}

type retentionBenchCorpus struct {
	store *Store
	refs  map[int][]string // candidate count -> unreferenced candidate refs
}

var (
	retentionBenchCache = map[int]*retentionBenchCorpus{}
	retentionBenchMu    sync.Mutex
)

func retentionBench(tb testing.TB, events int) *retentionBenchCorpus {
	tb.Helper()
	retentionBenchMu.Lock()
	defer retentionBenchMu.Unlock()
	if corpus, ok := retentionBenchCache[events]; ok {
		return corpus
	}
	corpus, err := seedRetentionBenchCorpus(events)
	if err != nil {
		tb.Fatalf("seed retention benchmark corpus at %d events: %v", events, err)
	}
	retentionBenchCache[events] = corpus
	return corpus
}

func seedRetentionBenchCorpus(events int) (*retentionBenchCorpus, error) {
	fail := func(err error) (*retentionBenchCorpus, error) {
		return nil, fmt.Errorf("seed retention benchmark corpus at %d events: %w", events, err)
	}
	ctx := context.Background()
	dir, err := os.MkdirTemp("", fmt.Sprintf("retention-bench-%d-", events))
	if err != nil {
		return fail(err)
	}
	store, err := Initialize(ctx, filepath.Join(dir, "bench.db"))
	if err != nil {
		return fail(err)
	}

	now := time.Now().UnixMicro()
	filler := strings.Repeat("x", sessionReadBenchContentBytes-40)
	sessionID := "retention-sess"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO adk_sessions
		(app_name, user_id, session_id, state, revision, create_time, update_time)
		VALUES (?, ?, ?, '{}', ?, ?, ?)`,
		sessionReadBenchApp, sessionReadBenchUser, sessionID, events, now, now); err != nil {
		store.Close()
		return fail(err)
	}

	tx, err := store.DB().BeginTx(ctx, nil)
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
	for i := 0; i < events; i++ {
		// No candidate ref ever appears in event content: this is the
		// worst-case, and also the common case, "not referenced" branch,
		// which forces a full instr scan with no early match.
		content := fmt.Sprintf(`{"role":"model","parts":[{"text":"event %d %s"}]}`, i, filler)
		if _, err := stmt.ExecContext(ctx, fmt.Sprintf("%s-evt-%d", sessionID, i),
			sessionReadBenchApp, sessionReadBenchUser, sessionID, i, "bench-invocation", "model",
			now+int64(i), content); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			store.Close()
			return fail(err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		store.Close()
		return fail(err)
	}
	if err := tx.Commit(); err != nil {
		store.Close()
		return fail(err)
	}

	maxCandidates := retentionBenchCandidates[len(retentionBenchCandidates)-1]
	refs := make([]string, maxCandidates)
	createdAt := time.Now().Unix()
	candidateTx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		store.Close()
		return fail(err)
	}
	candidateStmt, err := candidateTx.PrepareContext(ctx, `INSERT INTO recoverable_results
		(ref, actor, conversation_key, kind, storage_locator, size_bytes, code_points, sha256, created_at, expires_at)
		VALUES (?, 'bench-actor', 'slack:T1:dm:D1', 'bench', ?, 1, 1, ?, ?, ?)`)
	if err != nil {
		_ = candidateTx.Rollback()
		store.Close()
		return fail(err)
	}
	for i := 0; i < maxCandidates; i++ {
		ref := fmt.Sprintf("recoverable-ref-%d-%d", events, i)
		refs[i] = ref
		if _, err := candidateStmt.ExecContext(ctx, ref, "bench/"+ref, strings.Repeat("0", 64), createdAt, createdAt+3600); err != nil {
			_ = candidateStmt.Close()
			_ = candidateTx.Rollback()
			store.Close()
			return fail(fmt.Errorf("insert candidate %d: %w", i, err))
		}
	}
	if err := candidateStmt.Close(); err != nil {
		_ = candidateTx.Rollback()
		store.Close()
		return fail(err)
	}
	if err := candidateTx.Commit(); err != nil {
		store.Close()
		return fail(err)
	}

	byCandidateCount := make(map[int][]string, len(retentionBenchCandidates))
	for _, c := range retentionBenchCandidates {
		byCandidateCount[c] = refs[:c]
	}
	return &retentionBenchCorpus{store: store, refs: byCandidateCount}, nil
}

func BenchmarkRetentionSweep(b *testing.B) {
	for _, events := range retentionBenchEvents {
		corpus := retentionBench(b, events)
		for _, candidates := range retentionBenchCandidates {
			refs := corpus.refs[candidates]
			b.Run(fmt.Sprintf("events=%d/candidates=%d", events, candidates), func(b *testing.B) {
				ctx := context.Background()
				b.ReportMetric(float64(events), "events")
				b.ReportMetric(float64(candidates), "candidates")
				for b.Loop() {
					for _, ref := range refs {
						referenced, err := corpus.store.IsRecoverableResultReferenced(ctx, ref)
						if err != nil {
							b.Fatal(err)
						}
						if referenced {
							b.Fatal("benchmark ref unexpectedly reported referenced")
						}
					}
				}
			})
		}
	}
}
