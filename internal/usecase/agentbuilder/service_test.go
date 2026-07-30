package agentbuilder

import (
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestDefaultModelSortsProvidersAndProfiles(t *testing.T) {
	defs := &agentdef.Definitions{
		Providers: map[string]agentdef.Provider{
			"z-provider": {
				Type:     agentdef.ProviderTypeOpenAICompatible,
				Profiles: map[string]agentdef.Profile{"default": {}},
			},
			"a-provider": {
				Type: agentdef.ProviderTypeOpenAICompatible,
				Profiles: map[string]agentdef.Profile{
					"z-profile": {},
					"a-profile": {},
				},
			},
		},
	}

	if got := defaultModel(defs); got != "a-provider/a-profile" {
		t.Fatalf("defaultModel() = %q, want %q", got, "a-provider/a-profile")
	}
}

func TestPreviewBuildsLLMAgentFromProviderProfile(t *testing.T) {
	service := New()
	defs := &agentdef.Definitions{Providers: map[string]agentdef.Provider{
		"openai": {
			Name: "openai", Type: agentdef.ProviderTypeOpenAICompatible,
			BaseURL: "https://model.example/v1", APIKeyEnv: "MODEL_KEY",
			Profiles: map[string]agentdef.Profile{"fast": {Model: "model-fast"}},
		},
	}}
	result, err := service.Preview(domain.AgentDraft{
		Name: "release_notes", Description: "Writes notes", Instruction: "Summarize changes.",
		ProviderProfile: "openai/fast",
	}, defs)
	if err != nil {
		t.Fatal(err)
	}
	if result.AgentDef.AgentClass != "LlmAgent" || result.AgentDef.Model != "openai/fast" {
		t.Fatalf("preview identity = %#v", result.AgentDef)
	}
	if result.AgentDef.ExecutionMode != domain.ExecutionModeForeground || result.AgentDef.TimeoutSec != 0 {
		t.Fatalf("preview execution = %#v", result.AgentDef)
	}
	if !strings.Contains(result.YAML, "model: openai/fast") || !strings.Contains(result.YAML, "tool_scope: invocation_scoped") {
		t.Fatalf("LLM YAML missing expected fields: %s", result.YAML)
	}
	if strings.Contains(result.YAML, "runtime:") {
		t.Fatalf("LLM YAML unexpectedly contains runtime: %s", result.YAML)
	}
}

func TestPreviewBuildsACPAgentWithoutModel(t *testing.T) {
	service := New()
	defs := &agentdef.Definitions{Providers: map[string]agentdef.Provider{
		"opencode": {
			Name: "opencode", Type: agentdef.ProviderTypeACP, Command: "opencode",
			Profiles: map[string]agentdef.Profile{
				"default": {ConfigOptions: []agentdef.ACPConfigOption{{ID: "model", Value: "test"}}},
			},
		},
	}}
	result, err := service.Preview(domain.AgentDraft{
		Name: "acp_worker", Description: "Delegates work", Instruction: "Complete the task.",
		Kind: domain.AgentKindACP, ProviderProfile: "opencode/default", ExecutionMode: domain.ExecutionModeDurableJob,
	}, defs)
	if err != nil {
		t.Fatal(err)
	}
	if result.AgentDef.AgentClass != "AcpAgent" || result.AgentDef.Model != "" || result.AgentDef.ExecutionMode != domain.ExecutionModeDurableJob || result.AgentDef.TimeoutSec != domain.DefaultACPTimeoutSeconds {
		t.Fatalf("preview identity = %#v", result.AgentDef)
	}
	if !strings.Contains(result.YAML, "runtime: opencode/default") || !strings.Contains(result.YAML, "confirmation: required") {
		t.Fatalf("ACP YAML missing expected fields: %s", result.YAML)
	}
	for _, forbidden := range []string{"model:", "include_contents:", "tool_scope:"} {
		if strings.Contains(result.YAML, forbidden) {
			t.Fatalf("ACP YAML contains forbidden %q: %s", forbidden, result.YAML)
		}
	}
}

func TestPreviewRejectsInvalidV2Drafts(t *testing.T) {
	defs := &agentdef.Definitions{Providers: map[string]agentdef.Provider{
		"openai": {
			Type:     agentdef.ProviderTypeOpenAICompatible,
			Profiles: map[string]agentdef.Profile{"fast": {}},
		},
		"opencode": {
			Type:     agentdef.ProviderTypeACP,
			Profiles: map[string]agentdef.Profile{"default": {}},
		},
		"other-acp": {
			Type:     agentdef.ProviderTypeACP,
			Profiles: map[string]agentdef.Profile{"default": {}},
		},
	}}
	base := domain.AgentDraft{
		Name:            "builder_worker",
		Description:     "description",
		Instruction:     "instruction",
		Kind:            domain.AgentKindACP,
		ProviderProfile: "opencode/default",
		ExecutionMode:   domain.ExecutionModeForeground,
	}

	tests := []struct {
		name  string
		draft domain.AgentDraft
	}{
		{name: "invalid kind", draft: domain.AgentDraft{Name: "builder_worker", Kind: "other", ProviderProfile: "openai/fast"}},
		{name: "ACP provider outside allowlist", draft: func() domain.AgentDraft { d := base; d.ProviderProfile = "other-acp/default"; return d }()},
		{name: "ACP timeout too high", draft: func() domain.AgentDraft { d := base; d.TimeoutSeconds = 86401; return d }()},
		{name: "ACP timeout negative", draft: func() domain.AgentDraft { d := base; d.TimeoutSeconds = -1; return d }()},
		{name: "LLM timeout", draft: domain.AgentDraft{Name: "builder_worker", Kind: domain.AgentKindLLM, ProviderProfile: "openai/fast", ExecutionMode: domain.ExecutionModeForeground, TimeoutSeconds: 1}},
		{name: "LLM durable job", draft: domain.AgentDraft{Name: "builder_worker", Kind: domain.AgentKindLLM, ProviderProfile: "openai/fast", ExecutionMode: domain.ExecutionModeDurableJob}},
		{name: "provider profile and model conflict", draft: domain.AgentDraft{Name: "builder_worker", Kind: domain.AgentKindLLM, ProviderProfile: "openai/fast", Model: "openai/other", ExecutionMode: domain.ExecutionModeForeground}},
		{name: "ACP model", draft: func() domain.AgentDraft { d := base; d.Model = "openai/fast"; return d }()},
		{name: "ACP provider profile missing", draft: func() domain.AgentDraft { d := base; d.ProviderProfile = ""; return d }()},
		{name: "invalid provider profile format", draft: domain.AgentDraft{Name: "builder_worker", Kind: domain.AgentKindLLM, ProviderProfile: "openai", ExecutionMode: domain.ExecutionModeForeground}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New().Preview(tt.draft, defs); err == nil {
				t.Fatal("Preview() unexpectedly succeeded")
			}
		})
	}
}

func TestPreviewAcceptsACPMaximumTimeout(t *testing.T) {
	defs := &agentdef.Definitions{Providers: map[string]agentdef.Provider{
		"opencode": {
			Name: "opencode", Type: agentdef.ProviderTypeACP, Command: "opencode",
			Profiles: map[string]agentdef.Profile{"default": {ConfigOptions: []agentdef.ACPConfigOption{{ID: "model", Value: "test"}}}},
		},
	}}
	result, err := New().Preview(domain.AgentDraft{
		Name: "builder_worker", Description: "description", Instruction: "instruction",
		Kind: domain.AgentKindACP, ProviderProfile: "opencode/default",
		ExecutionMode: domain.ExecutionModeForeground, TimeoutSeconds: 86400,
	}, defs)
	if err != nil {
		t.Fatal(err)
	}
	if result.AgentDef.TimeoutSec != 86400 {
		t.Fatalf("timeout = %d, want 86400", result.AgentDef.TimeoutSec)
	}
}
