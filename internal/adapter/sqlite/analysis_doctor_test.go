package sqlite

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// doctorSeedSourceResult seeds one result_records row so AnalysisStore.Create
// can look up its byte length, exactly like newAnalysisStoreFixture does for
// a single source.
func doctorSeedSourceResult(t *testing.T, store *Store, id, sha string) {
	t.Helper()
	// producer_id must be unique per row (result_records has a unique index
	// on producer_kind, producer_id, producer_revision); the result id
	// itself is already unique per seeded source, so reusing it here keeps
	// every seeded row distinct.
	if _, err := store.DB().ExecContext(t.Context(), `INSERT INTO result_records (result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key,
		sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state)
		VALUES (?, 'tool_operation', ?, 0, 'artifact', 'key-1', ?, 1024, 'text/plain', 'U1', 'T1', 'slack:T1:dm:U1', 'workspace', 'context', 1, 'available')`,
		id, "op-"+id, sha); err != nil {
		t.Fatal(err)
	}
}

// doctorAnalysisIdentity builds a distinct valid analysis identity from one
// hex digit, so a run of these never collides on the
// result_analyses_by_semantic_identity unique index.
func doctorAnalysisIdentity(hexDigit string) domain.AnalysisIdentity {
	return domain.AnalysisIdentity{
		SourceResultID:      strings.Repeat(hexDigit, 64),
		SourceSHA256:        strings.Repeat(hexDigit, 64),
		ObjectiveClass:      domain.AnalysisObjectiveBoundedQuestionV1,
		ObjectiveDigest:     strings.Repeat(hexDigit, 64),
		SegmentationVersion: "text_v1",
		PromptVersion:       "leaf_v1",
		ModelFingerprint:    "model-v1",
		LimitsDigest:        testAnalysisLimits().Digest(),
	}
}

// doctorCreateAnalysis seeds one source result and creates its analysis row
// in 'preparing' state through the real AnalysisStore.Create, returning the
// analysis id. now is the caller's single injected clock: every seeded
// analysis in the test shares it, so a later MarkRunning or Complete call
// using the same now can never land before the row's own created_at.
func doctorCreateAnalysis(t *testing.T, store *Store, hexDigit string, now time.Time) string {
	t.Helper()
	identity := doctorAnalysisIdentity(hexDigit)
	doctorSeedSourceResult(t, store, identity.SourceResultID, identity.SourceSHA256)
	analyses := NewAnalysisStore(store)
	record, err := analyses.Create(t.Context(), identity, testAnalysisLimits(), "what changed?", testAnalysisScope(), "ws-1", now)
	if err != nil {
		t.Fatalf("create analysis %s: %v", hexDigit, err)
	}
	return record.AnalysisID
}

// doctorCompleteRootReduction prepares one completed leaf child and one
// completed root reduction step referencing it, so analysisID's coverage
// reads complete: a completed reduction step that is never referenced as
// anyone else's child.
func doctorCompleteRootReduction(t *testing.T, steps *AnalysisStepStore, analysisID string) {
	t.Helper()
	now := time.Now().UTC()
	leaf, err := steps.Prepare(t.Context(), port.AnalysisStep{AnalysisID: analysisID, StepID: "leaf-0", Kind: domain.AnalysisStepLeaf, SegmentOrdinal: 0, CreatedAt: now})
	if err != nil {
		t.Fatalf("prepare leaf step: %v", err)
	}
	_ = leaf
	claimedLeaf, ok, err := steps.ClaimNext(t.Context(), analysisID, now, time.Hour)
	if err != nil || !ok {
		t.Fatalf("claim leaf step: ok=%v err=%v", ok, err)
	}
	claim := domain.AnalysisStepClaim{AnalysisID: analysisID, StepID: claimedLeaf.StepID, Generation: claimedLeaf.Generation, LeaseUntil: claimedLeaf.LeaseUntil}
	if _, err := steps.Complete(t.Context(), claim, strings.Repeat("d", 64), now); err != nil {
		t.Fatalf("complete leaf step: %v", err)
	}

	if _, err := steps.Prepare(t.Context(), port.AnalysisStep{AnalysisID: analysisID, StepID: "reduce-0", Kind: domain.AnalysisStepReduction, ChildStepIDs: []string{"leaf-0"}, CreatedAt: now}); err != nil {
		t.Fatalf("prepare reduction step: %v", err)
	}
	claimedReduction, ok, err := steps.ClaimNext(t.Context(), analysisID, now, time.Hour)
	if err != nil || !ok {
		t.Fatalf("claim reduction step: ok=%v err=%v", ok, err)
	}
	reductionClaim := domain.AnalysisStepClaim{AnalysisID: analysisID, StepID: claimedReduction.StepID, Generation: claimedReduction.Generation, LeaseUntil: claimedReduction.LeaseUntil}
	if _, err := steps.Complete(t.Context(), reductionClaim, strings.Repeat("e", 64), now); err != nil {
		t.Fatalf("complete reduction step: %v", err)
	}
}

// TestCheckResultAnalysisStateReportsExactCounts is item 3 of the TRD 07
// closing round: it seeds known v40 state (non-terminal analyses, steps
// with an expired lease, and one analysis with proven-complete coverage)
// and pins the exact counts CheckResultAnalysisState must report.
func TestCheckResultAnalysisStateReportsExactCounts(t *testing.T) {
	store, _ := newTestStore(t)
	steps := NewAnalysisStepStore(store)
	analyses := NewAnalysisStore(store)

	now := time.Now().UTC()

	// Analysis "1": 'preparing', with one 'prepared' leaf, one 'claimed'
	// (fresh) leaf, and one 'claimed' (already expired) leaf.
	analysisA := doctorCreateAnalysis(t, store, "1", now)
	if _, err := steps.Prepare(t.Context(), port.AnalysisStep{AnalysisID: analysisA, StepID: "leaf-prepared", Kind: domain.AnalysisStepLeaf, SegmentOrdinal: 0, CreatedAt: now}); err != nil {
		t.Fatalf("prepare leaf-prepared: %v", err)
	}
	if _, err := steps.Prepare(t.Context(), port.AnalysisStep{AnalysisID: analysisA, StepID: "leaf-claimed-fresh", Kind: domain.AnalysisStepLeaf, SegmentOrdinal: 1, CreatedAt: now}); err != nil {
		t.Fatalf("prepare leaf-claimed-fresh: %v", err)
	}
	if _, ok, err := steps.ClaimNext(t.Context(), analysisA, now, time.Hour); err != nil || !ok {
		t.Fatalf("claim leaf-claimed-fresh: ok=%v err=%v", ok, err)
	}
	// A lease claimed two hours ago for one second has already elapsed by
	// the time CheckResultAnalysisState reads the real clock below. The step
	// is prepared at the same past instant so created_at never lands after
	// the claim's updated_at.
	staleClaimNow := now.Add(-2 * time.Hour)
	if _, err := steps.Prepare(t.Context(), port.AnalysisStep{AnalysisID: analysisA, StepID: "leaf-claimed-expired", Kind: domain.AnalysisStepLeaf, SegmentOrdinal: 2, CreatedAt: staleClaimNow}); err != nil {
		t.Fatalf("prepare leaf-claimed-expired: %v", err)
	}
	if _, ok, err := steps.ClaimNext(t.Context(), analysisA, staleClaimNow, time.Second); err != nil || !ok {
		t.Fatalf("claim leaf-claimed-expired: ok=%v err=%v", ok, err)
	}

	// Analysis "2": 'running' with no steps at all, so its coverage cannot
	// be proven complete.
	analysisB := doctorCreateAnalysis(t, store, "2", now)
	if _, err := analyses.MarkRunning(t.Context(), analysisB, testAnalysisScope(), now); err != nil {
		t.Fatalf("mark analysis B running: %v", err)
	}

	// Analysis "3": 'running' with a completed root reduction step that no
	// other step references as a child, so its coverage reads complete and
	// it must not be counted.
	analysisC := doctorCreateAnalysis(t, store, "3", now)
	if _, err := analyses.MarkRunning(t.Context(), analysisC, testAnalysisScope(), now); err != nil {
		t.Fatalf("mark analysis C running: %v", err)
	}
	doctorCompleteRootReduction(t, steps, analysisC)

	// Analysis "4": already terminal ('completed'). It must not be counted
	// as non-terminal or incomplete, proving the doctor check ignores
	// terminal rows entirely, not just running ones.
	analysisD := doctorCreateAnalysis(t, store, "4", now)
	if _, err := analyses.Complete(t.Context(), analysisD, testAnalysisScope(), now); err != nil {
		t.Fatalf("complete analysis D: %v", err)
	}

	health, err := store.CheckResultAnalysisState(t.Context())
	if err != nil {
		t.Fatalf("CheckResultAnalysisState() error = %v", err)
	}
	want := domain.ResultAnalysisHealth{
		SchemaVersion:              SchemaVersion,
		StepQueueDepth:             3, // leaf-prepared, leaf-claimed-fresh, leaf-claimed-expired
		ExpiredLeases:              1, // leaf-claimed-expired only
		NonTerminalAnalyses:        3, // A (preparing), B and C (running); D is terminal
		IncompleteCoverageAnalyses: 1, // B only; C has proven-complete coverage
	}
	if health != want {
		t.Fatalf("CheckResultAnalysisState() = %+v, want %+v", health, want)
	}
}

// TestResultAnalysisHealthCarriesOnlyBoundedCounts is item 3's content-free
// property: domain.ResultAnalysisHealth must carry only bounded integer
// counts, never a source id, objective, excerpt, or digest string. This
// checks the type itself with reflection, so a future field added to the
// struct with a string type (a place a leaked digest or excerpt could hide)
// fails this test even before any test data exercises it.
func TestResultAnalysisHealthCarriesOnlyBoundedCounts(t *testing.T) {
	typ := reflect.TypeOf(domain.ResultAnalysisHealth{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.Int {
			t.Fatalf("domain.ResultAnalysisHealth.%s has kind %s, want a bounded int count; the doctor view must never carry content", field.Name, field.Type.Kind())
		}
	}
}
