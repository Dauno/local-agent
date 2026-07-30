package slack

import (
	"context"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/usecase/agentbuilder"
	slackapi "github.com/slack-go/slack"
)

func TestBuilderSubmissionValidatesKindAndProviderBeforeACK(t *testing.T) {
	defs := &agentdef.Definitions{Providers: map[string]agentdef.Provider{
		"openai": {
			Type:     agentdef.ProviderTypeOpenAICompatible,
			Profiles: map[string]agentdef.Profile{"fast": {}},
		},
		"opencode": {
			Type:     agentdef.ProviderTypeACP,
			Profiles: map[string]agentdef.Profile{"default": {}},
		},
	}}
	handler := NewBuilderSubmissionHandler(nil, agentbuilder.New(), defs, nil)

	tests := []struct {
		name      string
		kind      string
		profile   string
		fieldWant string
	}{
		{name: "invalid kind", kind: "unsupported", profile: "openai/fast", fieldWant: "agent_type"},
		{name: "wrong provider for ACP", kind: "acp", profile: "openai/fast", fieldWant: "model"},
		{name: "wrong provider for LLM", kind: "llm", profile: "opencode/default", fieldWant: "model"},
		{name: "unknown provider", kind: "llm", profile: "missing/default", fieldWant: "model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := handler.HandleSubmission(context.Background(), builderSubmissionCallback(tt.kind, tt.profile))
			if response == nil {
				t.Fatal("HandleSubmission() returned nil for invalid submission")
			}
			if response.ResponseAction != slackapi.RAErrors {
				t.Fatalf("response action = %q, want errors", response.ResponseAction)
			}
			if _, ok := response.Errors[tt.fieldWant]; !ok {
				t.Fatalf("errors = %#v, want field %q", response.Errors, tt.fieldWant)
			}
		})
	}
}

func builderSubmissionCallback(kind, profile string) slackapi.InteractionCallback {
	return slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeViewSubmission,
		View: slackapi.View{
			CallbackID: builderSubmitCallbackID,
			State: &slackapi.ViewState{Values: map[string]map[string]slackapi.BlockAction{
				"name":        {"name": {BlockID: "name", ActionID: "name", Value: "builder_worker"}},
				"description": {"description": {BlockID: "description", ActionID: "description", Value: "description"}},
				"instruction": {"instruction": {BlockID: "instruction", ActionID: "instruction", Value: "instruction"}},
				"agent_type":  {"agent_type": {BlockID: "agent_type", ActionID: "agent_type", Value: kind}},
				"model":       {"model": {BlockID: "model", ActionID: "model", SelectedOption: slackapi.OptionBlockObject{Value: profile}}},
			}},
		},
	}
}
