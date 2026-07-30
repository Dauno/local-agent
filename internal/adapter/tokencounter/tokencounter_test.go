package tokencounter

import (
	"context"
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
	// 🔥 is 4 bytes in UTF-8. "test" is 4 bytes. Total = 8 bytes.
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

func TestNewUnsupportedStrategy(t *testing.T) {
	counter, err := New("nonexistent_strategy")
	if err == nil {
		t.Fatal("expected error for unsupported strategy, got nil")
	}
	if err != ErrUnsupportedStrategy {
		t.Errorf("expected ErrUnsupportedStrategy, got %v", err)
	}
	if counter != nil {
		t.Errorf("expected nil counter, got %v", counter)
	}
}

func TestNewByteBound(t *testing.T) {
	counter, err := New("byte_bound")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counter == nil {
		t.Fatal("expected non-nil counter")
	}
}

func TestCountRequestCancelledContext(t *testing.T) {
	counter := &byteBoundCounter{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	envelope := port.ModelRequestEnvelope{
		SerializerID: "test-serializer",
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
