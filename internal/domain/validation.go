package domain

import (
	"errors"
	"fmt"
)

const (
	ExecutionModeForeground            = "foreground"
	ExecutionModeDurableJob            = "durable_job"
	DefaultExternalAgentTimeoutSeconds = 7200
	MaxExternalAgentTimeoutSeconds     = 86400
)

var errBadAgentKind = errors.New("agent kind must be llm or agent_cli")

// ValidateAgentKind validates the builder agent kind.
func ValidateAgentKind(kind AgentKind) error {
	if kind == AgentKindLLM || kind == AgentKindAgentCLI {
		return nil
	}
	return errBadAgentKind
}

// ValidateProviderKind validates the provider family allowed for an agent kind.
func ValidateProviderKind(kind AgentKind, providerType string) error {
	if err := ValidateAgentKind(kind); err != nil {
		return err
	}
	if (kind == AgentKindLLM && providerType == "openai_compatible") || (kind == AgentKindAgentCLI && providerType == "agent_cli") {
		return nil
	}
	return fmt.Errorf("provider type %q is not valid for agent kind %q", providerType, kind)
}

// ValidateExecutionMode validates execution modes available to a kind.
func ValidateExecutionMode(kind AgentKind, mode string) error {
	if err := ValidateAgentKind(kind); err != nil {
		return err
	}
	if (kind == AgentKindLLM && mode == ExecutionModeForeground) || (kind == AgentKindAgentCLI && (mode == ExecutionModeForeground || mode == ExecutionModeDurableJob)) {
		return nil
	}
	return fmt.Errorf("execution mode %q is not valid for agent kind %q", mode, kind)
}

// ValidateExternalAgentTimeout validates an external-agent timeout. Zero uses the default.
func ValidateExternalAgentTimeout(seconds int) error {
	if seconds < 0 || seconds > MaxExternalAgentTimeoutSeconds {
		return fmt.Errorf("external-agent timeout must be between 0 and %d seconds", MaxExternalAgentTimeoutSeconds)
	}
	return nil
}
