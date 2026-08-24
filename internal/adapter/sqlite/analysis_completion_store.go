package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// AnalysisCompletionStore is the SQLite implementation of
// port.AnalysisCompletionStore. It commits the atomic checkpoint-4
// completion transaction: mark the analysis row completed, write one v34
// host_reduction result_representations row, and write one live v34
// result_references row for the source result.
//
// The terminal packet payload itself is not written here: it already lives
// in the root reduction step's output_payload, committed by the same
// AnalysisStepPayloadStore.WritePayload plus AnalysisStepStore.Complete
// pair that finished the root step. This store only records that the
// analysis, as a whole, is now complete.
type AnalysisCompletionStore struct {
	db *sql.DB
}

func NewAnalysisCompletionStore(store *Store) *AnalysisCompletionStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &AnalysisCompletionStore{db: store.db}
}

var _ port.AnalysisCompletionStore = (*AnalysisCompletionStore)(nil)

const analysisReferenceOwnerKind = "result_analysis"

// CompleteAnalysis commits the three writes in one transaction. Both the
// representation and the reference row use a deterministic identifier
// derived from stable inputs (see analysisRepresentationID and
// analysisReferenceID below) and are inserted with ON CONFLICT DO NOTHING,
// so a retried call for an analysis that already committed writes nothing a
// second time and still returns success.
func (s *AnalysisCompletionStore) CompleteAnalysis(ctx context.Context, input port.AnalysisCompletionInput, now time.Time) error {
	if s == nil || s.db == nil {
		return domain.ErrAnalysisUnavailable
	}
	if !domain.ValidAnalysisID(input.AnalysisID) {
		return fmt.Errorf("%w: completion requires a valid analysis id", domain.ErrAnalysisValidation)
	}
	if err := input.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: completion requires a valid scope", domain.ErrAnalysisValidation)
	}
	if !validResultOpaqueID(input.SourceResultID) || !validResultOpaqueID(input.SourceSHA256) {
		return fmt.Errorf("%w: completion requires a valid source identity", domain.ErrAnalysisValidation)
	}
	if input.SourceBytes <= 0 {
		return fmt.Errorf("%w: completion requires a positive source size", domain.ErrAnalysisValidation)
	}
	if !validResultOpaqueID(input.PacketSHA256) {
		return fmt.Errorf("%w: completion requires a valid packet digest", domain.ErrAnalysisValidation)
	}
	if input.PacketBytes <= 0 {
		return fmt.Errorf("%w: completion requires a positive packet size", domain.ErrAnalysisValidation)
	}
	if input.PromptVersion == "" {
		return fmt.Errorf("%w: completion requires a prompt version", domain.ErrAnalysisValidation)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: completion requires an injected clock", domain.ErrAnalysisValidation)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return wrapAnalysisCanceled(err, fmt.Errorf("%w: begin analysis completion: %v", domain.ErrAnalysisUnavailable, err))
	}
	defer func() { _ = tx.Rollback() }()

	nowUnix := now.UTC().Unix()
	result, err := tx.ExecContext(ctx, `UPDATE result_analyses SET state = 'completed', updated_at = ?
		WHERE analysis_id = ? AND actor = ? AND team_id = ? AND conversation_key = ? AND project = ?
			AND state IN ('preparing', 'running', 'completed')`,
		nowUnix, input.AnalysisID, input.Scope.Actor, input.Scope.TeamID, input.Scope.ConversationKey, input.Scope.Project)
	if err != nil {
		return wrapAnalysisCanceled(err, fmt.Errorf("%w: mark analysis completed: %v", domain.ErrAnalysisUnavailable, err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return wrapAnalysisCanceled(err, fmt.Errorf("%w: inspect analysis completion: %v", domain.ErrAnalysisUnavailable, err))
	}
	if affected != 1 {
		return fmt.Errorf("%w: analysis is not visible or already failed", domain.ErrAnalysisUnavailable)
	}

	representationID := analysisRepresentationID(input.AnalysisID, input.SourceResultID)
	if _, err := tx.ExecContext(ctx, `INSERT INTO result_representations (
		representation_id, result_id, kind, state, source_sha256, source_bytes,
		algorithm_or_prompt_version, payload_sha256, payload_bytes, created_at)
		VALUES (?, ?, 'host_reduction', 'available', ?, ?, ?, ?, ?, ?)
		ON CONFLICT(representation_id) DO NOTHING`,
		representationID, input.SourceResultID, input.SourceSHA256, input.SourceBytes,
		input.PromptVersion, input.PacketSHA256, input.PacketBytes, nowUnix); err != nil {
		return wrapAnalysisCanceled(err, fmt.Errorf("%w: write host_reduction representation: %v", domain.ErrAnalysisUnavailable, err))
	}

	referenceID := analysisReferenceID(input.AnalysisID, input.SourceResultID)
	if _, err := tx.ExecContext(ctx, `INSERT INTO result_references (
		reference_id, result_id, owner_kind, owner_id, state, created_at)
		VALUES (?, ?, ?, ?, 'live', ?)
		ON CONFLICT(reference_id) DO NOTHING`,
		referenceID, input.SourceResultID, analysisReferenceOwnerKind, input.AnalysisID, nowUnix); err != nil {
		return wrapAnalysisCanceled(err, fmt.Errorf("%w: write analysis result reference: %v", domain.ErrAnalysisUnavailable, err))
	}

	if err := tx.Commit(); err != nil {
		return wrapAnalysisCanceled(err, fmt.Errorf("%w: commit analysis completion: %v", domain.ErrAnalysisUnavailable, err))
	}
	return nil
}

// analysisReferenceID derives the same deterministic identifier scheme
// external_agent_job_store.go already uses for result_references:
// sha256(owner_kind || 0x00 || owner_id || 0x00 || result_id). owner_id is
// the analysis id here, so a retried completion of the same analysis always
// derives the same reference id and the ON CONFLICT DO NOTHING above makes
// the retry a no-op instead of a second row.
func analysisReferenceID(analysisID, sourceResultID string) string {
	digest := sha256.Sum256([]byte(analysisReferenceOwnerKind + "\x00" + analysisID + "\x00" + sourceResultID))
	return fmt.Sprintf("%x", digest)
}

// analysisRepresentationID derives a deterministic representation id from
// the analysis id and the source result id, netstring-free but still
// collision-safe because both inputs are fixed-length 64-hex identifiers
// with no separator ambiguity. This is not a TRD-mandated derivation (only
// the reference id derivation is specified); it is chosen so a retried
// completion is fully idempotent at the row level, matching the reference
// row's own idempotency, rather than leaving a second host_reduction
// representation as a byproduct of a retry.
func analysisRepresentationID(analysisID, sourceResultID string) string {
	digest := sha256.Sum256([]byte("host_reduction\x00" + analysisID + "\x00" + sourceResultID))
	return fmt.Sprintf("%x", digest)
}
