package agentdef

import (
	"fmt"
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
		Provider:              provider,
		Profile:               profile,
		Model:                 profile.Model,
		BaseURL:               provider.BaseURL,
		APIKeyEnv:             provider.APIKeyEnv,
		Headers:               provider.Headers,
		ReasoningEffort:       profile.ReasoningEffort,
		ExtraBody:             profile.ExtraBody,
		GenerateContentConfig: profile.GenerateContentConfig,
	}

	if resolved.Headers == nil {
		resolved.Headers = make(map[string]string)
	}
	if resolved.ExtraBody == nil {
		resolved.ExtraBody = make(map[string]any)
	}

	return resolved, nil
}

func (d *Definitions) RequiredAPIKeyEnvs() []string {
	seen := make(map[string]struct{})
	var envs []string
	for _, p := range d.Providers {
		if _, ok := seen[p.APIKeyEnv]; !ok {
			seen[p.APIKeyEnv] = struct{}{}
			envs = append(envs, p.APIKeyEnv)
		}
	}
	return envs
}
