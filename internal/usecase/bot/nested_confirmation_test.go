package bot

import (
	"context"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestResumePendingConfirmationIsPersistedAndPublishedInsteadOfApprovalFallback(t *testing.T) {
	invocation := botInvocation()
	key, err := invocation.ConversationKey()
	if err != nil {
		t.Fatal(err)
	}
	first := &port.ConfirmationDelivery{
		WrapperCallID: "first", OriginalCallID: "original-first", SessionID: "adk:" + string(key),
		Actor: invocation.UserID, TeamID: invocation.TeamID, ChannelID: invocation.ChannelID,
		ConversationKey: key, Status: port.ConfirmationPublished, Expiry: time.Now().Add(time.Hour),
	}
	second := &domain.PendingConfirmation{
		WrapperCallID: "second", OriginalCallID: "original-second", Actor: invocation.UserID,
		Summary: "second action", Expiry: time.Now().Add(time.Hour),
	}
	confirmations := &recordingConfirmationStore{fakeConfirmationStore: fakeConfirmationStore{delivery: first}}
	runtime := &fakeRuntime{resumeTurn: port.AgentTurn{PendingConfirmation: second}}
	publisher := &fakePublisher{}
	service := newTestServiceWithConfirmations(t, &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}, runtime, &fakeHistory{}, publisher, confirmations, nil)

	if outcome := service.HandleConfirmation(t.Context(), invocation, first.WrapperCallID, true); outcome != OutcomeResponded {
		t.Fatalf("HandleConfirmation() = %q", outcome)
	}
	if len(confirmations.created) != 1 || confirmations.created[0].WrapperCallID != "second" {
		t.Fatalf("created deliveries = %#v", confirmations.created)
	}
	if len(publisher.calls) != 1 || publisher.calls[0].text == "Confirmation approved." || publisher.calls[0].text == "" {
		t.Fatalf("published calls = %#v", publisher.calls)
	}
}

type recordingConfirmationStore struct {
	fakeConfirmationStore
	created []port.ConfirmationDelivery
}

func (s *recordingConfirmationStore) CreateDelivery(_ context.Context, delivery port.ConfirmationDelivery) error {
	s.created = append(s.created, delivery)
	return nil
}
