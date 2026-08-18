package recoverableresult

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// TypedStore stores V2 payloads under their catalog-assigned result IDs. It
// intentionally does not create legacy recoverable_results metadata.
type TypedStore struct {
	dir      string
	maxBytes int64
}

func NewTypedStore(dir string, maxBytes int64) (*TypedStore, error) {
	if strings.TrimSpace(dir) == "" || maxBytes <= 0 {
		return nil, errors.New("typed result directory and positive maximum size are required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("create typed result directory failed")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("typed result directory must be a real directory")
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil || filepath.Clean(canonical) != filepath.Clean(dir) {
		return nil, errors.New("typed result directory must be canonical")
	}
	return &TypedStore{dir: filepath.Clean(dir), maxBytes: maxBytes}, nil
}

func (s *TypedStore) StorageFor(resultID string) (domain.ResultStorage, error) {
	if s == nil || !validOpaqueID(resultID) {
		return domain.ResultStorage{}, domain.ErrResultInvalid
	}
	return domain.ResultStorage{Kind: domain.ResultStorageRecoverable, Key: resultID}, nil
}

func (s *TypedStore) Publish(ctx context.Context, storage domain.ResultStorage, payload string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validateStorage(storage); err != nil {
		return err
	}
	if !utf8.ValidString(payload) || strings.TrimSpace(payload) == "" || int64(len(payload)) > s.maxBytes {
		return domain.ErrResultInvalid
	}
	data := []byte(payload)
	if err := s.publishNew(storage.Key, data); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrExist) {
		return errors.New("publish typed result payload failed")
	}

	digest := sha256.Sum256(data)
	if err := s.Verify(ctx, storage, hex.EncodeToString(digest[:]), int64(len(data))); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: existing payload differs", port.ErrResultPayloadConflict)
	}
	return nil
}

func (s *TypedStore) Verify(ctx context.Context, storage domain.ResultStorage, expectedSHA256 string, expectedBytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validateStorage(storage); err != nil {
		return err
	}
	if expectedBytes <= 0 || expectedBytes > s.maxBytes {
		return domain.ErrResultUnavailable
	}
	data, actual, err := readAndVerifyFile(s.dir, storage.Key, expectedBytes, expectedSHA256)
	if err != nil || !utf8.Valid(data) || strings.TrimSpace(string(data)) == "" || actual != expectedSHA256 {
		return domain.ErrResultUnavailable
	}
	return nil
}

func (s *TypedStore) ReadRange(ctx context.Context, storage domain.ResultStorage, expectedSHA256 string, expectedBytes, offsetBytes, maxBytes int64) (domain.ResultChunk, error) {
	if err := ctx.Err(); err != nil {
		return domain.ResultChunk{}, err
	}
	if err := s.validateStorage(storage); err != nil {
		return domain.ResultChunk{}, err
	}
	if expectedBytes <= 0 || expectedBytes > s.maxBytes || offsetBytes < 0 || offsetBytes > expectedBytes || maxBytes <= 0 {
		return domain.ResultChunk{}, domain.ErrResultInvalid
	}
	data, actual, err := readAndVerifyFile(s.dir, storage.Key, expectedBytes, expectedSHA256)
	if err != nil || !utf8.Valid(data) || strings.TrimSpace(string(data)) == "" {
		return domain.ResultChunk{}, domain.ErrResultUnavailable
	}
	if offsetBytes == expectedBytes {
		return domain.ResultChunk{OffsetBytes: offsetBytes, NextOffsetBytes: offsetBytes, EOF: true, SHA256: actual}, nil
	}
	start := int(offsetBytes)
	if !utf8.RuneStart(data[start]) {
		return domain.ResultChunk{}, domain.ErrResultInvalid
	}
	end := offsetBytes + maxBytes
	if end < offsetBytes || end > expectedBytes {
		end = expectedBytes
	}
	endIndex := int(end)
	if endIndex < len(data) {
		endIndex = utf8BoundaryEnd(data, endIndex)
	}
	if endIndex == start {
		return domain.ResultChunk{}, domain.ErrResultInvalid
	}
	return domain.ResultChunk{
		Content: string(data[start:endIndex]), OffsetBytes: offsetBytes,
		NextOffsetBytes: int64(endIndex), EOF: endIndex == len(data), SHA256: actual,
	}, nil
}

func (s *TypedStore) validateStorage(storage domain.ResultStorage) error {
	if s == nil || s.maxBytes <= 0 || storage.Kind != domain.ResultStorageRecoverable || !validOpaqueID(storage.Key) {
		return domain.ErrResultInvalid
	}
	return nil
}

func (s *TypedStore) publishNew(key string, data []byte) error {
	tmp, err := os.CreateTemp(s.dir, ".typed-result-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpName, filepath.Join(s.dir, key)); err != nil {
		return err
	}
	return syncDirectory(s.dir)
}

var _ port.ResultPayloadStore = (*TypedStore)(nil)
