package sqlite

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func testPayloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum)
}

func TestAnalysisStepStoreWriteAndReadPayload(t *testing.T) {
	steps, analysisID := newAnalysisStepStoreFixture(t)
	now := time.Now().UTC()
	if _, err := steps.Prepare(t.Context(), stepFixture(analysisID, "leaf-0", now)); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := steps.ClaimNext(t.Context(), analysisID, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	claim := domain.AnalysisStepClaim{AnalysisID: analysisID, StepID: claimed.StepID, Generation: claimed.Generation, LeaseUntil: claimed.LeaseUntil}

	payload := []byte(`{"findings":["x"]}`)
	if err := steps.WritePayload(t.Context(), claim, payload, now); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	got, err := steps.ReadPayload(t.Context(), analysisID, claimed.StepID)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("read payload = %q, want %q", got, payload)
	}
}

// TestAnalysisStepStorePayloadSurvivesCompletionAndIsImmutable is criterion
// 4/10 support: once a step is completed, its payload is durable and the
// v40 analysis_steps_completed_immutable trigger blocks any further write,
// so a restarted worker always reads the exact payload the original run
// wrote.
func TestAnalysisStepStorePayloadSurvivesCompletionAndIsImmutable(t *testing.T) {
	steps, analysisID := newAnalysisStepStoreFixture(t)
	now := time.Now().UTC()
	if _, err := steps.Prepare(t.Context(), stepFixture(analysisID, "leaf-0", now)); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := steps.ClaimNext(t.Context(), analysisID, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	claim := domain.AnalysisStepClaim{AnalysisID: analysisID, StepID: claimed.StepID, Generation: claimed.Generation, LeaseUntil: claimed.LeaseUntil}

	payload := []byte(`{"findings":["x"]}`)
	if err := steps.WritePayload(t.Context(), claim, payload, now); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if _, err := steps.Complete(t.Context(), claim, testPayloadDigest(payload), now); err != nil {
		t.Fatalf("complete: %v", err)
	}

	got, err := steps.ReadPayload(t.Context(), analysisID, claimed.StepID)
	if err != nil {
		t.Fatalf("read payload after completion: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload after completion = %q, want %q", got, payload)
	}

	// A direct write attempt against the now-completed row must fail: its
	// lease was cleared by Complete, so the presented claim's lease no
	// longer matches, and the CAS transition rejects it.
	if err := steps.WritePayload(t.Context(), claim, []byte(`{"findings":["y"]}`), now.Add(time.Minute)); !errors.Is(err, domain.ErrAnalysisCASConflict) {
		t.Fatalf("write payload after completion = %v, want ErrAnalysisCASConflict", err)
	}

	// The v40 analysis_steps_completed_immutable trigger is the schema-level
	// backstop even when a caller bypasses the store's own CAS check
	// entirely with a direct UPDATE.
	if _, err := steps.db.ExecContext(t.Context(), `UPDATE analysis_steps SET output_payload = 'bypassed' WHERE analysis_id = ? AND step_id = ?`, analysisID, claimed.StepID); err == nil {
		t.Fatal("expected the completed-step immutability trigger to reject a direct UPDATE")
	}
}
