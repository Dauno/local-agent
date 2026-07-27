package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/memorycurator"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestRunnerOutboxOutcomes(t *testing.T) {
	validPatch := domain.MemoryPatch{Operations: []domain.MemoryOp{{Type: domain.MemoryOpCreateTopic, TopicSlug: "fact", TopicTitle: "Fact", Content: "A fact."}}}
	for _, test := range []struct {
		name        string
		messages    bool
		trusted     bool
		claimErr    error
		loadErr     error
		trustedErr  error
		topicsErr   error
		curatorErr  error
		patch       domain.MemoryPatch
		applyErr    error
		projectErr  error
		completeErr error
		maxRetries  int
		wantAction  string
		wantApplied bool
	}{
		{name: "no claimable item", wantAction: "none"},
		{name: "claim failure", claimErr: errors.New("claim failed"), wantAction: "none"},
		{name: "source load failure", messages: true, loadErr: errors.New("load failed"), wantAction: "retry"},
		{name: "missing source messages", wantAction: "retry"},
		{name: "trusted entity lookup failure", messages: true, trusted: true, trustedErr: errors.New("trusted lookup failed"), wantAction: "retry"},
		{name: "topic lookup failure", messages: true, topicsErr: errors.New("topic lookup failed"), wantAction: "retry"},
		{name: "model saturation without trusted operations", messages: true, curatorErr: port.ErrModelCallLimitReached, wantAction: "reschedule"},
		{name: "model saturation with trusted operations", messages: true, trusted: true, curatorErr: port.ErrModelCallLimitReached, wantAction: "complete", wantApplied: true},
		{name: "curator failure without trusted operations", messages: true, curatorErr: errors.New("curator failed"), wantAction: "retry"},
		{name: "curator failure with trusted operations", messages: true, trusted: true, curatorErr: errors.New("curator failed"), wantAction: "complete", wantApplied: true},
		{name: "incomplete curator response without trusted operations", messages: true, curatorErr: port.ErrCuratorResponseIncomplete, wantAction: "complete"},
		{name: "incomplete curator response with trusted operations", messages: true, trusted: true, curatorErr: port.ErrCuratorResponseIncomplete, wantAction: "complete", wantApplied: true},
		{name: "invalid optional patch without trusted operations", messages: true, patch: domain.MemoryPatch{Operations: []domain.MemoryOp{{Type: "unknown", TopicSlug: "bad"}}}, wantAction: "complete"},
		{name: "invalid optional patch with trusted operations", messages: true, trusted: true, patch: domain.MemoryPatch{Operations: []domain.MemoryOp{{Type: "unknown", TopicSlug: "bad"}}}, wantAction: "complete", wantApplied: true},
		{name: "patch apply failure", messages: true, patch: validPatch, applyErr: errors.New("apply failed"), wantAction: "retry"},
		{name: "projection failure", messages: true, patch: validPatch, projectErr: errors.New("projection failed"), wantAction: "retry", wantApplied: true},
		{name: "completion failure", messages: true, patch: validPatch, completeErr: errors.New("completion failed"), wantAction: "none", wantApplied: true},
		{name: "maximum retry exhaustion", messages: true, patch: validPatch, applyErr: errors.New("apply failed"), maxRetries: 1, wantAction: "fail"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := newCoverageStore(t)
			if err != nil {
				t.Fatal(err)
			}
			if test.messages {
				content := "ordinary message"
				if test.trusted {
					content = "Mi nombre es Dauno y soy el creador de local-agent"
				}
				store.messages = []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: content}}
			}
			store.claimErr, store.loadErr, store.trustedErr, store.topicsErr = test.claimErr, test.loadErr, test.trustedErr, test.topicsErr
			store.applyErr, store.completeErr = test.applyErr, test.completeErr
			store.trusted = test.trusted
			if test.name != "no claimable item" {
				store.item = &domain.OutboxItem{ID: 1, ConversationKey: "slack:T12345678:dm:D12345678", ExchangeTS: "1700000000.000001", Attempts: 1, LeaseUntil: time.Now().Add(time.Minute)}
			}
			curator, err := memorycurator.New(coverageLLM{err: test.curatorErr, patch: test.patch}, memorycurator.Config{})
			if err != nil {
				t.Fatal(err)
			}
			memoryService, err := New(Config{Recall: domain.MemoryRecallConfig{Enabled: true, MaxTopics: 1, MaxChars: 100}, Limits: domain.MemoryLimits{MaxTopics: 2, MaxLinks: 1, MaxTopicChars: 100}, MaxPatchOps: 2}, Dependencies{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			projector := &coverageProjector{err: test.projectErr, failures: -1}
			maxRetries := test.maxRetries
			if maxRetries == 0 {
				maxRetries = 3
			}
			runner, err := NewRunner(RunnerConfig{Interval: time.Hour, MaxRetries: maxRetries, MemoryDir: t.TempDir()}, RunnerDependencies{Store: store, ExchangeFinder: runnerFinder{}, Curator: curator, Memory: memoryService, Projector: projector, ProjectionReader: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			runner.processOutbox(t.Context())
			if got := store.action(); got != test.wantAction {
				t.Fatalf("action = %q, want %q", got, test.wantAction)
			}
			if store.applied != test.wantApplied {
				t.Fatalf("applied = %v, want %v", store.applied, test.wantApplied)
			}
			if test.trusted && test.wantApplied && len(store.lastPatch.Operations) == 0 {
				t.Fatal("trusted operations were not included in applied patch")
			}
			if test.trusted && test.wantApplied {
				ownerKey := domain.SlackOwnerKey(store.itemKey(), "U12345678")
				expectedSlug := domain.ScopedPersonTopicSlug("person-dauno", ownerKey)
				expected := domain.TrustedEntityMemoryOperations(store.messages, []domain.TopicReference{{Slug: expectedSlug, Revision: 1}}, ownerKey)
				if len(store.lastPatch.Operations) < len(expected) || !reflect.DeepEqual(store.lastPatch.Operations[:len(expected)], expected) {
					t.Fatalf("trusted operations = %#v, want prefix %#v", store.lastPatch.Operations, expected)
				}
			}
			for _, operation := range store.lastPatch.Operations {
				if operation.Type == "unknown" {
					t.Fatal("invalid optional curator operation survived validation")
				}
			}
		})
	}
}

func TestRunnerProjectionFailureIsSafelyRetried(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	baseStore, err := adaptersqlite.Initialize(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = baseStore.Close() })
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	metadata := domain.ConversationMetadata{Key: key, TeamID: "T12345678", ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "2"}
	if err := baseStore.AppendMessage(t.Context(), metadata, domain.Message{Role: domain.RoleUser, UserID: "U12345678", Content: "ordinary message", ExternalTS: "1", CreatedAt: time.Now().UTC()}, 10); err != nil {
		t.Fatal(err)
	}
	prepared, err := baseStore.PrepareAssistantExchange(t.Context(), metadata, domain.Message{Role: domain.RoleAssistant, Content: "assistant answer", ExternalTS: "2", CreatedAt: time.Now().UTC()}, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := baseStore.MarkAssistantExchangePublished(t.Context(), prepared.ID, "2"); err != nil {
		t.Fatal(err)
	}
	if err := baseStore.FinalizeAssistantExchange(t.Context(), prepared.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := baseStore.CreateTopic(t.Context(), "fact", "Fact", "", nil, "old fact", "seed"); err != nil {
		t.Fatal(err)
	}
	store := &coverageStore{Store: baseStore, realOutbox: true, persistApply: true, dbPath: dbPath}
	curator, err := memorycurator.New(coverageLLM{patch: domain.MemoryPatch{Operations: []domain.MemoryOp{{Type: domain.MemoryOpRevise, TopicSlug: "fact", ExpectedRev: 1, Content: "updated fact"}}}}, memorycurator.Config{})
	if err != nil {
		t.Fatal(err)
	}
	memoryService, err := New(Config{Recall: domain.MemoryRecallConfig{Enabled: true, MaxTopics: 1, MaxChars: 100}, Limits: domain.MemoryLimits{MaxTopics: 2, MaxLinks: 1, MaxTopicChars: 100}, MaxPatchOps: 2}, Dependencies{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	projector := &coverageProjector{err: errors.New("projection failed"), failures: 1}
	runner, err := NewRunner(RunnerConfig{Interval: time.Hour, MaxRetries: 3, MemoryDir: t.TempDir()}, RunnerDependencies{Store: store, ExchangeFinder: runnerFinder{}, Curator: curator, Memory: memoryService, Projector: projector, ProjectionReader: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	runner.processOutbox(t.Context())
	if !store.retried || store.completed {
		t.Fatalf("first projection attempt: retried=%v completed=%v", store.retried, store.completed)
	}
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
	if status != "pending" || attempts != 1 {
		t.Fatalf("retry persistence: status=%q attempts=%d", status, attempts)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE memory_outbox SET next_attempt = 0`); err != nil {
		t.Fatal(err)
	}
	runner.processOutbox(t.Context())
	if !store.completed || store.applyCalls != 2 || projector.calls != 2 {
		t.Fatalf("retry state: completed=%v apply_calls=%d projector_calls=%d", store.completed, store.applyCalls, projector.calls)
	}
	topic, err := store.GetTopic(t.Context(), "fact")
	if err != nil {
		t.Fatal(err)
	}
	revisions, err := store.ListRevisions(t.Context(), topic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 2 {
		t.Fatalf("safe retry revisions = %#v", revisions)
	}
	var receipts int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM memory_patch_receipts`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || store.lastApplied {
		t.Fatalf("receipts=%d lastApplied=%v, want one receipt and replay false", receipts, store.lastApplied)
	}
}

type coverageStore struct {
	*adaptersqlite.Store
	item                                     *domain.OutboxItem
	messages                                 []domain.Message
	claimErr, loadErr, trustedErr, topicsErr error
	applyErr, completeErr                    error
	trusted, applied                         bool
	completed, retried, rescheduled, failed  bool
	projectorCalled                          bool
	retryRequeues                            bool
	persistApply                             bool
	realOutbox                               bool
	dbPath                                   string
	lastPatch                                domain.MemoryPatch
	applyCalls                               int
	lastApplied                              bool
}

func newCoverageStore(t *testing.T) (*coverageStore, error) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = store.Close() })
	return &coverageStore{Store: store}, nil
}

func (s *coverageStore) ClaimNextOutboxItem(ctx context.Context) (*domain.OutboxItem, error) {
	if s.realOutbox {
		item, err := s.Store.ClaimNextOutboxItem(ctx)
		s.item = item
		return item, err
	}
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	if s.item == nil {
		return nil, nil
	}
	item := s.item
	s.item = nil
	return item, nil
}
func (s *coverageStore) LoadOutboxMessages(ctx context.Context, item *domain.OutboxItem) ([]domain.Message, error) {
	if s.realOutbox {
		return s.Store.LoadOutboxMessages(ctx, item)
	}
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return s.messages, nil
}
func (s *coverageStore) CompleteOutboxItem(ctx context.Context, id int, leaseUntil time.Time) error {
	if s.realOutbox {
		err := s.Store.CompleteOutboxItem(ctx, id, leaseUntil)
		if err == nil {
			s.completed = true
		}
		return err
	}
	if s.completeErr != nil {
		return s.completeErr
	}
	s.completed = true
	return nil
}
func (s *coverageStore) FailOutboxItem(context.Context, int, time.Time, string) error {
	s.failed = true
	return nil
}
func (s *coverageStore) RetryOutboxItem(ctx context.Context, id int, leaseUntil, nextAttempt time.Time) error {
	s.retried = true
	if s.realOutbox {
		return s.Store.RetryOutboxItem(ctx, id, leaseUntil, nextAttempt)
	}
	if s.retryRequeues && s.item == nil {
		s.item = &domain.OutboxItem{ID: 1, ConversationKey: "slack:T12345678:dm:D12345678", ExchangeTS: "1700000000.000001", Attempts: 2, LeaseUntil: time.Now().Add(time.Minute)}
	}
	return nil
}
func (s *coverageStore) RescheduleOutboxItem(context.Context, int, time.Time, time.Time) error {
	s.rescheduled = true
	return nil
}
func (s *coverageStore) GetTopicReference(_ context.Context, slug string) (*domain.TopicReference, error) {
	if s.trustedErr != nil {
		return nil, s.trustedErr
	}
	if s.trusted {
		return &domain.TopicReference{Slug: slug, Revision: 1}, nil
	}
	return nil, nil
}
func (s *coverageStore) SearchTopicReferences(context.Context, string, int) ([]domain.TopicReference, error) {
	return nil, s.topicsErr
}
func (s *coverageStore) TopicExistsBySlug(ctx context.Context, slug string) (bool, error) {
	if !s.persistApply {
		return false, nil
	}
	return s.Store.TopicExistsBySlug(ctx, slug)
}
func (s *coverageStore) ApplyMemoryPatch(ctx context.Context, patch domain.MemoryPatch, limits domain.MemoryLimits) (bool, error) {
	s.lastPatch = patch
	s.applyCalls++
	if s.applyErr != nil {
		return false, s.applyErr
	}
	if !s.persistApply {
		s.applied = true
		return true, nil
	}
	applied, err := s.Store.ApplyMemoryPatch(ctx, patch, limits)
	s.applied = applied
	s.lastApplied = applied
	return applied, err
}
func (s *coverageStore) itemKey() domain.ConversationKey {
	return "slack:T12345678:dm:D12345678"
}
func (s *coverageStore) action() string {
	switch {
	case s.failed:
		return "fail"
	case s.rescheduled:
		return "reschedule"
	case s.retried:
		return "retry"
	case s.completed:
		return "complete"
	default:
		return "none"
	}
}

type coverageLLM struct {
	err   error
	patch domain.MemoryPatch
}

func (c coverageLLM) GenerateText(context.Context, string) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	operations := make([]map[string]any, 0, len(c.patch.Operations))
	for _, op := range c.patch.Operations {
		operations = append(operations, map[string]any{
			"type": op.Type, "topic_slug": op.TopicSlug, "topic_title": op.TopicTitle,
			"content": op.Content, "change_reason": op.ChangeReason, "expected_rev": op.ExpectedRev,
		})
	}
	data, _ := json.Marshal(map[string]any{"operations": operations})
	return string(data), nil
}

type coverageProjector struct {
	err      error
	failures int
	calls    int
}

func (p *coverageProjector) Project(context.Context, port.ProjectionReader, string) error {
	p.calls++
	if p.failures < 0 {
		return p.err
	}
	if p.failures > 0 {
		p.failures--
		return p.err
	}
	return nil
}
