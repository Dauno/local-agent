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

func TestStandardPublisherNeutralizesProgressLabelControls(t *testing.T) {
	client := &fakeStandardMessageClient{}
	publisher := &StandardPublisher{
		client:    client,
		botUserID: "U00000001",
		progressLabels: ResolveProgressLabels(map[domain.ProgressState]string{
			domain.ProgressWorking:    "Trabajando <@U12345678> y <!here>",
			domain.ProgressFinalizing: "Fin <!subteam^S12345678> en <#C12345678>",
		}),
	}
	operation := domain.ProgressOperation{ID: "progress-1", ChannelID: "D00000001", ThreadTS: "1700000000.000001", State: domain.ProgressWorking}
	target := domain.ReplyTarget{ChannelID: operation.ChannelID, ThreadTS: operation.ThreadTS}
	published, err := publisher.PublishProgress(t.Context(), target, operation)
	if err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{"<@U12345678>", "<!here>"} {
		if strings.Contains(client.postedText, unsafe) {
			t.Fatalf("unsafe progress label rendering: %q", client.postedText)
		}
	}
	if !strings.Contains(client.postedText, "&lt;@U12345678>") || !strings.Contains(client.postedText, "&lt;!here>") {
		t.Fatalf("progress label not neutralized: %q", client.postedText)
	}
	operation.MessageTS = published.LastMessageTS
	operation.State = domain.ProgressFinalizing
	if err := publisher.UpdateProgress(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{"<!subteam^S12345678>", "<#C12345678>"} {
		if strings.Contains(client.updatedText, unsafe) {
			t.Fatalf("unsafe progress label update: %q", client.updatedText)
		}
	}
	if !strings.Contains(client.updatedText, "&lt;!subteam^S12345678>") || !strings.Contains(client.updatedText, "&lt;#C12345678>") {
		t.Fatalf("progress label update not neutralized: %q", client.updatedText)
	}
}

func TestStandardPublisherRejectsOversizedProgressLabels(t *testing.T) {
	for name, label := range map[string]string{
		"ascii":     strings.Repeat("a", domain.ProgressLabelMaxRunes+1),
		"multibyte": strings.Repeat("界", domain.ProgressLabelMaxRunes+1),
	} {
		t.Run(name, func(t *testing.T) {
			client := &fakeStandardMessageClient{}
			publisher := &StandardPublisher{
				client:    client,
				botUserID: "U00000001",
				progressLabels: ResolveProgressLabels(map[domain.ProgressState]string{
					domain.ProgressWorking: label,
				}),
			}
			operation := domain.ProgressOperation{ID: "progress-1", ChannelID: "D00000001", ThreadTS: "1700000000.000001", State: domain.ProgressWorking}
			target := domain.ReplyTarget{ChannelID: operation.ChannelID, ThreadTS: operation.ThreadTS}
			if _, err := publisher.PublishProgress(t.Context(), target, operation); err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("PublishProgress error = %v, want length error", err)
			}
			if client.postedText != "" {
				t.Fatalf("oversized label was posted: %q", client.postedText)
			}
			operation.MessageTS = "1700000001.000001"
			if err := publisher.UpdateProgress(t.Context(), operation); err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("UpdateProgress error = %v, want length error", err)
			}
			if client.updatedText != "" {
				t.Fatalf("oversized label was updated: %q", client.updatedText)
			}
		})
	}
}

func TestStandardPublisherAcceptsMultibyteProgressLabelAtLimit(t *testing.T) {
	client := &fakeStandardMessageClient{}
	publisher := &StandardPublisher{
		client:    client,
		botUserID: "U00000001",
		progressLabels: ResolveProgressLabels(map[domain.ProgressState]string{
			domain.ProgressWorking: strings.Repeat("界", domain.ProgressLabelMaxRunes),
		}),
	}
	operation := domain.ProgressOperation{ID: "progress-1", ChannelID: "D00000001", ThreadTS: "1700000000.000001", State: domain.ProgressWorking}
	target := domain.ReplyTarget{ChannelID: operation.ChannelID, ThreadTS: operation.ThreadTS}
	if _, err := publisher.PublishProgress(t.Context(), target, operation); err != nil {
		t.Fatalf("label at the code point limit should publish, got %v", err)
	}
	if runes := len([]rune(client.postedText)); runes != domain.ProgressLabelMaxRunes {
		t.Fatalf("posted runes = %d, want %d", runes, domain.ProgressLabelMaxRunes)
	}
}

// TestStandardPublisherRejectsProgressLabelOversizedOnlyAfterNeutralization
// pins the "neutralize first, then measure" ordering: a raw label at or under
// ProgressLabelMaxRunes can still exceed the limit after slackControlPattern
// expands "<" into "&lt;". A regression that measured the raw label before
// neutralization would let this label through, so the error must be produced
// and the Slack client must never be invoked.
func TestStandardPublisherRejectsProgressLabelOversizedOnlyAfterNeutralization(t *testing.T) {
	for name, label := range map[string]string{
		// 11900-14 "a" runes + 12 runes for "<@U12345678>" = 11900-2 runes raw
		// (within limit), but neutralization grows "<" to "&lt;" so the result
		// is 11900-14+15 = 11901 runes (over the limit).
		"mention": strings.Repeat("a", domain.ProgressLabelMaxRunes-14) + "<@U12345678>",
		// Same shape with a channel-reference control.
		"channel": strings.Repeat("a", domain.ProgressLabelMaxRunes-14) + "<#C12345678>",
	} {
		t.Run(name, func(t *testing.T) {
			if raw := len([]rune(label)); raw > domain.ProgressLabelMaxRunes {
				t.Fatalf("premise broken: raw label has %d runes, want <= %d", raw, domain.ProgressLabelMaxRunes)
			}
			if neutralized := len([]rune(neutralizeUnsafeControls(label))); neutralized <= domain.ProgressLabelMaxRunes {
				t.Fatalf("premise broken: neutralized label has %d runes, want > %d", neutralized, domain.ProgressLabelMaxRunes)
			}
			client := &fakeStandardMessageClient{}
			publisher := &StandardPublisher{
				client:    client,
				botUserID: "U00000001",
				progressLabels: ResolveProgressLabels(map[domain.ProgressState]string{
					domain.ProgressWorking: label,
				}),
			}
			operation := domain.ProgressOperation{ID: "progress-1", ChannelID: "D00000001", ThreadTS: "1700000000.000001", State: domain.ProgressWorking}
			target := domain.ReplyTarget{ChannelID: operation.ChannelID, ThreadTS: operation.ThreadTS}
			if _, err := publisher.PublishProgress(t.Context(), target, operation); err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("PublishProgress error = %v, want length error", err)
			}
			if client.postedText != "" {
				t.Fatalf("post-neutralization oversized label was posted: %q", client.postedText)
			}
			operation.MessageTS = "1700000001.000001"
			if err := publisher.UpdateProgress(t.Context(), operation); err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("UpdateProgress error = %v, want length error", err)
			}
			if client.updatedText != "" {
				t.Fatalf("post-neutralization oversized label was updated: %q", client.updatedText)
			}
		})
	}
}

// TestStandardPublisherAcceptsProgressLabelAtLimitAfterNeutralization pins the
// boundary of the "neutralize first, then measure" ordering: a raw label that
// neutralizes to exactly ProgressLabelMaxRunes runes is accepted and published.
func TestStandardPublisherAcceptsProgressLabelAtLimitAfterNeutralization(t *testing.T) {
	// 11900-15 "a" runes + 12 runes for "<@U12345678>" = 11900-3 runes raw,
	// neutralized to exactly 11900 runes (the maximum allowed).
	label := strings.Repeat("a", domain.ProgressLabelMaxRunes-15) + "<@U12345678>"
	if neutralized := len([]rune(neutralizeUnsafeControls(label))); neutralized != domain.ProgressLabelMaxRunes {
		t.Fatalf("premise broken: neutralized label has %d runes, want exactly %d", neutralized, domain.ProgressLabelMaxRunes)
	}
	client := &fakeStandardMessageClient{}
	publisher := &StandardPublisher{
		client:    client,
		botUserID: "U00000001",
		progressLabels: ResolveProgressLabels(map[domain.ProgressState]string{
			domain.ProgressWorking: label,
		}),
	}
	operation := domain.ProgressOperation{ID: "progress-1", ChannelID: "D00000001", ThreadTS: "1700000000.000001", State: domain.ProgressWorking}
	target := domain.ReplyTarget{ChannelID: operation.ChannelID, ThreadTS: operation.ThreadTS}
	if _, err := publisher.PublishProgress(t.Context(), target, operation); err != nil {
		t.Fatalf("label at the post-neutralization limit should publish, got %v", err)
	}
	if runes := len([]rune(client.postedText)); runes != domain.ProgressLabelMaxRunes {
		t.Fatalf("posted runes = %d, want %d", runes, domain.ProgressLabelMaxRunes)
	}
}

func TestStandardPublisherRecoversProgressByExactMetadata(t *testing.T) {
	operation := domain.ProgressOperation{ID: "progress-1", ChannelID: "D00000001", ThreadTS: "1700000000.000001", State: domain.ProgressWorking}
	client := &fakeStandardMessageClient{messages: []slackapi.Message{{
		User: "U00000001", Timestamp: "1700000001.000001", Metadata: progressMetadata(operation),
	}}}
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

	client.messages = []slackapi.Message{{User: "U00000001", Timestamp: "1700000001.000010", Metadata: client.postedMeta}}
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
	if client.updatedMeta.EventType != assistantMetadataEventType || client.updatedMeta.EventPayload["correlation_id"] != "assistant-correlation" ||
		client.updatedMeta.EventPayload["part_count"] != 1 {
		t.Fatalf("final metadata=%#v", client.updatedMeta)
	}
}
