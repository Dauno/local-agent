package openaillm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func (m *OpenAICompatibleLLM) requestParams(request *model.LLMRequest) (openai.ChatCompletionNewParams, error) {
	if request == nil {
		return openai.ChatCompletionNewParams{}, errors.New("ADK model request is nil")
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
		converted, err := contentToMessages(content)
		if err != nil {
			return openai.ChatCompletionNewParams{}, fmt.Errorf("convert content %d: %w", index, err)
		}
		messages = append(messages, converted...)
	}
	if len(messages) == 0 {
		return openai.ChatCompletionNewParams{}, errors.New("ADK model request contains no messages")
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

func contentToMessages(content *genai.Content) ([]openai.ChatCompletionMessageParamUnion, error) {
	if content == nil {
		return nil, ErrUnsupportedPart
	}

	var texts []string
	var functionCalls []*genai.FunctionCall
	var functionResponses []*genai.FunctionResponse

	for _, part := range content.Parts {
		if part == nil {
			return nil, ErrUnsupportedPart
		}
		switch {
		case part.FunctionCall != nil:
			functionCalls = append(functionCalls, part.FunctionCall)
		case part.FunctionResponse != nil:
			functionResponses = append(functionResponses, part.FunctionResponse)
		default:
			if part.ToolCall != nil || part.ToolResponse != nil || part.InlineData != nil || part.FileData != nil || part.CodeExecutionResult != nil || part.ExecutableCode != nil || part.VideoMetadata != nil || part.MediaResolution != nil || part.Thought || len(part.ThoughtSignature) > 0 || len(part.PartMetadata) > 0 {
				return nil, ErrUnsupportedPart
			}
			texts = append(texts, part.Text)
		}
	}

	if len(functionCalls) > 0 && len(functionResponses) > 0 {
		return nil, errors.New("content cannot mix function calls and responses")
	}
	if len(functionCalls) > 0 {
		if content.Role != genai.RoleModel {
			return nil, fmt.Errorf("function calls require model role, got %q", content.Role)
		}
		assistant := &openai.ChatCompletionAssistantMessageParam{
			ToolCalls: make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(functionCalls)),
		}
		for _, call := range functionCalls {
			if call == nil || strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
				return nil, errors.New("function call ID and name are required")
			}
			args := call.Args
			if args == nil {
				args = map[string]any{}
			}
			encoded, err := json.Marshal(args)
			if err != nil {
				return nil, fmt.Errorf("encode function call arguments: %w", err)
			}
			assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID:       call.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{Name: call.Name, Arguments: string(encoded)},
				},
			})
		}
		if len(texts) > 0 {
			assistant.Content.OfString = param.NewOpt(strings.Join(texts, ""))
		}
		return []openai.ChatCompletionMessageParamUnion{{OfAssistant: assistant}}, nil
	}

	if len(functionResponses) > 0 {
		if content.Role != genai.RoleUser || len(texts) > 0 {
			return nil, errors.New("function responses require a user-role content with no text")
		}
		messages := make([]openai.ChatCompletionMessageParamUnion, 0, len(functionResponses))
		for _, response := range functionResponses {
			if response == nil || strings.TrimSpace(response.ID) == "" || strings.TrimSpace(response.Name) == "" {
				return nil, errors.New("function response ID and name are required")
			}
			encoded, err := json.Marshal(response.Response)
			if err != nil {
				return nil, fmt.Errorf("encode function response: %w", err)
			}
			messages = append(messages, openai.ToolMessage(string(encoded), response.ID))
		}
		return messages, nil
	}

	text := strings.Join(texts, "")
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("content must have non-empty text")
	}
	switch content.Role {
	case "", genai.RoleUser:
		return []openai.ChatCompletionMessageParamUnion{openai.UserMessage(text)}, nil
	case genai.RoleModel:
		return []openai.ChatCompletionMessageParamUnion{openai.AssistantMessage(text)}, nil
	default:
		return nil, fmt.Errorf("unsupported ADK role %q", content.Role)
	}
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
			return "", ErrUnsupportedPart
		}
		if part.InlineData != nil || part.FileData != nil || part.CodeExecutionResult != nil || part.ExecutableCode != nil || part.VideoMetadata != nil || part.MediaResolution != nil || part.Thought || len(part.ThoughtSignature) > 0 || len(part.PartMetadata) > 0 {
			return "", ErrUnsupportedPart
		}
		text.WriteString(part.Text)
	}
	return text.String(), nil
}

func applyGenerateConfig(params *openai.ChatCompletionNewParams, cfg *genai.GenerateContentConfig) error {
	if cfg.ToolConfig != nil && len(cfg.Tools) == 0 {
		return errors.New("function calling configuration requires function declarations")
	}
	if len(cfg.Tools) > 0 {
		tools := make([]openai.ChatCompletionToolUnionParam, 0, len(cfg.Tools))
		seenNames := make(map[string]struct{})
		for toolIndex, tool := range cfg.Tools {
			if tool == nil {
				return fmt.Errorf("tool %d is nil", toolIndex)
			}
			if tool.Retrieval != nil || tool.ComputerUse != nil || tool.FileSearch != nil || tool.GoogleSearch != nil || tool.GoogleMaps != nil || tool.CodeExecution != nil || tool.EnterpriseWebSearch != nil || tool.GoogleSearchRetrieval != nil || tool.ParallelAISearch != nil || tool.URLContext != nil || len(tool.MCPServers) > 0 {
				return fmt.Errorf("tool %d uses an unsupported non-function variant", toolIndex)
			}
			if len(tool.FunctionDeclarations) == 0 {
				return fmt.Errorf("tool %d has no function declarations", toolIndex)
			}
			for _, decl := range tool.FunctionDeclarations {
				if decl == nil {
					return errors.New("function declaration is nil")
				}
				if strings.TrimSpace(decl.Name) == "" {
					return errors.New("function declaration name is required")
				}
				if _, exists := seenNames[decl.Name]; exists {
					return fmt.Errorf("duplicate function declaration %q", decl.Name)
				}
				seenNames[decl.Name] = struct{}{}
				var paramsSchema shared.FunctionParameters
				if decl.ParametersJsonSchema != nil && decl.Parameters != nil {
					return fmt.Errorf("function declaration %q has both parameter schemas", decl.Name)
				}
				if decl.ParametersJsonSchema != nil {
					encoded, err := json.Marshal(decl.ParametersJsonSchema)
					if err != nil {
						return fmt.Errorf("encode function declaration %q schema: %w", decl.Name, err)
					}
					var schema map[string]any
					if err := json.Unmarshal(encoded, &schema); err != nil || schema == nil {
						return fmt.Errorf("function declaration %q schema must be an object", decl.Name)
					}
					paramsSchema = shared.FunctionParameters(schema)
				} else if decl.Parameters != nil {
					schema, err := convertSchema(decl.Parameters)
					if err != nil {
						return fmt.Errorf("convert function declaration %q schema: %w", decl.Name, err)
					}
					objectSchema, ok := schema.(map[string]any)
					if !ok {
						return fmt.Errorf("function declaration %q schema must be an object", decl.Name)
					}
					paramsSchema = shared.FunctionParameters(objectSchema)
				}
				tools = append(tools, openai.ChatCompletionToolUnionParam{
					OfFunction: &openai.ChatCompletionFunctionToolParam{
						Function: shared.FunctionDefinitionParam{
							Name:        decl.Name,
							Description: param.NewOpt(decl.Description),
							Parameters:  paramsSchema,
						},
					},
				})
			}
		}
		params.Tools = tools
		if cfg.ToolConfig != nil {
			functionConfig := cfg.ToolConfig.FunctionCallingConfig
			if len(functionConfig.AllowedFunctionNames) > 0 || functionConfig.StreamFunctionCallArguments != nil {
				return errors.New("restricted or streamed function calling is not supported")
			}
			switch functionConfig.Mode {
			case genai.FunctionCallingConfigModeAuto:
				params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("auto")}
			case genai.FunctionCallingConfigModeAny:
				params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("required")}
			case genai.FunctionCallingConfigModeNone:
				params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("none")}
			case genai.FunctionCallingConfigModeUnspecified:
				params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("auto")}
			default:
				return fmt.Errorf("unsupported function calling mode %q", functionConfig.Mode)
			}
		}
		params.ParallelToolCalls = openai.Bool(false)
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
