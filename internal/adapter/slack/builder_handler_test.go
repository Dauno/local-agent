package slack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/blockkit"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/agentbuilder"
)

func TestBuilderSubmissionRejectsMalformedTimeoutWithTimeoutField(t *testing.T) {
	callback := builderSubmissionCallback(string(domain.AgentKindAgentCLI), "agentcli/default")
	callback.View.State.Values["timeout_seconds"] = map[string]slackapi.BlockAction{
		"timeout_seconds": {BlockID: "timeout_seconds", ActionID: "timeout_seconds", Value: "not-a-number"},
	}
	handler := NewBuilderSubmissionHandler(nil, agentbuilder.New(), validBuilderDefinitions(), nil)
	response := handler.HandleSubmission(context.Background(), callback)
	if response == nil || response.ResponseAction != slackapi.RAErrors {
		t.Fatalf("response = %#v, want validation errors", response)
	}
	if _, ok := response.Errors["timeout_seconds"]; !ok {
		t.Fatalf("validation errors = %#v, want timeout_seconds", response.Errors)
	}
}

func TestBuilderSubmissionRejectsMissingNameWithNameField(t *testing.T) {
	callback := builderSubmissionCallback(string(domain.AgentKindLLM), "openai/fast")
	delete(callback.View.State.Values, "name")
	handler := NewBuilderSubmissionHandler(nil, agentbuilder.New(), validBuilderDefinitions(), nil)
	response := handler.HandleSubmission(context.Background(), callback)
	if response == nil || response.ResponseAction != slackapi.RAErrors {
		t.Fatalf("response = %#v, want validation errors", response)
	}
	if _, ok := response.Errors["name"]; !ok {
		t.Fatalf("validation errors = %#v, want name", response.Errors)
	}
}

func TestBuilderSubmissionValidatesKindAndProviderBeforeACK(t *testing.T) {
	defs := &agentdef.Definitions{Providers: map[string]agentdef.Provider{
		"openai": {
			Type:     agentdef.ProviderTypeOpenAICompatible,
			Profiles: map[string]agentdef.Profile{"fast": {}},
		},
		"agentcli": {
			Type:     agentdef.ProviderTypeAgentCLI,
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
		{name: "wrong provider for agent_cli", kind: "agent_cli", profile: "openai/fast", fieldWant: "model"},
		{name: "wrong provider for LLM", kind: "llm", profile: "agentcli/default", fieldWant: "model"},
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
				"agent_type":  {"agent_type": {BlockID: "agent_type", ActionID: "agent_type", SelectedOption: slackapi.OptionBlockObject{Value: kind}}},
				"model":       {"model": {BlockID: "model", ActionID: "model", SelectedOption: slackapi.OptionBlockObject{Value: profile}}},
			}},
		},
	}
}

type builderPreviewPostClient struct {
	fallback string
	blocks   []slackapi.Block
	channel  string
	thread   string
}

func (c *builderPreviewPostClient) PostMessage(context.Context, postRequest) (string, error) {
	return "message-1", nil
}

func (c *builderPreviewPostClient) PostBlocks(_ context.Context, channelID, fallbackText string, blocks []slackapi.Block, _ slackapi.SlackMetadata, threadTS string) (string, error) {
	c.channel = channelID
	c.fallback = fallbackText
	c.blocks = blocks
	c.thread = threadTS
	return "message-1", nil
}

func TestBuilderPreviewUsesDeclarativeViewAndCarriesDraftIdentity(t *testing.T) {
	client := &builderPreviewPostClient{}
	publisher := newPublisher(client, 0, nil, false)
	draft := domain.AgentDraft{Name: "builder_worker", ProviderProfile: "openai/fast"}
	definition := port.AgentDefPreview{AgentClass: "LlmAgent", ExecutionMode: domain.ExecutionModeForeground}
	yaml := strings.Repeat("界", builderBlockTextLimit*2)
	sha256 := strings.Repeat("a", 64)
	draftID := "draft_opaque_1"
	target := domain.ReplyTarget{ChannelID: "C12345678", ThreadTS: "1700000000.000001"}

	if err := publisher.publishBuilderPreview(t.Context(), target, draft, definition, yaml, sha256, draftID); err != nil {
		t.Fatalf("publishBuilderPreview() error = %v", err)
	}
	if client.channel != target.ChannelID || client.thread != target.ThreadTS {
		t.Fatalf("preview target = %q/%q, want %q/%q", client.channel, client.thread, target.ChannelID, target.ThreadTS)
	}
	message, err := publisher.previewEngine.Message(agentPreviewView{
		Name: "builder_worker", AgentClass: "LlmAgent", ProviderProfile: "openai/fast",
		ExecutionMode: domain.ExecutionModeForeground, Timeout: "no aplica", SHA256: sha256,
		DraftID: draftID, PreviewYAML: yaml,
	})
	if err != nil {
		t.Fatalf("render agent preview view = %v", err)
	}
	yamlPrefix := string([]rune(yaml)[:100])
	if !blockkit.Reachable(message, draft.Name) || !blockkit.Reachable(message, yamlPrefix) {
		t.Fatal("preview values did not reach the declarative view")
	}
	if slot, ok := blockkit.SlotOf(message, draft.Name); !ok || slot != slackapi.MarkdownType {
		t.Fatalf("preview name slot = %q, %t", slot, ok)
	}
	encoded, err := json.Marshal(client.blocks)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"value":"draft_opaque_1"`) {
		t.Fatalf("install action did not carry draft ID: %s", encoded)
	}
	if client.fallback != message.FallbackText {
		t.Fatalf("preview fallback = %q, want %q", client.fallback, message.FallbackText)
	}
}

func TestBuilderInstallRejectsMismatchedDraftIdentityAndState(t *testing.T) {
	baseDraft := &port.AgentDraft{
		DraftID: "draft_1", TeamID: "T12345678", ActorID: "U12345678",
		ConversationKey: "slack:T12345678:channel:C12345678:thread:1700000000.000001",
		Name:            "builder_worker", DefinitionHash: strings.Repeat("a", 64),
		Status:    port.DraftStatusPreviewed,
		ExpiresAt: timeNowForBuilderTest().Add(time.Hour),
	}
	baseCallback := slackapi.InteractionCallback{
		Team: slackapi.Team{ID: "T12345678"}, User: slackapi.User{ID: "U12345678"},

		Container: slackapi.Container{ChannelID: "C12345678", ThreadTs: "1700000000.000001"},
	}

	tests := []struct {
		name   string
		mutate func(*port.AgentDraft, *slackapi.InteractionCallback)
	}{
		{name: "unknown draft", mutate: func(_ *port.AgentDraft, _ *slackapi.InteractionCallback) {}},
		{name: "wrong actor", mutate: func(_ *port.AgentDraft, callback *slackapi.InteractionCallback) { callback.User.ID = "U87654321" }},
		{name: "wrong team", mutate: func(_ *port.AgentDraft, callback *slackapi.InteractionCallback) { callback.Team.ID = "T87654321" }},
		{name: "wrong conversation", mutate: func(_ *port.AgentDraft, callback *slackapi.InteractionCallback) {
			callback.Container.ChannelID = "C87654321"
		}},
		{name: "invalid status", mutate: func(draft *port.AgentDraft, _ *slackapi.InteractionCallback) {
			draft.Status = port.DraftStatusInstalled
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			draft := *baseDraft
			callback := baseCallback
			test.mutate(&draft, &callback)
			store := &builderDraftStoreFake{draft: &draft}
			handler := NewBuilderSubmissionHandler(store, agentbuilder.New(), validBuilderDefinitions(), &builderPublisherFake{})
			draftID := draft.DraftID
			if test.name == "unknown draft" {
				draftID = "draft_missing"
			}
			if err := handler.HandleInstallRequest(t.Context(), callback, draftID); err == nil {
				t.Fatal("HandleInstallRequest() accepted invalid draft identity or state")
			}
			if store.statusCalls != 0 {
				t.Fatalf("status updates = %d, want 0", store.statusCalls)
			}
		})
	}
}

func timeNowForBuilderTest() time.Time {
	return time.Now().UTC()
}
