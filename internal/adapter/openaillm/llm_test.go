package openaillm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestGenerateContentSendsConfiguredChatCompletionAndReturnsOnlyAssistantText(t *testing.T) {
	t.Parallel()

	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		captured <- capturedRequest{
			path:       request.URL.Path,
			authorize:  request.Header.Get("Authorization"),
			clientName: request.Header.Get("X-Client-Name"),
			body:       body,
		}
		writeJSON(t, writer, http.StatusOK, map[string]any{
			"id":      "completion-1",
			"object":  "chat.completion",
			"created": 1,
			"model":   "provider-model-version",
			"choices": []any{
				map[string]any{
					"index":         0,
					"finish_reason": "stop",
					"message": map[string]any{
						"role":    "user",
						"content": "must not be selected",
					},
				},
				map[string]any{
					"index":         1,
					"finish_reason": "stop",
					"message": map[string]any{
						"role":    "assistant",
						"content": "  ",
					},
				},
				map[string]any{
					"index":         2,
					"finish_reason": "stop",
					"message": map[string]any{
						"role":              "assistant",
						"content":           "Visible answer",
						"reasoning_content": "hidden chain of thought",
					},
				},
			},
		})
	}))
	t.Cleanup(server.Close)

	extraBody := map[string]any{"thinking": map[string]any{"type": "enabled"}}
	headers := map[string]string{"X-Client-Name": "local-agent"}
	llm, err := New(
		WithAPIKey("test-api-key"),
		WithBaseURL(server.URL+"/v1"),
		WithHeaders(headers),
		WithModel("configured-model"),
		WithReasoningEffort("high"),
		WithExtraBody(extraBody),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// Options must own immutable copies of caller-supplied maps.
	headers["X-Client-Name"] = "mutated"
	extraBody["thinking"].(map[string]any)["type"] = "disabled"

	temperature := float32(0.25)
	topP := float32(0.8)
	request := &model.LLMRequest{
		Contents: []*genai.Content{
			{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hello "}, {Text: "world"}}},
			genai.NewContentFromText("previous answer", genai.RoleModel),
			genai.NewContentFromText("new question", genai.RoleUser),
		},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("system instruction", genai.RoleUser),
			Temperature:       &temperature,
			TopP:              &topP,
			MaxOutputTokens:   321,
			StopSequences:     []string{"END", "STOP"},
			ResponseMIMEType:  "application/json",
			ResponseSchema: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"answer": {Type: genai.TypeString},
				},
				Required: []string{"answer"},
			},
		},
	}

	response, generateErr, yields := collect(llm.GenerateContent(context.Background(), request, false))
	if generateErr != nil {
		t.Fatalf("GenerateContent() error = %v", generateErr)
	}
	if yields != 1 {
		t.Fatalf("GenerateContent() yielded %d values, want 1", yields)
	}
	if response == nil || response.Content == nil || len(response.Content.Parts) != 1 {
		t.Fatalf("GenerateContent() response = %#v", response)
	}
	if response.Content.Role != genai.RoleModel || response.Content.Parts[0].Text != "Visible answer" {
		t.Fatalf("assistant content = %#v", response.Content)
	}
	if strings.Contains(fmt.Sprintf("%#v", response), "hidden chain of thought") {
		t.Fatal("reasoning_content leaked into the ADK response")
	}
	if response.ModelVersion != "provider-model-version" || response.FinishReason != genai.FinishReasonStop || !response.TurnComplete {
		t.Fatalf("response metadata = %#v", response)
	}

	received := <-captured
	if received.path != "/v1/chat/completions" {
		t.Fatalf("request path = %q", received.path)
	}
	if received.authorize != "Bearer test-api-key" {
		t.Fatalf("Authorization = %q", received.authorize)
	}
	if received.clientName != "local-agent" {
		t.Fatalf("X-Client-Name = %q", received.clientName)
	}
	assertJSONValue(t, received.body, "model", "configured-model")
	assertJSONValue(t, received.body, "reasoning_effort", "high")
	assertJSONValue(t, received.body, "temperature", float64(0.25))
	assertJSONValue(t, received.body, "top_p", float64(float32(0.8)))
	assertJSONValue(t, received.body, "max_tokens", float64(321))
	if _, present := received.body["stream"]; present {
		t.Fatal("non-streaming request unexpectedly serialized stream")
	}

	thinking := received.body["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("extra body thinking = %#v", thinking)
	}
	stops := received.body["stop"].([]any)
	if fmt.Sprint(stops) != "[END STOP]" {
		t.Fatalf("stop = %#v", stops)
	}
	messages := received.body["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	assertMessage(t, messages[0], "system", "system instruction")
	assertMessage(t, messages[1], "user", "hello world")
	assertMessage(t, messages[2], "assistant", "previous answer")
	assertMessage(t, messages[3], "user", "new question")

	responseFormat := received.body["response_format"].(map[string]any)
	if responseFormat["type"] != "json_schema" {
		t.Fatalf("response_format = %#v", responseFormat)
	}
	jsonSchema := responseFormat["json_schema"].(map[string]any)
	if jsonSchema["name"] != "response" {
		t.Fatalf("json_schema.name = %#v", jsonSchema["name"])
	}
	schema := jsonSchema["schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("schema.type = %#v, want object", schema["type"])
	}
	answer := schema["properties"].(map[string]any)["answer"].(map[string]any)
	if answer["type"] != "string" {
		t.Fatalf("answer schema = %#v", answer)
	}
}

func TestGenerateContentReturnsProviderAndEmptyResponseErrors(t *testing.T) {
	t.Parallel()

	t.Run("provider error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(t, writer, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"message": "invalid request", "type": "invalid_request_error"},
			})
		}))
		t.Cleanup(server.Close)
		llm := mustTestLLM(t, server.URL)
		_, err, yields := collect(llm.GenerateContent(context.Background(), textRequest(), false))
		if err == nil || !strings.Contains(err.Error(), "Chat Completions request failed") || yields != 1 {
			t.Fatalf("GenerateContent() = err %v, yields %d", err, yields)
		}
	})

	t.Run("no assistant text", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(t, writer, http.StatusOK, map[string]any{
				"id": "empty", "object": "chat.completion", "created": 1, "model": "test", "choices": []any{},
			})
		}))
		t.Cleanup(server.Close)
		llm := mustTestLLM(t, server.URL)
		_, err, yields := collect(llm.GenerateContent(context.Background(), textRequest(), false))
		if !errors.Is(err, ErrNoAssistantText) || yields != 1 {
			t.Fatalf("GenerateContent() = err %v, yields %d", err, yields)
		}
	})
}

func TestGenerateContentRejectsUnsupportedRequestsBeforeHTTP(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(t, writer, http.StatusInternalServerError, map[string]any{})
	}))
	t.Cleanup(server.Close)
	llm := mustTestLLM(t, server.URL)

	tests := []struct {
		name   string
		req    *model.LLMRequest
		stream bool
		want   error
	}{
		{name: "stream", req: textRequest(), stream: true, want: ErrStreamingUnsupported},
		{name: "request tools", req: &model.LLMRequest{Contents: textRequest().Contents, Tools: map[string]any{"tool": true}}, want: ErrToolsUnsupported},
		{name: "function part", req: &model.LLMRequest{Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{genai.NewPartFromFunctionCall("fn", nil)}}}}, want: ErrToolsUnsupported},
		{name: "config tools", req: &model.LLMRequest{Contents: textRequest().Contents, Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{{}}}}, want: ErrToolsUnsupported},
		{name: "non text part", req: &model.LLMRequest{Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{genai.NewPartFromBytes([]byte("image"), "image/png")}}}}, want: ErrUnsupportedPart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err, yields := collect(llm.GenerateContent(context.Background(), tt.req, tt.stream))
			if !errors.Is(err, tt.want) || yields != 1 {
				t.Fatalf("GenerateContent() = err %v, yields %d; want %v", err, yields, tt.want)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("unsupported requests made %d HTTP calls", calls.Load())
	}
}

func TestNewValidatesOptionsWithoutExposingValues(t *testing.T) {
	t.Parallel()

	valid := []Option{WithAPIKey("key"), WithBaseURL("https://example.com/v1"), WithModel("model")}
	tests := []struct {
		name    string
		options []Option
	}{
		{name: "missing options"},
		{name: "nil option", options: append(append([]Option{}, valid...), nil)},
		{name: "empty key", options: []Option{WithAPIKey(" ")}},
		{name: "invalid base URL", options: []Option{WithBaseURL("relative")}},
		{name: "base URL credentials", options: []Option{WithBaseURL("https://user:password@example.com")}},
		{name: "base URL fragment", options: []Option{WithBaseURL("https://example.com/v1#fragment")}},
		{name: "invalid header", options: append(append([]Option{}, valid...), WithHeaders(map[string]string{"Bad\nHeader": "value"}))},
		{name: "header space", options: append(append([]Option{}, valid...), WithHeaders(map[string]string{"Bad Header": "value"}))},
		{name: "header injection", options: append(append([]Option{}, valid...), WithHeaders(map[string]string{"X-Test": "value\r\ninjected"}))},
		{name: "sensitive header", options: append(append([]Option{}, valid...), WithHeaders(map[string]string{"Authorization": "Bearer secret"}))},
		{name: "reserved extra field", options: append(append([]Option{}, valid...), WithExtraBody(map[string]any{"stream": true}))},
		{name: "non JSON extra field", options: append(append([]Option{}, valid...), WithExtraBody(map[string]any{"invalid": make(chan int)}))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.options...); err == nil {
				t.Fatal("New() unexpectedly succeeded")
			}
		})
	}
}

type capturedRequest struct {
	path       string
	authorize  string
	clientName string
	body       map[string]any
}

func mustTestLLM(t *testing.T, baseURL string) *OpenAICompatibleLLM {
	t.Helper()
	llm, err := New(WithAPIKey("test-key"), WithBaseURL(baseURL), WithModel("test-model"))
	if err != nil {
		t.Fatal(err)
	}
	return llm
}

func textRequest() *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}
}

func collect(sequence iter.Seq2[*model.LLMResponse, error]) (*model.LLMResponse, error, int) {
	var response *model.LLMResponse
	var resultErr error
	count := 0
	for current, err := range sequence {
		count++
		response = current
		resultErr = err
	}
	return response, resultErr, count
}

func writeJSON(t *testing.T, writer http.ResponseWriter, status int, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("write test response: %v", err)
	}
}

func assertJSONValue(t *testing.T, body map[string]any, key string, want any) {
	t.Helper()
	if got := body[key]; got != want {
		t.Fatalf("%s = %#v, want %#v", key, got, want)
	}
}

func assertMessage(t *testing.T, value any, role, content string) {
	t.Helper()
	message := value.(map[string]any)
	if message["role"] != role || message["content"] != content {
		t.Fatalf("message = %#v, want role %q content %q", message, role, content)
	}
}
