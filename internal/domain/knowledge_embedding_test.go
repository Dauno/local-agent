package domain

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestModelFingerprintIsStableAndCoversEveryInput(t *testing.T) {
	base := ModelFingerprint("acme", "m-3", 1536)
	if len(base) != 64 {
		t.Fatalf("ModelFingerprint() length = %d, want 64 hex characters", len(base))
	}
	if again := ModelFingerprint("acme", "m-3", 1536); again != base {
		t.Fatalf("ModelFingerprint() is not stable: %q then %q", base, again)
	}
	changes := []struct {
		name   string
		fp     string
		reason string
	}{
		{"provider id", ModelFingerprint("other", "m-3", 1536), "provider_id"},
		{"model", ModelFingerprint("acme", "m-4", 1536), "model"},
		{"dimensions", ModelFingerprint("acme", "m-3", 3072), "dimensions"},
	}
	for _, change := range changes {
		if change.fp == base {
			t.Fatalf("ModelFingerprint() did not change when %s changed", change.reason)
		}
	}
}

func TestModelFingerprintExactNULJoinedLayout(t *testing.T) {
	// Pinned digests over the exact NUL-joined literal layout
	// provider_id NUL model NUL dimensions NUL l2-f32le-v1.
	if got := ModelFingerprint("acme", "m-3", 1536); got != "7a383184569e95e85cbb1494ddd662e043b958c6faac5f8960b7340d2fb72e45" {
		t.Fatalf("ModelFingerprint() = %q, want the pinned 1536-dimension digest", got)
	}
	if got := ModelFingerprint("acme", "m-3", 3072); got != "cd6856e767c612d5ddb1b133c3a57b9e62565b4d72012779bb087bdd088c2e94" {
		t.Fatalf("ModelFingerprint() = %q, want the pinned 3072-dimension digest", got)
	}
}

func TestValidateEmbeddingOutput(t *testing.T) {
	valid := []float32{0.1, -0.2, 0.3}
	if err := ValidateEmbeddingOutput(valid, 3); err != nil {
		t.Fatalf("ValidateEmbeddingOutput() error = %v, want nil for a valid vector", err)
	}
	if err := ValidateEmbeddingOutput(valid, 2); err == nil {
		t.Fatal("ValidateEmbeddingOutput() accepted a short vector")
	}
	if err := ValidateEmbeddingOutput(valid, 4); err == nil {
		t.Fatal("ValidateEmbeddingOutput() accepted a long vector")
	}
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		if err := ValidateEmbeddingOutput([]float32{bad, 0, 0}, 3); err == nil {
			t.Fatalf("ValidateEmbeddingOutput() accepted non-finite value %v", bad)
		}
	}
}

func TestNormalizeEmbeddingVectorNormalizesInFloat64(t *testing.T) {
	vector := []float32{3, 4}
	normalized, err := NormalizeEmbeddingVector(vector, 2)
	if err != nil {
		t.Fatalf("NormalizeEmbeddingVector() error = %v", err)
	}
	if len(normalized) != 2 {
		t.Fatalf("NormalizeEmbeddingVector() length = %d, want 2", len(normalized))
	}
	if math.Abs(float64(normalized[0])-0.6) > 1e-7 || math.Abs(float64(normalized[1])-0.8) > 1e-7 {
		t.Fatalf("NormalizeEmbeddingVector() = %v, want [0.6 0.8] within tolerance", normalized)
	}
}

func TestNormalizeEmbeddingVectorRejectsZeroNormAndNonFinite(t *testing.T) {
	if _, err := NormalizeEmbeddingVector([]float32{0, 0, 0}, 3); err == nil {
		t.Fatal("NormalizeEmbeddingVector() accepted a zero-norm vector")
	}
	if _, err := NormalizeEmbeddingVector([]float32{float32(math.NaN()), 1}, 2); err == nil {
		t.Fatal("NormalizeEmbeddingVector() accepted a NaN value")
	}
	if _, err := NormalizeEmbeddingVector([]float32{1}, 2); err == nil {
		t.Fatal("NormalizeEmbeddingVector() accepted the wrong dimension count")
	}
}

func TestNormalizeEmbeddingVectorMatchesEncodedOutput(t *testing.T) {
	vector := []float32{3, 4}
	normalized, err := NormalizeEmbeddingVector(vector, 2)
	if err != nil {
		t.Fatalf("NormalizeEmbeddingVector() error = %v", err)
	}
	encoded, err := NormalizeAndEncodeEmbedding(vector, 2)
	if err != nil {
		t.Fatalf("NormalizeAndEncodeEmbedding() error = %v", err)
	}
	for index, value := range normalized {
		decoded := math.Float32frombits(binary.LittleEndian.Uint32(encoded[index*4:]))
		if decoded != value {
			t.Fatalf("encoded value %d = %v, want the NormalizeEmbeddingVector value %v", index, decoded, value)
		}
	}
}

func TestNormalizeAndEncodeEmbeddingRoundTrip(t *testing.T) {
	vector := []float32{3, 4}
	encoded, err := NormalizeAndEncodeEmbedding(vector, 2)
	if err != nil {
		t.Fatalf("NormalizeAndEncodeEmbedding() error = %v", err)
	}
	if len(encoded) != 8 {
		t.Fatalf("NormalizeAndEncodeEmbedding() length = %d, want 8 bytes", len(encoded))
	}
	want := []float32{0.6, 0.8}
	for index, expected := range want {
		got := math.Float32frombits(binary.LittleEndian.Uint32(encoded[index*4:]))
		if math.Abs(float64(got-expected)) > 1e-7 {
			t.Fatalf("decoded value %d = %v, want %v within normalization tolerance", index, got, expected)
		}
	}
}

func TestNormalizeAndEncodeEmbeddingRejectsZeroNormAndNonFinite(t *testing.T) {
	if _, err := NormalizeAndEncodeEmbedding([]float32{0, 0, 0}, 3); err == nil {
		t.Fatal("NormalizeAndEncodeEmbedding() accepted a zero-norm vector")
	}
	if _, err := NormalizeAndEncodeEmbedding([]float32{float32(math.NaN()), 1}, 2); err == nil {
		t.Fatal("NormalizeAndEncodeEmbedding() accepted a NaN value")
	}
	if _, err := NormalizeAndEncodeEmbedding([]float32{1}, 2); err == nil {
		t.Fatal("NormalizeAndEncodeEmbedding() accepted the wrong dimension count")
	}
}

func TestNormalizeAndEncodeEmbeddingUnitVectorSurvivesExactly(t *testing.T) {
	vector := []float32{1, 0, 0}
	encoded, err := NormalizeAndEncodeEmbedding(vector, 3)
	if err != nil {
		t.Fatalf("NormalizeAndEncodeEmbedding() error = %v", err)
	}
	for index, expected := range vector {
		got := math.Float32frombits(binary.LittleEndian.Uint32(encoded[index*4:]))
		if got != expected {
			t.Fatalf("unit vector value %d = %v, want %v unchanged", index, got, expected)
		}
	}
}
