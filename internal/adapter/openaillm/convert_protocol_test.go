package openaillm

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestOpenAIConversionMatchesProviderProtocolForSharedContentRules(t *testing.T) {
	tests := []struct {
		name              string
		contents          []*genai.Content
		options           domain.ProtocolValidationOptions
		adapterError      string
		protocolRule      domain.ProtocolValidationRule
		adapterErrorIndex int
	}{
		{
			name: "text, call with text, and response",
			contents: []*genai.Content{
				genai.NewContentFromText("lookup", genai.RoleUser),
				{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						genai.NewPartFromText("I will look that up."),
						{FunctionCall: &genai.FunctionCall{
							ID: "call-1", Name: "lookup", Args: map[string]any{"query": "status"},
						}},
					},
				},
				openAIProtocolResponse(genai.RoleUser, "call-1", "lookup", map[string]any{"result": "ready"}),
			},
			options: openAIProtocolOptions(true),
		},
		{
			name: "supported image part",
			contents: []*genai.Content{{
				Role:  genai.RoleUser,
				Parts: []*genai.Part{genai.NewPartFromBytes(realTestPNG(t), "image/png")},
			}},
			options: openAIProtocolOptions(true),
		},
		{
			name:              "function call requires model role",
			contents:          []*genai.Content{openAIProtocolCall(genai.RoleUser, "call-1", "lookup", nil)},
			options:           openAIProtocolOptions(false),
			adapterError:      "function calls require model role",
			protocolRule:      "",
			adapterErrorIndex: 0,
		},
		{
			name: "function response requires user role",
			contents: []*genai.Content{
				openAIProtocolCall(genai.RoleModel, "call-1", "lookup", nil),
				openAIProtocolResponse(genai.RoleModel, "call-1", "lookup", nil),
			},
			options:           openAIProtocolOptions(false),
			adapterError:      "function responses require a user-role content with no text",
			protocolRule:      "",
			adapterErrorIndex: 1,
		},
		{
			name: "function response cannot share content with text",
			contents: []*genai.Content{
				openAIProtocolCall(genai.RoleModel, "call-1", "lookup", nil),
				{
					Role: genai.RoleUser,
					Parts: []*genai.Part{
						{FunctionResponse: &genai.FunctionResponse{ID: "call-1", Name: "lookup"}},
						genai.NewPartFromText("unexpected text"),
					},
				},
			},
			options:           openAIProtocolOptions(false),
			adapterError:      "function responses require a user-role content with no text",
			protocolRule:      "",
			adapterErrorIndex: 1,
		},
		{
			name: "content cannot mix function calls and responses",
			contents: []*genai.Content{{
				Role: genai.RoleModel,
				Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{ID: "call-1", Name: "lookup"}},
					{FunctionResponse: &genai.FunctionResponse{ID: "call-1", Name: "lookup"}},
				},
			}},
			options:           openAIProtocolOptions(false),
			adapterError:      "content cannot mix function calls and responses",
			protocolRule:      "",
			adapterErrorIndex: 0,
		},
		{
			name: "unsupported structured part",
			contents: []*genai.Content{{
				Role:  genai.RoleUser,
				Parts: []*genai.Part{genai.NewPartFromBytes([]byte("pdf"), "application/pdf")},
			}},
			options:           openAIProtocolOptions(false),
			adapterError:      ErrUnsupportedPart.Error(),
			protocolRule:      "",
			adapterErrorIndex: 0,
		},
		{
			name: "image requires user role",
			contents: []*genai.Content{{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromBytes(realTestPNG(t), "image/png")},
			}},
			options:           openAIProtocolOptions(false),
			adapterError:      "image content requires user role",
			protocolRule:      "",
			adapterErrorIndex: 0,
		},
		{
			name:              "unsupported content role",
			contents:          []*genai.Content{{Role: "tool", Parts: []*genai.Part{genai.NewPartFromText("invalid")}}},
			options:           openAIProtocolOptions(false),
			adapterError:      "unsupported ADK role",
			protocolRule:      domain.ProtocolRuleContentRole,
			adapterErrorIndex: 0,
		},
		{
			name:              "empty content",
			contents:          []*genai.Content{{Role: genai.RoleUser}},
			options:           openAIProtocolOptions(false),
			adapterError:      "content must have non-empty text",
			protocolRule:      "",
			adapterErrorIndex: 0,
		},
		{
			name: "empty part",
			contents: []*genai.Content{{
				Role:  genai.RoleUser,
				Parts: []*genai.Part{{}},
			}},
			options:           openAIProtocolOptions(false),
			adapterError:      "content must have non-empty text",
			protocolRule:      "",
			adapterErrorIndex: 0,
		},
		{
			name:              "function call requires ID and name",
			contents:          []*genai.Content{openAIProtocolCall(genai.RoleModel, "", "lookup", nil)},
			options:           openAIProtocolOptions(false),
			adapterError:      "function call ID and name are required",
			protocolRule:      domain.ProtocolRuleCallIdentity,
			adapterErrorIndex: 0,
		},
		{
			name: "function response requires ID and name",
			contents: []*genai.Content{
				openAIProtocolCall(genai.RoleModel, "call-1", "lookup", nil),
				openAIProtocolResponse(genai.RoleUser, "", "lookup", nil),
			},
			options:           openAIProtocolOptions(false),
			adapterError:      "function response ID and name are required",
			protocolRule:      domain.ProtocolRuleResponseIdentity,
			adapterErrorIndex: 1,
		},
		{
			name: "complete confirmation lifecycle",
			contents: []*genai.Content{
				openAIProtocolCall(genai.RoleModel, "call-1", "write", map[string]any{"path": "README.md"}),
				openAIProtocolResponse(genai.RoleUser, "call-1", "write", map[string]any{"error": "requires confirmation"}),
				openAIProtocolConfirmationCall("wrapper-1", "call-1", "write"),
				openAIProtocolResponse(genai.RoleUser, "wrapper-1", domain.ConfirmationFunctionName, map[string]any{"confirmed": true}),
				openAIProtocolResponse(genai.RoleUser, "call-1", "write", map[string]any{"result": "done"}),
			},
			options: openAIProtocolOptions(true),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapterErrIndex := test.adapterErrorIndex
			if adapterErrIndex == 0 && test.adapterError == "" {
				adapterErrIndex = -1
			}
			for index, content := range test.contents {
				_, err := contentToMessages(content)
				if index == adapterErrIndex {
					if err == nil || !strings.Contains(err.Error(), test.adapterError) {
						t.Fatalf("contentToMessages(%d) error = %v, want substring %q", index, err, test.adapterError)
					}
					continue
				}
				if err != nil {
					t.Fatalf("contentToMessages(%d) error = %v, want success", index, err)
				}
			}

			domainErr := domain.ValidateContentProtocol(domainContentsFromGenAI(t, test.contents), test.options)
			if test.protocolRule == "" {
				if domainErr != nil {
					t.Fatalf("ValidateContentProtocol() error = %v, want success", domainErr)
				}
				return
			}
			var validationErr *domain.ProtocolValidationError
			if !errors.As(domainErr, &validationErr) {
				t.Fatalf("ValidateContentProtocol() error = %v, want rule %q", domainErr, test.protocolRule)
			}
			if validationErr.Rule != test.protocolRule {
				t.Fatalf("ValidateContentProtocol() rule = %q, want %q", validationErr.Rule, test.protocolRule)
			}
		})
	}
}

func TestOpenAIConversionLeavesSequenceLedgerRulesToProtocol(t *testing.T) {
	tests := []struct {
		name         string
		contents     []*genai.Content
		wantProtocol domain.ProtocolValidationRule
	}{
		{
			name: "duplicate call ID",
			contents: []*genai.Content{
				openAIProtocolCall(genai.RoleModel, "call-1", "lookup", nil),
				openAIProtocolResponse(genai.RoleUser, "call-1", "lookup", nil),
				openAIProtocolCall(genai.RoleModel, "call-1", "write", nil),
			},
			wantProtocol: domain.ProtocolRuleDuplicateCall,
		},
		{
			name: "duplicate response ID",
			contents: []*genai.Content{
				openAIProtocolCall(genai.RoleModel, "call-1", "lookup", nil),
				openAIProtocolResponse(genai.RoleUser, "call-1", "lookup", nil),
				openAIProtocolResponse(genai.RoleUser, "call-1", "lookup", nil),
			},
			wantProtocol: domain.ProtocolRuleDuplicateResponse,
		},
		{
			name: "confirmation references unknown call",
			contents: []*genai.Content{
				openAIProtocolCall(genai.RoleModel, "call-1", "write", nil),
				openAIProtocolResponse(genai.RoleUser, "call-1", "write", map[string]any{"error": "requires confirmation"}),
				openAIProtocolConfirmationCall("wrapper-1", "missing-call", "write"),
			},
			wantProtocol: domain.ProtocolRuleConfirmationLifecycle,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for index, content := range test.contents {
				if _, err := contentToMessages(content); err != nil {
					t.Fatalf("contentToMessages(%d) error = %v, want success", index, err)
				}
			}
			var validationErr *domain.ProtocolValidationError
			err := domain.ValidateContentProtocol(domainContentsFromGenAI(t, test.contents), openAIProtocolOptions(false))
			if !errors.As(err, &validationErr) || validationErr.Rule != test.wantProtocol {
				t.Fatalf("ValidateContentProtocol() error = %v, want rule %q", err, test.wantProtocol)
			}
		})
	}
}

func TestOpenAIConversionSerializesFunctionPayloads(t *testing.T) {
	serializable := []struct {
		name    string
		content *genai.Content
	}{
		{
			name: "function call arguments",
			content: openAIProtocolCall(genai.RoleModel, "call-1", "lookup", map[string]any{
				"query":   "status",
				"filters": []any{map[string]any{"active": true}, 3},
			}),
		},
		{
			name: "function response data",
			content: openAIProtocolResponse(genai.RoleUser, "call-1", "lookup", map[string]any{
				"result": map[string]any{"value": "ready"},
			}),
		},
	}
	for _, test := range serializable {
		t.Run(test.name, func(t *testing.T) {
			messages, err := contentToMessages(test.content)
			if err != nil {
				t.Fatalf("contentToMessages() error = %v, want success", err)
			}
			if len(messages) == 0 {
				t.Fatal("contentToMessages() returned no messages")
			}
			if _, err := json.Marshal(messages); err != nil {
				t.Fatalf("converted messages are not serializable: %v", err)
			}
		})
	}

	nonSerializable := []struct {
		name string
		part *genai.Part
		want string
	}{
		{
			name: "function call unsupported type",
			part: &genai.Part{FunctionCall: &genai.FunctionCall{
				ID: "call-1", Name: "lookup", Args: map[string]any{"value": func() {}},
			}},
			want: "encode function call arguments: json: unsupported type: func()",
		},
		{
			name: "function call unsupported value",
			part: &genai.Part{FunctionCall: &genai.FunctionCall{
				ID: "call-1", Name: "lookup", Args: map[string]any{"value": math.NaN()},
			}},
			want: "encode function call arguments: json: unsupported value: NaN",
		},
		{
			name: "function response unsupported type",
			part: &genai.Part{FunctionResponse: &genai.FunctionResponse{
				ID: "call-1", Name: "lookup", Response: map[string]any{"value": make(chan int)},
			}},
			want: "encode function response: json: unsupported type: chan int",
		},
		{
			name: "function response unsupported value",
			part: &genai.Part{FunctionResponse: &genai.FunctionResponse{
				ID: "call-1", Name: "lookup", Response: map[string]any{"value": math.NaN()},
			}},
			want: "encode function response: json: unsupported value: NaN",
		},
	}
	for _, test := range nonSerializable {
		t.Run(test.name, func(t *testing.T) {
			content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{test.part}}
			if test.part.FunctionResponse != nil {
				content.Role = genai.RoleUser
			}
			_, err := contentToMessages(content)
			if err == nil || err.Error() != test.want {
				t.Fatalf("contentToMessages() error = %v, want %q", err, test.want)
			}
			_, err = (&OpenAICompatibleLLM{model: "test-model"}).requestParams(&model.LLMRequest{Contents: []*genai.Content{content}})
			wantRequestError := "convert content 0: " + test.want
			if err == nil || err.Error() != wantRequestError {
				t.Fatalf("requestParams() error = %v, want %q", err, wantRequestError)
			}
		})
	}
}

func openAIProtocolOptions(complete bool) domain.ProtocolValidationOptions {
	return domain.ProtocolValidationOptions{
		RequireComplete:            complete,
		AllowConfirmationLifecycle: true,
	}
}

func openAIProtocolCall(role, id, name string, args map[string]any) *genai.Content {
	return &genai.Content{Role: role, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
		ID: id, Name: name, Args: args,
	}}}}
}

func openAIProtocolResponse(role, id, name string, response map[string]any) *genai.Content {
	return &genai.Content{Role: role, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
		ID: id, Name: name, Response: response,
	}}}}
}

func openAIProtocolConfirmationCall(id, originalID, originalName string) *genai.Content {
	return openAIProtocolCall(genai.RoleModel, id, domain.ConfirmationFunctionName, map[string]any{
		"originalFunctionCall": map[string]any{"id": originalID, "name": originalName},
	})
}

func domainContentsFromGenAI(t *testing.T, contents []*genai.Content) []domain.Content {
	t.Helper()
	result := make([]domain.Content, len(contents))
	for contentIndex, content := range contents {
		if content == nil {
			t.Fatalf("content %d is nil", contentIndex)
		}
		result[contentIndex] = domain.Content{Role: domain.ContentRole(content.Role), Parts: make([]domain.ContentPart, len(content.Parts))}
		for partIndex, part := range content.Parts {
			if part == nil {
				t.Fatalf("content part %d.%d is nil", contentIndex, partIndex)
			}
			if part.FunctionCall != nil {
				result[contentIndex].Parts[partIndex].FunctionCall = &domain.FunctionCall{
					ID: part.FunctionCall.ID, Name: part.FunctionCall.Name, Args: part.FunctionCall.Args,
				}
			}
			if part.FunctionResponse != nil {
				result[contentIndex].Parts[partIndex].FunctionResponse = &domain.FunctionResponse{
					ID: part.FunctionResponse.ID, Name: part.FunctionResponse.Name, Response: part.FunctionResponse.Response, WillContinue: part.FunctionResponse.WillContinue,
				}
			}
			if part.InlineData != nil || part.FileData != nil || part.ToolCall != nil || part.ToolResponse != nil || part.CodeExecutionResult != nil || part.ExecutableCode != nil || part.VideoMetadata != nil || part.MediaResolution != nil || part.Thought || len(part.ThoughtSignature) > 0 || len(part.PartMetadata) > 0 {
				encoded, err := json.Marshal(part)
				if err != nil {
					t.Fatalf("encode content part %d.%d: %v", contentIndex, partIndex, err)
				}
				result[contentIndex].Parts[partIndex].StructuredJSON = encoded
			}
			result[contentIndex].Parts[partIndex].Text = part.Text
		}
	}
	return result
}
