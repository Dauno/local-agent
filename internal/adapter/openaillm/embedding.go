package openaillm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// EmbeddingProviderConfig configures the OpenAI-compatible embedding
// provider. Values come from the validated application configuration; the
// adapter owns its client construction and its concurrency budget.
type EmbeddingProviderConfig struct {
	APIKey     string
	BaseURL    string
	Model      string
	Dimensions int
	Timeout    time.Duration
	MaxBatch   int
	Limiter    port.ModelCallLimiter
	Redact     func(string) string
}

// OpenAIEmbeddingProvider implements port.EmbeddingProvider over the OpenAI
// embeddings endpoint using the shared OpenAI SDK client construction. It
// never logs request text, response vectors, credentials, base URLs, item
// identities, actors, or conversations.
type OpenAIEmbeddingProvider struct {
	client     openai.Client
	model      string
	dimensions int
	timeout    time.Duration
	maxBatch   int
	limiter    port.ModelCallLimiter
	redact     func(string) string
}

var _ port.EmbeddingProvider = (*OpenAIEmbeddingProvider)(nil)

// NewEmbeddingProvider constructs the adapter and validates its bounds.
func NewEmbeddingProvider(config EmbeddingProviderConfig) (*OpenAIEmbeddingProvider, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("OpenAI-compatible embedding API key is required")
	}
	if strings.TrimSpace(config.BaseURL) == "" {
		return nil, errors.New("OpenAI-compatible embedding base URL is required")
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, errors.New("OpenAI-compatible embedding model is required")
	}
	if config.Dimensions < 1 || config.Dimensions > domain.HardMaxKnowledgeEmbeddingDimensions {
		return nil, fmt.Errorf("OpenAI-compatible embedding dimensions must be between 1 and %d", domain.HardMaxKnowledgeEmbeddingDimensions)
	}
	if config.Timeout <= 0 {
		return nil, errors.New("OpenAI-compatible embedding timeout must be positive")
	}
	if config.MaxBatch < 1 || config.MaxBatch > domain.HardMaxKnowledgeRetrievalWorkerBatchSize {
		return nil, fmt.Errorf("OpenAI-compatible embedding batch size must be between 1 and %d", domain.HardMaxKnowledgeRetrievalWorkerBatchSize)
	}
	if config.Limiter == nil {
		return nil, errors.New("OpenAI-compatible embedding limiter is required")
	}
	return &OpenAIEmbeddingProvider{
		client:     openai.NewClient(option.WithAPIKey(config.APIKey), option.WithBaseURL(config.BaseURL)),
		model:      config.Model,
		dimensions: config.Dimensions,
		timeout:    config.Timeout,
		maxBatch:   config.MaxBatch,
		limiter:    config.Limiter,
		redact:     config.Redact,
	}, nil
}

// Embed embeds the given redacted inputs in one bounded provider call. Inputs
// pass through the configured credential redactor before provider contact.
// The shared model-call limiter bounds embedding concurrency together with
// all other model calls in the process.
func (p *OpenAIEmbeddingProvider) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, errors.New("OpenAI-compatible embedding requires at least one input")
	}
	if len(inputs) > p.maxBatch {
		return nil, fmt.Errorf("OpenAI-compatible embedding batch of %d exceeds the configured bound %d", len(inputs), p.maxBatch)
	}
	release, acquired := p.limiter.TryAcquire()
	if !acquired {
		return nil, port.ErrModelCallLimitReached
	}
	defer release()
	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	redacted := make([]string, len(inputs))
	for index, input := range inputs {
		if p.redact != nil {
			input = p.redact(input)
		}
		redacted[index] = input
	}

	params := openai.EmbeddingNewParams{
		Input:      openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: redacted},
		Model:      openai.EmbeddingModel(p.model),
		Dimensions: openai.Int(int64(p.dimensions)),
	}
	response, err := p.client.Embeddings.New(callCtx, params)
	if err != nil {
		return nil, fmt.Errorf("OpenAI-compatible embeddings request failed: %w", err)
	}
	if len(response.Data) != len(redacted) {
		return nil, fmt.Errorf("OpenAI-compatible embeddings response contained %d vectors for %d inputs", len(response.Data), len(redacted))
	}
	vectors := make([][]float32, len(response.Data))
	seen := make([]bool, len(response.Data))
	for _, embedding := range response.Data {
		if embedding.Index < 0 || embedding.Index >= int64(len(response.Data)) {
			return nil, fmt.Errorf("OpenAI-compatible embeddings response contained an out-of-range index %d for %d inputs", embedding.Index, len(response.Data))
		}
		index := int(embedding.Index)
		if seen[index] {
			return nil, fmt.Errorf("OpenAI-compatible embeddings response contained a duplicate index %d", index)
		}
		seen[index] = true
		converted := make([]float32, len(embedding.Embedding))
		for valueIndex, value := range embedding.Embedding {
			converted[valueIndex] = float32(value)
		}
		vectors[index] = converted
	}
	return vectors, nil
}
