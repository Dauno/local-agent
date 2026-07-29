package port

import (
	"context"
	"errors"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

var ErrSummaryNotFound = errors.New("conversation summary not found")

type ConversationSummaryRequest struct {
	PreviousSummary string
	ClosedTurns     []domain.ConversationTurn
	MaxChars        int
	MaxOutputTokens int
}

type ConversationSummary struct {
	Text                  string
	CoveredThroughOrdinal int64
	SourceDigest          string
	PromptVersion         string
}

type ConversationSummarizer interface {
	Summarize(context.Context, ConversationSummaryRequest) (ConversationSummary, error)
}

type SummaryScheduler interface {
	ScheduleConversation(context.Context, string) error
}

type SummaryRecord struct {
	SessionIdentity       string
	CoveredThroughOrdinal int64
	SourceDigest          string
	SanitizedText         string
	PromptVersion         string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type SummaryCommit struct {
	SessionIdentity      string
	ExpectedOrdinal      int64
	ExpectedSourceDigest string
	Summary              ConversationSummary
}

type SummaryJob struct {
	SessionIdentity string
	TargetOrdinal   int64
	Status          string
	Attempts        int
	NextAttempt     time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// SummaryStore owns only sanitized summary state. It is never consulted for
// authorization, tool execution, confirmation, or audit decisions.
type SummaryStore interface {
	LatestSummary(context.Context, string) (SummaryRecord, error)
	CommitSummary(context.Context, SummaryCommit) (bool, error)
	ScheduleSummaryJob(context.Context, string, int64, time.Time) (bool, error)
	ClaimSummaryJob(context.Context, time.Time) (SummaryJob, error)
	CompleteSummaryJob(context.Context, SummaryJob) error
	FailSummaryJob(context.Context, SummaryJob, time.Time) error
}
