package agentdef_test

import (
	"testing"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
)

func TestNormalizeLegacyPreservesModelSettingsWithoutMutatingInput(t *testing.T) {
	extraBody := map[string]any{"thinking": map[string]any{"type": "disabled"}}
	headers := map[string]string{"X-Tenant": "tenant-a"}
	defs := agentdef.NormalizeLegacy("Legacy Agent", "model-name", "https://model.example/v1", "MODEL_KEY", "high", headers, extraBody)

	provider := defs.Providers["legacy"]
	profile := provider.Profiles["default"]
	root := defs.Agents["root_agent"]
	if provider.Type != agentdef.ProviderTypeOpenAICompatible || provider.BaseURL != "https://model.example/v1" || provider.APIKeyEnv != "MODEL_KEY" || provider.Headers["X-Tenant"] != "tenant-a" {
		t.Fatalf("provider = %#v", provider)
	}
	if profile.Model != "model-name" || profile.ReasoningEffort != "high" || profile.ExtraBody["thinking"] == nil {
		t.Fatalf("profile = %#v", profile)
	}
	if root.Name != "Legacy Agent" || root.Model != "legacy/default" || root.AgentClass != "LlmAgent" {
		t.Fatalf("root = %#v", root)
	}
	profile.ExtraBody["new"] = true
	if _, exists := extraBody["new"]; exists {
		t.Fatal("normalization mutated the legacy extra body")
	}
	provider.Headers["X-New"] = "value"
	if _, exists := headers["X-New"]; exists {
		t.Fatal("normalization mutated the legacy headers")
	}
}
