// Package workpoll provides wake-assisted durable polling for background workers.
package workpoll

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

const recoveryCeiling = time.Minute

// Timer is the minimum timer surface needed by Scheduler.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// Options injects deterministic timer and jitter sources for tests.
type Options struct {
	NewTimer func(time.Duration) Timer
	Jitter   func(time.Duration) time.Duration
}

// Scheduler combines a coalesced local wake signal with durable recovery polls.
// Each Scheduler must have one consumer.
type Scheduler struct {
	interval time.Duration
	ceiling  time.Duration
	wake     chan struct{}
	newTimer func(time.Duration) Timer
	jitter   func(time.Duration) time.Duration
	next     time.Duration
}

type systemTimer struct{ *time.Timer }

func (t systemTimer) C() <-chan time.Time { return t.Timer.C }

// New constructs a scheduler. The first recovery delay is interval.
func New(interval time.Duration, options Options) (*Scheduler, error) {
	if interval <= 0 {
		return nil, errors.New("worker poll interval must be positive")
	}
	if options.NewTimer == nil {
		options.NewTimer = func(delay time.Duration) Timer { return systemTimer{time.NewTimer(delay)} }
	}
	if options.Jitter == nil {
		options.Jitter = func(base time.Duration) time.Duration {
			span := base / 5
			return base - span + rand.N(span+1)
		}
	}
	return &Scheduler{
		interval: interval,
		ceiling:  max(interval, recoveryCeiling),
		wake:     make(chan struct{}, 1),
		newTimer: options.NewTimer,
		jitter:   options.Jitter,
		next:     interval,
	}, nil
}

// Wake signals local work without blocking. Repeated signals coalesce.
func (s *Scheduler) Wake() {
	if s == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run polls immediately, then waits for a wake or a durable recovery poll.
func (s *Scheduler) Run(ctx context.Context, stop <-chan struct{}, poll func() bool) {
	if s == nil || poll == nil {
		return
	}
	for {
		if stopped(ctx, stop) || !s.Wait(ctx, stop, poll()) {
			return
		}
	}
}

// Wait blocks until cancellation, stop admission, a wake, or a recovery poll.
// false means the worker must stop. workFound resets idle backoff.
func (s *Scheduler) Wait(ctx context.Context, stop <-chan struct{}, workFound bool) bool {
	if s == nil {
		return false
	}
	if workFound {
		s.next = s.interval
	}
	base := s.next
	if !workFound {
		if base >= s.ceiling/2 {
			s.next = s.ceiling
		} else {
			s.next = base * 2
		}
	}
	delay := s.jitter(base)
	delay = max(base-base/5, min(delay, base))
	timer := s.newTimer(delay)
	defer timer.Stop()

	if stopped(ctx, stop) {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-stop:
		return false
	case <-s.wake:
		if stopped(ctx, stop) {
			return false
		}
		s.next = s.interval
		return true
	case <-timer.C():
		return !stopped(ctx, stop)
	}
}

func stopped(ctx context.Context, stop <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return true
	default:
	}
	if stop == nil {
		return false
	}
	select {
	case <-stop:
		return true
	default:
		return false
	}
}
