package contextsummary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const (
	DefaultPromptVersion   = "conversation-summary-v1"
	maxAttempts            = 5
	defaultMaxSourceTurns  = 50
	defaultMaxPromptChars  = 120_000
	defaultMaxOutputTokens = 2_048
	defaultJobTimeout      = 2 * time.Minute
)

type TurnSource interface {
	ClosedTurns(context.Context, string, int64, int64) ([]domain.ConversationTurn, error)
}

type Config struct {
	MaxChars        int
	RecentTurns     int
	WorkerInterval  time.Duration
	MaxSourceTurns  int
	MaxPromptChars  int
	MaxOutputTokens int
	JobTimeout      time.Duration
}

type Dependencies struct {
	Store      port.SummaryStore
	Summarizer port.ConversationSummarizer
	TurnSource TurnSource
	Logger     port.Logger
}

type Service struct {
	config     Config
	store      port.SummaryStore
	summarizer port.ConversationSummarizer
	turnSource TurnSource
	logger     port.Logger
	metrics    *SummaryWorkerMetrics
}

type SummaryWorkerMetrics struct {
	Failures atomic.Uint64
}

func New(config Config, deps Dependencies) (*Service, error) {
	if config.MaxChars <= 0 || config.RecentTurns <= 0 {
		return nil, errors.New("summary max chars and recent turns must be positive")
	}
	if deps.Store == nil || deps.Summarizer == nil || deps.TurnSource == nil {
		return nil, errors.New("summary store, summarizer, and turn source are required")
	}
	if config.WorkerInterval <= 0 {
		config.WorkerInterval = time.Second
	}
	if config.MaxSourceTurns <= 0 {
		config.MaxSourceTurns = defaultMaxSourceTurns
	}
	if config.MaxPromptChars <= 0 {
		config.MaxPromptChars = defaultMaxPromptChars
	}
	if config.MaxOutputTokens <= 0 {
		config.MaxOutputTokens = defaultMaxOutputTokens
	}
	if config.JobTimeout <= 0 {
		config.JobTimeout = defaultJobTimeout
	}
	return &Service{config: config, store: deps.Store, summarizer: deps.Summarizer, turnSource: deps.TurnSource, logger: deps.Logger, metrics: &SummaryWorkerMetrics{}}, nil
}

// ScheduleConversation finds the newest contiguous closed prefix and relies on
// the store's session/target uniqueness to coalesce repeated successful turns.
func (s *Service) ScheduleConversation(ctx context.Context, sessionIdentity string) error {
	previous, err := s.store.LatestSummary(ctx, sessionIdentity)
	if err != nil && !errors.Is(err, port.ErrSummaryNotFound) {
		return err
	}
	turns, err := s.turnSource.ClosedTurns(ctx, sessionIdentity, previous.CoveredThroughOrdinal, int64(^uint64(0)>>1))
	if err != nil {
		return err
	}
	if len(turns) <= s.config.RecentTurns {
		return nil
	}
	target := turns[len(turns)-s.config.RecentTurns-1].Ordinal
	_, err = s.store.ScheduleSummaryJob(ctx, sessionIdentity, target, time.Now().UTC())
	return err
}

// RunOnce processes one durable job. A summary failure never escapes to the
// foreground caller; the job is left retryable with bounded attempts.
func (s *Service) RunOnce(ctx context.Context, now time.Time) error {
	jobCtx, cancel := context.WithTimeout(ctx, s.config.JobTimeout)
	defer cancel()
	job, err := s.store.ClaimSummaryJob(jobCtx, now)
	if err != nil {
		return err
	}
	return s.runClaimedJob(ctx, jobCtx, job)
}

func (s *Service) runClaimedJob(persistCtx, ctx context.Context, job port.SummaryJob) error {
	previous, err := s.store.LatestSummary(ctx, job.SessionIdentity)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return s.retry(context.WithoutCancel(persistCtx), job, err)
	}
	if err != nil && !errors.Is(err, port.ErrSummaryNotFound) {
		return s.retry(context.WithoutCancel(persistCtx), job, err)
	}
	previousOrdinal := previous.CoveredThroughOrdinal
	turns, err := s.turnSource.ClosedTurns(ctx, job.SessionIdentity, previousOrdinal, job.TargetOrdinal)
	if err != nil {
		return s.retry(context.WithoutCancel(persistCtx), job, err)
	}
	if err := validateContiguousTurns(turns, previousOrdinal, job.TargetOrdinal); err != nil {
		return s.retry(context.WithoutCancel(persistCtx), job, err)
	}
	batchTurns := turns
	if len(batchTurns) > s.config.MaxSourceTurns {
		batchTurns = batchTurns[:s.config.MaxSourceTurns]
	}
	for len(batchTurns) > 1 && summaryPromptChars(previous.SanitizedText, batchTurns) > s.config.MaxPromptChars {
		batchTurns = batchTurns[:len(batchTurns)-1]
	}
	if len(batchTurns) == 0 || summaryPromptChars(previous.SanitizedText, batchTurns) > s.config.MaxPromptChars {
		return s.retry(context.WithoutCancel(persistCtx), job, errors.New("summary prompt exceeds configured bound"))
	}
	effectiveTarget := batchTurns[len(batchTurns)-1].Ordinal
	request := port.ConversationSummaryRequest{PreviousSummary: previous.SanitizedText, ClosedTurns: cloneTurns(batchTurns), MaxChars: s.config.MaxChars, MaxOutputTokens: s.config.MaxOutputTokens}
	summary, err := s.summarizer.Summarize(ctx, request)
	if err != nil {
		return s.retry(context.WithoutCancel(persistCtx), job, err)
	}
	if summary.PromptVersion == "" {
		summary.PromptVersion = DefaultPromptVersion
	}
	expectedDigest := domain.ConversationSummarySourceDigest(previous.SanitizedText, request.ClosedTurns)
	if summary.SourceDigest != expectedDigest {
		return s.retry(context.WithoutCancel(persistCtx), job, errors.New("summarizer returned an invalid source digest"))
	}
	if summary.CoveredThroughOrdinal != effectiveTarget {
		return s.retry(context.WithoutCancel(persistCtx), job, errors.New("summarizer returned non-contiguous coverage"))
	}
	cleanSummary, err := domain.SanitizeConversationSummary(summary.Text, s.config.MaxChars)
	if err != nil {
		return s.retry(context.WithoutCancel(persistCtx), job, err)
	}
	summary.Text = cleanSummary
	committed, err := s.store.CommitSummary(ctx, port.SummaryCommit{
		SessionIdentity: job.SessionIdentity, ExpectedOrdinal: previousOrdinal,
		ExpectedSourceDigest: previous.SourceDigest, Summary: summary,
	})
	if err != nil {
		return s.retry(context.WithoutCancel(persistCtx), job, err)
	}
	// A false CAS means another worker already advanced the summary. The job is
	// obsolete, not failed, and must not overwrite the newer revision.
	if err := s.store.CompleteSummaryJob(ctx, job); err != nil {
		return err
	}
	if !committed && s.logger != nil {
		s.logger.Warn("summary CAS lost; job marked obsolete", "session_identity", job.SessionIdentity, "target_ordinal", job.TargetOrdinal)
	}
	if effectiveTarget < job.TargetOrdinal {
		if _, err := s.store.ScheduleSummaryJob(context.WithoutCancel(persistCtx), job.SessionIdentity, job.TargetOrdinal, time.Now().UTC()); err != nil {
			return fmt.Errorf("schedule remaining summary batch: %w", err)
		}
	}
	return nil
}

func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.config.WorkerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			_ = s.RunOnce(ctx, now)
		}
	}
}

func (s *Service) retry(ctx context.Context, job port.SummaryJob, cause error) error {
	if s.metrics != nil {
		s.metrics.Failures.Add(1)
	}
	if job.Attempts >= maxAttempts {
		if err := s.store.FailSummaryJob(ctx, job, time.Time{}); err != nil {
			return fmt.Errorf("summary permanently failed (%v), then job update failed: %w", cause, err)
		}
		return cause
	}
	backoff := time.Duration(job.Attempts*job.Attempts) * time.Minute
	if err := s.store.FailSummaryJob(ctx, job, time.Now().UTC().Add(backoff)); err != nil {
		return fmt.Errorf("summary failed (%v), then job update failed: %w", cause, err)
	}
	return cause
}

func validateContiguousTurns(turns []domain.ConversationTurn, previous, target int64) error {
	if target <= previous || len(turns) == 0 {
		return errors.New("summary source has no contiguous closed turns")
	}
	want := previous + 1
	for _, turn := range turns {
		if !turn.Closed || turn.HasOpenConfirmation || turn.Ordinal != want {
			return errors.New("summary source contains a non-contiguous or ineligible turn")
		}
		want++
	}
	if want-1 != target {
		return errors.New("summary source does not reach requested target")
	}
	return nil
}

func cloneTurns(turns []domain.ConversationTurn) []domain.ConversationTurn {
	result := make([]domain.ConversationTurn, len(turns))
	for index, turn := range turns {
		result[index] = turn.Clone()
	}
	return result
}

func summaryPromptChars(previous string, turns []domain.ConversationTurn) int {
	data, _ := json.Marshal(turns)
	return len([]rune(previous)) + len(data) + 512
}
