// Package agentdef defines declarative, versioned agent and provider
// definitions loaded from .local-agent/agents/ and .local-agent/providers/.
//
// This package is dependency-free within the project: it depends only on the Go
// standard library and gopkg.in/yaml.v3.
package agentdef

type Provider struct {
	Name      string            `yaml:"name"`
	Type      string            `yaml:"type"`
	BaseURL   string            `yaml:"base_url"`
	APIKeyEnv string            `yaml:"api_key_env"`
	Headers   map[string]string `yaml:"headers,omitempty"`
	Profiles  map[string]Profile `yaml:"profiles"`
}

type Profile struct {
	Model                 string                  `yaml:"model"`
	ReasoningEffort       string                  `yaml:"reasoning_effort,omitempty"`
	ExtraBody             map[string]any          `yaml:"extra_body,omitempty"`
	GenerateContentConfig *GenerateContentConfig  `yaml:"generate_content_config,omitempty"`
}

type GenerateContentConfig struct {
	Temperature     *float64 `yaml:"temperature,omitempty"`
	MaxOutputTokens int      `yaml:"max_output_tokens,omitempty"`
	TopP            *float64 `yaml:"top_p,omitempty"`
	TopK            *float64 `yaml:"top_k,omitempty"`
	StopSequences   []string `yaml:"stop_sequences,omitempty"`
}

type AgentDef struct {
	AgentClass      string `yaml:"agent_class"`
	Name            string `yaml:"name"`
	Model           string `yaml:"model"`
	Description     string `yaml:"description,omitempty"`
	Instruction     string `yaml:"instruction"`
	IncludeContents string `yaml:"include_contents,omitempty"`
	Mode            string `yaml:"mode,omitempty"`
	DurableSession  bool   `yaml:"durable_session,omitempty"`
	ToolScope       string `yaml:"tool_scope,omitempty"`
	TimeoutSeconds  int    `yaml:"timeout_seconds,omitempty"`
	Role            string `yaml:"role,omitempty"`
}

type Definitions struct {
	Providers map[string]Provider
	Agents    map[string]AgentDef
}

type ResolvedModel struct {
	Provider              Provider
	Profile               Profile
	Model                 string
	BaseURL               string
	APIKeyEnv             string
	Headers               map[string]string
	ReasoningEffort       string
	ExtraBody             map[string]any
	GenerateContentConfig *GenerateContentConfig
}
