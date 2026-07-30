package domain

import (
	"errors"
	"fmt"
)

var ErrIrreducibleContext = errors.New("request_context_irreducible")

// CompileRequest carries the full input the compiler needs to produce a
// bounded model-facing projection. Budget values are in tokens.
type CompileRequest struct {
	Contents           []Content
	Continuity         ContinuityCapsule
	ExistingSummary    string
	ModelBudget        RequestBudget
	FixedRequestTokens int
	Actor              string
	ConversationKey    string
	SessionRevision    int64
	OpenInvocationIDs  map[string]struct{}
}

// CompileResult carries the bounded content projection and diagnostics.
type CompileResult struct {
	Contents    []Content
	Diagnostics CompileDiagnostics
}

// CompileDiagnostics records what the compiler changed without exposing
// content, paths, function arguments, result references, or digests.
type CompileDiagnostics struct {
	RequestTokensBefore   int
	RequestTokensAfter    int
	ProtectedTokens       int
	ContinuityTokens      int
	RecentTurnsRetained   int
	ResponsesExternalized int
	ResponseTokensRemoved int
	ReductionReason       string
	HardLimitTokens       int
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
