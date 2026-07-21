package agentdef

import (
	"fmt"
	"strings"
)

func (d *Definitions) ResolveModel(modelRef string) (*ResolvedModel, error) {
	providerName, profileName, ok := splitModelReference(modelRef)
	if !ok {
		return nil, fmt.Errorf("invalid model reference %q: must be provider/profile", modelRef)
	}

	provider, ok := d.Providers[providerName]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q in model reference %q", providerName, modelRef)
	}
	profile, ok := provider.Profiles[profileName]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q in provider %q", profileName, providerName)
	}

	resolved := &ResolvedModel{
		Provider: provider,
		Profile:  profile,
		Model:    profile.Model,
	}

	switch provider.Type {
	case ProviderTypeACP:
		resolved.Command = provider.Command
		resolved.Args = provider.Args
		resolved.ConfigOptions = profile.ConfigOptions
		resolved.PermissionOptionKind = profile.PermissionOptionKind
		if resolved.PermissionOptionKind == "" {
			resolved.PermissionOptionKind = "reject_once"
		}
	case ProviderTypeAgentCLI:
		if provider.Shim != nil {
			resolved.Shim = *provider.Shim
		}
		resolved.Agent = profile.Agent
		resolved.Approval = profile.Approval
		if resolved.Approval == "" {
			resolved.Approval = ApprovalReject
		}
		resolved.Variant = profile.Variant
	default:
		resolved.BaseURL = provider.BaseURL
		resolved.APIKeyEnv = provider.APIKeyEnv
		resolved.Headers = provider.Headers
		resolved.ReasoningEffort = profile.ReasoningEffort
		resolved.ExtraBody = profile.ExtraBody
		resolved.GenerateContentConfig = profile.GenerateContentConfig
		if resolved.Headers == nil {
			resolved.Headers = make(map[string]string)
		}
		if resolved.ExtraBody == nil {
			resolved.ExtraBody = make(map[string]any)
		}
	}

	return resolved, nil
}

// RequiredAPIKeyEnvs returns environment variable names only for provider
// types that require a model API key. agent_cli providers contribute nothing.
func (d *Definitions) RequiredAPIKeyEnvs() []string {
	seen := make(map[string]struct{})
	var envs []string
	for _, p := range d.Providers {
		if p.Type == ProviderTypeAgentCLI || p.Type == ProviderTypeACP {
			continue
		}
		if strings.TrimSpace(p.APIKeyEnv) == "" {
			continue
		}
		if _, ok := seen[p.APIKeyEnv]; !ok {
			seen[p.APIKeyEnv] = struct{}{}
			envs = append(envs, p.APIKeyEnv)
		}
	}
	return envs
}
