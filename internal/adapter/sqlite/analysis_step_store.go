package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// AnalysisStepStore is the SQLite implementation of port.AnalysisStepStore,
// fixed over the v40 analysis_steps and analysis_step_children tables. It
// follows the exact generation-plus-lease CAS discipline of
// internal/adapter/sqlite/knowledge_queue_store.go: ClaimNext claims inside
// a serializable transaction, and every terminal or retry transition is a
// single UPDATE ... WHERE that includes the exact claimed generation and
// lease, returning domain.ErrAnalysisCASConflict when it touches zero rows.
type AnalysisStepStore struct {
	db *sql.DB
}

func NewAnalysisStepStore(store *Store) *AnalysisStepStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &AnalysisStepStore{db: store.db}
}

var _ port.AnalysisStepStore = (*AnalysisStepStore)(nil)
var _ port.AnalysisStepPayloadStore = (*AnalysisStepStore)(nil)

const hardMaxAnalysisStepIDRunes = 128

// validBoundedStepID mirrors domain's unexported validAnalysisBoundedID: a
// non-empty, bounded, valid UTF-8 identifier with no control characters. It
// applies to step, evidence, and bundle identifiers, none of which are the
// fixed 64-hex analysis ID shape.
func validBoundedStepID(value string) bool {
	if strings.TrimSpace(value) == "" || !utf8.ValidString(value) {
		return false
	}
	if utf8.RuneCountInString(value) > hardMaxAnalysisStepIDRunes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Prepare inserts one step in domain.AnalysisStepPrepared state. step.State,
// step.Attempt, step.Generation, and step.LeaseUntil are ignored on input: a
// prepared step always starts at attempt 0, generation 0, and no lease.
// step.CreatedAt is the caller's injected clock and is required; it becomes
// both created_at and updated_at.
func (s *AnalysisStepStore) Prepare(ctx context.Context, step port.AnalysisStep) (port.AnalysisStep, error) {
	if s == nil || s.db == nil {
		return port.AnalysisStep{}, domain.ErrAnalysisUnavailable
	}
	if !domain.ValidAnalysisID(step.AnalysisID) {
		return port.AnalysisStep{}, fmt.Errorf("%w: step requires a valid analysis id", domain.ErrAnalysisValidation)
	}
	if !validBoundedStepID(step.StepID) {
		return port.AnalysisStep{}, fmt.Errorf("%w: step requires a bounded step id", domain.ErrAnalysisValidation)
	}
	if !domain.ValidAnalysisStepKind(step.Kind) {
		return port.AnalysisStep{}, fmt.Errorf("%w: step requires a known kind", domain.ErrAnalysisValidation)
	}
	if step.CreatedAt.IsZero() {
		return port.AnalysisStep{}, fmt.Errorf("%w: step requires an injected clock", domain.ErrAnalysisValidation)
	}
	var segmentOrdinal sql.NullInt64
	switch step.Kind {
	case domain.AnalysisStepLeaf:
		if step.SegmentOrdinal < 0 {
			return port.AnalysisStep{}, fmt.Errorf("%w: leaf step requires a non-negative segment ordinal", domain.ErrAnalysisValidation)
		}
		if len(step.ChildStepIDs) > 0 {
			return port.AnalysisStep{}, fmt.Errorf("%w: leaf step must not declare children", domain.ErrAnalysisValidation)
		}
		segmentOrdinal = sql.NullInt64{Int64: int64(step.SegmentOrdinal), Valid: true}
	case domain.AnalysisStepReduction:
		if len(step.ChildStepIDs) == 0 {
			return port.AnalysisStep{}, fmt.Errorf("%w: reduction step requires at least one child", domain.ErrAnalysisValidation)
		}
		if len(step.ChildStepIDs) > domain.HardMaxAnalysisReductionFanIn {
			return port.AnalysisStep{}, fmt.Errorf("%w: reduction step exceeds the maximum fan-in", domain.ErrAnalysisValidation)
		}
		for _, childID := range step.ChildStepIDs {
			if !validBoundedStepID(childID) {
				return port.AnalysisStep{}, fmt.Errorf("%w: reduction step child id is not bounded", domain.ErrAnalysisValidation)
			}
		}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return port.AnalysisStep{}, wrapAnalysisCanceled(err, fmt.Errorf("%w: begin step prepare: %v", domain.ErrAnalysisUnavailable, err))
	}
	defer func() { _ = tx.Rollback() }()

	nowUnix := step.CreatedAt.UTC().Unix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO analysis_steps (
		analysis_id, step_id, kind, state, attempt, generation, next_attempt, lease_until,
		segment_ordinal, failure_code, output_digest, output_payload, created_at, updated_at)
		VALUES (?, ?, ?, 'prepared', 0, 0, 0, 0, ?, '', '', '', ?, ?)`,
		step.AnalysisID, step.StepID, string(step.Kind), segmentOrdinal, nowUnix, nowUnix); err != nil {
		return port.AnalysisStep{}, wrapAnalysisCanceled(err, fmt.Errorf("%w: insert analysis step: %v", domain.ErrAnalysisUnavailable, err))
	}
	for i, childID := range step.ChildStepIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO analysis_step_children
			(analysis_id, parent_step_id, ordinal, child_step_id) VALUES (?, ?, ?, ?)`,
			step.AnalysisID, step.StepID, i, childID); err != nil {
			return port.AnalysisStep{}, wrapAnalysisCanceled(err, fmt.Errorf("%w: insert analysis step child: %v", domain.ErrAnalysisUnavailable, err))
		}
	}
	prepared, err := scanOneAnalysisStep(ctx, tx, step.AnalysisID, step.StepID)
	if err != nil {
		return port.AnalysisStep{}, err
	}
	if err := tx.Commit(); err != nil {
		return port.AnalysisStep{}, wrapAnalysisCanceled(err, fmt.Errorf("%w: commit step prepare: %v", domain.ErrAnalysisUnavailable, err))
	}
	return prepared, nil
}

// ClaimNext claims the next eligible prepared step: a leaf only needs
// domain.AnalysisStepPrepared state and a due next-attempt time; a
// reduction additionally requires every declared child to already be
// domain.AnalysisStepCompleted. Both conditions are evaluated in the SQL
// itself, not in Go after an unconditional read, so a reduction step can
// never be claimed one child short.
func (s *AnalysisStepStore) ClaimNext(ctx context.Context, analysisID string, now time.Time, lease time.Duration) (port.AnalysisStep, bool, error) {
	if s == nil || s.db == nil {
		return port.AnalysisStep{}, false, domain.ErrAnalysisUnavailable
	}
	if !domain.ValidAnalysisID(analysisID) {
		return port.AnalysisStep{}, false, fmt.Errorf("%w: claim requires a valid analysis id", domain.ErrAnalysisValidation)
	}
	if now.IsZero() {
		return port.AnalysisStep{}, false, fmt.Errorf("%w: claim requires an injected clock", domain.ErrAnalysisValidation)
	}
	if lease <= 0 {
		return port.AnalysisStep{}, false, fmt.Errorf("%w: claim lease must be positive", domain.ErrAnalysisValidation)
	}
	nowUnix := now.UTC().Unix()
	leaseUnix := now.UTC().Add(lease).Unix()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return port.AnalysisStep{}, false, wrapAnalysisCanceled(err, fmt.Errorf("%w: begin step claim: %v", domain.ErrAnalysisUnavailable, err))
	}
	defer func() { _ = tx.Rollback() }()

	// A step is eligible either as a fresh due 'prepared' row or as a
	// 'claimed' row whose lease already expired: an expired claimant must
	// be reclaimable by a new owner, exactly like the knowledge queue's
	// processing-with-expired-lease branch.
	const eligible = `(
		(state = 'prepared' AND (next_attempt = 0 OR next_attempt <= ?))
		OR (state = 'claimed' AND lease_until > 0 AND lease_until <= ?)
	)`
	var stepID string
	err = tx.QueryRowContext(ctx, `
		SELECT step_id FROM analysis_steps
		WHERE analysis_id = ? AND `+eligible+`
			AND (
				kind = 'leaf'
				OR NOT EXISTS (
					SELECT 1 FROM analysis_step_children c
					WHERE c.analysis_id = analysis_steps.analysis_id AND c.parent_step_id = analysis_steps.step_id
						AND NOT EXISTS (
							SELECT 1 FROM analysis_steps child
							WHERE child.analysis_id = c.analysis_id AND child.step_id = c.child_step_id AND child.state = 'completed'
						)
				)
			)
		ORDER BY step_id LIMIT 1`, analysisID, nowUnix, nowUnix).Scan(&stepID)
	if errors.Is(err, sql.ErrNoRows) {
		return port.AnalysisStep{}, false, nil
	}
	if err != nil {
		return port.AnalysisStep{}, false, wrapAnalysisCanceled(err, fmt.Errorf("%w: select step claim: %v", domain.ErrAnalysisUnavailable, err))
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE analysis_steps SET state = 'claimed', lease_until = ?, updated_at = ?
		WHERE analysis_id = ? AND step_id = ? AND `+eligible,
		leaseUnix, nowUnix, analysisID, stepID, nowUnix, nowUnix)
	if err != nil {
		return port.AnalysisStep{}, false, wrapAnalysisCanceled(err, fmt.Errorf("%w: update step claim: %v", domain.ErrAnalysisUnavailable, err))
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return port.AnalysisStep{}, false, fmt.Errorf("%w: step claim row changed", domain.ErrAnalysisUnavailable)
	}
	claimed, err := scanOneAnalysisStep(ctx, tx, analysisID, stepID)
	if err != nil {
		return port.AnalysisStep{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return port.AnalysisStep{}, false, wrapAnalysisCanceled(err, fmt.Errorf("%w: commit step claim: %v", domain.ErrAnalysisUnavailable, err))
	}
	return claimed, true, nil
}

// validateAnalysisStepClaim enforces the structural claim token before any
// terminal or retry transition, mirroring validateClaimToken for
// domain.KnowledgeQueueClaim.
func validateAnalysisStepClaim(claim domain.AnalysisStepClaim) error {
	if err := claim.Validate(); err != nil {
		return err
	}
	if claim.LeaseUntil.IsZero() {
		return fmt.Errorf("%w: step claim requires its lease identity", domain.ErrAnalysisValidation)
	}
	return nil
}

func (s *AnalysisStepStore) transition(ctx context.Context, claim domain.AnalysisStepClaim, set string, args []any) error {
	if s == nil || s.db == nil {
		return domain.ErrAnalysisUnavailable
	}
	if err := validateAnalysisStepClaim(claim); err != nil {
		return err
	}
	updateArgs := append([]any{}, args...)
	updateArgs = append(updateArgs, claim.AnalysisID, claim.StepID, claim.Generation, claim.LeaseUntil.UTC().Unix())
	result, err := s.db.ExecContext(ctx, `
		UPDATE analysis_steps SET `+set+`
		WHERE analysis_id = ? AND step_id = ? AND generation = ? AND state = 'claimed' AND lease_until = ?`,
		updateArgs...)
	if err != nil {
		return wrapAnalysisCanceled(err, fmt.Errorf("%w: step transition: %v", domain.ErrAnalysisUnavailable, err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return wrapAnalysisCanceled(err, fmt.Errorf("%w: inspect step transition: %v", domain.ErrAnalysisUnavailable, err))
	}
	if affected != 1 {
		return fmt.Errorf("%w: step claim is stale", domain.ErrAnalysisCASConflict)
	}
	return nil
}

// Complete commits a step's typed output digest and marks it
// domain.AnalysisStepCompleted. The v40
// analysis_steps_completed_immutable trigger is the second, durable layer
// of protection: even a caller that bypasses this store's CAS check could
// never rewrite a completed step's output.
func (s *AnalysisStepStore) Complete(ctx context.Context, claim domain.AnalysisStepClaim, outputDigest string, now time.Time) (port.AnalysisStep, error) {
	if !validResultOpaqueID(outputDigest) {
		return port.AnalysisStep{}, fmt.Errorf("%w: step completion requires a valid output digest", domain.ErrAnalysisValidation)
	}
	if now.IsZero() {
		return port.AnalysisStep{}, fmt.Errorf("%w: step completion requires an injected clock", domain.ErrAnalysisValidation)
	}
	if err := s.transition(ctx, claim, `state = 'completed', lease_until = 0, output_digest = ?, updated_at = ?`,
		[]any{outputDigest, now.UTC().Unix()}); err != nil {
		return port.AnalysisStep{}, err
	}
	return scanOneAnalysisStep(ctx, s.db, claim.AnalysisID, claim.StepID)
}

// Retry returns a step to domain.AnalysisStepPrepared for a later attempt.
// consumeAttempt false is the non-blocking-limiter-permit-exhaustion case
// the TRD declares is not a failure: the attempt counter is left untouched.
func (s *AnalysisStepStore) Retry(ctx context.Context, claim domain.AnalysisStepClaim, nextAttempt time.Time, consumeAttempt bool) error {
	if nextAttempt.IsZero() {
		return fmt.Errorf("%w: step retry requires a next attempt time", domain.ErrAnalysisValidation)
	}
	attemptDelta := 0
	if consumeAttempt {
		attemptDelta = 1
	}
	return s.transition(ctx, claim,
		`state = 'prepared', lease_until = 0, next_attempt = ?, attempt = attempt + ?, updated_at = ?`,
		[]any{nextAttempt.UTC().Unix(), attemptDelta, time.Now().UTC().Unix()})
}

// Fail marks a step domain.AnalysisStepFailed with a closed failure code,
// ending its retry lifecycle.
func (s *AnalysisStepStore) Fail(ctx context.Context, claim domain.AnalysisStepClaim, code domain.AnalysisFailureCode, now time.Time) error {
	if !domain.ValidAnalysisFailureCode(string(code)) {
		return fmt.Errorf("%w: step failure code is not closed", domain.ErrAnalysisValidation)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: step failure requires an injected clock", domain.ErrAnalysisValidation)
	}
	return s.transition(ctx, claim, `state = 'failed', lease_until = 0, failure_code = ?, updated_at = ?`,
		[]any{string(code), now.UTC().Unix()})
}

// WritePayload persists the bounded structured output payload for a step
// that is still 'claimed' under claim's exact generation and lease
// (checkpoint 4), following the same CAS discipline as every other
// transition in this store. It must be called before Complete: once
// Complete transitions the step to 'completed', the v40
// analysis_steps_completed_immutable trigger blocks any further write to
// this row, output_payload included.
func (s *AnalysisStepStore) WritePayload(ctx context.Context, claim domain.AnalysisStepClaim, payload []byte, now time.Time) error {
	if len(payload) == 0 || len(payload) > 65536 {
		return fmt.Errorf("%w: step payload must be non-empty and bounded to 65536 bytes", domain.ErrAnalysisValidation)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: step payload write requires an injected clock", domain.ErrAnalysisValidation)
	}
	return s.transition(ctx, claim, `output_payload = ?, updated_at = ?`, []any{string(payload), now.UTC().Unix()})
}

// ReadPayload returns a step's stored output payload, completed or not. A
// step with no payload written yet (a leaf whose evidence-only checkpoint 3
// pipeline never called WritePayload, or a step not found at all) returns
// an error wrapping domain.ErrAnalysisUnavailable.
func (s *AnalysisStepStore) ReadPayload(ctx context.Context, analysisID string, stepID string) ([]byte, error) {
	if s == nil || s.db == nil {
		return nil, domain.ErrAnalysisUnavailable
	}
	if !domain.ValidAnalysisID(analysisID) {
		return nil, fmt.Errorf("%w: payload read requires a valid analysis id", domain.ErrAnalysisValidation)
	}
	if !validBoundedStepID(stepID) {
		return nil, fmt.Errorf("%w: payload read requires a bounded step id", domain.ErrAnalysisValidation)
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT output_payload FROM analysis_steps WHERE analysis_id = ? AND step_id = ?`, analysisID, stepID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: analysis step not found", domain.ErrAnalysisUnavailable)
	}
	if err != nil {
		return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: read analysis step payload: %v", domain.ErrAnalysisUnavailable, err))
	}
	if payload == "" {
		return nil, fmt.Errorf("%w: analysis step has no stored payload", domain.ErrAnalysisUnavailable)
	}
	return []byte(payload), nil
}

// List pages one analysis' steps in step-ID order.
func (s *AnalysisStepStore) List(ctx context.Context, analysisID string, afterStepID string, limit int) ([]port.AnalysisStep, error) {
	if s == nil || s.db == nil {
		return nil, domain.ErrAnalysisUnavailable
	}
	if !domain.ValidAnalysisID(analysisID) {
		return nil, fmt.Errorf("%w: list requires a valid analysis id", domain.ErrAnalysisValidation)
	}
	if limit <= 0 || limit > 500 {
		return nil, fmt.Errorf("%w: list limit is not bounded", domain.ErrAnalysisValidation)
	}
	if afterStepID != "" && !validBoundedStepID(afterStepID) {
		return nil, fmt.Errorf("%w: list cursor is not bounded", domain.ErrAnalysisValidation)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+analysisStepColumns+` FROM analysis_steps
		WHERE analysis_id = ? AND step_id > ? ORDER BY step_id LIMIT ?`, analysisID, afterStepID, limit)
	if err != nil {
		return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: list analysis steps: %v", domain.ErrAnalysisUnavailable, err))
	}
	defer rows.Close()
	var steps []port.AnalysisStep
	for rows.Next() {
		step, err := scanAnalysisStepRow(rows)
		if err != nil {
			return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: scan analysis step: %v", domain.ErrAnalysisUnavailable, err))
		}
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: scan analysis steps: %v", domain.ErrAnalysisUnavailable, err))
	}
	for i := range steps {
		if steps[i].Kind != domain.AnalysisStepReduction {
			continue
		}
		children, err := loadAnalysisStepChildren(ctx, s.db, analysisID, steps[i].StepID)
		if err != nil {
			return nil, err
		}
		steps[i].ChildStepIDs = children
	}
	return steps, nil
}

const analysisStepColumns = `analysis_id, step_id, kind, state, attempt, generation, next_attempt, lease_until,
	segment_ordinal, failure_code, output_digest, created_at, updated_at`

type analysisStepScanner interface {
	Scan(dest ...any) error
}

func scanAnalysisStepRow(row analysisStepScanner) (port.AnalysisStep, error) {
	var (
		step                     port.AnalysisStep
		kind, state, failureCode string
		segmentOrdinal           sql.NullInt64
		nextAttempt, leaseUntil  int64
		createdAt, updatedAt     int64
	)
	if err := row.Scan(&step.AnalysisID, &step.StepID, &kind, &state, &step.Attempt, &step.Generation,
		&nextAttempt, &leaseUntil, &segmentOrdinal, &failureCode, &step.OutputDigest, &createdAt, &updatedAt); err != nil {
		return port.AnalysisStep{}, err
	}
	step.Kind = domain.AnalysisStepKind(kind)
	step.State = domain.AnalysisStepState(state)
	step.FailureCode = domain.AnalysisFailureCode(failureCode)
	if segmentOrdinal.Valid {
		step.SegmentOrdinal = int(segmentOrdinal.Int64)
	}
	if leaseUntil > 0 {
		step.LeaseUntil = time.Unix(leaseUntil, 0).UTC()
	}
	step.CreatedAt = time.Unix(createdAt, 0).UTC()
	step.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	_ = nextAttempt
	return step, nil
}

type analysisStepQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func scanOneAnalysisStep(ctx context.Context, q analysisStepQuerier, analysisID, stepID string) (port.AnalysisStep, error) {
	row := q.QueryRowContext(ctx, `SELECT `+analysisStepColumns+` FROM analysis_steps WHERE analysis_id = ? AND step_id = ?`, analysisID, stepID)
	step, err := scanAnalysisStepRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return port.AnalysisStep{}, fmt.Errorf("%w: analysis step not found", domain.ErrAnalysisUnavailable)
	}
	if err != nil {
		return port.AnalysisStep{}, wrapAnalysisCanceled(err, fmt.Errorf("%w: read analysis step: %v", domain.ErrAnalysisUnavailable, err))
	}
	if step.Kind == domain.AnalysisStepReduction {
		children, err := loadAnalysisStepChildren(ctx, q, analysisID, stepID)
		if err != nil {
			return port.AnalysisStep{}, err
		}
		step.ChildStepIDs = children
	}
	return step, nil
}

func loadAnalysisStepChildren(ctx context.Context, q analysisStepQuerier, analysisID, stepID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT child_step_id FROM analysis_step_children
		WHERE analysis_id = ? AND parent_step_id = ? ORDER BY ordinal`, analysisID, stepID)
	if err != nil {
		return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: read analysis step children: %v", domain.ErrAnalysisUnavailable, err))
	}
	defer rows.Close()
	var children []string
	for rows.Next() {
		var childID string
		if err := rows.Scan(&childID); err != nil {
			return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: scan analysis step child: %v", domain.ErrAnalysisUnavailable, err))
		}
		children = append(children, childID)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: scan analysis step children: %v", domain.ErrAnalysisUnavailable, err))
	}
	return children, nil
}
