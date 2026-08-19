package sqlite

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// newAnalysisStepStoreFixture opens a fresh v40 database, creates one
// result_analyses row (analysis_steps foreign-keys to it), and returns the
// step store plus that analysis's id.
func newAnalysisStepStoreFixture(t *testing.T) (*AnalysisStepStore, string) {
	t.Helper()
	store, err := Initialize(t.Context(), t.TempDir()+"/analysis-step-store.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Now().UTC().Unix()
	hex64 := strings.Repeat("a", 64)
	if _, err := store.DB().ExecContext(t.Context(), `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, created_at, updated_at)
		VALUES (?, ?, ?, 100, 'bounded_question_v1', ?, 'objective text', 'text_v1', 'prompt_v1', 'model_v1', ?, '{}',
		'U1', 'T1', 'slack:T1:dm:U1', 'workspace', 'ws-1', 'preparing', ?, ?)`,
		hex64, hex64, hex64, hex64, hex64, now, now); err != nil {
		t.Fatal(err)
	}
	return NewAnalysisStepStore(store), hex64
}

func TestAnalysisStepStoreClaimCASConflict(t *testing.T) {
	store, analysisID := newAnalysisStepStoreFixture(t)
	now := time.Now().UTC()

	if _, err := store.Prepare(t.Context(), stepFixture(analysisID, "leaf-0", now)); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	claim1, ok, err := store.ClaimNext(t.Context(), analysisID, now, 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	token1 := domain.AnalysisStepClaim{AnalysisID: analysisID, StepID: claim1.StepID, Generation: claim1.Generation, LeaseUntil: claim1.LeaseUntil}

	// The lease expires; a second claimant reclaims the same step.
	afterExpiry := claim1.LeaseUntil.Add(time.Second)
	claim2, ok, err := store.ClaimNext(t.Context(), analysisID, afterExpiry, 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("reclaim: ok=%v err=%v", ok, err)
	}
	if claim2.LeaseUntil.Equal(claim1.LeaseUntil) {
		t.Fatal("reclaim did not obtain a fresh lease")
	}

	// The expired claimant can no longer complete, retry, or fail the row.
	if _, err := store.Complete(t.Context(), token1, strings.Repeat("b", 64), afterExpiry); !errors.Is(err, domain.ErrAnalysisCASConflict) {
		t.Fatalf("stale complete = %v, want ErrAnalysisCASConflict", err)
	}
	if err := store.Retry(t.Context(), token1, afterExpiry.Add(time.Minute), true); !errors.Is(err, domain.ErrAnalysisCASConflict) {
		t.Fatalf("stale retry = %v, want ErrAnalysisCASConflict", err)
	}
	if err := store.Fail(t.Context(), token1, domain.AnalysisFailureAttemptsExhausted, afterExpiry); !errors.Is(err, domain.ErrAnalysisCASConflict) {
		t.Fatalf("stale fail = %v, want ErrAnalysisCASConflict", err)
	}

	// The current owner's own token still works.
	token2 := domain.AnalysisStepClaim{AnalysisID: analysisID, StepID: claim2.StepID, Generation: claim2.Generation, LeaseUntil: claim2.LeaseUntil}
	if _, err := store.Complete(t.Context(), token2, strings.Repeat("b", 64), afterExpiry); err != nil {
		t.Fatalf("current owner complete: %v", err)
	}
}

func TestAnalysisStepStoreReductionNotClaimableUntilChildrenComplete(t *testing.T) {
	store, analysisID := newAnalysisStepStoreFixture(t)
	now := time.Now().UTC()

	if _, err := store.Prepare(t.Context(), stepFixture(analysisID, "leaf-0", now)); err != nil {
		t.Fatalf("prepare leaf-0: %v", err)
	}
	if _, err := store.Prepare(t.Context(), stepFixture(analysisID, "leaf-1", now)); err != nil {
		t.Fatalf("prepare leaf-1: %v", err)
	}
	reduction := analysisStepReductionFixture(analysisID, "reduce-0", now, "leaf-0", "leaf-1")
	if _, err := store.Prepare(t.Context(), reduction); err != nil {
		t.Fatalf("prepare reduce-0: %v", err)
	}

	// Only the two leaves are claimable; three ClaimNext calls exhaust them
	// and the fourth finds nothing (never the reduction step).
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		claimed, ok, err := store.ClaimNext(t.Context(), analysisID, now, time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		if claimed.Kind != domain.AnalysisStepLeaf {
			t.Fatalf("claim %d picked a non-leaf step %q", i, claimed.StepID)
		}
		seen[claimed.StepID] = true
	}
	if _, ok, err := store.ClaimNext(t.Context(), analysisID, now, time.Minute); err != nil || ok {
		t.Fatalf("expected no further claimable step while the reduction's children are still claimed: ok=%v err=%v", ok, err)
	}

	// Complete both leaves.
	for stepID := range seen {
		claim := domain.AnalysisStepClaim{AnalysisID: analysisID, StepID: stepID, Generation: 0, LeaseUntil: now.Add(time.Minute)}
		if _, err := store.Complete(t.Context(), claim, strings.Repeat("c", 64), now); err != nil {
			t.Fatalf("complete %s: %v", stepID, err)
		}
	}

	claimed, ok, err := store.ClaimNext(t.Context(), analysisID, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim reduction after children complete: ok=%v err=%v", ok, err)
	}
	if claimed.StepID != "reduce-0" || claimed.Kind != domain.AnalysisStepReduction {
		t.Fatalf("expected to claim reduce-0, got %q kind %q", claimed.StepID, claimed.Kind)
	}
	if len(claimed.ChildStepIDs) != 2 {
		t.Fatalf("expected 2 child step ids, got %v", claimed.ChildStepIDs)
	}
}

func TestAnalysisStepStoreRetryConsumeAttempt(t *testing.T) {
	store, analysisID := newAnalysisStepStoreFixture(t)
	now := time.Now().UTC()

	if _, err := store.Prepare(t.Context(), stepFixture(analysisID, "leaf-0", now)); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	claimed, ok, err := store.ClaimNext(t.Context(), analysisID, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claimed.Attempt != 0 {
		t.Fatalf("attempt before any retry = %d, want 0", claimed.Attempt)
	}
	token := domain.AnalysisStepClaim{AnalysisID: analysisID, StepID: claimed.StepID, Generation: claimed.Generation, LeaseUntil: claimed.LeaseUntil}

	if err := store.Retry(t.Context(), token, now.Add(time.Minute), false); err != nil {
		t.Fatalf("retry without consuming: %v", err)
	}
	steps, err := store.List(t.Context(), analysisID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Attempt != 0 {
		t.Fatalf("attempt after consumeAttempt=false = %d, want 0", steps[0].Attempt)
	}

	claimed2, ok, err := store.ClaimNext(t.Context(), analysisID, now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("reclaim: ok=%v err=%v", ok, err)
	}
	token2 := domain.AnalysisStepClaim{AnalysisID: analysisID, StepID: claimed2.StepID, Generation: claimed2.Generation, LeaseUntil: claimed2.LeaseUntil}
	if err := store.Retry(t.Context(), token2, now.Add(3*time.Minute), true); err != nil {
		t.Fatalf("retry consuming: %v", err)
	}
	steps, err = store.List(t.Context(), analysisID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Attempt != 1 {
		t.Fatalf("attempt after consumeAttempt=true = %d, want 1", steps[0].Attempt)
	}
	if steps[0].State != domain.AnalysisStepPrepared {
		t.Fatalf("state after retry = %q, want prepared", steps[0].State)
	}
}

func TestAnalysisStepStoreListPagesInOrder(t *testing.T) {
	store, analysisID := newAnalysisStepStoreFixture(t)
	now := time.Now().UTC()
	for _, id := range []string{"leaf-0", "leaf-1", "leaf-2", "leaf-3"} {
		if _, err := store.Prepare(t.Context(), stepFixture(analysisID, id, now)); err != nil {
			t.Fatalf("prepare %s: %v", id, err)
		}
	}
	page1, err := store.List(t.Context(), analysisID, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || page1[0].StepID != "leaf-0" || page1[1].StepID != "leaf-1" {
		t.Fatalf("page1 = %+v", page1)
	}
	page2, err := store.List(t.Context(), analysisID, page1[len(page1)-1].StepID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].StepID != "leaf-2" || page2[1].StepID != "leaf-3" {
		t.Fatalf("page2 = %+v", page2)
	}
	page3, err := store.List(t.Context(), analysisID, page2[len(page2)-1].StepID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page3) != 0 {
		t.Fatalf("page3 = %+v, want empty", page3)
	}
}

func stepFixture(analysisID, stepID string, now time.Time) port.AnalysisStep {
	return port.AnalysisStep{
		AnalysisID: analysisID,
		StepID:     stepID,
		Kind:       domain.AnalysisStepLeaf,
		CreatedAt:  now,
	}
}

func analysisStepReductionFixture(analysisID, stepID string, now time.Time, children ...string) port.AnalysisStep {
	return port.AnalysisStep{
		AnalysisID:   analysisID,
		StepID:       stepID,
		Kind:         domain.AnalysisStepReduction,
		ChildStepIDs: children,
		CreatedAt:    now,
	}
}
