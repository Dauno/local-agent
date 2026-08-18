package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/memorycurator"
	"github.com/Dauno/slack-local-agent/internal/adapter/memoryprojector"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestRunnerReschedulesModelSaturationWithoutUsingRetries(t *testing.T) {
	store, key, _ := prepareRunnerExchange(t, "record a durable fact", "the fact is durable")
	shared := modelcalllimiter.New(1)
	release, acquired := shared.TryAcquire()
	if !acquired {
		t.Fatal("failed to occupy shared model permit")
	}
	llm := &runnerTestLLM{}
	curator, err := memorycurator.New(llm, memorycurator.Config{ModelCalls: shared})
	if err != nil {
		t.Fatal(err)
	}
	memoryService := newRunnerMemoryService(t, store, 1)

	for range 3 {
		runner := newTestRunner(t, store, curator, memoryService, 1)
		runner.processOutbox(t.Context())
		item, err := store.ClaimNextOutboxItem(t.Context())
		if err != nil || item == nil || item.Attempts != 1 {
			t.Fatalf("saturated item = %#v, %v", item, err)
		}
		if err := store.RescheduleOutboxItem(t.Context(), item.ID, item.LeaseUntil, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if llm.calls != 0 {
		t.Fatalf("curator calls while saturated = %d", llm.calls)
	}
	release()
	newTestRunner(t, store, curator, memoryService, 1).processOutbox(t.Context())
	if llm.calls != 1 {
		t.Fatalf("curator calls after permit release = %d, want 1", llm.calls)
	}
	if topics, err := store.ListTopics(t.Context()); err != nil || len(topics) != 1 {
		t.Fatalf("successful topics = %#v, %v", topics, err)
	}
	_ = key
}

func TestRunnerAppliesTrustedEntityOperationsWhenCuratorFails(t *testing.T) {
	for _, test := range []struct {
		name     string
		response string
		err      error
	}{
		{name: "LLM call failure", err: errors.New("model unavailable")},
		{name: "curator parse failure", response: "not JSON"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, key, _ := prepareRunnerExchange(t, "Mi nombre es Dauno y soy el creador de local-agent", "noted")
			curator, err := memorycurator.New(runnerFailingLLM{response: test.response, err: test.err}, memorycurator.Config{})
			if err != nil {
				t.Fatal(err)
			}
			memoryService := newRunnerMemoryService(t, store, 2)
			newTestRunner(t, store, curator, memoryService, 1).processOutbox(t.Context())
			topic, err := store.GetTopic(t.Context(), domain.ScopedPersonTopicSlug("person-dauno", domain.SlackOwnerKey(key, "U12345678")))
			if err != nil || topic.CurrentRev != 1 {
				t.Fatalf("trusted topic = %#v, %v", topic, err)
			}
			if item, err := store.ClaimNextOutboxItem(t.Context()); err != nil || item != nil {
				t.Fatalf("failed curator left exchange pending: %#v, %v", item, err)
			}
		})
	}
}

func TestRunnerDiscardsInvalidOptionalPatchWithoutRetry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store, err := adaptersqlite.Initialize(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	key, metadata := domain.ConversationKey("slack:T12345678:dm:D12345678"), domain.ConversationMetadata{}
	metadata = domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "1"}
	if err := store.AppendMessage(t.Context(), metadata, domain.Message{Role: domain.RoleUser, Content: "describe the sandbox", UserID: "U12345678", ExternalTS: "1", CreatedAt: time.Now().UTC()}, 10); err != nil {
		t.Fatal(err)
	}
	assistant := domain.Message{Role: domain.RoleAssistant, Content: "sandbox description", ExternalTS: "2", CreatedAt: time.Now().UTC()}
	prepared, err := store.PrepareAssistantExchange(t.Context(), metadata, assistant, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAssistantExchangePublished(t.Context(), prepared.ID, assistant.ExternalTS); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeAssistantExchange(t.Context(), prepared.ID); err != nil {
		t.Fatal(err)
	}
	llm := &runnerTestLLM{response: `{"operations":[{"type":"create_topic","topic_slug":"sandbox","topic_title":"Sandbox","content":"Read every file."}]}`}
	curator, err := memorycurator.New(llm, memorycurator.Config{})
	if err != nil {
		t.Fatal(err)
	}
	memoryService := newRunnerMemoryService(t, store, 1)
	newTestRunner(t, store, curator, memoryService, 3).processOutbox(t.Context())
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var status string
	var attempts int
	if err := db.QueryRowContext(t.Context(), `SELECT status, attempts FROM memory_outbox LIMIT 1`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "done" || attempts != 1 {
		t.Fatalf("outbox = status %q, attempts %d", status, attempts)
	}
	if topics, err := store.ListTopics(t.Context()); err != nil || len(topics) != 0 {
		t.Fatalf("discarded patch topics = %#v, %v", topics, err)
	}
}

func prepareRunnerExchange(t *testing.T, userContent, assistantContent string) (*adaptersqlite.Store, domain.ConversationKey, domain.ConversationMetadata) {
	store, key, metadata, _ := prepareRunnerExchangeWithPath(t, userContent, assistantContent)
	return store, key, metadata
}

func prepareRunnerExchangeWithPath(t *testing.T, userContent, assistantContent string) (*adaptersqlite.Store, domain.ConversationKey, domain.ConversationMetadata, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store, err := adaptersqlite.Initialize(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "2"}
	if err := store.AppendMessage(t.Context(), metadata, domain.Message{Role: domain.RoleUser, Content: userContent, UserID: "U12345678", ExternalTS: "1", CreatedAt: time.Now().UTC()}, 10); err != nil {
		t.Fatal(err)
	}
	assistant := domain.Message{Role: domain.RoleAssistant, Content: assistantContent, ExternalTS: "2", CreatedAt: time.Now().UTC()}
	prepared, err := store.PrepareAssistantExchange(t.Context(), metadata, assistant, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAssistantExchangePublished(t.Context(), prepared.ID, assistant.ExternalTS); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeAssistantExchange(t.Context(), prepared.ID); err != nil {
		t.Fatal(err)
	}
	return store, key, metadata, dbPath
}

func newRunnerMemoryService(t *testing.T, store *adaptersqlite.Store, maxOps int) *Service {
	t.Helper()
	service, err := New(Config{Recall: domain.MemoryRecallConfig{Enabled: true, MaxTopics: 1, MaxChars: 100}, Limits: domain.MemoryLimits{MaxTopics: 2, MaxLinks: 1, MaxTopicChars: 100}, MaxPatchOps: maxOps}, Dependencies{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func newTestRunner(t *testing.T, store *adaptersqlite.Store, curator port.MemoryCurator, service *Service, maxRetries int) *Runner {
	t.Helper()
	runner, err := NewRunner(RunnerConfig{Interval: time.Hour, MaxRetries: maxRetries, MemoryDir: t.TempDir()}, RunnerDependencies{Store: store, ExchangeFinder: runnerFinder{}, Curator: curator, Memory: service, Projector: memoryprojector.New(), ProjectionReader: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

type runnerFinder struct{}

func (runnerFinder) FindPublishedAssistantExchange(context.Context, port.AssistantExchangeIntent) (string, bool, error) {
	return "", false, nil
}

type runnerTestLLM struct {
	calls    int
	response string
}

func (l *runnerTestLLM) GenerateText(context.Context, string) (string, error) {
	l.calls++
	if l.response != "" {
		return l.response, nil
	}
	return `{"operations":[{"type":"create_topic","topic_slug":"durable-fact","topic_title":"Durable fact","content":"A durable fact."}]}`, nil
}

type runnerFailingLLM struct {
	response string
	err      error
}

func (l runnerFailingLLM) GenerateText(context.Context, string) (string, error) {
	return l.response, l.err
}

func TestRunnerCancellationDuringPanicBackoffStopsWithoutRestart(t *testing.T) {
	panicValue := "model-secret-panic"
	store := &panicWorkerStore{panicValue: panicValue, panicked: make(chan struct{})}
	logger := &runnerTestLogger{}
	runner, err := NewRunner(RunnerConfig{Interval: time.Millisecond, MaxRetries: 1, MemoryDir: t.TempDir()}, RunnerDependencies{
		Store: store, ExchangeFinder: runnerFinder{}, Curator: runnerNoopCurator{}, Memory: &Service{},
		Projector: runnerProjector{}, ProjectionReader: runnerSnapshotReader{}, Logger: logger,
		Sanitize: func(value string) string { return strings.ReplaceAll(value, panicValue, "[REDACTED]") },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()
	select {
	case <-store.panicked:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("runner did not enter panic recovery")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not stop during panic backoff")
	}
	if strings.Contains(logger.errorText(), panicValue) || !strings.Contains(logger.errorText(), "[REDACTED]") {
		t.Fatalf("panic log was not redacted: %q", logger.errorText())
	}
}

func TestRunnerCancellationDuringNormalOperationStopsPromptly(t *testing.T) {
	store := &panicWorkerStore{panicked: make(chan struct{})}
	runner, err := NewRunner(RunnerConfig{Interval: time.Hour, MaxRetries: 1, MemoryDir: t.TempDir()}, RunnerDependencies{
		Store: store, ExchangeFinder: runnerFinder{}, Curator: runnerNoopCurator{}, Memory: &Service{},
		Projector: runnerProjector{}, ProjectionReader: runnerSnapshotReader{}, Logger: &runnerTestLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not stop during normal operation")
	}
}

func TestRunnerRestartsWorkerAfterPanic(t *testing.T) {
	store := &restartWorkerStore{restarted: make(chan struct{})}
	runner, err := NewRunner(RunnerConfig{Interval: time.Millisecond, MaxRetries: 1, MemoryDir: t.TempDir()}, RunnerDependencies{
		Store: store, ExchangeFinder: runnerFinder{}, Curator: runnerNoopCurator{}, Memory: &Service{},
		Projector: runnerProjector{}, ProjectionReader: runnerSnapshotReader{}, Logger: &runnerTestLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()
	select {
	case <-store.restarted:
		cancel()
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("runner did not restart worker after panic")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("restarted runner did not stop after cancellation")
	}
}

func TestNextPanicBackoffIsBounded(t *testing.T) {
	for _, test := range []struct {
		current time.Duration
		want    time.Duration
	}{
		{current: time.Second, want: 2 * time.Second},
		{current: 16 * time.Second, want: maxPanicBackoff},
		{current: maxPanicBackoff, want: maxPanicBackoff},
	} {
		if got := nextPanicBackoff(test.current); got != test.want {
			t.Errorf("nextPanicBackoff(%s) = %s, want %s", test.current, got, test.want)
		}
	}
}

type panicWorkerStore struct {
	panicValue string
	panicked   chan struct{}
	once       sync.Once
}

func (s *panicWorkerStore) ReconcileAssistantExchanges(context.Context, port.AssistantExchangeFinder) error {
	s.once.Do(func() { close(s.panicked) })
	panic(s.panicValue)
}
func (*panicWorkerStore) ClaimNextOutboxItem(context.Context) (*domain.OutboxItem, error) {
	return nil, nil
}
func (*panicWorkerStore) LoadOutboxMessages(context.Context, *domain.OutboxItem) ([]domain.Message, error) {
	return nil, nil
}
func (*panicWorkerStore) CompleteOutboxItem(context.Context, int, time.Time) error     { return nil }
func (*panicWorkerStore) FailOutboxItem(context.Context, int, time.Time, string) error { return nil }
func (*panicWorkerStore) RetryOutboxItem(context.Context, int, time.Time, time.Time) error {
	return nil
}
func (*panicWorkerStore) RescheduleOutboxItem(context.Context, int, time.Time, time.Time) error {
	return nil
}
func (*panicWorkerStore) CleanupOutbox(context.Context, time.Time) error { return nil }

type restartWorkerStore struct {
	mu        sync.Mutex
	calls     int
	restarted chan struct{}
}

func (s *restartWorkerStore) ReconcileAssistantExchanges(ctx context.Context, _ port.AssistantExchangeFinder) error {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 {
		panic("restart worker")
	}
	close(s.restarted)
	<-ctx.Done()
	return ctx.Err()
}
func (*restartWorkerStore) ClaimNextOutboxItem(context.Context) (*domain.OutboxItem, error) {
	return nil, nil
}
func (*restartWorkerStore) LoadOutboxMessages(context.Context, *domain.OutboxItem) ([]domain.Message, error) {
	return nil, nil
}
func (*restartWorkerStore) CompleteOutboxItem(context.Context, int, time.Time) error { return nil }
func (*restartWorkerStore) FailOutboxItem(context.Context, int, time.Time, string) error {
	return nil
}
func (*restartWorkerStore) RetryOutboxItem(context.Context, int, time.Time, time.Time) error {
	return nil
}
func (*restartWorkerStore) RescheduleOutboxItem(context.Context, int, time.Time, time.Time) error {
	return nil
}
func (*restartWorkerStore) CleanupOutbox(context.Context, time.Time) error { return nil }

type runnerNoopCurator struct{}

func (runnerNoopCurator) ProposePatch(context.Context, domain.ConversationKey, string, []domain.Message, []domain.TopicReference) (domain.MemoryPatch, error) {
	return domain.MemoryPatch{}, nil
}

type cleanupFailingProjector struct {
	inner        port.OKFProjector
	mu           sync.Mutex
	failures     int
	projectCalls int
	recoverCalls int
}

func (p *cleanupFailingProjector) Project(ctx context.Context, reader port.ProjectionReader, dir string) error {
	p.mu.Lock()
	p.projectCalls++
	fail := p.failures > 0
	if fail {
		p.failures--
	}
	p.mu.Unlock()
	if fail {
		return port.ErrProjectionCleanup
	}
	return p.inner.Project(ctx, reader, dir)
}

func (p *cleanupFailingProjector) Recover(dir string) error {
	p.mu.Lock()
	p.recoverCalls++
	p.mu.Unlock()
	return p.inner.Recover(dir)
}

func (p *cleanupFailingProjector) counts() (projectCalls, recoverCalls int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.projectCalls, p.recoverCalls
}

func runnerOutboxState(t *testing.T, dbPath string) (status string, attempts int) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.QueryRowContext(t.Context(), `SELECT status, attempts FROM memory_outbox LIMIT 1`).Scan(&status, &attempts); err != nil {
		t.Fatalf("outbox state query: %v", err)
	}
	return status, attempts
}

func TestRunnerCleanupFailureReschedulesWithoutConsumingRetries(t *testing.T) {
	store, _, _, dbPath := prepareRunnerExchangeWithPath(t, "record a durable fact", "the fact is durable")
	service := newRunnerMemoryService(t, store, 2)
	// Fault-inject the real projector: backup removal fails five times, so
	// the first promotion publishes the live bundle but leaves a residue
	// backup, and every retry re-encounters that residue through the
	// projector's internal recovery instead of a pre-return fake error.
	backupRemovals := 0
	projector := memoryprojector.NewWithFaults(os.Rename, func(path string) error {
		if strings.Contains(path, ".okf-backup-") {
			if _, err := os.Lstat(path); err == nil {
				backupRemovals++
				if backupRemovals <= 5 {
					return errors.New("injected backup removal failure")
				}
			}
		}
		return os.RemoveAll(path)
	})
	runner, err := NewRunner(RunnerConfig{Interval: time.Hour, MaxRetries: 1, MemoryDir: t.TempDir()}, RunnerDependencies{
		Store: store, ExchangeFinder: runnerFinder{}, Curator: runnerNoopCurator{}, Memory: service,
		Projector: projector, ProjectionReader: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	// MaxRetries=1 would fail the item terminal on the very first failure
	// under the normal path. Four cleanup failures must neither fail the
	// item nor consume the retry budget: it stays pending, attempt-neutral.
	for i := 0; i < 4; i++ {
		runner.processOutbox(t.Context())
		if status, attempts := runnerOutboxState(t, dbPath); status != "pending" || attempts != 0 {
			t.Fatalf("cleanup failure %d: outbox = status %q attempts %d; want pending with budget restored", i, status, attempts)
		}
	}
	if backupRemovals == 0 {
		t.Fatal("fault injection never exercised backup removal")
	}
	// Once the backup can be removed, the same item completes.
	runner.processOutbox(t.Context())
	if status, attempts := runnerOutboxState(t, dbPath); status != "done" || attempts != 1 {
		t.Fatalf("recovered item: outbox = status %q attempts %d; want done at attempts 1", status, attempts)
	}
}

func TestRunnerStagingCleanupFailureReschedulesWithoutConsumingRetries(t *testing.T) {
	store, _, _, dbPath := prepareRunnerExchangeWithPath(t, "record a durable fact", "the fact is durable")
	service := newRunnerMemoryService(t, store, 2)
	// Fault-inject the real projector: the first promotion fails (rollback
	// succeeds), leaving a staging residue whose removal fails five times.
	// Each retry re-encounters the residue through the projector's
	// internal recovery, and the joined error stays typed.
	promoteFailures := 1
	stagingRemovals := 0
	projector := memoryprojector.NewWithFaults(
		func(oldpath, newpath string) error {
			if promoteFailures > 0 && strings.Contains(oldpath, ".okf-staging-") {
				promoteFailures--
				return errors.New("injected promotion failure")
			}
			return os.Rename(oldpath, newpath)
		},
		func(path string) error {
			if strings.Contains(path, ".okf-staging-") {
				if _, err := os.Lstat(path); err == nil {
					stagingRemovals++
					if stagingRemovals <= 5 {
						return errors.New("injected staging removal failure")
					}
				}
			}
			return os.RemoveAll(path)
		},
	)
	runner, err := NewRunner(RunnerConfig{Interval: time.Hour, MaxRetries: 1, MemoryDir: t.TempDir()}, RunnerDependencies{
		Store: store, ExchangeFinder: runnerFinder{}, Curator: runnerNoopCurator{}, Memory: service,
		Projector: projector, ProjectionReader: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	// MaxRetries=1 would fail the item terminal on the very first failure
	// under the normal path. Five staging cleanup failures must neither
	// fail the item nor consume the retry budget.
	for i := 0; i < 5; i++ {
		runner.processOutbox(t.Context())
		if status, attempts := runnerOutboxState(t, dbPath); status != "pending" || attempts != 0 {
			t.Fatalf("staging cleanup failure %d: outbox = status %q attempts %d; want pending with budget restored", i, status, attempts)
		}
	}
	if stagingRemovals == 0 {
		t.Fatal("fault injection never exercised staging removal")
	}
	// Once the staging residue can be removed, the same item completes.
	runner.processOutbox(t.Context())
	if status, attempts := runnerOutboxState(t, dbPath); status != "done" || attempts != 1 {
		t.Fatalf("recovered item: outbox = status %q attempts %d; want done at attempts 1", status, attempts)
	}
}

func TestRunnerRollbackStagingFailureReschedulesWithoutConsumingRetries(t *testing.T) {
	store, _, _, dbPath := prepareRunnerExchangeWithPath(t, "record a durable fact", "the fact is durable")
	service := newRunnerMemoryService(t, store, 2)
	// Fault-inject the real projector: the first promotion fails and its
	// rollback fails too, leaving the previous bundle only at the backup
	// path and a staging residue whose removal fails five times. Each
	// retry re-encounters the residue through internal recovery and the
	// combined error stays typed.
	renameFailures := 2 // one promotion failure, then one rollback failure
	stagingRemovals := 0
	projector := memoryprojector.NewWithFaults(
		func(oldpath, newpath string) error {
			if renameFailures > 0 && strings.Contains(oldpath, ".okf-") {
				renameFailures--
				if strings.Contains(oldpath, ".okf-staging-") {
					return errors.New("injected promotion failure")
				}
				return errors.New("injected rollback failure")
			}
			return os.Rename(oldpath, newpath)
		},
		func(path string) error {
			if strings.Contains(path, ".okf-staging-") {
				if _, err := os.Lstat(path); err == nil {
					stagingRemovals++
					if stagingRemovals <= 5 {
						return errors.New("injected staging removal failure")
					}
				}
			}
			return os.RemoveAll(path)
		},
	)
	runner, err := NewRunner(RunnerConfig{Interval: time.Hour, MaxRetries: 1, MemoryDir: t.TempDir()}, RunnerDependencies{
		Store: store, ExchangeFinder: runnerFinder{}, Curator: runnerNoopCurator{}, Memory: service,
		Projector: projector, ProjectionReader: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	// MaxRetries=1 would fail the item terminal on the very first failure
	// under the normal path. Five residue cleanup failures after the
	// promotion+rollback failure must neither fail the item nor consume
	// the retry budget.
	for i := 0; i < 5; i++ {
		runner.processOutbox(t.Context())
		if status, attempts := runnerOutboxState(t, dbPath); status != "pending" || attempts != 0 {
			t.Fatalf("rollback+staging cleanup failure %d: outbox = status %q attempts %d; want pending with budget restored", i, status, attempts)
		}
	}
	if stagingRemovals == 0 {
		t.Fatal("fault injection never exercised staging removal")
	}
	// Once the staging residue can be removed, the previous bundle is
	// restored and the same item completes.
	runner.processOutbox(t.Context())
	if status, attempts := runnerOutboxState(t, dbPath); status != "done" || attempts != 1 {
		t.Fatalf("recovered item: outbox = status %q attempts %d; want done at attempts 1", status, attempts)
	}
}

func TestRunnerRecoversResidueAtStartupWithoutPendingOutbox(t *testing.T) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	projector := &cleanupFailingProjector{inner: memoryprojector.New()}
	runner, err := NewRunner(RunnerConfig{Interval: time.Hour, MaxRetries: 1, MemoryDir: t.TempDir()}, RunnerDependencies{
		Store: store, ExchangeFinder: runnerFinder{}, Curator: runnerNoopCurator{}, Memory: &Service{},
		Projector: projector, ProjectionReader: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, recoverCalls := projector.counts(); recoverCalls >= 1 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("startup recovery never ran with an empty outbox")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not stop after cancellation")
	}
}

type runnerProjector struct{}

func (runnerProjector) Project(context.Context, port.ProjectionReader, string) error { return nil }
func (runnerProjector) Recover(string) error                                         { return nil }

type runnerSnapshotReader struct{}

func (runnerSnapshotReader) ReadProjectionSnapshot(context.Context) (port.ProjectionSnapshot, error) {
	return port.ProjectionSnapshot{}, nil
}

type runnerTestLogger struct {
	mu    sync.Mutex
	error string
}

func (l *runnerTestLogger) Debug(string, ...any) {}
func (l *runnerTestLogger) Info(string, ...any)  {}
func (l *runnerTestLogger) Warn(string, ...any)  {}
func (l *runnerTestLogger) Error(message string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.error = message + " " + fmt.Sprint(args...)
}
func (l *runnerTestLogger) errorText() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.error
}
