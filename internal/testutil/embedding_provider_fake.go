// Package testutil provides shared deterministic fakes for adapter and
// use-case tests. It is production-free: no application code imports it.
package testutil

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
)

// FakeEmbeddingProvider is a deterministic in-memory port.EmbeddingProvider
// used by embedding-pipeline tests. It records every input it receives
// (exactly, so redaction regressions can assert what reached the provider),
// returns either preconfigured per-input vectors or deterministic vectors
// derived from the input, and can be armed with an error.
type FakeEmbeddingProvider struct {
	mu      sync.Mutex
	records []string
	vectors [][]float32
	err     error
	derived int
}

// NewFakeEmbeddingProvider constructs a fake that derives deterministic
// dimension-count vectors from each input. Dimension must be at least 1.
func NewFakeEmbeddingProvider(dimensions int) *FakeEmbeddingProvider {
	return &FakeEmbeddingProvider{derived: dimensions}
}

// SetVectors arms the fake with explicit per-input outputs. It returns the
// fake so callers can chain configuration. The number of vectors must match
// the number of inputs or Embed fails.
func (f *FakeEmbeddingProvider) SetVectors(vectors [][]float32) *FakeEmbeddingProvider {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vectors = vectors
	return f
}

// SetErr arms the fake to fail every call with the given error.
func (f *FakeEmbeddingProvider) SetErr(err error) *FakeEmbeddingProvider {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
	return f
}

// Embed records the inputs and returns preconfigured or derived vectors.
func (f *FakeEmbeddingProvider) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, inputs...)
	if f.err != nil {
		return nil, f.err
	}
	if f.vectors != nil {
		if len(f.vectors) != len(inputs) {
			return nil, errors.New("fake embedding provider: configured vectors do not match input count")
		}
		return f.vectors, nil
	}
	outputs := make([][]float32, len(inputs))
	for index, input := range inputs {
		outputs[index] = DeriveFakeEmbeddingVector(input, f.derived)
	}
	return outputs, nil
}

// RecordedInputs returns a copy of every input seen so far, in call order.
func (f *FakeEmbeddingProvider) RecordedInputs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.records...)
}

// CallCount returns the number of Embed calls completed so far.
func (f *FakeEmbeddingProvider) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

// DeriveFakeEmbeddingVector returns a deterministic non-zero dimension-count
// vector derived from the input text. Values are in (-1, 1) and stable
// across calls and processes.
func DeriveFakeEmbeddingVector(input string, dimensions int) []float32 {
	sum := sha256.Sum256([]byte(input))
	vector := make([]float32, dimensions)
	for index := range vector {
		seed := binary.LittleEndian.Uint32(sum[index%28 : index%28+4])
		value := (float64(seed%65536) / 65536.0) - 0.5
		if value == 0 {
			value = 0.25
		}
		vector[index] = float32(value)
	}
	return vector
}
