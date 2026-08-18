package fsartifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// TypedStore binds V2 catalog-assigned result IDs to private artifact files.
// It is intentionally separate from delivery artifacts so legacy delivery
// cleanup cannot remove catalog payloads.
type TypedStore struct{ artifacts *Store }

func NewTypedStore(dir string, maxBytes int64) (*TypedStore, error) {
	artifacts, err := New(dir, maxBytes)
	if err != nil {
		return nil, err
	}
	return &TypedStore{artifacts: artifacts}, nil
}

func (s *TypedStore) StorageFor(resultID string) (domain.ResultStorage, error) {
	if s == nil || !validTypedResultID(resultID) {
		return domain.ResultStorage{}, domain.ErrResultInvalid
	}
	return domain.ResultStorage{Kind: domain.ResultStorageArtifact, Key: resultID}, nil
}

func (s *TypedStore) Publish(ctx context.Context, storage domain.ResultStorage, payload string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validateStorage(storage); err != nil || !utf8.ValidString(payload) || strings.TrimSpace(payload) == "" {
		return domain.ErrResultInvalid
	}
	digest := sha256.Sum256([]byte(payload))
	expectedSHA256 := hex.EncodeToString(digest[:])
	expectedBytes := int64(len(payload))
	artifact, err := s.artifacts.Put(ctx, storage.Key, payload)
	if err == nil && artifact.Reference == storage.Key+".result" && artifact.SHA256 == expectedSHA256 && artifact.Bytes == expectedBytes {
		return nil
	}
	if verifyErr := s.Verify(ctx, storage, expectedSHA256, expectedBytes); verifyErr == nil {
		return nil
	} else if errors.Is(verifyErr, context.Canceled) || errors.Is(verifyErr, context.DeadlineExceeded) {
		return verifyErr
	}
	return port.ErrResultPayloadConflict
}

func (s *TypedStore) Verify(ctx context.Context, storage domain.ResultStorage, expectedSHA256 string, expectedBytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validateStorage(storage); err != nil || expectedBytes <= 0 || !validTypedResultSHA256(expectedSHA256) {
		return domain.ErrResultInvalid
	}
	data, err := s.artifacts.Get(ctx, storage.Key, storage.Key+".result", expectedSHA256, expectedBytes)
	if err != nil || int64(len(data)) != expectedBytes || !utf8.Valid(data) || strings.TrimSpace(string(data)) == "" {
		return domain.ErrResultUnavailable
	}
	return nil
}

func (s *TypedStore) ReadRange(ctx context.Context, storage domain.ResultStorage, expectedSHA256 string, expectedBytes, offsetBytes, maxBytes int64) (domain.ResultChunk, error) {
	if err := ctx.Err(); err != nil {
		return domain.ResultChunk{}, err
	}
	if err := s.validateStorage(storage); err != nil || expectedBytes <= 0 || offsetBytes < 0 || maxBytes <= 0 || !validTypedResultSHA256(expectedSHA256) {
		return domain.ResultChunk{}, domain.ErrResultInvalid
	}
	chunk, err := s.artifacts.ReadChunk(ctx, domain.ResultArtifactChunkRequest{
		OwnerID: storage.Key, Reference: storage.Key + ".result", ExpectedSHA256: expectedSHA256,
		ExpectedBytes: expectedBytes, OffsetBytes: offsetBytes, MaxBytes: maxBytes,
	})
	if err == nil {
		return chunk, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return domain.ResultChunk{}, err
	}
	var resultErr *domain.ResultError
	if errors.As(err, &resultErr) && resultErr.Code == domain.ResultErrorChunkRequestInvalid {
		return domain.ResultChunk{}, domain.ErrResultInvalid
	}
	return domain.ResultChunk{}, domain.ErrResultUnavailable
}

func (s *TypedStore) validateStorage(storage domain.ResultStorage) error {
	if s == nil || s.artifacts == nil || storage.Kind != domain.ResultStorageArtifact || !validTypedResultID(storage.Key) {
		return domain.ErrResultInvalid
	}
	return nil
}

func validTypedResultID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validTypedResultSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

var _ port.ResultPayloadStore = (*TypedStore)(nil)
