package integration_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"

	"github.com/Dauno/slack-local-agent/internal/adapter/adkartifact"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	"github.com/Dauno/slack-local-agent/internal/adapter/tokencounter"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// attachmentRegressionFixture is the sanitized ~600 KiB screenshot-like JPEG
// that reproduces the reported incident: under the legacy base64 + byte_bound
// pipeline its serialized request exceeds 65,536 tokens even though the visual
// cost is small.
const attachmentRegressionFixture = "testdata/attachment-screenshot-600k.jpg"

func loadAttachmentFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(attachmentRegressionFixture)
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("fixture does not decode: %v", err)
	}
	if format != "jpeg" || config.Width != 1600 || config.Height != 1200 {
		t.Fatalf("fixture = %s %dx%d", format, config.Width, config.Height)
	}
	if len(data) < 550*1024 || len(data) > 700*1024 {
		t.Fatalf("fixture is %d bytes, want 550-700 KiB", len(data))
	}
	return data
}

// recordingCounter captures every countable envelope before delegating to the
// real estimator, so tests can prove the guard never counts base64.
type recordingCounter struct {
	port.RequestTokenCounter
	mu        sync.Mutex
	envelopes []port.ModelRequestEnvelope
}

func (c *recordingCounter) CountRequest(ctx context.Context, envelope port.ModelRequestEnvelope) (port.TokenCount, error) {
	c.mu.Lock()
	c.envelopes = append(c.envelopes, envelope)
	c.mu.Unlock()
	return c.RequestTokenCounter.CountRequest(ctx, envelope)
}

func (c *recordingCounter) snapshot() []port.ModelRequestEnvelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]port.ModelRequestEnvelope(nil), c.envelopes...)
}

// visualFixtureServer simulates the visual provider: the first call requests
// load_artifacts for the artifact named in the instruction, the second returns
// a textual description.
type visualFixtureServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests [][]byte
	call     atomic.Int64
}

func newVisualFixtureServer(t *testing.T) *visualFixtureServer {
	t.Helper()
	server := &visualFixtureServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/chat/completions") {
			http.Error(writer, "not found", http.StatusNotFound)
			return
		}
		server.mu.Lock()
		body := mustReadAll(request)
		server.requests = append(server.requests, body)
		server.mu.Unlock()
		call := server.call.Add(1)
		if call == 1 {
			writeJSONResponse(writer, map[string]any{
				"id": "completion-tool", "object": "chat.completion", "created": 1, "model": "vision",
				"choices": []any{map[string]any{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []any{map[string]any{
							"id": "call-load", "type": "function",
							"function": map[string]any{"name": "load_artifacts", "arguments": fmt.Sprintf(`{"artifact_names":["%s"]}`, artifactNameFromBody(body))},
						}},
					},
				}},
			})
			return
		}
		writeJSONResponse(writer, map[string]any{
			"id": "completion-visual", "object": "chat.completion", "created": 1, "model": "vision",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "A dark terminal screenshot with grid panels."},
			}},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func mustReadAll(request *http.Request) []byte {
	var buffer bytes.Buffer
	_, _ = buffer.ReadFrom(request.Body)
	request.Body.Close()
	return buffer.Bytes()
}

func artifactNameFromBody(body []byte) string {
	var decoded struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "image"
	}
	for _, message := range decoded.Messages {
		if text, ok := message.Content.(string); ok {
			if start := strings.Index(text, `named "`); start >= 0 {
				name := text[start+len(`named "`):]
				if end := strings.Index(name, `"`); end > 0 {
					return name[:end]
				}
			}
		}
	}
	return "image"
}

func writeJSONResponse(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

func buildVisualModel(t *testing.T, serverURL string, budget domain.RequestBudget, counter port.RequestTokenCounter) (*openaillm.OpenAICompatibleLLM, *recordingCounter) {
	t.Helper()
	llm, err := openaillm.New(
		openaillm.WithAPIKey("test-key"),
		openaillm.WithBaseURL(serverURL),
		openaillm.WithModel("vision-model"),
	)
	if err != nil {
		t.Fatal(err)
	}
	recording := &recordingCounter{RequestTokenCounter: counter}
	if err := llm.ConfigureRequestGuard(recording, budget, "test/vision"); err != nil {
		t.Fatal(err)
	}
	return llm, recording
}

// TestAttachmentImageRegressionAdmitsRealisticScreenshotUnder65536 reproduces
// the incident end-to-end: normalize -> Artifact -> load_artifacts -> admitted
// visual request -> textual description, with the countable payload carrying no
// binary data.
func TestAttachmentImageRegressionAdmitsRealisticScreenshotUnder65536(t *testing.T) {
	fixture := loadAttachmentFixture(t)

	// The legacy pipeline would serialize the base64 data URL and count every
	// byte as a token; demonstrate it exceeds the observed hard limit.
	legacyBase64 := base64.StdEncoding.EncodeToString(fixture)
	if len(legacyBase64) <= 65_536 {
		t.Fatalf("fixture legacy base64 serialization is %d bytes, must exceed 65536 to reproduce the incident", len(legacyBase64))
	}

	server := newVisualFixtureServer(t)
	counter, err := tokencounter.New("estimator", "visual-tile-conservative-v1")
	if err != nil {
		t.Fatal(err)
	}
	budget, err := domain.NewRequestBudget(128_000, domain.RequestBudgetPolicy{MaxRequestPercent: 51})
	if err != nil {
		t.Fatal(err)
	}
	if budget.HardTokens != 65_280 {
		t.Fatalf("hard budget = %d, want 65280 (close to the observed 65536 ceiling)", budget.HardTokens)
	}
	llm, recording := buildVisualModel(t, server.URL, budget, counter)
	processor := adkartifact.NewProcessor(artifact.InMemoryService(), llm, "", 120*time.Second, modelcalllimiter.New(1))

	processed, err := processor.Process(t.Context(), port.AttachmentRequest{
		ProcessingID: "integration-image:1",
		Attachment: port.LoadedAttachment{
			ID: "FIMAGE", Name: "attachment-screenshot-600k.jpg", MIMEType: "image/jpeg", Data: fixture,
		},
	})
	if err != nil {
		t.Fatalf("admitted visual request failed: %v", err)
	}
	if processed.MIMEType != "image-description" || strings.TrimSpace(processed.Text) != "A dark terminal screenshot with grid panels." {
		t.Fatalf("processed = %#v", processed)
	}
	if strings.Contains(processed.Text, "base64") || strings.Contains(processed.Text, "iVBOR") || strings.Contains(processed.Text, "/9j/") {
		t.Fatal("visual description leaked binary data")
	}

	// Exactly two HTTP requests: one text tool call and one admitted visual call.
	if got := server.call.Load(); got != 2 {
		t.Fatalf("HTTP calls = %d, want 2", got)
	}

	// The second HTTP request carries the real data URL of the normalized
	// derivative (canonical JPEG, edge <= 1568).
	server.mu.Lock()
	visualBody := server.requests[1]
	server.mu.Unlock()
	var visualRequest struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(visualBody, &visualRequest); err != nil {
		t.Fatal(err)
	}
	var dataURL string
	for _, message := range visualRequest.Messages {
		parts, ok := message.Content.([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}
			if imageURL, ok := part["image_url"].(map[string]any)["url"].(string); ok {
				dataURL = imageURL
			}
		}
	}
	if !strings.HasPrefix(dataURL, "data:image/jpeg;base64,") {
		t.Fatalf("visual data URL = %q, want canonical jpeg", dataURL)
	}
	derived, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(dataURL, "data:image/jpeg;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	if len(derived) > 2<<20 {
		t.Fatalf("derivative is %d bytes, want <= 2 MiB", len(derived))
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(derived))
	if err != nil || format != "jpeg" {
		t.Fatalf("derivative decode = %v %s", err, format)
	}
	if config.Width > 1568 || config.Height > 1568 {
		t.Fatalf("derivative dims = %dx%d, want edge <= 1568", config.Width, config.Height)
	}

	// The countable envelope for the visual request has no base64, carries the
	// fixed marker, and the total estimate stays under the hard limit.
	envelopes := recording.snapshot()
	if len(envelopes) != 2 {
		t.Fatalf("counted envelopes = %d, want 2", len(envelopes))
	}
	if envelopes[0].SerializerID != port.SerializerOpenAIChatCompletionsV1 || len(envelopes[0].Media) != 0 {
		t.Fatalf("text envelope = %#v", envelopes[0])
	}
	visualEnvelope := envelopes[1]
	if visualEnvelope.SerializerID != port.SerializerOpenAIChatCompletionsMultimodalV2 {
		t.Fatalf("visual serializer = %q", visualEnvelope.SerializerID)
	}
	if len(visualEnvelope.Media) != 1 {
		t.Fatalf("visual media = %#v", visualEnvelope.Media)
	}
	if visualEnvelope.Media[0].MIMEType != "image/jpeg" || visualEnvelope.Media[0].Width != config.Width || visualEnvelope.Media[0].Height != config.Height {
		t.Fatalf("visual media metadata = %#v", visualEnvelope.Media[0])
	}
	if strings.Contains(visualEnvelope.Serialized, "base64") || strings.Contains(visualEnvelope.Serialized, dataURL) {
		t.Fatal("countable envelope leaked the data URL or base64")
	}
	if !strings.Contains(visualEnvelope.Serialized, "local-agent://media/omitted") {
		t.Fatal("countable envelope is missing the media marker")
	}
	visualCount, err := counter.CountRequest(t.Context(), visualEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if visualCount.Tokens > budget.HardTokens {
		t.Fatalf("visual estimate %d exceeds hard limit %d", visualCount.Tokens, budget.HardTokens)
	}
}

// TestAttachmentImageRegressionRejectsOverBudgetBeforeHTTP proves the guard
// fails before any visual HTTP request when the multimodal total exceeds the
// hard limit.
func TestAttachmentImageRegressionRejectsOverBudgetBeforeHTTP(t *testing.T) {
	fixture := loadAttachmentFixture(t)
	server := newVisualFixtureServer(t)
	counter, err := tokencounter.New("estimator", "visual-tile-conservative-v1")
	if err != nil {
		t.Fatal(err)
	}
	llm, recording := buildVisualModel(t, server.URL, domain.RequestBudget{WindowTokens: 128_000, HardTokens: 4096}, counter)
	processor := adkartifact.NewProcessor(artifact.InMemoryService(), llm, "", 120*time.Second, modelcalllimiter.New(1))
	_, err = processor.Process(t.Context(), port.AttachmentRequest{
		ProcessingID: "integration-image:2",
		Attachment: port.LoadedAttachment{
			ID: "FIMAGE2", Name: "screenshot.jpg", MIMEType: "image/jpeg", Data: fixture,
		},
	})
	if err == nil {
		t.Fatal("over-budget image was admitted")
	}
	// The first (text-only) tool call is admitted; the visual request must
	// never reach HTTP.
	if got := server.call.Load(); got != 1 {
		t.Fatalf("HTTP calls = %d, want exactly 1 (text tool call only)", got)
	}
	envelopes := recording.snapshot()
	if len(envelopes) != 2 {
		t.Fatalf("counted envelopes = %d, want 2 (text admitted, visual rejected)", len(envelopes))
	}
	if envelopes[1].SerializerID != port.SerializerOpenAIChatCompletionsMultimodalV2 {
		t.Fatalf("visual envelope serializer = %q", envelopes[1].SerializerID)
	}
	if !errors.Is(err, domain.ErrIrreducibleContext) && !strings.Contains(err.Error(), "request_context_irreducible") && !strings.Contains(err.Error(), "request_token_count_unavailable") {
		t.Fatalf("error = %v, want irreducible or count failure", err)
	}
}

// TestAttachmentImageNormalizedDerivativeRoundTrip verifies the stored
// Artifact only ever contains the normalized derivative.
func TestAttachmentImageNormalizedDerivativeRoundTrip(t *testing.T) {
	fixture := loadAttachmentFixture(t)
	server := newVisualFixtureServer(t)
	counter, err := tokencounter.New("estimator", "visual-tile-conservative-v1")
	if err != nil {
		t.Fatal(err)
	}
	budget, err := domain.NewRequestBudget(128_000, domain.RequestBudgetPolicy{MaxRequestPercent: 51})
	if err != nil {
		t.Fatal(err)
	}
	llm, _ := buildVisualModel(t, server.URL, budget, counter)
	service := artifact.InMemoryService()
	processor := adkartifact.NewProcessor(service, llm, "", 120*time.Second, modelcalllimiter.New(1))
	if _, err := processor.Process(t.Context(), port.AttachmentRequest{
		ProcessingID: "integration-image:3",
		Attachment: port.LoadedAttachment{
			ID: "FIMAGE3", Name: "screenshot.jpg", MIMEType: "image/jpeg", Data: fixture,
		},
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := service.Load(t.Context(), &artifact.LoadRequest{
		AppName: "local-agent-attachment-analyzer", UserID: "local_user",
		SessionID: "attachment:integration-image:3", FileName: "screenshot.jpg",
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.Part == nil || loaded.Part.InlineData == nil {
		t.Fatalf("artifact = %#v", loaded)
	}
	if loaded.Part.InlineData.MIMEType != "image/jpeg" {
		t.Fatalf("artifact MIME = %q", loaded.Part.InlineData.MIMEType)
	}
	if bytes.Equal(loaded.Part.InlineData.Data, fixture) {
		t.Fatal("artifact stored the original bytes, not the derivative")
	}
	if len(loaded.Part.InlineData.Data) > 2<<20 {
		t.Fatalf("artifact bytes = %d, want <= 2 MiB", len(loaded.Part.InlineData.Data))
	}
}
