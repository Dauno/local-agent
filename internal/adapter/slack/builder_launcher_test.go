package slack

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	slackapi "github.com/slack-go/slack"
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
	if len(client.blocks) != 3 {
		t.Fatalf("block count = %d, want 3", len(client.blocks))
	}
	section, ok := client.blocks[0].(*slackapi.SectionBlock)
	if !ok || section.Text == nil || section.Text.Type != slackapi.MarkdownType || section.Text.Text != onboardingIntroText {
		t.Fatalf("welcome section = %#v", client.blocks[0])
	}
	actions, ok := client.blocks[1].(*slackapi.ActionBlock)
	if !ok || actions.BlockID != "onboarding_actions" || len(actions.Elements.ElementSet) != 2 {
		t.Fatalf("launcher actions = %#v", client.blocks[1])
	}
	button, ok := actions.Elements.ElementSet[0].(*slackapi.ButtonBlockElement)
	if !ok || button.ActionID != "local_agent.builder.open" || button.Value != metadata || button.Text == nil || button.Text.Text != "Abrir formulario" || button.Style != slackapi.StylePrimary || button.URL != "" {
		t.Fatalf("launcher button = %#v", actions.Elements.ElementSet[0])
	}
	describe, ok := actions.Elements.ElementSet[1].(*slackapi.ButtonBlockElement)
	if !ok || describe.ActionID != "local_agent.onboarding.describe" || describe.Value != metadata || describe.Text == nil || describe.Text.Text != onboardingDescribePrompt {
		t.Fatalf("describe button = %#v", actions.Elements.ElementSet[1])
	}
	if client.metadata.EventType != "" || len(client.metadata.EventPayload) != 0 {
		t.Fatalf("unexpected launcher metadata = %#v", client.metadata)
	}
}

func TestOnboardingTemplateCompilationIsDeterministic(t *testing.T) {
	renderer := mustEmbeddedRenderer(t)
	renderContext := TemplateContext{Values: map[string]string{
		"builder_context": `{"v":1,"actor_id":"U12345678","conversation_key":"slack:T12345678:dm:D12345678"}`,
		"intro":           onboardingIntroText, "describe_prompt": onboardingDescribePrompt,
	}}
	firstFallback, firstBlocks, err := renderer.CompileMessageWithFallback("onboarding_message", renderContext)
	if err != nil {
		t.Fatalf("first compilation error = %v", err)
	}
	secondFallback, secondBlocks, err := renderer.CompileMessageWithFallback("onboarding_message", renderContext)
	if err != nil {
		t.Fatalf("second compilation error = %v", err)
	}
	first, err := json.Marshal(struct {
		Fallback string
		Blocks   []slackapi.Block
	}{firstFallback, firstBlocks})
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(struct {
		Fallback string
		Blocks   []slackapi.Block
	}{secondFallback, secondBlocks})
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("identical onboarding contexts produced different payloads\nfirst: %s\nsecond: %s", first, second)
	}
}
