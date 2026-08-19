package app

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/fsartifact"
	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
	resultanalysisusecase "github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

// resultAnalysisRestartModels builds runtimeModels bound to a specific,
// caller-owned payload backend, so every phase of the restart test can
// point at the same on-disk artifact directory instead of each getting an
// isolated one.
func resultAnalysisRestartModels(payloads port.ResultPayloadStore) runtimeModels {
	return runtimeModels{
		rootModel: fakeRootLLM{}, rootFamily: domain.ProviderFamilyOpenAICompatible,
		resultPayloadStore: payloads,
		redactor:           secure.NewRedactor("sk-analysis-secret-value"),
		logger:             logging.New(io.Discard, "error", secure.NewRedactor("sk-analysis-secret-value")),
	}
}

// resultAnalysisRestartTestConfig is the shared shape for every phase of
// TestResultAnalysisGateRestartRollbackSequence: a small segment size
// forces the tiny fixture source into more than one leaf, and a 1s worker
// interval keeps the test fast without racing the fixed
// resultanalysis.RunLeafRetryBackoff used elsewhere.
func resultAnalysisRestartTestConfig(enabled bool) config.Config {
	cfg := config.Default()
	cfg.Orchestration.ResultAnalysis.Enabled = enabled
	cfg.Orchestration.ResultAnalysis.MaxSegmentBytes = 12
	cfg.Orchestration.ResultAnalysis.MaxLeaves = 8
	cfg.Orchestration.ResultAnalysis.WorkerIntervalSeconds = 1
	cfg.Model.Name = "main-model-v1"
	return cfg
}

// blockingSecondLeafAnalyzer behaves like fixedAnalyzer for the first
// segment it sees (ordinal 0), producing a real finding and one real
// evidence selector, but blocks every other leaf call until its context is
// cancelled. It simulates an analysis interrupted mid-drain: leaf-0's
// completion (and its evidence) is real durable state; the rest of the
// tree is still pending when the process "crashes".
type blockingSecondLeafAnalyzer struct {
	fixedAnalyzer
}

func (a *blockingSecondLeafAnalyzer) AnalyzeLeaf(ctx context.Context, input port.AnalysisLeafInput) (domain.AnalysisLeaf, error) {
	if input.SegmentOrdinal == 0 {
		leaf, err := a.fixedAnalyzer.AnalyzeLeaf(ctx, input)
		if err != nil {
			return leaf, err
		}
		leaf.EvidenceSelectors = []domain.AnalysisByteRange{{OffsetBytes: 0, LengthBytes: 1}}
		return leaf, nil
	}
	<-ctx.Done()
	return domain.AnalysisLeaf{}, ctx.Err()
}

// fixedAnalyzer/fakeRootLLM-style helpers already exist in sqlite package
// test files but are unexported there; this local copy keeps this file
// self-contained without an adapter-internal-test import.
type fixedAnalyzer struct{}

func (fixedAnalyzer) AnalyzeLeaf(_ context.Context, input port.AnalysisLeafInput) (domain.AnalysisLeaf, error) {
	return domain.AnalysisLeaf{
		ObjectiveClass:  input.ObjectiveClass,
		ObjectiveDigest: domain.AnalysisObjectiveDigest(input.ObjectiveClass, input.ObjectiveText),
		SegmentOrdinal:  input.SegmentOrdinal,
		Findings:        []domain.AnalysisStatement{{Text: fmt.Sprintf("finding for segment %d", input.SegmentOrdinal)}},
	}, nil
}

func (fixedAnalyzer) Reduce(_ context.Context, input port.AnalysisReductionInput) (domain.AnalysisPacket, error) {
	packet := domain.AnalysisPacket{
		ObjectiveClass:  input.ObjectiveClass,
		ObjectiveDigest: domain.AnalysisObjectiveDigest(input.ObjectiveClass, input.ObjectiveText),
		Findings:        []domain.AnalysisStatement{{Text: "combined finding"}},
	}
	for _, child := range input.Children {
		packet.Lineage = append(packet.Lineage, child.StepID)
	}
	return packet, nil
}

// dumpResultAnalysisV40Tables renders every row of every v40 table TRD 07
// owns, in a fixed column and row order, as one deterministic string. Two
// dumps taken before and after a disabled-gate boot are compared byte for
// byte: a row-count comparison alone would miss a column silently mutated
// in place, which is exactly the gap the TRD 06 sub-round 6d precedent
// exists to close.
func dumpResultAnalysisV40Tables(t *testing.T, store *adaptersqlite.Store) string {
	t.Helper()
	tables := []struct {
		name    string
		columns string
		order   string
	}{
		{"result_analyses", "analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest, objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json, actor, team_id, conversation_key, project, workstream_id, state, failure_code, created_at, updated_at", "analysis_id"},
		{"analysis_segments", "analysis_id, ordinal, offset_bytes, length_bytes, sha256, segmenter_version, overlap_prev_bytes", "analysis_id, ordinal"},
		{"analysis_steps", "analysis_id, step_id, kind, state, attempt, generation, next_attempt, lease_until, segment_ordinal, failure_code, output_digest, output_payload, created_at, updated_at", "analysis_id, step_id"},
		{"analysis_step_children", "analysis_id, parent_step_id, ordinal, child_step_id", "analysis_id, parent_step_id, ordinal"},
		{"analysis_evidence", "evidence_id, analysis_id, leaf_step_id, segment_ordinal, offset_bytes, length_bytes, sha256, excerpt_bytes, created_at", "evidence_id"},
		{"analysis_bundles", "bundle_id, analysis_id, actor, team_id, conversation_key, project, state, content_sha256, content_bytes, created_at", "bundle_id"},
	}
	dump := ""
	for _, table := range tables {
		rows, err := store.DB().QueryContext(t.Context(), fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s`, table.columns, table.name, table.order))
		if err != nil {
			t.Fatalf("dump %s: %v", table.name, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatalf("dump %s columns: %v", table.name, err)
		}
		for rows.Next() {
			values := make([]any, len(cols))
			pointers := make([]any, len(cols))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatalf("scan %s row: %v", table.name, err)
			}
			dump += fmt.Sprintf("%s:%v\n", table.name, values)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("iterate %s: %v", table.name, err)
		}
		rows.Close()
	}
	return dump
}

func waitForRestartCondition(t *testing.T, message string, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", message)
}

// TestResultAnalysisGateRestartRollbackSequence is checkpoint 6's central
// criterion: four real process restarts over one SQLite store, proving the
// gate never destroys v40 data and that re-enabling it resumes an
// interrupted analysis to completion. It is modelled directly on TRD 06
// sub-round 6d (internal/app/knowledge_retrieval_test.go,
// TestKnowledgeRetrievalRestartRollbackSequence): the comparison after the
// disabled-gate boot is byte for byte over every v40 table, not a row
// count.
//
//  1. gate off, boot, shutdown: nothing is created.
//  2. gate on, boot, request a real analysis, drain it with a real worker
//     until leaf-0 completes (real step and evidence rows), then shut down
//     while the rest of the tree is still pending.
//  3. gate off, boot: composeResultAnalysis returns nothing, and the v40
//     data written in phase 2 is byte-for-byte identical before and after.
//  4. gate on, boot: the worker resumes the interrupted analysis and
//     completes it.
func TestResultAnalysisGateRestartRollbackSequence(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.db")
	// payloadDir is fixed and reopened by every phase, exactly like a real
	// restart reopens the same on-disk artifact directory: phase 2's
	// materialized source must still be verifiable in phase 4's separate
	// *fsartifact.TypedStore instance, over the same store's data.
	payloadDir := filepath.Join(root, "v2-results")
	openPayloads := func() *fsartifact.TypedStore {
		store, err := fsartifact.NewTypedStore(payloadDir, 1024*1024)
		if err != nil {
			t.Fatalf("open payload store: %v", err)
		}
		return store
	}
	scope := domain.ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:U1", Project: "app"}
	content := "aaaaaaaaaaaa" + "bbbbbbbbbbbb" + "cccccccccccc" // three 12-byte segments at MaxSegmentBytes=12

	// Phase 1: gate off, boot, shutdown. Nothing is created.
	store1, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	models1 := resultAnalysisTestModels(t)
	composition1, err := composeResultAnalysis(resultAnalysisRestartTestConfig(false), models1, nil, store1)
	if err != nil || composition1 != nil {
		t.Fatalf("phase 1 compose(disabled) = %v, %v, want nil, nil", composition1, err)
	}
	var preCount int
	if err := store1.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM result_analyses`).Scan(&preCount); err != nil {
		t.Fatal(err)
	}
	if preCount != 0 {
		t.Fatalf("phase 1 result_analyses has %d rows, want 0", preCount)
	}
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase 2: gate on, boot, create a real analysis with real steps and
	// real evidence, then shut down mid-drain.
	store2, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	payloads2 := openPayloads()
	models2 := resultAnalysisRestartModels(payloads2)
	results2, err := adaptersqlite.NewResultStore(store2, payloads2)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := results2.Materialize(ctx, port.ResultMaterialization{
		Producer: domain.ResultProducer{Kind: domain.ResultProducerACPJob, ID: "op-1", Revision: 0},
		Payload:  content, Scope: scope, Retention: domain.ResultRetentionWorkstream, MediaType: "text/plain; charset=utf-8",
	})
	if err != nil {
		t.Fatalf("phase 2 materialize source: %v", err)
	}

	composition2, err := composeResultAnalysis(resultAnalysisRestartTestConfig(true), models2, modelcalllimiter.New(2), store2)
	if err != nil || composition2 == nil {
		t.Fatalf("phase 2 compose(enabled) = %v, %v", composition2, err)
	}
	// Swap in the blocking analyzer by rebuilding the worker over the same
	// durable stores composeResultAnalysis already built: the composition
	// itself is exercised for its wiring (criterion 2/7 elsewhere); this
	// phase only needs deterministic, hermetic draining with no live model
	// credentials.
	blockingWorker := rebuildResultAnalysisWorkerForTest(t, store2, payloads2, &blockingSecondLeafAnalyzer{})

	analysisResult, err := composition2.service.RequestAnalysis(ctx, handle.ResultID, scope, "ws-1", "what changed in this document?", time.Now().UTC())
	if err != nil {
		t.Fatalf("phase 2 request analysis: %v", err)
	}
	analysisID := analysisResult.AnalysisID

	workerCtx2, cancelWorker2 := context.WithCancel(ctx)
	go blockingWorker.Run(workerCtx2)
	// Wait for leaf-0 itself to reach 'completed' (RunLeaf's own last
	// step), not merely for its evidence row to appear: evidence is
	// written before the step's Complete call inside RunLeaf, so waiting
	// on evidence alone races that ordering.
	waitForRestartCondition(t, "phase 2 leaf-0 completes", 10*time.Second, func() bool {
		var completedLeafZero int
		if err := store2.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM analysis_steps
			WHERE analysis_id = ? AND kind = 'leaf' AND segment_ordinal = 0 AND state = 'completed'`, analysisID).Scan(&completedLeafZero); err != nil {
			return false
		}
		return completedLeafZero == 1
	})
	waitForRestartCondition(t, "phase 2 leaf-0's evidence is durable", 5*time.Second, func() bool {
		var evidenceCount int
		if err := store2.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM analysis_evidence WHERE analysis_id = ?`, analysisID).Scan(&evidenceCount); err != nil {
			return false
		}
		return evidenceCount >= 1
	})
	state2, _ := readAnalysisStateForRestartTest(t, store2, analysisID)
	if state2 == string(domain.AnalysisCompleted) {
		t.Fatal("phase 2: analysis completed before shutdown, the test needs an interrupted analysis to prove resume in phase 4")
	}
	cancelWorker2()
	if err := blockingWorker.WaitStopped(ctx); err != nil {
		t.Fatalf("phase 2 WaitStopped: %v", err)
	}
	snapshotAfterPhase2 := dumpResultAnalysisV40Tables(t, store2)
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase 3: gate off, boot. No new analysis, no drained step, and the
	// v40 data from phase 2 is byte for byte identical.
	store3, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshotBeforePhase3Boot := dumpResultAnalysisV40Tables(t, store3)
	if snapshotBeforePhase3Boot != snapshotAfterPhase2 {
		t.Fatalf("phase 3 pre-boot v40 snapshot != phase 2 end-of-run snapshot")
	}
	models3 := resultAnalysisRestartModels(openPayloads())
	composition3, err := composeResultAnalysis(resultAnalysisRestartTestConfig(false), models3, nil, store3)
	if err != nil || composition3 != nil {
		t.Fatalf("phase 3 compose(disabled) = %v, %v, want nil, nil", composition3, err)
	}
	// No worker exists, so nothing drains; wait one interval's worth of
	// wall-clock time to prove that nothing mutates state in the
	// background even though the previous phase's config left the
	// analysis non-terminal.
	time.Sleep(50 * time.Millisecond)
	snapshotAfterPhase3Boot := dumpResultAnalysisV40Tables(t, store3)
	if snapshotAfterPhase3Boot != snapshotAfterPhase2 {
		t.Fatal("phase 3: a disabled-gate boot mutated v40 data; the gate must never destroy or alter analysis data while off")
	}
	if err := store3.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase 4: gate on, boot. The worker resumes the interrupted analysis
	// and completes it, using the same deterministic hermetic analyzer
	// (now allowed to run every leaf, simulating the restart that lets the
	// blocked leaf proceed).
	store4, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	payloads4 := openPayloads()
	models4 := resultAnalysisRestartModels(payloads4)
	composition4, err := composeResultAnalysis(resultAnalysisRestartTestConfig(true), models4, modelcalllimiter.New(2), store4)
	if err != nil || composition4 == nil {
		t.Fatalf("phase 4 compose(enabled) = %v, %v", composition4, err)
	}
	resumeWorker := rebuildResultAnalysisWorkerForTest(t, store4, payloads4, &fixedAnalyzer{})
	workerCtx4, cancelWorker4 := context.WithCancel(ctx)
	go resumeWorker.Run(workerCtx4)
	waitForRestartCondition(t, "phase 4 resumed analysis completes", 10*time.Second, func() bool {
		state, _ := readAnalysisStateForRestartTest(t, store4, analysisID)
		return state == string(domain.AnalysisCompleted)
	})
	cancelWorker4()
	if err := resumeWorker.WaitStopped(ctx); err != nil {
		t.Fatalf("phase 4 WaitStopped: %v", err)
	}

	// leaf-0's evidence from phase 2 must still be exactly one row: the
	// resumed run must never re-run or duplicate already-completed work.
	var finalEvidenceCount int
	if err := store4.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM analysis_evidence WHERE analysis_id = ?`, analysisID).Scan(&finalEvidenceCount); err != nil {
		t.Fatal(err)
	}
	if finalEvidenceCount != 1 {
		t.Fatalf("phase 4 final evidence count = %d, want 1 (leaf-0's evidence, never duplicated)", finalEvidenceCount)
	}
	if err := store4.Close(); err != nil {
		t.Fatal(err)
	}
}

func readAnalysisStateForRestartTest(t *testing.T, store *adaptersqlite.Store, analysisID string) (state, failureCode string) {
	t.Helper()
	if err := store.DB().QueryRowContext(t.Context(), `SELECT state, failure_code FROM result_analyses WHERE analysis_id = ?`, analysisID).Scan(&state, &failureCode); err != nil {
		t.Fatalf("read analysis state: %v", err)
	}
	return state, failureCode
}

// rebuildResultAnalysisWorkerForTest wires a *resultanalysis.Worker over
// the same v40 adapters composeResultAnalysis itself uses, but with the
// hermetic in-process analyzer passed in, so this test never needs a live
// model credential or network access while still exercising the real
// SQLite-backed worker.
func rebuildResultAnalysisWorkerForTest(t *testing.T, store *adaptersqlite.Store, payloads port.ResultPayloadStore, analyzer port.ResultAnalyzer) *resultanalysisusecase.Worker {
	t.Helper()
	analyses := adaptersqlite.NewAnalysisStore(store)
	steps := adaptersqlite.NewAnalysisStepStore(store)
	segments := adaptersqlite.NewAnalysisSegmentStore(store)
	evidence := adaptersqlite.NewAnalysisEvidenceStore(store)
	completion := adaptersqlite.NewAnalysisCompletionStore(store)
	trustedResults, resultsErr := adaptersqlite.NewResultStore(store, payloads)
	if resultsErr != nil {
		t.Fatalf("rebuild trusted result store: %v", resultsErr)
	}
	w, newErr := resultanalysisusecase.NewWorker(resultanalysisusecase.WorkerConfig{
		// The lease must be short relative to the wall-clock time between
		// phase 2's shutdown and phase 4's boot in this test (a few
		// seconds), or leaf-1/leaf-2's claim from phase 2 (cancelled, but
		// never explicitly released) would still look unexpired when
		// phase 4 starts, and ClaimNext would never reclaim it.
		Interval: time.Second, Lease: 2 * time.Second,
	}, resultanalysisusecase.WorkerDependencies{
		Analyses: analyses, Active: analyses, Running: analyses, Steps: steps, Segments: segments,
		Evidence: evidence, Payloads: steps, Completion: completion, Source: trustedResults, Analyzer: analyzer,
		Clock: port.SystemClock{}, Logger: noopRestartTestLogger{},
	})
	if newErr != nil {
		t.Fatalf("rebuild result analysis worker: %v", newErr)
	}
	return w
}

type noopRestartTestLogger struct{}

func (noopRestartTestLogger) Debug(string, ...any) {}
func (noopRestartTestLogger) Info(string, ...any)  {}
func (noopRestartTestLogger) Warn(string, ...any)  {}
func (noopRestartTestLogger) Error(string, ...any) {}
