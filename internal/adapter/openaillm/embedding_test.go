package openaillm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

type fakeLimiter struct {
	acquired bool
}

func (f fakeLimiter) TryAcquire() (func(), bool) {
	if !f.acquired {
		return func() {}, false
	}
	return func() {}, true
}

type embeddingRequestCapture struct {
	path      string
	authorize string
	body      map[string]any
}

func startEmbeddingServer(t *testing.T, respond func(body map[string]any) (int, any)) (server *httptest.Server, captured *atomic.Pointer[embeddingRequestCapture]) {
	t.Helper()
	captured = &atomic.Pointer[embeddingRequestCapture]{}
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		captured.Store(&embeddingRequestCapture{path: request.URL.Path, authorize: request.Header.Get("Authorization"), body: body})
		status, payload := respond(body)
		data, err := json.Marshal(payload)
		if err != nil {
			http.Error(writer, "marshal", http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write(data)
	}))
	t.Cleanup(server.Close)
	return server, captured
}

func okEmbeddingResponse(dimensions int) func(map[string]any) (int, any) {
	return func(body map[string]any) (int, any) {
		inputs, _ := body["input"].([]any)
		data := make([]any, len(inputs))
		for index := range inputs {
			vector := make([]float64, dimensions)
			for value := range vector {
				vector[value] = float64(index+1) + float64(value)*0.5
			}
			data[index] = map[string]any{"object": "embedding", "index": index, "embedding": vector}
		}
		return http.StatusOK, map[string]any{
			"object": "list",
			"data":   data,
			"model":  "provider-model",
			"usage":  map[string]any{"prompt_tokens": 2, "total_tokens": 2},
		}
	}
}

func newTestEmbeddingProvider(t *testing.T, serverURL string, mutate func(*EmbeddingProviderConfig)) *OpenAIEmbeddingProvider {
	t.Helper()
	config := EmbeddingProviderConfig{
		APIKey:     "test-api-key",
		BaseURL:    serverURL,
		Model:      "configured-embedding-model",
		Dimensions: 3,
		Timeout:    5 * time.Second,
		MaxBatch:   32,
		Limiter:    fakeLimiter{acquired: true},
	}
	if mutate != nil {
		mutate(&config)
	}
	provider, err := NewEmbeddingProvider(config)
	if err != nil {
		t.Fatalf("NewEmbeddingProvider() error = %v", err)
	}
	return provider
}

func TestEmbeddingProviderSendsConfiguredRequestAndReturnsPositionalVectors(t *testing.T) {
	server, captured := startEmbeddingServer(t, okEmbeddingResponse(3))
	provider := newTestEmbeddingProvider(t, server.URL, nil)

	vectors, err := provider.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if len(vectors) != 2 {
		t.Fatalf("Embed() returned %d vectors, want 2", len(vectors))
	}
	want := [][]float32{{1, 1.5, 2}, {2, 2.5, 3}}
	for index := range want {
		if len(vectors[index]) != 3 || vectors[index][0] != want[index][0] || vectors[index][1] != want[index][1] || vectors[index][2] != want[index][2] {
			t.Fatalf("vectors[%d] = %v, want %v", index, vectors[index], want[index])
		}
	}
	request := captured.Load()
	if request == nil {
		t.Fatal("no request reached the provider")
	}
	if request.path != "/embeddings" {
		t.Fatalf("request path = %q, want /embeddings", request.path)
	}
	if request.authorize != "Bearer test-api-key" {
		t.Fatalf("Authorization header = %q, want the configured credential", request.authorize)
	}
	if request.body["model"] != "configured-embedding-model" {
		t.Fatalf("request model = %v", request.body["model"])
	}
	if request.body["dimensions"] != float64(3) {
		t.Fatalf("request dimensions = %v, want 3", request.body["dimensions"])
	}
	inputs, _ := request.body["input"].([]any)
	if len(inputs) != 2 || inputs[0] != "first" || inputs[1] != "second" {
		t.Fatalf("request input = %v, want the exact inputs in order", inputs)
	}
}

func TestEmbeddingProviderRedactsSecretsBeforeProviderContact(t *testing.T) {
	secret := "sk-super-secret-value"
	server, captured := startEmbeddingServer(t, okEmbeddingResponse(2))
	redactor := secure.NewRedactor(secret)
	provider := newTestEmbeddingProvider(t, server.URL, func(config *EmbeddingProviderConfig) {
		config.Redact = redactor.String
		config.Dimensions = 2
	})

	if _, err := provider.Embed(context.Background(), []string{"context mentions " + secret + " and more"}); err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	request := captured.Load()
	if request == nil {
		t.Fatal("no request reached the provider")
	}
	encoded, _ := json.Marshal(request.body)
	wire := string(encoded)
	if strings.Contains(wire, secret) {
		t.Fatal("the raw secret reached the provider request body")
	}
	if !strings.Contains(wire, "context mentions ") {
		t.Fatal("redaction destroyed non-secret input text")
	}
}

func TestEmbeddingProviderEnforcesLimiterBatchAndEmptyInput(t *testing.T) {
	server, captured := startEmbeddingServer(t, okEmbeddingResponse(2))
	provider := newTestEmbeddingProvider(t, server.URL, func(config *EmbeddingProviderConfig) {
		config.Limiter = fakeLimiter{acquired: false}
		config.Dimensions = 2
	})
	if _, err := provider.Embed(context.Background(), []string{"a"}); !errors.Is(err, port.ErrModelCallLimitReached) {
		t.Fatalf("Embed() error = %v, want ErrModelCallLimitReached", err)
	}
	if captured.Load() != nil {
		t.Fatal("a limited provider still reached the network")
	}

	bounded := newTestEmbeddingProvider(t, server.URL, func(config *EmbeddingProviderConfig) {
		config.MaxBatch = 2
		config.Dimensions = 2
	})
	if _, err := bounded.Embed(context.Background(), []string{"a", "b", "c"}); err == nil {
		t.Fatal("Embed() accepted a batch above the configured bound")
	}
	if captured.Load() != nil {
		t.Fatal("an over-bounded batch still reached the network")
	}
	if _, err := bounded.Embed(context.Background(), nil); err == nil {
		t.Fatal("Embed() accepted an empty batch")
	}
}

func TestEmbeddingProviderReordersOutOfOrderResponseByIndex(t *testing.T) {
	server, captured := startEmbeddingServer(t, func(body map[string]any) (int, any) {
		return http.StatusOK, map[string]any{
			"object": "list",
			"data": []any{
				// The provider returns the second input's vector first and
				// the first input's vector second. The SDK-exposed Index
				// field is the only trustworthy positional binding.
				map[string]any{"object": "embedding", "index": 1, "embedding": []float64{2, 2.5}},
				map[string]any{"object": "embedding", "index": 0, "embedding": []float64{1, 1.5}},
			},
			"model": "provider-model",
			"usage": map[string]any{"prompt_tokens": 2, "total_tokens": 2},
		}
	})
	provider := newTestEmbeddingProvider(t, server.URL, func(config *EmbeddingProviderConfig) {
		config.Dimensions = 2
	})
	vectors, err := provider.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if len(vectors) != 2 {
		t.Fatalf("Embed() returned %d vectors, want 2", len(vectors))
	}
	if vectors[0][0] != 1 || vectors[0][1] != 1.5 {
		t.Fatalf("vectors[0] = %v, want the first input's vector %v", vectors[0], []float32{1, 1.5})
	}
	if vectors[1][0] != 2 || vectors[1][1] != 2.5 {
		t.Fatalf("vectors[1] = %v, want the second input's vector %v", vectors[1], []float32{2, 2.5})
	}
	if captured.Load() == nil {
		t.Fatal("no request reached the provider")
	}
}

func TestEmbeddingProviderRejectsMalformedIndices(t *testing.T) {
	cases := []struct {
		name string
		data []any
		want string
	}{
		{
			name: "duplicate index",
			data: []any{
				map[string]any{"object": "embedding", "index": 0, "embedding": []float64{1}},
				map[string]any{"object": "embedding", "index": 0, "embedding": []float64{2}},
			},
			want: "duplicate index",
		},
		{
			name: "out of range index",
			data: []any{
				map[string]any{"object": "embedding", "index": 2, "embedding": []float64{1}},
				map[string]any{"object": "embedding", "index": 0, "embedding": []float64{2}},
			},
			want: "out-of-range index",
		},
		{
			name: "negative index",
			data: []any{
				map[string]any{"object": "embedding", "index": -1, "embedding": []float64{1}},
				map[string]any{"object": "embedding", "index": 0, "embedding": []float64{2}},
			},
			want: "out-of-range index",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, captured := startEmbeddingServer(t, func(body map[string]any) (int, any) {
				return http.StatusOK, map[string]any{
					"object": "list",
					"data":   tc.data,
					"model":  "provider-model",
					"usage":  map[string]any{"prompt_tokens": 2, "total_tokens": 2},
				}
			})
			provider := newTestEmbeddingProvider(t, server.URL, func(config *EmbeddingProviderConfig) {
				config.Dimensions = 1
			})
			_, err := provider.Embed(context.Background(), []string{"a", "b"})
			if err == nil {
				t.Fatal("Embed() accepted a response with malformed indices")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Embed() error = %v, want it to mention %q", err, tc.want)
			}
			if captured.Load() == nil {
				t.Fatal("no request reached the provider")
			}
		})
	}
}

func TestEmbeddingProviderRejectsMismatchedResponseCount(t *testing.T) {
	server, captured := startEmbeddingServer(t, func(body map[string]any) (int, any) {
		return http.StatusOK, map[string]any{
			"object": "list",
			"data": []any{
				map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2}},
			},
			"model": "provider-model",
			"usage": map[string]any{"prompt_tokens": 1, "total_tokens": 1},
		}
	})
	provider := newTestEmbeddingProvider(t, server.URL, func(config *EmbeddingProviderConfig) {
		config.Dimensions = 2
	})
	if _, err := provider.Embed(context.Background(), []string{"one", "two"}); err == nil {
		t.Fatal("Embed() accepted a response with the wrong vector count")
	}
	if captured.Load() == nil {
		t.Fatal("no request reached the provider")
	}
}

func TestEmbeddingProviderBoundsRequestTimeout(t *testing.T) {
	server, captured := startEmbeddingServer(t, func(body map[string]any) (int, any) {
		time.Sleep(300 * time.Millisecond)
		return http.StatusOK, map[string]any{
			"object": "list",
			"data":   []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1}}},
			"model":  "provider-model",
			"usage":  map[string]any{"prompt_tokens": 1, "total_tokens": 1},
		}
	})
	provider := newTestEmbeddingProvider(t, server.URL, func(config *EmbeddingProviderConfig) {
		config.Dimensions = 1
		config.Timeout = 50 * time.Millisecond
	})
	start := time.Now()
	if _, err := provider.Embed(context.Background(), []string{"one"}); err == nil {
		t.Fatal("Embed() succeeded despite the provider exceeding the configured timeout")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Embed() took %v, want the bounded timeout to win", elapsed)
	}
	if captured.Load() == nil {
		t.Fatal("no request reached the provider")
	}
}

func TestNewEmbeddingProviderRejectsInvalidConfig(t *testing.T) {
	base := func(mutate func(*EmbeddingProviderConfig)) {
		config := EmbeddingProviderConfig{
			APIKey: "k", BaseURL: "http://localhost:9", Model: "m", Dimensions: 2,
			Timeout: time.Second, MaxBatch: 4, Limiter: fakeLimiter{acquired: true},
		}
		mutate(&config)
		if _, err := NewEmbeddingProvider(config); err == nil {
			t.Fatal("NewEmbeddingProvider() accepted an invalid configuration")
		}
	}
	base(func(c *EmbeddingProviderConfig) { c.APIKey = "" })
	base(func(c *EmbeddingProviderConfig) { c.BaseURL = "" })
	base(func(c *EmbeddingProviderConfig) { c.Model = "" })
	base(func(c *EmbeddingProviderConfig) { c.Dimensions = 0 })
	base(func(c *EmbeddingProviderConfig) { c.Dimensions = 4097 })
	base(func(c *EmbeddingProviderConfig) { c.Timeout = 0 })
	base(func(c *EmbeddingProviderConfig) { c.MaxBatch = 0 })
	base(func(c *EmbeddingProviderConfig) { c.MaxBatch = 129 })
	base(func(c *EmbeddingProviderConfig) { c.Limiter = nil })
}
