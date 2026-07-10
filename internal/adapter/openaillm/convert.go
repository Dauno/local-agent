package openaillm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func (m *OpenAICompatibleLLM) requestParams(request *model.LLMRequest) (openai.ChatCompletionNewParams, error) {
	if request == nil {
		return openai.ChatCompletionNewParams{}, errors.New("ADK model request is nil")
	}
	if len(request.Tools) > 0 {
		return openai.ChatCompletionNewParams{}, ErrToolsUnsupported
	}

	messages := make([]openai.ChatCompletionMessageParamUnion, 0, len(request.Contents)+1)
	if request.Config != nil && request.Config.SystemInstruction != nil {
		text, err := textFromContent(request.Config.SystemInstruction)
		if err != nil {
			return openai.ChatCompletionNewParams{}, fmt.Errorf("convert system instruction: %w", err)
		}
		if strings.TrimSpace(text) != "" {
			messages = append(messages, openai.SystemMessage(text))
		}
	}
	for index, content := range request.Contents {
		text, err := textFromContent(content)
		if err != nil {
			return openai.ChatCompletionNewParams{}, fmt.Errorf("convert content %d: %w", index, err)
		}
		switch content.Role {
		case "", genai.RoleUser:
			messages = append(messages, openai.UserMessage(text))
		case genai.RoleModel:
			messages = append(messages, openai.AssistantMessage(text))
		default:
			return openai.ChatCompletionNewParams{}, fmt.Errorf("convert content %d: unsupported ADK role %q", index, content.Role)
		}
	}
	if len(messages) == 0 {
		return openai.ChatCompletionNewParams{}, errors.New("ADK model request contains no text messages")
	}

	params := openai.ChatCompletionNewParams{
		Messages: messages,
		Model:    openai.ChatModel(m.model),
	}
	if m.reasoningEffort != "" {
		params.ReasoningEffort = openai.ReasoningEffort(m.reasoningEffort)
	}
	if request.Config != nil {
		if err := applyGenerateConfig(&params, request.Config); err != nil {
			return openai.ChatCompletionNewParams{}, err
		}
	}
	if len(m.extraBody) > 0 {
		params.SetExtraFields(m.extraBody)
	}
	return params, nil
}

func textFromContent(content *genai.Content) (string, error) {
	if content == nil {
		return "", ErrUnsupportedPart
	}
	var text strings.Builder
	for _, part := range content.Parts {
		if part == nil {
			return "", ErrUnsupportedPart
		}
		if part.FunctionCall != nil || part.FunctionResponse != nil || part.ToolCall != nil || part.ToolResponse != nil {
			return "", ErrToolsUnsupported
		}

		encoded, err := json.Marshal(part)
		if err != nil {
			return "", fmt.Errorf("inspect ADK content part: %w", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			return "", fmt.Errorf("inspect ADK content part: %w", err)
		}
		delete(fields, "text")
		if len(fields) != 0 {
			return "", ErrUnsupportedPart
		}
		text.WriteString(part.Text)
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", errors.New("text model content must not be empty")
	}
	return text.String(), nil
}

func applyGenerateConfig(params *openai.ChatCompletionNewParams, cfg *genai.GenerateContentConfig) error {
	if len(cfg.Tools) > 0 || cfg.ToolConfig != nil {
		return ErrToolsUnsupported
	}
	if cfg.Temperature != nil {
		params.Temperature = openai.Float(float64(*cfg.Temperature))
	}
	if cfg.TopP != nil {
		params.TopP = openai.Float(float64(*cfg.TopP))
	}
	if cfg.MaxOutputTokens < 0 {
		return errors.New("max output tokens must not be negative")
	}
	if cfg.MaxOutputTokens > 0 {
		params.MaxTokens = openai.Int(int64(cfg.MaxOutputTokens))
	}
	if len(cfg.StopSequences) > 0 {
		params.Stop.OfStringArray = append([]string(nil), cfg.StopSequences...)
	}
	if cfg.PresencePenalty != nil {
		params.PresencePenalty = openai.Float(float64(*cfg.PresencePenalty))
	}
	if cfg.FrequencyPenalty != nil {
		params.FrequencyPenalty = openai.Float(float64(*cfg.FrequencyPenalty))
	}
	if cfg.Seed != nil {
		params.Seed = openai.Int(int64(*cfg.Seed))
	}
	if cfg.CandidateCount < 0 {
		return errors.New("candidate count must not be negative")
	}
	if cfg.CandidateCount > 0 {
		params.N = openai.Int(int64(cfg.CandidateCount))
	}
	if cfg.ResponseLogprobs {
		params.Logprobs = openai.Bool(true)
	}
	if cfg.Logprobs != nil {
		params.Logprobs = openai.Bool(true)
		params.TopLogprobs = openai.Int(int64(*cfg.Logprobs))
	}
	return applyResponseFormat(params, cfg)
}

func applyResponseFormat(params *openai.ChatCompletionNewParams, cfg *genai.GenerateContentConfig) error {
	if cfg.ResponseSchema != nil && cfg.ResponseJsonSchema != nil {
		return errors.New("response_schema and response_json_schema cannot both be configured")
	}
	if cfg.ResponseMIMEType != "" && cfg.ResponseMIMEType != "text/plain" && cfg.ResponseMIMEType != "application/json" {
		return fmt.Errorf("unsupported response MIME type %q", cfg.ResponseMIMEType)
	}

	var schema any
	if cfg.ResponseJsonSchema != nil {
		schema = cfg.ResponseJsonSchema
	} else if cfg.ResponseSchema != nil {
		converted, err := convertSchema(cfg.ResponseSchema)
		if err != nil {
			return err
		}
		schema = converted
	}
	if schema != nil {
		format := shared.ResponseFormatJSONSchemaParam{
			JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
				Name:   "response",
				Schema: schema,
			},
		}
		params.ResponseFormat.OfJSONSchema = &format
		return nil
	}
	if cfg.ResponseMIMEType == "application/json" {
		format := shared.NewResponseFormatJSONObjectParam()
		params.ResponseFormat.OfJSONObject = &format
	}
	return nil
}

func convertSchema(schema *genai.Schema) (any, error) {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("encode ADK response schema: %w", err)
	}
	var result any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, fmt.Errorf("decode ADK response schema: %w", err)
	}
	normalizeSchemaTypes(result)
	return result, nil
}

func normalizeSchemaTypes(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "type" {
				if name, ok := child.(string); ok {
					if name == string(genai.TypeUnspecified) {
						delete(typed, key)
					} else {
						typed[key] = strings.ToLower(name)
					}
				}
				continue
			}
			normalizeSchemaTypes(child)
		}
	case []any:
		for _, child := range typed {
			normalizeSchemaTypes(child)
		}
	}
}
