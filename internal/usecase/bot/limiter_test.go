package bot

import "testing"

func TestLimiterIsGlobalAndPerConversation(t *testing.T) {
	limiter := NewLimiter(2)
	releaseA, ok := limiter.TryAcquire("a")
	if !ok {
		t.Fatal("first acquisition rejected")
	}
	if _, ok := limiter.TryAcquire("a"); ok {
		t.Fatal("same conversation acquired twice")
	}
	releaseB, ok := limiter.TryAcquire("b")
	if !ok {
		t.Fatal("second global slot rejected")
	}
	if _, ok := limiter.TryAcquire("c"); ok {
		t.Fatal("global limit was not enforced")
	}
	releaseA()
	releaseA()
	if releaseC, ok := limiter.TryAcquire("c"); !ok {
		t.Fatal("released slot was not reusable")
	} else {
		releaseC()
	}
	releaseB()
}
