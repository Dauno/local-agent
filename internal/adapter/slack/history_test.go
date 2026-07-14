package slack

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestHistoryReaderUsesRepliesForChannelThreadAndMapsRoles(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{replies: []slackapi.Message{
		{Msg: slackapi.Msg{User: testUser, Text: "pregunta", Timestamp: "1720000000.000001"}},
		{Msg: slackapi.Msg{User: testBot, Text: "respuesta", Timestamp: "1720000001.000002", SubType: slackapi.MsgSubTypeBotMessage}},
		{Msg: slackapi.Msg{User: "U00000003", Text: "seguimiento", Timestamp: "1720000002.000003"}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil)
	invocation := validThreadInvocation()

	got, err := reader.RecentHistory(context.Background(), invocation, domain.ContextLimits{MaxMessages: 3, MaxChars: 100})
	if err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	if !got.BotParticipated {
		t.Fatal("BotParticipated = false")
	}
	if len(got.Messages) != 3 {
		t.Fatalf("message count = %d, want 3", len(got.Messages))
	}
	roles := []domain.Role{domain.RoleUser, domain.RoleAssistant, domain.RoleUser}
	for index, role := range roles {
		if got.Messages[index].Role != role {
			t.Fatalf("message %d role = %q, want %q", index, got.Messages[index].Role, role)
		}
	}
	if got.Messages[1].CreatedAt.IsZero() || got.Messages[1].CreatedAt.Nanosecond() != 2000 {
		t.Fatalf("assistant CreatedAt = %v", got.Messages[1].CreatedAt)
	}
	call := client.lastCall()
	if call.method != "replies" || call.channelID != testChannel || call.rootTS != testThread || call.latest != testTS || call.limit != 3 {
		t.Fatalf("history call = %#v", call)
	}
	if !call.hadDeadline {
		t.Fatal("history API call had no timeout deadline")
	}
}

func TestHistoryReaderUsesInvocationAsRootForNewChannelThread(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{}
	reader := newHistoryReader(client, testBot, time.Second, nil)
	invocation := validThreadInvocation()
	invocation.Trigger = domain.TriggerMention
	invocation.ThreadTS = ""

	if _, err := reader.RecentHistory(context.Background(), invocation, domain.ContextLimits{MaxMessages: 5, MaxChars: 100}); err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	if call := client.lastCall(); call.method != "replies" || call.rootTS != testTS {
		t.Fatalf("history call = %#v", call)
	}
}

func TestHistoryReaderUsesDMHistoryAndRestoresChronologicalOrder(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testUser, Text: "latest", Timestamp: "1720000003.000003"}},
		{Msg: slackapi.Msg{User: testBot, Text: "middle", Timestamp: "1720000002.000002"}},
		{Msg: slackapi.Msg{User: testUser, Text: "oldest", Timestamp: "1720000001.000001"}},
	}}
	reader := newHistoryReader(client, testBot, 0, nil)
	invocation := validDMInvocation()

	got, err := reader.RecentHistory(context.Background(), invocation, domain.ContextLimits{MaxMessages: 3, MaxChars: 100})
	if err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	want := []string{"oldest", "middle", "latest"}
	for index := range want {
		if got.Messages[index].Content != want[index] {
			t.Fatalf("message %d = %q, want %q", index, got.Messages[index].Content, want[index])
		}
	}
	call := client.lastCall()
	if call.method != "history" || call.channelID != testDM || call.latest != testTS || call.limit != 3 || call.hadDeadline {
		t.Fatalf("history call = %#v", call)
	}
}

func TestHistoryReaderEnforcesMessageAndCharacterLimitsDefensively(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{replies: []slackapi.Message{
		{Msg: slackapi.Msg{User: testUser, Text: "discard-old", Timestamp: "1.000001"}},
		{Msg: slackapi.Msg{User: testUser, Text: "discard-too", Timestamp: "2.000001"}},
		{Msg: slackapi.Msg{User: testBot, Text: "1234", Timestamp: "3.000001"}},
		{Msg: slackapi.Msg{User: testUser, Text: "abcdefgh", Timestamp: "4.000001"}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil)

	got, err := reader.RecentHistory(context.Background(), validThreadInvocation(), domain.ContextLimits{MaxMessages: 2, MaxChars: 7})
	if err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "abcdefg" {
		t.Fatalf("limited messages = %#v", got.Messages)
	}
	if !got.BotParticipated {
		t.Fatal("bot participation from bounded API messages was lost after character limiting")
	}
}

func TestHistoryReaderFiltersUnsupportedHistoryMessages(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{replies: []slackapi.Message{
		{Msg: slackapi.Msg{User: testUser, Text: "", Timestamp: "1.1"}},
		{Msg: slackapi.Msg{User: "", Text: "system", Timestamp: "1.2"}},
		{Msg: slackapi.Msg{User: testUser, Text: "edited", Timestamp: "1.3", Edited: &slackapi.Edited{}}},
		{Msg: slackapi.Msg{User: testUser, Text: "file", Timestamp: "1.4", Files: []slackapi.File{{ID: "F"}}}},
		{Msg: slackapi.Msg{User: testUser, Text: "join", Timestamp: "1.5", SubType: slackapi.MsgSubTypeChannelJoin}},
		{Msg: slackapi.Msg{User: testBot, Text: "bot", Timestamp: "1.6", SubType: slackapi.MsgSubTypeBotMessage}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil)

	got, err := reader.RecentHistory(context.Background(), validThreadInvocation(), domain.ContextLimits{MaxMessages: 10, MaxChars: 100})
	if err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != domain.RoleAssistant || got.Messages[0].Content != "bot" || !got.BotParticipated {
		t.Fatalf("filtered history = %#v", got)
	}
}

func TestHistoryReaderReturnsRedactedWrappedAPIErrors(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("request failed with xoxb-123456789-secret")
	client := &fakeHistoryClient{err: wantErr}
	reader := newHistoryReader(client, testBot, time.Second, nil)

	_, err := reader.RecentHistory(context.Background(), validDMInvocation(), domain.ContextLimits{MaxMessages: 3, MaxChars: 100})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RecentHistory() error = %v, want wrapped API error", err)
	}
	if strings.Contains(err.Error(), "123456789-secret") {
		t.Fatalf("RecentHistory() leaked token: %v", err)
	}
}

func exchangeMetadataFor(correlationID, renderMode string, partIndex, partCount int, digest string) slackapi.SlackMetadata {
	return slackapi.SlackMetadata{
		EventType: "local_agent.assistant_exchange",
		EventPayload: map[string]any{
			"correlation_id": correlationID,
			"render_mode":    renderMode,
			"part_index":     float64(partIndex),
			"part_count":     float64(partCount),
			"content_sha256": digest,
		},
	}
}

func TestHistoryReaderFindsPublishedAssistantExchangeByMetadataDigest(t *testing.T) {
	t.Parallel()
	chunks := SplitMarkdown("published reply", SlackMarkdownChunkRunes)
	digest := contentSHA256(chunks[0])
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Text: "older reply", Timestamp: "1719999999.000001"}},
		{Msg: slackapi.Msg{User: testBot, Text: "translated by slack", Timestamp: "1720000001.000002", Metadata: slackapi.SlackMetadata{
			EventType: "local_agent.assistant_exchange",
			EventPayload: map[string]any{
				"correlation_id": "intent-correlation",
				"render_mode":    "markdown_v1",
				"part_index":     float64(1),
				"part_count":     float64(1),
				"content_sha256": digest,
			},
		}}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil)
	intent := port.AssistantExchangeIntent{
		ID: "intent", ChannelID: testDM, ChannelKind: domain.ChannelDM,
		Content: "published reply", CorrelationID: "intent-correlation",
	}
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), intent)
	if err != nil || !found || timestamp != "1720000001.000002" {
		t.Fatalf("FindPublishedAssistantExchange() = %q, %t, %v", timestamp, found, err)
	}
	if call := client.lastCall(); call.method != "history" || call.latest != "" || call.limit != 100 || !call.hadDeadline {
		t.Fatalf("recovery history call = %#v", call)
	}
}

func TestHistoryReaderRejectsRecoveryWithWrongDigest(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Text: "some text", Timestamp: "1720000001.000002", Metadata: slackapi.SlackMetadata{
			EventType: "local_agent.assistant_exchange",
			EventPayload: map[string]any{
				"correlation_id": "intent-correlation",
				"render_mode":    "markdown_v1",
				"part_index":     float64(1),
				"part_count":     float64(1),
				"content_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
			},
		}}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: "published reply", CorrelationID: "intent-correlation",
	})
	if err != nil || found || timestamp != "" {
		t.Fatalf("FindPublishedAssistantExchange() = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderRecoveryUsesSameSafetyRenderingAsPublisher(t *testing.T) {
	t.Parallel()
	content := "Do not notify <@U12345678> or <!channel>."
	parts := renderMarkdownV1(content)
	client := &fakeHistoryClient{history: []slackapi.Message{{Msg: slackapi.Msg{
		User: testBot, Timestamp: "1720000001.000002",
		Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 1, contentSHA256(parts[0])),
	}}}}
	reader := newHistoryReader(client, testBot, time.Second, nil)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: content, CorrelationID: "intent-correlation",
	})
	if err != nil || !found || timestamp != "1720000001.000002" {
		t.Fatalf("FindPublishedAssistantExchange() = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderRejectsConflictingCandidateMetadata(t *testing.T) {
	t.Parallel()
	part := renderMarkdownV1("published reply")[0]
	valid := exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 1, contentSHA256(part))
	unknown := exchangeMetadataFor("intent-correlation", "markdown_v2", 1, 1, contentSHA256(part))
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000001.000001", Metadata: valid}},
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000002.000002", Metadata: unknown}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: "published reply", CorrelationID: "intent-correlation",
	})
	if err != nil || found || timestamp != "" {
		t.Fatalf("conflicting recovery = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderRejectsReorderedMultipartDelivery(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("x", SlackMarkdownChunkRunes+100)
	parts := renderMarkdownV1(content)
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000002.000002", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 2, contentSHA256(parts[0]))}},
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000001.000001", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 2, 2, contentSHA256(parts[1]))}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: content, CorrelationID: "intent-correlation",
	})
	if err != nil || found || timestamp != "" {
		t.Fatalf("reordered recovery = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderReturnsFinalMultipartTimestamp(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("x", SlackMarkdownChunkRunes+100)
	parts := renderMarkdownV1(content)
	client := &fakeHistoryClient{history: []slackapi.Message{
		// conversations.history is newest first; metadata indices remain authoritative.
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000002.000002", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 2, 2, contentSHA256(parts[1]))}},
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000001.000001", Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 2, contentSHA256(parts[0]))}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: content, CorrelationID: "intent-correlation",
	})
	if err != nil || !found || timestamp != "1720000002.000002" {
		t.Fatalf("multipart recovery = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderRejectsDuplicatePart(t *testing.T) {
	t.Parallel()
	part := renderMarkdownV1("published reply")[0]
	metadata := exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 1, contentSHA256(part))
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000001.000001", Metadata: metadata}},
		{Msg: slackapi.Msg{User: testBot, Timestamp: "1720000002.000002", Metadata: metadata}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil)
	_, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: "published reply", CorrelationID: "intent-correlation",
	})
	if err != nil || found {
		t.Fatalf("duplicate recovery found = %t, err = %v", found, err)
	}
}

func TestHistoryReaderRejectsEditedMatchingCandidate(t *testing.T) {
	t.Parallel()
	part := renderMarkdownV1("published reply")[0]
	client := &fakeHistoryClient{history: []slackapi.Message{{Msg: slackapi.Msg{
		User: testBot, Timestamp: "1720000001.000001", Edited: &slackapi.Edited{},
		Metadata: exchangeMetadataFor("intent-correlation", markdownRenderMode, 1, 1, contentSHA256(part)),
	}}}}
	reader := newHistoryReader(client, testBot, time.Second, nil)
	_, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: "published reply", CorrelationID: "intent-correlation",
	})
	if err != nil || found {
		t.Fatalf("edited recovery found = %t, err = %v", found, err)
	}
}

func TestHistoryReaderRejectsRecoveryWithoutMetadata(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Text: "published reply", Timestamp: "1720000001.000002"}},
	}}
	reader := newHistoryReader(client, testBot, 0, nil)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: "published reply", CorrelationID: "intent-correlation",
	})
	if err != nil || found || timestamp != "" {
		t.Fatalf("ambiguous FindPublishedAssistantExchange() = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderRequiresCorrelationOnEveryMultipartChunk(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("x", SlackMarkdownChunkRunes+100)
	chunks := SplitMarkdown(content, SlackMarkdownChunkRunes)
	if len(chunks) != 2 {
		t.Fatalf("SplitMarkdown returned %d chunks, want 2", len(chunks))
	}
	digest0 := contentSHA256(chunks[0])
	client := &fakeHistoryClient{history: []slackapi.Message{
		{Msg: slackapi.Msg{User: testBot, Text: chunks[0], Timestamp: "1720000001.000001", Metadata: slackapi.SlackMetadata{
			EventType: "local_agent.assistant_exchange",
			EventPayload: map[string]any{
				"correlation_id": "intent-correlation",
				"render_mode":    "markdown_v1",
				"part_index":     float64(1),
				"part_count":     float64(2),
				"content_sha256": digest0,
			},
		}}},
		// Missing metadata on second chunk
		{Msg: slackapi.Msg{User: testBot, Text: chunks[1], Timestamp: "1720000002.000002"}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil)
	timestamp, found, err := reader.FindPublishedAssistantExchange(context.Background(), port.AssistantExchangeIntent{
		ChannelID: testDM, ChannelKind: domain.ChannelDM, Content: content, CorrelationID: "intent-correlation",
	})
	if err != nil || found || timestamp != "" {
		t.Fatalf("partial correlation recovery = %q, %t, %v", timestamp, found, err)
	}
}

func TestHistoryReaderDetectsBotParticipationWithTranslatedMessage(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{replies: []slackapi.Message{
		{Msg: slackapi.Msg{User: testUser, Text: "question", Timestamp: "1.1"}},
		{Msg: slackapi.Msg{User: testBot, Text: "", Timestamp: "1.2", SubType: slackapi.MsgSubTypeBotMessage,
			Blocks: slackapi.Blocks{BlockSet: []slackapi.Block{
				slackapi.NewSectionBlock(slackapi.NewTextBlockObject("mrkdwn", "translated response", false, false), nil, nil),
			}},
		}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil)

	got, err := reader.RecentHistory(context.Background(), validThreadInvocation(), domain.ContextLimits{MaxMessages: 5, MaxChars: 100})
	if err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	if !got.BotParticipated {
		t.Fatal("bot participation = false for translated message")
	}
	if len(got.Messages) != 2 {
		t.Fatalf("message count = %d, want 2 (user + bot with translated content)", len(got.Messages))
	}
	if got.Messages[1].Content != "translated response" {
		t.Fatalf("extracted translated content = %q", got.Messages[1].Content)
	}
}

func TestHistoryReaderSkipsEmptyTranslatedBotMessage(t *testing.T) {
	t.Parallel()
	client := &fakeHistoryClient{replies: []slackapi.Message{
		{Msg: slackapi.Msg{User: testUser, Text: "question", Timestamp: "1.1"}},
		{Msg: slackapi.Msg{User: testBot, Text: "", Timestamp: "1.2", SubType: slackapi.MsgSubTypeBotMessage}},
	}}
	reader := newHistoryReader(client, testBot, time.Second, nil)

	got, err := reader.RecentHistory(context.Background(), validThreadInvocation(), domain.ContextLimits{MaxMessages: 5, MaxChars: 100})
	if err != nil {
		t.Fatalf("RecentHistory() error = %v", err)
	}
	if !got.BotParticipated {
		t.Fatal("bot participation should still be detected even with empty text")
	}
	if len(got.Messages) != 1 {
		t.Fatalf("message count = %d, want 1 (only user message)", len(got.Messages))
	}
}

func TestHistoryReaderExtractsObservedTranslatedBlocksWithinLimit(t *testing.T) {
	t.Parallel()
	table := slackapi.NewTableBlock("").AddRow(
		slackapi.NewTableRawTextCell("Name"),
		slackapi.NewTableRawTextCell("Value"),
	).AddRow(
		slackapi.NewTableRawTextCell("item"),
		slackapi.NewTableRawNumberCell(42),
	)
	blocks := []slackapi.Block{
		slackapi.NewMarkdownBlock("", "# Heading"),
		table,
	}
	got := extractPlainTextFromBlocks(blocks, 100)
	if got != "# Heading\nName | Value\nitem | 42" {
		t.Fatalf("translated block text = %q", got)
	}
	bounded := extractPlainTextFromBlocks([]slackapi.Block{
		slackapi.NewMarkdownBlock("", strings.Repeat("界", 50)),
	}, 12)
	if len([]rune(bounded)) != 12 {
		t.Fatalf("bounded translated text has %d runes", len([]rune(bounded)))
	}
}

func TestHistoryReaderValidatesDependenciesAndInput(t *testing.T) {
	t.Parallel()
	validClient := &fakeHistoryClient{}
	tests := []struct {
		name   string
		reader *HistoryReader
		inv    domain.Invocation
		limits domain.ContextLimits
	}{
		{name: "missing client", reader: newHistoryReader(nil, testBot, time.Second, nil), inv: validDMInvocation(), limits: domain.ContextLimits{MaxMessages: 1, MaxChars: 1}},
		{name: "missing bot ID", reader: newHistoryReader(validClient, "", time.Second, nil), inv: validDMInvocation(), limits: domain.ContextLimits{MaxMessages: 1, MaxChars: 1}},
		{name: "invalid limits", reader: newHistoryReader(validClient, testBot, time.Second, nil), inv: validDMInvocation()},
		{name: "invalid invocation", reader: newHistoryReader(validClient, testBot, time.Second, nil), inv: domain.Invocation{}, limits: domain.ContextLimits{MaxMessages: 1, MaxChars: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tt.reader.RecentHistory(context.Background(), tt.inv, tt.limits); err == nil {
				t.Fatal("RecentHistory() error = nil")
			}
		})
	}
}

type historyCall struct {
	method      string
	channelID   string
	rootTS      string
	latest      string
	limit       int
	hadDeadline bool
}

type fakeHistoryClient struct {
	mu      sync.Mutex
	calls   []historyCall
	replies []slackapi.Message
	history []slackapi.Message
	err     error
}

func (c *fakeHistoryClient) ConversationReplies(ctx context.Context, channelID, rootTS, latest string, limit int) ([]slackapi.Message, error) {
	c.record(ctx, historyCall{method: "replies", channelID: channelID, rootTS: rootTS, latest: latest, limit: limit})
	return append([]slackapi.Message(nil), c.replies...), c.err
}

func (c *fakeHistoryClient) ConversationHistory(ctx context.Context, channelID, latest string, limit int) ([]slackapi.Message, error) {
	c.record(ctx, historyCall{method: "history", channelID: channelID, latest: latest, limit: limit})
	return append([]slackapi.Message(nil), c.history...), c.err
}

func (c *fakeHistoryClient) record(ctx context.Context, call historyCall) {
	_, call.hadDeadline = ctx.Deadline()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

func (c *fakeHistoryClient) lastCall() historyCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return historyCall{}
	}
	return c.calls[len(c.calls)-1]
}

func validThreadInvocation() domain.Invocation {
	return domain.Invocation{
		EventID: testEventID, EventType: "message", TeamID: testTeam,
		ChannelID: testChannel, ChannelKind: domain.ChannelPublic, UserID: testUser,
		EventTS: testTS, ThreadTS: testThread, Text: "follow up", Trigger: domain.TriggerThreadReply,
	}
}

func validDMInvocation() domain.Invocation {
	return domain.Invocation{
		EventID: testEventID, EventType: "message", TeamID: testTeam,
		ChannelID: testDM, ChannelKind: domain.ChannelDM, UserID: testUser,
		EventTS: testTS, Text: "hello", Trigger: domain.TriggerDirectMessage,
	}
}
