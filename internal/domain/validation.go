package domain

import "fmt"

const (
	ExecutionModeForeground  = "foreground"
	ExecutionModeDurableJob  = "durable_job"
	DefaultACPTimeoutSeconds = 7200
	MaxACPTimeoutSeconds     = 86400
)

// ValidateAgentKind validates the builder agent kind.
func ValidateAgentKind(kind AgentKind) error {
	switch kind {
	case AgentKindLLM, AgentKindACP:
		return nil
	default:
		return fmt.Errorf("agent kind must be llm or acp")
	}
}

// ValidateProviderKind validates the provider family allowed for an agent kind.
func ValidateProviderKind(kind AgentKind, providerType string) error {
	switch kind {
	case AgentKindLLM:
		if providerType == "openai_compatible" {
			return nil
		}
	case AgentKindACP:
		if providerType == "acp" {
			return nil
		}
	default:
		return fmt.Errorf("agent kind must be llm or acp")
	}
	return fmt.Errorf("provider type %q is not valid for agent kind %q", providerType, kind)
}

// ValidateExecutionMode validates execution modes available to a kind.
func ValidateExecutionMode(kind AgentKind, mode string) error {
	switch kind {
	case AgentKindLLM:
		if mode == ExecutionModeForeground {
			return nil
		}
	case AgentKindACP:
		if mode == ExecutionModeForeground || mode == ExecutionModeDurableJob {
			return nil
		}
	default:
		return fmt.Errorf("agent kind must be llm or acp")
	}
	return fmt.Errorf("execution mode %q is not valid for agent kind %q", mode, kind)
}

// ValidateACPTimeout validates an ACP timeout. Zero is accepted for defaulting.
func ValidateACPTimeout(seconds int) error {
	if seconds < 0 || seconds > MaxACPTimeoutSeconds {
		return fmt.Errorf("ACP timeout must be between 0 and %d seconds", MaxACPTimeoutSeconds)
	}
	return nil
}

// ValidateACPAllowlist enforces the only supported ACP provider name.
func ValidateACPAllowlist(providerName string) error {
	if providerName == "opencode" {
		return nil
	}
	return fmt.Errorf("ACP provider %q is not allowed", providerName)
}
