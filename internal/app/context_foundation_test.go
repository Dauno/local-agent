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
