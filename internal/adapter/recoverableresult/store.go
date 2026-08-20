package recoverableresult

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var (
	errUnavailable = errors.New("result unavailable")
	errCleanup     = errors.New("recoverable result cleanup failed")
)

const cleanupClaimLease = time.Hour

type Store struct {
	db               *sql.DB
	dir              string
	maxResultBytes   int64
	chunkMaxBytes    int
	retentionDays    int
	cleanupBatchSize int
	refChecker       port.RecoverableResultReferenceChecker
	metrics          port.MetricRecorder
}

func NewStore(db *sql.DB, dir string, maxResultBytes int64, chunkMaxBytes int, retentionDays int, cleanupBatchSize int, recorders ...port.MetricRecorder) *Store {
	if db == nil || strings.TrimSpace(dir) == "" {
		panic("recoverable result store requires a database and storage directory")
	}
	if maxResultBytes <= 0 || chunkMaxBytes <= 0 || retentionDays <= 0 || cleanupBatchSize <= 0 {
		panic("recoverable result store requires positive config values")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		panic(fmt.Sprintf("create recoverable result directory: %v", err))
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		panic("recoverable result directory must be a real directory")
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil || filepath.Clean(canonical) != filepath.Clean(dir) {
		panic("recoverable result directory must be canonical")
	}
	var recorder port.MetricRecorder
	if len(recorders) > 0 {
		recorder = recorders[0]
	}
	return &Store{
		db:               db,
		dir:              filepath.Clean(dir),
		maxResultBytes:   maxResultBytes,
		chunkMaxBytes:    chunkMaxBytes,
		retentionDays:    retentionDays,
		cleanupBatchSize: cleanupBatchSize, metrics: recorder,
	}
}

func (s *Store) SetReferenceChecker(checker port.RecoverableResultReferenceChecker) {
	if s != nil {
		s.refChecker = checker
	}
}

func (s *Store) Put(ctx context.Context, req port.PutResultRequest) (result domain.RecoverableResult, err error) {
	succeeded := false
	defer func() {
		if s != nil && s.metrics != nil && !succeeded {
			s.metrics.AddCounter(domain.MetricRecoverableResultUnavailableTotal, 1, port.MetricLabels{"result_kind": req.Kind})
		}
	}()
	if err := ctx.Err(); err != nil {
		return domain.RecoverableResult{}, err
	}
	if s == nil {
		return domain.RecoverableResult{}, errUnavailable
	}
	if strings.TrimSpace(req.Actor) == "" || strings.TrimSpace(req.ConversationKey) == "" || strings.TrimSpace(req.Kind) == "" {
		return domain.RecoverableResult{}, errUnavailable
	}

	if !utf8.ValidString(req.Content) {
		return domain.RecoverableResult{}, errUnavailable
	}

	contentBytes := int64(len(req.Content))
	if contentBytes > s.maxResultBytes {
		return domain.RecoverableResult{}, errUnavailable
	}

	digest := sha256.Sum256([]byte(req.Content))
	sha256Hex := hex.EncodeToString(digest[:])

	refBytes := make([]byte, 32)
	if _, err := rand.Read(refBytes); err != nil {
		return domain.RecoverableResult{}, errUnavailable
	}
	ref := hex.EncodeToString(refBytes)

	locBytes := make([]byte, 32)
	if _, err := rand.Read(locBytes); err != nil {
		return domain.RecoverableResult{}, errUnavailable
	}
	storageLocator := hex.EncodeToString(locBytes)

	if err := atomicWriteFile(s.dir, storageLocator, []byte(req.Content)); err != nil {
		return domain.RecoverableResult{}, errUnavailable
	}

	codePoints := utf8.RuneCountInString(req.Content)
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(s.retentionDays) * 24 * time.Hour)
	if !expiresAt.After(now) {
		expiresAt = now.Add(1 * time.Second)
	}

	// Load-bearing order (FIND-115): this INSERT must commit before Put
	// returns ref to the caller. The sqlite adapter's recoverable-reference
	// index (internal/adapter/sqlite/recoverable_reference_index.go) only
	// records a hex-64 window as a live ref when it matches an existing
	// recoverable_results row at the moment content naming that ref is
	// written. If a caller ever received ref before this row existed, an
	// event or capsule commit racing ahead of this INSERT would find no
	// match, index nothing, and retention would later delete a result that
	// was in fact referenced: silent data loss, with no error and no test
	// failing. Do not return ref, start a goroutine that inserts later, or
	// otherwise let this statement become non-blocking.
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO recoverable_results
			(ref, actor, conversation_key, kind, storage_locator, size_bytes,
			 code_points, sha256, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ref, req.Actor, req.ConversationKey, req.Kind, storageLocator,
		contentBytes, codePoints, sha256Hex, now.Unix(), expiresAt.Unix())
	if err != nil {
		_ = os.Remove(filepath.Join(s.dir, storageLocator))
		return domain.RecoverableResult{}, errUnavailable
	}

	result = domain.RecoverableResult{
		Ref:             ref,
		Kind:            req.Kind,
		Actor:           req.Actor,
		ConversationKey: req.ConversationKey,
		SizeBytes:       contentBytes,
		CodePoints:      codePoints,
		SHA256:          sha256Hex,
		CreatedAt:       now,
		ExpiresAt:       expiresAt,
	}
	if s.metrics != nil {
		s.metrics.AddCounter(domain.MetricRecoverableResultPutTotal, 1, port.MetricLabels{"result_kind": req.Kind})
		s.metrics.Observe(domain.MetricRecoverableResultPutBytes, float64(contentBytes), port.MetricLabels{"result_kind": req.Kind})
		s.recordActiveCount(ctx)
	}
	succeeded = true
	return result, nil
}

func (s *Store) ReadChunk(ctx context.Context, req domain.ResultChunkRequest) (result domain.ResultChunk, err error) {
	defer func() {
		if s == nil || s.metrics == nil {
			return
		}
		if err != nil {
			s.metrics.AddCounter(domain.MetricRecoverableResultUnavailableTotal, 1, nil)
		} else {
			s.metrics.AddCounter(domain.MetricRecoverableResultChunkReadTotal, 1, nil)
		}
	}()
	if err := ctx.Err(); err != nil {
		return domain.ResultChunk{}, err
	}
	if s == nil {
		return domain.ResultChunk{}, errUnavailable
	}

	row, err := s.queryResultMeta(ctx, req.Ref)
	if err != nil {
		return domain.ResultChunk{}, err
	}

	if row.Actor != req.Actor || row.ConversationKey != req.ConversationKey {
		return domain.ResultChunk{}, errUnavailable
	}

	fileData, fileSHA256, err := readAndVerifyFile(s.dir, row.StorageLocator, row.SizeBytes, row.SHA256)
	if err != nil {
		if s.metrics != nil {
			s.metrics.AddCounter(domain.MetricRecoverableResultIntegrityFailure, 1, nil)
		}
		return domain.ResultChunk{}, errUnavailable
	}

	if req.OffsetBytes < 0 {
		return domain.ResultChunk{}, errUnavailable
	}
	if req.OffsetBytes > int64(len(fileData)) {
		return domain.ResultChunk{}, errUnavailable
	}
	start := int(req.OffsetBytes)
	if start == len(fileData) {
		return domain.ResultChunk{Content: "", OffsetBytes: req.OffsetBytes, NextOffsetBytes: req.OffsetBytes, EOF: true, SHA256: fileSHA256}, nil
	}
	if start > 0 && !utf8.RuneStart(fileData[start]) {
		return domain.ResultChunk{}, errUnavailable
	}

	maxBytes := req.MaxBytes
	if maxBytes <= 0 || maxBytes > s.chunkMaxBytes {
		maxBytes = s.chunkMaxBytes
	}
	end := start + maxBytes
	if end >= len(fileData) {
		end = len(fileData)
	} else {
		end = utf8BoundaryEnd(fileData, end)
		if end == start {
			return domain.ResultChunk{}, errUnavailable
		}
	}

	content := string(fileData[start:end])
	nextOffset := int64(end)

	result = domain.ResultChunk{
		Content:         content,
		OffsetBytes:     int64(start),
		NextOffsetBytes: nextOffset,
		EOF:             end == len(fileData),
		SHA256:          fileSHA256,
	}
	return result, nil
}

func (s *Store) Stat(ctx context.Context, req port.StatResultRequest) (result domain.RecoverableResult, err error) {
	defer func() {
		if s != nil && s.metrics != nil && err != nil {
			s.metrics.AddCounter(domain.MetricRecoverableResultUnavailableTotal, 1, nil)
		}
	}()
	if err := ctx.Err(); err != nil {
		return domain.RecoverableResult{}, err
	}
	if s == nil {
		return domain.RecoverableResult{}, errUnavailable
	}

	row, err := s.queryResultMeta(ctx, req.Ref)
	if err != nil {
		return domain.RecoverableResult{}, err
	}

	if row.Actor != req.Actor || row.ConversationKey != req.ConversationKey {
		return domain.RecoverableResult{}, errUnavailable
	}

	if _, _, err := readAndVerifyFile(s.dir, row.StorageLocator, row.SizeBytes, row.SHA256); err != nil {
		if s.metrics != nil {
			s.metrics.AddCounter(domain.MetricRecoverableResultIntegrityFailure, 1, nil)
		}
		return domain.RecoverableResult{}, errUnavailable
	}

	result = domain.RecoverableResult{
		Ref:             row.Ref,
		Kind:            row.Kind,
		Actor:           row.Actor,
		ConversationKey: row.ConversationKey,
		SizeBytes:       row.SizeBytes,
		CodePoints:      row.CodePoints,
		SHA256:          row.SHA256,
		CreatedAt:       time.Unix(row.CreatedAt, 0).UTC(),
		ExpiresAt:       time.Unix(row.ExpiresAt, 0).UTC(),
	}
	return result, nil
}

func (s *Store) DeleteExpired(ctx context.Context, cutoff time.Time, batchSize int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s == nil {
		return 0, nil
	}
	if s.refChecker == nil {
		return 0, nil
	}
	if batchSize <= 0 || batchSize > s.cleanupBatchSize {
		batchSize = s.cleanupBatchSize
	}

	cutoffUnix := cutoff.UTC().Unix()
	staleClaimUnix := time.Now().UTC().Add(-cleanupClaimLease).Unix()

	rows, err := s.db.QueryContext(ctx,
		`SELECT ref, storage_locator, cleanup_claim, cleanup_version FROM recoverable_results
		 WHERE expires_at <= ? AND (cleanup_claim = '' OR cleanup_claimed_at <= ?)
		 LIMIT ?`, cutoffUnix, staleClaimUnix, batchSize)
	if err != nil {
		return 0, fmt.Errorf("%w: query candidates", errCleanup)
	}

	var candidates []cleanupCandidate
	for rows.Next() {
		var candidate cleanupCandidate
		if err := rows.Scan(&candidate.ref, &candidate.locator, &candidate.previousClaim, &candidate.version); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("%w: scan candidate", errCleanup)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("%w: iterate candidates", errCleanup)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("%w: close candidate query", errCleanup)
	}

	deleted := 0
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}

		claim, err := randomOpaqueID()
		if err != nil {
			return deleted, fmt.Errorf("%w: create claim", errCleanup)
		}
		claimedVersion, claimed, err := s.claimCleanup(ctx, candidate, claim, cutoffUnix, staleClaimUnix)
		if err != nil {
			return deleted, fmt.Errorf("%w: claim candidate", errCleanup)
		}
		if !claimed {
			continue
		}

		referenced, err := s.refChecker.IsRecoverableResultReferenced(ctx, candidate.ref)
		if err != nil {
			if releaseErr := s.releaseCleanupClaim(ctx, candidate.ref, claim, claimedVersion); releaseErr != nil {
				return deleted, fmt.Errorf("%w: release candidate after reference check", errCleanup)
			}
			return deleted, fmt.Errorf("%w: check durable reference", errCleanup)
		}
		if referenced {
			if err := s.releaseCleanupClaim(ctx, candidate.ref, claim, claimedVersion); err != nil {
				return deleted, fmt.Errorf("%w: release referenced candidate", errCleanup)
			}
			continue
		}

		if err := verifyFileExists(s.dir, candidate.locator); err != nil {
			if err := s.deleteMetadataCAS(ctx, candidate.ref, candidate.locator, claim, claimedVersion); err != nil {
				return deleted, fmt.Errorf("%w: remove unavailable candidate metadata", errCleanup)
			}
			deleted++
			continue
		}

		if err := removeFileNoFollow(s.dir, candidate.locator); err != nil && !os.IsNotExist(err) {
			if releaseErr := s.releaseCleanupClaim(ctx, candidate.ref, claim, claimedVersion); releaseErr != nil {
				return deleted, fmt.Errorf("%w: release candidate after remove failure", errCleanup)
			}
			return deleted, fmt.Errorf("%w: remove candidate file", errCleanup)
		}

		if err := s.deleteMetadataCAS(ctx, candidate.ref, candidate.locator, claim, claimedVersion); err != nil {
			return deleted, fmt.Errorf("%w: remove candidate metadata", errCleanup)
		}
		deleted++
	}

	if s.metrics != nil && deleted > 0 {
		s.metrics.AddCounter(domain.MetricRecoverableResultCleanupTotal, int64(deleted), nil)
		s.recordActiveCount(ctx)
	}
	return deleted, nil
}

func (s *Store) recordActiveCount(ctx context.Context) {
	if s == nil || s.metrics == nil {
		return
	}
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recoverable_results`).Scan(&count); err == nil {
		s.metrics.SetGauge(domain.MetricRecoverableResultActiveCount, count, nil)
	}
}

type cleanupCandidate struct {
	ref           string
	locator       string
	previousClaim string
	version       int64
}

type resultMetaRow struct {
	Ref             string
	Actor           string
	ConversationKey string
	Kind            string
	StorageLocator  string
	SizeBytes       int64
	CodePoints      int
	SHA256          string
	CreatedAt       int64
	ExpiresAt       int64
}

func (s *Store) queryResultMeta(ctx context.Context, ref string) (resultMetaRow, error) {
	if !validOpaqueID(ref) {
		return resultMetaRow{}, errUnavailable
	}
	var row resultMetaRow
	now := time.Now().UTC().Unix()
	err := s.db.QueryRowContext(ctx,
		`SELECT ref, actor, conversation_key, kind, storage_locator,
		        size_bytes, code_points, sha256, created_at, expires_at
		 FROM recoverable_results
		 WHERE ref = ? AND expires_at > ?`, ref, now).Scan(
		&row.Ref, &row.Actor, &row.ConversationKey, &row.Kind,
		&row.StorageLocator, &row.SizeBytes, &row.CodePoints,
		&row.SHA256, &row.CreatedAt, &row.ExpiresAt)
	if err != nil {
		return resultMetaRow{}, errUnavailable
	}
	return row, nil
}

func (s *Store) claimCleanup(ctx context.Context, candidate cleanupCandidate, claim string, cutoffUnix, staleClaimUnix int64) (int64, bool, error) {
	claimedAt := time.Now().UTC().Unix()
	result, err := s.db.ExecContext(ctx,
		`UPDATE recoverable_results
		 SET cleanup_claim = ?, cleanup_version = cleanup_version + 1, cleanup_claimed_at = ?
		 WHERE ref = ? AND storage_locator = ? AND cleanup_claim = ?
		   AND cleanup_version = ? AND expires_at <= ?
		   AND (cleanup_claim = '' OR cleanup_claimed_at <= ?)`,
		claim, claimedAt, candidate.ref, candidate.locator, candidate.previousClaim,
		candidate.version, cutoffUnix, staleClaimUnix)
	if err != nil {
		return 0, false, err
	}
	affected, err := result.RowsAffected()
	return candidate.version + 1, affected == 1, err
}

func (s *Store) releaseCleanupClaim(ctx context.Context, ref, claim string, version int64) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE recoverable_results SET cleanup_claim = '', cleanup_claimed_at = 0
		 WHERE ref = ? AND cleanup_claim = ? AND cleanup_version = ?`,
		ref, claim, version)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("cleanup claim changed")
	}
	return nil
}

func (s *Store) deleteMetadataCAS(ctx context.Context, ref, locator, claim string, version int64) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM recoverable_results
		 WHERE ref = ? AND storage_locator = ? AND cleanup_claim = ? AND cleanup_version = ?`,
		ref, locator, claim, version)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("no matching row")
	}
	return nil
}

func randomOpaqueID() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func atomicWriteFile(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".recoverable-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	target := filepath.Join(dir, name)
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func readAndVerifyFile(baseDir, locator string, expectedSize int64, expectedSHA256 string) ([]byte, string, error) {
	if !isWithinDir(filepath.Join(baseDir, locator), baseDir) {
		return nil, "", errors.New("storage path escapes base directory")
	}
	if expectedSize < 0 || expectedSize >= int64(math.MaxInt) {
		return nil, "", errors.New("storage size is invalid")
	}

	f, err := openRegularNoFollow(baseDir, locator)
	if err != nil {
		return nil, "", fmt.Errorf("open storage file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("stat storage file: %w", err)
	}
	if info.Size() != expectedSize {
		return nil, "", errors.New("storage size mismatch")
	}

	data, err := io.ReadAll(io.LimitReader(f, expectedSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("read storage file: %w", err)
	}
	if int64(len(data)) != expectedSize {
		return nil, "", errors.New("storage size mismatch")
	}

	digest := sha256.Sum256(data)
	actualSHA256 := hex.EncodeToString(digest[:])
	if !strings.EqualFold(actualSHA256, expectedSHA256) {
		return nil, "", errors.New("sha256 integrity check failed")
	}

	return data, actualSHA256, nil
}

func verifyFileExists(baseDir, locator string) error {
	if !isWithinDir(filepath.Join(baseDir, locator), baseDir) {
		return errors.New("storage path escapes base directory")
	}

	f, err := openRegularNoFollow(baseDir, locator)
	if err != nil {
		return fmt.Errorf("open storage file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat storage file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("storage file is not a regular file")
	}
	return nil
}

func isWithinDir(filePath, baseDir string) bool {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(baseDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func utf8BoundaryEnd(data []byte, pos int) int {
	if pos <= 0 || pos > len(data) {
		return len(data)
	}
	if pos < len(data) && !utf8.RuneStart(data[pos]) {
		start := pos
		for start > 0 && !utf8.RuneStart(data[start]) {
			start--
		}
		return start
	}
	return pos
}

var _ port.RecoverableResultStore = (*Store)(nil)
