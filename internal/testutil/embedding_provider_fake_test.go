package testutil

import (
	"context"
	"errors"
	"testing"
)

func TestFakeEmbeddingProviderRecordsInputsExactly(t *testing.T) {
	provider := NewFakeEmbeddingProvider(4)
	if _, err := provider.Embed(context.Background(), []string{"raw secret one", "raw secret two"}); err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	recorded := provider.RecordedInputs()
	if len(recorded) != 2 || recorded[0] != "raw secret one" || recorded[1] != "raw secret two" {
		t.Fatalf("RecordedInputs() = %q, want the exact inputs in call order", recorded)
	}
	if provider.CallCount() != 2 {
		t.Fatalf("CallCount() = %d, want 2 recorded inputs", provider.CallCount())
	}
}

func TestFakeEmbeddingProviderDerivesDeterministicVectors(t *testing.T) {
	provider := NewFakeEmbeddingProvider(8)
	first, err := provider.Embed(context.Background(), []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if len(first) != 2 || len(first[0]) != 8 || len(first[1]) != 8 {
		t.Fatalf("Embed() returned %d vectors with lengths %d/%d, want 2 vectors of 8", len(first), len(first[0]), len(first[1]))
	}
	again, err := provider.Embed(context.Background(), []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("second Embed() error = %v", err)
	}
	for index := range first {
		for value := range first[index] {
			if again[index][value] != first[index][value] {
				t.Fatalf("derived vector %d value %d changed between calls", index, value)
			}
		}
	}
	allZero := true
	for _, value := range first[0] {
		if value != 0 {
			allZero = false
		}
	}
	if allZero {
		t.Fatal("derived vector is all zeros and would be rejected as zero-norm")
	}
}

func TestFakeEmbeddingProviderExplicitVectorsAndError(t *testing.T) {
	provider := NewFakeEmbeddingProvider(2).SetVectors([][]float32{{1, 2}})
	vectors, err := provider.Embed(context.Background(), []string{"one"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if len(vectors) != 1 || vectors[0][0] != 1 || vectors[0][1] != 2 {
		t.Fatalf("Embed() = %v, want the configured vector", vectors)
	}
	provider.SetErr(errors.New("provider down"))
	if _, err := provider.Embed(context.Background(), []string{"one"}); err == nil || err.Error() != "provider down" {
		t.Fatalf("Embed() error = %v, want the armed error", err)
	}
}
