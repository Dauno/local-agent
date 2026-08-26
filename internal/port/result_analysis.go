package port

import (
	"context"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// AnalysisRecord is the durable, bounded view of one TRD 07 analysis row.
// It never carries source content, prompts, or provider credentials.
//
// Limits is the immutable limits snapshot persisted alongside the row
// (FIND-103, checkpoint 6): AnalysisStore.Create already required and
// stored the full domain.AnalysisLimits, not only its digest, but nothing
// read it back until now. A durable worker resuming a running analysis
// after a restart must drive it under the exact bounds it was created
// under, never live configuration, so a configuration change can never
// mutate a running or completed analysis. Adding this field is additive:
// every existing caller that only reads the other fields is unaffected.
type AnalysisRecord struct {
	AnalysisID    string
	Identity      domain.AnalysisIdentity
	ObjectiveText string
	Scope         domain.ResultScope
	WorkstreamID  string
	State         domain.AnalysisState
	FailureCode   domain.AnalysisFailureCode
	Limits        domain.AnalysisLimits
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
	// otherwise. limits is the immutable snapshot persisted alongside the
	// row, not only its digest: identity.LimitsDigest must equal
	// limits.Digest(), so a resumed or restarted analysis can always
	// reconstruct the exact bounds it ran under, independent of whatever
	// the live configuration has since become.
	Create(
		ctx context.Context,
		identity domain.AnalysisIdentity,
		limits domain.AnalysisLimits,
		objectiveText string,
		scope domain.ResultScope,
		workstreamID string,
		now time.Time,
	) (AnalysisRecord, error)
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

// AnalysisEvidenceStore persists host-resolved, digest-verified evidence
// excerpts into the v40 analysis_evidence table (checkpoint 4). Evidence is
// immutable once written: the v40 analysis_evidence_immutable trigger
// enforces this at the schema level, and Write is idempotent by
// excerpt.EvidenceID, so a retried resolution of the same selector against
// the same segment never rewrites a stored excerpt.
//
// This is a new interface, not a change to an existing one: checkpoint 3
// left evidence persistence unimplemented because no port for it existed
// yet.
type AnalysisEvidenceStore interface {
	// Write persists one excerpt, owned by leafStepID. now is the caller's
	// injected clock; it is never used to decide identity or ordering, only
	// to populate created_at.
	Write(ctx context.Context, analysisID string, leafStepID string, excerpt AnalysisEvidenceExcerpt, now time.Time) error
	// ListByLeafStep returns every excerpt written for one leaf step, in
	// evidence-id order, so a reduction step can rebuild that leaf's bounded
	// evidence references without ever re-reading source text.
	ListByLeafStep(ctx context.Context, analysisID string, leafStepID string) ([]AnalysisEvidenceExcerpt, error)
}

// AnalysisStepPayloadStore persists and reads the bounded structured output
// payload of one step (checkpoint 4), separately from
// AnalysisStepStore.Complete's typed digest commit. It exists because
// checkpoint 3's AnalysisStepStore.Complete signature is frozen and carries
// no payload parameter, and a reduction step must be able to rebuild its
// children's typed findings from durable storage after a restart, without
// replaying any model call. WritePayload must be called for a step that is
// still 'claimed' under the caller's own claim token, before that step's
// Complete call; the v40 analysis_steps_completed_immutable trigger then
// protects the payload once the step is completed, exactly like it already
// protects output_digest.
type AnalysisStepPayloadStore interface {
	WritePayload(ctx context.Context, claim domain.AnalysisStepClaim, payload []byte, now time.Time) error
	ReadPayload(ctx context.Context, analysisID string, stepID string) ([]byte, error)
}

// AnalysisCompletionInput carries everything the atomic completion
// transaction needs to persist in one commit: the terminal packet's payload
// identity for the v34 host_reduction representation row, and the source
// result identity for the v34 result_references row that protects the
// source while this analysis exists. The terminal packet payload itself is
// not part of this input: it already lives in the root step's
// output_payload, written by the same AnalysisStepPayloadStore call that
// completed the root reduction step.
type AnalysisCompletionInput struct {
	AnalysisID     string
	Scope          domain.ResultScope
	SourceResultID string
	SourceSHA256   string
	SourceBytes    int64
	PacketSHA256   string
	PacketBytes    int64
	PromptVersion  string
}

// AnalysisCompletionStore atomically completes one analysis (checkpoint 4):
// marks the analysis row domain.AnalysisCompleted, writes one v34
// host_reduction result_representations row, and writes one live v34
// result_references row for the source result, all in a single transaction.
// A failure writing either row leaves the analysis in its prior
// non-terminal state; it is never marked completed on a partial commit. A
// retried call for an analysis that is already completed is idempotent: the
// representation and reference rows are derived from deterministic
// identifiers, so a retry writes the same rows, never a second copy.
type AnalysisCompletionStore interface {
	CompleteAnalysis(ctx context.Context, input AnalysisCompletionInput, now time.Time) error
}

// AnalysesByWorkstream is the bounded read TRD 07's Completion and Dependent
// Dispatch section requires: every analysis bound to one workstream, scoped
// by the trusted actor, conversation, and project a workstream binding
// carries. It is a separate interface from AnalysisStore (frozen since
// checkpoint 1) so this listing capability never widens AnalysisStore's own
// four methods. team_id is intentionally not part of the filter: the
// canonical conversation key already encodes the team
// ("slack:{team}:..."), and actor plus conversation plus project already
// scope one workstream's owner uniquely, so team_id adds no further
// selectivity here.
type AnalysesByWorkstream interface {
	ListByWorkstream(ctx context.Context, workstreamID, actor string, conversationKey domain.ConversationKey, project string) ([]AnalysisRecord, error)
}

// AnalysisActiveLister lists every non-terminal (preparing or running)
// analysis system-wide, in stable analysis-id order, so the durable worker
// (checkpoint 6) can discover work across every workstream in one paginated
// scan. This is a new interface: AnalysesByWorkstream stays scoped to one
// workstream, and AnalysisStore's four methods are unchanged.
type AnalysisActiveLister interface {
	ListActive(ctx context.Context, afterAnalysisID string, limit int) ([]AnalysisRecord, error)
}

// AnalysisRunningMarker transitions an analysis from preparing to running
// (checkpoint 6). It is a new interface rather than a fifth AnalysisStore
// method, because AnalysisStore's four methods (Create, Get, Complete,
// Fail) are frozen since checkpoint 1. MarkRunning is idempotent: calling
// it again on an already-running analysis succeeds and returns the current
// record rather than failing, so a worker that crashes between marking an
// analysis running and its next tick never fails on resume.
type AnalysisRunningMarker interface {
	MarkRunning(ctx context.Context, analysisID string, scope domain.ResultScope, now time.Time) (AnalysisRecord, error)
}

// AnalysisSegmentStore persists and reads the durable segment manifest
// (checkpoint 6) backed by the v40 analysis_segments table, which
// checkpoint 2 created but left unwritten: no store existed for it until
// now. A worker resuming after a restart needs each leaf step's exact byte
// range, which is not otherwise recoverable from an AnalysisStep alone
// (only its segment ordinal is durable there). WriteManifest is idempotent
// by (analysis_id, ordinal): a retried write after a crash never fails and
// never duplicates a row, matching the v40 analysis_segments_immutable
// trigger's UPDATE-only protection.
type AnalysisSegmentStore interface {
	WriteManifest(ctx context.Context, analysisID string, manifest domain.AnalysisSegmentManifest, now time.Time) error
	ListSegments(ctx context.Context, analysisID string) ([]domain.AnalysisSegment, error)
}
