package resultanalysis_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

// fakeReductionStepStore is a minimal in-memory port.AnalysisStepStore.
// RunReduction only ever calls Retry, Fail, and Complete; Prepare,
// ClaimNext, and List are implemented but unused by these tests.
type fakeReductionStepStore struct {
	completedDigest string
	completeCalls   int
	retryCalls      int
	failCalls       int
	failCode        domain.AnalysisFailureCode
}

func (f *fakeReductionStepStore) Prepare(context.Context, port.AnalysisStep) (port.AnalysisStep, error) {
	return port.AnalysisStep{}, errors.New("not implemented")
}
func (f *fakeReductionStepStore) ClaimNext(context.Context, string, time.Time, time.Duration) (port.AnalysisStep, bool, error) {
	return port.AnalysisStep{}, false, errors.New("not implemented")
}
func (f *fakeReductionStepStore) Complete(_ context.Context, _ domain.AnalysisStepClaim, outputDigest string, _ time.Time) (port.AnalysisStep, error) {
	f.completeCalls++
	f.completedDigest = outputDigest
	return port.AnalysisStep{State: domain.AnalysisStepCompleted, OutputDigest: outputDigest}, nil
}
func (f *fakeReductionStepStore) Retry(context.Context, domain.AnalysisStepClaim, time.Time, bool) error {
	f.retryCalls++
	return nil
}
func (f *fakeReductionStepStore) Fail(_ context.Context, _ domain.AnalysisStepClaim, code domain.AnalysisFailureCode, _ time.Time) error {
	f.failCalls++
	f.failCode = code
	return nil
}
func (f *fakeReductionStepStore) List(context.Context, string, string, int) ([]port.AnalysisStep, error) {
	return nil, errors.New("not implemented")
}

// fakePayloadStore is a minimal in-memory port.AnalysisStepPayloadStore.
type fakePayloadStore struct {
	byStep map[string][]byte
}

func newFakePayloadStore() *fakePayloadStore { return &fakePayloadStore{byStep: map[string][]byte{}} }

func (f *fakePayloadStore) WritePayload(_ context.Context, claim domain.AnalysisStepClaim, payload []byte, _ time.Time) error {
	f.byStep[claim.StepID] = append([]byte{}, payload...)
	return nil
}
func (f *fakePayloadStore) ReadPayload(_ context.Context, _ string, stepID string) ([]byte, error) {
	payload, ok := f.byStep[stepID]
	if !ok {
		return nil, domain.ErrAnalysisUnavailable
	}
	return payload, nil
}

// fakeEvidenceLister is a minimal in-memory port.AnalysisEvidenceStore.
type fakeEvidenceLister struct {
	byLeafStep map[string][]port.AnalysisEvidenceExcerpt
}

func newFakeEvidenceLister() *fakeEvidenceLister {
	return &fakeEvidenceLister{byLeafStep: map[string][]port.AnalysisEvidenceExcerpt{}}
}

func (f *fakeEvidenceLister) Write(_ context.Context, _ string, leafStepID string, excerpt port.AnalysisEvidenceExcerpt, _ time.Time) error {
	f.byLeafStep[leafStepID] = append(f.byLeafStep[leafStepID], excerpt)
	return nil
}
func (f *fakeEvidenceLister) ListByLeafStep(_ context.Context, _ string, leafStepID string) ([]port.AnalysisEvidenceExcerpt, error) {
	return f.byLeafStep[leafStepID], nil
}

// fakeReductionAnalyzer returns a configurable packet or error from Reduce.
// AnalyzeLeaf is never used by these tests.
type fakeReductionAnalyzer struct {
	packet domain.AnalysisPacket
	err    error
}

func (f *fakeReductionAnalyzer) AnalyzeLeaf(context.Context, port.AnalysisLeafInput) (domain.AnalysisLeaf, error) {
	return domain.AnalysisLeaf{}, errors.New("not implemented")
}
func (f *fakeReductionAnalyzer) Reduce(context.Context, port.AnalysisReductionInput) (domain.AnalysisPacket, error) {
	return f.packet, f.err
}

func testReductionClaim(stepID string) domain.AnalysisStepClaim {
	return domain.AnalysisStepClaim{AnalysisID: strings.Repeat("a", 64), StepID: stepID, Generation: 0, LeaseUntil: time.Now().Add(time.Minute)}
}

func testReductionIdentity() domain.AnalysisIdentity {
	return domain.AnalysisIdentity{
		SourceResultID: strings.Repeat("a", 64), SourceSHA256: strings.Repeat("b", 64),
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1, ObjectiveDigest: strings.Repeat("c", 64),
		SegmentationVersion: "text_v1", PromptVersion: "reduction_v1", ModelFingerprint: "model_v1",
		LimitsDigest: strings.Repeat("d", 64),
	}
}

func seedLeafChild(t *testing.T, payloads *fakePayloadStore, stepID string, leaf domain.AnalysisLeaf) {
	t.Helper()
	encoded, err := resultanalysis.EncodeLeafPayload(leaf)
	if err != nil {
		t.Fatal(err)
	}
	payloads.byStep[stepID] = encoded
}

// TestRunReductionAssemblesRealSourceDigestCoverageAndLineage is criterion 3:
// the terminal packet must pass domain.AnalysisPacket.Validate() in full,
// with a real source digest, real coverage, and real lineage, not the
// content-only validation the analyzer itself applies.
func TestRunReductionAssemblesRealSourceDigestCoverageAndLineage(t *testing.T) {
	payloads := newFakePayloadStore()
	evidence := newFakeEvidenceLister()
	seedLeafChild(t, payloads, "leaf-0", domain.AnalysisLeaf{
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1, ObjectiveDigest: strings.Repeat("c", 64),
		Findings: []domain.AnalysisStatement{{Text: "finding one"}},
	})
	steps := &fakeReductionStepStore{}
	analyzer := &fakeReductionAnalyzer{packet: domain.AnalysisPacket{
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1, ObjectiveDigest: strings.Repeat("c", 64),
		Findings: []domain.AnalysisStatement{{Text: "combined finding"}},
		Lineage:  []string{"leaf-0"},
	}}
	runner := &resultanalysis.ReductionRunner{Steps: steps, Payloads: payloads, Evidence: evidence, Analyzer: analyzer}

	identity := testReductionIdentity()
	coverage := domain.AnalysisCoverage{CoveredBytes: 100, Complete: true}
	children := []port.AnalysisStep{{StepID: "leaf-0", Kind: domain.AnalysisStepLeaf}}
	claim := testReductionClaim("reduce-1-0")

	packet, err := runner.RunReduction(context.Background(), claim, children, true, identity, "objective text", nil, "reduction_v1", coverage, time.Now())
	if err != nil {
		t.Fatalf("run reduction: %v", err)
	}
	if packet.SourceSHA256 != identity.SourceSHA256 {
		t.Fatalf("packet source sha256 = %q, want %q", packet.SourceSHA256, identity.SourceSHA256)
	}
	if !packet.Coverage.Complete || packet.Coverage.CoveredBytes != 100 {
		t.Fatalf("packet coverage = %+v, want complete with 100 covered bytes", packet.Coverage)
	}
	if err := packet.Validate(); err != nil {
		t.Fatalf("terminal packet failed its own full Validate: %v", err)
	}
	if steps.completeCalls != 1 {
		t.Fatalf("expected exactly one step completion, got %d", steps.completeCalls)
	}
}

// TestRunReductionFailsTypedIrreducibleWhenAnalyzerRejects is criterion 6:
// findings that cannot fit a bounded reduction must fail typed as
// analysis_reduction_irreducible, never silently drop evidence into a
// packet that claims a complete decision.
func TestRunReductionFailsTypedIrreducibleWhenAnalyzerRejects(t *testing.T) {
	payloads := newFakePayloadStore()
	evidence := newFakeEvidenceLister()
	seedLeafChild(t, payloads, "leaf-0", domain.AnalysisLeaf{
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1, ObjectiveDigest: strings.Repeat("c", 64),
		Findings: []domain.AnalysisStatement{{Text: "finding"}},
	})
	steps := &fakeReductionStepStore{}
	analyzer := &fakeReductionAnalyzer{err: errors.New("model rejected the reduction as unbounded")}
	runner := &resultanalysis.ReductionRunner{Steps: steps, Payloads: payloads, Evidence: evidence, Analyzer: analyzer}

	children := []port.AnalysisStep{{StepID: "leaf-0", Kind: domain.AnalysisStepLeaf}}
	claim := testReductionClaim("reduce-1-0")

	_, err := runner.RunReduction(context.Background(), claim, children, false, testReductionIdentity(), "objective", nil, "reduction_v1", domain.AnalysisCoverage{}, time.Now())
	if err != nil {
		t.Fatalf("run reduction: %v", err)
	}
	if steps.failCalls != 1 || steps.failCode != domain.AnalysisFailureReductionIrreducible {
		t.Fatalf("expected one Fail call with analysis_reduction_irreducible, got calls=%d code=%q", steps.failCalls, steps.failCode)
	}
}

// TestRunReductionFailsTypedCoverageIncompleteAtRoot is criterion 7:
// incomplete coverage must never complete the analysis, even when the
// model's own combined output would otherwise validate.
func TestRunReductionFailsTypedCoverageIncompleteAtRoot(t *testing.T) {
	payloads := newFakePayloadStore()
	evidence := newFakeEvidenceLister()
	seedLeafChild(t, payloads, "leaf-0", domain.AnalysisLeaf{
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1, ObjectiveDigest: strings.Repeat("c", 64),
		Findings: []domain.AnalysisStatement{{Text: "finding"}},
	})
	steps := &fakeReductionStepStore{}
	analyzer := &fakeReductionAnalyzer{packet: domain.AnalysisPacket{
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1, ObjectiveDigest: strings.Repeat("c", 64),
		Findings: []domain.AnalysisStatement{{Text: "combined finding"}},
		Lineage:  []string{"leaf-0"},
	}}
	runner := &resultanalysis.ReductionRunner{Steps: steps, Payloads: payloads, Evidence: evidence, Analyzer: analyzer}

	children := []port.AnalysisStep{{StepID: "leaf-0", Kind: domain.AnalysisStepLeaf}}
	claim := testReductionClaim("reduce-1-0")
	incomplete := domain.AnalysisCoverage{CoveredBytes: 50, Complete: false, Gaps: []domain.AnalysisByteRange{{OffsetBytes: 50, LengthBytes: 50}}}

	_, err := runner.RunReduction(context.Background(), claim, children, true, testReductionIdentity(), "objective", nil, "reduction_v1", incomplete, time.Now())
	if err != nil {
		t.Fatalf("run reduction: %v", err)
	}
	if steps.failCalls != 1 || steps.failCode != domain.AnalysisFailureCoverageIncomplete {
		t.Fatalf("expected one Fail call with analysis_coverage_incomplete, got calls=%d code=%q", steps.failCalls, steps.failCode)
	}
}
