package adkagent

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type originContextLLM struct {
	turnContext port.AgentTurnContext
	contextOK   bool
	request     *model.LLMRequest
	calls       int
}

func (*originContextLLM) Name() string { return "origin-context-model" }

func (m *originContextLLM) GenerateContent(ctx context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.turnContext, m.contextOK = port.AgentTurnContextFromContext(ctx)
		m.request = request
		m.calls++
		yield(&model.LLMResponse{
			Content: genai.NewContentFromText("root synthesis", genai.RoleModel),
			CustomMetadata: map[string]any{
				port.AgentTurnOriginMetadataKey:       "model-spoof",
				port.AgentTurnActivationIDMetadataKey: "model-spoof",
				"model_metadata":                      "retained",
			},
			TurnComplete: true,
		}, nil)
	}
}

type recordingOriginToolFactory struct {
	actor string
	key   domain.ConversationKey
}

func (f *recordingOriginToolFactory) ToolsForInvocation(actor string, key domain.ConversationKey) ([]any, error) {
	f.actor = actor
	f.key = key
	return nil, nil
}

func (f *recordingOriginToolFactory) ToolsForActivation(actor string, key domain.ConversationKey, _ domain.ExternalAgentJobActivation) ([]any, error) {
	f.actor = actor
	f.key = key
	return nil, nil
}

func TestRuntimeUsesTypedJobOriginAndHostMetadata(t *testing.T) {
	llm := &originContextLLM{}
	factory := &recordingOriginToolFactory{}
	runtime, err := NewRuntime(RuntimeConfig{
		AgentName:      "Dev Agent",
		Model:          llm,
		SessionService: session.InMemoryService(),
		ToolFactory:    factory,
	})
	if err != nil {
		t.Fatal(err)
	}

	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	activationID := "activation_opaque_123"
	envelope := "External-agent completion notification. Job ID: `job-1`. Result bytes: `64`."
	turn, err := runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey: key,
		Origin: port.AgentTurnOrigin{
			Kind:         port.AgentTurnOriginJobCompletion,
			Actor:        "UORIGINAL1",
			ActivationID: activationID,
		},
		Messages: []domain.Message{{
			Role:       domain.RoleUser,
			Source:     domain.MessageSourceJobCompletion,
			Content:    envelope,
			UserID:     "USLACKACTOR",
			ExternalTS: "slack-message-ts",
		}},
		Activation: &domain.ExternalAgentJobActivation{ActivationID: activationID, Actor: "UORIGINAL1", ConversationKey: key},
	})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Text != "root synthesis" {
		t.Fatalf("turn = %#v, want root synthesis", turn)
	}
	if factory.actor != "UORIGINAL1" || factory.key != key {
		t.Fatalf("tool factory binding = actor %q, key %q; want original actor and key", factory.actor, factory.key)
	}
	if !llm.contextOK || llm.turnContext.Origin.Kind != port.AgentTurnOriginJobCompletion || llm.turnContext.Origin.Actor != "UORIGINAL1" || llm.turnContext.Origin.ActivationID != activationID {
		t.Fatalf("model context = %#v, present=%v", llm.turnContext, llm.contextOK)
	}
	if llm.turnContext.ConversationKey != key {
		t.Fatalf("model context conversation = %q, want %q", llm.turnContext.ConversationKey, key)
	}
	if len(llm.request.Contents) != 1 || llm.request.Contents[0].Parts[0].Text != envelope || strings.Contains(llm.request.Contents[0].Parts[0].Text, "terminal result bytes") {
		t.Fatalf("model envelope = %#v, want compact host envelope only", llm.request.Contents)
	}

	loaded, err := runtime.sessionService.Get(t.Context(), &session.GetRequest{
		AppName: applicationName, UserID: ephemeralUserID, SessionID: adkSessionID(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Session.Events().Len() != 2 {
		t.Fatalf("durable event count = %d, want input and model events", loaded.Session.Events().Len())
	}
	for index := 0; index < loaded.Session.Events().Len(); index++ {
		event := loaded.Session.Events().At(index)
		if event.CustomMetadata[port.AgentTurnOriginMetadataKey] != string(port.AgentTurnOriginJobCompletion) {
			t.Errorf("event %d origin metadata = %#v", index, event.CustomMetadata)
		}
		if event.CustomMetadata[port.AgentTurnActivationIDMetadataKey] != activationID {
			t.Errorf("event %d activation metadata = %#v", index, event.CustomMetadata)
		}
	}
	if loaded.Session.Events().At(1).CustomMetadata["model_metadata"] != "retained" {
		t.Fatal("non-reserved model metadata was discarded")
	}
}

func TestRuntimeRecoversDurableActivationFinalWithoutModelReplay(t *testing.T) {
	llm := &originContextLLM{}
	runtime, err := NewRuntime(RuntimeConfig{
		AgentName: "Dev Agent", Model: llm, SessionService: session.InMemoryService(),
	})
	if err != nil {
		t.Fatal(err)
	}
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	activationID := "activation_recover_123"
	_, err = runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey: key,
		Origin:          port.AgentTurnOrigin{Kind: port.AgentTurnOriginJobCompletion, Actor: "U12345678", ActivationID: activationID},
		Messages:        []domain.Message{{Role: domain.RoleUser, Source: domain.MessageSourceJobCompletion, Content: "compact envelope", UserID: "U12345678", ExternalTS: activationID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, found, err := runtime.RecoverActivation(t.Context(), key, activationID)
	if err != nil || !found || turn.Text != "root synthesis" {
		t.Fatalf("recovery = %#v, found=%t, err=%v", turn, found, err)
	}
	if llm.calls != 1 {
		t.Fatalf("model calls = %d, want one original call", llm.calls)
	}
}

func TestRuntimeReportsMissingActivationFinalAsUnrecoverable(t *testing.T) {
	runtime, err := NewRuntime(RuntimeConfig{
		AgentName: "Dev Agent", Model: &originContextLLM{}, SessionService: session.InMemoryService(),
	})
	if err != nil {
		t.Fatal(err)
	}
	foundTurn, found, err := runtime.RecoverActivation(t.Context(), "slack:T12345678:dm:D12345678", "activation_missing")
	if err != nil || found || foundTurn.Text != "" {
		t.Fatalf("missing recovery = %#v, found=%t, err=%v", foundTurn, found, err)
	}
}

func TestRuntimeInfersActivationOriginFromDurableCompletionMessage(t *testing.T) {
	llm := &originContextLLM{}
	runtime, err := NewRuntime(RuntimeConfig{
		AgentName: "Dev Agent", Model: llm, SessionService: session.InMemoryService(),
	})
	if err != nil {
		t.Fatal(err)
	}

	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	_, err = runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey: key,
		Messages: []domain.Message{{
			Role: domain.RoleUser, Source: domain.MessageSourceJobCompletion,
			Content: "compact host envelope", UserID: "UORIGINAL1", ExternalTS: "activation_from_message",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !llm.contextOK || llm.turnContext.Origin.Kind != port.AgentTurnOriginJobCompletion || llm.turnContext.Origin.ActivationID != "activation_from_message" {
		t.Fatalf("inferred origin = %#v, present=%v", llm.turnContext, llm.contextOK)
	}
}

func TestRuntimeNormalTurnKeepsUserOriginAndNoActivationID(t *testing.T) {
	llm := &originContextLLM{}
	runtime, err := NewRuntime(RuntimeConfig{
		AgentName: "Dev Agent", Model: llm, SessionService: session.InMemoryService(),
	})
	if err != nil {
		t.Fatal(err)
	}

	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	if _, err := runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey: key,
		Messages:        []domain.Message{{Role: domain.RoleUser, Content: "hello", UserID: "U12345678"}},
	}); err != nil {
		t.Fatal(err)
	}
	if !llm.contextOK || llm.turnContext.Origin.Kind != port.AgentTurnOriginUser || llm.turnContext.Origin.Actor != "U12345678" || llm.turnContext.Origin.ActivationID != "" {
		t.Fatalf("normal origin = %#v, present=%v", llm.turnContext, llm.contextOK)
	}
	loaded, err := runtime.sessionService.Get(t.Context(), &session.GetRequest{
		AppName: applicationName, UserID: ephemeralUserID, SessionID: adkSessionID(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < loaded.Session.Events().Len(); index++ {
		metadata := loaded.Session.Events().At(index).CustomMetadata
		if metadata[port.AgentTurnOriginMetadataKey] != string(port.AgentTurnOriginUser) {
			t.Errorf("event %d user origin metadata = %#v", index, metadata)
		}
		if _, ok := metadata[port.AgentTurnActivationIDMetadataKey]; ok {
			t.Errorf("event %d unexpectedly carries activation metadata = %#v", index, metadata)
		}
	}
}

func TestRuntimeRejectsOversizedHostCompletionEnvelope(t *testing.T) {
	llm := &originContextLLM{}
	runtime, err := NewRuntime(RuntimeConfig{
		AgentName: "Dev Agent", Model: llm, SessionService: session.InMemoryService(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey: "slack:T12345678:dm:D12345678",
		Origin:          port.AgentTurnOrigin{Kind: port.AgentTurnOriginJobCompletion, Actor: "U12345678", ActivationID: "activation_123"},
		Messages: []domain.Message{{
			Role: domain.RoleUser, Source: domain.MessageSourceJobCompletion,
			Content: strings.Repeat("x", maxHostCompletionEnvelopeRunes+1),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "compact host limit") {
		t.Fatalf("oversized envelope error = %v", err)
	}
}
