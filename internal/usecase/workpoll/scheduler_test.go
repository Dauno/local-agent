package workpoll

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeTimer struct {
	c       chan time.Time
	stopped bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }
func (t *fakeTimer) Stop() bool {
	t.stopped = true
	return true
}

type fakeTimers struct {
	mu      sync.Mutex
	delays  []time.Duration
	created chan *fakeTimer
}

func newFakeTimers() *fakeTimers { return &fakeTimers{created: make(chan *fakeTimer, 16)} }

func (f *fakeTimers) New(delay time.Duration) Timer {
	timer := &fakeTimer{c: make(chan time.Time, 1)}
	f.mu.Lock()
	f.delays = append(f.delays, delay)
	f.mu.Unlock()
	f.created <- timer
	return timer
}

func (f *fakeTimers) Delays() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.delays...)
}

func runWait(t *testing.T, scheduler *Scheduler, ctx context.Context, stop <-chan struct{}, found bool) (*fakeTimer, <-chan bool) {
	t.Helper()
	done := make(chan bool, 1)
	go func() { done <- scheduler.Wait(ctx, stop, found) }()
	return <-schedulerTimerFactory(t, scheduler).created, done
}

var schedulerTimers sync.Map

func newTestScheduler(t *testing.T, interval time.Duration, jitter func(time.Duration) time.Duration) (*Scheduler, *fakeTimers) {
	t.Helper()
	timers := newFakeTimers()
	scheduler, err := New(interval, Options{NewTimer: timers.New, Jitter: jitter})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	schedulerTimers.Store(scheduler, timers)
	t.Cleanup(func() { schedulerTimers.Delete(scheduler) })
	return scheduler, timers
}

func schedulerTimerFactory(t *testing.T, scheduler *Scheduler) *fakeTimers {
	t.Helper()
	value, ok := schedulerTimers.Load(scheduler)
	if !ok {
		t.Fatal("test scheduler timer factory is missing")
	}
	return value.(*fakeTimers)
}

func TestWakeIsNonBlockingAndCoalesced(t *testing.T) {
	scheduler, _ := newTestScheduler(t, time.Second, func(base time.Duration) time.Duration { return base })
	for range 1000 {
		scheduler.Wake()
	}
	if got := len(scheduler.wake); got != 1 {
		t.Fatalf("coalesced wake count = %d, want 1", got)
	}
	timer, done := runWait(t, scheduler, t.Context(), nil, false)
	if !<-done {
		t.Fatal("Wait() stopped after wake")
	}
	if !timer.stopped {
		t.Fatal("wake did not stop recovery timer")
	}
}

func TestRunPollsImmediatelyBeforeRecoveryWait(t *testing.T) {
	scheduler, timers := newTestScheduler(t, time.Hour, func(base time.Duration) time.Duration { return base })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	polls := 0
	go func() {
		scheduler.Run(ctx, nil, func() bool {
			polls++
			return false
		})
		close(done)
	}()
	<-timers.created
	if polls != 1 {
		t.Fatalf("startup polls = %d, want 1 before recovery timer", polls)
	}
	cancel()
	<-done
}

func TestRecoveryBackoffCeilingAndResets(t *testing.T) {
	scheduler, timers := newTestScheduler(t, 10*time.Second, func(base time.Duration) time.Duration { return base })
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second}
	for range want {
		timer, done := runWait(t, scheduler, t.Context(), nil, false)
		timer.c <- time.Time{}
		if !<-done {
			t.Fatal("recovery timer stopped worker")
		}
	}
	if got := timers.Delays(); !equalDurations(got, want) {
		t.Fatalf("recovery delays = %v, want %v", got, want)
	}

	timer, done := runWait(t, scheduler, t.Context(), nil, true)
	timer.c <- time.Time{}
	if !<-done {
		t.Fatal("work reset stopped worker")
	}
	if got := timers.Delays()[len(want)]; got != 10*time.Second {
		t.Fatalf("delay after work = %v, want 10s", got)
	}

	_, done = runWait(t, scheduler, t.Context(), nil, false)
	scheduler.Wake()
	if !<-done {
		t.Fatal("wake reset stopped worker")
	}
	timer, done = runWait(t, scheduler, t.Context(), nil, false)
	timer.c <- time.Time{}
	if !<-done {
		t.Fatal("post-wake recovery stopped worker")
	}
	if got := timers.Delays()[len(want)+2]; got != 10*time.Second {
		t.Fatalf("delay after wake = %v, want 10s", got)
	}
}

func TestJitterIsClampedToEightyThroughOneHundredPercent(t *testing.T) {
	for _, test := range []struct {
		name   string
		jitter func(time.Duration) time.Duration
		want   time.Duration
	}{
		{name: "below", jitter: func(time.Duration) time.Duration { return 0 }, want: 8 * time.Second},
		{name: "inside", jitter: func(time.Duration) time.Duration { return 9 * time.Second }, want: 9 * time.Second},
		{name: "above", jitter: func(time.Duration) time.Duration { return time.Hour }, want: 10 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheduler, timers := newTestScheduler(t, 10*time.Second, test.jitter)
			timer, done := runWait(t, scheduler, t.Context(), nil, false)
			timer.c <- time.Time{}
			<-done
			if got := timers.Delays()[0]; got != test.want {
				t.Fatalf("jittered delay = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCancellationAndStopTakePriority(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func() (context.Context, <-chan struct{})
	}{
		{name: "context", setup: func() (context.Context, <-chan struct{}) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, nil
		}},
		{name: "stop", setup: func() (context.Context, <-chan struct{}) {
			stop := make(chan struct{})
			close(stop)
			return context.Background(), stop
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheduler, _ := newTestScheduler(t, time.Second, func(base time.Duration) time.Duration { return base })
			scheduler.Wake()
			ctx, stop := test.setup()
			_, done := runWait(t, scheduler, ctx, stop, false)
			if <-done {
				t.Fatal("Wait() continued after shutdown")
			}
		})
	}
}

func TestIntervalAboveCeilingDoesNotIncrease(t *testing.T) {
	scheduler, timers := newTestScheduler(t, 90*time.Second, func(base time.Duration) time.Duration { return base })
	for range 2 {
		timer, done := runWait(t, scheduler, t.Context(), nil, false)
		timer.c <- time.Time{}
		<-done
	}
	if got, want := timers.Delays(), []time.Duration{90 * time.Second, 90 * time.Second}; !equalDurations(got, want) {
		t.Fatalf("recovery delays = %v, want %v", got, want)
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
