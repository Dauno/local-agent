package app

import (
	"context"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

// TestComposeResultAnalysisWakesItsOwnSchedulerOnly is FIND-122's repair for
// the result-analysis class: it drives the actual production composition
// function, composeResultAnalysis, instead of a hand-built stand-in that
// wires resultanalysis.Service and resultanalysis.Worker to a scheduler
// itself. A regression that gives the service and the worker two separate
// schedulers inside composeResultAnalysis (internal/app/result_analysis.go)
// makes this test hang until the go test binary's own -timeout, because the
// worker would then advance only through its own recovery timer, which this
// test never fires.
func TestComposeResultAnalysisWakesItsOwnSchedulerOnly(t *testing.T) {
	cfg := config.Default()
	cfg.Orchestration.ResultAnalysis.Enabled = true
	store := resultAnalysisTestStore(t)
	models, payloads := resultAnalysisTestModelsWithPayloads(t)
	scheduler, timers := newWakeCompositionScheduler(t)

	composition, err := composeResultAnalysis(cfg, models, modelcalllimiter.New(1), store, scheduler)
	if err != nil || composition == nil {
		t.Fatalf("composeResultAnalysis() = %v, %v", composition, err)
	}

	runCtx, runCancel := context.WithCancel(t.Context())
	t.Cleanup(runCancel)
	go composition.worker.Run(runCtx)
	waitCompositionPoll(t, timers) // initial poll: no analysis is active yet.

	scope := domain.ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:U1", Project: "workspace"}
	resultID := materializeResultAnalysisSource(t, store, payloads, scope)
	if _, err := composition.service.RequestAnalysis(t.Context(), resultID, scope, "ws-1", "What changed?", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// The tick triggered by composeResultAnalysis's own service.Wake wiring,
	// never a real recovery timer.
	waitCompositionPoll(t, timers)
}
