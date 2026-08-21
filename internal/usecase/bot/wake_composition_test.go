package bot

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/memorycurator"
	"github.com/Dauno/slack-local-agent/internal/adapter/memoryprojector"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	memoryusecase "github.com/Dauno/slack-local-agent/internal/usecase/memory"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

// wakeTimer is a workpoll.Timer whose channel never fires on its own.
type wakeTimer struct{ c chan time.Time }

func (t *wakeTimer) C() <-chan time.Time { return t.c }
func (t *wakeTimer) Stop() bool          { return true }

type wakeTimers struct{ created chan *wakeTimer }

func newWakeTimers() *wakeTimers { return &wakeTimers{created: make(chan *wakeTimer, 16)} }

func (f *wakeTimers) New(time.Duration) workpoll.Timer {
	timer := &wakeTimer{c: make(chan time.Time, 1)}
	f.created <- timer
	return timer
}

func newWakeScheduler(t *testing.T) (*workpoll.Scheduler, *wakeTimers) {
	t.Helper()
	timers := newWakeTimers()
	scheduler, err := workpoll.New(time.Hour, workpoll.Options{NewTimer: timers.New})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler, timers
}

// waitPoll blocks on the fake timer's creation channel only: it carries no
// real duration of its own, so a hang here surfaces as the go test binary's
// own -timeout, never as a flaky arbitrary wait inside the test.
func waitPoll(t *testing.T, timers *wakeTimers) {
	t.Helper()
	<-timers.created
}

type noopExchangeFinder struct{}

func (noopExchangeFinder) FindPublishedAssistantExchange(context.Context, port.AssistantExchangeIntent) (string, bool, error) {
	return "", false, nil
}

// emptyPatchLLM proposes zero memory operations, so the memory runner tick
// under test settles quickly without depending on curator content details.
type emptyPatchLLM struct{}

func (emptyPatchLLM) GenerateText(context.Context, string) (string, error) {
	return `{"operations":[]}`, nil
}

// memoryWakeFixture builds the real durable exchange store, the real
// memory.Runner over it, and a bot.Service whose memoryWake is wired to the
// same scheduler as the runner, exactly like composition.go's
// `service.AddMemory(memorySvc, infra.store, memoryScheduler.Wake)` wires
// bot.Service.AddMemory and memory.Runner over one shared *workpoll.Scheduler.
func memoryWakeFixture(t *testing.T, scheduler *workpoll.Scheduler) (*Service, *memoryusecase.Runner, *adaptersqlite.Store, domain.ConversationMetadata) {
	t.Helper()
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "memory-wake.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "1"}
	if err := store.AppendMessage(t.Context(), metadata, domain.Message{Role: domain.RoleUser, Content: "record a durable fact", UserID: "U12345678", ExternalTS: "1", CreatedAt: time.Now().UTC()}, 10); err != nil {
		t.Fatal(err)
	}

	memoryService, err := memoryusecase.New(memoryusecase.Config{
		Recall: domain.MemoryRecallConfig{Enabled: true, MaxTopics: 1, MaxChars: 100},
		Limits: domain.MemoryLimits{MaxTopics: 2, MaxLinks: 1, MaxTopicChars: 100}, MaxPatchOps: 1,
	}, memoryusecase.Dependencies{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	curator, err := memorycurator.New(emptyPatchLLM{}, memorycurator.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := memoryusecase.NewRunner(memoryusecase.RunnerConfig{Interval: time.Hour, MaxRetries: 1, MemoryDir: t.TempDir()}, memoryusecase.RunnerDependencies{
		Store: store, ExchangeFinder: noopExchangeFinder{}, Curator: curator, Memory: memoryService,
		Projector: memoryprojector.New(), ProjectionReader: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Scheduler: scheduler,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{exchange: store, memoryEnabled: true, memoryWake: scheduler.Wake, logger: memoryWakeTestLogger{}}
	return service, runner, store, metadata
}

// memoryOutboxStatus reads the durable outbox row's status. waitPoll already
// synchronizes on the scheduler completing its poll cycle before this runs.
func memoryOutboxStatus(t *testing.T, store *adaptersqlite.Store) (status string, ok bool) {
	t.Helper()
	err := store.DB().QueryRowContext(t.Context(), `SELECT status FROM memory_outbox ORDER BY rowid DESC LIMIT 1`).Scan(&status)
	if err != nil {
		return "", false
	}
	return status, true
}

// TestMemorySchedulerConsumesWithoutRecoveryTimer pins FIND-122 for the
// legacy memory outbox class: memory.Runner only ever advances through the
// initial durable poll or a real wake from bot.Service.persistAssistantTurn
// (the AddMemory producer path), never through the recovery timer.
func TestMemorySchedulerConsumesWithoutRecoveryTimer(t *testing.T) {
	t.Run("restart empty then produced", func(t *testing.T) {
		scheduler, timers := newWakeScheduler(t)
		service, runner, store, metadata := memoryWakeFixture(t, scheduler)
		assistant := domain.Message{Role: domain.RoleAssistant, Content: "the fact is durable", ExternalTS: "2", CreatedAt: time.Now().UTC()}
		prepared, err := store.PrepareAssistantExchange(t.Context(), metadata, assistant, 10, true)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go runner.Run(ctx)
		waitPoll(t, timers) // initial poll: outbox is empty; the exchange is not finalized yet.

		if err := service.persistAssistantTurn(t.Context(), metadata, assistant.ExternalTS, assistant.Content, prepared); err != nil {
			t.Fatal(err)
		}
		waitPoll(t, timers) // poll triggered by the producer's wake, not the timer.
		if status, ok := memoryOutboxStatus(t, store); !ok || status != "done" {
			t.Fatalf("outbox status after wake-driven poll = %q, ok = %v", status, ok)
		}
	})

	t.Run("pending before restart", func(t *testing.T) {
		scheduler, timers := newWakeScheduler(t)
		_, runner, store, metadata := memoryWakeFixture(t, scheduler)
		assistant := domain.Message{Role: domain.RoleAssistant, Content: "the fact is durable", ExternalTS: "2", CreatedAt: time.Now().UTC()}
		prepared, err := store.PrepareAssistantExchange(t.Context(), metadata, assistant, 10, true)
		if err != nil {
			t.Fatal(err)
		}
		// The durable outbox row is created directly through the store, not
		// through bot.Service.persistAssistantTurn: that producer path also
		// calls memoryWake, which would leave a stray buffered wake ahead of
		// Runner.Run and trigger an extra poll cycle, confusing this fixture
		// with an unrelated property of workpoll.Scheduler's coalescing.
		if err := store.MarkAssistantExchangePublished(t.Context(), prepared.ID, assistant.ExternalTS); err != nil {
			t.Fatal(err)
		}
		if err := store.FinalizeAssistantExchange(t.Context(), prepared.ID); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go runner.Run(ctx)
		waitPoll(t, timers) // initial poll must consume the pre-existing durable outbox item.
		if status, ok := memoryOutboxStatus(t, store); !ok || status != "done" {
			t.Fatalf("pre-existing outbox status after initial poll = %q, ok = %v", status, ok)
		}
	})
}
