package port

import (
	"context"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// AnalysisRecord is the durable, bounded view of one TRD 07 analysis row.
// It never carries source content, prompts, or provider credentials.
type AnalysisRecord struct {
	AnalysisID    string
	Identity      domain.AnalysisIdentity
	ObjectiveText string
	Scope         domain.ResultScope
	WorkstreamID  string
	State         domain.AnalysisState
	FailureCode   domain.AnalysisFailureCode
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// AnalysisStore owns the durable analysis row lifecycle: creation from a
// validated identity, reads, and the two terminal transitions. Create is
// idempotent by semantic identity (domain.AnalysisIdentity.SemanticDigest):
// a concurrent or retried call for the same identity returns the existing
// row instead of creating a second one, mirroring the TRD 02 producer
// uniqueness rule. Every method scopes reads to the caller's exact
// domain.ResultScope; a cross-scope read is indistinguishable from a
// missing row.
type AnalysisStore interface {
	// Create returns the existing analysis record when one with the same
	// semantic identity already exists, or creates a new one in
	// domain.AnalysisPreparing state with a fresh opaque analysis ID
	// otherwise.
	Create(ctx context.Context, identity domain.AnalysisIdentity, objectiveText string, scope domain.ResultScope, workstreamID string, now time.Time) (AnalysisRecord, error)
	// Get reads one analysis record. It returns an error wrapping
	// domain.ErrAnalysisUnavailable when the row does not exist or does not
	// match scope.
	Get(ctx context.Context, analysisID string, scope domain.ResultScope) (AnalysisRecord, error)
	// Complete marks the analysis domain.AnalysisCompleted. It is the
	// caller's responsibility to have already written the host_reduction
	// representation and the live result_references row in the same
	// transaction (checkpoint 4); this call only advances the analysis
	// state.
	Complete(ctx context.Context, analysisID string, scope domain.ResultScope, now time.Time) (AnalysisRecord, error)
	// Fail marks the analysis domain.AnalysisFailed with a closed failure
	// code. An analysis that is already terminal (completed or failed)
	// rejects a second terminal transition.
	Fail(ctx context.Context, analysisID string, scope domain.ResultScope, code domain.AnalysisFailureCode, now time.Time) (AnalysisRecord, error)
}

// AnalysisStep is the durable, bounded view of one leaf or reduction step.
// ChildStepIDs is populated only for domain.AnalysisStepReduction steps, in
// deterministic segment-ordinal order; SegmentOrdinal is populated only for
// domain.AnalysisStepLeaf steps.
type AnalysisStep struct {
	AnalysisID     string
	StepID         string
	Kind           domain.AnalysisStepKind
	State          domain.AnalysisStepState
	Attempt        int
	Generation     int64
	LeaseUntil     time.Time
	SegmentOrdinal int
	ChildStepIDs   []string
	FailureCode    domain.AnalysisFailureCode
	OutputDigest   string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// AnalysisStepStore is the generation-CAS durable queue for one analysis'
// leaf and reduction steps. It reuses the TRD 06 domain.AnalysisStepClaim
// claim-token discipline exactly: every terminal or retry transition must
// present the exact generation and lease that ClaimNext returned, so a
// worker whose lease expired and was reclaimed can never mutate the new
// owner's claim. A stale token returns an error wrapping
// domain.ErrAnalysisCASConflict.
type AnalysisStepStore interface {
	// Prepare inserts one step in domain.AnalysisStepPrepared state. For a
	// reduction step, ChildStepIDs must already reference existing steps of
	// the same analysis; a reduction step becomes claimable only once every
	// declared child step is domain.AnalysisStepCompleted.
	Prepare(ctx context.Context, step AnalysisStep) (AnalysisStep, error)
	// ClaimNext claims the next eligible prepared step for one analysis:
	// for a leaf step, eligibility only requires domain.AnalysisStepPrepared
	// state and a due next-attempt time; for a reduction step, it
	// additionally requires every child step to be completed. The bounded
	// injected clock and lease duration are explicit arguments.
	ClaimNext(ctx context.Context, analysisID string, now time.Time, lease time.Duration) (AnalysisStep, bool, error)
	// Complete commits a step's typed output digest and marks it
	// domain.AnalysisStepCompleted. Immutability is enforced: a completed
	// step's output can never change on a later call.
	Complete(ctx context.Context, claim domain.AnalysisStepClaim, outputDigest string, now time.Time) (AnalysisStep, error)
	// Retry returns a step to domain.AnalysisStepPrepared for a later
	// attempt without consuming the attempt budget when release is due to
	// permit exhaustion on the process-wide model-call limiter, or with the
	// attempt budget consumed for any other retryable failure.
	Retry(ctx context.Context, claim domain.AnalysisStepClaim, nextAttempt time.Time, consumeAttempt bool) error
	// Fail marks a step domain.AnalysisStepFailed with a closed failure
	// code, ending its retry lifecycle.
	Fail(ctx context.Context, claim domain.AnalysisStepClaim, code domain.AnalysisFailureCode, now time.Time) error
	// List pages one analysis' steps in step-ID order for restart resume
	// and coverage/lineage verification.
	List(ctx context.Context, analysisID string, afterStepID string, limit int) ([]AnalysisStep, error)
}

// AnalysisLeafInput is the complete, bounded input to one leaf model call.
// It carries no ambient conversation history, tool surface, or Slack
// context: only the bounded objective, the approved constraints required
// for analysis, and the one segment's own text.
type AnalysisLeafInput struct {
	ObjectiveClass domain.AnalysisObjectiveClass
	ObjectiveText  string
	Constraints    []string
	SegmentOrdinal int
	SegmentText    string
	PromptVersion  string
}

// AnalysisChildSummary is the bounded structured view of one completed
// child step that a reduction call combines. It carries only closed
// structured fields, never source text or raw model output, so a reduction
// call at any tree depth sees typed child findings only.
type AnalysisChildSummary struct {
	StepID              string
	Findings            []domain.AnalysisStatement
	Constraints         []domain.AnalysisStatement
	Contradictions      []domain.AnalysisContradiction
	UnresolvedQuestions []domain.AnalysisStatement
	EvidenceRefs        []string
}

// AnalysisReductionInput is the complete, bounded input to one reduction
// model call: the same bounded objective and constraints as a leaf call,
// plus the ordered bounded child summaries being combined.
type AnalysisReductionInput struct {
	ObjectiveClass domain.AnalysisObjectiveClass
	ObjectiveText  string
	Constraints    []string
	Children       []AnalysisChildSummary
	PromptVersion  string
}

// ResultAnalyzer is the pure no-tool inference boundary for one leaf or
// reduction call. It follows the exact call shape
// internal/adapter/openaillm/summarizer.go already uses for pure inference:
// no tools, no conversation history, a bounded prompt, untrusted source text
// fenced as data, and a pinned prompt version. An implementation must never
// give the underlying model call tools, conversation history, Slack access,
// or artifacts; nothing in AnalysisLeafInput or AnalysisReductionInput
// carries such access, so an implementation cannot widen it without
// changing this contract.
type ResultAnalyzer interface {
	AnalyzeLeaf(ctx context.Context, input AnalysisLeafInput) (domain.AnalysisLeaf, error)
	Reduce(ctx context.Context, input AnalysisReductionInput) (domain.AnalysisPacket, error)
}

// AnalysisEvidenceExcerpt is one host-resolved, digest-verified evidence
// excerpt bound into a downstream bundle. Excerpt is already bounded to
// domain.HardMaxAnalysisEvidenceExcerptBytes by the host resolver that
// produced it (checkpoint 3), not by the bundle builder.
type AnalysisEvidenceExcerpt struct {
	EvidenceID     string
	SegmentOrdinal int
	Range          domain.AnalysisByteRange
	SHA256         string
	Excerpt        string
}

// AnalysisBundle is the bounded downstream selector handed to a dependent
// task. Its identifier is a selector, not a capability: it carries no
// storage path, credential, or raw provider error.
type AnalysisBundle struct {
	BundleID   string
	AnalysisID string
	Packet     domain.AnalysisPacket
	Evidence   []AnalysisEvidenceExcerpt
	Bytes      int64
	SHA256     string
}

// AnalysisBundleBuilder constructs one bounded downstream bundle from a
// completed analysis' terminal packet and its verified evidence. A bundle
// that would exceed limits.BundleBytes (or the caller's own smaller frame
// bound) is a typed rejection wrapping domain.ErrAnalysisValidation; it is
// never silently truncated.
type AnalysisBundleBuilder interface {
	Build(ctx context.Context, analysisID string, packet domain.AnalysisPacket, evidence []AnalysisEvidenceExcerpt, limits domain.AnalysisLimits) (AnalysisBundle, error)
}
