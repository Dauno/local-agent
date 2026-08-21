package app

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	"github.com/Dauno/slack-local-agent/internal/adapter/memoryprojector"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
)

// handleWakeAgentRuntime answers one hermetic turn with fixed, non-empty
// text, so bot.Service.Handle reaches finalizeTurn and persistAssistantTurn
// instead of short-circuiting on an empty model response.
type handleWakeAgentRuntime struct{}

func (handleWakeAgentRuntime) Run(context.Context, port.AgentRequest) (port.AgentTurn, error) {
	return port.AgentTurn{Text: "the fact is durable"}, nil
}

func (handleWakeAgentRuntime) Resume(context.Context, domain.ConfirmationDecision) (port.AgentTurn, error) {
	return port.AgentTurn{}, nil
}

// handleWakePublisher answers with a fixed Slack timestamp, so
// bot.Service.finalizeTurn treats the response as delivered and proceeds to
// persistAssistantTurn instead of failing on a missing timestamp.
type handleWakePublisher struct{}

func (handleWakePublisher) Publish(context.Context, domain.ReplyTarget, string) (port.PublishedResponse, error) {
	return port.PublishedResponse{LastMessageTS: "1700000000.000002"}, nil
}

func handleWakeInvocation() domain.Invocation {
	return domain.Invocation{
		EventID: "ev-handle-wake", EventType: "message.im", TeamID: "T12345678",
		ChannelID: "D12345678", ChannelKind: domain.ChannelDM, UserID: "U12345678",
		EventTS: "1700000000.000001", Text: "record a durable fact", Trigger: domain.TriggerDirectMessage,
	}
}

// TestStartMemoryCuratorFullLinkFromRealTurnToRunner is FIND-122's repair
// for the part of the memory-curator link the initial-poll fixture in
// memory_curator_wake_test.go cannot reach: the full chain the code
// declares in its own comments,
//
//	startMemoryCurator -> Service.AddMemory wake -> persistAssistantTurn ->
//	memoryScheduler -> memory.Runner
//
// This test drives the real bot.Service built by startMemoryCurator's own
// fixture through the smallest hermetic Service.Handle call that finalizes
// an assistant exchange (a fake AgentRuntime and a fake Publisher, no
// network and no ADK), and confirms the durable memory outbox item that
// turn produces is consumed without ever firing the fake recovery timer. A
// regression that passes service.AddMemory a no-op wake, or a scheduler
// different from the one startMemoryCurator hands memory.Runner, makes this
// test hang until the go test binary's own -timeout, because the runner
// would then advance only through its own recovery timer, which this test
// never fires.
func TestStartMemoryCuratorFullLinkFromRealTurnToRunner(t *testing.T) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "memory-curator-handle-wake.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	logger := logging.New(io.Discard, "error", secure.NewRedactor())
	models := newRuntimeModels()
	models.redactor = secure.NewRedactor()
	models.logger = logger
	models.curatorLLM = wakeCompositionPatchLLM{}

	infra := &runtimeInfrastructure{store: store, modelCalls: modelcalllimiter.New(1)}
	setup := runtimeSetup{cfg: config.Default(), paths: config.Paths{MemoryDir: t.TempDir()}}

	service, err := botusecase.New(botusecase.Config{
		AccessPolicy:   domain.AccessPolicy{AllowAllUsers: true},
		ContextLimits:  domain.ContextLimits{MaxMessages: 10, MaxChars: 20000},
		RetainMessages: 10, MaxConcurrentCalls: 1,
		BusyMessage: "busy", ModelErrorMessage: "error", UnauthorizedMessage: "unauthorized",
	}, botusecase.Dependencies{Store: store, Runtime: handleWakeAgentRuntime{}, Publisher: handleWakePublisher{}})
	if err != nil {
		t.Fatal(err)
	}

	scheduler, timers := newWakeCompositionScheduler(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if err := (&Application{}).startMemoryCurator(ctx, setup, models, infra, service, memoryprojector.New(), scheduler); err != nil {
		t.Fatal(err)
	}
	// startMemoryCurator wires service.AddMemory(memorySvc, infra.store,
	// memoryScheduler.Wake) as part of this call: from here on, the outbox
	// item this turn produces can only be consumed if that wake argument is
	// the identical scheduler memory.Runner drains.
	waitCompositionPoll(t, timers) // initial poll: outbox is empty.

	outcome, err := service.Handle(t.Context(), handleWakeInvocation())
	if err != nil || outcome != botusecase.OutcomeResponded {
		t.Fatalf("Handle() = %q, %v, want OutcomeResponded", outcome, err)
	}
	// The tick triggered by persistAssistantTurn's call to s.memoryWake(),
	// not the timer, must drain the outbox item this turn just produced.
	waitCompositionPoll(t, timers)

	var status string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT status FROM memory_outbox ORDER BY rowid DESC LIMIT 1`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "done" {
		t.Fatalf("outbox status after wake-driven poll = %q, want done", status)
	}
}
