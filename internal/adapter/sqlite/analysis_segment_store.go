package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// AnalysisSegmentStore is the SQLite implementation of
// port.AnalysisSegmentStore, fixed over the v40 analysis_segments table
// (checkpoint 6): checkpoint 2 created the table but no store wrote to it
// until now.
type AnalysisSegmentStore struct {
	db *sql.DB
}

func NewAnalysisSegmentStore(store *Store) *AnalysisSegmentStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &AnalysisSegmentStore{db: store.db}
}

var _ port.AnalysisSegmentStore = (*AnalysisSegmentStore)(nil)

// WriteManifest persists every segment of manifest. It is idempotent by
// (analysis_id, ordinal): INSERT OR IGNORE means a retried write after a
// crash, for example one that completed some rows before it crashed, never
// fails and never duplicates or overwrites a row, matching the v40
// analysis_segments_immutable trigger's UPDATE-only protection.
func (s *AnalysisSegmentStore) WriteManifest(ctx context.Context, analysisID string, manifest domain.AnalysisSegmentManifest, now time.Time) error {
	if s == nil || s.db == nil {
		return domain.ErrAnalysisUnavailable
	}
	if !domain.ValidAnalysisID(analysisID) {
		return fmt.Errorf("%w: segment manifest write requires a valid analysis id", domain.ErrAnalysisValidation)
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("%w: segment manifest write requires an injected clock", domain.ErrAnalysisValidation)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrapAnalysisCanceled(err, fmt.Errorf("%w: begin segment manifest write: %v", domain.ErrAnalysisUnavailable, err))
	}
	defer func() { _ = tx.Rollback() }()
	for _, segment := range manifest.Segments {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO analysis_segments
			(analysis_id, ordinal, offset_bytes, length_bytes, sha256, segmenter_version, overlap_prev_bytes)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			analysisID, segment.Ordinal, segment.OffsetBytes, segment.LengthBytes, segment.SHA256, segment.SegmenterVersion, segment.OverlapPrevBytes); err != nil {
			return wrapAnalysisCanceled(err, fmt.Errorf("%w: insert analysis segment: %v", domain.ErrAnalysisUnavailable, err))
		}
	}
	if err := tx.Commit(); err != nil {
		return wrapAnalysisCanceled(err, fmt.Errorf("%w: commit segment manifest write: %v", domain.ErrAnalysisUnavailable, err))
	}
	return nil
}

// ListSegments returns every durable segment for analysisID, in ordinal
// order.
func (s *AnalysisSegmentStore) ListSegments(ctx context.Context, analysisID string) ([]domain.AnalysisSegment, error) {
	if s == nil || s.db == nil {
		return nil, domain.ErrAnalysisUnavailable
	}
	if !domain.ValidAnalysisID(analysisID) {
		return nil, fmt.Errorf("%w: list segments requires a valid analysis id", domain.ErrAnalysisValidation)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ordinal, offset_bytes, length_bytes, sha256, segmenter_version, overlap_prev_bytes
		FROM analysis_segments WHERE analysis_id = ? ORDER BY ordinal ASC`, analysisID)
	if err != nil {
		return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: list analysis segments: %v", domain.ErrAnalysisUnavailable, err))
	}
	defer func() { _ = rows.Close() }()
	var segments []domain.AnalysisSegment
	for rows.Next() {
		var segment domain.AnalysisSegment
		if err := rows.Scan(&segment.Ordinal, &segment.OffsetBytes, &segment.LengthBytes, &segment.SHA256, &segment.SegmenterVersion, &segment.OverlapPrevBytes); err != nil {
			return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: scan analysis segment: %v", domain.ErrAnalysisUnavailable, err))
		}
		segments = append(segments, segment)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: iterate analysis segments: %v", domain.ErrAnalysisUnavailable, err))
	}
	return segments, nil
}
