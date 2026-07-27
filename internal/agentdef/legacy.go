package agentdef

// NormalizeLegacy creates the in-memory definitions used by the legacy model
// configuration path. The returned definitions are never persisted.
func NormalizeLegacy(agentName, modelName, baseURL, apiKeyEnv, reasoningEffort string, headers map[string]string, extraBody map[string]any) *Definitions {
	clonedHeaders := make(map[string]string, len(headers))
	for key, value := range headers {
		clonedHeaders[key] = value
	}
	clonedExtraBody := make(map[string]any, len(extraBody))
	for key, value := range extraBody {
		clonedExtraBody[key] = value
	}
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
