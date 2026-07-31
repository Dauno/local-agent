package slack

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/agentbuilder"
	slackapi "github.com/slack-go/slack"
)

func TestBuilderInteractionContextRoundTripsConversationIdentity(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:channel:C12345678:thread:1785451523.453999")
	encoded, err := encodeBuilderInteractionContext("U12345678", key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded, `"v":1`) || strings.Contains(encoded, "secret") {
		t.Fatalf("encoded context = %q", encoded)
	}

	callback := slackapi.InteractionCallback{
		User:      slackapi.User{ID: "U12345678"},
		Team:      slackapi.Team{ID: "T12345678"},
		Container: slackapi.Container{ChannelID: "C12345678", ThreadTs: "1785451523.453999"},
	}
	decoded, target, err := decodeBuilderInteractionContext(encoded, callback)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ActorID != "U12345678" || decoded.ConversationKey != string(key) {
		t.Fatalf("decoded context = %#v", decoded)
	}
	if target.ChannelID != "C12345678" || target.ThreadTS != "1785451523.453999" {
		t.Fatalf("target = %#v", target)
	}
}

func TestLegacyBuilderActionIsConvertedToVersionedMetadata(t *testing.T) {
	callback := slackapi.InteractionCallback{
		User:      slackapi.User{ID: "U12345678"},
		Team:      slackapi.Team{ID: "T12345678"},
		Container: slackapi.Container{ChannelID: "C12345678", ThreadTs: "1785451523.453999"},
	}
	metadata, target, err := builderActionContext(callback, "U12345678")
	if err != nil {
		t.Fatal(err)
	}
	if target.ChannelID != "C12345678" || target.ThreadTS != "1785451523.453999" {
		t.Fatalf("target = %#v", target)
	}
	if _, _, err := decodeBuilderInteractionContext(metadata, callback); err != nil {
		t.Fatalf("legacy metadata = %q is not versioned: %v", metadata, err)
	}
}

func TestLegacyBuilderGroupActionUsesGroupConversationKind(t *testing.T) {
	callback := slackapi.InteractionCallback{
		User:      slackapi.User{ID: "U12345678"},
		Team:      slackapi.Team{ID: "T12345678"},
		Container: slackapi.Container{ChannelID: "G12345678", ThreadTs: "1785451523.453999"},
	}
	metadata, _, err := builderActionContext(callback, "U12345678")
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := decodeBuilderInteractionContext(metadata, callback)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ConversationKey != "slack:T12345678:group:G12345678:thread:1785451523.453999" {
		t.Fatalf("conversation key = %q", decoded.ConversationKey)
	}
}

func TestBuilderInteractionContextRejectsCrossActorAndCrossTeam(t *testing.T) {
	metadata, err := encodeBuilderInteractionContext("U12345678", "slack:T12345678:dm:D12345678")
	if err != nil {
		t.Fatal(err)
	}
	for _, callback := range []slackapi.InteractionCallback{
		{User: slackapi.User{ID: "U87654321"}, Team: slackapi.Team{ID: "T12345678"}},
		{User: slackapi.User{ID: "U12345678"}, Team: slackapi.Team{ID: "T87654321"}},
	} {
		if _, _, err := decodeBuilderInteractionContext(metadata, callback); err == nil {
			t.Fatal("tampered builder context was accepted")
		}
	}
}

func TestBuilderInteractionContextRejectsMismatchedConversationKind(t *testing.T) {
	for _, key := range []domain.ConversationKey{
		"slack:T12345678:dm:C12345678",
		"slack:T12345678:channel:D12345678:thread:1785451523.453999",
		"slack:T12345678:group:C12345678:thread:1785451523.453999",
	} {
		if _, err := encodeBuilderInteractionContext("U12345678", key); err == nil {
			t.Fatalf("mismatched conversation %q was accepted", key)
		}
	}
}

func TestBuilderModalUpdatePreservesPrivateMetadata(t *testing.T) {
	const metadata = `{"v":1,"actor_id":"U12345678","conversation_key":"slack:T12345678:dm:D12345678"}`
	presenter := NewBuilderModalPresenter(nil)
	callback := slackapi.InteractionCallback{
		User: slackapi.User{ID: "U12345678"},
		View: slackapi.View{
			PrivateMetadata: metadata,
			State: &slackapi.ViewState{Values: map[string]map[string]slackapi.BlockAction{
				"agent_type": {"agent_type": {Value: string(domain.AgentKindACP)}},
			}},
		},
	}
	view := presenter.BuildViewForCallback(callback)
	if view.PrivateMetadata != metadata {
		t.Fatalf("private metadata = %q, want byte-identical metadata", view.PrivateMetadata)
	}
}

func TestBuilderSubmissionRejectsMissingConversationMetadata(t *testing.T) {
	callback := builderSubmissionCallback(string(domain.AgentKindLLM), "openai/fast")
	callback.Team.ID = "T12345678"
	callback.User.ID = "U12345678"
	defs := &agentdef.Definitions{Providers: map[string]agentdef.Provider{
		"openai": {
			Name: "openai", Type: agentdef.ProviderTypeOpenAICompatible,
			BaseURL: "https://model.example/v1", APIKeyEnv: "MODEL_KEY",
			Profiles: map[string]agentdef.Profile{"fast": {Model: "test-model"}},
		},
	}}
	handler := NewBuilderSubmissionHandler(nil, agentbuilder.New(), defs, nil)
	response := handler.HandleSubmission(t.Context(), callback)
	if response == nil || response.ResponseAction != slackapi.RAErrors {
		t.Fatalf("response = %#v, want actionable metadata error", response)
	}
	if !strings.Contains(response.Errors["name"], "cierra") {
		t.Fatalf("metadata error = %#v", response.Errors)
	}
}

type builderDraftStoreFake struct {
	draft       *port.AgentDraft
	created     *port.AgentDraft
	statusCalls int
}

func (s *builderDraftStoreFake) Create(_ context.Context, draft *port.AgentDraft) error {
	s.created = draft
	return nil
}
func (s *builderDraftStoreFake) Get(context.Context, string) (*port.AgentDraft, error) {
	return s.draft, nil
}
func (*builderDraftStoreFake) FindByNameAndDefinitionHash(context.Context, string, string) (*port.AgentDraft, error) {
	return nil, nil
}
func (*builderDraftStoreFake) MarkPreviewed(context.Context, string, string, int) error { return nil }
func (s *builderDraftStoreFake) UpdateStatus(context.Context, string, port.AgentDraftStatus, port.AgentDraftStatus) error {
	s.statusCalls++
	return nil
}
func (*builderDraftStoreFake) ExpireDrafts(context.Context, time.Time) error { return nil }

type builderPublisherFake struct{ targets []domain.ReplyTarget }

func (p *builderPublisherFake) Publish(_ context.Context, target domain.ReplyTarget, _ string) (port.PublishedResponse, error) {
	p.targets = append(p.targets, target)
	return port.PublishedResponse{}, nil
}

func TestBuilderSubmissionUsesMetadataTargetWhenCallbackOmitsLocation(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:channel:C12345678:thread:1785451523.453999")
	metadata, err := encodeBuilderInteractionContext("U12345678", key)
	if err != nil {
		t.Fatal(err)
	}
	callback := builderSubmissionCallback(string(domain.AgentKindLLM), "openai/fast")
	callback.Team.ID = "T12345678"
	callback.User.ID = "U12345678"
	callback.View.PrivateMetadata = metadata
	store := &builderDraftStoreFake{}
	publisher := &builderPublisherFake{}
	handler := NewBuilderSubmissionHandler(store, agentbuilder.New(), validBuilderDefinitions(), publisher)
	if response := handler.HandleSubmission(t.Context(), callback); response != nil {
		t.Fatalf("HandleSubmission() = %#v", response)
	}
	if err := handler.PreviewAndPublish(t.Context(), callback); err != nil {
		t.Fatal(err)
	}
	if store.created == nil || store.created.ConversationKey != string(key) {
		t.Fatalf("created draft = %#v", store.created)
	}
	if len(publisher.targets) != 1 || publisher.targets[0].ChannelID != "C12345678" || publisher.targets[0].ThreadTS != "1785451523.453999" {
		t.Fatalf("published targets = %#v", publisher.targets)
	}
}

func TestBuilderInstallUsesDraftConversationWithoutCallbackLocation(t *testing.T) {
	key := "slack:T12345678:channel:C12345678:thread:1785451523.453999"
	store := &builderDraftStoreFake{draft: &port.AgentDraft{
		DraftID: "draft-1", TeamID: "T12345678", ActorID: "U12345678", ConversationKey: key,
		Name: "builder_worker", DefinitionHash: strings.Repeat("a", 64), Status: port.DraftStatusPreviewed,
		ExpiresAt: time.Now().Add(time.Hour),
	}}
	publisher := &builderPublisherFake{}
	handler := NewBuilderSubmissionHandler(store, agentbuilder.New(), validBuilderDefinitions(), publisher)
	callback := slackapi.InteractionCallback{Team: slackapi.Team{ID: "T12345678"}, User: slackapi.User{ID: "U12345678"}}

	if err := handler.HandleInstallRequest(t.Context(), callback, "draft-1"); err != nil {
		t.Fatal(err)
	}
	if store.statusCalls != 1 || len(publisher.targets) != 1 || publisher.targets[0].ChannelID != "C12345678" || publisher.targets[0].ThreadTS != "1785451523.453999" {
		t.Fatalf("status calls = %d, targets = %#v", store.statusCalls, publisher.targets)
	}

	callback.Container.ChannelID = "C87654321"
	if err := handler.HandleInstallRequest(t.Context(), callback, "draft-1"); err == nil {
		t.Fatal("mismatched callback location was accepted")
	}
}

func validBuilderDefinitions() *agentdef.Definitions {
	return &agentdef.Definitions{Providers: map[string]agentdef.Provider{
		"openai": {
			Name: "openai", Type: agentdef.ProviderTypeOpenAICompatible,
			BaseURL: "https://model.example/v1", APIKeyEnv: "MODEL_KEY",
			Profiles: map[string]agentdef.Profile{"fast": {Model: "test-model"}},
		},
	}}
}
