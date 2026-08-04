package app

import (
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
)

func TestComposeRootTokenCounter(t *testing.T) {
	resolved := &agentdef.ResolvedModel{
		Provider:            agentdef.Provider{Type: agentdef.ProviderTypeOpenAICompatible},
		Model:               "model",
		ContextWindowTokens: 128_000,
		CounterStrategy:     "byte_bound",
	}
	counter, err := composeRootTokenCounter(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if counter == nil {
		t.Fatal("counter was not composed")
	}
}

func TestComposeRootTokenCounterRejectsIncompleteCapability(t *testing.T) {
	resolved := &agentdef.ResolvedModel{
		Provider: agentdef.Provider{Type: agentdef.ProviderTypeOpenAICompatible},
		Model:    "model",
	}
	_, err := composeRootTokenCounter(resolved)
	if err == nil || !strings.Contains(err.Error(), "context_window_tokens") {
		t.Fatalf("expected actionable capability error, got %v", err)
	}
}

func TestComposeRootTokenCounterRejectsUnavailableStrategy(t *testing.T) {
	resolved := &agentdef.ResolvedModel{
		Provider:            agentdef.Provider{Type: agentdef.ProviderTypeOpenAICompatible},
		Model:               "model",
		ContextWindowTokens: 128_000,
		CounterStrategy:     "official",
		CounterID:           "not-installed",
	}
	_, err := composeRootTokenCounter(resolved)
	if err == nil || !strings.Contains(err.Error(), "unsupported token counter strategy") {
		t.Fatalf("expected unavailable strategy error, got %v", err)
	}
}

// TestComposeRootTokenCounterAvailabilityMatrix pins the coherence between
// what profiles may declare and what this release implements: byte_bound needs
// no ID, estimator requires the exact versioned ID, and anything else fails.
func TestComposeRootTokenCounterAvailabilityMatrix(t *testing.T) {
	base := agentdef.ResolvedModel{
		Provider:            agentdef.Provider{Type: agentdef.ProviderTypeOpenAICompatible},
		Model:               "model",
		ContextWindowTokens: 128_000,
	}
	tests := []struct {
		name      string
		strategy  string
		id        string
		wantError bool
	}{
		{name: "byte_bound", strategy: "byte_bound"},
		{name: "byte_bound with id", strategy: "byte_bound", id: "unexpected", wantError: true},
		{name: "estimator with known id", strategy: "estimator", id: agentdef.VisualEstimatorID},
		{name: "estimator with unknown id", strategy: "estimator", id: "not-installed", wantError: true},
		{name: "estimator without id", strategy: "estimator", wantError: true},
		{name: "official", strategy: "official", id: "some-id", wantError: true},
		{name: "endpoint", strategy: "endpoint", id: "some-id", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved := base
			resolved.CounterStrategy = tt.strategy
			resolved.CounterID = tt.id
			counter, err := composeRootTokenCounter(&resolved)
			if tt.wantError {
				if err == nil {
					t.Fatal("expected composition error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("composeRootTokenCounter() error = %v", err)
			}
			if counter == nil {
				t.Fatal("counter was not composed")
			}
		})
	}
}
