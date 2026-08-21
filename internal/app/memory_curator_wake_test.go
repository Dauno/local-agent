package app

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	"github.com/Dauno/slack-local-agent/internal/adapter/memorycurator"
	"github.com/Dauno/slack-local-agent/internal/adapter/memoryprojector"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
)

// wakeCompositionPatchLLM proposes zero memory operations, so a curator
// tick under test settles without depending on real model content.
type wakeCompositionPatchLLM struct{}

func (wakeCompositionPatchLLM) GenerateText(context.Context, string) (string, error) {
	return `{"operations":[]}`, nil
}

var _ memorycurator.LLM = wakeCompositionPatchLLM{}

// wakeCompositionAgentRuntime and wakeCompositionPublisher only satisfy
// botusecase.New's non-nil constructor checks: startMemoryCurator's wake
// wiring never routes through a turn, so neither is ever invoked.
type wakeCompositionAgentRuntime struct{}

func (wakeCompositionAgentRuntime) Run(context.Context, port.AgentRequest) (port.AgentTurn, error) {
	return port.AgentTurn{}, nil
}

func (wakeCompositionAgentRuntime) Resume(context.Context, domain.ConfirmationDecision) (port.AgentTurn, error) {
	return port.AgentTurn{}, nil
}

type wakeCompositionPublisher struct{}

func (wakeCompositionPublisher) Publish(context.Context, domain.ReplyTarget, string) (port.PublishedResponse, error) {
	return port.PublishedResponse{}, nil
}

// TestStartMemoryCuratorRunnerConsumesThroughItsOwnInjectedScheduler is
// FIND-122's repair for the legacy memory outbox class: it drives the
// actual production composition method, (*Application).startMemoryCurator,
// instead of a hand-built stand-in for the memory.Runner it constructs. A
// regression that stops passing the supplied scheduler through to
// memoryusecase.NewRunner (internal/app/composition.go) makes this test
// hang until the go test binary's own -timeout, because the runner would
// then advance only through its own recovery timer, which this test never
// fires.
//
// This test covers the runner side of startMemoryCurator's wiring. It does
// not independently prove that service.AddMemory receives the identical
// scheduler's Wake function: that half is only exercised by a real turn
// through bot.Service.SendMessage, which this composition-level fixture
// does not attempt to reconstruct (the bot package's own equivalent test
// bypasses SendMessage for the same reason). The risk this leaves
// uncovered is narrow: startMemoryCurator uses one local memoryScheduler
// variable at both call sites, six lines apart.
func TestStartMemoryCuratorRunnerConsumesThroughItsOwnInjectedScheduler(t *testing.T) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "memory-curator-wake.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "1"}
	if err := store.AppendMessage(t.Context(), metadata, domain.Message{
		Role: domain.RoleUser, Content: "record a durable fact", UserID: "U12345678", ExternalTS: "1", CreatedAt: time.Now().UTC(),
	}, 10); err != nil {
		t.Fatal(err)
	}
	assistant := domain.Message{Role: domain.RoleAssistant, Content: "the fact is durable", ExternalTS: "2", CreatedAt: time.Now().UTC()}
	prepared, err := store.PrepareAssistantExchange(t.Context(), metadata, assistant, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	// The durable outbox row is seeded directly through the store, exactly
	// like bot.wake_composition_test.go's "pending before restart" fixture,
	// so no stray wake from a real turn races the fake scheduler below.
	if err := store.MarkAssistantExchangePublished(t.Context(), prepared.ID, assistant.ExternalTS); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeAssistantExchange(t.Context(), prepared.ID); err != nil {
		t.Fatal(err)
	}

	logger := logging.New(io.Discard, "error", secure.NewRedactor())
	models := newRuntimeModels()
	models.redactor = secure.NewRedactor()
	models.logger = logger
	models.curatorLLM = wakeCompositionPatchLLM{}

	infra := &runtimeInfrastructure{store: store, modelCalls: modelcalllimiter.New(1)}
	setup := runtimeSetup{cfg: config.Default(), paths: config.Paths{MemoryDir: t.TempDir()}}

	service, err := botusecase.New(botusecase.Config{
		ContextLimits: domain.ContextLimits{MaxMessages: 1, MaxChars: 100}, RetainMessages: 10,
		MaxConcurrentCalls: 1, BusyMessage: "busy", ModelErrorMessage: "error", UnauthorizedMessage: "unauthorized",
	}, botusecase.Dependencies{Store: store, Runtime: wakeCompositionAgentRuntime{}, Publisher: wakeCompositionPublisher{}})
	if err != nil {
		t.Fatal(err)
	}

	scheduler, timers := newWakeCompositionScheduler(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if err := (&Application{}).startMemoryCurator(ctx, setup, models, infra, service, memoryprojector.New(), scheduler); err != nil {
		t.Fatal(err)
	}
	// The initial poll must consume the pre-existing durable outbox item
	// through the exact scheduler startMemoryCurator was handed, never
	// through a real recovery timer.
	waitCompositionPoll(t, timers)

	var status string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT status FROM memory_outbox ORDER BY rowid DESC LIMIT 1`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "done" {
		t.Fatalf("outbox status after initial poll = %q, want done", status)
	}
}
