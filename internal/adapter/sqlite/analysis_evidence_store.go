package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// AnalysisEvidenceStore is the SQLite implementation of
// port.AnalysisEvidenceStore, fixed over the v40 analysis_evidence table.
type AnalysisEvidenceStore struct {
	db *sql.DB
}

func NewAnalysisEvidenceStore(store *Store) *AnalysisEvidenceStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &AnalysisEvidenceStore{db: store.db}
}

var _ port.AnalysisEvidenceStore = (*AnalysisEvidenceStore)(nil)

// Write persists one excerpt. It is idempotent by excerpt.EvidenceID:
// INSERT OR IGNORE means a retried resolution of the same selector against
// the same segment (which always derives the same evidence id, see
// resultanalysis.evidenceID) writes nothing a second time. The v40
// analysis_evidence_immutable trigger is the second, durable layer of
// protection against a later caller ever changing a written excerpt.
func (s *AnalysisEvidenceStore) Write(ctx context.Context, analysisID string, leafStepID string, excerpt port.AnalysisEvidenceExcerpt, now time.Time) error {
	if s == nil || s.db == nil {
		return domain.ErrAnalysisUnavailable
	}
	if !domain.ValidAnalysisID(analysisID) {
		return fmt.Errorf("%w: evidence write requires a valid analysis id", domain.ErrAnalysisValidation)
	}
	if !validBoundedStepID(leafStepID) {
		return fmt.Errorf("%w: evidence write requires a bounded leaf step id", domain.ErrAnalysisValidation)
	}
	if !validResultOpaqueID(excerpt.EvidenceID) {
		return fmt.Errorf("%w: evidence write requires a valid evidence id", domain.ErrAnalysisValidation)
	}
	if !validResultOpaqueID(excerpt.SHA256) {
		return fmt.Errorf("%w: evidence write requires a valid excerpt digest", domain.ErrAnalysisValidation)
	}
	if excerpt.Excerpt == "" || len(excerpt.Excerpt) > domain.HardMaxAnalysisEvidenceExcerptBytes {
		return fmt.Errorf("%w: evidence excerpt must be bounded and non-empty", domain.ErrAnalysisValidation)
	}
	if err := excerpt.Range.Validate(); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("%w: evidence write requires an injected clock", domain.ErrAnalysisValidation)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO analysis_evidence (
		evidence_id, analysis_id, leaf_step_id, segment_ordinal, offset_bytes, length_bytes, sha256, excerpt_bytes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(evidence_id) DO NOTHING`,
		excerpt.EvidenceID, analysisID, leafStepID, excerpt.SegmentOrdinal, excerpt.Range.OffsetBytes, excerpt.Range.LengthBytes,
		excerpt.SHA256, excerpt.Excerpt, now.UTC().Unix())
	if err != nil {
		return wrapAnalysisCanceled(err, fmt.Errorf("%w: insert analysis evidence: %v", domain.ErrAnalysisUnavailable, err))
	}
	return nil
}

// ListByLeafStep returns every excerpt written for one leaf step, in
// evidence-id order for deterministic reduction-time assembly.
func (s *AnalysisEvidenceStore) ListByLeafStep(ctx context.Context, analysisID string, leafStepID string) ([]port.AnalysisEvidenceExcerpt, error) {
	if s == nil || s.db == nil {
		return nil, domain.ErrAnalysisUnavailable
	}
	if !domain.ValidAnalysisID(analysisID) {
		return nil, fmt.Errorf("%w: evidence list requires a valid analysis id", domain.ErrAnalysisValidation)
	}
	if !validBoundedStepID(leafStepID) {
		return nil, fmt.Errorf("%w: evidence list requires a bounded leaf step id", domain.ErrAnalysisValidation)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT evidence_id, segment_ordinal, offset_bytes, length_bytes, sha256, excerpt_bytes
		FROM analysis_evidence WHERE analysis_id = ? AND leaf_step_id = ? ORDER BY evidence_id`, analysisID, leafStepID)
	if err != nil {
		return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: list analysis evidence: %v", domain.ErrAnalysisUnavailable, err))
	}
	defer rows.Close()
	var out []port.AnalysisEvidenceExcerpt
	for rows.Next() {
		var e port.AnalysisEvidenceExcerpt
		if err := rows.Scan(&e.EvidenceID, &e.SegmentOrdinal, &e.Range.OffsetBytes, &e.Range.LengthBytes, &e.SHA256, &e.Excerpt); err != nil {
			return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: scan analysis evidence: %v", domain.ErrAnalysisUnavailable, err))
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: scan analysis evidence rows: %v", domain.ErrAnalysisUnavailable, err))
	}
	return out, nil
}
