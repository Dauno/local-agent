package agentbuilder

import (
	"testing"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
)

func TestDefaultModelSortsProvidersAndProfiles(t *testing.T) {
	defs := &agentdef.Definitions{
		Providers: map[string]agentdef.Provider{
			"z-provider": {
				Type:     agentdef.ProviderTypeOpenAICompatible,
				Profiles: map[string]agentdef.Profile{"default": {}},
			},
			"a-provider": {
				Type: agentdef.ProviderTypeOpenAICompatible,
				Profiles: map[string]agentdef.Profile{
					"z-profile": {},
					"a-profile": {},
				},
			},
		},
	}

	if got := defaultModel(defs); got != "a-provider/a-profile" {
		t.Fatalf("defaultModel() = %q, want %q", got, "a-provider/a-profile")
	}
}
