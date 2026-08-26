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
	"crypto/sha256"
	"encoding/hex"
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
		_ = store.Close()
		return fail(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO adk_user_state (app_name, user_id, state, update_time) VALUES (?, ?, '{}', ?)`,
		sessionReadBenchApp, sessionReadBenchUser, now); err != nil {
		_ = store.Close()
		return fail(err)
	}
	for _, n := range sessionReadBenchSizes {
		sessionID := fmt.Sprintf("sess-%d", n)
		sessionIDs[n] = sessionID

		if _, err := db.ExecContext(ctx, `INSERT INTO adk_sessions
			(app_name, user_id, session_id, state, revision, create_time, update_time)
			VALUES (?, ?, ?, '{}', ?, ?, ?)`,
			sessionReadBenchApp, sessionReadBenchUser, sessionID, n, now, now); err != nil {
			_ = store.Close()
			return fail(fmt.Errorf("insert session %s: %w", sessionID, err))
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			_ = store.Close()
			return fail(err)
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO adk_events
			(id, app_name, user_id, session_id, ordinal, invocation_id, author, actions, timestamp, content, partial, turn_complete, interrupted)
			VALUES (?, ?, ?, ?, ?, ?, ?, '{}', ?, ?, 0, 1, 0)`)
		if err != nil {
			_ = tx.Rollback()
			_ = store.Close()
			return fail(err)
		}
		for i := range n {
			content := fmt.Sprintf(`{"role":"model","parts":[{"text":"event %d %s"}]}`, i, filler)
			if _, err := stmt.ExecContext(ctx, fmt.Sprintf("%s-evt-%d", sessionID, i),
				sessionReadBenchApp, sessionReadBenchUser, sessionID, i, "bench-invocation", "model",
				now+int64(i), content); err != nil {
				_ = stmt.Close()
				_ = tx.Rollback()
				_ = store.Close()
				return fail(fmt.Errorf("insert event %d for session %s: %w", i, sessionID, err))
			}
		}
		if err := stmt.Close(); err != nil {
			_ = tx.Rollback()
			_ = store.Close()
			return fail(err)
		}
		if err := tx.Commit(); err != nil {
			_ = store.Close()
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
			// b.Loop's first call runs ResetTimer, which clears any metric
			// reported before it (FIND-113): report after the loop instead.
			b.ReportMetric(float64(n), "events")
		})
	}
}

// BenchmarkSessionGetBounded measures the Get path the ADK runner uses:
// NumRecentEvents capped at domain.MaxContextEpochRange (128).
func BenchmarkSessionGetBounded(b *testing.B) {
	corpus := sessionReadBench(b)
	for _, n := range sessionReadBenchSizes {
		sessionID := corpus.sessionID[n]
		want := min(n, domain.MaxContextEpochRange)
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			ctx := context.Background()
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
			b.ReportMetric(float64(n), "events")
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
			for b.Loop() {
				head, err := corpus.service.LatestEventOrdinal(ctx, sessionReadBenchApp, sessionReadBenchUser, sessionID)
				if err != nil {
					b.Fatal(err)
				}
				if head != int64(n-1) {
					b.Fatalf("head ordinal = %d, want %d: benchmark measured the wrong session", head, n-1)
				}
			}
			b.ReportMetric(float64(n), "events")
		})
	}
}

// --- Item 2: the retention sweep, as a function of event count and
// candidate count. Store.IsRecoverableResultReferenced is called once per
// candidate by recoverableresult/store.go. Through TRD 08 checkpoint 2 this
// cost was candidates times a per-candidate instr-scan of adk_events.content
// and continuity_capsules.capsule_json, growing with event count. Checkpoint
// 3 (docs/root-orchestrator-v2/hallazgos/worker-prompt-trd08-checkpoint3.md)
// replaced that scan with a lookup against the indexed relation
// recoverable_result_refs, so what this benchmark now measures is that
// index lookup, against a populated index (FIND-114): candidate refs are
// valid 64-character lowercase hex (recoverable_result_refs rejects
// anything else by CHECK), and both branches IsRecoverableResultReferenced
// can take are exercised, not just the absent one.
//
// recoverable_result_refs rows here are inserted directly rather than
// through AppendEvent: seeding maxCandidates present refs through the real
// write path, once per event-count corpus, added enough setup time to not
// be worth it, and the write path itself is already exercised end to end
// by TestAppendEventIndexesEmbeddedRefWithoutNarrowingDetection in
// recoverable_reference_index_test.go. What this benchmark needs is an
// index with real rows to search, which a direct insert gives as
// faithfully as an indirect one for that purpose.
//
// The full product (10,000 events x thousands of candidates, matching the
// deployed 7,626 live candidates) is not run here: that is the documented
// cap from the worker prompt. This grid holds candidates <= 300, enough to
// show the shape without an hours-long run.

var (
	retentionBenchEvents     = []int{100, 1_000, 10_000}
	retentionBenchCandidates = []int{10, 100, 300}
)

// retentionBenchRef derives a valid 64-character lowercase hex ref from a
// label, so seeding code can build distinct, deterministic, schema-valid
// refs without a random source.
func retentionBenchRef(label string) string {
	digest := sha256.Sum256([]byte(label))
	return hex.EncodeToString(digest[:])
}

type retentionBenchCorpus struct {
	store *Store
	// absentRefs candidates have a recoverable_results row but no
	// recoverable_result_refs row: the branch retention pays for, since an
	// absent result is the one it deletes.
	absentRefs map[int][]string
	// presentRefs candidates have both rows: the branch that must report
	// referenced=true and so must never be deleted.
	presentRefs map[int][]string
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
		_ = store.Close()
		return fail(err)
	}

	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		_ = store.Close()
		return fail(err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO adk_events
		(id, app_name, user_id, session_id, ordinal, invocation_id, author, actions, timestamp, content, partial, turn_complete, interrupted)
		VALUES (?, ?, ?, ?, ?, ?, ?, '{}', ?, ?, 0, 1, 0)`)
	if err != nil {
		_ = tx.Rollback()
		_ = store.Close()
		return fail(err)
	}
	for i := range events {
		// adk_events content no longer matters to
		// IsRecoverableResultReferenced's cost after checkpoint 3 (it reads
		// recoverable_result_refs only), but this table is still populated
		// at each events size so the "events" axis stays meaningful to
		// compare against: it should now show no effect on ns/op, which is
		// itself the result checkpoint 3 set out to prove.
		content := fmt.Sprintf(`{"role":"model","parts":[{"text":"event %d %s"}]}`, i, filler)
		if _, err := stmt.ExecContext(ctx, fmt.Sprintf("%s-evt-%d", sessionID, i),
			sessionReadBenchApp, sessionReadBenchUser, sessionID, i, "bench-invocation", "model",
			now+int64(i), content); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			_ = store.Close()
			return fail(err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		_ = store.Close()
		return fail(err)
	}
	if err := tx.Commit(); err != nil {
		_ = store.Close()
		return fail(err)
	}

	maxCandidates := retentionBenchCandidates[len(retentionBenchCandidates)-1]
	absentRefs := make([]string, maxCandidates)
	presentRefs := make([]string, maxCandidates)
	createdAt := time.Now().Unix()
	candidateTx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		_ = store.Close()
		return fail(err)
	}
	resultStmt, err := candidateTx.PrepareContext(ctx, `INSERT INTO recoverable_results
		(ref, actor, conversation_key, kind, storage_locator, size_bytes, code_points, sha256, created_at, expires_at)
		VALUES (?, 'bench-actor', 'slack:T1:dm:D1', 'bench', ?, 1, 1, ?, ?, ?)`)
	if err != nil {
		_ = candidateTx.Rollback()
		_ = store.Close()
		return fail(err)
	}
	// See the comment above retentionBenchEvents: recoverable_result_refs is
	// seeded directly here, not through AppendEvent.
	indexStmt, err := candidateTx.PrepareContext(ctx, `INSERT INTO recoverable_result_refs
		(ref, owner_kind, owner_id, created_at) VALUES (?, 'adk_event', ?, ?)`)
	if err != nil {
		_ = candidateTx.Rollback()
		_ = store.Close()
		return fail(err)
	}
	for i := range maxCandidates {
		absentRef := retentionBenchRef(fmt.Sprintf("absent-%d-%d", events, i))
		absentRefs[i] = absentRef
		if _, err := resultStmt.ExecContext(ctx, absentRef, "bench/"+absentRef, strings.Repeat("0", 64), createdAt, createdAt+3600); err != nil {
			_ = indexStmt.Close()
			_ = resultStmt.Close()
			_ = candidateTx.Rollback()
			_ = store.Close()
			return fail(fmt.Errorf("insert absent candidate %d: %w", i, err))
		}

		presentRef := retentionBenchRef(fmt.Sprintf("present-%d-%d", events, i))
		presentRefs[i] = presentRef
		if _, err := resultStmt.ExecContext(ctx, presentRef, "bench/"+presentRef, strings.Repeat("0", 64), createdAt, createdAt+3600); err != nil {
			_ = indexStmt.Close()
			_ = resultStmt.Close()
			_ = candidateTx.Rollback()
			_ = store.Close()
			return fail(fmt.Errorf("insert present candidate %d: %w", i, err))
		}
		ownerID := fmt.Sprintf("bench-owner-%d-%d", events, i)
		if _, err := indexStmt.ExecContext(ctx, presentRef, ownerID, createdAt); err != nil {
			_ = indexStmt.Close()
			_ = resultStmt.Close()
			_ = candidateTx.Rollback()
			_ = store.Close()
			return fail(fmt.Errorf("index present candidate %d: %w", i, err))
		}
	}
	if err := indexStmt.Close(); err != nil {
		_ = resultStmt.Close()
		_ = candidateTx.Rollback()
		_ = store.Close()
		return fail(err)
	}
	if err := resultStmt.Close(); err != nil {
		_ = candidateTx.Rollback()
		_ = store.Close()
		return fail(err)
	}
	if err := candidateTx.Commit(); err != nil {
		_ = store.Close()
		return fail(err)
	}

	absentByCandidateCount := make(map[int][]string, len(retentionBenchCandidates))
	presentByCandidateCount := make(map[int][]string, len(retentionBenchCandidates))
	for _, c := range retentionBenchCandidates {
		absentByCandidateCount[c] = absentRefs[:c]
		presentByCandidateCount[c] = presentRefs[:c]
	}
	return &retentionBenchCorpus{store: store, absentRefs: absentByCandidateCount, presentRefs: presentByCandidateCount}, nil
}

func BenchmarkRetentionSweep(b *testing.B) {
	for _, events := range retentionBenchEvents {
		corpus := retentionBench(b, events)

		// FIND-114: the absent branch is only a meaningful measurement if
		// the index it searches actually holds rows; an empty index would
		// make "not found" trivially fast and indistinguishable from a
		// working index that legitimately found nothing. This check runs
		// once per events size, outside any timed loop.
		var indexRows int
		if err := corpus.store.DB().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM recoverable_result_refs`).Scan(&indexRows); err != nil {
			b.Fatal(err)
		}
		if indexRows == 0 {
			b.Fatal("recoverable_result_refs is empty: this benchmark cannot measure the indexed lookup path against nothing")
		}

		for _, candidates := range retentionBenchCandidates {
			absentRefs := corpus.absentRefs[candidates]
			b.Run(fmt.Sprintf("events=%d/candidates=%d/absent", events, candidates), func(b *testing.B) {
				ctx := context.Background()
				for b.Loop() {
					for _, ref := range absentRefs {
						referenced, err := corpus.store.IsRecoverableResultReferenced(ctx, ref)
						if err != nil {
							b.Fatal(err)
						}
						if referenced {
							b.Fatal("absent benchmark ref unexpectedly reported referenced")
						}
					}
				}
				b.ReportMetric(float64(events), "events")
				b.ReportMetric(float64(candidates), "candidates")
				b.ReportMetric(float64(indexRows), "index_rows")
			})

			presentRefs := corpus.presentRefs[candidates]
			b.Run(fmt.Sprintf("events=%d/candidates=%d/present", events, candidates), func(b *testing.B) {
				ctx := context.Background()
				for b.Loop() {
					for _, ref := range presentRefs {
						referenced, err := corpus.store.IsRecoverableResultReferenced(ctx, ref)
						if err != nil {
							b.Fatal(err)
						}
						if !referenced {
							b.Fatal("present benchmark ref unexpectedly reported not referenced")
						}
					}
				}
				b.ReportMetric(float64(events), "events")
				b.ReportMetric(float64(candidates), "candidates")
				b.ReportMetric(float64(indexRows), "index_rows")
			})
		}
	}
}
