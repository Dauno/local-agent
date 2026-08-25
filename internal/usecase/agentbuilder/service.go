package agentbuilder

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// Service implements port.AgentBuilderService.
type Service struct{}

func New() *Service {
	return &Service{}
}

// Preview compiles an AgentDraft into a validated AgentDef and returns the canonical YAML + SHA-256.
func (s *Service) Preview(draft domain.AgentDraft, current any) (*port.PreviewResult, error) {
	defs, ok := current.(*agentdef.Definitions)
	if !ok || defs == nil {
		return nil, fmt.Errorf("current agent definitions must not be nil")
	}

	kind := defaultKind(draft)
	if err := domain.ValidateAgentKind(kind); err != nil {
		return nil, err
	}

	mode := defaultMode(draft)
	if err := domain.ValidateExecutionMode(kind, mode); err != nil {
		return nil, err
	}

	var agent agentdef.AgentDef
	switch kind {
	case domain.AgentKindLLM:
		if draft.TimeoutSeconds != 0 {
			return nil, fmt.Errorf("timeout_seconds is only valid for ACP agents")
		}
		providerProfile := strings.TrimSpace(draft.ProviderProfile)
		model := providerProfile
		if model == "" {
			model = defaultModel(defs)
			if model == "" {
				return nil, fmt.Errorf("no default model available: no openai_compatible provider found")
			}
		}
		if err := validateProviderProfile(defs, kind, model); err != nil {
			return nil, err
		}
		agent = agentdef.AgentDef{
			AgentClass:      string(agentdef.AgentClassLLM),
			Name:            draft.Name,
			Model:           model,
			Description:     draft.Description,
			Instruction:     draft.Instruction,
			IncludeContents: "none",
			ToolScope:       agentdef.ToolScope{"invocation_scoped"},
		}
	case domain.AgentKindAgentCLI:
		if strings.TrimSpace(draft.Model) != "" {
			return nil, fmt.Errorf("model is not valid for ACP agents")
		}
		runtime := strings.TrimSpace(draft.ProviderProfile)
		if runtime == "" {
			return nil, fmt.Errorf("provider_profile is required for ACP agents")
		}
		if err := validateProviderProfile(defs, kind, runtime); err != nil {
			return nil, err
		}
		if err := domain.ValidateExternalAgentAllowlist(providerName(runtime)); err != nil {
			return nil, err
		}
		timeout := draft.TimeoutSeconds
		if timeout == 0 {
			timeout = domain.DefaultExternalAgentTimeoutSeconds
		}
		if err := domain.ValidateExternalAgentTimeout(timeout); err != nil {
			return nil, err
		}
		agent = agentdef.AgentDef{
			AgentClass:      string(agentdef.AgentClassLLM),
			Name:            draft.Name,
			Model:           runtime,
			IncludeContents: "none",
			Description:     draft.Description,
			Instruction:     draft.Instruction,
			ExecutionMode:   mode,
			TimeoutSeconds:  timeout,
			Confirmation:    "required",
		}
	}

	// Validate the candidate.
	if err := agentdef.ValidateCandidateAgent(defs, agent); err != nil {
		return nil, fmt.Errorf("invalid agent definition: %w", err)
	}

	// Marshal to canonical YAML.
	yamlBytes, err := agentdef.MarshalAgentDef(agent)
	if err != nil {
		return nil, fmt.Errorf("marshal agent definition: %w", err)
	}
	yamlStr := string(yamlBytes)

	// Calculate SHA-256.
	hash := sha256.Sum256(yamlBytes)
	shaHex := fmt.Sprintf("%x", hash)

	return &port.PreviewResult{
		AgentDef: port.AgentDefPreview{
			Name:          agent.Name,
			Model:         agent.Model,
			AgentClass:    agent.AgentClass,
			ExecutionMode: mode,
			TimeoutSec:    agent.TimeoutSeconds,
		},
		YAML:   yamlStr,
		SHA256: shaHex,
	}, nil
}

// ValidateInstallCandidate revalidates the persisted canonical definition
// against the current provider catalog and the persisted draft policy.
func (s *Service) ValidateInstallCandidate(draft domain.AgentDraft, candidate agentdef.AgentDef, defs *agentdef.Definitions) error {
	if defs == nil {
		return fmt.Errorf("current agent definitions are not available")
	}
	kind := defaultKind(draft)
	if err := domain.ValidateAgentKind(kind); err != nil {
		return err
	}
	if err := agentdef.ValidateCandidateAgent(defs, candidate); err != nil {
		return fmt.Errorf("invalid canonical agent definition: %w", err)
	}
	if candidate.Name != draft.Name {
		return fmt.Errorf("canonical agent definition does not match draft name")
	}

	expectedClass := string(agentdef.AgentClassLLM)
	reference := candidate.Model
	if kind == domain.AgentKindAgentCLI {
		reference = candidate.Model
	}
	if candidate.AgentClass != expectedClass {
		return fmt.Errorf("canonical agent class does not match draft kind")
	}
	if err := validateProviderProfile(defs, kind, reference); err != nil {
		return fmt.Errorf("invalid canonical agent provider: %w", err)
	}
	provider := providerName(reference)
	if kind == domain.AgentKindAgentCLI {
		if strings.TrimSpace(draft.Model) != "" {
			return fmt.Errorf("model is not valid for ACP agents")
		}
		if err := domain.ValidateExternalAgentAllowlist(provider); err != nil {
			return err
		}
		if candidate.Model != "" || candidate.Confirmation != "required" {
			return fmt.Errorf("canonical ACP agent has incompatible model or confirmation")
		}
		expectedMode := defaultMode(draft)
		if err := domain.ValidateExecutionMode(kind, expectedMode); err != nil {
			return err
		}
		if candidate.ExecutionMode != expectedMode {
			return fmt.Errorf("canonical agent execution mode does not match draft")
		}
		expectedTimeout := draft.TimeoutSeconds
		if expectedTimeout == 0 {
			expectedTimeout = domain.DefaultExternalAgentTimeoutSeconds
		}
		if err := domain.ValidateExternalAgentTimeout(draft.TimeoutSeconds); err != nil {
			return err
		}
		if candidate.TimeoutSeconds != expectedTimeout {
			return fmt.Errorf("canonical agent timeout does not match draft")
		}
		return nil
	}

	if draft.TimeoutSeconds != 0 || candidate.TimeoutSeconds != 0 || candidate.ExecutionMode != "" || draft.ExecutionMode != domain.ExecutionModeForeground {
		return fmt.Errorf("canonical LLM agent has incompatible execution policy")
	}
	if draft.Model != "" && candidate.Model != draft.Model {
		return fmt.Errorf("canonical agent model does not match draft")
	}
	return nil
}

func defaultKind(draft domain.AgentDraft) domain.AgentKind {
	if draft.Kind == "" {
		return domain.AgentKindLLM
	}
	return draft.Kind
}

func defaultMode(draft domain.AgentDraft) string {
	if draft.ExecutionMode == "" {
		return domain.ExecutionModeForeground
	}
	return draft.ExecutionMode
}

func providerName(ref string) string { return strings.SplitN(ref, "/", 2)[0] }

func validateProviderProfile(defs *agentdef.Definitions, kind domain.AgentKind, reference string) error {
	parts := strings.Split(reference, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return fmt.Errorf("provider_profile must be provider/profile format")
	}
	provider, exists := defs.Providers[parts[0]]
	if !exists {
		return fmt.Errorf("unknown provider %q", parts[0])
	}
	if err := domain.ValidateProviderKind(kind, provider.Type); err != nil {
		return err
	}
	if _, exists := provider.Profiles[parts[1]]; !exists {
		return fmt.Errorf("unknown profile %q in provider %q", parts[1], parts[0])
	}
	return nil
}

// defaultModel finds the first openai_compatible provider/profile as default.
func defaultModel(defs *agentdef.Definitions) string {
	providerNames := make([]string, 0, len(defs.Providers))
	for name := range defs.Providers {
		providerNames = append(providerNames, name)
	}
	sort.Strings(providerNames)

	for _, name := range providerNames {
		provider := defs.Providers[name]
		if provider.Type == agentdef.ProviderTypeOpenAICompatible {
			profileNames := make([]string, 0, len(provider.Profiles))
			for profileName := range provider.Profiles {
				profileNames = append(profileNames, profileName)
			}
			sort.Strings(profileNames)
			for _, profileName := range profileNames {
				return name + "/" + profileName
			}
		}
	}
	return ""
}
