package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	"github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	resultanalysisusecase "github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

// resultAnalysisPromptVersion is the pinned prompt/policy suite version
// TRD 07's Analysis Identity section requires. It is derived from the pair
// of leaf and reduction prompts (openaillm.LeafPromptVersion,
// openaillm.ReductionPromptVersion) as one semantic unit (FIND-108): a
// change to either prompt constant changes this tag automatically, so a
// prompt change always starts a new analysis identity. Nothing pins this
// value to a fixed string; the identity test in result_analysis_test.go
// pins a golden derived value instead, so a prompt change must update that
// golden value on purpose.
var resultAnalysisPromptVersion = deriveResultAnalysisPromptVersion(openaillm.LeafPromptVersion, openaillm.ReductionPromptVersion)

// deriveResultAnalysisPromptVersion combines the leaf and reduction prompt
// versions into one suite tag, deterministically and one way: same inputs
// always produce the same tag, and a change to either input always changes
// the tag.
func deriveResultAnalysisPromptVersion(leafPromptVersion, reductionPromptVersion string) string {
	sum := sha256.Sum256([]byte("analysis-suite\x00" + leafPromptVersion + "\x00" + reductionPromptVersion))
	return "analysis-" + hex.EncodeToString(sum[:])[:16]
}

// resultAnalysisComposition carries every piece composeResultAnalysis
// builds: the tool-facing service, the durable worker, and the gate
// interface injected into the workstream service's dependent-dispatch
// check. gate and service are backed by the same *sqlite.AnalysisStore;
// they are separate fields only because they are separate roles at their
// call sites.
type resultAnalysisComposition struct {
	service *resultanalysisusecase.Service
	worker  *resultanalysisusecase.Worker
	gate    port.AnalysesByWorkstream
}

// composeResultAnalysis is the pure composition gate for TRD 07 (checkpoint
// 6): with orchestration.result_analysis.enabled false, it returns (nil,
// nil) and touches no v40 adapter constructor, so no v40 table is ever
// opened or written and the worker is never created. This mirrors
// composeLexicalRetrieval's gate exactly: the gate is positive, not a
// worker that exists and does nothing.
func composeResultAnalysis(cfg config.Config, models runtimeModels, modelCalls port.ModelCallLimiter, store *sqlite.Store, supplied ...*workpoll.Scheduler) (*resultAnalysisComposition, error) {
	analysis := cfg.Orchestration.ResultAnalysis
	if !analysis.Enabled {
		return nil, nil
	}
	if modelCalls == nil {
		return nil, errors.New("orchestration.result_analysis.enabled requires the shared model-call limiter")
	}

	trustedResults, err := sqlite.NewResultStore(store, models.resultPayloadStore)
	if err != nil {
		return nil, fmt.Errorf("initialize result analysis source reader: %w", err)
	}
	analysesStore := sqlite.NewAnalysisStore(store)
	stepsStore := sqlite.NewAnalysisStepStore(store)
	segmentsStore := sqlite.NewAnalysisSegmentStore(store)
	evidenceStore := sqlite.NewAnalysisEvidenceStore(store)
	completionStore := sqlite.NewAnalysisCompletionStore(store)
	if analysesStore == nil || stepsStore == nil || segmentsStore == nil || evidenceStore == nil || completionStore == nil {
		return nil, errors.New("result analysis adapters are not configured")
	}

	// The analysis model profile is optional (TRD 07's Concurrency and
	// Model Profile section): empty means the main model profile, and the
	// fingerprint recorded on the analysis row is derived from whichever
	// profile is effective, so the fallback is explicit in the identity.
	llm := models.rootModel
	providerID, modelName := models.rootProviderID, models.rootModelName
	if analysis.Model.Enabled {
		if models.resultAnalysisAPIKey == "" {
			return nil, errors.New("orchestration.result_analysis.model.enabled requires a resolved API key")
		}
		reasoningEffort := analysis.Model.ReasoningEffort
		if reasoningEffort == "" {
			reasoningEffort = models.rootReasoningEffort
		}
		options := []openaillm.Option{
			openaillm.WithAPIKey(models.resultAnalysisAPIKey),
			openaillm.WithBaseURL(analysis.Model.BaseURL),
			openaillm.WithModel(analysis.Model.Model),
		}
		if reasoningEffort != "" {
			options = append(options, openaillm.WithReasoningEffort(reasoningEffort))
		}
		built, buildErr := openaillm.New(options...)
		if buildErr != nil {
			return nil, fmt.Errorf("initialize result analysis model: %w", buildErr)
		}
		llm = built
		providerID, modelName = analysis.Model.ProviderID, analysis.Model.Model
	}
	if llm == nil {
		return nil, errors.New("result analysis requires a configured root model")
	}
	fingerprint := domain.AnalysisModelFingerprint(providerID, modelName)

	analyzer, err := openaillm.NewResultAnalyzer(llm, openaillm.ResultAnalyzerConfig{
		ModelCalls: modelCalls,
		Redact:     models.redactor.String,
		Timeout:    time.Duration(analysis.CallTimeoutSeconds) * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize result analyzer: %w", err)
	}

	limits := domain.AnalysisLimits{
		MaxSegmentBytes: int64(analysis.MaxSegmentBytes), OverlapBasisPoints: analysis.OverlapBasisPoints, OverlapMaxBytes: int64(analysis.OverlapMaxBytes),
		MaxLeaves: analysis.MaxLeaves, MaxReductionFanIn: analysis.MaxReductionFanIn, MaxReductionDepth: analysis.MaxReductionDepth,
		MaxConcurrentLeaves: analysis.MaxConcurrentLeaves, MaxAttemptsPerStep: analysis.MaxAttemptsPerStep,
		CallTimeoutSeconds: analysis.CallTimeoutSeconds, WallTimeSeconds: analysis.WallTimeSeconds,
		EvidenceExcerptBytes: analysis.Evidence.ExcerptBytes, EvidenceSelectorsPerLeaf: analysis.Evidence.SelectorsPerLeaf,
		EvidenceReferencesPerPacket: analysis.Evidence.ReferencesPerPacket, BundleBytes: analysis.Evidence.BundleBytes,
	}
	if err := limits.Validate(); err != nil {
		return nil, fmt.Errorf("result analysis limits are invalid: %w", err)
	}
	var scheduler *workpoll.Scheduler
	if len(supplied) > 0 {
		scheduler = supplied[0]
	} else {
		var err error
		scheduler, err = workpoll.New(time.Duration(analysis.WorkerIntervalSeconds)*time.Second, workpoll.Options{})
		if err != nil {
			return nil, fmt.Errorf("initialize result analysis scheduler: %w", err)
		}
	}

	service, err := resultanalysisusecase.NewService(resultanalysisusecase.ServiceConfig{
		SegmentationVersion: resultanalysisusecase.SegmenterTextV1,
		PromptVersion:       resultAnalysisPromptVersion,
		ModelFingerprint:    fingerprint,
		Limits:              limits,
	}, resultanalysisusecase.ServiceDependencies{
		Source: trustedResults, Analyses: analysesStore, Steps: stepsStore, Evidence: evidenceStore, Payloads: stepsStore, Analyzer: analyzer,
		Wake: scheduler.Wake,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize result analysis service: %w", err)
	}

	// The claim lease outlives one call_timeout so a legitimately slow
	// in-flight call is never reclaimed by another tick mid-flight; the
	// fixed 30s margin covers scheduling and store round-trip latency, not
	// a second full model call.
	worker, err := resultanalysisusecase.NewWorker(resultanalysisusecase.WorkerConfig{
		Interval: time.Duration(analysis.WorkerIntervalSeconds) * time.Second,
		Lease:    time.Duration(analysis.CallTimeoutSeconds)*time.Second + 30*time.Second,
	}, resultanalysisusecase.WorkerDependencies{
		Analyses: analysesStore, Active: analysesStore, Running: analysesStore, Steps: stepsStore, Segments: segmentsStore,
		Evidence: evidenceStore, Payloads: stepsStore, Completion: completionStore, Source: trustedResults, Analyzer: analyzer,
		Clock: port.SystemClock{}, Logger: models.logger, Sanitize: models.redactor.String, Scheduler: scheduler,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize result analysis worker: %w", err)
	}

	return &resultAnalysisComposition{service: service, worker: worker, gate: analysesStore}, nil
}
