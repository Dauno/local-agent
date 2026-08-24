package port

import (
	"context"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// ResultMaterialization is accepted only from a trusted producer boundary.
// Payload has already completed host redaction and control sanitization before
// it is handed to a concrete typed result store.
type ResultMaterialization struct {
	Producer  domain.ResultProducer
	Payload   string
	Scope     domain.ResultScope
	Retention domain.ResultRetentionClass
	MediaType string
}

// TrustedResultStore owns V2 result materialization and resolution. Resolve
// fully verifies its typed physical payload before returning identity or handle.
// Store-produced handles never advertise transient inline availability; only
// the context layer may add it after exact provider-shaped frame admission.
type TrustedResultStore interface {
	Materialize(context.Context, ResultMaterialization) (domain.ResultHandle, error)
	Resolve(context.Context, string, domain.ResultScope) (domain.ResultIdentity, domain.ResultHandle, error)
	ReadRange(context.Context, string, domain.ResultScope, int64, int64) (domain.ResultChunk, error)
}

// WorkstreamResultVerification contains only host-derived selectors. The
// verifier resolves the workstream first and rejects model-provided scope.
type WorkstreamResultVerification struct {
	ResultID     string
	WorkstreamID string
	Actor        string
	TeamID       string
	Conversation string
	Project      string
}

type ResultIdentityVerifier interface {
	VerifyForWorkstream(context.Context, WorkstreamResultVerification) (domain.ResultIdentity, error)
}

// WorkstreamResultLinkCommit carries a physically verified identity into the
// authoritative SQLite commit boundary. The implementation must re-read and
// compare the canonical available result record in the same transaction that
// applies the workstream transition and inserts both the normalized verified
// binding and live result reference.
type WorkstreamResultLinkCommit struct {
	Verification     WorkstreamResultVerification
	VerifiedIdentity domain.ResultIdentity
	Transition       domain.WorkstreamTransition
	Limits           domain.WorkstreamLimits
	Now              time.Time
}

// WorkstreamResultLinkCommitter prevents a verify-then-Apply split from
// creating a TOCTOU gap at the durable workstream/result boundary.
type WorkstreamResultLinkCommitter interface {
	CommitVerifiedResultLink(context.Context, WorkstreamResultLinkCommit) (domain.WorkstreamTransitionRecord, error)
}
