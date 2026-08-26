package slack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type onboardingGuidancePublisherFake struct {
	target domain.ReplyTarget
	text   string
	calls  int
}

func (p *onboardingGuidancePublisherFake) Publish(_ context.Context, target domain.ReplyTarget, text string) (port.PublishedResponse, error) {
	p.calls++
	p.target = target
	p.text = text
	return port.PublishedResponse{LastMessageTS: "1700000001.000001"}, nil
}

func TestOnboardingDescribeActionPublishesGuidanceWithoutModelDispatch(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata, err := encodeBuilderInteractionContext("U12345678", key)
	if err != nil {
		t.Fatal(err)
	}
	publisher := &onboardingGuidancePublisherFake{}
	listener := newListener(nil, NewRouter(testBot), nil).WithResponsePublisher(publisher)
	callback := slackapi.InteractionCallback{
		Type:      slackapi.InteractionTypeBlockActions,
		Team:      slackapi.Team{ID: "T12345678"},
		User:      slackapi.User{ID: "U12345678"},
		Container: slackapi.Container{ChannelID: "D12345678"},
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{{
			ActionID: "local_agent.onboarding.describe", Value: metadata,
		}}},
	}

	if err := listener.handleOnboardingDescribeAction(t.Context(), callback); err != nil {
		t.Fatal(err)
	}
	if publisher.calls != 1 || publisher.target.ChannelID != "D12345678" || publisher.text != onboardingDescribePrompt {
		t.Fatalf("publisher=%#v", publisher)
	}
}

func TestOnboardingDescribeActionRejectsMismatchedContext(t *testing.T) {
	publisher := &onboardingGuidancePublisherFake{}
	listener := newListener(nil, NewRouter(testBot), nil).WithResponsePublisher(publisher)
	callback := slackapi.InteractionCallback{
		Type:      slackapi.InteractionTypeBlockActions,
		Team:      slackapi.Team{ID: "T12345678"},
		User:      slackapi.User{ID: "U12345678"},
		Container: slackapi.Container{ChannelID: "D12345678"},
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{{
			ActionID: "local_agent.onboarding.describe", Value: "U87654321",
		}}},
	}

	if err := listener.handleOnboardingDescribeAction(t.Context(), callback); err != nil {
		t.Fatal(err)
	}
	if publisher.calls != 0 {
		t.Fatalf("publisher calls=%d, want 0", publisher.calls)
	}
}
