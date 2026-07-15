package domain

// Provider families stored in durable root session state. Switching a durable
// conversation between families requires an operator-run init --reset-state.
const (
	// ProviderFamilyStateKey is the reserved ADK session-state key holding the
	// provider family that created the session.
	ProviderFamilyStateKey = "local_agent_provider_family"

	ProviderFamilyOpenAICompatible = "openai_compatible"
	ProviderFamilyAgentCLI         = "agent_cli"
)
