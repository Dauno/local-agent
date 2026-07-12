// Package modelcalllimiter provides process-wide non-blocking model backpressure.
package modelcalllimiter

import (
	"sync"

	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.ModelCallLimiter = (*Limiter)(nil)

// Limiter shares a fixed pool of model-call permits across all consumers.
type Limiter struct {
	permits chan struct{}
}

func New(maxConcurrent int) *Limiter {
	return &Limiter{permits: make(chan struct{}, maxConcurrent)}
}

func (l *Limiter) TryAcquire() (func(), bool) {
	select {
	case l.permits <- struct{}{}:
	default:
		return nil, false
	}

	var once sync.Once
	return func() {
		once.Do(func() { <-l.permits })
	}, true
}
