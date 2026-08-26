package bot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type onboardingStoreFake struct {
	state     port.OnboardingDeliveryState
	claim     port.OnboardingDeliveryClaim
	markCalls int
	messageTS string
}

func (s *onboardingStoreFake) ClaimOnboarding(_ context.Context, _, _ string, key domain.ConversationKey, _ time.Time) (port.OnboardingDeliveryClaim, port.OnboardingDeliveryState, error) {
	if s.state == "" {
		s.state = port.OnboardingClaimed
		s.claim = port.OnboardingDeliveryClaim{DeliveryID: "standard_onboarding:T12345678:U12345678", ClaimToken: "claim-1", ConversationKey: key}
	}
	return s.claim, s.state, nil
}

func (s *onboardingStoreFake) MarkOnboardingPublished(_ context.Context, claim port.OnboardingDeliveryClaim, messageTS string, _ time.Time) error {
	if claim != s.claim {
		return errors.New("unexpected onboarding claim")
	}
	s.markCalls++
	s.state = port.OnboardingAlreadyPublished
	s.messageTS = messageTS
	return nil
}

type onboardingPublisherFake struct {
	recoverResult port.PublishedResponse
	recoverFound  bool
	recoverErr    error
	publishResult port.PublishedResponse
	publishErr    error
	recoverCalls  int
	publishCalls  int
	recoverTarget domain.ReplyTarget
	publishTarget domain.ReplyTarget
	publishReq    port.OnboardingPublishRequest
}

func (p *onboardingPublisherFake) PublishOnboarding(_ context.Context, target domain.ReplyTarget, req port.OnboardingPublishRequest) (port.PublishedResponse, error) {
	p.publishCalls++
	p.publishTarget = target
	p.publishReq = req
	if p.publishErr != nil {
		return port.PublishedResponse{}, p.publishErr
	}
	if p.publishResult.LastMessageTS == "" {
		p.publishResult.LastMessageTS = "1700000001.000001"
	}
	return p.publishResult, nil
}

func (p *onboardingPublisherFake) RecoverOnboarding(_ context.Context, target domain.ReplyTarget, _ string) (port.PublishedResponse, bool, error) {
	p.recoverCalls++
	p.recoverTarget = target
	return p.recoverResult, p.recoverFound, p.recoverErr
}

func newOnboardingService(t *testing.T, store *onboardingStoreFake, publisher *onboardingPublisherFake, runtime *fakeRuntime) *Service {
	t.Helper()
	service := newTestService(t, &fakeStore{claimAll: true, recent: make(map[domain.ConversationKey][]domain.Message)}, runtime, &fakeHistory{}, &fakePublisher{}, nil)
	service.cfg.PromptsEnabled = true
	service.cfg.SuggestedPrompts = []string{"Analiza este proyecto."}
	service.onboardingStore = store
	service.onboardingPublisher = publisher
	return service
}

func onboardingInvocation() domain.Invocation {
	invocation := botInvocation()
	invocation.ThreadedDM = true
	invocation.Text = "hola"
	return invocation
}

func TestHandleIsolatedGreetingPublishesOnboardingWithoutModelCall(t *testing.T) {
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "model response"}}
	store := &onboardingStoreFake{}
	publisher := &onboardingPublisherFake{}
	service := newOnboardingService(t, store, publisher, runtime)

	outcome, err := service.Handle(t.Context(), onboardingInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if runtime.runCalls != 0 || publisher.publishCalls != 1 || publisher.recoverCalls != 1 || store.markCalls != 1 {
		t.Fatalf("runtime=%d publish=%d recover=%d mark=%d", runtime.runCalls, publisher.publishCalls, publisher.recoverCalls, store.markCalls)
	}
}

func TestHandleRepeatedGreetingDoesNotPublishOrCallModel(t *testing.T) {
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "model response"}}
	store := &onboardingStoreFake{
		state: port.OnboardingAlreadyPublished,
		claim: port.OnboardingDeliveryClaim{DeliveryID: "standard_onboarding:T12345678:U12345678", ConversationKey: "slack:T12345678:dm:D12345678:thread:1700000000.000001"},
	}
	publisher := &onboardingPublisherFake{}
	service := newOnboardingService(t, store, publisher, runtime)

	outcome, err := service.Handle(t.Context(), onboardingInvocation())
	if err != nil || outcome != OutcomeDuplicate {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if runtime.runCalls != 0 || publisher.publishCalls != 0 {
		t.Fatalf("runtime=%d publish=%d", runtime.runCalls, publisher.publishCalls)
	}
}

func TestHandleSubstantiveGreetingUsesNormalModelPath(t *testing.T) {
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "model response"}}
	store := &onboardingStoreFake{}
	publisher := &onboardingPublisherFake{}
	service := newOnboardingService(t, store, publisher, runtime)
	invocation := onboardingInvocation()
	invocation.Text = "hola, revisa este error"

	outcome, err := service.Handle(t.Context(), invocation)
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if runtime.runCalls != 1 || publisher.publishCalls != 0 || publisher.recoverCalls != 0 {
		t.Fatalf("runtime=%d publish=%d recover=%d", runtime.runCalls, publisher.publishCalls, publisher.recoverCalls)
	}
}

func TestHandleOnboardingPublicationFailureDoesNotFallBackToModel(t *testing.T) {
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "model response"}}
	store := &onboardingStoreFake{}
	publisher := &onboardingPublisherFake{publishErr: errors.New("slack unavailable")}
	service := newOnboardingService(t, store, publisher, runtime)

	outcome, err := service.Handle(t.Context(), onboardingInvocation())
	if err != nil || outcome != OutcomePublishFailed {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if runtime.runCalls != 0 || publisher.publishCalls != 1 || store.markCalls != 0 {
		t.Fatalf("runtime=%d publish=%d mark=%d", runtime.runCalls, publisher.publishCalls, store.markCalls)
	}
}

func TestHandleOnboardingRecoversPublishedMessageBeforeRetry(t *testing.T) {
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "model response"}}
	store := &onboardingStoreFake{}
	publisher := &onboardingPublisherFake{recoverResult: port.PublishedResponse{LastMessageTS: "1700000001.000099"}, recoverFound: true}
	service := newOnboardingService(t, store, publisher, runtime)

	outcome, err := service.Handle(t.Context(), onboardingInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if runtime.runCalls != 0 || publisher.publishCalls != 0 || store.messageTS != "1700000001.000099" {
		t.Fatalf("runtime=%d publish=%d message=%q", runtime.runCalls, publisher.publishCalls, store.messageTS)
	}
}

func TestHandleOnboardingUsesDurableConversationForRecoveryAndPublication(t *testing.T) {
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "model response"}}
	originalKey := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1699999999.000001")
	store := &onboardingStoreFake{
		state: port.OnboardingClaimed,
		claim: port.OnboardingDeliveryClaim{
			DeliveryID: "standard_onboarding:T12345678:U12345678", ClaimToken: "claim-2", ConversationKey: originalKey,
		},
	}
	publisher := &onboardingPublisherFake{}
	service := newOnboardingService(t, store, publisher, runtime)
	invocation := onboardingInvocation()
	invocation.EventTS = "1700000002.000001"

	outcome, err := service.Handle(t.Context(), invocation)
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	want := domain.ReplyTarget{ChannelID: "D12345678", ThreadTS: "1699999999.000001"}
	if publisher.recoverTarget != want || publisher.publishTarget != want || publisher.publishReq.ConversationKey != originalKey {
		t.Fatalf("recover=%#v publish=%#v request_key=%q", publisher.recoverTarget, publisher.publishTarget, publisher.publishReq.ConversationKey)
	}
}

func TestHandleDuplicateGreetingRecoversInFlightOnboarding(t *testing.T) {
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "model response"}}
	conversationStore := &fakeStore{claimed: true, recent: make(map[domain.ConversationKey][]domain.Message)}
	service := newTestService(t, conversationStore, runtime, &fakeHistory{}, &fakePublisher{}, nil)
	service.cfg.PromptsEnabled = true
	service.cfg.SuggestedPrompts = []string{"Analiza este proyecto."}
	claim := port.OnboardingDeliveryClaim{
		DeliveryID: "standard_onboarding:T12345678:U12345678", ClaimToken: "claim-live",
		ConversationKey: "slack:T12345678:dm:D12345678:thread:1700000000.000001",
	}
	store := &onboardingStoreFake{state: port.OnboardingInFlight, claim: claim}
	publisher := &onboardingPublisherFake{
		recoverResult: port.PublishedResponse{LastMessageTS: "1700000001.000099"}, recoverFound: true,
	}
	service.onboardingStore = store
	service.onboardingPublisher = publisher

	outcome, err := service.Handle(t.Context(), onboardingInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	if publisher.recoverCalls != 1 || publisher.publishCalls != 0 || store.markCalls != 1 || runtime.runCalls != 0 {
		t.Fatalf("recover=%d publish=%d mark=%d runtime=%d", publisher.recoverCalls, publisher.publishCalls, store.markCalls, runtime.runCalls)
	}
}

func TestIsolatedGreetingGateRejectsSubstantiveAndIneligibleMessages(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*domain.Invocation)
		want   bool
	}{
		{name: "punctuated hola", mutate: func(invocation *domain.Invocation) { invocation.Text = "¡Hola!" }, want: true},
		{name: "substantive", mutate: func(invocation *domain.Invocation) { invocation.Text = "hola, revisa este error" }, want: false},
		{name: "file-bearing", mutate: func(invocation *domain.Invocation) {
			invocation.Attachments = []domain.Attachment{{ID: "F12345678", Name: "error.txt", MIMEType: "text/plain"}}
		}, want: false},
		{name: "channel thread", mutate: func(invocation *domain.Invocation) {
			invocation.ChannelKind = domain.ChannelPublic
			invocation.ChannelID = "C12345678"
			invocation.Trigger = domain.TriggerThreadReply
			invocation.ThreadTS = invocation.EventTS
		}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invocation := onboardingInvocation()
			test.mutate(&invocation)
			if got := isIsolatedGreeting(invocation); got != test.want {
				t.Fatalf("isIsolatedGreeting()=%v, want %v", got, test.want)
			}
		})
	}
}
