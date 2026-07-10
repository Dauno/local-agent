// Package openaillm adapts OpenAI-compatible Chat Completions endpoints to the
// ADK model.LLM boundary.
package openaillm

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/openai/openai-go/v3"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

var (
	// ErrStreamingUnsupported is returned because the MVP only performs one
	// non-streaming Chat Completions request.
	ErrStreamingUnsupported = errors.New("streaming model responses are not supported")
	// ErrToolsUnsupported is returned before any provider call when ADK supplies
	// tools, function calls, or tool responses.
	ErrToolsUnsupported = errors.New("model tools and function parts are not supported")
	// ErrUnsupportedPart indicates non-text input content.
	ErrUnsupportedPart = errors.New("only text model content is supported")
	// ErrNoAssistantText indicates a successful provider response without usable
	// assistant text.
	ErrNoAssistantText = errors.New("model response contained no non-empty assistant text")
)

// OpenAICompatibleLLM implements ADK's model.LLM using non-streaming OpenAI
// Chat Completions.
type OpenAICompatibleLLM struct {
	client          openai.Client
	model           string
	reasoningEffort string
	extraBody       map[string]any
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
	}, nil
}

// Name returns the configured provider model identifier.
func (m *OpenAICompatibleLLM) Name() string {
	if m == nil {
		return ""
	}
	return m.model
}

// GenerateContent converts one ADK request into one non-streaming Chat
// Completions request and yields at most one ADK response.
func (m *OpenAICompatibleLLM) GenerateContent(ctx context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stream {
			yield(nil, ErrStreamingUnsupported)
			return
		}
		if m == nil {
			yield(nil, errors.New("OpenAI-compatible model is nil"))
			return
		}

		params, err := m.requestParams(request)
		if err != nil {
			yield(nil, err)
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

func responseFromCompletion(completion *openai.ChatCompletion) (*model.LLMResponse, error) {
	if completion == nil {
		return nil, ErrNoAssistantText
	}
	for _, choice := range completion.Choices {
		if string(choice.Message.Role) != "assistant" || strings.TrimSpace(choice.Message.Content) == "" {
			continue
		}
		return &model.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromText(choice.Message.Content)},
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
		return genai.FinishReasonUnexpectedToolCall
	default:
		return genai.FinishReasonOther
	}
}
