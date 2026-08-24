package app

import (
	"context"
	"io"
	"iter"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/Dauno/slack-local-agent/internal/adapter/fsartifact"
	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

// TestResultAnalysisPromptVersionTracksLeafAndReductionConstants is
// FIND-108: resultAnalysisPromptVersion must be derived from
// openaillm.LeafPromptVersion and openaillm.ReductionPromptVersion, so a
// change to either constant changes the derived suite tag. This test pins
// the golden derived value for the current pair of constants; changing
// either constant, without also updating this golden value on purpose,
// turns this test red. That is the point: a prompt change must always be a
// deliberate new analysis identity, never a silent one.
func TestResultAnalysisPromptVersionTracksLeafAndReductionConstants(t *testing.T) {
	const goldenSuiteTag = "analysis-ab039b147a8ccc70"
	if resultAnalysisPromptVersion != goldenSuiteTag {
		t.Fatalf("resultAnalysisPromptVersion = %q, want the golden tag %q derived from openaillm.LeafPromptVersion=%q and openaillm.ReductionPromptVersion=%q; if this drifted on purpose, update goldenSuiteTag",
			resultAnalysisPromptVersion, goldenSuiteTag, openaillm.LeafPromptVersion, openaillm.ReductionPromptVersion)
	}
	if deriveResultAnalysisPromptVersion("leaf-a", "reduction-a") == deriveResultAnalysisPromptVersion("leaf-b", "reduction-a") {
		t.Fatal("changing the leaf prompt version did not change the derived suite tag")
	}
	if deriveResultAnalysisPromptVersion("leaf-a", "reduction-a") == deriveResultAnalysisPromptVersion("leaf-a", "reduction-b") {
		t.Fatal("changing the reduction prompt version did not change the derived suite tag")
	}
}

// fakeRootLLM is the minimal model.LLM stub used to exercise composition
// wiring only: composeResultAnalysis never calls GenerateContent in these
// tests, since the worker is never run against real claimed steps here.
type fakeRootLLM struct{}

func (fakeRootLLM) Name() string { return "fake-root" }
func (fakeRootLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {}
}

func resultAnalysisTestModels(t *testing.T) runtimeModels {
	t.Helper()
	models, _ := resultAnalysisTestModelsWithPayloads(t)
	return models
}

// resultAnalysisTestModelsWithPayloads also returns the payload backend, so
// a test that needs to materialize a real source through the identical
// backend composeResultAnalysis wires up can do so.
func resultAnalysisTestModelsWithPayloads(t *testing.T) (runtimeModels, *fsartifact.TypedStore) {
	t.Helper()
	payloads, err := fsartifact.NewTypedStore(filepath.Join(t.TempDir(), "v2-results"), 1024*1024)
	if err != nil {
		t.Fatalf("new payload store: %v", err)
	}
	return runtimeModels{
		rootModel:          fakeRootLLM{},
		rootFamily:         domain.ProviderFamilyOpenAICompatible,
		rootProviderID:     "main",
		rootModelName:      "main-model-v1",
		resultPayloadStore: payloads,
		redactor:           secure.NewRedactor("sk-analysis-secret-value"),
		logger:             logging.New(io.Discard, "error", secure.NewRedactor("sk-analysis-secret-value")),
	}, payloads
}

func resultAnalysisTestStore(t *testing.T) *adaptersqlite.Store {
	t.Helper()
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "result-analysis-composition.db"))
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestComposeResultAnalysisDisabledCreatesNothing is criterion 2: with the
// gate off, composition must return nothing, never touch a v40 adapter
// constructor, and never require a model-call limiter.
func TestComposeResultAnalysisDisabledCreatesNothing(t *testing.T) {
	cfg := config.Default()
	store := resultAnalysisTestStore(t)
	composition, err := composeResultAnalysis(cfg, resultAnalysisTestModels(t), nil, store)
	if err != nil || composition != nil {
		t.Fatalf("composeResultAnalysis(disabled) = %v, %v, want nil, nil", composition, err)
	}
}

// TestComposeResultAnalysisEnabledConstructsEverything is criterion 2's
// positive counterpart and criterion 7: with the gate on, the worker, the
// tool-facing service, and the workstream dispatch gate are all
// constructed from the same durable analysis store.
func TestComposeResultAnalysisEnabledConstructsEverything(t *testing.T) {
	cfg := config.Default()
	cfg.Orchestration.ResultAnalysis.Enabled = true
	store := resultAnalysisTestStore(t)
	composition, err := composeResultAnalysis(cfg, resultAnalysisTestModels(t), modelcalllimiter.New(1), store)
	if err != nil {
		t.Fatalf("composeResultAnalysis(enabled) error = %v", err)
	}
	if composition == nil || composition.worker == nil || composition.service == nil || composition.gate == nil {
		t.Fatalf("composeResultAnalysis(enabled) = %+v, want a non-nil worker, service, and gate", composition)
	}
}

// TestComposeResultAnalysisEnabledRequiresModelCallLimiter proves the gate
// fails closed rather than silently composing an analyzer with no
// backpressure.
func TestComposeResultAnalysisEnabledRequiresModelCallLimiter(t *testing.T) {
	cfg := config.Default()
	cfg.Orchestration.ResultAnalysis.Enabled = true
	store := resultAnalysisTestStore(t)
	if _, err := composeResultAnalysis(cfg, resultAnalysisTestModels(t), nil, store); err == nil {
		t.Fatal("composeResultAnalysis(enabled, no limiter) succeeded, want an error")
	}
}

// materializeResultAnalysisSource writes one real, verifiable source result
// through the exact adapter composeResultAnalysis itself resolves sources
// through, over the same store and payload backend, so
// Service.RequestAnalysis's call to TrustedResultStore.Resolve actually
// succeeds instead of quarantining an unverifiable fixture row.
func materializeResultAnalysisSource(t *testing.T, store *adaptersqlite.Store, payloads *fsartifact.TypedStore, scope domain.ResultScope) string {
	t.Helper()
	results, err := adaptersqlite.NewResultStore(store, payloads)
	if err != nil {
		t.Fatalf("new result store: %v", err)
	}
	handle, err := results.Materialize(t.Context(), port.ResultMaterialization{
		Producer:  domain.ResultProducer{Kind: domain.ResultProducerACPJob, ID: "op-1", Revision: 0},
		Payload:   "source content for fingerprint test",
		Scope:     scope,
		Retention: domain.ResultRetentionWorkstream,
		MediaType: "text/plain; charset=utf-8",
	})
	if err != nil {
		t.Fatalf("materialize source: %v", err)
	}
	return handle.ResultID
}

func resultAnalysisFingerprintFromDB(t *testing.T, store *adaptersqlite.Store, analysisID string) string {
	t.Helper()
	var fingerprint string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT model_fingerprint FROM result_analyses WHERE analysis_id = ?`, analysisID).Scan(&fingerprint); err != nil {
		t.Fatalf("read model_fingerprint: %v", err)
	}
	return fingerprint
}

// TestResultAnalysisModelFingerprintFallsBackToMainProfile is criterion 6:
// an empty result_analysis.model block uses the main model profile, and the
// fingerprint actually persisted on the analysis row (not just an
// independently recomputed value) reflects that fallback explicitly. A
// dedicated analysis model profile persists a different fingerprint for the
// identical objective and source, so the two cases are never confused.
func TestResultAnalysisModelFingerprintFallsBackToMainProfile(t *testing.T) {
	scope := domain.ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:U1", Project: "app"}
	now := time.Now().UTC()

	// Case A: no dedicated model profile, falls back to the main profile.
	cfgA := config.Default()
	cfgA.Orchestration.ResultAnalysis.Enabled = true
	storeA := resultAnalysisTestStore(t)
	modelsA, payloadsA := resultAnalysisTestModelsWithPayloads(t)
	resultIDA := materializeResultAnalysisSource(t, storeA, payloadsA, scope)
	compositionA, err := composeResultAnalysis(cfgA, modelsA, modelcalllimiter.New(1), storeA)
	if err != nil || compositionA == nil {
		t.Fatalf("composeResultAnalysis(fallback) = %v, %v", compositionA, err)
	}
	resultA, err := compositionA.service.RequestAnalysis(t.Context(), resultIDA, scope, "ws-1", "what changed?", now)
	if err != nil {
		t.Fatalf("RequestAnalysis(fallback) error = %v", err)
	}
	fingerprintA := resultAnalysisFingerprintFromDB(t, storeA, resultA.AnalysisID)
	wantFallback := domain.AnalysisModelFingerprint("main", "main-model-v1")
	if fingerprintA != wantFallback {
		t.Fatalf("fallback fingerprint = %q, want %q (derived from the main profile)", fingerprintA, wantFallback)
	}

	// Case B: a dedicated analysis model profile is enabled.
	cfgB := config.Default()
	cfgB.Orchestration.ResultAnalysis.Enabled = true
	cfgB.Orchestration.ResultAnalysis.Model = config.ResultAnalysisModelConfig{
		Enabled: true, ProviderID: "acme", BaseURL: "http://127.0.0.1:9", APIKeyEnv: "RESULT_ANALYSIS_API_KEY", Model: "analysis-model-v1",
	}
	storeB := resultAnalysisTestStore(t)
	modelsB, payloadsB := resultAnalysisTestModelsWithPayloads(t)
	resultIDB := materializeResultAnalysisSource(t, storeB, payloadsB, scope)
	modelsB.resultAnalysisAPIKey = "sk-dedicated-analysis-key"
	compositionB, err := composeResultAnalysis(cfgB, modelsB, modelcalllimiter.New(1), storeB)
	if err != nil || compositionB == nil {
		t.Fatalf("composeResultAnalysis(dedicated) = %v, %v", compositionB, err)
	}
	resultB, err := compositionB.service.RequestAnalysis(t.Context(), resultIDB, scope, "ws-1", "what changed?", now)
	if err != nil {
		t.Fatalf("RequestAnalysis(dedicated) error = %v", err)
	}
	fingerprintB := resultAnalysisFingerprintFromDB(t, storeB, resultB.AnalysisID)
	wantDedicated := domain.AnalysisModelFingerprint("acme", "analysis-model-v1")
	if fingerprintB != wantDedicated {
		t.Fatalf("dedicated fingerprint = %q, want %q (derived from the dedicated profile)", fingerprintB, wantDedicated)
	}

	if fingerprintA == fingerprintB {
		t.Fatal("fallback and dedicated fingerprints must not collide")
	}
}
