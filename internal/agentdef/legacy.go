package agentdef

import "maps"

// NormalizeLegacy creates the in-memory definitions used by the legacy model
// configuration path. The returned definitions are never persisted.
func NormalizeLegacy(agentName, modelName, baseURL, apiKeyEnv, reasoningEffort string, headers map[string]string, extraBody map[string]any) *Definitions {
	clonedHeaders := maps.Clone(headers)
	clonedExtraBody := maps.Clone(extraBody)
	return &Definitions{
		Providers: map[string]Provider{
			"legacy": {
				Name:      "legacy",
				Type:      ProviderTypeOpenAICompatible,
				BaseURL:   baseURL,
				APIKeyEnv: apiKeyEnv,
				Headers:   clonedHeaders,
				Profiles: map[string]Profile{
					"default": {Model: modelName, ReasoningEffort: reasoningEffort, ExtraBody: clonedExtraBody},
				},
			},
		},
		Agents: map[string]AgentDef{
			"root_agent": {Name: agentName, AgentClass: "LlmAgent", Model: "legacy/default", DurableSession: true},
		},
	}
}
