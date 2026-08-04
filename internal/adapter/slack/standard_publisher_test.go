package slack

import (
	"context"
	"errors"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type fakeStandardMessageClient struct {
	postedText     string
	postedMetadata slackapi.SlackMetadata
	updatedText    string
	updatedMeta    slackapi.SlackMetadata
	messages       []slackapi.Message
	hasMore        bool
}

func (c *fakeStandardMessageClient) PostStandard(_ context.Context, _, _, markdown string, metadata slackapi.SlackMetadata) (string, error) {
	c.postedText, c.postedMetadata = markdown, metadata
	return "1700000001.000001", nil
}

func (c *fakeStandardMessageClient) UpdateStandard(_ context.Context, _, _, markdown string, metadata slackapi.SlackMetadata) error {
	c.updatedText, c.updatedMeta = markdown, metadata
	return nil
}

func (c *fakeStandardMessageClient) StandardMessages(context.Context, string, string, int) ([]slackapi.Message, bool, error) {
	return c.messages, c.hasMore, nil
}

func TestStandardPublisherUsesApplicationOwnedProgressLabels(t *testing.T) {
	client := &fakeStandardMessageClient{}
	publisher := &StandardPublisher{client: client, botUserID: "U00000001"}
	operation := domain.ProgressOperation{ID: "progress-1", ChannelID: "D00000001", ThreadTS: "1700000000.000001", State: domain.ProgressWorking}
	target := domain.ReplyTarget{ChannelID: operation.ChannelID, ThreadTS: operation.ThreadTS}

	published, err := publisher.PublishProgress(t.Context(), target, operation)
	if err != nil {
		t.Fatal(err)
	}
	if published.LastMessageTS == "" || client.postedText != "Working" || client.postedMetadata.EventType != progressMetadataEventType {
		t.Fatalf("published=%#v text=%q metadata=%#v", published, client.postedText, client.postedMetadata)
	}
	operation.MessageTS = published.LastMessageTS
	operation.State = domain.ProgressFinalizing
	if err := publisher.UpdateProgress(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	if client.updatedText != "Finalizing" || client.updatedMeta.EventPayload["state"] != string(domain.ProgressFinalizing) {
		t.Fatalf("updated text=%q metadata=%#v", client.updatedText, client.updatedMeta)
	}
}

func TestStandardPublisherUsesConfiguredProgressLabelsWithFallback(t *testing.T) {
	client := &fakeStandardMessageClient{}
	publisher := &StandardPublisher{
		client:    client,
		botUserID: "U00000001",
		progressLabels: ResolveProgressLabels(map[domain.ProgressState]string{
			domain.ProgressWorking: "Pensando",
			domain.ProgressCleared: "Listo",
		}),
	}
	operation := domain.ProgressOperation{ID: "progress-1", ChannelID: "D00000001", ThreadTS: "1700000000.000001", State: domain.ProgressWorking}
	target := domain.ReplyTarget{ChannelID: operation.ChannelID, ThreadTS: operation.ThreadTS}
	published, err := publisher.PublishProgress(t.Context(), target, operation)
	if err != nil {
		t.Fatal(err)
	}
	if client.postedText != "Pensando" {
		t.Fatalf("configured working label text=%q", client.postedText)
	}
	for state, want := range map[domain.ProgressState]string{
		domain.ProgressWaitingConfirmation: "Waiting for approval",
		domain.ProgressFinalizing:          "Finalizing",
		domain.ProgressCleared:             "Listo",
		domain.ProgressFailed:              "Interrupted",
		domain.ProgressInterrupted:         "Interrupted",
	} {
		operation.MessageTS = published.LastMessageTS
		operation.State = state
		if err := publisher.UpdateProgress(t.Context(), operation); err != nil {
			t.Fatal(err)
		}
		if client.updatedText != want {
			t.Fatalf("state %q label=%q, want %q", state, client.updatedText, want)
		}
	}
}

func TestResolveProgressLabelsOverlaysDefaults(t *testing.T) {
	resolved := ResolveProgressLabels(nil)
	if len(resolved) != 6 || resolved[domain.ProgressWorking] != "Working" || resolved[domain.ProgressInterrupted] != "Interrupted" {
		t.Fatalf("nil resolution = %#v", resolved)
	}
	resolved = ResolveProgressLabels(map[domain.ProgressState]string{
		domain.ProgressWorking: "Pensando",
		domain.ProgressFailed:  "Interrumpido",
	})
	if resolved[domain.ProgressWorking] != "Pensando" || resolved[domain.ProgressFailed] != "Interrumpido" || resolved[domain.ProgressCleared] != "Completed" {
		t.Fatalf("overlay resolution = %#v", resolved)
	}
	resolved = ResolveProgressLabels(map[domain.ProgressState]string{
		domain.ProgressWorking: "   ",
	})
	if resolved[domain.ProgressWorking] != "Working" {
		t.Fatalf("empty configured label should keep the default: %#v", resolved)
	}
}

func TestStandardPublisherRecoversProgressByExactMetadata(t *testing.T) {
	operation := domain.ProgressOperation{ID: "progress-1", ChannelID: "D00000001", ThreadTS: "1700000000.000001", State: domain.ProgressWorking}
	client := &fakeStandardMessageClient{messages: []slackapi.Message{{Msg: slackapi.Msg{
		User: "U00000001", Timestamp: "1700000001.000001", Metadata: progressMetadata(operation),
	}}}}
	publisher := &StandardPublisher{client: client, botUserID: "U00000001"}

	published, found, err := publisher.RecoverProgress(t.Context(), operation)
	if err != nil || !found || published.LastMessageTS != "1700000001.000001" {
		t.Fatalf("published=%#v found=%v err=%v", published, found, err)
	}
	client.messages = append(client.messages, client.messages[0])
	if _, _, err := publisher.RecoverProgress(t.Context(), operation); err == nil {
		t.Fatal("duplicate progress metadata was accepted")
	}
}

func TestSuggestedPromptsNeutralizeSlackControls(t *testing.T) {
	client := &fakeStandardMessageClient{}
	publisher := &StandardPublisher{client: client, botUserID: "U00000001"}
	_, err := publisher.PublishSuggestedPrompts(t.Context(), domain.ReplyTarget{
		ChannelID: "D00000001", ThreadTS: "1700000000.000001",
	}, "prompts-1", []string{"Ask <@U99999999> for help"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(client.postedText, "<@U99999999>") || !strings.Contains(client.postedText, "&lt;@U99999999>") {
		t.Fatalf("unsafe prompt rendering: %q", client.postedText)
	}
	if client.postedMetadata.EventType != promptMetadataEventType {
		t.Fatalf("metadata=%#v", client.postedMetadata)
	}
}

type fakeOnboardingBlockClient struct {
	fakeStandardMessageClient
	postedBlocks []slackapi.Block
	postedTarget domain.ReplyTarget
	postedMeta   slackapi.SlackMetadata
}

func (c *fakeOnboardingBlockClient) PostBlocks(_ context.Context, channelID, _ string, blocks []slackapi.Block, metadata slackapi.SlackMetadata, threadTS string) (string, error) {
	c.postedBlocks = blocks
	c.postedTarget = domain.ReplyTarget{ChannelID: channelID, ThreadTS: threadTS}
	c.postedMeta = metadata
	return "1700000001.000010", nil
}

func TestOnboardingPublisherUsesTypedTemplateAndRecoveryMetadata(t *testing.T) {
	client := &fakeOnboardingBlockClient{}
	renderer := mustEmbeddedRenderer(t)
	publisher := &StandardPublisher{client: client, blockClient: client, botUserID: "U00000001", renderer: renderer}
	key := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1700000000.000001")
	request := port.OnboardingPublishRequest{
		DeliveryID: "standard_onboarding:T12345678:U00000001", Actor: "U00000001", ConversationKey: key,
		SuggestedPrompts: []string{"Analiza <@U99999999>"},
	}
	target := domain.ReplyTarget{ChannelID: "D00000001", ThreadTS: "1700000000.000001"}
	if _, err := publisher.PublishOnboarding(t.Context(), target, request); err != nil {
		t.Fatal(err)
	}
	if len(client.postedBlocks) != 4 || client.postedTarget != target || client.postedMeta.EventType != onboardingMetadataEventType {
		t.Fatalf("blocks=%d target=%#v metadata=%#v", len(client.postedBlocks), client.postedTarget, client.postedMeta)
	}
	if client.postedMeta.EventPayload["delivery_id"] != request.DeliveryID {
		t.Fatalf("metadata=%#v", client.postedMeta)
	}
	promptBlock := client.postedBlocks[3].(*slackapi.SectionBlock)
	if strings.Contains(promptBlock.Text.Text, "<@U99999999>") {
		t.Fatalf("unsafe onboarding prompt = %q", promptBlock.Text.Text)
	}

	client.messages = []slackapi.Message{{Msg: slackapi.Msg{User: "U00000001", Timestamp: "1700000001.000010", Metadata: client.postedMeta}}}
	recovered, found, err := publisher.RecoverOnboarding(t.Context(), target, request.DeliveryID)
	if err != nil || !found || recovered.LastMessageTS != "1700000001.000010" {
		t.Fatalf("recovered=%#v found=%v err=%v", recovered, found, err)
	}
	client.messages = append(client.messages, client.messages[0])
	if _, _, err := publisher.RecoverOnboarding(t.Context(), target, request.DeliveryID); err == nil {
		t.Fatal("duplicate onboarding metadata was accepted")
	}
}

func TestIncrementalPublisherEnforcesObservedLimitAndCanonicalFinalMetadata(t *testing.T) {
	client := &fakeStandardMessageClient{}
	publisher := &StandardPublisher{client: client, botUserID: "U00000001"}
	operation := domain.IncrementalOperation{
		ID: "incremental-1", ChannelID: "D00000001", ThreadTS: "1700000000.000001",
		MessageTS: "1700000001.000001", RendererVersion: standardIncrementalRenderer, Sequence: 2, PrefixDigest: "digest",
	}
	oversized := strings.Repeat("界", SlackMarkdownChunkRunes+1)
	if err := publisher.ValidateIncrementalText(oversized); !errors.Is(err, port.ErrIncrementalTextTooLong) {
		t.Fatalf("oversized incremental validation error=%v", err)
	}
	if err := publisher.UpdateIncremental(t.Context(), operation, oversized); !errors.Is(err, port.ErrIncrementalTextTooLong) {
		t.Fatalf("oversized incremental update error=%v", err)
	}
	if err := publisher.FinalizeIncremental(t.Context(), operation, "final answer", "assistant-correlation"); err != nil {
		t.Fatal(err)
	}
	if client.updatedMeta.EventType != assistantMetadataEventType || client.updatedMeta.EventPayload["correlation_id"] != "assistant-correlation" || client.updatedMeta.EventPayload["part_count"] != 1 {
		t.Fatalf("final metadata=%#v", client.updatedMeta)
	}
}
