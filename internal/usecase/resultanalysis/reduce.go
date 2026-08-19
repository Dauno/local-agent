package resultanalysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// leafPayloadJSON and packetPayloadJSON are the deterministic wire shapes
// this package writes into a completed step's output_payload
// (port.AnalysisStepPayloadStore), so a restarted worker can rebuild a
// reduction's child summaries from durable storage instead of replaying any
// model call. encoding/json.Marshal on these types is deterministic because
// neither has a map field or a custom MarshalJSON.
type leafPayloadJSON struct {
	ObjectiveClass      string   `json:"objective_class"`
	ObjectiveDigest     string   `json:"objective_digest"`
	SegmentOrdinal      int      `json:"segment_ordinal"`
	Findings            []string `json:"findings"`
	Constraints         []string `json:"constraints"`
	Contradictions      []string `json:"contradictions"`
	UnresolvedQuestions []string `json:"unresolved_questions"`
}

// EncodeLeafPayload serializes the structured fields of leaf that a
// reduction needs, in the fixed shape leafPayloadJSON declares. Evidence
// selectors are not included: they are host-resolved and durable in
// analysis_evidence, addressed by evidence id, not re-encoded here.
func EncodeLeafPayload(leaf domain.AnalysisLeaf) ([]byte, error) {
	payload := leafPayloadJSON{
		ObjectiveClass:  string(leaf.ObjectiveClass),
		ObjectiveDigest: leaf.ObjectiveDigest,
		SegmentOrdinal:  leaf.SegmentOrdinal,
	}
	for _, f := range leaf.Findings {
		payload.Findings = append(payload.Findings, f.Text)
	}
	for _, c := range leaf.Constraints {
		payload.Constraints = append(payload.Constraints, c.Text)
	}
	for _, c := range leaf.Contradictions {
		payload.Contradictions = append(payload.Contradictions, c.Text)
	}
	for _, q := range leaf.UnresolvedQuestions {
		payload.UnresolvedQuestions = append(payload.UnresolvedQuestions, q.Text)
	}
	return json.Marshal(payload)
}

// DecodeLeafPayload is the inverse of EncodeLeafPayload.
func DecodeLeafPayload(raw []byte) (domain.AnalysisLeaf, error) {
	var payload leafPayloadJSON
	if err := json.Unmarshal(raw, &payload); err != nil {
		return domain.AnalysisLeaf{}, fmt.Errorf("%w: leaf payload is not valid JSON: %v", domain.ErrAnalysisValidation, err)
	}
	leaf := domain.AnalysisLeaf{
		ObjectiveClass:  domain.AnalysisObjectiveClass(payload.ObjectiveClass),
		ObjectiveDigest: payload.ObjectiveDigest,
		SegmentOrdinal:  payload.SegmentOrdinal,
	}
	for _, text := range payload.Findings {
		leaf.Findings = append(leaf.Findings, domain.AnalysisStatement{Text: text})
	}
	for _, text := range payload.Constraints {
		leaf.Constraints = append(leaf.Constraints, domain.AnalysisStatement{Text: text})
	}
	for _, text := range payload.Contradictions {
		leaf.Contradictions = append(leaf.Contradictions, domain.AnalysisContradiction{Text: text})
	}
	for _, text := range payload.UnresolvedQuestions {
		leaf.UnresolvedQuestions = append(leaf.UnresolvedQuestions, domain.AnalysisStatement{Text: text})
	}
	return leaf, nil
}

type packetContradictionJSON struct {
	Text      string   `json:"text"`
	LeafSteps []string `json:"leaf_steps"`
}

type packetPayloadJSON struct {
	ObjectiveClass      string                    `json:"objective_class"`
	ObjectiveDigest     string                    `json:"objective_digest"`
	Findings            []string                  `json:"findings"`
	Constraints         []string                  `json:"constraints"`
	Contradictions      []packetContradictionJSON `json:"contradictions"`
	UnresolvedQuestions []string                  `json:"unresolved_questions"`
	EvidenceRefs        []string                  `json:"evidence_refs"`
	SourceSHA256        string                    `json:"source_sha256"`
	CoveredBytes        int64                     `json:"covered_bytes"`
	CoverageComplete    bool                      `json:"coverage_complete"`
	Lineage             []string                  `json:"lineage"`
}

// EncodePacketPayload serializes a complete domain.AnalysisPacket
// (analysis_packet_v1) for durable storage in a reduction step's
// output_payload. Gaps are not persisted: a completed packet's own
// Coverage.Complete flag is the durable claim; the underlying gap list is
// reconstructible from the segment manifest whenever it is needed again.
func EncodePacketPayload(packet domain.AnalysisPacket) ([]byte, error) {
	payload := packetPayloadJSON{
		ObjectiveClass:   string(packet.ObjectiveClass),
		ObjectiveDigest:  packet.ObjectiveDigest,
		SourceSHA256:     packet.SourceSHA256,
		CoveredBytes:     packet.Coverage.CoveredBytes,
		CoverageComplete: packet.Coverage.Complete,
		EvidenceRefs:     packet.EvidenceRefs,
		Lineage:          packet.Lineage,
	}
	for _, f := range packet.Findings {
		payload.Findings = append(payload.Findings, f.Text)
	}
	for _, c := range packet.Constraints {
		payload.Constraints = append(payload.Constraints, c.Text)
	}
	for _, c := range packet.Contradictions {
		payload.Contradictions = append(payload.Contradictions, packetContradictionJSON{Text: c.Text, LeafSteps: c.LeafSteps})
	}
	for _, q := range packet.UnresolvedQuestions {
		payload.UnresolvedQuestions = append(payload.UnresolvedQuestions, q.Text)
	}
	return json.Marshal(payload)
}

// DecodePacketPayload is the inverse of EncodePacketPayload.
func DecodePacketPayload(raw []byte) (domain.AnalysisPacket, error) {
	var payload packetPayloadJSON
	if err := json.Unmarshal(raw, &payload); err != nil {
		return domain.AnalysisPacket{}, fmt.Errorf("%w: packet payload is not valid JSON: %v", domain.ErrAnalysisValidation, err)
	}
	packet := domain.AnalysisPacket{
		ObjectiveClass:  domain.AnalysisObjectiveClass(payload.ObjectiveClass),
		ObjectiveDigest: payload.ObjectiveDigest,
		EvidenceRefs:    payload.EvidenceRefs,
		SourceSHA256:    payload.SourceSHA256,
		Coverage:        domain.AnalysisCoverage{CoveredBytes: payload.CoveredBytes, Complete: payload.CoverageComplete},
		Lineage:         payload.Lineage,
	}
	for _, text := range payload.Findings {
		packet.Findings = append(packet.Findings, domain.AnalysisStatement{Text: text})
	}
	for _, text := range payload.Constraints {
		packet.Constraints = append(packet.Constraints, domain.AnalysisStatement{Text: text})
	}
	for _, c := range payload.Contradictions {
		packet.Contradictions = append(packet.Contradictions, domain.AnalysisContradiction{Text: c.Text, LeafSteps: c.LeafSteps})
	}
	for _, text := range payload.UnresolvedQuestions {
		packet.UnresolvedQuestions = append(packet.UnresolvedQuestions, domain.AnalysisStatement{Text: text})
	}
	return packet, nil
}

// ReductionRunner ties one claimed reduction step to the no-tool analyzer,
// child-summary reconstruction from durable payloads, and the matching
// durable step transition. It never re-reads source text: every child's
// typed findings are already durable, either as a leaf's own payload plus
// its resolved evidence, or as a prior reduction's packet payload.
type ReductionRunner struct {
	Steps    port.AnalysisStepStore
	Payloads port.AnalysisStepPayloadStore
	Evidence port.AnalysisEvidenceStore
	Analyzer port.ResultAnalyzer
}

// RunReduction executes one already-claimed reduction step to exactly one
// durable outcome, mirroring LeafRunner.RunLeaf's outcome contract:
//
//   - port.ErrModelCallLimitReached releases the step back to prepared
//     without consuming an attempt.
//   - context.Canceled or context.DeadlineExceeded propagates unchanged,
//     with no store transition.
//   - A model failure, or a combined packet that still fails validation
//     after the host assembles its source digest, coverage, and lineage,
//     fails the step typed as analysis_reduction_irreducible: findings that
//     cannot fit a bounded reduction without dropping objective-required
//     evidence must never produce a packet that claims a complete decision.
//   - When isRoot is true and sourceCoverage is not complete, the step
//     fails typed as analysis_coverage_incomplete instead: an incomplete
//     manifest can never claim to be the terminal packet, regardless of
//     whether the model's own output would otherwise validate.
//   - A successful reduction persists the assembled domain.AnalysisPacket
//     as the step's payload, then completes the step with its digest.
//
// sourceCoverage is only meaningful (and only checked) for the root step:
// an intermediate reduction node covers a strict subset of the source by
// construction, so domain.AnalysisPacket.Coverage is left at its zero value
// for every non-root node. domain.AnalysisPacket.Validate does not inspect
// Coverage, so this never blocks an intermediate node from completing.
func (r *ReductionRunner) RunReduction(ctx context.Context, claim domain.AnalysisStepClaim, children []port.AnalysisStep, isRoot bool, identity domain.AnalysisIdentity, objectiveText string, constraints []string, promptVersion string, sourceCoverage domain.AnalysisCoverage, now time.Time) (domain.AnalysisPacket, error) {
	childSummaries, err := r.loadChildSummaries(ctx, claim.AnalysisID, children)
	if err != nil {
		return domain.AnalysisPacket{}, err
	}

	input := port.AnalysisReductionInput{
		ObjectiveClass: identity.ObjectiveClass,
		ObjectiveText:  objectiveText,
		Constraints:    constraints,
		Children:       childSummaries,
		PromptVersion:  promptVersion,
	}
	packet, err := r.Analyzer.Reduce(ctx, input)
	if err != nil {
		if errors.Is(err, port.ErrModelCallLimitReached) {
			return domain.AnalysisPacket{}, r.Steps.Retry(ctx, claim, now.Add(RunLeafRetryBackoff), false)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return domain.AnalysisPacket{}, err
		}
		return domain.AnalysisPacket{}, r.Steps.Fail(ctx, claim, domain.AnalysisFailureReductionIrreducible, now)
	}

	packet.SourceSHA256 = identity.SourceSHA256
	if isRoot {
		packet.Coverage = sourceCoverage
	}

	if err := packet.Validate(); err != nil {
		return domain.AnalysisPacket{}, r.Steps.Fail(ctx, claim, domain.AnalysisFailureReductionIrreducible, now)
	}
	if isRoot && !sourceCoverage.Complete {
		return domain.AnalysisPacket{}, r.Steps.Fail(ctx, claim, domain.AnalysisFailureCoverageIncomplete, now)
	}

	payload, err := EncodePacketPayload(packet)
	if err != nil {
		return domain.AnalysisPacket{}, fmt.Errorf("%w: encode reduction payload: %v", domain.ErrAnalysisValidation, err)
	}
	if len(payload) > 65536 {
		return domain.AnalysisPacket{}, r.Steps.Fail(ctx, claim, domain.AnalysisFailureReductionIrreducible, now)
	}
	if err := r.Payloads.WritePayload(ctx, claim, payload, now); err != nil {
		return domain.AnalysisPacket{}, err
	}
	digest := hexSHA256(payload)
	if _, err := r.Steps.Complete(ctx, claim, digest, now); err != nil {
		return domain.AnalysisPacket{}, err
	}
	return packet, nil
}

// loadChildSummaries rebuilds one port.AnalysisChildSummary per declared
// child from durable storage: a leaf child's own payload plus its resolved
// evidence ids, or a prior reduction's packet payload directly. children
// must already be in the deterministic order PrepareSteps assigned; that
// order is preserved into the summaries and, through them, into the
// model's prompt and the packet's own Lineage.
func (r *ReductionRunner) loadChildSummaries(ctx context.Context, analysisID string, children []port.AnalysisStep) ([]port.AnalysisChildSummary, error) {
	summaries := make([]port.AnalysisChildSummary, 0, len(children))
	for _, child := range children {
		payload, err := r.Payloads.ReadPayload(ctx, analysisID, child.StepID)
		if err != nil {
			return nil, err
		}
		switch child.Kind {
		case domain.AnalysisStepLeaf:
			leaf, err := DecodeLeafPayload(payload)
			if err != nil {
				return nil, err
			}
			refs, err := r.Evidence.ListByLeafStep(ctx, analysisID, child.StepID)
			if err != nil {
				return nil, err
			}
			evidenceIDs := make([]string, 0, len(refs))
			for _, ref := range refs {
				evidenceIDs = append(evidenceIDs, ref.EvidenceID)
			}
			summaries = append(summaries, port.AnalysisChildSummary{
				StepID: child.StepID, Findings: leaf.Findings, Constraints: leaf.Constraints,
				Contradictions: leaf.Contradictions, UnresolvedQuestions: leaf.UnresolvedQuestions,
				EvidenceRefs: evidenceIDs,
			})
		case domain.AnalysisStepReduction:
			packet, err := DecodePacketPayload(payload)
			if err != nil {
				return nil, err
			}
			summaries = append(summaries, port.AnalysisChildSummary{
				StepID: child.StepID, Findings: packet.Findings, Constraints: packet.Constraints,
				Contradictions: packet.Contradictions, UnresolvedQuestions: packet.UnresolvedQuestions,
				EvidenceRefs: packet.EvidenceRefs,
			})
		default:
			return nil, fmt.Errorf("%w: reduction child %s has an unknown step kind", domain.ErrAnalysisValidation, child.StepID)
		}
	}
	return summaries, nil
}
