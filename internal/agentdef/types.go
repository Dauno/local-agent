// Package agentdef defines declarative, versioned agent and provider
// definitions loaded from .local-agent/agents/ and .local-agent/providers/.
//
// This package is dependency-free within the project: it depends only on the Go
// standard library and gopkg.in/yaml.v3.
package agentdef

import (
	"fmt"
	"regexp"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Provider types.
const (
	ProviderTypeOpenAICompatible = "openai_compatible"
	ProviderTypeAgentCLI         = "agent_cli"
	ProviderTypeACP              = "acp"
)

const (
	ExecutionModeForeground = "foreground"
	ExecutionModeDurableJob = "durable_job"
	MaxACPTimeoutSeconds    = 24 * 60 * 60
)

// VisualEstimatorID is the only media-capable token counter combination
// implemented for P0 (FR-18). The profile selected for attachment_analyzer
// must use it before rollout; any other combination fails before traffic.
const VisualEstimatorID = "visual-tile-conservative-v1"

const (
	MaxAgentNameLength   = 64
	MinAgentNameLength   = 3
	MaxDescriptionLength = 500
	MaxInstructionLength = 3000
	AgentNamePattern     = `^[a-z][a-z0-9_-]{2,63}$`
)

var validAgentNamePattern = regexp.MustCompile(AgentNamePattern)

func IsReservedAgentName(name string) bool {
	switch name {
	case "root_agent", "user", "explore", "attachment_analyzer", "memory_curator":
		return true
	default:
		return false
	}
}

func IsDirectToolName(name string) bool {
	switch name {
	case "list_repos", "list_directory", "read_file", "list_worktrees", "create_worktree", "remove_worktree", "list_messages", "job_status", "read_job_result", "read_job_result_chunk", "create_canvas", "export_text", "export_markdown", "export_csv", "export_json", "preview_agent_def", "install_agent_def", "publish_builder_launcher", "manage_opencode":
		return true
	default:
		return false
	}
}

func ValidateAgentName(name string) error {
	if name == "" {
		return fmt.Errorf("agent name must not be empty")
	}
	if len(name) < MinAgentNameLength || len(name) > MaxAgentNameLength {
		return fmt.Errorf("agent name length must be between %d and %d characters", MinAgentNameLength, MaxAgentNameLength)
	}
	if !validAgentNamePattern.MatchString(name) {
		return fmt.Errorf("agent name must match %s", AgentNamePattern)
	}
	if IsReservedAgentName(name) {
		return fmt.Errorf("agent name %q is reserved", name)
	}
	if IsDirectToolName(name) {
		return fmt.Errorf("agent name %q conflicts with a direct tool", name)
	}
	return nil
}

// Approval modes for agent_cli profiles.
const (
	ApprovalReject = "reject"
	ApprovalAuto   = "auto"
)

type Provider struct {
	Name      string             `yaml:"name"`
	Type      string             `yaml:"type"`
	BaseURL   string             `yaml:"base_url,omitempty"`
	APIKeyEnv string             `yaml:"api_key_env,omitempty"`
	Headers   map[string]string  `yaml:"headers,omitempty"`
	Shim      *ShimConfig        `yaml:"shim,omitempty"`
	Profiles  map[string]Profile `yaml:"profiles"`

	// ACP provider fields.
	Command string   `yaml:"command,omitempty"`
	Args    []string `yaml:"args,omitempty"`
}

// ShimConfig is the executable mapper configuration for an agent_cli provider.
type ShimConfig struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args,omitempty"`
}

type Profile struct {
	Model                 string                 `yaml:"model"`
	ReasoningEffort       string                 `yaml:"reasoning_effort,omitempty"`
	ExtraBody             map[string]any         `yaml:"extra_body,omitempty"`
	GenerateContentConfig *GenerateContentConfig `yaml:"generate_content_config,omitempty"`

	// agent_cli profile fields.
	Agent    string `yaml:"agent,omitempty"`
	Approval string `yaml:"approval,omitempty"`
	Variant  string `yaml:"variant,omitempty"`

	ContextWindowTokens *int             `yaml:"context_window_tokens,omitempty"`
	MaxOutputTokens     *int             `yaml:"max_output_tokens,omitempty"`
	TokenCounter        *TokenCounterDef `yaml:"token_counter,omitempty"`

	// acp profile fields.
	ConfigOptions        []ACPConfigOption `yaml:"config_options,omitempty"`
	PermissionOptionKind string            `yaml:"permission_option_kind,omitempty"`
}

type TokenCounterDef struct {
	Strategy string `yaml:"strategy"`
	ID       string `yaml:"id,omitempty"`
}

type ACPConfigOption struct {
	ID    string `yaml:"id"`
	Value any    `yaml:"value"`
}

type GenerateContentConfig struct {
	Temperature     *float64 `yaml:"temperature,omitempty"`
	MaxOutputTokens int      `yaml:"max_output_tokens,omitempty"`
	TopP            *float64 `yaml:"top_p,omitempty"`
	TopK            *float64 `yaml:"top_k,omitempty"`
	StopSequences   []string `yaml:"stop_sequences,omitempty"`
}

type AgentContextBudget struct {
	MaxRequestPercent int `yaml:"max_request_percent,omitempty"`
}

// ToolScope lists the scopes and declarative tool names available to an agent.
// YAML accepts a scalar ("invocation_scoped") or a sequence
// ("[invocation_scoped, ripgrep]").
type ToolScope []string

// UnmarshalYAML accepts a scalar or a sequence of strings.
func (t *ToolScope) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*t = ToolScope{node.Value}
		return nil
	case yaml.SequenceNode:
		result := make(ToolScope, 0, len(node.Content))
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("tool_scope entries must be strings")
			}
			result = append(result, item.Value)
		}
		*t = result
		return nil
	default:
		return fmt.Errorf("tool_scope must be a string or a list of strings")
	}
}

// MarshalYAML renders a single scope as a scalar to keep seeded definitions
// stable, and a multi-entry scope as a sequence.
func (t ToolScope) MarshalYAML() (any, error) {
	if len(t) == 1 {
		return t[0], nil
	}
	return []string(t), nil
}

// Contains reports whether the scope list includes value.
func (t ToolScope) Contains(value string) bool {
	for _, entry := range t {
		if entry == value {
			return true
		}
	}
	return false
}

type AgentDef struct {
	AgentClass                 string              `yaml:"agent_class"`
	Name                       string              `yaml:"name"`
	Model                      string              `yaml:"model,omitempty"`
	Description                string              `yaml:"description,omitempty"`
	GlobalInstruction          string              `yaml:"global_instruction,omitempty"`
	DelegatedGlobalInstruction string              `yaml:"delegated_global_instruction,omitempty"`
	Instruction                string              `yaml:"instruction"`
	IncludeContents            string              `yaml:"include_contents,omitempty"`
	Mode                       string              `yaml:"mode,omitempty"`
	DurableSession             bool                `yaml:"durable_session,omitempty"`
	ToolScope                  ToolScope           `yaml:"tool_scope,omitempty"`
	AgentTools                 []string            `yaml:"agent_tools,omitempty"`
	WorkflowTools              []string            `yaml:"workflow_tools,omitempty"`
	ContextBudget              *AgentContextBudget `yaml:"context_budget,omitempty"`
	TimeoutSeconds             int                 `yaml:"timeout_seconds,omitempty"`
	ExecutionMode              string              `yaml:"execution_mode,omitempty"`
	Role                       string              `yaml:"role,omitempty"`

	// AcpAgent fields.
	Runtime      string `yaml:"runtime,omitempty"`
	Confirmation string `yaml:"confirmation,omitempty"`
}

// EffectiveContextBudgetPercent returns the agent override or the configured
// root percentage when no override is present. Zero means inherit.
func (a AgentDef) EffectiveContextBudgetPercent(defaultPercent int) int {
	if a.ContextBudget != nil && a.ContextBudget.MaxRequestPercent != 0 {
		return a.ContextBudget.MaxRequestPercent
	}
	return defaultPercent
}

func (a AgentDef) ValidateName() error {
	return ValidateAgentName(a.Name)
}

func (a AgentDef) ValidateSize() error {
	if utf8.RuneCountInString(a.Description) > MaxDescriptionLength {
		return fmt.Errorf("agent description exceeds %d characters", MaxDescriptionLength)
	}
	if utf8.RuneCountInString(a.Instruction) > MaxInstructionLength {
		return fmt.Errorf("agent instruction exceeds %d characters", MaxInstructionLength)
	}
	return nil
}

func (a AgentDef) EffectiveRootGlobalInstruction() string {
	if a.DelegatedGlobalInstruction == "" {
		return a.GlobalInstruction
	}
	return a.DelegatedGlobalInstruction + "\n\n" + a.GlobalInstruction
}

func (a AgentDef) EffectiveDelegatedGlobalInstruction() string {
	if a.DelegatedGlobalInstruction != "" {
		return a.DelegatedGlobalInstruction
	}
	return a.GlobalInstruction
}

type Definitions struct {
	Providers map[string]Provider
	Agents    map[string]AgentDef
}

type ResolvedModel struct {
	Provider Provider
	Profile  Profile
	Model    string

	// openai_compatible provider fields.
	BaseURL               string
	APIKeyEnv             string
	Headers               map[string]string
	ReasoningEffort       string
	ExtraBody             map[string]any
	GenerateContentConfig *GenerateContentConfig

	// agent_cli provider fields.
	Shim     ShimConfig
	Agent    string
	Approval string
	Variant  string

	ContextWindowTokens int
	MaxOutputTokens     int
	CounterStrategy     string
	CounterID           string

	// acp provider fields.
	Command              string
	Args                 []string
	ConfigOptions        []ACPConfigOption
	PermissionOptionKind string
}

// Type returns the resolved provider family.
func (r *ResolvedModel) Type() string {
	if r == nil {
		return ""
	}
	return r.Provider.Type
}

// IsAgentCLI reports whether the resolved model is backed by an agent CLI.
func (r *ResolvedModel) IsAgentCLI() bool {
	return r.Type() == ProviderTypeAgentCLI
}

// IsACP reports whether the resolved model is backed by an ACP agent.
func (r *ResolvedModel) IsACP() bool {
	return r.Type() == ProviderTypeACP
}
