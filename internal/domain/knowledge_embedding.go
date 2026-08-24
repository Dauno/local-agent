package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
)

// ModelFingerprint returns the frozen SHA-256 fingerprint over the
// NUL-joined sequence `provider_id NUL model NUL dimensions NUL l2-f32le-v1`.
// The opaque provider ID distinguishes endpoints without persisting their
// URL; the fingerprint is hex-encoded over the full digest.
func ModelFingerprint(providerID, model string, dimensions int) string {
	sum := sha256.Sum256([]byte(providerID + "\x00" + model + "\x00" + strconv.Itoa(dimensions) + "\x00" + KnowledgeVectorEncodingVersion))
	return hex.EncodeToString(sum[:])
}

// ValidateEmbeddingOutput enforces the provider output contract: the vector
// must have exactly the configured dimension count and every value must be
// finite. NaN and positive or negative infinity are rejected.
func ValidateEmbeddingOutput(vector []float32, dimensions int) error {
	if dimensions < 1 || dimensions > HardMaxKnowledgeEmbeddingDimensions {
		return fmt.Errorf("%w: embedding dimensions %d are outside the closed 1..%d bound", ErrKnowledgeInvalidValue, dimensions, HardMaxKnowledgeEmbeddingDimensions)
	}
	if len(vector) != dimensions {
		return fmt.Errorf("%w: embedding output has %d values, want exactly %d", ErrKnowledgeInvalidValue, len(vector), dimensions)
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("%w: embedding output contains a non-finite value", ErrKnowledgeInvalidValue)
		}
	}
	return nil
}

// NormalizeEmbeddingVector validates the provider output, L2-normalizes it
// in float64, rejects a zero norm, and returns the normalized finite
// float32 vector. Query vectors use this directly for in-memory comparison;
// they are never persisted.
func NormalizeEmbeddingVector(vector []float32, dimensions int) ([]float32, error) {
	if err := ValidateEmbeddingOutput(vector, dimensions); err != nil {
		return nil, err
	}
	squared := 0.0
	for _, value := range vector {
		asFloat64 := float64(value)
		squared += asFloat64 * asFloat64
	}
	norm := math.Sqrt(squared)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, fmt.Errorf("%w: embedding output has a zero norm and cannot be normalized", ErrKnowledgeInvalidValue)
	}
	normalized := make([]float32, dimensions)
	for index, value := range vector {
		converted := float32(float64(value) / norm)
		if math.IsNaN(float64(converted)) || math.IsInf(float64(converted), 0) {
			return nil, fmt.Errorf("%w: embedding normalization produced a non-finite value", ErrKnowledgeInvalidValue)
		}
		normalized[index] = converted
	}
	return normalized, nil
}

// NormalizeAndEncodeEmbedding normalizes the provider output and encodes the
// result as little-endian IEEE-754 bytes for storage in
// knowledge_embeddings.vector. The returned byte slice always has length
// dimensions * 4.
func NormalizeAndEncodeEmbedding(vector []float32, dimensions int) ([]byte, error) {
	normalized, err := NormalizeEmbeddingVector(vector, dimensions)
	if err != nil {
		return nil, err
	}
	encoded := make([]byte, dimensions*4)
	for index, value := range normalized {
		binary.LittleEndian.PutUint32(encoded[index*4:], math.Float32bits(value))
	}
	return encoded, nil
}
