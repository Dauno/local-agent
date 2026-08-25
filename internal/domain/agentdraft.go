package domain

// AgentKind identifies the type of agent to build.
type AgentKind string

const (
	AgentKindLLM      AgentKind = "llm"
	AgentKindAgentCLI AgentKind = "agent_cli"
)

// AgentDraft is the user-provided input for creating a new agent definition.
type AgentDraft struct {
	Name            string
	Description     string
	Instruction     string
	Model           string // optional; provider/profile for LLM, empty for ACP
	Kind            AgentKind
	ProviderProfile string // canonical provider/profile reference
	ExecutionMode   string // foreground or durable_job for ACP
	TimeoutSeconds  int    // ACP only
}
