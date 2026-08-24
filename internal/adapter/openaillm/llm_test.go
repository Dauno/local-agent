package openaillm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"iter"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/openai/openai-go/v3"

	"github.com/Dauno/slack-local-agent/internal/adapter/adkagent"
	"github.com/Dauno/slack-local-agent/internal/adapter/tokencounter"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
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
	configureTestGuard(t, llm)
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

	response, yields, generateErr := collect(llm.GenerateContent(context.Background(), request, false))
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

func TestRequestParamsUsesProfileDefaultMaxOutputTokens(t *testing.T) {
	llm := &OpenAICompatibleLLM{model: "configured-model", defaultMaxOutputTokens: 32_000}
	params, err := llm.requestParams(&model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if got, ok := body["max_tokens"].(float64); !ok || got != 32_000 {
		t.Fatalf("max_tokens = %#v, want 32000", body["max_tokens"])
	}
}

func TestGenerateContentStreamsTrueTextDeltasAndAuthoritativeFinal(t *testing.T) {
	t.Parallel()
	requestBody := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		requestBody <- body
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: {\"id\":\"completion-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"provider-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"},\"finish_reason\":\"\"}]}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"id\":\"completion-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"provider-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"\"}]}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"id\":\"completion-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"provider-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	llm, err := New(WithAPIKey("test-api-key"), WithBaseURL(server.URL+"/v1"), WithModel("configured-model"))
	if err != nil {
		t.Fatal(err)
	}
	configureTestGuard(t, llm)

	var responses []*model.LLMResponse
	for response, err := range llm.GenerateContent(context.Background(), textRequest(), true) {
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, response)
	}
	if len(responses) != 3 || !responses[0].Partial || !responses[1].Partial || responses[2].Partial {
		t.Fatalf("responses=%#v", responses)
	}
	if responses[0].Content.Parts[0].Text != "Hel" || responses[1].Content.Parts[0].Text != "lo" || responses[2].Content.Parts[0].Text != "Hello" || !responses[2].TurnComplete {
		t.Fatalf("streamed responses=%#v", responses)
	}
	body := <-requestBody
	if body["stream"] != true {
		t.Fatalf("stream=%#v", body["stream"])
	}
}

func TestGenerateContentIgnoresSSEKeepAliveAndRetryBlocks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("test server does not support flushing")
		}
		_, _ = fmt.Fprint(writer, ": PROCESSING\n\nretry: 3000\n\n")
		flusher.Flush()
		_, _ = fmt.Fprint(writer, "data: {\"id\":\"completion-keepalive\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":\"\"}]}\n\n")
		_, _ = fmt.Fprint(writer, ": one\n\n: two\n\nretry: 3000\n\n")
		flusher.Flush()
		_, _ = fmt.Fprint(writer, "data: {\"id\":\"completion-keepalive\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" after keep-alive\"},\"finish_reason\":\"\"}]}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"id\":\"completion-keepalive\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	llm := mustTestLLM(t, server.URL)

	var responses []*model.LLMResponse
	for response, err := range llm.GenerateContent(context.Background(), textRequest(), true) {
		if err != nil {
			t.Fatalf("keep-alive stream error = %v", err)
		}
		responses = append(responses, response)
	}
	if len(responses) != 3 || responses[0].Content.Parts[0].Text != "Hello" || responses[1].Content.Parts[0].Text != " after keep-alive" || responses[2].Content.Parts[0].Text != "Hello after keep-alive" {
		t.Fatalf("keep-alive responses = %#v", responses)
	}
}

func TestGenerateContentAccumulatesToolCallChunksAfterEmptySSEBlocks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, ": PROCESSING\n\nretry: 3000\n\n")
		writeSSEChunk(writer, map[string]any{"id": "completion-tool", "object": "chat.completion.chunk", "created": 1, "model": "test", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"query":"sta`}}}}, "finish_reason": ""}}})
		_, _ = fmt.Fprint(writer, ": gap\n\n: gap2\n\n")
		writeSSEChunk(writer, map[string]any{"id": "completion-tool", "object": "chat.completion.chunk", "created": 1, "model": "test", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": `tus"}`}}}}, "finish_reason": ""}}})
		writeSSEChunk(writer, map[string]any{"id": "completion-tool", "object": "chat.completion.chunk", "created": 1, "model": "test", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})
		_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	llm := mustTestLLM(t, server.URL)
	request := textRequest()
	request.Config = &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "lookup"}}}}}
	response, yields, err := collect(llm.GenerateContent(context.Background(), request, true))
	if err != nil || yields != 1 || response == nil || response.Content == nil || len(response.Content.Parts) != 1 {
		t.Fatalf("tool stream = %#v, %v, yields=%d", response, err, yields)
	}
	call := response.Content.Parts[0].FunctionCall
	if call == nil || call.ID != "call-1" || call.Name != "lookup" || call.Args["query"] != "status" {
		t.Fatalf("accumulated tool call = %#v", call)
	}
}

func TestGenerateContentMalformedNonEmptySSEFailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: {\"id\":\"truncated\"\n\n")
	}))
	t.Cleanup(server.Close)
	llm := mustTestLLM(t, server.URL)
	_, yields, err := collect(llm.GenerateContent(context.Background(), textRequest(), true))
	if err == nil || yields != 1 {
		t.Fatalf("malformed SSE = err %v, yields %d", err, yields)
	}
	var streamErr *SSEError
	if !errors.As(err, &streamErr) || streamErr.Category != SSEErrorDecode || streamErr.FramePresent || streamErr.PayloadPresent {
		t.Fatalf("malformed SSE classification = %#v, %v", streamErr, err)
	}
}

func TestGenerateContentEmptySSEDataIsTerminalWithoutReplay(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: \n\n")
		_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	_, yields, err := collect(mustTestLLM(t, server.URL).GenerateContent(context.Background(), textRequest(), true))
	if err == nil || yields != 1 || requests.Load() != 1 {
		t.Fatalf("empty SSE data = err %v, yields %d, requests %d", err, yields, requests.Load())
	}
}

func TestGenerateContentCancellationAndTransportErrorsAreTerminal(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			<-request.Context().Done()
		}))
		t.Cleanup(server.Close)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, yields, err := collect(mustTestLLM(t, server.URL).GenerateContent(ctx, textRequest(), true))
		if err == nil || yields != 1 {
			t.Fatalf("cancellation = err %v, yields %d", err, yields)
		}
	})

	t.Run("transport EOF", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support hijacking")
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fmt.Fprintf(connection, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\ndata: {\"id\":\"partial\"\n\n")
			_ = connection.Close()
		}))
		t.Cleanup(server.Close)
		_, yields, err := collect(mustTestLLM(t, server.URL).GenerateContent(context.Background(), textRequest(), true))
		if err == nil || yields != 1 {
			t.Fatalf("transport EOF = err %v, yields %d", err, yields)
		}
	})
}

func TestRequestParamsPreservesTextAndImagePartOrder(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	llm := mustTestLLM(t, server.URL)
	request := &model.LLMRequest{Contents: []*genai.Content{{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			genai.NewPartFromBytes(realTestPNG(t), "image/png"),
			genai.NewPartFromText("between"),
			genai.NewPartFromBytes(realTestJPEG(t), "image/jpeg"),
		},
	}}}
	params, err := llm.requestParams(request)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	messages := body["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	got := make([]string, 0, len(content))
	for _, raw := range content {
		got = append(got, raw.(map[string]any)["type"].(string))
	}
	if strings.Join(got, ",") != "image_url,text,image_url" {
		t.Fatalf("content part order = %v", got)
	}
}

func TestConvertRequestBuildsRealAndCountableMultimodalProjection(t *testing.T) {
	llm := &OpenAICompatibleLLM{model: "test-model"}
	image := realTestPNG(t)
	request := &model.LLMRequest{Contents: []*genai.Content{{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			genai.NewPartFromText("caption: "),
			genai.NewPartFromBytes(image, "image/png"),
		},
	}}}
	converted, err := llm.convertRequest(request, false)
	if err != nil {
		t.Fatal(err)
	}
	if converted.envelope.SerializerID != port.SerializerOpenAIChatCompletionsMultimodalV2 {
		t.Fatalf("serializer = %q, want %q", converted.envelope.SerializerID, port.SerializerOpenAIChatCompletionsMultimodalV2)
	}
	if len(converted.envelope.Media) != 1 {
		t.Fatalf("media = %#v", converted.envelope.Media)
	}
	media := converted.envelope.Media[0]
	if media.MIMEType != "image/png" || media.Width != 4 || media.Height != 4 || media.Detail != "" {
		t.Fatalf("media metadata = %#v", media)
	}

	// The countable projection carries the fixed marker and no binary data.
	if !strings.Contains(converted.envelope.Serialized, mediaMarker) {
		t.Fatal("countable projection is missing the media marker")
	}
	if strings.Contains(converted.envelope.Serialized, "base64") {
		t.Fatal("countable projection contains base64")
	}
	if strings.Contains(converted.envelope.Serialized, "iVBOR") {
		t.Fatal("countable projection contains binary image data")
	}
	var countableBody map[string]any
	if err := json.Unmarshal([]byte(converted.envelope.Serialized), &countableBody); err != nil {
		t.Fatalf("countable projection is not JSON: %v", err)
	}
	innerParams, ok := countableBody["params"].(map[string]any)
	if !ok {
		t.Fatalf("countable projection params = %#v", countableBody["params"])
	}
	countableURL := countableImageURL(t, innerParams)
	if countableURL != mediaMarker {
		t.Fatalf("countable image URL = %q, want marker", countableURL)
	}
	countableOrder := countablePartOrder(t, innerParams)
	if strings.Join(countableOrder, ",") != "text,image_url" {
		t.Fatalf("countable part order = %v", countableOrder)
	}

	// The real params keep the actual data URL and the original order.
	encoded, err := json.Marshal(converted.params)
	if err != nil {
		t.Fatal(err)
	}
	var realBody map[string]any
	if err := json.Unmarshal(encoded, &realBody); err != nil {
		t.Fatal(err)
	}
	realURL := countableImageURL(t, realBody)
	if !strings.HasPrefix(realURL, "data:image/png;base64,") {
		t.Fatalf("real image URL = %q, want data URL", realURL)
	}
	if strings.Contains(converted.envelope.Serialized, realURL) {
		t.Fatal("countable projection leaked the real data URL")
	}
	realOrder := countablePartOrder(t, realBody)
	if strings.Join(realOrder, ",") != "text,image_url" {
		t.Fatalf("real part order = %v", realOrder)
	}
}

func TestConvertRequestKeepsV1SerializerForTextAndTools(t *testing.T) {
	llm := &OpenAICompatibleLLM{model: "test-model"}
	request := &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)},
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "lookup"}}}},
		},
	}
	converted, err := llm.convertRequest(request, false)
	if err != nil {
		t.Fatal(err)
	}
	if converted.envelope.SerializerID != port.SerializerOpenAIChatCompletionsV1 {
		t.Fatalf("serializer = %q, want %q", converted.envelope.SerializerID, port.SerializerOpenAIChatCompletionsV1)
	}
	if len(converted.envelope.Media) != 0 {
		t.Fatalf("text-only envelope media = %#v", converted.envelope.Media)
	}
	if strings.Contains(converted.envelope.Serialized, mediaMarker) {
		t.Fatal("text-only countable projection contains the media marker")
	}
	// The v1 projection is exactly the real params plus the stream flag, so
	// text-only counting is byte-identical in shape to the previous behavior.
	var serializedEnvelope struct {
		Params any  `json:"params"`
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal([]byte(converted.envelope.Serialized), &serializedEnvelope); err != nil {
		t.Fatal(err)
	}
	if serializedEnvelope.Stream != false {
		t.Fatalf("countable stream flag = %v", serializedEnvelope.Stream)
	}
	var realParams any
	realEncoded, err := json.Marshal(converted.params)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(realEncoded, &realParams); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(serializedEnvelope.Params, realParams) {
		t.Fatal("text-only countable projection differs from the real params")
	}
}

func TestGuardRejectsMediaOverBudgetBeforeHTTP(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(server.Close)
	llm, err := New(WithAPIKey("test"), WithBaseURL(server.URL), WithModel("model"))
	if err != nil {
		t.Fatal(err)
	}
	counter, err := tokencounter.New("estimator", "visual-tile-conservative-v1")
	if err != nil {
		t.Fatal(err)
	}
	// The visual estimate for a 4x4 image is 2048 tokens plus the countable
	// payload, which exceeds this hard limit.
	if err := llm.ConfigureRequestGuard(counter, domain.RequestBudget{HardTokens: 500}, "test/vision"); err != nil {
		t.Fatal(err)
	}
	request := &model.LLMRequest{Contents: []*genai.Content{{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			genai.NewPartFromText("describe"),
			genai.NewPartFromBytes(realTestPNG(t), "image/png"),
		},
	}}}
	_, _, gotErr := collect(llm.GenerateContent(context.Background(), request, false))
	if !errors.Is(gotErr, domain.ErrIrreducibleContext) {
		t.Fatalf("GenerateContent() error = %v, want irreducible context", gotErr)
	}
	if calls.Load() != 0 {
		t.Fatalf("over-budget media request made %d HTTP calls, want 0", calls.Load())
	}
}

func TestGuardRejectsMediaWithByteBoundBeforeHTTP(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(server.Close)
	llm, err := New(WithAPIKey("test"), WithBaseURL(server.URL), WithModel("model"))
	if err != nil {
		t.Fatal(err)
	}
	counter, err := tokencounter.New("byte_bound", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := llm.ConfigureRequestGuard(counter, domain.RequestBudget{HardTokens: 1_000_000}, "test/vision"); err != nil {
		t.Fatal(err)
	}
	request := &model.LLMRequest{Contents: []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromBytes(realTestPNG(t), "image/png")},
	}}}
	_, _, gotErr := collect(llm.GenerateContent(context.Background(), request, false))
	if gotErr == nil || !strings.Contains(gotErr.Error(), "request_token_count_unavailable") {
		t.Fatalf("GenerateContent() error = %v, want request_token_count_unavailable", gotErr)
	}
	if calls.Load() != 0 {
		t.Fatalf("media request with byte_bound made %d HTTP calls, want 0", calls.Load())
	}
}

func TestGuardRejectsLargeInputAudioBeforeHTTP(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(writer, "unexpected HTTP request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	llm, err := New(WithAPIKey("test"), WithBaseURL(server.URL), WithModel("model"))
	if err != nil {
		t.Fatal(err)
	}
	counter, err := tokencounter.New(tokencounter.StrategyEstimator, tokencounter.EstimatorVisualTileConservativeV1)
	if err != nil {
		t.Fatal(err)
	}
	budget := domain.RequestBudget{HardTokens: 512}
	if err := llm.ConfigureRequestGuard(counter, budget, "test/audio"); err != nil {
		t.Fatal(err)
	}
	request := &model.LLMRequest{Contents: []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromBytes(bytes.Repeat([]byte("audio"), 4096), "audio/wav")},
	}}}
	converted, err := llm.convertRequest(request, false)
	if err != nil {
		t.Fatal(err)
	}
	if converted.envelope.SerializerID != port.SerializerOpenAIChatCompletionsV1 {
		t.Fatalf("audio serializer = %q, want %q", converted.envelope.SerializerID, port.SerializerOpenAIChatCompletionsV1)
	}
	if strings.Contains(converted.envelope.Serialized, mediaMarker) {
		t.Fatal("audio countable projection replaced real bytes with media marker")
	}
	_, _, gotErr := collect(llm.GenerateContent(context.Background(), request, false))
	if !errors.Is(gotErr, domain.ErrIrreducibleContext) {
		t.Fatalf("GenerateContent() error = %v, want irreducible context", gotErr)
	}
	var irreducible *domain.IrreducibleContextError
	if !errors.As(gotErr, &irreducible) {
		t.Fatalf("GenerateContent() error = %v, want IrreducibleContextError", gotErr)
	}
	if irreducible.MinimumTokens <= budget.HardTokens || irreducible.HardTokens != budget.HardTokens {
		t.Fatalf("irreducible budget = %#v, want minimum > %d and hard %d", irreducible, budget.HardTokens, budget.HardTokens)
	}
	if calls.Load() != 0 {
		t.Fatalf("over-budget audio request made %d HTTP calls, want 0", calls.Load())
	}
}

func TestConvertRequestRejectsUndecodableImagePart(t *testing.T) {
	llm := &OpenAICompatibleLLM{model: "test-model"}
	request := &model.LLMRequest{Contents: []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromBytes([]byte("png"), "image/png")},
	}}}
	_, err := llm.convertRequest(request, false)
	if err == nil || !strings.Contains(err.Error(), "not a decodable image") {
		t.Fatalf("convertRequest() error = %v", err)
	}
}

// countableImageURL extracts the first image_url url from a decoded request body.
func countableImageURL(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, rawMessage := range body["messages"].([]any) {
		content, ok := rawMessage.(map[string]any)["content"]
		if !ok {
			continue
		}
		if _, isText := content.(string); isText {
			continue
		}
		for _, rawPart := range content.([]any) {
			part := rawPart.(map[string]any)
			if part["type"] != "image_url" {
				continue
			}
			imageURL, ok := part["image_url"].(map[string]any)["url"].(string)
			if !ok {
				t.Fatalf("image_url part = %#v", part)
			}
			return imageURL
		}
	}
	t.Fatalf("no image_url found in %#v", body)
	return ""
}

// countablePartOrder returns the ordered content part types of the first user
// message in a decoded request body.
func countablePartOrder(t *testing.T, body map[string]any) []string {
	t.Helper()
	messages := body["messages"].([]any)
	for _, rawMessage := range messages {
		content, ok := rawMessage.(map[string]any)["content"]
		if !ok {
			continue
		}
		if _, isText := content.(string); isText {
			return []string{"text"}
		}
		parts := content.([]any)
		order := make([]string, 0, len(parts))
		for _, rawPart := range parts {
			order = append(order, rawPart.(map[string]any)["type"].(string))
		}
		return order
	}
	t.Fatalf("no user content found in %#v", body)
	return nil
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
		_, yields, err := collect(llm.GenerateContent(context.Background(), textRequest(), false))
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
		_, yields, err := collect(llm.GenerateContent(context.Background(), textRequest(), false))
		if !errors.Is(err, ErrNoAssistantText) || yields != 1 {
			t.Fatalf("GenerateContent() = err %v, yields %d", err, yields)
		}
	})
}

func TestGenerateContentTranslatesFunctionToolsAndCalls(t *testing.T) {
	t.Parallel()

	captured := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		captured <- body
		writeJSON(t, writer, http.StatusOK, map[string]any{
			"id": "completion", "object": "chat.completion", "created": 1, "model": "test",
			"choices": []any{map[string]any{
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id": "provider-call", "type": "function",
						"function": map[string]any{"name": "lookup", "arguments": `{"query":"status"}`},
					}},
				},
			}},
		})
	}))
	t.Cleanup(server.Close)

	llm := mustTestLLM(t, server.URL)
	response, yields, err := collect(llm.GenerateContent(context.Background(), &model.LLMRequest{
		Contents: []*genai.Content{
			genai.NewContentFromText("status", genai.RoleUser),
			{Role: genai.RoleModel, Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "call-1", Name: "lookup", Args: map[string]any{"query": "one"}}},
				{FunctionCall: &genai.FunctionCall{ID: "call-2", Name: "lookup", Args: map[string]any{"query": "two"}}},
			}},
			{Role: genai.RoleUser, Parts: []*genai.Part{
				{FunctionResponse: &genai.FunctionResponse{ID: "call-1", Name: "lookup", Response: map[string]any{"result": "one"}}},
				{FunctionResponse: &genai.FunctionResponse{ID: "call-2", Name: "lookup", Response: map[string]any{"result": "two"}}},
			}},
		},
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
				Name: "lookup", ParametersJsonSchema: map[string]any{"type": "object"},
			}}}},
			ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto}},
		},
	}, false))
	if err != nil || yields != 1 {
		t.Fatalf("GenerateContent() = %#v, %v, %d", response, err, yields)
	}
	if response.FinishReason != genai.FinishReasonStop || len(response.Content.Parts) != 1 {
		t.Fatalf("response = %#v", response)
	}
	call := response.Content.Parts[0].FunctionCall
	if call == nil || call.ID != "provider-call" || call.Name != "lookup" || call.Args["query"] != "status" {
		t.Fatalf("function call = %#v", call)
	}

	body := <-captured
	if body["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v", body["parallel_tool_calls"])
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %#v", body["tool_choice"])
	}
	tools := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" {
		t.Fatalf("tools = %#v", tools)
	}
	messages := body["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	assistant := messages[1].(map[string]any)
	if calls := assistant["tool_calls"].([]any); len(calls) != 2 {
		t.Fatalf("assistant tool_calls = %#v", calls)
	}
	if messages[2].(map[string]any)["tool_call_id"] != "call-1" || messages[3].(map[string]any)["tool_call_id"] != "call-2" {
		t.Fatalf("tool responses = %#v", messages[2:])
	}
}

func TestGenerateContentRejectsInvalidFunctionProtocolBeforeHTTP(t *testing.T) {
	t.Parallel()

	llm := mustTestLLM(t, "https://example.com")
	tests := []struct {
		name string
		req  *model.LLMRequest
	}{
		{
			name: "duplicate declaration",
			req:  &model.LLMRequest{Contents: textRequest().Contents, Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "same"}, {Name: "same"}}}}}},
		},
		{
			name: "missing call ID",
			req:  &model.LLMRequest{Contents: []*genai.Content{{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "lookup"}}}}}},
		},
		{
			name: "function response with text",
			req:  &model.LLMRequest{Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "unexpected"}, {FunctionResponse: &genai.FunctionResponse{ID: "call", Name: "lookup"}}}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := llm.requestParams(tt.req)
			if err == nil {
				t.Fatal("requestParams() unexpectedly succeeded")
			}
		})
	}
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
		{name: "non text part", req: &model.LLMRequest{Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{genai.NewPartFromBytes([]byte("binary"), "application/pdf")}}}}, want: ErrUnsupportedPart},
		{name: "thought part", req: &model.LLMRequest{Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "reasoning", Thought: true}}}}}, want: ErrUnsupportedPart},
		{name: "thought signature", req: &model.LLMRequest{Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "text", ThoughtSignature: []byte("signature")}}}}}, want: ErrUnsupportedPart},
		{name: "part metadata", req: &model.LLMRequest{Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "text", PartMetadata: map[string]any{"source": "test"}}}}}}, want: ErrUnsupportedPart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, yields, err := collect(llm.GenerateContent(context.Background(), tt.req, tt.stream))
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

// realTestPNG returns a tiny decodable PNG for conversion tests.
func realTestPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := range 4 {
		for x := range 4 {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 60), G: uint8(y * 60), B: 120, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// realTestJPEG returns a tiny decodable JPEG for conversion tests.
func realTestJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := range 4 {
		for x := range 4 {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 60), G: uint8(y * 60), B: 120, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
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
	configureTestGuard(t, llm)
	return llm
}

type testRequestCounter struct{}

func (testRequestCounter) CountRequest(_ context.Context, envelope port.ModelRequestEnvelope) (port.TokenCount, error) {
	return port.TokenCount{Tokens: len(envelope.Serialized), Strategy: "test"}, nil
}

type fixedRequestCounter int

func (c fixedRequestCounter) CountRequest(context.Context, port.ModelRequestEnvelope) (port.TokenCount, error) {
	return port.TokenCount{Tokens: int(c), Strategy: "fixed", Exact: true}, nil
}

type capturingRequestCounter struct {
	envelope port.ModelRequestEnvelope
}

func (c *capturingRequestCounter) CountRequest(_ context.Context, envelope port.ModelRequestEnvelope) (port.TokenCount, error) {
	c.envelope = envelope
	return port.TokenCount{Tokens: len(envelope.Serialized), Strategy: "captured", Exact: true}, nil
}

func TestCountLLMRequestUsesFinalGuardEnvelope(t *testing.T) {
	counter := &capturingRequestCounter{}
	llm := &OpenAICompatibleLLM{model: "test-model", profileID: "test/profile", requestCounter: counter}
	request := &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)},
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "lookup"}}}},
		},
	}
	want, err := llm.convertRequest(request, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := llm.CountLLMRequest(t.Context(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(counter.envelope, want.envelope) {
		t.Fatalf("counted envelope = %#v, want %#v", counter.envelope, want.envelope)
	}
	if got.Tokens != len(want.envelope.Serialized) || got.Strategy != "captured" || !got.Exact {
		t.Fatalf("count = %#v", got)
	}
}

type frameCountingCompiler struct{}

func (frameCountingCompiler) Compile(_ context.Context, req domain.CompileRequest) (domain.CompileResult, error) {
	return domain.CompileResult{Contents: req.Contents}, nil
}

func (frameCountingCompiler) CompileFrame(ctx context.Context, req domain.CompileRequest, counter port.ContextFrameCounter) (domain.CompileResult, error) {
	if _, err := counter.CountContextFrame(ctx, req.Contents); err != nil {
		return domain.CompileResult{}, err
	}
	return domain.CompileResult{Contents: req.Contents}, nil
}

type envelopeSequenceCounter struct {
	envelopes []port.ModelRequestEnvelope
}

func (c *envelopeSequenceCounter) CountRequest(_ context.Context, envelope port.ModelRequestEnvelope) (port.TokenCount, error) {
	c.envelopes = append(c.envelopes, envelope)
	return port.TokenCount{Tokens: 2, Strategy: "captured", Exact: true}, nil
}

func TestProviderFrameCountMatchesFinalGuardEnvelope(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			counter := &envelopeSequenceCounter{}
			llm := &OpenAICompatibleLLM{
				model:                  "test-model",
				reasoningEffort:        "high",
				extraBody:              map[string]any{"provider_option": true},
				requestCounter:         counter,
				requestBudget:          domain.RequestBudget{HardTokens: 1},
				profileID:              "test/profile",
				defaultMaxOutputTokens: 256,
			}
			request := &model.LLMRequest{
				Model:    "adk-model-field",
				Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)},
				Config: &genai.GenerateContentConfig{
					SystemInstruction: genai.NewContentFromText("system policy", genai.RoleUser),
					Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
						Name:        "lookup",
						Description: "look up one item",
					}}}},
				},
				Tools: map[string]any{"host_metadata": "retained"},
			}
			callback := adkagent.CompilerBeforeModelCallback(adkagent.CompilerCallbackConfig{
				Compiler: frameCountingCompiler{}, RequestModel: llm, Stream: stream,
				Budget: domain.RequestBudget{HardTokens: 100}, Actor: "U12345678",
			})
			if _, err := callback(nil, request); err != nil {
				t.Fatal(err)
			}

			var guardErr error
			for _, err := range llm.GenerateContent(t.Context(), request, stream) {
				guardErr = err
				break
			}
			if _, ok := errors.AsType[*domain.IrreducibleContextError](guardErr); !ok {
				t.Fatalf("GenerateContent() error = %v, want final guard rejection", guardErr)
			}
			if len(counter.envelopes) != 2 {
				t.Fatalf("counted envelopes = %d, want frame and final guard", len(counter.envelopes))
			}
			if !reflect.DeepEqual(counter.envelopes[0], counter.envelopes[1]) {
				t.Fatalf("frame envelope = %#v\nfinal guard envelope = %#v", counter.envelopes[0], counter.envelopes[1])
			}
		})
	}
}

type failingRequestCounter struct{}

func (failingRequestCounter) CountRequest(context.Context, port.ModelRequestEnvelope) (port.TokenCount, error) {
	return port.TokenCount{}, errors.New("counter unavailable")
}

type guardMetricCapture struct {
	samples map[string][]port.MetricSample
}

func (m *guardMetricCapture) add(sample port.MetricSample) {
	if m.samples == nil {
		m.samples = make(map[string][]port.MetricSample)
	}
	m.samples[sample.Name] = append(m.samples[sample.Name], sample)
}
func (m *guardMetricCapture) AddCounter(name string, delta int64, labels port.MetricLabels) {
	m.add(port.MetricSample{Name: name, Kind: port.MetricKindCounter, Value: float64(delta), Labels: labels})
}
func (m *guardMetricCapture) SetGauge(name string, value int64, labels port.MetricLabels) {
	m.add(port.MetricSample{Name: name, Kind: port.MetricKindGauge, Value: float64(value), Labels: labels})
}
func (m *guardMetricCapture) Observe(name string, value float64, labels port.MetricLabels) {
	m.add(port.MetricSample{Name: name, Kind: port.MetricKindObservation, Value: value, Labels: labels})
}
func (m *guardMetricCapture) Snapshot() []port.MetricSample { return nil }

func TestFinalRequestGuardRejectsBeforeProviderCall(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(server.Close)
	llm, err := New(WithAPIKey("test"), WithBaseURL(server.URL), WithModel("model"))
	if err != nil {
		t.Fatal(err)
	}
	if err := llm.ConfigureRequestGuard(fixedRequestCounter(11), domain.RequestBudget{HardTokens: 10}, "test/profile"); err != nil {
		t.Fatal(err)
	}
	_, _, gotErr := collect(llm.GenerateContent(context.Background(), textRequest(), false))
	if !errors.Is(gotErr, domain.ErrIrreducibleContext) {
		t.Fatalf("GenerateContent() error = %v", gotErr)
	}
	if calls.Load() != 0 {
		t.Fatalf("provider calls = %d, want 0", calls.Load())
	}
}

func TestFinalRequestGuardEmitsCompleteMetrics(t *testing.T) {
	metrics := &guardMetricCapture{}
	llm, err := New(WithAPIKey("test"), WithBaseURL("https://model.example/v1"), WithModel("model"))
	if err != nil {
		t.Fatal(err)
	}
	budget := domain.RequestBudget{WindowTokens: 100, HardTokens: 10}
	if err := llm.ConfigureRequestGuard(fixedRequestCounter(5), budget, "test/profile", metrics); err != nil {
		t.Fatal(err)
	}
	if err := llm.guardRequest(t.Context(), testConvertedRequest(5)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		domain.MetricModelRequestContextWindowTokens,
		domain.MetricModelRequestHardLimitTokens,
		domain.MetricModelRequestTokens,
		domain.MetricModelRequestUtilizationBasisPoints,
		domain.MetricModelRequestCounterStrategyTotal,
		domain.MetricModelRequestGuardOutcomeTotal,
	} {
		if len(metrics.samples[name]) == 0 {
			t.Errorf("metric %q was not emitted", name)
		}
	}

	if err := llm.ConfigureRequestGuard(fixedRequestCounter(11), budget, "test/profile", metrics); err != nil {
		t.Fatal(err)
	}
	if err := llm.guardRequest(t.Context(), testConvertedRequest(11)); err != nil && !errors.Is(err, domain.ErrIrreducibleContext) {
		t.Fatalf("rejected guard error = %v", err)
	}
	if len(metrics.samples[domain.MetricModelRequestIrreducibleTotal]) == 0 {
		t.Fatal("irreducible metric was not emitted")
	}

	if err := llm.ConfigureRequestGuard(failingRequestCounter{}, budget, "test/profile", metrics); err != nil {
		t.Fatal(err)
	}
	if err := llm.guardRequest(t.Context(), testConvertedRequest(1)); err == nil {
		t.Fatal("count failure was accepted")
	}
	outcomes := metrics.samples[domain.MetricModelRequestGuardOutcomeTotal]
	if outcomes[len(outcomes)-1].Labels["guard_outcome"] != "count_failed" {
		t.Fatalf("guard outcomes = %#v", outcomes)
	}
}

func configureTestGuard(t *testing.T, llm *OpenAICompatibleLLM) {
	t.Helper()
	if err := llm.ConfigureRequestGuard(testRequestCounter{}, domain.RequestBudget{HardTokens: 1_000_000}, "test/profile"); err != nil {
		t.Fatal(err)
	}
}

// testConvertedRequest builds a minimal countable request for guard tests; the
// Serialized bytes are what fixed counters measure.
func testConvertedRequest(_ int) convertedRequest {
	return convertedRequest{
		params: openai.ChatCompletionNewParams{},
		envelope: port.ModelRequestEnvelope{
			SerializerID: port.SerializerOpenAIChatCompletionsV1,
			ProfileID:    "test/profile",
			Serialized:   "{}",
		},
	}
}

func textRequest() *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}
}

func collect(sequence iter.Seq2[*model.LLMResponse, error]) (*model.LLMResponse, int, error) {
	var response *model.LLMResponse
	var resultErr error
	count := 0
	for current, err := range sequence {
		count++
		response = current
		resultErr = err
	}
	return response, count, resultErr
}

func writeJSON(t *testing.T, writer http.ResponseWriter, status int, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("write test response: %v", err)
	}
}

func writeSSEChunk(writer http.ResponseWriter, value any) {
	data, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(writer, "data: %s\n\n", data)
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
