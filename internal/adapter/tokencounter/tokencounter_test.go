package tokencounter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestByteBoundCountRequest(t *testing.T) {
	counter := &byteBoundCounter{}
	envelope := port.ModelRequestEnvelope{
		SerializerID: "test-serializer",
		ProfileID:    "test-profile",
		Serialized:   "hello world",
	}

	tc, err := counter.CountRequest(context.Background(), envelope)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc.Tokens != len(envelope.Serialized) {
		t.Errorf("expected %d tokens, got %d", len(envelope.Serialized), tc.Tokens)
	}
	if tc.Strategy != "byte_bound" {
		t.Errorf("expected strategy %q, got %q", "byte_bound", tc.Strategy)
	}
	if tc.Exact != false {
		t.Errorf("expected Exact=false, got true")
	}
}

func TestByteBoundCountRequestEmpty(t *testing.T) {
	counter := &byteBoundCounter{}
	envelope := port.ModelRequestEnvelope{
		SerializerID: "test-serializer",
		ProfileID:    "test-profile",
		Serialized:   "",
	}

	tc, err := counter.CountRequest(context.Background(), envelope)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc.Tokens != 0 {
		t.Errorf("expected 0 tokens, got %d", tc.Tokens)
	}
}

func TestByteBoundCountRequestSingleByte(t *testing.T) {
	counter := &byteBoundCounter{}
	envelope := port.ModelRequestEnvelope{
		SerializerID: "test-serializer",
		ProfileID:    "test-profile",
		Serialized:   "A",
	}

	tc, err := counter.CountRequest(context.Background(), envelope)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc.Tokens != 1 {
		t.Errorf("expected 1 token, got %d", tc.Tokens)
	}
}

func TestByteBoundCountRequestUTF8(t *testing.T) {
	counter := &byteBoundCounter{}
	envelope := port.ModelRequestEnvelope{
		SerializerID: "test-serializer",
		ProfileID:    "test-profile",
		Serialized:   "🔥test",
	}

	tc, err := counter.CountRequest(context.Background(), envelope)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedBytes := len("🔥test") // byte length, not rune count
	if tc.Tokens != expectedBytes {
		t.Errorf("expected %d tokens (byte length), got %d", expectedBytes, tc.Tokens)
	}
	if tc.Tokens == 5 {
		t.Errorf("token count matches rune count (%d); should be byte length (%d)", 5, expectedBytes)
	}
}

func TestByteBoundCountRequestLongString(t *testing.T) {
	counter := &byteBoundCounter{}
	// 10KB+ string
	longStr := strings.Repeat("x", 11_000)
	envelope := port.ModelRequestEnvelope{
		SerializerID: "test-serializer",
		ProfileID:    "test-profile",
		Serialized:   longStr,
	}

	tc, err := counter.CountRequest(context.Background(), envelope)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc.Tokens != len(longStr) {
		t.Errorf("expected %d tokens, got %d", len(longStr), tc.Tokens)
	}
}

func TestByteBoundRejectsMediaWithoutFallback(t *testing.T) {
	counter := &byteBoundCounter{}
	envelope := port.ModelRequestEnvelope{
		SerializerID: port.SerializerOpenAIChatCompletionsMultimodalV2,
		ProfileID:    "test-profile",
		Serialized:   `{"params":{}}`,
		Media: []port.ModelRequestMedia{
			{MIMEType: "image/png", Width: 100, Height: 100},
		},
	}
	_, err := counter.CountRequest(context.Background(), envelope)
	if !errors.Is(err, ErrMediaNotCountable) {
		t.Fatalf("byte_bound media error = %v, want ErrMediaNotCountable", err)
	}
}

func TestNewUnsupportedStrategy(t *testing.T) {
	counter, err := New("nonexistent_strategy", "")
	if !errors.Is(err, ErrUnsupportedStrategy) {
		t.Fatalf("expected ErrUnsupportedStrategy, got %v", err)
	}
	if counter != nil {
		t.Errorf("expected nil counter, got %v", counter)
	}
}

func TestNewByteBound(t *testing.T) {
	counter, err := New("byte_bound", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counter == nil {
		t.Fatal("expected non-nil counter")
	}
}

func TestNewEstimatorUnknownIDFailsWithoutFallback(t *testing.T) {
	counter, err := New("estimator", "not-installed")
	if !errors.Is(err, ErrUnsupportedCounterID) {
		t.Fatalf("expected ErrUnsupportedCounterID, got %v", err)
	}
	if counter != nil {
		t.Fatal("expected nil counter for unknown estimator id")
	}
}

func TestNewEstimatorVisualTileConservativeV1(t *testing.T) {
	counter, err := New("estimator", "visual-tile-conservative-v1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counter == nil {
		t.Fatal("expected non-nil estimator counter")
	}
}

func TestVisualTileSameDimensionsDifferentCompressionSameCost(t *testing.T) {
	counter, err := New("estimator", "visual-tile-conservative-v1")
	if err != nil {
		t.Fatal(err)
	}
	envelope := func(serialized string) port.ModelRequestEnvelope {
		return port.ModelRequestEnvelope{
			SerializerID: port.SerializerOpenAIChatCompletionsMultimodalV2,
			ProfileID:    "test-profile",
			Serialized:   serialized,
			Media: []port.ModelRequestMedia{
				{MIMEType: "image/jpeg", Width: 1600, Height: 1200},
			},
		}
	}
	// The Serialized countable payload is identical for equal dimensions, so
	// the visual cost must be equal regardless of the underlying compression.
	first, err := counter.CountRequest(context.Background(), envelope("payload"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := counter.CountRequest(context.Background(), envelope("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Tokens != second.Tokens {
		t.Fatalf("same dimensions produced different costs: %d vs %d", first.Tokens, second.Tokens)
	}
	// 1600x1200: 4*3 tiles -> 1024 + 1024*12 = 13312 visual tokens on top of the
	// byte-bound payload.
	expected := len("payload") + 1024 + 1024*12
	if first.Tokens != expected {
		t.Fatalf("tokens = %d, want %d", first.Tokens, expected)
	}
	if first.Strategy != "estimator" || first.Exact {
		t.Fatalf("estimator metadata = %#v", first)
	}
}

func TestVisualTileDetailLevels(t *testing.T) {
	counter, err := New("estimator", "visual-tile-conservative-v1")
	if err != nil {
		t.Fatal(err)
	}
	base := func(detail string) port.ModelRequestEnvelope {
		return port.ModelRequestEnvelope{
			SerializerID: port.SerializerOpenAIChatCompletionsMultimodalV2,
			ProfileID:    "test-profile",
			Serialized:   "x",
			Media: []port.ModelRequestMedia{
				{MIMEType: "image/png", Width: 1024, Height: 1024, Detail: detail},
			},
		}
	}
	low, err := counter.CountRequest(context.Background(), base("low"))
	if err != nil {
		t.Fatal(err)
	}
	if low.Tokens != 1+1024 {
		t.Fatalf("low tokens = %d, want %d", low.Tokens, 1+1024)
	}
	for _, detail := range []string{"", "auto", "high"} {
		counted, err := counter.CountRequest(context.Background(), base(detail))
		if err != nil {
			t.Fatal(err)
		}
		// 1024x1024 -> 2*2 tiles -> 1024 + 1024*4 = 5120.
		if counted.Tokens != 1+1024+1024*4 {
			t.Fatalf("detail %q tokens = %d, want %d", detail, counted.Tokens, 1+1024+1024*4)
		}
	}
}

func TestVisualTileRejectsUnknownDetailAndBadMetadata(t *testing.T) {
	counter, err := New("estimator", "visual-tile-conservative-v1")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		media []port.ModelRequestMedia
	}{
		{name: "unknown detail", media: []port.ModelRequestMedia{{MIMEType: "image/png", Width: 10, Height: 10, Detail: "ultra"}}},
		{name: "zero width", media: []port.ModelRequestMedia{{MIMEType: "image/png", Width: 0, Height: 10}}},
		{name: "negative height", media: []port.ModelRequestMedia{{MIMEType: "image/png", Width: 10, Height: -1}}},
		{name: "unsupported MIME", media: []port.ModelRequestMedia{{MIMEType: "application/pdf", Width: 10, Height: 10}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := counter.CountRequest(context.Background(), port.ModelRequestEnvelope{
				SerializerID: port.SerializerOpenAIChatCompletionsMultimodalV2,
				ProfileID:    "test-profile",
				Serialized:   "x",
				Media:        tt.media,
			})
			if err == nil {
				t.Fatal("expected rejection, got nil")
			}
		})
	}
}

func TestVisualTileRejectsUnknownSerializer(t *testing.T) {
	counter, err := New("estimator", "visual-tile-conservative-v1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = counter.CountRequest(context.Background(), port.ModelRequestEnvelope{
		SerializerID: "some-other-serializer",
		ProfileID:    "test-profile",
		Serialized:   "x",
		Media: []port.ModelRequestMedia{
			{MIMEType: "image/png", Width: 10, Height: 10},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "serializer") {
		t.Fatalf("unknown serializer error = %v", err)
	}
}

func TestVisualTileCountsTextOnlyV1LikeByteBound(t *testing.T) {
	counter, err := New("estimator", "visual-tile-conservative-v1")
	if err != nil {
		t.Fatal(err)
	}
	counted, err := counter.CountRequest(context.Background(), port.ModelRequestEnvelope{
		SerializerID: port.SerializerOpenAIChatCompletionsV1,
		ProfileID:    "test-profile",
		Serialized:   "hello world",
	})
	if err != nil {
		t.Fatal(err)
	}
	if counted.Tokens != len("hello world") {
		t.Fatalf("text-only estimate = %d, want byte-bound", counted.Tokens)
	}
	// Media smuggled through a v1 envelope must fail, never be ignored.
	_, err = counter.CountRequest(context.Background(), port.ModelRequestEnvelope{
		SerializerID: port.SerializerOpenAIChatCompletionsV1,
		ProfileID:    "test-profile",
		Serialized:   "x",
		Media: []port.ModelRequestMedia{
			{MIMEType: "image/png", Width: 10, Height: 10},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "media") {
		t.Fatalf("v1 media error = %v", err)
	}
}

func TestVisualTileMultiImageSum(t *testing.T) {
	counter, err := New("estimator", "visual-tile-conservative-v1")
	if err != nil {
		t.Fatal(err)
	}
	envelope := port.ModelRequestEnvelope{
		SerializerID: port.SerializerOpenAIChatCompletionsMultimodalV2,
		ProfileID:    "test-profile",
		Serialized:   "abc",
		Media: []port.ModelRequestMedia{
			{MIMEType: "image/png", Width: 512, Height: 512},   // 1024 + 1024*1*1 = 2048
			{MIMEType: "image/jpeg", Width: 512, Height: 1024}, // 1024 + 1024*1*2 = 3072
			{MIMEType: "image/webp", Width: 100, Height: 100},  // 1024 + 1024*1*1 = 2048
		},
	}
	counted, err := counter.CountRequest(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	expected := len("abc") + 2048 + 3072 + 2048
	if counted.Tokens != expected {
		t.Fatalf("tokens = %d, want %d", counted.Tokens, expected)
	}
}

func TestCountRequestCancelledContext(t *testing.T) {
	for _, counter := range []port.RequestTokenCounter{&byteBoundCounter{}, &visualTileCounter{}} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately

		envelope := port.ModelRequestEnvelope{
			SerializerID: port.SerializerOpenAIChatCompletionsMultimodalV2,
			ProfileID:    "test-profile",
			Serialized:   "test",
		}

		_, err := counter.CountRequest(ctx, envelope)
		if err == nil {
			t.Fatal("expected error for cancelled context, got nil")
		}
		if !strings.Contains(err.Error(), "context") {
			t.Errorf("expected context-related error, got %q", err.Error())
		}
	}
}
