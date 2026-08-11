package port

import (
	"context"

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
