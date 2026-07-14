package agentdef

import "gopkg.in/yaml.v3"

type SeedModelConfig struct {
	Name            string
	BaseURL         string
	APIKeyEnv       string
	Headers         map[string]string
	ReasoningEffort string
	ExtraBody       map[string]any
}

func SeedDeepSeekProvider(cfg SeedModelConfig) Provider {
	extraBody := make(map[string]any)
	for k, v := range cfg.ExtraBody {
		extraBody[k] = v
	}

	return Provider{
		Name:      "deepseek",
		Type:      "openai_compatible",
		BaseURL:   cfg.BaseURL,
		APIKeyEnv: cfg.APIKeyEnv,
		Headers:   copyStringMap(cfg.Headers),
		Profiles: map[string]Profile{
			"flash-reasoning": {
				Model:           cfg.Name,
				ReasoningEffort: cfg.ReasoningEffort,
				ExtraBody:       extraBody,
			},
			"flash-json": {
				Model: cfg.Name,
				ExtraBody: map[string]any{
					// DeepSeek V4 enables thinking by default; reserve this profile's output budget for curator JSON.
					"thinking": map[string]any{
						"type": "disabled",
					},
					"response_format": map[string]any{
						"type": "json_object",
					},
				},
				GenerateContentConfig: &GenerateContentConfig{
					Temperature:     float64Ptr(0),
					MaxOutputTokens: 1200,
				},
			},
		},
	}
}

func SeedRootAgent(modelRef string) AgentDef {
	return AgentDef{
		AgentClass:      "LlmAgent",
		Name:            "root_agent",
		Model:           modelRef,
		Description:     "Slack conversational assistant with approved tools.",
		Instruction:     "You are Dev Agent.\n\nAnswer concisely by default.\n",
		Mode:            "chat",
		IncludeContents: "default",
		DurableSession:  true,
		ToolScope:       "invocation_scoped",
	}
}

func SeedMemoryCurator(modelRef string) AgentDef {
	return AgentDef{
		AgentClass:      "LlmAgent",
		Name:            "memory_curator",
		Model:           modelRef,
		Description:     "Extracts durable knowledge as JSON.",
		Instruction:     "You are a Memory Curator for a knowledge management system.\n\nReturn only one JSON object with an operations array.\nExample: {\"operations\":[]}\n",
		IncludeContents: "none",
		TimeoutSeconds:  120,
		Role:            "memory_curator",
	}
}

func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func float64Ptr(v float64) *float64 {
	return &v
}

func MarshalProvider(p Provider) ([]byte, error) {
	return yaml.Marshal(p)
}

func MarshalAgentDef(a AgentDef) ([]byte, error) {
	return yaml.Marshal(a)
}
