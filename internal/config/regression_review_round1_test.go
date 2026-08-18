package config_test

import (
	"testing"

	"github.com/Dauno/slack-local-agent/internal/config"
)

// The tests in this file are the reviewer's round-1 regression fixtures for
// FIND-097 and FIND-099 (docs/root-orchestrator-v2/hallazgos/tests-regresion-ronda1/regresion-config.go.txt),
// incorporated with definitive, asserting names. Do not weaken these
// assertions to make a future change pass; if a future change requires
// that, stop and say so instead.

// TestFIND097UnsatisfiableTreeRejectedAtConfigValidation is the FIND-097
// regression: fan-in 2, depth 2 (2^2=4) cannot cover 512 leaves.
// domain.AnalysisLimits.Validate already rejected this combination;
// config.Validate must reject it too, at startup, per TRD 07 (Concurrency
// and Model Profile): "an unsatisfiable combination fails at startup and
// not mid-analysis."
func TestFIND097UnsatisfiableTreeRejectedAtConfigValidation(t *testing.T) {
	cfg := config.Default()
	cfg.Orchestration.ResultAnalysis.Enabled = true
	cfg.Orchestration.ResultAnalysis.MaxLeaves = 512
	cfg.Orchestration.ResultAnalysis.MaxReductionFanIn = 2
	cfg.Orchestration.ResultAnalysis.MaxReductionDepth = 2 // 2^2 = 4 << 512

	err := config.Validate(cfg)
	if err == nil {
		t.Fatal("FIND-097: unsatisfiable config accepted at startup; the failure would be deferred to runtime")
	}
	t.Logf("config.Validate -> %v", err)
}

// TestFIND099AnalysisReasoningEffortRejectsUnknownValue is the FIND-099
// regression: the main model profile's reasoning_effort validates against a
// closed set; the analysis profile's field did not.
func TestFIND099AnalysisReasoningEffortRejectsUnknownValue(t *testing.T) {
	cfg := config.Default()
	cfg.Orchestration.ResultAnalysis.Model.ReasoningEffort = "banana"
	errAnalysis := config.Validate(cfg)

	cfg2 := config.Default()
	cfg2.Model.ReasoningEffort = "banana"
	errMain := config.Validate(cfg2)

	t.Logf("analysis.model.reasoning_effort='banana' -> %v", errAnalysis)
	t.Logf("model.reasoning_effort='banana'          -> %v", errMain)
	if errAnalysis == nil {
		t.Fatal("FIND-099: analysis profile did not reject an unknown reasoning_effort value")
	}
	if errMain == nil {
		t.Fatal("fixture assumption broken: the main model profile must still reject an unknown reasoning_effort value")
	}
}
