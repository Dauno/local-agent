package resultanalysis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// ServiceConfig carries the semantic components of every analysis this
// service requests, other than the source and the objective, which the
// caller supplies per call. These are the same components
// domain.AnalysisIdentity requires; they come from configuration, not from
// the model.
type ServiceConfig struct {
	SegmentationVersion string
	PromptVersion       string
	ModelFingerprint    string
	Limits              domain.AnalysisLimits
}

// ServiceDependencies are the durable stores and the pure analyzer this
// service is built from. Analyzer is carried here only so a future worker
// (checkpoint 6) has one place to obtain it; RequestAnalysis, Status, and
// ReadPacket never call it. That is a structural guarantee, not a runtime
// check: none of those three methods takes a code path that reaches
// Analyzer at all.
type ServiceDependencies struct {
	Source   port.TrustedResultStore
	Analyses port.AnalysisStore
	Steps    port.AnalysisStepStore
	Evidence port.AnalysisEvidenceStore
	Payloads port.AnalysisStepPayloadStore
	Analyzer port.ResultAnalyzer
	Wake     func()
}

// Service is the host-validated surface TRD 07's Tool Surface section
// describes: it resolves a source, creates or returns an analysis identity,
// reports bounded status, and reads a completed terminal packet. It never
// makes a model call and never returns source content, storage keys,
// prompts, or credentials.
type Service struct {
	cfg  ServiceConfig
	deps ServiceDependencies
}

// NewService validates its dependencies and its limits once at construction,
// so a misconfigured service fails at startup rather than on first use.
func NewService(cfg ServiceConfig, deps ServiceDependencies) (*Service, error) {
	if deps.Source == nil || deps.Analyses == nil || deps.Steps == nil || deps.Evidence == nil || deps.Payloads == nil {
		return nil, errors.New("result analysis service requires every durable dependency")
	}
	if err := cfg.Limits.Validate(); err != nil {
		return nil, err
	}
	return &Service{cfg: cfg, deps: deps}, nil
}

// RequestAnalysisResult is the bounded status returned to
// workstream_request_result_analysis. It carries no source content.
type RequestAnalysisResult struct {
	AnalysisID string
	State      domain.AnalysisState
}

// RequestAnalysis validates the source result through
// port.TrustedResultStore.Resolve (the existing verifier) and creates or
// returns the analysis identity for the given objective. It never calls
// port.ResultAnalyzer: an analysis row only ever reaches
// domain.AnalysisPreparing on creation, and the actual leaf and reduction
// model calls happen later, driven by a durable worker over the step
// queue, never inline with this call.
func (s *Service) RequestAnalysis(ctx context.Context, resultID string, scope domain.ResultScope, workstreamID string, objectiveText string, now time.Time) (RequestAnalysisResult, error) {
	if s == nil {
		return RequestAnalysisResult{}, errors.New("result analysis service is not configured")
	}
	identity, _, err := s.deps.Source.Resolve(ctx, resultID, scope)
	if err != nil {
		return RequestAnalysisResult{}, err
	}
	normalizedObjective, err := domain.NormalizeAnalysisObjective(objectiveText)
	if err != nil {
		return RequestAnalysisResult{}, err
	}
	objectiveDigest := domain.AnalysisObjectiveDigest(domain.AnalysisObjectiveBoundedQuestionV1, normalizedObjective)
	analysisIdentity := domain.AnalysisIdentity{
		SourceResultID:      resultID,
		SourceSHA256:        identity.SHA256,
		ObjectiveClass:      domain.AnalysisObjectiveBoundedQuestionV1,
		ObjectiveDigest:     objectiveDigest,
		SegmentationVersion: s.cfg.SegmentationVersion,
		PromptVersion:       s.cfg.PromptVersion,
		ModelFingerprint:    s.cfg.ModelFingerprint,
		LimitsDigest:        s.cfg.Limits.Digest(),
	}
	if err := analysisIdentity.Validate(); err != nil {
		return RequestAnalysisResult{}, err
	}
	record, err := s.deps.Analyses.Create(ctx, analysisIdentity, s.cfg.Limits, normalizedObjective, scope, workstreamID, now)
	if err != nil {
		return RequestAnalysisResult{}, err
	}
	if s.deps.Wake != nil {
		s.deps.Wake()
	}
	return RequestAnalysisResult{AnalysisID: record.AnalysisID, State: record.State}, nil
}

// AnalysisStatusResult is the bounded status returned to
// workstream_analysis_status. It carries state, step counts, and the typed
// failure code, and no source content: LeafCompleted/LeafTotal and
// ReductionCompleted/ReductionTotal report step progress, not verified
// byte coverage, which is only meaningful once the terminal packet exists.
type AnalysisStatusResult struct {
	AnalysisID         string
	State              domain.AnalysisState
	FailureCode        domain.AnalysisFailureCode
	LeafTotal          int
	LeafCompleted      int
	ReductionTotal     int
	ReductionCompleted int
	ProgressText       string
}

// Status reads state, step counts, and the typed failure code for one
// analysis. It never reads source content or a step's payload.
func (s *Service) Status(ctx context.Context, analysisID string, scope domain.ResultScope) (AnalysisStatusResult, error) {
	if s == nil {
		return AnalysisStatusResult{}, errors.New("result analysis service is not configured")
	}
	record, err := s.deps.Analyses.Get(ctx, analysisID, scope)
	if err != nil {
		return AnalysisStatusResult{}, err
	}
	steps, err := s.listAllSteps(ctx, analysisID)
	if err != nil {
		return AnalysisStatusResult{}, err
	}
	result := AnalysisStatusResult{AnalysisID: record.AnalysisID, State: record.State, FailureCode: record.FailureCode}
	for _, step := range steps {
		switch step.Kind {
		case domain.AnalysisStepLeaf:
			result.LeafTotal++
			if step.State == domain.AnalysisStepCompleted {
				result.LeafCompleted++
			}
		case domain.AnalysisStepReduction:
			result.ReductionTotal++
			if step.State == domain.AnalysisStepCompleted {
				result.ReductionCompleted++
			}
		}
	}
	result.ProgressText = fmt.Sprintf("%d of %d leaf segments completed, %d of %d reduction steps completed. This reports step progress, not verified byte coverage or comprehension.",
		result.LeafCompleted, result.LeafTotal, result.ReductionCompleted, result.ReductionTotal)
	return result, nil
}

// AnalysisPacketResult is the bounded terminal packet and its verified
// evidence excerpts returned to workstream_read_analysis_packet.
type AnalysisPacketResult struct {
	AnalysisID   string
	Packet       domain.AnalysisPacket
	Evidence     []port.AnalysisEvidenceExcerpt
	CoverageText string
}

// ErrAnalysisNotCompleted is returned by ReadPacket when the analysis has
// not reached domain.AnalysisCompleted. It wraps domain.ErrAnalysisValidation
// so callers can still test with errors.Is against the shared sentinel.
var ErrAnalysisNotCompleted = fmt.Errorf("%w: analysis is not completed", domain.ErrAnalysisValidation)

// ReadPacket returns the terminal packet and its bounded evidence excerpts
// for a completed analysis only. It rejects an analysis in any other state.
func (s *Service) ReadPacket(ctx context.Context, analysisID string, scope domain.ResultScope) (AnalysisPacketResult, error) {
	if s == nil {
		return AnalysisPacketResult{}, errors.New("result analysis service is not configured")
	}
	record, err := s.deps.Analyses.Get(ctx, analysisID, scope)
	if err != nil {
		return AnalysisPacketResult{}, err
	}
	if record.State != domain.AnalysisCompleted {
		return AnalysisPacketResult{}, ErrAnalysisNotCompleted
	}
	steps, err := s.listAllSteps(ctx, analysisID)
	if err != nil {
		return AnalysisPacketResult{}, err
	}
	rootStepID := rootReductionStepID(steps)
	if rootStepID == "" {
		return AnalysisPacketResult{}, fmt.Errorf("%w: completed analysis has no root reduction step", domain.ErrAnalysisUnavailable)
	}
	payload, err := s.deps.Payloads.ReadPayload(ctx, analysisID, rootStepID)
	if err != nil {
		return AnalysisPacketResult{}, err
	}
	packet, err := DecodePacketPayload(payload)
	if err != nil {
		return AnalysisPacketResult{}, err
	}

	evidenceByID := make(map[string]port.AnalysisEvidenceExcerpt)
	for _, step := range steps {
		if step.Kind != domain.AnalysisStepLeaf {
			continue
		}
		excerpts, err := s.deps.Evidence.ListByLeafStep(ctx, analysisID, step.StepID)
		if err != nil {
			return AnalysisPacketResult{}, err
		}
		for _, excerpt := range excerpts {
			evidenceByID[excerpt.EvidenceID] = excerpt
		}
	}
	evidence := make([]port.AnalysisEvidenceExcerpt, 0, len(packet.EvidenceRefs))
	for _, ref := range packet.EvidenceRefs {
		if excerpt, ok := evidenceByID[ref]; ok {
			evidence = append(evidence, excerpt)
		}
	}

	return AnalysisPacketResult{AnalysisID: analysisID, Packet: packet, Evidence: evidence, CoverageText: packet.Coverage.StatusText()}, nil
}

func (s *Service) listAllSteps(ctx context.Context, analysisID string) ([]port.AnalysisStep, error) {
	var all []port.AnalysisStep
	after := ""
	for {
		page, err := s.deps.Steps.List(ctx, analysisID, after, 500)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 500 {
			break
		}
		after = page[len(page)-1].StepID
	}
	return all, nil
}

// rootReductionStepID finds the one reduction step that is not declared as
// a child of any other step. A completed analysis' reduction tree has
// exactly one such node: the root, whose output is the terminal packet.
func rootReductionStepID(steps []port.AnalysisStep) string {
	isChild := make(map[string]bool)
	for _, step := range steps {
		for _, child := range step.ChildStepIDs {
			isChild[child] = true
		}
	}
	for _, step := range steps {
		if step.Kind == domain.AnalysisStepReduction && !isChild[step.StepID] {
			return step.StepID
		}
	}
	return ""
}
