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
