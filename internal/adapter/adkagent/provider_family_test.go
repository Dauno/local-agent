package adkagent

import (
	"errors"
	"testing"

	"google.golang.org/adk/v2/session"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func runOneTurn(t *testing.T, runtime *Runtime, key domain.ConversationKey) error {
	t.Helper()
	_, err := runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey: key,
		Messages:        []domain.Message{{Role: domain.RoleUser, Content: "hello", UserID: "U12345678"}},
	})
	return err
}

func TestProviderFamilyMarkerBlocksCrossFamilyReuse(t *testing.T) {
	t.Parallel()

	service := session.InMemoryService()
	llm := &fakeLLM{}

	openaiRuntime, err := NewRuntime(RuntimeConfig{
		AgentName: "Dev Agent", Model: llm, SessionService: service,
		ProviderFamily: domain.ProviderFamilyOpenAICompatible,
	})
	if err != nil {
		t.Fatal(err)
	}
	cliRuntime, err := NewRuntime(RuntimeConfig{
		AgentName: "Dev Agent", Model: llm, SessionService: service,
		ProviderFamily: domain.ProviderFamilyAgentCLI,
	})
	if err != nil {
		t.Fatal(err)
	}

	const key = domain.ConversationKey("slack:T12345678:dm:D33333333")
	if err := runOneTurn(t, openaiRuntime, key); err != nil {
		t.Fatalf("openai turn failed: %v", err)
	}
	err = runOneTurn(t, cliRuntime, key)
	if !errors.Is(err, ErrProviderFamilyMismatch) {
		t.Fatalf("expected provider family mismatch, got %v", err)
	}

	const cliKey = domain.ConversationKey("slack:T12345678:dm:D44444444")
	if err := runOneTurn(t, cliRuntime, cliKey); err != nil {
		t.Fatalf("agent_cli turn on fresh session failed: %v", err)
	}
	if err := runOneTurn(t, cliRuntime, cliKey); err != nil {
		t.Fatalf("agent_cli turn on same-family session failed: %v", err)
	}
	err = runOneTurn(t, openaiRuntime, cliKey)
	if !errors.Is(err, ErrProviderFamilyMismatch) {
		t.Fatalf("expected mismatch for openai on agent_cli session, got %v", err)
	}
}

func TestProviderFamilyDefaultsToOpenAICompatible(t *testing.T) {
	t.Parallel()

	service := session.InMemoryService()
	llm := &fakeLLM{}
	defaulted, err := NewRuntime(RuntimeConfig{
		AgentName: "Dev Agent", Model: llm, SessionService: service,
	})
	if err != nil {
		t.Fatal(err)
	}
	const key = domain.ConversationKey("slack:T12345678:dm:D55555555")
	if err := runOneTurn(t, defaulted, key); err != nil {
		t.Fatalf("default family turn failed: %v", err)
	}
	if err := runOneTurn(t, defaulted, key); err != nil {
		t.Fatalf("second default family turn failed: %v", err)
	}
}
