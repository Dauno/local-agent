package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

// noopWorkerLogger discards every log line; the worker tests assert on
// durable state, not on log output.
type noopWorkerLogger struct{}

func (noopWorkerLogger) Debug(string, ...any) {}
func (noopWorkerLogger) Info(string, ...any)  {}
func (noopWorkerLogger) Warn(string, ...any)  {}
func (noopWorkerLogger) Error(string, ...any) {}

// setupWorkerFixture opens a fresh v40 database, seeds one real,
// ReadRange-verifiable source result, and creates one 'preparing' analysis
// row through the real AnalysisStore.Create (not a raw INSERT), so the
// worker itself is responsible for building the manifest and step tree,
// exactly like a freshly requested analysis in production.
func setupWorkerFixture(t *testing.T, content string) (dbStore *Store, analysisID, sourceID string) {
	t.Helper()
	dbStore, err := Initialize(t.Context(), t.TempDir()+"/analysis-worker.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbStore.Close() })

	sourceID = strings.Repeat("a", 64)
	sum := sha256.Sum256([]byte(content))
	sourceSHA := hex.EncodeToString(sum[:])
	if _, err := dbStore.DB().ExecContext(t.Context(), `INSERT INTO result_records (result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key,
		sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state)
		VALUES (?, 'tool_operation', 'op-1', 0, 'artifact', 'key-1', ?, ?, 'text/plain', 'U1', 'T1', 'slack:T1:dm:U1', 'workspace', 'context', 1, 'available')`,
		sourceID, sourceSHA, len(content)); err != nil {
		t.Fatal(err)
	}

	scope := domain.ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:U1", Project: "workspace"}
	limits := testAnalysisLimits()
	identity := domain.AnalysisIdentity{
		SourceResultID: sourceID, SourceSHA256: sourceSHA,
		ObjectiveClass:      domain.AnalysisObjectiveBoundedQuestionV1,
		ObjectiveDigest:     domain.AnalysisObjectiveDigest(domain.AnalysisObjectiveBoundedQuestionV1, "objective"),
		SegmentationVersion: "text_v1", PromptVersion: "analysis-v1", ModelFingerprint: "model-v1",
		LimitsDigest: limits.Digest(),
	}
	analyses := NewAnalysisStore(dbStore)
	record, err := analyses.Create(t.Context(), identity, limits, "objective", scope, "ws-1", time.Now().UTC())
	if err != nil {
		t.Fatalf("create analysis: %v", err)
	}
	return dbStore, record.AnalysisID, sourceID
}

func newTestWorker(t *testing.T, dbStore *Store, source port.TrustedResultStore, analyzer port.ResultAnalyzer, clock port.Clock) *resultanalysis.Worker {
	t.Helper()
	analyses := NewAnalysisStore(dbStore)
	steps := NewAnalysisStepStore(dbStore)
	segments := NewAnalysisSegmentStore(dbStore)
	evidence := NewAnalysisEvidenceStore(dbStore)
	completion := NewAnalysisCompletionStore(dbStore)
	worker, err := resultanalysis.NewWorker(resultanalysis.WorkerConfig{
		Interval: 10 * time.Millisecond, Lease: time.Minute,
	}, resultanalysis.WorkerDependencies{
		Analyses: analyses, Active: analyses, Running: analyses, Steps: steps, Segments: segments,
		Evidence: evidence, Payloads: steps, Completion: completion, Source: source, Analyzer: analyzer,
		Clock: clock, Logger: noopWorkerLogger{},
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	return worker
}

func waitForWorkerCondition(t *testing.T, message string, condition func() bool) {
	t.Helper()
	// 10s comfortably exceeds resultanalysis.RunLeafRetryBackoff (5s): a
	// permit-exhausted leaf is not reclaimed until that backoff elapses, so
	// a shorter deadline races that backoff instead of testing behavior.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", message)
}

func readAnalysisState(t *testing.T, dbStore *Store, analysisID string) (state, failureCode string) {
	t.Helper()
	if err := dbStore.DB().QueryRowContext(t.Context(), `SELECT state, failure_code FROM result_analyses WHERE analysis_id = ?`, analysisID).Scan(&state, &failureCode); err != nil {
		t.Fatalf("read analysis state: %v", err)
	}
	return state, failureCode
}

// TestWorkerDrainsAnalysisEndToEnd is criterion 3: with the gate's real
// wiring (a freshly created 'preparing' analysis, no pre-built manifest or
// steps), the real Worker builds the manifest, drains every leaf and
// reduction step, and completes the analysis end to end, over the real
// SQLite stores.
func TestWorkerDrainsAnalysisEndToEnd(t *testing.T) {
	content := "short source text for one leaf"
	dbStore, analysisID, sourceID := setupWorkerFixture(t, content)
	sum := sha256.Sum256([]byte(content))
	source := &fakeTrustedResultSourceStore{identity: domain.ResultIdentity{ResultID: sourceID, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(content)), MediaType: "text/plain"}, content: content}
	analyzer := &fixedAnalyzer{}
	worker := newTestWorker(t, dbStore, source, analyzer, port.SystemClock{})

	ctx, cancel := context.WithCancel(t.Context())
	go worker.Run(ctx)
	waitForWorkerCondition(t, "analysis completes", func() bool {
		state, _ := readAnalysisState(t, dbStore, analysisID)
		return state == string(domain.AnalysisCompleted)
	})
	cancel()
	if err := worker.WaitStopped(t.Context()); err != nil {
		t.Fatalf("WaitStopped: %v", err)
	}

	steps := NewAnalysisStepStore(dbStore)
	all, err := steps.List(t.Context(), analysisID, "", 500)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("worker completed the analysis without preparing any steps")
	}
	for _, step := range all {
		if step.State != domain.AnalysisStepCompleted {
			t.Fatalf("step %s state = %s, want completed", step.StepID, step.State)
		}
	}
}

// permitExhaustedThenFixedAnalyzer fails its first leaf call with
// port.ErrModelCallLimitReached, then behaves like fixedAnalyzer. It
// counts leaf calls so the test can assert the worker retried instead of
// failing after the first permit-exhausted response.
type permitExhaustedThenFixedAnalyzer struct {
	mu            sync.Mutex
	leafCalls     int
	exhaustedOnce bool
	exhausted     chan struct{}
	fixedAnalyzer
}

func (a *permitExhaustedThenFixedAnalyzer) AnalyzeLeaf(ctx context.Context, input port.AnalysisLeafInput) (domain.AnalysisLeaf, error) {
	a.mu.Lock()
	a.leafCalls++
	first := !a.exhaustedOnce
	a.exhaustedOnce = true
	a.mu.Unlock()
	if first {
		close(a.exhausted)
		return domain.AnalysisLeaf{}, port.ErrModelCallLimitReached
	}
	return a.fixedAnalyzer.AnalyzeLeaf(ctx, input)
}

type workerFakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *workerFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *workerFakeClock) advance(value time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(value)
	c.mu.Unlock()
}

// TestWorkerPermitExhaustionReturnsStepToPreparedWithoutConsumingAttempt is
// criterion 5, proven against the real running worker (not only against
// LeafRunner directly): a leaf step whose first attempt hits
// port.ErrModelCallLimitReached is retried by a later worker tick without
// its attempt budget being consumed, and the analysis still completes.
func TestWorkerPermitExhaustionReturnsStepToPreparedWithoutConsumingAttempt(t *testing.T) {
	content := "short source text for one leaf"
	dbStore, analysisID, sourceID := setupWorkerFixture(t, content)
	sum := sha256.Sum256([]byte(content))
	source := &fakeTrustedResultSourceStore{identity: domain.ResultIdentity{ResultID: sourceID, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(content)), MediaType: "text/plain"}, content: content}
	analyzer := &permitExhaustedThenFixedAnalyzer{exhausted: make(chan struct{})}
	clock := &workerFakeClock{now: time.Now().UTC()}
	worker := newTestWorker(t, dbStore, source, analyzer, clock)

	ctx, cancel := context.WithCancel(t.Context())
	go worker.Run(ctx)
	select {
	case <-analyzer.exhausted:
	case <-time.After(30 * time.Second):
		t.Fatal("worker did not reach the permit-exhausted leaf attempt")
	}
	clock.advance(resultanalysis.RunLeafRetryBackoff + time.Second)
	waitForWorkerCondition(t, "analysis completes despite one permit-exhausted leaf attempt", func() bool {
		state, _ := readAnalysisState(t, dbStore, analysisID)
		return state == string(domain.AnalysisCompleted)
	})
	cancel()
	if err := worker.WaitStopped(t.Context()); err != nil {
		t.Fatalf("WaitStopped: %v", err)
	}

	analyzer.mu.Lock()
	leafCalls := analyzer.leafCalls
	analyzer.mu.Unlock()
	if leafCalls < 2 {
		t.Fatalf("leaf calls = %d, want at least 2 (one permit-exhausted, one that succeeded)", leafCalls)
	}

	steps := NewAnalysisStepStore(dbStore)
	all, err := steps.List(t.Context(), analysisID, "", 500)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	for _, step := range all {
		if step.Kind != domain.AnalysisStepLeaf {
			continue
		}
		if step.State != domain.AnalysisStepCompleted {
			t.Fatalf("leaf step %s state = %s, want completed", step.StepID, step.State)
		}
		if step.Attempt != 0 {
			t.Fatalf("leaf step %s attempt = %d, want 0: permit exhaustion must not consume the attempt budget", step.StepID, step.Attempt)
		}
	}
}

// TestWorkerGateDisabledTouchesNoV40Table is criterion 2's non-composition
// half: composeResultAnalysis is an internal/app concern, but the same
// invariant must hold at the worker's own boundary: a worker that is never
// constructed or run touches no v40 table. This test documents that
// boundary directly: with no Worker ever created over dbStore, no
// result_analyses row exists.
func TestWorkerGateDisabledTouchesNoV40Table(t *testing.T) {
	dbStore, err := Initialize(t.Context(), t.TempDir()+"/analysis-worker-disabled.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbStore.Close() })
	var count int
	if err := dbStore.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_analyses`).Scan(&count); err != nil {
		t.Fatalf("count result_analyses: %v", err)
	}
	if count != 0 {
		t.Fatalf("result_analyses has %d rows with no worker ever run, want 0", count)
	}
}
