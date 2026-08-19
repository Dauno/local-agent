package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// TestAnalysisStoreCancellationOnRealSQLPath is the FIND-094 cancellation
// probe adopted from review, run against a real SQLite database (not a
// fake): TRD 06 broke this exact contract on the real SQL path without a
// fake-backed test noticing. Every method covered must satisfy
// errors.Is(err, context.Canceled); this also pins the errors.Join decision
// that a cancellation error keeps satisfying errors.Is(err,
// domain.ErrAnalysisUnavailable) at the same time.
func TestAnalysisStoreCancellationOnRealSQLPath(t *testing.T) {
	store, sourceID, sourceSHA := newAnalysisStoreFixture(t)
	identity := testAnalysisIdentity(sourceID, sourceSHA)
	limits := testAnalysisLimits()
	scope := testAnalysisScope()

	// Seed one real row with a live context so Get/Complete/Fail have
	// something to find before their own context is cancelled.
	created, err := store.Create(context.Background(), identity, limits, "objective", scope, "ws-1", time.Now().UTC())
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assertCanceled := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: cancelled context call unexpectedly succeeded", name)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: FIND-094 contract broken on the real SQL path: %v", name, err)
		}
		if !errors.Is(err, domain.ErrAnalysisUnavailable) {
			t.Errorf("%s: cancellation error dropped the domain sentinel: %v", name, err)
		}
	}

	_, err = store.Create(ctx, testAnalysisIdentity(sourceID, sourceSHA), limits, "objetivo acotado", scope, "ws-1", time.Now())
	assertCanceled("Create", err)

	_, err = store.Get(ctx, created.AnalysisID, scope)
	assertCanceled("Get", err)

	_, err = store.Complete(ctx, created.AnalysisID, scope, time.Now())
	assertCanceled("Complete", err)

	_, err = store.Fail(ctx, created.AnalysisID, scope, domain.AnalysisFailureWallTimeExceeded, time.Now())
	assertCanceled("Fail", err)
}

// TestAnalysisStepStoreCancellationOnRealSQLPath extends the same probe to
// the step store's ClaimNext, Retry, and List, the three methods item 1
// names explicitly.
func TestAnalysisStepStoreCancellationOnRealSQLPath(t *testing.T) {
	store, analysisID := newAnalysisStepStoreFixture(t)
	now := time.Now().UTC()
	if _, err := store.Prepare(context.Background(), stepFixture(analysisID, "leaf-0", now)); err != nil {
		t.Fatalf("seed prepare: %v", err)
	}
	claimed, ok, err := store.ClaimNext(context.Background(), analysisID, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	claim := domain.AnalysisStepClaim{AnalysisID: analysisID, StepID: claimed.StepID, Generation: claimed.Generation, LeaseUntil: claimed.LeaseUntil}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assertCanceled := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: cancelled context call unexpectedly succeeded", name)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: FIND-094 contract broken on the real SQL path: %v", name, err)
		}
		if !errors.Is(err, domain.ErrAnalysisUnavailable) {
			t.Errorf("%s: cancellation error dropped the domain sentinel: %v", name, err)
		}
	}

	_, _, err = store.ClaimNext(ctx, analysisID, now, time.Minute)
	assertCanceled("ClaimNext", err)

	err = store.Retry(ctx, claim, now.Add(time.Minute), true)
	assertCanceled("Retry", err)

	_, err = store.List(ctx, analysisID, "", 10)
	assertCanceled("List", err)
}
