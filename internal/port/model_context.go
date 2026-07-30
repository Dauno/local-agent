package port

import (
	"context"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

type ModelRequestEnvelope struct {
	SerializerID string
	ProfileID    string
	Serialized   string
}

type TokenCount struct {
	Tokens   int
	Strategy string
	Exact    bool
}

type RequestTokenCounter interface {
	CountRequest(ctx context.Context, envelope ModelRequestEnvelope) (TokenCount, error)
}

// RecoverableResultReferenceChecker lets retention skip results still required
// by durable application state.
type RecoverableResultReferenceChecker interface {
	IsRecoverableResultReferenced(ctx context.Context, ref string) (bool, error)
}

type PutResultRequest struct {
	Actor           string
	ConversationKey string
	Kind            string
	Content         string
}

type StatResultRequest struct {
	Ref             string
	Actor           string
	ConversationKey string
}

type RecoverableResultStore interface {
	Put(ctx context.Context, req PutResultRequest) (domain.RecoverableResult, error)
	ReadChunk(ctx context.Context, req domain.ResultChunkRequest) (domain.ResultChunk, error)
	Stat(ctx context.Context, req StatResultRequest) (domain.RecoverableResult, error)
	DeleteExpired(ctx context.Context, cutoff time.Time, batchSize int) (int, error)
}
