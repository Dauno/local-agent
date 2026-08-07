package domain

import (
	"errors"
	"fmt"
)

const (
	ExecutionModeForeground  = "foreground"
	ExecutionModeDurableJob  = "durable_job"
	DefaultACPTimeoutSeconds = 7200
	MaxACPTimeoutSeconds     = 86400
)

var errBadAgentKind = errors.New("agent kind must be llm or acp")

// ValidateAgentKind validates the builder agent kind.
func ValidateAgentKind(kind AgentKind) error {
	if kind == AgentKindLLM || kind == AgentKindACP {
		return nil
	}
	return errBadAgentKind
}

// ValidateProviderKind validates the provider family allowed for an agent kind.
func ValidateProviderKind(kind AgentKind, providerType string) error {
	if err := ValidateAgentKind(kind); err != nil {
		return err
	}
	if (kind == AgentKindLLM && providerType == "openai_compatible") || (kind == AgentKindACP && providerType == "acp") {
		return nil
	}
	return fmt.Errorf("provider type %q is not valid for agent kind %q", providerType, kind)
}

// ValidateExecutionMode validates execution modes available to a kind.
func ValidateExecutionMode(kind AgentKind, mode string) error {
	if err := ValidateAgentKind(kind); err != nil {
		return err
	}
	if (kind == AgentKindLLM && mode == ExecutionModeForeground) || (kind == AgentKindACP && (mode == ExecutionModeForeground || mode == ExecutionModeDurableJob)) {
		return nil
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
