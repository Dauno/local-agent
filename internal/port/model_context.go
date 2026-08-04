package port

import (
	"context"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// Serializer identifiers for countable model request envelopes. Requests
// without media keep the v1 representation; multimodal requests use v2, which
// replaces binary payloads with fixed markers and carries order-preserving
// media metadata. Counters must reject serializer IDs whose shape they cannot
// interpret.
const (
	SerializerOpenAIChatCompletionsV1           = "openai-chat-completions-v1"
	SerializerOpenAIChatCompletionsMultimodalV2 = "openai-chat-completions-multimodal-v2"
	SerializerContextProjectionV1               = "context-projection-v1"
)

// ModelRequestMedia is provider-neutral metadata for one binary media part in
// a countable envelope. It deliberately carries no bytes or data URL.
type ModelRequestMedia struct {
	MIMEType string
	Width    int
	Height   int
	Detail   string
}

type ModelRequestEnvelope struct {
	SerializerID string
	ProfileID    string
	Serialized   string
	Media        []ModelRequestMedia
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
