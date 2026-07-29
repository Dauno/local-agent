package agentbuilder

import (
	"crypto/sha256"
	"fmt"
	"sort"

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

	// Use default model if not specified.
	model := draft.Model
	if model == "" {
		// Find the first openai_compatible provider/profile as default.
		model = defaultModel(defs)
		if model == "" {
			return nil, fmt.Errorf("no default model available: no openai_compatible provider found")
		}
	}

	// Build the AgentDef with restricted fields.
	agent := agentdef.AgentDef{
		AgentClass:      "LlmAgent",
		Name:            draft.Name,
		Model:           model,
		Description:     draft.Description,
		Instruction:     draft.Instruction,
		IncludeContents: "none",
		ToolScope:       "invocation_scoped",
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
		AgentDef: port.AgentDefPreview{Name: agent.Name, Model: agent.Model, AgentClass: agent.AgentClass},
		YAML:     yamlStr,
		SHA256:   shaHex,
	}, nil
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
