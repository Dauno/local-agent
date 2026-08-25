package externalagent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type fakeProgressStore struct {
	mu     sync.Mutex
	writes []domain.ExternalAgentJobProgress
	read   *domain.ExternalAgentJobProgress
	fail   bool
}

func (s *fakeProgressStore) WriteJobProgress(_ context.Context, jobID, owner string, attempt int, progress domain.ExternalAgentJobProgress) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("write failed")
	}
	progress.JobID, progress.Attempt = jobID, attempt
	s.writes = append(s.writes, progress)
	return nil
}

func (s *fakeProgressStore) ReadJobProgress(context.Context, string) (*domain.ExternalAgentJobProgress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read, nil
}

func (s *fakeProgressStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func (s *fakeProgressStore) latest() domain.ExternalAgentJobProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.writes) == 0 {
		return domain.ExternalAgentJobProgress{}
	}
	return s.writes[len(s.writes)-1]
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(value time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(value)
	c.mu.Unlock()
}

type recorderLog struct {
	mu    sync.Mutex
	warns []string
}

func (l *recorderLog) Debug(string, ...any) {}
func (l *recorderLog) Info(string, ...any)  {}
func (l *recorderLog) Warn(message string, args ...any) {
	l.mu.Lock()
	l.warns = append(l.warns, message)
	l.mu.Unlock()
}
func (l *recorderLog) Error(string, ...any) {}

func (l *recorderLog) warnings() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.warns...)
}

type fakeRegistry struct {
	alive *bool
	mu    sync.Mutex
	pids  map[string]int
}

func (r *fakeRegistry) Register(jobID string, attempt int, pid int) {
	r.mu.Lock()
	if r.pids == nil {
		r.pids = make(map[string]int)
	}
	r.pids[registryKeyForTest(jobID, attempt)] = pid
	r.mu.Unlock()
}

func (r *fakeRegistry) ProcessAlive(jobID string, attempt int) *bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, exists := r.pids[registryKeyForTest(jobID, attempt)]
	if !exists {
		return nil
	}
	return r.alive
}

func registryKeyForTest(jobID string, attempt int) string {
	return jobID
}

func newTestRecorder(store *fakeProgressStore, registry *fakeRegistry, clock *fakeClock, warnAfter time.Duration) *ProgressRecorder {
	return NewProgressRecorder(store, registry, clock, &recorderLog{}, port.NoopMetricRecorder{}, nil, warnAfter, "job_1", "owner_1", 1)
}

func TestRecorderPhaseBoundariesFlushImmediately(t *testing.T) {
	store := &fakeProgressStore{}
	clock := &fakeClock{now: time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)}
	recorder := newTestRecorder(store, &fakeRegistry{}, clock, time.Hour)
	recorder.Start(context.Background())
	defer recorder.Close()
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventProcessStarted, PID: 99})
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventSessionNew})
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventPromptSent})
	clock.advance(time.Minute)
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventToolCall, Tool: &domain.ExternalAgentToolProgress{CallID: "t1", Kind: domain.ExternalAgentToolKindExecute, Status: domain.ExternalAgentToolStatusPending}})
	// Phase boundaries persist without any 30-second interval elapsing.
	waitFor(t, func() bool { return store.latest().Phase == domain.ExternalAgentPhaseToolPending })
	if store.count() > 6 {
		t.Fatalf("phase boundaries must not cause per-event write storms; writes = %d", store.count())
	}
	latest := store.latest()
	if latest.Phase != domain.ExternalAgentPhaseToolPending || latest.LastToolCallID != "t1" {
		t.Fatalf("latest projection = %+v", latest)
	}
	if latest.PromptStartedAt.IsZero() {
		t.Fatal("prompt start must be durable")
	}
}

func TestRecorderCoalescesRepeatedChunks(t *testing.T) {
	store := &fakeProgressStore{}
	clock := &fakeClock{now: time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)}
	recorder := newTestRecorder(store, &fakeRegistry{}, clock, time.Hour)
	recorder.Start(context.Background())
	defer recorder.Close()
	for range 200 {
		clock.advance(time.Millisecond)
		recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventMessageChunk})
	}
	// Advance the fake clock past the 30-second flush interval and wake the
	// monitor with an immediate phase boundary. The repeated chunks must have
	// coalesced into a bounded number of durable writes. The first chunk also
	// flushes (phase change to responding), so wait for the tool projection.
	clock.advance(31 * time.Second)
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventToolCall, Tool: &domain.ExternalAgentToolProgress{CallID: "t1", Kind: domain.ExternalAgentToolKindExecute, Status: domain.ExternalAgentToolStatusPending}})
	waitFor(t, func() bool { return store.latest().ActiveToolCount == 1 })
	if store.count() > 5 {
		t.Fatalf("repeated chunks caused %d durable writes, want bounded coalescing", store.count())
	}
	latest := store.latest()
	if latest.LastMeaningfulProgressAt.IsZero() {
		t.Fatal("coalesced flush must preserve the newest meaningful timestamp")
	}
	if latest.ActiveToolCount != 1 {
		t.Fatalf("latest active tool count = %d, want 1", latest.ActiveToolCount)
	}
}

func TestRecorderWarningOncePerSilentEpisode(t *testing.T) {
	store := &fakeProgressStore{}
	logger := &recorderLog{}
	clock := &fakeClock{now: time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)}
	registry := &fakeRegistry{alive: new(true)}
	recorder := NewProgressRecorder(store, registry, clock, logger, port.NoopMetricRecorder{}, nil, 2*time.Second, "job_1", "owner_1", 1)
	recorder.Start(context.Background())
	defer recorder.Close()
	base := clock.Now()
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventProcessStarted, PID: 999})
	registry.Register("job_1", 1, 999)
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventPromptSent})
	// A real inbound frame establishes the transport clock.
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventThoughtChunk})
	// First silent episode: cross the threshold and let the ticker warn once.
	clock.mu.Lock()
	clock.now = base.Add(6 * time.Second)
	clock.mu.Unlock()
	waitFor(t, func() bool { return len(logger.warnings()) >= 1 })
	clock.mu.Lock()
	clock.now = base.Add(10 * time.Second)
	clock.mu.Unlock()
	time.Sleep(800 * time.Millisecond)
	if len(logger.warnings()) != 1 {
		t.Fatalf("warnings = %d, want exactly one per silent episode", len(logger.warnings()))
	}
	// New activity re-arms; the next silent episode warns again.
	clock.mu.Lock()
	clock.now = base.Add(11 * time.Second)
	clock.mu.Unlock()
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventThoughtChunk})
	time.Sleep(800 * time.Millisecond)
	clock.mu.Lock()
	clock.now = base.Add(20 * time.Second)
	clock.mu.Unlock()
	waitFor(t, func() bool { return len(logger.warnings()) >= 2 })
}

func TestRecorderFailureIsNotFatal(t *testing.T) {
	store := &fakeProgressStore{fail: true}
	logger := &recorderLog{}
	clock := &fakeClock{now: time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)}
	recorder := NewProgressRecorder(store, &fakeRegistry{}, clock, logger, port.NoopMetricRecorder{}, nil, time.Hour, "job_1", "owner_1", 1)
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventProcessStarted})
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventPromptResponse, StopReason: domain.ExternalAgentStopReasonEndTurn})
	recorder.Close()
	// The failed writes must be observable but must never panic or fail the
	// otherwise healthy invocation.
	if len(logger.warnings()) == 0 {
		t.Fatal("persist failure must be observable in logs")
	}
}

func TestRecorderTerminalFlushOnClose(t *testing.T) {
	store := &fakeProgressStore{}
	clock := &fakeClock{now: time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)}
	recorder := newTestRecorder(store, &fakeRegistry{}, clock, time.Hour)
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventPromptSent})
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventPromptResponse, StopReason: domain.ExternalAgentStopReasonEndTurn})
	recorder.Close()
	latest := store.latest()
	if latest.Phase != domain.ExternalAgentPhaseCompleted || latest.StopReason != domain.ExternalAgentStopReasonEndTurn {
		t.Fatalf("final flush lost the terminal projection: %+v", latest)
	}
}

func TestRecorderNeverBlocksReaderOnSlowSQLite(t *testing.T) {
	store := &blockingProgressStore{
		fakeProgressStore: fakeProgressStore{},
		writeStarted:      make(chan struct{}, 1),
		release:           make(chan struct{}),
	}
	clock := &fakeClock{now: time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)}
	recorder := newTestRecorder(&store.fakeProgressStore, &fakeRegistry{}, clock, time.Hour)
	recorder.store = store
	recorder.Start(context.Background())
	defer recorder.Close()
	// An immediate event wakes the monitor, which starts a write and blocks.
	recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventProcessStarted, PID: 99})
	<-store.writeStarted
	// While the durable write is stuck, the reader path must keep returning
	// without waiting on SQLite.
	started := time.Now()
	for range 200 {
		recorder.Record(domain.ExternalAgentProgressEvent{Kind: domain.ExternalAgentEventMessageChunk})
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("Record blocked on a slow durable write for %s", elapsed)
	}
	close(store.release)
}

type blockingProgressStore struct {
	fakeProgressStore
	writeStarted chan struct{}
	release      chan struct{}
}

func (s *blockingProgressStore) WriteJobProgress(ctx context.Context, jobID, owner string, attempt int, progress domain.ExternalAgentJobProgress) error {
	s.writeStarted <- struct{}{}
	<-s.release
	return s.fakeProgressStore.WriteJobProgress(ctx, jobID, owner, attempt, progress)
}

func TestDeriveProgressHealthTable(t *testing.T) {
	now := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	threshold := 10 * time.Second
	base := func() ExternalAgentJobProgressTemplate {
		return ExternalAgentJobProgressTemplate{}
	}
	_ = base
	tests := []struct {
		name     string
		proj     domain.ExternalAgentJobProgress
		alive    *bool
		terminal bool
		want     domain.ExternalAgentProgressHealth
	}{
		{
			name: "terminal phase",
			proj: domain.ExternalAgentJobProgress{Phase: domain.ExternalAgentPhaseCompleted},
			want: domain.ExternalAgentHealthTerminal,
		},
		{
			name:     "terminal durable job",
			proj:     domain.ExternalAgentJobProgress{Phase: domain.ExternalAgentPhaseToolRunning},
			terminal: true,
			want:     domain.ExternalAgentHealthTerminal,
		},
		{
			name: "failed phase without terminal response is disconnected",
			proj: domain.ExternalAgentJobProgress{Phase: domain.ExternalAgentPhaseFailed},
			want: domain.ExternalAgentHealthDisconnected,
		},
		{
			name: "failed phase with terminal stop reason is terminal",
			proj: domain.ExternalAgentJobProgress{Phase: domain.ExternalAgentPhaseFailed, StopReason: domain.ExternalAgentStopReasonMaxTokens},
			want: domain.ExternalAgentHealthTerminal,
		},
		{
			name:     "durable terminal job is terminal even when projection is failed",
			proj:     domain.ExternalAgentJobProgress{Phase: domain.ExternalAgentPhaseFailed, LastTransportActivityAt: now.Add(-time.Minute), LastMeaningfulProgressAt: now.Add(-time.Minute)},
			terminal: true,
			want:     domain.ExternalAgentHealthTerminal,
		},
		{
			name: "pre-prompt phase is active",
			proj: domain.ExternalAgentJobProgress{Phase: domain.ExternalAgentPhaseSessionReady},
			want: domain.ExternalAgentHealthActive,
		},
		{
			name:  "known dead process is disconnected",
			proj:  domain.ExternalAgentJobProgress{Phase: domain.ExternalAgentPhaseAgentProcessing, LastTransportActivityAt: now.Add(-time.Minute), LastMeaningfulProgressAt: now.Add(-time.Minute)},
			alive: new(false),
			want:  domain.ExternalAgentHealthDisconnected,
		},
		{
			name:  "recent meaningful progress is active",
			proj:  domain.ExternalAgentJobProgress{Phase: domain.ExternalAgentPhaseAgentProcessing, LastTransportActivityAt: now, LastMeaningfulProgressAt: now.Add(-2 * time.Second)},
			alive: new(true),
			want:  domain.ExternalAgentHealthActive,
		},
		{
			name:  "silent live process is possibly stalled",
			proj:  domain.ExternalAgentJobProgress{Phase: domain.ExternalAgentPhaseToolRunning, LastTransportActivityAt: now.Add(-time.Minute), LastMeaningfulProgressAt: now.Add(-time.Minute)},
			alive: new(true),
			want:  domain.ExternalAgentHealthPossiblyStalled,
		},
		{
			name:  "live process with no inbound frames ever is possibly stalled",
			proj:  domain.ExternalAgentJobProgress{Phase: domain.ExternalAgentPhaseAgentProcessing, LastMeaningfulProgressAt: now.Add(-time.Minute)},
			alive: new(true),
			want:  domain.ExternalAgentHealthPossiblyStalled,
		},
		{
			name:  "transport recent but progress stale is quiet",
			proj:  domain.ExternalAgentJobProgress{Phase: domain.ExternalAgentPhaseAgentProcessing, LastTransportActivityAt: now.Add(-2 * time.Second), LastMeaningfulProgressAt: now.Add(-time.Minute)},
			alive: new(true),
			want:  domain.ExternalAgentHealthQuiet,
		},
		{
			name:  "unknown process handle cannot claim stalled",
			proj:  domain.ExternalAgentJobProgress{Phase: domain.ExternalAgentPhaseToolRunning, LastTransportActivityAt: now.Add(-time.Minute), LastMeaningfulProgressAt: now.Add(-time.Minute)},
			alive: nil,
			want:  domain.ExternalAgentHealthQuiet,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := DeriveProgressHealth(test.proj, now, threshold, test.alive, test.terminal)
			if got != test.want {
				t.Fatalf("health = %s, want %s", got, test.want)
			}
		})
	}
}

// ExternalAgentJobProgressTemplate keeps the table test self-documenting.
type ExternalAgentJobProgressTemplate = domain.ExternalAgentJobProgress

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition was not met in time")
	}
}
