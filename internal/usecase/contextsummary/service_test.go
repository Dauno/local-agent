package contextsummary

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type summaryStoreFake struct {
	record      port.SummaryRecord
	hasRecord   bool
	job         port.SummaryJob
	scheduled   int64
	completed   bool
	failed      bool
	scheduleErr error
}

func (s *summaryStoreFake) LatestSummary(context.Context, string) (port.SummaryRecord, error) {
	if !s.hasRecord {
		return port.SummaryRecord{}, port.ErrSummaryNotFound
	}
	return s.record, nil
}

func (s *summaryStoreFake) CommitSummary(_ context.Context, commit port.SummaryCommit) (bool, error) {
	s.record = port.SummaryRecord{
		SessionIdentity:       commit.SessionIdentity,
		CoveredThroughOrdinal: commit.Summary.CoveredThroughOrdinal,
		SourceDigest:          commit.Summary.SourceDigest,
		SanitizedText:         commit.Summary.Text,
	}
	s.hasRecord = true
	return true, nil
}

func (s *summaryStoreFake) ScheduleSummaryJob(context.Context, string, int64, time.Time) (bool, error) {
	s.scheduled++
	return s.scheduleErr == nil, s.scheduleErr
}

func TestScheduleConversationWakesOnlyAfterCommittedInsert(t *testing.T) {
	for _, test := range []struct {
		name     string
		storeErr error
		wantWake bool
	}{
		{name: "committed", wantWake: true},
		{name: "failed", storeErr: errors.New("write failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &summaryStoreFake{scheduleErr: test.storeErr}
			wakes := 0
			service, err := New(Config{MaxChars: 100, RecentTurns: 1, WorkerInterval: time.Hour}, Dependencies{
				Store: store, Summarizer: summarizerFake{}, ScheduleWake: func() { wakes++ },
				TurnSource: summarySourceFake{turns: []domain.ConversationTurn{{Ordinal: 1, Closed: true}, {Ordinal: 2, Closed: true}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			gotErr := service.ScheduleConversation(t.Context(), "session-wake")
			if (gotErr != nil) != (test.storeErr != nil) {
				t.Fatalf("ScheduleConversation() error = %v", gotErr)
			}
			want := 0
			if test.wantWake {
				want = 1
			}
			if wakes != want {
				t.Fatalf("summary wake count = %d, want %d", wakes, want)
			}
		})
	}
}

func (s *summaryStoreFake) ClaimSummaryJob(context.Context, time.Time) (port.SummaryJob, error) {
	return s.job, nil
}

func (s *summaryStoreFake) CompleteSummaryJob(context.Context, port.SummaryJob) error {
	s.completed = true
	return nil
}

func (s *summaryStoreFake) FailSummaryJob(context.Context, port.SummaryJob, time.Time) error {
	s.failed = true
	return nil
}

type summarySourceFake struct {
	turns []domain.ConversationTurn
}

func (s summarySourceFake) ClosedTurns(context.Context, string, int64, int64) ([]domain.ConversationTurn, error) {
	return s.turns, nil
}

type summarizerFake struct{}

func (summarizerFake) Summarize(_ context.Context, request port.ConversationSummaryRequest) (port.ConversationSummary, error) {
	return port.ConversationSummary{
		Text:                  "The user stated a goal.",
		CoveredThroughOrdinal: request.ClosedTurns[len(request.ClosedTurns)-1].Ordinal,
		SourceDigest:          domain.ConversationSummarySourceDigest(request.PreviousSummary, request.ClosedTurns),
	}, nil
}

type recordingSummarizer struct {
	request port.ConversationSummaryRequest
}

func (s *recordingSummarizer) Summarize(_ context.Context, request port.ConversationSummaryRequest) (port.ConversationSummary, error) {
	s.request = request
	return port.ConversationSummary{
		Text:                  "The user stated that the batch completed.",
		CoveredThroughOrdinal: request.ClosedTurns[len(request.ClosedTurns)-1].Ordinal,
		SourceDigest:          domain.ConversationSummarySourceDigest(request.PreviousSummary, request.ClosedTurns),
	}, nil
}

func TestServiceSchedulesOnlyClosedPrefixOutsideRecentWindow(t *testing.T) {
	store := &summaryStoreFake{}
	service, err := New(
		Config{MaxChars: 100, RecentTurns: 2},
		Dependencies{
			Store:      store,
			Summarizer: summarizerFake{},
			TurnSource: summarySourceFake{turns: []domain.ConversationTurn{{Ordinal: 1, Closed: true}, {Ordinal: 2, Closed: true}, {Ordinal: 3, Closed: true}}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ScheduleConversation(context.Background(), "session-1"); err != nil {
		t.Fatal(err)
	}
	if store.scheduled != 1 {
		t.Fatalf("scheduled jobs = %d, want 1", store.scheduled)
	}
}

func TestServiceIncrementallyCommitsSummaryAndLeavesFailureRetryable(t *testing.T) {
	store := &summaryStoreFake{job: port.SummaryJob{SessionIdentity: "session-1", TargetOrdinal: 2, Attempts: 1}}
	service, err := New(
		Config{MaxChars: 100, RecentTurns: 1},
		Dependencies{Store: store, Summarizer: summarizerFake{}, TurnSource: summarySourceFake{turns: []domain.ConversationTurn{{Ordinal: 1, Closed: true}, {Ordinal: 2, Closed: true}}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if !store.completed || !store.hasRecord || store.record.CoveredThroughOrdinal != 2 {
		t.Fatalf("summary commit state = %#v, completed=%v", store.record, store.completed)
	}

	failedStore := &summaryStoreFake{job: port.SummaryJob{SessionIdentity: "session-2", TargetOrdinal: 1, Attempts: 1}}
	failing, err := New(
		Config{MaxChars: 100, RecentTurns: 1},
		Dependencies{Store: failedStore, Summarizer: failingSummarizer{}, TurnSource: summarySourceFake{turns: []domain.ConversationTurn{{Ordinal: 1, Closed: true}}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := failing.RunOnce(context.Background(), time.Now()); err == nil || !failedStore.failed {
		t.Fatalf("failed summary = %v, failed=%v", err, failedStore.failed)
	}
}

func TestServiceBatchesLargeSummarySourceAndSchedulesRemainder(t *testing.T) {
	turns := make([]domain.ConversationTurn, 331)
	for index := range turns {
		turns[index] = domain.ConversationTurn{Ordinal: int64(index + 1), Closed: true, Contents: []domain.Content{{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "turn"}}}}}
	}
	store := &summaryStoreFake{job: port.SummaryJob{SessionIdentity: "large", TargetOrdinal: 331, Attempts: 1}}
	summarizer := &recordingSummarizer{}
	service, err := New(
		Config{MaxChars: 100, RecentTurns: 1, MaxSourceTurns: 50, MaxPromptChars: 100000},
		Dependencies{Store: store, Summarizer: summarizer, TurnSource: summarySourceFake{turns: turns}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(summarizer.request.ClosedTurns) != 50 || store.scheduled != 1 || !store.completed {
		t.Fatalf("large summary batch = turns=%d scheduled=%d completed=%v", len(summarizer.request.ClosedTurns), store.scheduled, store.completed)
	}
}

type failingSummarizer struct{}

func (failingSummarizer) Summarize(context.Context, port.ConversationSummaryRequest) (port.ConversationSummary, error) {
	return port.ConversationSummary{}, errors.New("summarizer unavailable")
}

type invalidDigestSummarizer struct{}

func (invalidDigestSummarizer) Summarize(context.Context, port.ConversationSummaryRequest) (port.ConversationSummary, error) {
	return port.ConversationSummary{Text: "The user stated a goal.", CoveredThroughOrdinal: 1, SourceDigest: "invalid"}, nil
}

func TestServiceRejectsInvalidSummaryDigest(t *testing.T) {
	store := &summaryStoreFake{job: port.SummaryJob{SessionIdentity: "digest", TargetOrdinal: 1, Attempts: 1}}
	service, err := New(
		Config{MaxChars: 100, RecentTurns: 1},
		Dependencies{Store: store, Summarizer: invalidDigestSummarizer{}, TurnSource: summarySourceFake{turns: []domain.ConversationTurn{{Ordinal: 1, Closed: true}}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background(), time.Now()); err == nil || !store.failed || store.hasRecord {
		t.Fatalf("invalid digest result = %v failed=%v record=%v", err, store.failed, store.hasRecord)
	}
}
