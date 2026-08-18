package agentdef

import "maps"

// NormalizeLegacy creates the in-memory definitions used by the legacy model
// configuration path. The returned definitions are never persisted. The
// legacy direct-inline admission is folded into the synthesized profile so
// both paths resolve the same per-profile contract.
func NormalizeLegacy(agentName, modelName, baseURL, apiKeyEnv, reasoningEffort string, headers map[string]string, extraBody map[string]any, maxDirectInlineBytes int) *Definitions {
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
					"default": {Model: modelName, ReasoningEffort: reasoningEffort, ExtraBody: clonedExtraBody, ResultHandles: ProfileResultHandlesConfig{MaxDirectInlineBytes: maxDirectInlineBytes}},
				},
			},
		},
		Agents: map[string]AgentDef{
			"root_agent": {Name: agentName, AgentClass: "LlmAgent", Model: "legacy/default", DurableSession: true},
		},
	}
}
