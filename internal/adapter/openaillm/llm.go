// Package openaillm adapts OpenAI-compatible Chat Completions endpoints to the
// ADK model.LLM boundary.
package openaillm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync/atomic"

	"github.com/openai/openai-go/v3"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

var (
	// ErrToolsUnsupported is returned before any provider call when ADK supplies
	// tools, function calls, or tool responses.
	ErrToolsUnsupported = errors.New("model tools and function parts are not supported")
	// ErrUnsupportedPart indicates non-text input content.
	ErrUnsupportedPart = errors.New("only text model content is supported")
	// ErrNoAssistantText indicates a successful provider response without usable
	// assistant text.
	ErrNoAssistantText = errors.New("model response contained no non-empty assistant text")
)

const (
	SSEErrorProviderRequest  = "provider_request"
	SSEErrorDecode           = "sse_decode"
	SSEErrorTransportRead    = "transport_read"
	SSEErrorChunkConsistency = "chunk_consistency"
	SSEErrorFinalAggregation = "final_aggregation"
)

// SSEError is a typed, content-free classification for streaming failures.
// It deliberately carries no frame or payload so diagnostics cannot leak raw
// provider data.
type SSEError struct {
	Category       string
	Err            error
	FramePresent   bool
	PayloadPresent bool
}

type StreamMetrics struct {
	SSEDecodeFailures atomic.Uint64
}

func (e *SSEError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("OpenAI-compatible stream error (%s)", e.Category)
}

func (e *SSEError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func classifyStreamReadError(err error) string {
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{"status code", "api error", "provider error", "invalid request"} {
		if strings.Contains(message, fragment) {
			return SSEErrorProviderRequest
		}
	}
	for _, fragment := range []string{"decode", "json", "unexpected end", "unexpected eof", "invalid character"} {
		if strings.Contains(message, fragment) {
			return SSEErrorDecode
		}
	}
	return SSEErrorTransportRead
}

// OpenAICompatibleLLM implements ADK's model.LLM using OpenAI Chat Completions.
type OpenAICompatibleLLM struct {
	client          openai.Client
	model           string
	reasoningEffort string
	extraBody       map[string]any
	metrics         *StreamMetrics
}

var _ model.LLM = (*OpenAICompatibleLLM)(nil)

// New constructs an OpenAI-compatible ADK model adapter.
func New(options ...Option) (*OpenAICompatibleLLM, error) {
	var cfg settings
	for index, apply := range options {
		if apply == nil {
			return nil, fmt.Errorf("OpenAI-compatible option %d is nil", index)
		}
		if err := apply(&cfg); err != nil {
			return nil, err
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &OpenAICompatibleLLM{
		client:          openai.NewClient(cfg.clientOptions()...),
		model:           cfg.model,
		reasoningEffort: cfg.reasoningEffort,
		extraBody:       cfg.extraBody,
		metrics:         &StreamMetrics{},
	}, nil
}

func (m *OpenAICompatibleLLM) Metrics() *StreamMetrics {
	if m == nil {
		return nil
	}
	return m.metrics
}

func (m *OpenAICompatibleLLM) Counters() map[string]uint64 {
	if m == nil || m.metrics == nil {
		return map[string]uint64{}
	}
	return map[string]uint64{"sse_decode_failures": m.metrics.SSEDecodeFailures.Load()}
}

// Name returns the configured provider model identifier.
func (m *OpenAICompatibleLLM) Name() string {
	if m == nil {
		return ""
	}
	return m.model
}

func (m *OpenAICompatibleLLM) SupportsStreaming() bool { return m != nil }

// GenerateContent converts one ADK request into one non-streaming Chat
// Completions request and yields at most one ADK response.
func (m *OpenAICompatibleLLM) GenerateContent(ctx context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m == nil {
			yield(nil, errors.New("OpenAI-compatible model is nil"))
			return
		}

		params, err := m.requestParams(request)
		if err != nil {
			yield(nil, err)
			return
		}
		if stream {
			m.generateStream(ctx, params, yield)
			return
		}
		completion, err := m.client.Chat.Completions.New(ctx, params)
		if err != nil {
			yield(nil, fmt.Errorf("OpenAI-compatible Chat Completions request failed: %w", err))
			return
		}

		response, err := responseFromCompletion(completion)
		if err != nil {
			yield(nil, err)
			return
		}
		yield(response, nil)
	}
}

func (m *OpenAICompatibleLLM) generateStream(ctx context.Context, params openai.ChatCompletionNewParams, yield func(*model.LLMResponse, error) bool) {
	stream := m.client.Chat.Completions.NewStreaming(ctx, params)
	accumulator := openai.ChatCompletionAccumulator{}
	for stream.Next() {
		chunk := stream.Current()
		if !accumulator.AddChunk(chunk) {
			yield(nil, &SSEError{Category: SSEErrorChunkConsistency, Err: errors.New("OpenAI-compatible stream contained inconsistent chunks"), FramePresent: true, PayloadPresent: true})
			return
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content == "" {
				continue
			}
			if !yield(&model.LLMResponse{
				Content:      genai.NewContentFromText(choice.Delta.Content, genai.RoleModel),
				ModelVersion: chunk.Model,
				Partial:      true,
			}, nil) {
				return
			}
			break
		}
	}
	if err := stream.Err(); err != nil {
		category := classifyStreamReadError(err)
		if category == SSEErrorDecode && m.metrics != nil {
			m.metrics.SSEDecodeFailures.Add(1)
		}
		yield(nil, &SSEError{Category: category, Err: err})
		return
	}
	response, err := responseFromCompletion(&accumulator.ChatCompletion)
	if err != nil {
		yield(nil, &SSEError{Category: SSEErrorFinalAggregation, Err: err, FramePresent: true, PayloadPresent: true})
		return
	}
	response.Partial = false
	response.TurnComplete = true
	yield(response, nil)
}

func responseFromCompletion(completion *openai.ChatCompletion) (*model.LLMResponse, error) {
	if completion == nil {
		return nil, ErrNoAssistantText
	}
	for _, choice := range completion.Choices {
		msg := choice.Message
		if string(msg.Role) != "assistant" {
			continue
		}

		parts := make([]*genai.Part, 0, len(msg.ToolCalls)+1)
		if strings.TrimSpace(msg.Content) != "" {
			parts = append(parts, genai.NewPartFromText(msg.Content))
		}
		for _, tc := range msg.ToolCalls {
			if tc.Type != "function" {
				return nil, fmt.Errorf("unsupported tool call type %q", tc.Type)
			}
			if strings.TrimSpace(tc.ID) == "" || strings.TrimSpace(tc.Function.Name) == "" {
				return nil, errors.New("tool call ID and function name are required")
			}
			var args map[string]any
			if strings.TrimSpace(tc.Function.Arguments) != "" {
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil || args == nil {
					if err == nil {
						err = errors.New("arguments must be a JSON object")
					}
					return nil, fmt.Errorf("decode tool call arguments for %q (call %s): %w", tc.Function.Name, tc.ID, err)
				}
			} else {
				return nil, fmt.Errorf("decode tool call arguments for %q (call %s): empty arguments", tc.Function.Name, tc.ID)
			}
			parts = append(parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   tc.ID,
					Name: tc.Function.Name,
					Args: args,
				},
			})
		}

		if len(parts) == 0 {
			continue
		}

		return &model.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: parts,
			},
			ModelVersion: completion.Model,
			FinishReason: finishReason(choice.FinishReason),
			TurnComplete: true,
		}, nil
	}
	return nil, ErrNoAssistantText
}

func finishReason(value string) genai.FinishReason {
	switch value {
	case "stop":
		return genai.FinishReasonStop
	case "length":
		return genai.FinishReasonMaxTokens
	case "content_filter":
		return genai.FinishReasonSafety
	case "tool_calls", "function_call":
		return genai.FinishReasonStop
	default:
		return genai.FinishReasonOther
	}
}
