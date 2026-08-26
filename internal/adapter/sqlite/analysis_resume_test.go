package sqlite

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

// fixedAnalyzer is a deterministic port.ResultAnalyzer: every leaf and every
// reduction call returns a fixed, content-addressed output derived only
// from its input, with no randomness, so two independent runs over the
// same durable state always produce the same digests. It also counts calls,
// so a test can prove a resumed run never re-invokes it for already
// completed work.
type fixedAnalyzer struct {
	leafCalls   int
	reduceCalls int
}

func (f *fixedAnalyzer) AnalyzeLeaf(_ context.Context, input port.AnalysisLeafInput) (domain.AnalysisLeaf, error) {
	f.leafCalls++
	return domain.AnalysisLeaf{
		ObjectiveClass:  input.ObjectiveClass,
		ObjectiveDigest: domain.AnalysisObjectiveDigest(input.ObjectiveClass, input.ObjectiveText),
		SegmentOrdinal:  input.SegmentOrdinal,
		Findings:        []domain.AnalysisStatement{{Text: "finding for segment " + input.SegmentText[:1]}},
	}, nil
}

func (f *fixedAnalyzer) Reduce(_ context.Context, input port.AnalysisReductionInput) (domain.AnalysisPacket, error) {
	f.reduceCalls++
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

// runAnalysisToCompletion drains every claimable step for analysisID to
// completion, running leaves through resultanalysis.LeafRunner and
// reductions through resultanalysis.ReductionRunner, using the exact same
// durable stores a restarted worker would use. It stops when ClaimNext
// finds nothing left. It returns the root reduction step id (the last
// completed reduction step) and its output digest.
func runAnalysisToCompletion(
	t *testing.T,
	ctx context.Context,
	steps *AnalysisStepStore,
	evidence *AnalysisEvidenceStore,
	source *fakeTrustedResultSourceStore,
	analyzer *fixedAnalyzer,
	analysisID string,
	segments map[string]domain.AnalysisSegment,
	limits domain.AnalysisLimits,
	scope domain.ResultScope,
) (rootStepID string, rootDigest string) {
	t.Helper()
	leafRunner := &resultanalysis.LeafRunner{Steps: steps, Source: source, Analyzer: analyzer, Evidence: evidence, Payloads: steps}
	reductionRunner := &resultanalysis.ReductionRunner{Steps: steps, Payloads: steps, Evidence: evidence, Analyzer: analyzer}
	identity := domain.AnalysisIdentity{
		SourceResultID: source.identity.ResultID, SourceSHA256: source.identity.SHA256,
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1, ObjectiveDigest: domain.AnalysisObjectiveDigest(domain.AnalysisObjectiveBoundedQuestionV1, "objective"),
		SegmentationVersion: "text_v1", PromptVersion: "reduction_v1", ModelFingerprint: "model_v1", LimitsDigest: limits.Digest(),
	}

	for {
		claimed, ok, err := steps.ClaimNext(ctx, analysisID, time.Now().UTC(), time.Minute)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if !ok {
			break
		}
		claim := domain.AnalysisStepClaim{AnalysisID: analysisID, StepID: claimed.StepID, Generation: claimed.Generation, LeaseUntil: claimed.LeaseUntil}
		now := time.Now().UTC()
		if claimed.Kind == domain.AnalysisStepLeaf {
			segment := segments[claimed.StepID]
			input := port.AnalysisLeafInput{
				ObjectiveClass: identity.ObjectiveClass, ObjectiveText: "objective",
				SegmentOrdinal: segment.Ordinal, SegmentText: source.content[segment.OffsetBytes : segment.OffsetBytes+segment.LengthBytes],
				PromptVersion: "leaf_v1",
			}
			if _, _, err := leafRunner.RunLeaf(ctx, claim, segment, input, source.identity.ResultID, scope, limits, now); err != nil {
				t.Fatalf("run leaf %s: %v", claimed.StepID, err)
			}
			continue
		}
		children, err := steps.List(ctx, analysisID, "", 500)
		if err != nil {
			t.Fatalf("list steps: %v", err)
		}
		byID := map[string]port.AnalysisStep{}
		for _, s := range children {
			byID[s.StepID] = s
		}
		childSteps := make([]port.AnalysisStep, 0, len(claimed.ChildStepIDs))
		for _, id := range claimed.ChildStepIDs {
			childSteps = append(childSteps, byID[id])
		}
		isRoot := len(claimed.ChildStepIDs) == len(segments)
		coverage := domain.AnalysisCoverage{}
		if isRoot {
			coverage = domain.AnalysisCoverage{CoveredBytes: int64(len(source.content)), Complete: true}
		}
		packet, err := reductionRunner.RunReduction(ctx, claim, childSteps, isRoot, identity, "objective", nil, "reduction_v1", coverage, now)
		if err != nil {
			t.Fatalf("run reduction %s: %v", claimed.StepID, err)
		}
		if isRoot {
			rootStepID = claimed.StepID
			payload, err := resultanalysis.EncodePacketPayload(packet)
			if err != nil {
				t.Fatal(err)
			}
			rootDigest = testPayloadDigest(payload)
		}
	}
	return rootStepID, rootDigest
}

// fakeTrustedResultSourceStore is a minimal port.TrustedResultStore reused
// across both the interrupted and uninterrupted runs.
type fakeTrustedResultSourceStore struct {
	identity domain.ResultIdentity
	content  string
}

func (f *fakeTrustedResultSourceStore) Materialize(context.Context, port.ResultMaterialization) (domain.ResultHandle, error) {
	return domain.ResultHandle{}, errors.New("not implemented")
}

func (f *fakeTrustedResultSourceStore) Resolve(_ context.Context, resultID string, _ domain.ResultScope) (domain.ResultIdentity, domain.ResultHandle, error) {
	if resultID != f.identity.ResultID {
		return domain.ResultIdentity{}, domain.ResultHandle{}, domain.ErrResultUnavailable
	}
	return f.identity, domain.ResultHandle{}, nil
}

func (f *fakeTrustedResultSourceStore) ReadRange(_ context.Context, resultID string, _ domain.ResultScope, offsetBytes, maxBytes int64) (domain.ResultChunk, error) {
	if resultID != f.identity.ResultID {
		return domain.ResultChunk{}, domain.ErrResultUnavailable
	}
	if offsetBytes >= int64(len(f.content)) {
		return domain.ResultChunk{OffsetBytes: offsetBytes, NextOffsetBytes: offsetBytes, EOF: true, SHA256: f.identity.SHA256}, nil
	}
	end := min(offsetBytes+maxBytes, int64(len(f.content)))
	return domain.ResultChunk{Content: f.content[offsetBytes:end], OffsetBytes: offsetBytes, NextOffsetBytes: end, EOF: end >= int64(len(f.content)), SHA256: f.identity.SHA256}, nil
}

// readStepOutputDigest reads the digest the store actually persisted for
// one step, through the same List call a restarted worker's coverage and
// lineage verification would use. It never trusts a value the test itself
// computed independently.
func readStepOutputDigest(t *testing.T, ctx context.Context, steps *AnalysisStepStore, analysisID, stepID string) string {
	t.Helper()
	all, err := steps.List(ctx, analysisID, "", 500)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	for _, step := range all {
		if step.StepID == stepID {
			return step.OutputDigest
		}
	}
	t.Fatalf("step %s not found in store", stepID)
	return ""
}

func setupResumeFixture(
	t *testing.T,
) (*AnalysisStepStore, *AnalysisEvidenceStore, *fakeTrustedResultSourceStore, string, map[string]domain.AnalysisSegment, domain.AnalysisLimits, domain.ResultScope) {
	t.Helper()
	dbStore, err := Initialize(t.Context(), t.TempDir()+"/analysis-resume.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbStore.Close() })

	content := "aaaaaaaa" + "bbbbbbbb" // two 8-byte segments
	sourceID := strings.Repeat("a", 64)
	sourceSHA := strings.Repeat("b", 64)
	if _, err := dbStore.DB().ExecContext(t.Context(), `INSERT INTO result_records (result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key,
		sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state)
		VALUES (?, 'tool_operation', 'op-1', 0, 'artifact', 'key-1', ?, ?, 'text/plain', 'U1', 'T1', 'slack:T1:dm:U1', 'workspace', 'context', 1, 'available')`,
		sourceID, sourceSHA, len(content)); err != nil {
		t.Fatal(err)
	}

	analysisID := strings.Repeat("c", 64)
	limits := testAnalysisLimits()
	now := time.Now().UTC().Unix()
	if _, err := dbStore.DB().ExecContext(t.Context(), `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'bounded_question_v1', ?, 'objective', 'text_v1', 'leaf_v1', 'model_v1', ?, '{}',
		'U1', 'T1', 'slack:T1:dm:U1', 'workspace', 'ws-1', 'running', ?, ?)`,
		analysisID, sourceID, sourceSHA, len(content), domain.AnalysisObjectiveDigest(domain.AnalysisObjectiveBoundedQuestionV1, "objective"),
		limits.Digest(), now, now); err != nil {
		t.Fatal(err)
	}

	steps := NewAnalysisStepStore(dbStore)
	evidence := NewAnalysisEvidenceStore(dbStore)
	manifest := domain.AnalysisSegmentManifest{
		SourceSHA256: sourceSHA, SourceBytes: int64(len(content)),
		Segments: []domain.AnalysisSegment{
			{Ordinal: 0, OffsetBytes: 0, LengthBytes: 8, SHA256: strings.Repeat("1", 64), SegmenterVersion: "text_v1"},
			{Ordinal: 1, OffsetBytes: 8, LengthBytes: 8, SHA256: strings.Repeat("2", 64), SegmenterVersion: "text_v1", OverlapPrevBytes: 0},
		},
	}
	prepared, err := resultanalysis.PrepareSteps(analysisID, manifest, limits, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	segmentsByStep := map[string]domain.AnalysisSegment{}
	for _, step := range prepared {
		if _, err := steps.Prepare(t.Context(), step); err != nil {
			t.Fatalf("prepare %s: %v", step.StepID, err)
		}
		if step.Kind == domain.AnalysisStepLeaf {
			segmentsByStep[step.StepID] = manifest.Segments[step.SegmentOrdinal]
		}
	}

	source := &fakeTrustedResultSourceStore{identity: domain.ResultIdentity{ResultID: sourceID, SHA256: sourceSHA, Bytes: int64(len(content)), MediaType: "text/plain"}, content: content}
	scope := domain.ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:U1", Project: "workspace"}
	return steps, evidence, source, analysisID, segmentsByStep, limits, scope
}

// TestAnalysisResumeAfterRestartProducesIdenticalTerminalPacketDigest is
// criterion 10: an analysis interrupted mid-way (one leaf completed, the
// other claimed but never finished, simulating a crash before its lease
// expires) and then resumed by a fresh claim loop over the same durable
// store must produce the identical terminal packet digest that an
// uninterrupted run over the same manifest produces, without ever
// re-invoking the analyzer for the leaf that had already completed.
func TestAnalysisResumeAfterRestartProducesIdenticalTerminalPacketDigest(t *testing.T) {
	ctx := t.Context()

	// Uninterrupted baseline run, in its own database.
	baseSteps, baseEvidence, baseSource, baseAnalysisID, baseSegments, limits, scope := setupResumeFixture(t)
	baseAnalyzer := &fixedAnalyzer{}
	baseRootStepID, baselineDigest := runAnalysisToCompletion(t, ctx, baseSteps, baseEvidence, baseSource, baseAnalyzer, baseAnalysisID, baseSegments, limits, scope)
	if baselineDigest == "" {
		t.Fatal("baseline run produced no root digest")
	}
	// FIND-105: compare the digest the store actually persisted for the
	// root step, not only the digest a test independently recomputes from
	// the payload. This is the value that ends up in
	// result_representations.payload_sha256, the identity a downstream
	// consumer verifies against per TRD 02, so a store that persisted
	// something other than sha256(payload) must fail here even though a
	// digest recomputed straight from the payload would still match itself.
	basePersistedDigest := readStepOutputDigest(t, ctx, baseSteps, baseAnalysisID, baseRootStepID)
	if basePersistedDigest != baselineDigest {
		t.Fatalf("baseline persisted step digest = %q, want %q (sha256 of the stored payload)", basePersistedDigest, baselineDigest)
	}

	// Interrupted-and-resumed run, in a second database with the same
	// deterministic manifest and content.
	steps, evidence, source, analysisID, segments, _, _ := setupResumeFixture(t)
	analyzer := &fixedAnalyzer{}
	leafRunner := &resultanalysis.LeafRunner{Steps: steps, Source: source, Analyzer: analyzer, Evidence: evidence, Payloads: steps}

	// Complete leaf-0 normally.
	claimed, ok, err := steps.ClaimNext(ctx, analysisID, time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim leaf-0: ok=%v err=%v", ok, err)
	}
	claim := domain.AnalysisStepClaim{AnalysisID: analysisID, StepID: claimed.StepID, Generation: claimed.Generation, LeaseUntil: claimed.LeaseUntil}
	segment := segments[claimed.StepID]
	input := port.AnalysisLeafInput{
		ObjectiveClass: domain.AnalysisObjectiveBoundedQuestionV1, ObjectiveText: "objective",
		SegmentOrdinal: segment.Ordinal, SegmentText: source.content[segment.OffsetBytes : segment.OffsetBytes+segment.LengthBytes], PromptVersion: "leaf_v1",
	}
	if _, _, err := leafRunner.RunLeaf(ctx, claim, segment, input, source.identity.ResultID, scope, limits, time.Now().UTC()); err != nil {
		t.Fatalf("run leaf-0: %v", err)
	}

	// Claim leaf-1 but simulate a crash: never call RunLeaf, and let its
	// lease expire so a resumed worker can reclaim it.
	crashedClaim, ok, err := steps.ClaimNext(ctx, analysisID, time.Now().UTC(), time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("claim leaf-1 (to crash): ok=%v err=%v", ok, err)
	}
	if crashedClaim.Kind != domain.AnalysisStepLeaf {
		t.Fatalf("expected to claim the remaining leaf, got kind %q", crashedClaim.Kind)
	}
	time.Sleep(2 * time.Millisecond) // let the short lease expire

	// Resume: drain everything left through the ordinary claim loop.
	resumedRootStepID, resumedDigest := runAnalysisToCompletion(t, ctx, steps, evidence, source, analyzer, analysisID, segments, limits, scope)
	if resumedDigest == "" {
		t.Fatal("resumed run produced no root digest")
	}

	if resumedDigest != baselineDigest {
		t.Fatalf("resumed terminal packet digest = %q, want identical to the uninterrupted run's %q", resumedDigest, baselineDigest)
	}

	// FIND-105: same check on the resumed run's persisted digest, and cross-
	// check the two persisted (store-read) digests directly against each
	// other, not only their independently recomputed counterparts.
	resumedPersistedDigest := readStepOutputDigest(t, ctx, steps, analysisID, resumedRootStepID)
	if resumedPersistedDigest != resumedDigest {
		t.Fatalf("resumed persisted step digest = %q, want %q (sha256 of the stored payload)", resumedPersistedDigest, resumedDigest)
	}
	if resumedPersistedDigest != basePersistedDigest {
		t.Fatalf("resumed persisted step digest = %q, want identical to the baseline persisted digest %q", resumedPersistedDigest, basePersistedDigest)
	}
	// leaf-0 was never re-run by the resumed pass, only reclaimed leaf-1
	// plus the root reduction: two leaf calls (leaf-0 pre-crash, leaf-1
	// post-resume) and one reduction call total for the whole run.
	if analyzer.leafCalls != 2 {
		t.Fatalf("leaf analyzer calls = %d, want 2 (leaf-0 once, leaf-1 once after resume, never leaf-0 again)", analyzer.leafCalls)
	}
	if analyzer.reduceCalls != 1 {
		t.Fatalf("reduce analyzer calls = %d, want 1", analyzer.reduceCalls)
	}
}
