package integration_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/adapter/memorycurator"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
)

func TestSharedModelCallLimitIncludesForegroundAndCurator(t *testing.T) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "local-agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	shared := modelcalllimiter.New(1)
	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	service, err := botusecase.New(botusecase.Config{
		AccessPolicy:  domain.AccessPolicy{AllowedUserIDs: []string{"U12345678"}},
		ContextLimits: domain.ContextLimits{MaxMessages: 30, MaxChars: 20_000}, RetainMessages: 100, MaxConcurrentCalls: 1,
		BusyMessage: "busy", ModelErrorMessage: "model error", UnauthorizedMessage: "denied",
	}, botusecase.Dependencies{
		Store: store, Agent: blockingForegroundAgent{started: started, unblock: unblock}, Publisher: integrationPublisher{}, ModelCalls: shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	curatorLLM := &countingCuratorLLM{}
	curator, err := memorycurator.New(curatorLLM, memorycurator.Config{ModelCalls: shared})
	if err != nil {
		t.Fatal(err)
	}

	foregroundDone := make(chan error, 1)
	go func() {
		outcome, err := service.Handle(t.Context(), dmInvocation("Ev-model-limit", "D12345678", "1700000000.000001", "question"))
		if err == nil && outcome != botusecase.OutcomeResponded {
			err = errors.New("foreground request did not respond")
		}
		foregroundDone <- err
	}()
	<-started

	_, err = curator.ProposePatch(t.Context(), "slack:T12345678:dm:D12345678", "1700000000.000001", []domain.Message{{Role: domain.RoleUser, Content: "durable fact"}}, nil)
	if !errors.Is(err, port.ErrModelCallLimitReached) {
		t.Fatalf("curator error = %v, want shared-limit error", err)
	}
	if curatorLLM.calls != 0 {
		t.Fatalf("curator made %d model calls while foreground occupied the only slot", curatorLLM.calls)
	}

	close(unblock)
	if err := <-foregroundDone; err != nil {
		t.Fatal(err)
	}
	if _, err := curator.ProposePatch(t.Context(), "slack:T12345678:dm:D12345678", "1700000000.000001", []domain.Message{{Role: domain.RoleUser, Content: "durable fact"}}, nil); err != nil {
		t.Fatalf("curator did not acquire released shared slot: %v", err)
	}
	if curatorLLM.calls != 1 {
		t.Fatalf("curator model calls = %d, want 1 after foreground release", curatorLLM.calls)
	}
}

type blockingForegroundAgent struct {
	started chan<- struct{}
	unblock <-chan struct{}
}

func (a blockingForegroundAgent) Respond(_ context.Context, _ []domain.Message, _ []domain.MemorySnippet) (string, error) {
	a.started <- struct{}{}
	<-a.unblock
	return "answer", nil
}

type countingCuratorLLM struct{ calls int }

func (l *countingCuratorLLM) GenerateText(context.Context, string) (string, error) {
	l.calls++
	return "[]", nil
}
