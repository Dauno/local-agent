package app

import (
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	workstreamusecase "github.com/Dauno/slack-local-agent/internal/usecase/workstream"
)

// TestComposedWorkstreamServiceBlocksOnIncompleteBoundAnalysis is FIND-107:
// it builds the application's real composeResultAnalysis and composeWorkstream
// wiring, exactly as composeRuntime does, then observes the TRD 07
// dependent-dispatch gate through the composed workstream service's public
// API only, never by reaching into an internal struct field. Deleting
// `AnalysisGate: analysisGate` from composeWorkstream's call to
// workstreamusecase.New (equivalently, from composition.go before this
// extraction) must turn this test red.
func TestComposedWorkstreamServiceBlocksOnIncompleteBoundAnalysis(t *testing.T) {
	cfg := config.Default()
	cfg.Orchestration.ResultAnalysis.Enabled = true
	cfg.Orchestration.Workstreams.Enabled = true

	store := resultAnalysisTestStore(t)
	models, payloads := resultAnalysisTestModelsWithPayloads(t)

	resultAnalysisComp, err := composeResultAnalysis(cfg, models, modelcalllimiter.New(1), store)
	if err != nil || resultAnalysisComp == nil {
		t.Fatalf("composeResultAnalysis() = %v, %v, want a non-nil composition", resultAnalysisComp, err)
	}

	paths := config.Paths{SandboxProjectRoots: map[string]string{"workspace": t.TempDir()}}
	workstreamService, err := composeWorkstream(cfg, paths, store, models.resultPayloadStore, resultAnalysisComp.gate)
	if err != nil || workstreamService == nil {
		t.Fatalf("composeWorkstream() = %v, %v, want a non-nil service", workstreamService, err)
	}

	scope := domain.ResultScope{Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678", Project: "workspace"}
	binding := workstreamusecase.Binding{Actor: scope.Actor, ConversationKey: domain.ConversationKey(scope.ConversationKey), Project: scope.Project}

	if _, err := workstreamService.Create(t.Context(), binding, "ws-1", "investigate the incident"); err != nil {
		t.Fatalf("create workstream: %v", err)
	}

	sourceResultID := materializeResultAnalysisSource(t, store, payloads, scope)
	analysisResult, err := resultAnalysisComp.service.RequestAnalysis(t.Context(), sourceResultID, scope, "ws-1", "what changed?", time.Now().UTC())
	if err != nil {
		t.Fatalf("request analysis: %v", err)
	}
	// The freshly requested analysis is 'preparing': non-terminal, so the
	// gate must block. Nothing in this test drives the durable worker, so it
	// never reaches a terminal state on its own.
	if analysisResult.State != domain.AnalysisPreparing {
		t.Fatalf("analysis state = %q, want %q so the gate has something non-terminal to block on", analysisResult.State, domain.AnalysisPreparing)
	}

	proposal := domain.WorkstreamTransition{WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "root-activate", Action: domain.WorkstreamActionActivateWorkstream}
	confirmation := domain.WorkstreamConfirmation{ID: "host-confirmation", Approved: true, ExpiresAt: time.Now().Add(time.Hour)}
	if _, _, err := workstreamService.ApplyConfirmed(t.Context(), binding, domain.WorkstreamSourceRoot, proposal, confirmation); err == nil {
		t.Fatal("ApplyConfirmed with an incomplete bound analysis succeeded, want the dependent-dispatch gate to block it")
	} else if want := domain.ErrWorkstreamAnalysisBlocking; !errors.Is(err, want) {
		t.Fatalf("ApplyConfirmed error = %v, want it to wrap %v", err, want)
	}
}
