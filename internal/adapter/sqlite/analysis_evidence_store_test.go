package sqlite

import (
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// newAnalysisEvidenceStoreFixture opens a fresh v40 database, seeds one
// result_analyses row and one leaf analysis_steps row (analysis_evidence
// foreign-keys to the step), and returns the evidence store, the step
// store over the same database, and that analysis's id.
func newAnalysisEvidenceStoreFixture(t *testing.T) (*AnalysisEvidenceStore, *AnalysisStepStore, string) {
	t.Helper()
	dbStore, err := Initialize(t.Context(), t.TempDir()+"/analysis-evidence-store.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbStore.Close() })
	now := time.Now().UTC().Unix()
	hex64 := strings.Repeat("a", 64)
	if _, err := dbStore.DB().ExecContext(t.Context(), `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, created_at, updated_at)
		VALUES (?, ?, ?, 100, 'bounded_question_v1', ?, 'objective text', 'text_v1', 'prompt_v1', 'model_v1', ?, '{}',
		'U1', 'T1', 'slack:T1:dm:U1', 'workspace', 'ws-1', 'preparing', ?, ?)`,
		hex64, hex64, hex64, hex64, hex64, now, now); err != nil {
		t.Fatal(err)
	}
	steps := NewAnalysisStepStore(dbStore)
	if _, err := steps.Prepare(t.Context(), stepFixture(hex64, "leaf-0", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	return NewAnalysisEvidenceStore(dbStore), steps, hex64
}

func testEvidenceExcerpt(id string) port.AnalysisEvidenceExcerpt {
	return port.AnalysisEvidenceExcerpt{
		EvidenceID:     id,
		SegmentOrdinal: 0,
		Range:          domain.AnalysisByteRange{OffsetBytes: 0, LengthBytes: 8},
		SHA256:         strings.Repeat("d", 64),
		Excerpt:        "excerpt1",
	}
}

func TestAnalysisEvidenceStoreWriteAndListByLeafStep(t *testing.T) {
	store, _, analysisID := newAnalysisEvidenceStoreFixture(t)
	now := time.Now().UTC()

	excerpt := testEvidenceExcerpt(strings.Repeat("1", 64))
	if err := store.Write(t.Context(), analysisID, "leaf-0", excerpt, now); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := store.ListByLeafStep(t.Context(), analysisID, "leaf-0")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].EvidenceID != excerpt.EvidenceID || got[0].Excerpt != excerpt.Excerpt {
		t.Fatalf("listed evidence = %+v, want one row matching %+v", got, excerpt)
	}
}

// TestAnalysisEvidenceStoreWriteIsIdempotentByEvidenceID is criterion 2: a
// second write of the same excerpt (same deterministic evidence id) must
// not alter the stored row, and the v40 analysis_evidence_immutable trigger
// backs this even if the store's own ON CONFLICT DO NOTHING were bypassed.
func TestAnalysisEvidenceStoreWriteIsIdempotentByEvidenceID(t *testing.T) {
	store, _, analysisID := newAnalysisEvidenceStoreFixture(t)
	now := time.Now().UTC()

	first := testEvidenceExcerpt(strings.Repeat("2", 64))
	if err := store.Write(t.Context(), analysisID, "leaf-0", first, now); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Same evidence id, different excerpt text: a second write attempt for
	// the same id must never change what is stored.
	second := first
	second.Excerpt = "different-text"
	if err := store.Write(t.Context(), analysisID, "leaf-0", second, now.Add(time.Minute)); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := store.ListByLeafStep(t.Context(), analysisID, "leaf-0")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one durable row after a retried write, got %d", len(got))
	}
	if got[0].Excerpt != first.Excerpt {
		t.Fatalf("stored excerpt = %q, want the first write's %q (immutable)", got[0].Excerpt, first.Excerpt)
	}
}

// TestAnalysisEvidenceStoreUpdateIsRejectedByImmutabilityTrigger is the
// schema-level backstop: even a direct UPDATE against analysis_evidence,
// bypassing the store entirely, must be rejected.
func TestAnalysisEvidenceStoreUpdateIsRejectedByImmutabilityTrigger(t *testing.T) {
	store, steps, analysisID := newAnalysisEvidenceStoreFixture(t)
	now := time.Now().UTC()
	excerpt := testEvidenceExcerpt(strings.Repeat("3", 64))
	if err := store.Write(t.Context(), analysisID, "leaf-0", excerpt, now); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := steps.db.ExecContext(t.Context(), `UPDATE analysis_evidence SET excerpt_bytes = 'changed' WHERE evidence_id = ?`, excerpt.EvidenceID)
	if err == nil {
		t.Fatal("expected the immutability trigger to reject a direct UPDATE")
	}
}
