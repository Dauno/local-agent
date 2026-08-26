package resultanalysis_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
)

type wakeAnalysisStore struct{ createErr error }

func (s wakeAnalysisStore) Create(
	_ context.Context,
	identity domain.AnalysisIdentity,
	limits domain.AnalysisLimits,
	_ string,
	scope domain.ResultScope,
	workstreamID string,
	now time.Time,
) (port.AnalysisRecord, error) {
	if s.createErr != nil {
		return port.AnalysisRecord{}, s.createErr
	}
	return port.AnalysisRecord{AnalysisID: "analysis-wake", Identity: identity, Limits: limits, Scope: scope, WorkstreamID: workstreamID, State: domain.AnalysisPreparing, CreatedAt: now}, nil
}

func (wakeAnalysisStore) Get(context.Context, string, domain.ResultScope) (port.AnalysisRecord, error) {
	return port.AnalysisRecord{}, errors.New("not implemented")
}

func (wakeAnalysisStore) Complete(context.Context, string, domain.ResultScope, time.Time) (port.AnalysisRecord, error) {
	return port.AnalysisRecord{}, errors.New("not implemented")
}

func (wakeAnalysisStore) Fail(context.Context, string, domain.ResultScope, domain.AnalysisFailureCode, time.Time) (port.AnalysisRecord, error) {
	return port.AnalysisRecord{}, errors.New("not implemented")
}

func TestAnalysisCreateWakesOnlyAfterCommittedInsert(t *testing.T) {
	for _, test := range []struct {
		name      string
		createErr error
		wantWakes int
	}{
		{name: "committed", wantWakes: 1},
		{name: "failed", createErr: errors.New("write failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := testLimits()
			wakes := 0
			source := newFakeTrustedResultStore([]byte("source"), 16)
			source.identity.ResultID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			service, err := resultanalysis.NewService(resultanalysis.ServiceConfig{
				SegmentationVersion: resultanalysis.SegmenterTextV1,
				PromptVersion:       "prompt-v1",
				ModelFingerprint:    domain.AnalysisModelFingerprint("provider", "model"),
				Limits:              limits,
			}, resultanalysis.ServiceDependencies{
				Source: source, Analyses: wakeAnalysisStore{createErr: test.createErr},
				Steps: &fakeReductionStepStore{}, Evidence: newFakeEvidenceLister(), Payloads: newFakePayloadStore(),
				Wake: func() { wakes++ },
			})
			if err != nil {
				t.Fatal(err)
			}
			_, gotErr := service.RequestAnalysis(t.Context(), source.identity.ResultID, testScope(), "ws-1", "What changed?", time.Now().UTC())
			if (gotErr != nil) != (test.createErr != nil) {
				t.Fatalf("RequestAnalysis() error = %v", gotErr)
			}
			if wakes != test.wantWakes {
				t.Fatalf("analysis wake count = %d, want %d", wakes, test.wantWakes)
			}
		})
	}
}
