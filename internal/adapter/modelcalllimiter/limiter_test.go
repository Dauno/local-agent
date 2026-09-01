package modelcalllimiter

import "testing"

func TestTryAcquireHonorsCapacityAndReleasesOnce(t *testing.T) {
	limiter := New(1)
	release, acquired := limiter.TryAcquire()
	if !acquired || release == nil {
		t.Fatal("first permit was not acquired")
	}
	if blockedRelease, blocked := limiter.TryAcquire(); blocked || blockedRelease != nil {
		t.Fatal("second permit exceeded the configured capacity")
	}

	release()
	release()
	if nextRelease, acquired := limiter.TryAcquire(); !acquired || nextRelease == nil {
		t.Fatal("permit was not returned after release")
	} else {
		nextRelease()
	}
}

func TestZeroCapacityNeverAcquires(t *testing.T) {
	limiter := New(0)
	if release, acquired := limiter.TryAcquire(); acquired || release != nil {
		t.Fatal("zero-capacity limiter acquired a permit")
	}
}
