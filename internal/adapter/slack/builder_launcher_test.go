package slack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/blockkit"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type builderLauncherBlockClient struct {
	channel   string
	fallback  string
	blocks    []slackapi.Block
	metadata  slackapi.SlackMetadata
	threadTS  string
	postCount int
}

func (c *builderLauncherBlockClient) PostBlocks(_ context.Context, channelID, fallbackText string, blocks []slackapi.Block, metadata slackapi.SlackMetadata, threadTS string) (string, error) {
	c.channel = channelID
	c.fallback = fallbackText
	c.blocks = blocks
	c.metadata = metadata
	c.threadTS = threadTS
	c.postCount++
	return "1720000000.000001", nil
}

func TestBuilderLauncherUsesOnboardingTemplateAndPreservesContract(t *testing.T) {
	client := &builderLauncherBlockClient{}
	publisher := newBuilderLauncherPublisher(client, nil, nil)
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	req := port.BuilderLauncherRequest{
		Actor:           "U12345678",
		ConversationKey: key,
		IdempotencyKey:  "launcher-1",
	}

	if err := publisher.PublishBuilderLauncher(t.Context(), req); err != nil {
		t.Fatalf("PublishBuilderLauncher() error = %v", err)
	}
	metadata, err := encodeBuilderInteractionContext(req.Actor, key)
	if err != nil {
		t.Fatal(err)
	}
	if client.channel != "D12345678" || client.threadTS != "" || client.postCount != 1 {
		t.Fatalf("post target/count = %q/%q/%d", client.channel, client.threadTS, client.postCount)
	}
	if client.fallback != "Local Agent puede analizar proyectos, revisar errores, resumir contexto y ayudarte a crear agentes. Puedes abrir el formulario o describir una necesidad." {
		t.Fatalf("fallback = %q", client.fallback)
	}
	message := blockkit.Message{FallbackText: client.fallback, Blocks: client.blocks}
	for _, value := range []string{onboardingIntroText, onboardingDescribePrompt, metadata} {
		if !blockkit.Reachable(message, value) {
			t.Fatalf("onboarding value %q did not reach the view", value)
		}
	}
	encoded, err := json.Marshal(client.blocks)
	if err != nil {
		t.Fatal(err)
	}
	for _, actionID := range []string{"local_agent.builder.open", "local_agent.onboarding.describe"} {
		if !strings.Contains(string(encoded), actionID) {
			t.Fatalf("onboarding action %q is missing from %s", actionID, encoded)
		}
	}
	if client.metadata.EventType != "" || len(client.metadata.EventPayload) != 0 {
		t.Fatalf("unexpected launcher metadata = %#v", client.metadata)
	}
}

func TestOnboardingTemplateCompilationIsDeterministic(t *testing.T) {
	engine, err := newOnboardingEngine()
	if err != nil {
		t.Fatal(err)
	}
	view := onboardingWelcomeView{
		BuilderContext: `{"v":1,"actor_id":"U12345678","conversation_key":"slack:T12345678:dm:D12345678"}`,
		Intro:          onboardingIntroText, DescribePrompt: onboardingDescribePrompt,
	}
	first, err := engine.Message(view)
	if err != nil {
		t.Fatalf("first compilation error = %v", err)
	}
	second, err := engine.Message(view)
	if err != nil {
		t.Fatalf("second compilation error = %v", err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("identical onboarding contexts produced different payloads\nfirst: %s\nsecond: %s", firstJSON, secondJSON)
	}
}
