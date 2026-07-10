package bot

import "sync"

type Limiter struct {
	mu     sync.Mutex
	max    int
	active int
	byKey  map[string]struct{}
}

func NewLimiter(maxConcurrent int) *Limiter {
	return &Limiter{max: maxConcurrent, byKey: make(map[string]struct{})}
}

func (l *Limiter) TryAcquire(key string) (func(), bool) {
	l.mu.Lock()
	if l.max <= 0 || l.active >= l.max {
		l.mu.Unlock()
		return nil, false
	}
	if _, exists := l.byKey[key]; exists {
		l.mu.Unlock()
		return nil, false
	}
	l.active++
	l.byKey[key] = struct{}{}
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.active--
			delete(l.byKey, key)
			l.mu.Unlock()
		})
	}, true
}
