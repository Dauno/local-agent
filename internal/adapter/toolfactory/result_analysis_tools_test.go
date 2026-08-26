package toolfactory_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/toolfactory"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	resultanalysisusecase "github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

// fakeAnalysisSourceStore is a minimal port.TrustedResultStore: it resolves
// exactly one fixed identity when both the result id and the scope match,
// otherwise it fails exactly like a real store would for a cross-scope
// request: indistinguishable from an unknown result.
type fakeAnalysisSourceStore struct {
	identity domain.ResultIdentity
	scope    domain.ResultScope
}

func (f *fakeAnalysisSourceStore) Materialize(context.Context, port.ResultMaterialization) (domain.ResultHandle, error) {
	return domain.ResultHandle{}, errors.New("not implemented")
}

func (f *fakeAnalysisSourceStore) Resolve(_ context.Context, resultID string, scope domain.ResultScope) (domain.ResultIdentity, domain.ResultHandle, error) {
	if resultID != f.identity.ResultID || scope != f.scope {
		return domain.ResultIdentity{}, domain.ResultHandle{}, domain.ErrResultUnavailable
	}
	return f.identity, domain.ResultHandle{}, nil
}

func (f *fakeAnalysisSourceStore) ReadRange(context.Context, string, domain.ResultScope, int64, int64) (domain.ResultChunk, error) {
	return domain.ResultChunk{}, errors.New("not implemented")
}

// fakeAnalysisStore is an in-memory port.AnalysisStore. Get filters by exact
// scope match, exactly like the real store's documented contract: a
// cross-scope read is indistinguishable from a missing row.
type fakeAnalysisStore struct {
	byID map[string]port.AnalysisRecord
}

func newFakeAnalysisStore() *fakeAnalysisStore {
	return &fakeAnalysisStore{byID: map[string]port.AnalysisRecord{}}
}

func (f *fakeAnalysisStore) Create(
	_ context.Context,
	identity domain.AnalysisIdentity,
	_ domain.AnalysisLimits,
	objectiveText string,
	scope domain.ResultScope,
	workstreamID string,
	now time.Time,
) (port.AnalysisRecord, error) {
	digest := identity.SemanticDigest()
	for _, record := range f.byID {
		if record.Identity.SemanticDigest() == digest {
			return record, nil
		}
	}
	record := port.AnalysisRecord{
		AnalysisID: digest, Identity: identity, ObjectiveText: objectiveText, Scope: scope,
		WorkstreamID: workstreamID, State: domain.AnalysisPreparing, CreatedAt: now, UpdatedAt: now,
	}
	f.byID[record.AnalysisID] = record
	return record, nil
}

func (f *fakeAnalysisStore) Get(_ context.Context, analysisID string, scope domain.ResultScope) (port.AnalysisRecord, error) {
	record, ok := f.byID[analysisID]
	if !ok || record.Scope != scope {
		return port.AnalysisRecord{}, domain.ErrAnalysisUnavailable
	}
	return record, nil
}

func (f *fakeAnalysisStore) Complete(_ context.Context, analysisID string, scope domain.ResultScope, now time.Time) (port.AnalysisRecord, error) {
	record, err := f.Get(context.Background(), analysisID, scope)
	if err != nil {
		return port.AnalysisRecord{}, err
	}
	record.State = domain.AnalysisCompleted
	record.UpdatedAt = now
	f.byID[analysisID] = record
	return record, nil
}

func (f *fakeAnalysisStore) Fail(_ context.Context, analysisID string, scope domain.ResultScope, code domain.AnalysisFailureCode, now time.Time) (port.AnalysisRecord, error) {
	record, err := f.Get(context.Background(), analysisID, scope)
	if err != nil {
		return port.AnalysisRecord{}, err
	}
	record.State = domain.AnalysisFailed
	record.FailureCode = code
	record.UpdatedAt = now
	f.byID[analysisID] = record
	return record, nil
}

// fakeAnalysisStepStore only implements List meaningfully; the fixture
// populates steps directly, and these tool tests never claim or complete a
// step through the store.
type fakeAnalysisStepStore struct {
	steps []port.AnalysisStep
}

func (f *fakeAnalysisStepStore) Prepare(context.Context, port.AnalysisStep) (port.AnalysisStep, error) {
	return port.AnalysisStep{}, errors.New("not implemented")
}

func (f *fakeAnalysisStepStore) ClaimNext(context.Context, string, time.Time, time.Duration) (port.AnalysisStep, bool, error) {
	return port.AnalysisStep{}, false, errors.New("not implemented")
}

func (f *fakeAnalysisStepStore) Complete(context.Context, domain.AnalysisStepClaim, string, time.Time) (port.AnalysisStep, error) {
	return port.AnalysisStep{}, errors.New("not implemented")
}

func (f *fakeAnalysisStepStore) Retry(context.Context, domain.AnalysisStepClaim, time.Time, bool) error {
	return errors.New("not implemented")
}

func (f *fakeAnalysisStepStore) Fail(context.Context, domain.AnalysisStepClaim, domain.AnalysisFailureCode, time.Time) error {
	return errors.New("not implemented")
}

func (f *fakeAnalysisStepStore) List(_ context.Context, analysisID string, _ string, _ int) ([]port.AnalysisStep, error) {
	var out []port.AnalysisStep
	for _, step := range f.steps {
		if step.AnalysisID == analysisID {
			out = append(out, step)
		}
	}
	return out, nil
}

// fakeAnalysisEvidenceStore is an in-memory port.AnalysisEvidenceStore.
type fakeAnalysisEvidenceStore struct {
	byLeafStep map[string][]port.AnalysisEvidenceExcerpt
}

func newFakeAnalysisEvidenceStore() *fakeAnalysisEvidenceStore {
	return &fakeAnalysisEvidenceStore{byLeafStep: map[string][]port.AnalysisEvidenceExcerpt{}}
}

func (f *fakeAnalysisEvidenceStore) Write(_ context.Context, _ string, leafStepID string, excerpt port.AnalysisEvidenceExcerpt, _ time.Time) error {
	f.byLeafStep[leafStepID] = append(f.byLeafStep[leafStepID], excerpt)
	return nil
}

func (f *fakeAnalysisEvidenceStore) ListByLeafStep(_ context.Context, _ string, leafStepID string) ([]port.AnalysisEvidenceExcerpt, error) {
	return f.byLeafStep[leafStepID], nil
}

// fakeAnalysisStepPayloadStore is an in-memory port.AnalysisStepPayloadStore.
type fakeAnalysisStepPayloadStore struct {
	byStep map[string][]byte
}

func newFakeAnalysisStepPayloadStore() *fakeAnalysisStepPayloadStore {
	return &fakeAnalysisStepPayloadStore{byStep: map[string][]byte{}}
}

func (f *fakeAnalysisStepPayloadStore) WritePayload(_ context.Context, claim domain.AnalysisStepClaim, payload []byte, _ time.Time) error {
	f.byStep[claim.StepID] = payload
	return nil
}

func (f *fakeAnalysisStepPayloadStore) ReadPayload(_ context.Context, _ string, stepID string) ([]byte, error) {
	payload, ok := f.byStep[stepID]
	if !ok {
		return nil, errors.New("no stored payload")
	}
	return payload, nil
}

// spyAnalyzer records whether either analyzer method was ever invoked. It is
// used to prove workstream_request_result_analysis makes no model call
// inline (criterion 4): a call to either method here would be a defect.
type spyAnalyzer struct {
	called bool
}

func (s *spyAnalyzer) AnalyzeLeaf(context.Context, port.AnalysisLeafInput) (domain.AnalysisLeaf, error) {
	s.called = true
	return domain.AnalysisLeaf{}, errors.New("spy analyzer must never be called by a tool")
}

func (s *spyAnalyzer) Reduce(context.Context, port.AnalysisReductionInput) (domain.AnalysisPacket, error) {
	s.called = true
	return domain.AnalysisPacket{}, errors.New("spy analyzer must never be called by a tool")
}

const (
	trustedActor   = "U12345678"
	trustedProject = "workspace"
)

var trustedKey = domain.ConversationKey("slack:T12345678:dm:D12345678")

func trustedScope() domain.ResultScope {
	return domain.ResultScope{Actor: trustedActor, TeamID: "T12345678", ConversationKey: string(trustedKey), Project: trustedProject}
}

// resultAnalysisFixture wires a resultanalysisusecase.Service over the fakes
// above, plus a Factory built with WithResultAnalysis. The source result is
// resolvable by trustedScope() only, and analysisID names one already-
// completed analysis whose root reduction step payload and evidence are
// pre-populated, so status/packet tests never need to drive the store
// through a full leaf/reduction run.
type resultAnalysisFixture struct {
	factory    *toolfactory.Factory
	analyzer   *spyAnalyzer
	analyses   *fakeAnalysisStore
	analysisID string
	resultID   string
}

func newResultAnalysisFixture(t *testing.T) resultAnalysisFixture {
	t.Helper()
	resultID := strings.Repeat("a", 64)
	sourceSHA := strings.Repeat("b", 64)
	identity := domain.ResultIdentity{ResultID: resultID, SHA256: sourceSHA, Bytes: 16, MediaType: "text/plain", Scope: trustedScope()}
	source := &fakeAnalysisSourceStore{identity: identity, scope: trustedScope()}

	limits := domain.AnalysisLimits{
		MaxSegmentBytes: 120, OverlapBasisPoints: 2000, OverlapMaxBytes: 40, MaxLeaves: 64,
		MaxReductionFanIn: 8, MaxReductionDepth: 4, MaxConcurrentLeaves: 2, MaxAttemptsPerStep: 2,
		CallTimeoutSeconds: 120, WallTimeSeconds: 900, EvidenceExcerptBytes: 2048,
		EvidenceSelectorsPerLeaf: 8, EvidenceReferencesPerPacket: 32, BundleBytes: 32768,
	}
	analyses := newFakeAnalysisStore()
	steps := &fakeAnalysisStepStore{}
	evidence := newFakeAnalysisEvidenceStore()
	payloads := newFakeAnalysisStepPayloadStore()
	analyzer := &spyAnalyzer{}

	svc, err := resultanalysisusecase.NewService(
		resultanalysisusecase.ServiceConfig{SegmentationVersion: "text_v1", PromptVersion: "leaf_v1", ModelFingerprint: "model_v1", Limits: limits},
		resultanalysisusecase.ServiceDependencies{Source: source, Analyses: analyses, Steps: steps, Evidence: evidence, Payloads: payloads, Analyzer: analyzer},
	)
	if err != nil {
		t.Fatalf("build result analysis service: %v", err)
	}

	// Seed one completed analysis with one leaf step (and its evidence) and
	// one root reduction step (with its packet payload).
	objectiveDigest := domain.AnalysisObjectiveDigest(domain.AnalysisObjectiveBoundedQuestionV1, "objective text")
	identityForRow := domain.AnalysisIdentity{
		SourceResultID: resultID, SourceSHA256: sourceSHA, ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1,
		ObjectiveDigest: objectiveDigest, SegmentationVersion: "text_v1", PromptVersion: "leaf_v1",
		ModelFingerprint: "model_v1", LimitsDigest: limits.Digest(),
	}
	analysisID := identityForRow.SemanticDigest()
	analyses.byID[analysisID] = port.AnalysisRecord{
		AnalysisID: analysisID, Identity: identityForRow, ObjectiveText: "objective text", Scope: trustedScope(),
		WorkstreamID: "ws-1", State: domain.AnalysisCompleted, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}

	leafStepID := strings.Repeat("1", 64)
	rootStepID := strings.Repeat("2", 64)
	evidenceID := strings.Repeat("3", 64)
	steps.steps = []port.AnalysisStep{
		{AnalysisID: analysisID, StepID: leafStepID, Kind: domain.AnalysisStepLeaf, State: domain.AnalysisStepCompleted, SegmentOrdinal: 0},
		{AnalysisID: analysisID, StepID: rootStepID, Kind: domain.AnalysisStepReduction, State: domain.AnalysisStepCompleted, ChildStepIDs: []string{leafStepID}},
	}
	if err := evidence.Write(t.Context(), analysisID, leafStepID, port.AnalysisEvidenceExcerpt{
		EvidenceID: evidenceID, SegmentOrdinal: 0, Range: domain.AnalysisByteRange{OffsetBytes: 0, LengthBytes: 4}, SHA256: strings.Repeat("c", 64), Excerpt: "abcd",
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	packet := domain.AnalysisPacket{
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1, ObjectiveDigest: objectiveDigest,
		Findings: []domain.AnalysisStatement{{Text: "the answer is bounded"}}, EvidenceRefs: []string{evidenceID},
		SourceSHA256: sourceSHA, Coverage: domain.AnalysisCoverage{CoveredBytes: 16, Complete: true},
	}
	payload, err := resultanalysisusecase.EncodePacketPayload(packet)
	if err != nil {
		t.Fatal(err)
	}
	payloads.byStep[rootStepID] = payload

	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithResultAnalysis(svc)
	return resultAnalysisFixture{factory: factory, analyzer: analyzer, analyses: analyses, analysisID: analysisID, resultID: resultID}
}

func resultAnalysisToolsByName(t *testing.T, factory *toolfactory.Factory, actor string, key domain.ConversationKey) map[string]runnableFunctionTool {
	t.Helper()
	tools, err := factory.ToolsForInvocation(actor, key)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]runnableFunctionTool)
	for _, raw := range tools {
		if candidate, ok := raw.(runnableFunctionTool); ok {
			byName[candidate.Name()] = candidate
		}
	}
	return byName
}

// TestResultAnalysisToolsHiddenWhenDependencyIsNil is criterion 3.
func TestResultAnalysisToolsHiddenWhenDependencyIsNil(t *testing.T) {
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil)
	tools, err := factory.ToolsForInvocation(trustedActor, trustedKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range tools {
		if named, ok := raw.(interface{ Name() string }); ok {
			for _, forbidden := range []string{"workstream_request_result_analysis", "workstream_analysis_status", "workstream_read_analysis_packet"} {
				if named.Name() == forbidden {
					t.Fatalf("%s is exposed while result analysis is not configured", forbidden)
				}
			}
		}
	}
}

// TestResultAnalysisToolsAreRegisteredWhenConfigured checks all three tools
// appear once the dependency is non-nil.
func TestResultAnalysisToolsAreRegisteredWhenConfigured(t *testing.T) {
	fx := newResultAnalysisFixture(t)
	byName := resultAnalysisToolsByName(t, fx.factory, trustedActor, trustedKey)
	for _, name := range []string{"workstream_request_result_analysis", "workstream_analysis_status", "workstream_read_analysis_packet"} {
		if byName[name] == nil {
			t.Fatalf("%s was not registered", name)
		}
	}
}

// TestResultAnalysisRequestToolMakesNoModelCallInline is criterion 4.
func TestResultAnalysisRequestToolMakesNoModelCallInline(t *testing.T) {
	fx := newResultAnalysisFixture(t)
	byName := resultAnalysisToolsByName(t, fx.factory, trustedActor, trustedKey)
	result, err := byName["workstream_request_result_analysis"].Run(&stubToolContext{callID: "req-1"}, map[string]any{
		"result_id": fx.resultID, "project": trustedProject, "workstream_id": "ws-1", "objective": "objective text",
	})
	if err != nil {
		t.Fatalf("request analysis: %v", err)
	}
	if result["analysis_id"] == "" || result["analysis_id"] == nil {
		t.Fatalf("expected a non-empty analysis id, got %+v", result)
	}
	if fx.analyzer.called {
		t.Fatal("workstream_request_result_analysis invoked the analyzer inline")
	}
}

// TestResultAnalysisPacketToolRejectsIncompleteAnalysis is criterion 5.
func TestResultAnalysisPacketToolRejectsIncompleteAnalysis(t *testing.T) {
	fx := newResultAnalysisFixture(t)
	running := fx.analyses.byID[fx.analysisID]
	running.State = domain.AnalysisRunning
	fx.analyses.byID[fx.analysisID] = running

	byName := resultAnalysisToolsByName(t, fx.factory, trustedActor, trustedKey)
	if _, err := byName["workstream_read_analysis_packet"].Run(&stubToolContext{callID: "packet-1"}, map[string]any{
		"analysis_id": fx.analysisID, "project": trustedProject,
	}); err == nil {
		t.Fatal("expected a running analysis to be rejected by workstream_read_analysis_packet")
	}
}

// TestResultAnalysisPacketToolReturnsPacketAndEvidenceWhenCompleted is the
// positive counterpart to the rejection test above, and exercises the
// evidence join.
func TestResultAnalysisPacketToolReturnsPacketAndEvidenceWhenCompleted(t *testing.T) {
	fx := newResultAnalysisFixture(t)
	byName := resultAnalysisToolsByName(t, fx.factory, trustedActor, trustedKey)
	result, err := byName["workstream_read_analysis_packet"].Run(&stubToolContext{callID: "packet-1"}, map[string]any{
		"analysis_id": fx.analysisID, "project": trustedProject,
	})
	if err != nil {
		t.Fatalf("read packet: %v", err)
	}
	findings, _ := result["findings"].([]any)
	if len(findings) != 1 {
		t.Fatalf("expected one finding, got %+v", result["findings"])
	}
	evidence, _ := result["evidence"].([]any)
	if len(evidence) != 1 {
		t.Fatalf("expected one evidence excerpt, got %+v", result["evidence"])
	}
	coverageText, _ := result["coverage_text"].(string)
	if !strings.Contains(coverageText, "verified") {
		t.Fatalf("expected coverage text to report verified bytes, got %q", coverageText)
	}
}

// TestResultAnalysisStatusToolReportsStepCounts checks the status tool
// surfaces step progress without any source content.
func TestResultAnalysisStatusToolReportsStepCounts(t *testing.T) {
	fx := newResultAnalysisFixture(t)
	byName := resultAnalysisToolsByName(t, fx.factory, trustedActor, trustedKey)
	result, err := byName["workstream_analysis_status"].Run(&stubToolContext{callID: "status-1"}, map[string]any{
		"analysis_id": fx.analysisID, "project": trustedProject,
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if result["state"] != "completed" {
		t.Fatalf("expected completed state, got %+v", result["state"])
	}
	if leafTotal, _ := result["leaf_total"].(float64); leafTotal != 1 {
		t.Fatalf("expected leaf_total 1, got %+v", result["leaf_total"])
	}
}

// The following four tests are criterion 2: a hostile argument along each
// of actor, team, conversation, and project must never widen scope past the
// trusted invocation. Each drives workstream_analysis_status, which is
// representative of the scope resolution all three tools share through
// resultAnalysisScope.

func TestResultAnalysisStatusToolRejectsCrossActorInvocation(t *testing.T) {
	fx := newResultAnalysisFixture(t)
	byName := resultAnalysisToolsByName(t, fx.factory, "U_ATTACKER", trustedKey)
	if _, err := byName["workstream_analysis_status"].Run(&stubToolContext{callID: "hostile-actor"}, map[string]any{
		"analysis_id": fx.analysisID, "project": trustedProject,
	}); err == nil {
		t.Fatal("expected a cross-actor invocation to be rejected")
	}
}

func TestResultAnalysisStatusToolRejectsCrossTeamInvocation(t *testing.T) {
	fx := newResultAnalysisFixture(t)
	hostileKey := domain.ConversationKey("slack:T_ATTACKER:dm:D12345678")
	byName := resultAnalysisToolsByName(t, fx.factory, trustedActor, hostileKey)
	if _, err := byName["workstream_analysis_status"].Run(&stubToolContext{callID: "hostile-team"}, map[string]any{
		"analysis_id": fx.analysisID, "project": trustedProject,
	}); err == nil {
		t.Fatal("expected a cross-team invocation to be rejected")
	}
}

func TestResultAnalysisStatusToolRejectsCrossConversationInvocation(t *testing.T) {
	fx := newResultAnalysisFixture(t)
	hostileKey := domain.ConversationKey("slack:T12345678:dm:D_ATTACKER")
	byName := resultAnalysisToolsByName(t, fx.factory, trustedActor, hostileKey)
	if _, err := byName["workstream_analysis_status"].Run(&stubToolContext{callID: "hostile-conversation"}, map[string]any{
		"analysis_id": fx.analysisID, "project": trustedProject,
	}); err == nil {
		t.Fatal("expected a cross-conversation invocation to be rejected")
	}
}

func TestResultAnalysisStatusToolRejectsCrossProjectArgument(t *testing.T) {
	fx := newResultAnalysisFixture(t)
	byName := resultAnalysisToolsByName(t, fx.factory, trustedActor, trustedKey)
	if _, err := byName["workstream_analysis_status"].Run(&stubToolContext{callID: "hostile-project"}, map[string]any{
		"analysis_id": fx.analysisID, "project": "someone-elses-project",
	}); err == nil {
		t.Fatal("expected a cross-project argument to be rejected")
	}
}
