package port

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// The fakes below are behavioral: they implement the frozen result-analysis
// contracts with deterministic canned semantics (idempotent create,
// generation-CAS claims, bounded reduction, and bounded bundling) so the
// interfaces are pinned by behavior, not compile-only assertions.

// --- fakeAnalysisStore ---------------------------------------------------

type fakeAnalysisStore struct {
	mu       sync.Mutex
	byDigest map[string]string
	rows     map[string]AnalysisRecord
	nextID   int
}

func newFakeAnalysisStore() *fakeAnalysisStore {
	return &fakeAnalysisStore{byDigest: map[string]string{}, rows: map[string]AnalysisRecord{}}
}

func (f *fakeAnalysisStore) Create(_ context.Context, identity domain.AnalysisIdentity, objectiveText string, scope domain.ResultScope, workstreamID string, now time.Time) (AnalysisRecord, error) {
	if err := identity.Validate(); err != nil {
		return AnalysisRecord{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	digest := identity.SemanticDigest()
	if existingID, ok := f.byDigest[digest]; ok {
		return f.rows[existingID], nil
	}
	f.nextID++
	id := fmt.Sprintf("%064d", f.nextID)
	record := AnalysisRecord{
		AnalysisID: id, Identity: identity, ObjectiveText: objectiveText,
		Scope: scope, WorkstreamID: workstreamID, State: domain.AnalysisPreparing,
		CreatedAt: now, UpdatedAt: now,
	}
	f.byDigest[digest] = id
	f.rows[id] = record
	return record, nil
}

func (f *fakeAnalysisStore) Get(_ context.Context, analysisID string, scope domain.ResultScope) (AnalysisRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, ok := f.rows[analysisID]
	if !ok || record.Scope != scope {
		return AnalysisRecord{}, fmt.Errorf("%w: analysis not found", domain.ErrAnalysisUnavailable)
	}
	return record, nil
}

func (f *fakeAnalysisStore) Complete(_ context.Context, analysisID string, scope domain.ResultScope, now time.Time) (AnalysisRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, ok := f.rows[analysisID]
	if !ok || record.Scope != scope {
		return AnalysisRecord{}, fmt.Errorf("%w: analysis not found", domain.ErrAnalysisUnavailable)
	}
	if record.State == domain.AnalysisCompleted || record.State == domain.AnalysisFailed {
		return AnalysisRecord{}, fmt.Errorf("%w: analysis already terminal", domain.ErrAnalysisCASConflict)
	}
	record.State = domain.AnalysisCompleted
	record.UpdatedAt = now
	f.rows[analysisID] = record
	return record, nil
}

func (f *fakeAnalysisStore) Fail(_ context.Context, analysisID string, scope domain.ResultScope, code domain.AnalysisFailureCode, now time.Time) (AnalysisRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, ok := f.rows[analysisID]
	if !ok || record.Scope != scope {
		return AnalysisRecord{}, fmt.Errorf("%w: analysis not found", domain.ErrAnalysisUnavailable)
	}
	if record.State == domain.AnalysisCompleted || record.State == domain.AnalysisFailed {
		return AnalysisRecord{}, fmt.Errorf("%w: analysis already terminal", domain.ErrAnalysisCASConflict)
	}
	if !domain.ValidAnalysisFailureCode(string(code)) {
		return AnalysisRecord{}, fmt.Errorf("%w: unknown failure code %q", domain.ErrAnalysisValidation, code)
	}
	record.State = domain.AnalysisFailed
	record.FailureCode = code
	record.UpdatedAt = now
	f.rows[analysisID] = record
	return record, nil
}

func testScope() domain.ResultScope {
	return domain.ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:U1", Project: "workspace"}
}

func testIdentity() domain.AnalysisIdentity {
	return domain.AnalysisIdentity{
		SourceResultID:      strings.Repeat("a", 64),
		SourceSHA256:        strings.Repeat("b", 64),
		ObjectiveClass:      domain.AnalysisObjectiveBoundedQuestionV1,
		ObjectiveDigest:     strings.Repeat("c", 64),
		SegmentationVersion: "text_v1",
		PromptVersion:       "prompt_v1",
		ModelFingerprint:    "model:v1",
		LimitsDigest:        strings.Repeat("d", 64),
	}
}

func TestFakeAnalysisStoreCreateIsIdempotentBySemanticIdentity(t *testing.T) {
	store := newFakeAnalysisStore()
	identity := testIdentity()
	first, err := store.Create(context.Background(), identity, "which configs use value < 10?", testScope(), "ws-1", time.Now())
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	second, err := store.Create(context.Background(), identity, "which configs use value < 10?", testScope(), "ws-1", time.Now())
	if err != nil {
		t.Fatalf("second create failed: %v", err)
	}
	if first.AnalysisID != second.AnalysisID {
		t.Fatalf("expected idempotent create to return the same analysis id, got %s and %s", first.AnalysisID, second.AnalysisID)
	}
}

func TestFakeAnalysisStoreCompleteAndFailAreTerminalOnce(t *testing.T) {
	store := newFakeAnalysisStore()
	record, err := store.Create(context.Background(), testIdentity(), "objective", testScope(), "ws-1", time.Now())
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if _, err := store.Complete(context.Background(), record.AnalysisID, testScope(), time.Now()); err != nil {
		t.Fatalf("complete failed: %v", err)
	}
	if _, err := store.Fail(context.Background(), record.AnalysisID, testScope(), domain.AnalysisFailureWallTimeExceeded, time.Now()); !errors.Is(err, domain.ErrAnalysisCASConflict) {
		t.Fatalf("expected ErrAnalysisCASConflict on double-terminal transition, got %v", err)
	}
}

func TestFakeAnalysisStoreGetRejectsCrossScope(t *testing.T) {
	store := newFakeAnalysisStore()
	record, err := store.Create(context.Background(), testIdentity(), "objective", testScope(), "ws-1", time.Now())
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	foreignScope := testScope()
	foreignScope.Actor = "U2"
	if _, err := store.Get(context.Background(), record.AnalysisID, foreignScope); !errors.Is(err, domain.ErrAnalysisUnavailable) {
		t.Fatalf("expected ErrAnalysisUnavailable for cross-scope read, got %v", err)
	}
}

var _ AnalysisStore = (*fakeAnalysisStore)(nil)

// --- fakeAnalysisStepStore -----------------------------------------------

type fakeAnalysisStepStore struct {
	mu    sync.Mutex
	steps map[string]AnalysisStep
}

func newFakeAnalysisStepStore() *fakeAnalysisStepStore {
	return &fakeAnalysisStepStore{steps: map[string]AnalysisStep{}}
}

func (f *fakeAnalysisStepStore) key(analysisID, stepID string) string {
	return analysisID + "/" + stepID
}

func (f *fakeAnalysisStepStore) Prepare(_ context.Context, step AnalysisStep) (AnalysisStep, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	step.State = domain.AnalysisStepPrepared
	step.Generation = 1
	f.steps[f.key(step.AnalysisID, step.StepID)] = step
	return step, nil
}

func (f *fakeAnalysisStepStore) childrenReady(step AnalysisStep) bool {
	for _, childID := range step.ChildStepIDs {
		child, ok := f.steps[f.key(step.AnalysisID, childID)]
		if !ok || child.State != domain.AnalysisStepCompleted {
			return false
		}
	}
	return true
}

func (f *fakeAnalysisStepStore) ClaimNext(_ context.Context, analysisID string, now time.Time, lease time.Duration) (AnalysisStep, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, step := range f.steps {
		if step.AnalysisID != analysisID || step.State != domain.AnalysisStepPrepared {
			continue
		}
		if step.Kind == domain.AnalysisStepReduction && !f.childrenReady(step) {
			continue
		}
		step.State = domain.AnalysisStepClaimed
		step.Generation++
		step.LeaseUntil = now.Add(lease)
		f.steps[key] = step
		return step, true, nil
	}
	return AnalysisStep{}, false, nil
}

func (f *fakeAnalysisStepStore) authorize(claim domain.AnalysisStepClaim) (AnalysisStep, error) {
	step, ok := f.steps[f.key(claim.AnalysisID, claim.StepID)]
	if !ok || step.Generation != claim.Generation || step.State != domain.AnalysisStepClaimed {
		return AnalysisStep{}, fmt.Errorf("%w: stale step claim", domain.ErrAnalysisCASConflict)
	}
	return step, nil
}

func (f *fakeAnalysisStepStore) Complete(_ context.Context, claim domain.AnalysisStepClaim, outputDigest string, now time.Time) (AnalysisStep, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	step, err := f.authorize(claim)
	if err != nil {
		return AnalysisStep{}, err
	}
	step.State = domain.AnalysisStepCompleted
	step.OutputDigest = outputDigest
	step.UpdatedAt = now
	f.steps[f.key(claim.AnalysisID, claim.StepID)] = step
	return step, nil
}

func (f *fakeAnalysisStepStore) Retry(_ context.Context, claim domain.AnalysisStepClaim, nextAttempt time.Time, consumeAttempt bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	step, err := f.authorize(claim)
	if err != nil {
		return err
	}
	if consumeAttempt {
		step.Attempt++
	}
	step.State = domain.AnalysisStepPrepared
	f.steps[f.key(claim.AnalysisID, claim.StepID)] = step
	return nil
}

func (f *fakeAnalysisStepStore) Fail(_ context.Context, claim domain.AnalysisStepClaim, code domain.AnalysisFailureCode, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	step, err := f.authorize(claim)
	if err != nil {
		return err
	}
	step.State = domain.AnalysisStepFailed
	step.FailureCode = code
	step.UpdatedAt = now
	f.steps[f.key(claim.AnalysisID, claim.StepID)] = step
	return nil
}

func (f *fakeAnalysisStepStore) List(_ context.Context, analysisID string, afterStepID string, limit int) ([]AnalysisStep, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []AnalysisStep
	for _, step := range f.steps {
		if step.AnalysisID == analysisID && step.StepID > afterStepID {
			out = append(out, step)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

var _ AnalysisStepStore = (*fakeAnalysisStepStore)(nil)

func TestFakeAnalysisStepStoreExpiredClaimantCannotMutateReclaimedStep(t *testing.T) {
	store := newFakeAnalysisStepStore()
	ctx := context.Background()
	step, err := store.Prepare(ctx, AnalysisStep{AnalysisID: "a1", StepID: "leaf-1", Kind: domain.AnalysisStepLeaf})
	if err != nil {
		t.Fatalf("prepare failed: %v", err)
	}
	claimed, ok, err := store.ClaimNext(ctx, "a1", time.Now(), time.Second)
	if err != nil || !ok {
		t.Fatalf("expected a successful claim, got ok=%v err=%v", ok, err)
	}
	staleClaim := domain.AnalysisStepClaim{AnalysisID: "a1", StepID: step.StepID, Generation: claimed.Generation}

	// The lease expires and a new worker reclaims the step.
	if err := store.Retry(ctx, staleClaim, time.Now(), false); err != nil {
		t.Fatalf("retry (reclaim path) failed: %v", err)
	}
	reclaimed, ok, err := store.ClaimNext(ctx, "a1", time.Now(), time.Second)
	if err != nil || !ok {
		t.Fatalf("expected reclaim to succeed, got ok=%v err=%v", ok, err)
	}
	if reclaimed.Generation == staleClaim.Generation {
		t.Fatal("expected the reclaim to advance the generation")
	}

	// The original, now-stale claimant must not be able to complete the step.
	if _, err := store.Complete(ctx, staleClaim, "digest", time.Now()); !errors.Is(err, domain.ErrAnalysisCASConflict) {
		t.Fatalf("expected ErrAnalysisCASConflict for the stale claimant, got %v", err)
	}
}

func TestFakeAnalysisStepStoreReductionClaimableOnlyWhenChildrenComplete(t *testing.T) {
	store := newFakeAnalysisStepStore()
	ctx := context.Background()
	if _, err := store.Prepare(ctx, AnalysisStep{AnalysisID: "a1", StepID: "leaf-1", Kind: domain.AnalysisStepLeaf}); err != nil {
		t.Fatalf("prepare leaf failed: %v", err)
	}
	if _, err := store.Prepare(ctx, AnalysisStep{AnalysisID: "a1", StepID: "reduce-1", Kind: domain.AnalysisStepReduction, ChildStepIDs: []string{"leaf-1"}}); err != nil {
		t.Fatalf("prepare reduction failed: %v", err)
	}

	claimed, ok, err := store.ClaimNext(ctx, "a1", time.Now(), time.Second)
	if err != nil || !ok || claimed.StepID != "leaf-1" {
		t.Fatalf("expected the leaf step to be claimed first, got step=%q ok=%v err=%v", claimed.StepID, ok, err)
	}
	if _, ok, _ := store.ClaimNext(ctx, "a1", time.Now(), time.Second); ok {
		t.Fatal("expected the reduction step to be unclaimable while its child is not completed")
	}
	claim := domain.AnalysisStepClaim{AnalysisID: "a1", StepID: "leaf-1", Generation: claimed.Generation}
	if _, err := store.Complete(ctx, claim, "digest", time.Now()); err != nil {
		t.Fatalf("complete leaf failed: %v", err)
	}
	reduction, ok, err := store.ClaimNext(ctx, "a1", time.Now(), time.Second)
	if err != nil || !ok || reduction.StepID != "reduce-1" {
		t.Fatalf("expected the reduction step to become claimable once its child completed, got step=%q ok=%v err=%v", reduction.StepID, ok, err)
	}
}

// --- fakeResultAnalyzer ---------------------------------------------------

type fakeResultAnalyzer struct{}

func (fakeResultAnalyzer) AnalyzeLeaf(_ context.Context, input AnalysisLeafInput) (domain.AnalysisLeaf, error) {
	if !domain.ValidAnalysisObjectiveClass(string(input.ObjectiveClass)) {
		return domain.AnalysisLeaf{}, fmt.Errorf("%w: unknown objective class", domain.ErrAnalysisValidation)
	}
	digest := domain.AnalysisObjectiveDigest(input.ObjectiveClass, input.ObjectiveText)
	return domain.AnalysisLeaf{
		ObjectiveClass:  input.ObjectiveClass,
		ObjectiveDigest: digest,
		SegmentOrdinal:  input.SegmentOrdinal,
		Findings:        []domain.AnalysisStatement{{Text: "canned finding for " + input.SegmentText}},
	}, nil
}

func (fakeResultAnalyzer) Reduce(_ context.Context, input AnalysisReductionInput) (domain.AnalysisPacket, error) {
	if len(input.Children) == 0 {
		return domain.AnalysisPacket{}, fmt.Errorf("%w: reduction requires at least one child", domain.ErrAnalysisValidation)
	}
	digest := domain.AnalysisObjectiveDigest(input.ObjectiveClass, input.ObjectiveText)
	var findings []domain.AnalysisStatement
	var lineage []string
	for _, child := range input.Children {
		findings = append(findings, child.Findings...)
		lineage = append(lineage, child.StepID)
	}
	return domain.AnalysisPacket{
		ObjectiveClass:  input.ObjectiveClass,
		ObjectiveDigest: digest,
		Findings:        findings,
		SourceSHA256:    strings.Repeat("e", 64),
		Coverage:        domain.AnalysisCoverage{CoveredBytes: 1, Complete: true},
		Lineage:         lineage,
	}, nil
}

var _ ResultAnalyzer = fakeResultAnalyzer{}

func TestFakeResultAnalyzerLeafAndReduceRoundTrip(t *testing.T) {
	analyzer := fakeResultAnalyzer{}
	leaf, err := analyzer.AnalyzeLeaf(context.Background(), AnalysisLeafInput{
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1,
		ObjectiveText:  "which configs use value < 10?",
		SegmentOrdinal: 0,
		SegmentText:    "segment text",
		PromptVersion:  "prompt_v1",
	})
	if err != nil {
		t.Fatalf("analyze leaf failed: %v", err)
	}
	if err := leaf.Validate(); err != nil {
		t.Fatalf("expected a valid leaf, got %v", err)
	}
	packet, err := analyzer.Reduce(context.Background(), AnalysisReductionInput{
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1,
		ObjectiveText:  "which configs use value < 10?",
		Children:       []AnalysisChildSummary{{StepID: "leaf-1", Findings: leaf.Findings}},
		PromptVersion:  "prompt_v1",
	})
	if err != nil {
		t.Fatalf("reduce failed: %v", err)
	}
	if err := packet.Validate(); err != nil {
		t.Fatalf("expected a valid packet, got %v", err)
	}
}

func TestFakeResultAnalyzerReduceRejectsNoChildren(t *testing.T) {
	analyzer := fakeResultAnalyzer{}
	_, err := analyzer.Reduce(context.Background(), AnalysisReductionInput{ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1, ObjectiveText: "objective"})
	if !errors.Is(err, domain.ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation, got %v", err)
	}
}

// --- fakeAnalysisBundleBuilder --------------------------------------------

type fakeAnalysisBundleBuilder struct{}

func (fakeAnalysisBundleBuilder) Build(_ context.Context, analysisID string, packet domain.AnalysisPacket, evidence []AnalysisEvidenceExcerpt, limits domain.AnalysisLimits) (AnalysisBundle, error) {
	if err := packet.Validate(); err != nil {
		return AnalysisBundle{}, err
	}
	var total int64
	for _, excerpt := range evidence {
		total += int64(len(excerpt.Excerpt))
	}
	if total > int64(limits.BundleBytes) {
		return AnalysisBundle{}, fmt.Errorf("%w: bundle of %d bytes exceeds the %d byte limit", domain.ErrAnalysisValidation, total, limits.BundleBytes)
	}
	return AnalysisBundle{
		BundleID: "bundle-" + analysisID, AnalysisID: analysisID,
		Packet: packet, Evidence: evidence, Bytes: total,
		SHA256: strings.Repeat("f", 64),
	}, nil
}

var _ AnalysisBundleBuilder = fakeAnalysisBundleBuilder{}

func TestFakeAnalysisBundleBuilderRejectsOversizedBundle(t *testing.T) {
	builder := fakeAnalysisBundleBuilder{}
	packet := domain.AnalysisPacket{
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1, ObjectiveDigest: strings.Repeat("a", 64),
		Findings: []domain.AnalysisStatement{{Text: "finding"}}, SourceSHA256: strings.Repeat("b", 64),
		Coverage: domain.AnalysisCoverage{CoveredBytes: 1, Complete: true},
	}
	limits := domain.AnalysisLimits{BundleBytes: 10}
	oversized := []AnalysisEvidenceExcerpt{{Excerpt: strings.Repeat("x", 11)}}
	if _, err := builder.Build(context.Background(), "a1", packet, oversized, limits); !errors.Is(err, domain.ErrAnalysisValidation) {
		t.Fatalf("expected ErrAnalysisValidation for an oversized bundle, got %v", err)
	}

	fitting := []AnalysisEvidenceExcerpt{{Excerpt: strings.Repeat("x", 5)}}
	bundle, err := builder.Build(context.Background(), "a1", packet, fitting, limits)
	if err != nil {
		t.Fatalf("expected a fitting bundle to build, got %v", err)
	}
	if bundle.Bytes != 5 {
		t.Fatalf("expected 5 bundle bytes, got %d", bundle.Bytes)
	}
}
