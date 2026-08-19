package sqlite

import (
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// newAnalysisCompletionStoreFixture opens a fresh v40 database, seeds one
// available result_records row and one 'running' result_analyses row, and
// returns the completion store plus everything needed to build a
// port.AnalysisCompletionInput for it.
func newAnalysisCompletionStoreFixture(t *testing.T) (store *AnalysisCompletionStore, dbStore *Store, analysisID, sourceID, sourceSHA string) {
	t.Helper()
	dbStore, err := Initialize(t.Context(), t.TempDir()+"/analysis-completion-store.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbStore.Close() })

	sourceID = strings.Repeat("a", 64)
	sourceSHA = strings.Repeat("b", 64)
	if _, err := dbStore.DB().ExecContext(t.Context(), `INSERT INTO result_records (result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key,
		sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state)
		VALUES (?, 'tool_operation', 'op-1', 0, 'artifact', 'key-1', ?, 1024, 'text/plain', 'U1', 'T1', 'slack:T1:dm:U1', 'workspace', 'context', 1, 'available')`,
		sourceID, sourceSHA); err != nil {
		t.Fatal(err)
	}

	analysisID = strings.Repeat("c", 64)
	now := time.Now().UTC().Unix()
	objectiveDigest := strings.Repeat("d", 64)
	limitsDigest := strings.Repeat("e", 64)
	if _, err := dbStore.DB().ExecContext(t.Context(), `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, created_at, updated_at)
		VALUES (?, ?, ?, 1024, 'bounded_question_v1', ?, 'objective text', 'text_v1', 'prompt_v1', 'model_v1', ?, '{}',
		'U1', 'T1', 'slack:T1:dm:U1', 'workspace', 'ws-1', 'running', ?, ?)`,
		analysisID, sourceID, sourceSHA, objectiveDigest, limitsDigest, now, now); err != nil {
		t.Fatal(err)
	}

	return NewAnalysisCompletionStore(dbStore), dbStore, analysisID, sourceID, sourceSHA
}

func testCompletionInput(analysisID, sourceID, sourceSHA string) port.AnalysisCompletionInput {
	return port.AnalysisCompletionInput{
		AnalysisID:     analysisID,
		Scope:          domain.ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:U1", Project: "workspace"},
		SourceResultID: sourceID,
		SourceSHA256:   sourceSHA,
		SourceBytes:    1024,
		PacketSHA256:   strings.Repeat("f", 64),
		PacketBytes:    128,
		PromptVersion:  "analysis-reduction-v1",
	}
}

func TestAnalysisCompletionStoreCommitsAllThreeWrites(t *testing.T) {
	store, dbStore, analysisID, sourceID, sourceSHA := newAnalysisCompletionStoreFixture(t)
	now := time.Now().UTC()

	if err := store.CompleteAnalysis(t.Context(), testCompletionInput(analysisID, sourceID, sourceSHA), now); err != nil {
		t.Fatalf("complete analysis: %v", err)
	}

	var state string
	if err := dbStore.DB().QueryRowContext(t.Context(), `SELECT state FROM result_analyses WHERE analysis_id = ?`, analysisID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "completed" {
		t.Fatalf("analysis state = %q, want completed", state)
	}

	var reprCount, refCount int
	if err := dbStore.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_representations WHERE result_id = ? AND kind = 'host_reduction'`, sourceID).Scan(&reprCount); err != nil {
		t.Fatal(err)
	}
	if reprCount != 1 {
		t.Fatalf("host_reduction representation count = %d, want 1", reprCount)
	}
	if err := dbStore.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_references WHERE result_id = ? AND owner_kind = 'result_analysis' AND owner_id = ? AND state = 'live'`, sourceID, analysisID).Scan(&refCount); err != nil {
		t.Fatal(err)
	}
	if refCount != 1 {
		t.Fatalf("live result_references count = %d, want 1", refCount)
	}

	// FIND-105: the persisted payload_sha256 must equal the digest the
	// caller declared for the terminal packet payload. A prior version of
	// this test never read the column back, so a store that persisted a
	// clock-dependent or otherwise wrong digest still passed.
	var persistedPacketSHA256 string
	if err := dbStore.DB().QueryRowContext(t.Context(), `SELECT payload_sha256 FROM result_representations WHERE result_id = ? AND kind = 'host_reduction'`, sourceID).Scan(&persistedPacketSHA256); err != nil {
		t.Fatal(err)
	}
	if persistedPacketSHA256 != strings.Repeat("f", 64) {
		t.Fatalf("persisted representation payload_sha256 = %q, want %q", persistedPacketSHA256, strings.Repeat("f", 64))
	}
}

// TestAnalysisCompletionStoreRetryInsertsSameReferenceRowNotASecond is
// criterion 9: a retried completion of the same analysis must not create a
// second result_references row.
func TestAnalysisCompletionStoreRetryInsertsSameReferenceRowNotASecond(t *testing.T) {
	store, dbStore, analysisID, sourceID, sourceSHA := newAnalysisCompletionStoreFixture(t)
	now := time.Now().UTC()
	input := testCompletionInput(analysisID, sourceID, sourceSHA)

	if err := store.CompleteAnalysis(t.Context(), input, now); err != nil {
		t.Fatalf("first complete: %v", err)
	}
	if err := store.CompleteAnalysis(t.Context(), input, now.Add(time.Minute)); err != nil {
		t.Fatalf("retried complete: %v", err)
	}

	var refCount, reprCount int
	if err := dbStore.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_references WHERE result_id = ? AND owner_kind = 'result_analysis'`, sourceID).Scan(&refCount); err != nil {
		t.Fatal(err)
	}
	if refCount != 1 {
		t.Fatalf("result_references count after retry = %d, want 1 (idempotent)", refCount)
	}
	if err := dbStore.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_representations WHERE result_id = ? AND kind = 'host_reduction'`, sourceID).Scan(&reprCount); err != nil {
		t.Fatal(err)
	}
	if reprCount != 1 {
		t.Fatalf("result_representations count after retry = %d, want 1 (idempotent)", reprCount)
	}
}

// TestAnalysisCompletionStoreAtomicOnRepresentationFailure is criterion 8:
// forcing a mid-transaction failure on the representation write (a
// source-identity mismatch that the v34
// result_representations_require_source_identity trigger rejects) must
// leave the analysis non-completed and write neither row, not a partial
// commit.
func TestAnalysisCompletionStoreAtomicOnRepresentationFailure(t *testing.T) {
	store, dbStore, analysisID, sourceID, _ := newAnalysisCompletionStoreFixture(t)
	now := time.Now().UTC()

	input := testCompletionInput(analysisID, sourceID, strings.Repeat("9", 64)) // wrong source digest
	if err := store.CompleteAnalysis(t.Context(), input, now); err == nil {
		t.Fatal("expected a source-identity mismatch to fail the completion transaction")
	}

	var state string
	if err := dbStore.DB().QueryRowContext(t.Context(), `SELECT state FROM result_analyses WHERE analysis_id = ?`, analysisID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Fatalf("analysis state after failed completion = %q, want running (rolled back)", state)
	}

	var reprCount, refCount int
	if err := dbStore.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_representations`).Scan(&reprCount); err != nil {
		t.Fatal(err)
	}
	if err := dbStore.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_references`).Scan(&refCount); err != nil {
		t.Fatal(err)
	}
	if reprCount != 0 || refCount != 0 {
		t.Fatalf("expected zero rows written on a rolled-back completion, got representations=%d references=%d", reprCount, refCount)
	}
}
