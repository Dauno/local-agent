package domain

import (
	"errors"
	"fmt"
)

var ErrIrreducibleContext = errors.New("request_context_irreducible")

// CompileRequest carries the full input the compiler needs to produce a
// bounded model-facing projection. Budget values are in tokens. Knowledge
// carries the ordered frame-card candidates from one retrieval run; the
// compiler selects whole cards under KnowledgeBudgetTokens and never
// populates retrieval itself. Workstream carries the bounded snapshot of the
// active actor-bound workstream, when one exists; the compiler admits it as
// one atomic block under WorkstreamBudgetTokens and never mutates or
// authorizes from it.
type CompileRequest struct {
	Contents               []Content
	Continuity             ContinuityCapsule
	ExistingSummary        string
	Compaction             ContextCompactionSettings
	ModelBudget            RequestBudget
	FixedRequestTokens     int
	Actor                  string
	ConversationKey        string
	OpenInvocationIDs      map[string]struct{}
	Knowledge              []KnowledgeFrameCard
	KnowledgeBudgetTokens  int
	Workstream             *WorkstreamSnapshot
	WorkstreamBudgetTokens int
}

// ContextCompactionSettings are the effective limits owned by the token-aware
// compiler. History limits apply only to optional summary and completed turns;
// active protocol contents remain governed by request-token admission.
type ContextCompactionSettings struct {
	Enabled         bool
	MaxHistoryChars int
	RecentTurns     int
	SummaryEnabled  bool
	SummaryMaxChars int
	// SummaryBudgetTokens bounds the optional summary as measured by the
	// provider-shaped frame counter. Zero preserves the test/legacy default
	// where no source-specific cap is configured.
	SummaryBudgetTokens int
}

// CompileResult carries the bounded content projection and diagnostics.
// KnowledgeIdentities records the sorted content-free identities of the
// selected frame cards, and KnowledgeSelectedCount/KnowledgeOmittedCount
// carry the bounded counts for the epoch recorder. Cards are never durable
// and never appear here. WorkstreamSnapshotIncluded and
// WorkstreamOmissionReason report the deterministic all-or-nothing admission
// outcome of the active workstream snapshot without ever exposing its
// content; WorkstreamOmissionReason is one of a closed set of bounded
// categories ("no_snapshot", "disabled", "source_budget", "total_pressure").
type CompileResult struct {
	Contents                   []Content
	Diagnostics                CompileDiagnostics
	KnowledgeIdentities        []string
	KnowledgeSelectedCount     int
	KnowledgeOmittedCount      int
	WorkstreamSnapshotIncluded bool
	WorkstreamOmissionReason   string
	// SummaryIncluded reports whether the rolling summary source survived
	// every selection and eviction phase into the final admitted frame. The
	// epoch recorder uses it to decide, by outcome rather than omission,
	// whether SummaryIdentity should be populated.
	SummaryIncluded bool
}

// CompileDiagnostics records what the compiler changed without exposing
// content, paths, function arguments, result references, or digests.
// ProtectedTokens remains a dashboard-compatibility field; new code-point
// measurements use ProtectedCodePoints.
type CompileDiagnostics struct {
	RequestTokensBefore       int
	RequestTokensAfter        int
	RequestCodePointsBefore   int
	RequestCodePointsAfter    int
	ProtectedTokens           int
	ProtectedCodePoints       int
	ContinuityTokens          int
	ContinuityCodePoints      int
	RecentTurnsRetained       int
	SummarySourceTokens       int
	SummaryOmitted            bool
	SummaryOmissionReason     string
	ResponsesExternalized     int
	ResponseTokensRemoved     int
	ResponseCodePointsRemoved int
	ReductionReason           string
	ReductionStage            string
	LateExternalized          bool
	HardLimitTokens           int
	RecountPasses             int
	CounterStrategy           string
}

// ContextProjectionMarker is the application-owned structured field inserted
// into a FunctionResponse.Response map when a payload has been externalized.
// It carries recoverable-reference metadata and never contains content.
type ContextProjectionMarker struct {
	Reason        string `json:"reason"`
	ResultRef     string `json:"result_ref"`
	SHA256        string `json:"sha256"`
	OriginalBytes int    `json:"original_bytes"`
	InlineBytes   int    `json:"inline_bytes"`
	Complete      bool   `json:"complete"`
}

// IrreducibleContextError signals that even the minimum protected context plus
// response identity envelopes exceed the hard token limit.
type IrreducibleContextError struct {
	MinimumTokens int
	HardTokens    int
}

func (e *IrreducibleContextError) Error() string {
	return fmt.Sprintf("%s: minimum required tokens %d exceeds hard limit %d", ErrIrreducibleContext, e.MinimumTokens, e.HardTokens)
}

func (e *IrreducibleContextError) Unwrap() error { return ErrIrreducibleContext }
