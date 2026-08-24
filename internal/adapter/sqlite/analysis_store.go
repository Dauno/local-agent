package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// AnalysisStore is the SQLite implementation of port.AnalysisStore, fixed
// over the v40 result_analyses table. Every read filters by the caller's
// exact domain.ResultScope in the query itself, so a cross-scope row is
// indistinguishable from a missing one at the SQL level, not just in Go.
type AnalysisStore struct {
	db *sql.DB
}

func NewAnalysisStore(store *Store) *AnalysisStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &AnalysisStore{db: store.db}
}

var _ port.AnalysisStore = (*AnalysisStore)(nil)
var _ port.AnalysesByWorkstream = (*AnalysisStore)(nil)

const analysisRecordColumns = `analysis_id, source_result_id, source_sha256, objective_class, objective_digest,
	objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
	actor, team_id, conversation_key, project, workstream_id, state, failure_code, created_at, updated_at`

// Create returns the existing analysis record when one with the same
// semantic identity already exists, or inserts a fresh one otherwise. The
// unique index result_analyses_by_semantic_identity, not a prior
// unsynchronized read, is what actually guarantees idempotency: two
// concurrent callers racing an INSERT ... ON CONFLICT DO NOTHING for the
// same identity always converge on the same row, whichever wins.
//
// source_bytes is a v40 column with no corresponding AnalysisIdentity field
// or Create parameter. It is populated from the existing result_records row
// for identity.SourceResultID: this is a plain query, not a SQL foreign
// key, so it does not violate the v40 rule that no existing truth table
// gains a new foreign key to these tables.
//
// limits_json stores the deterministic JSON encoding of limits itself, not
// only its digest (FIND-103): encoding/json.Marshal on domain.AnalysisLimits
// is deterministic because the struct has no map field and no custom
// MarshalJSON, so it always serializes its fields in declaration order.
// identity.LimitsDigest must equal limits.Digest(); Create rejects a
// mismatched pair rather than trusting the digest string blindly.
func (s *AnalysisStore) Create(ctx context.Context, identity domain.AnalysisIdentity, limits domain.AnalysisLimits, objectiveText string, scope domain.ResultScope, workstreamID string, now time.Time) (port.AnalysisRecord, error) {
	if s == nil || s.db == nil {
		return port.AnalysisRecord{}, domain.ErrAnalysisUnavailable
	}
	if err := identity.Validate(); err != nil {
		return port.AnalysisRecord{}, err
	}
	if err := limits.Validate(); err != nil {
		return port.AnalysisRecord{}, err
	}
	if limits.Digest() != identity.LimitsDigest {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis limits do not match the identity's limits digest", domain.ErrAnalysisValidation)
	}
	if err := scope.Validate(); err != nil {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis create requires a valid scope", domain.ErrAnalysisValidation)
	}
	if strings.TrimSpace(workstreamID) == "" {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis create requires a workstream id", domain.ErrAnalysisValidation)
	}
	if strings.TrimSpace(objectiveText) == "" || utf8.RuneCountInString(objectiveText) > domain.HardMaxAnalysisObjectiveRunes {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis create requires a bounded objective text", domain.ErrAnalysisValidation)
	}
	if now.IsZero() {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis create requires an injected clock", domain.ErrAnalysisValidation)
	}
	limitsJSON, err := json.Marshal(limits)
	if err != nil {
		return port.AnalysisRecord{}, fmt.Errorf("%w: encode analysis limits snapshot: %v", domain.ErrAnalysisValidation, err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return port.AnalysisRecord{}, wrapAnalysisCanceled(err, fmt.Errorf("%w: begin analysis create: %v", domain.ErrAnalysisUnavailable, err))
	}
	defer func() { _ = tx.Rollback() }()

	var sourceBytes int64
	err = tx.QueryRowContext(ctx, `SELECT bytes FROM result_records WHERE result_id = ?`, identity.SourceResultID).Scan(&sourceBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis source result not found", domain.ErrAnalysisUnavailable)
	}
	if err != nil {
		return port.AnalysisRecord{}, wrapAnalysisCanceled(err, fmt.Errorf("%w: read analysis source bytes: %v", domain.ErrAnalysisUnavailable, err))
	}

	analysisID, err := newAnalysisID()
	if err != nil {
		return port.AnalysisRecord{}, fmt.Errorf("%w: generate analysis id: %v", domain.ErrAnalysisUnavailable, err)
	}
	nowUnix := now.UTC().Unix()

	if _, err := tx.ExecContext(ctx, `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'preparing', ?, ?)
		ON CONFLICT(source_result_id, source_sha256, objective_class, objective_digest,
			segmentation_version, prompt_version, model_fingerprint, limits_digest) DO NOTHING`,
		analysisID, identity.SourceResultID, identity.SourceSHA256, sourceBytes, string(identity.ObjectiveClass), identity.ObjectiveDigest,
		objectiveText, identity.SegmentationVersion, identity.PromptVersion, identity.ModelFingerprint, identity.LimitsDigest, string(limitsJSON),
		scope.Actor, scope.TeamID, scope.ConversationKey, scope.Project, workstreamID, nowUnix, nowUnix); err != nil {
		return port.AnalysisRecord{}, wrapAnalysisCanceled(err, fmt.Errorf("%w: insert analysis: %v", domain.ErrAnalysisUnavailable, err))
	}

	record, err := scanOneAnalysisRecord(ctx, tx, `WHERE source_result_id = ? AND source_sha256 = ? AND objective_class = ? AND objective_digest = ?
		AND segmentation_version = ? AND prompt_version = ? AND model_fingerprint = ? AND limits_digest = ?`,
		identity.SourceResultID, identity.SourceSHA256, string(identity.ObjectiveClass), identity.ObjectiveDigest,
		identity.SegmentationVersion, identity.PromptVersion, identity.ModelFingerprint, identity.LimitsDigest)
	if err != nil {
		return port.AnalysisRecord{}, err
	}
	if record.Scope != scope {
		// Same semantic identity, different caller scope: never surface
		// another scope's row, even the one this call just tried to
		// create. Indistinguishable from a missing row, like every other
		// cross-scope read.
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis identity belongs to a different scope", domain.ErrAnalysisUnavailable)
	}
	if err := tx.Commit(); err != nil {
		return port.AnalysisRecord{}, wrapAnalysisCanceled(err, fmt.Errorf("%w: commit analysis create: %v", domain.ErrAnalysisUnavailable, err))
	}
	return record, nil
}

// Get reads one analysis record, scoped by the query itself.
func (s *AnalysisStore) Get(ctx context.Context, analysisID string, scope domain.ResultScope) (port.AnalysisRecord, error) {
	if s == nil || s.db == nil {
		return port.AnalysisRecord{}, domain.ErrAnalysisUnavailable
	}
	if !domain.ValidAnalysisID(analysisID) {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis id is not well formed", domain.ErrAnalysisValidation)
	}
	if err := scope.Validate(); err != nil {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis get requires a valid scope", domain.ErrAnalysisValidation)
	}
	return scanOneAnalysisRecord(ctx, s.db, `WHERE analysis_id = ? AND actor = ? AND team_id = ? AND conversation_key = ? AND project = ?`,
		analysisID, scope.Actor, scope.TeamID, scope.ConversationKey, scope.Project)
}

// Complete marks the analysis domain.AnalysisCompleted. A second terminal
// transition, or a row that is not visible under scope, is rejected.
func (s *AnalysisStore) Complete(ctx context.Context, analysisID string, scope domain.ResultScope, now time.Time) (port.AnalysisRecord, error) {
	return s.transitionTerminal(ctx, analysisID, scope, string(domain.AnalysisCompleted), now)
}

// Fail marks the analysis domain.AnalysisFailed with a closed failure code.
// A second terminal transition, or a row that is not visible under scope,
// is rejected.
func (s *AnalysisStore) Fail(ctx context.Context, analysisID string, scope domain.ResultScope, code domain.AnalysisFailureCode, now time.Time) (port.AnalysisRecord, error) {
	if !domain.ValidAnalysisFailureCode(string(code)) {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis failure code is not closed", domain.ErrAnalysisValidation)
	}
	return s.transitionTerminal(ctx, analysisID, scope, string(domain.AnalysisFailed), now, string(code))
}

func (s *AnalysisStore) transitionTerminal(ctx context.Context, analysisID string, scope domain.ResultScope, state string, now time.Time, failureCode ...string) (port.AnalysisRecord, error) {
	if s == nil || s.db == nil {
		return port.AnalysisRecord{}, domain.ErrAnalysisUnavailable
	}
	if !domain.ValidAnalysisID(analysisID) {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis id is not well formed", domain.ErrAnalysisValidation)
	}
	if err := scope.Validate(); err != nil {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis transition requires a valid scope", domain.ErrAnalysisValidation)
	}
	if now.IsZero() {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis transition requires an injected clock", domain.ErrAnalysisValidation)
	}
	code := ""
	if len(failureCode) > 0 {
		code = failureCode[0]
	}
	result, err := s.db.ExecContext(ctx, `UPDATE result_analyses SET state = ?, failure_code = ?, updated_at = ?
		WHERE analysis_id = ? AND actor = ? AND team_id = ? AND conversation_key = ? AND project = ?
			AND state IN ('preparing', 'running')`,
		state, code, now.UTC().Unix(), analysisID, scope.Actor, scope.TeamID, scope.ConversationKey, scope.Project)
	if err != nil {
		return port.AnalysisRecord{}, wrapAnalysisCanceled(err, fmt.Errorf("%w: transition analysis: %v", domain.ErrAnalysisUnavailable, err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return port.AnalysisRecord{}, wrapAnalysisCanceled(err, fmt.Errorf("%w: inspect analysis transition: %v", domain.ErrAnalysisUnavailable, err))
	}
	if affected != 1 {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis is not visible or already terminal", domain.ErrAnalysisUnavailable)
	}
	return s.Get(ctx, analysisID, scope)
}

// ListByWorkstream implements port.AnalysesByWorkstream: every analysis row
// bound to one workstream, scoped to the trusted actor, conversation, and
// project. It is a separate method from the four port.AnalysisStore methods
// above (Create, Get, Complete, Fail), which stay exactly as checkpoint 1
// declared them; this is additive, backed by the v40
// result_analyses_by_workstream index.
//
// FIND-106: the filter also requires the stored team_id to match the team
// encoded in conversationKey. By convention the canonical conversation key
// already encodes the team ("slack:{team}:..."), but that convention is not
// an enforced invariant anywhere else in the schema, and every other scope
// check in this codebase (domain.ResultScope.Matches) compares team_id
// exactly rather than trusting the key. This predicate closes that gap at
// no cost to callers: teamFromConversationKey returns "" for a key with no
// parseable team, which matches no row.
func (s *AnalysisStore) ListByWorkstream(ctx context.Context, workstreamID, actor string, conversationKey domain.ConversationKey, project string) ([]port.AnalysisRecord, error) {
	if s == nil || s.db == nil {
		return nil, domain.ErrAnalysisUnavailable
	}
	if strings.TrimSpace(workstreamID) == "" || strings.TrimSpace(actor) == "" || strings.TrimSpace(string(conversationKey)) == "" || strings.TrimSpace(project) == "" {
		return nil, fmt.Errorf("%w: list by workstream requires workstream id, actor, conversation, and project", domain.ErrAnalysisValidation)
	}
	team := teamFromConversationKey(conversationKey)
	rows, err := s.db.QueryContext(ctx, `SELECT `+analysisRecordColumns+` FROM result_analyses
		WHERE workstream_id = ? AND actor = ? AND conversation_key = ? AND project = ? AND team_id = ?
		ORDER BY created_at DESC`,
		workstreamID, actor, string(conversationKey), project, team)
	if err != nil {
		return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: list analyses by workstream: %v", domain.ErrAnalysisUnavailable, err))
	}
	defer rows.Close()

	var records []port.AnalysisRecord
	for rows.Next() {
		record, err := scanAnalysisRecordRow(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: iterate analyses by workstream: %v", domain.ErrAnalysisUnavailable, err))
	}
	return records, nil
}

// ListActive implements port.AnalysisActiveLister (checkpoint 6): every
// analysis row in 'preparing' or 'running' state system-wide, in stable
// analysis-id order, paginated so the durable worker can discover work
// across every workstream without a per-workstream scope.
func (s *AnalysisStore) ListActive(ctx context.Context, afterAnalysisID string, limit int) ([]port.AnalysisRecord, error) {
	if s == nil || s.db == nil {
		return nil, domain.ErrAnalysisUnavailable
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+analysisRecordColumns+` FROM result_analyses
		WHERE state IN ('preparing', 'running') AND analysis_id > ?
		ORDER BY analysis_id ASC LIMIT ?`,
		afterAnalysisID, limit)
	if err != nil {
		return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: list active analyses: %v", domain.ErrAnalysisUnavailable, err))
	}
	defer rows.Close()

	var records []port.AnalysisRecord
	for rows.Next() {
		record, err := scanAnalysisRecordRow(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapAnalysisCanceled(err, fmt.Errorf("%w: iterate active analyses: %v", domain.ErrAnalysisUnavailable, err))
	}
	return records, nil
}

// MarkRunning implements port.AnalysisRunningMarker (checkpoint 6):
// transitions an analysis from 'preparing' to 'running'. It is idempotent:
// a row already 'running' is left untouched and its current record is
// still returned, never an error, so a worker that crashed between marking
// running and its next tick never fails on resume.
func (s *AnalysisStore) MarkRunning(ctx context.Context, analysisID string, scope domain.ResultScope, now time.Time) (port.AnalysisRecord, error) {
	if s == nil || s.db == nil {
		return port.AnalysisRecord{}, domain.ErrAnalysisUnavailable
	}
	if !domain.ValidAnalysisID(analysisID) {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis id is not well formed", domain.ErrAnalysisValidation)
	}
	if err := scope.Validate(); err != nil {
		return port.AnalysisRecord{}, fmt.Errorf("%w: mark running requires a valid scope", domain.ErrAnalysisValidation)
	}
	if now.IsZero() {
		return port.AnalysisRecord{}, fmt.Errorf("%w: mark running requires an injected clock", domain.ErrAnalysisValidation)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE result_analyses SET state = 'running', updated_at = ?
		WHERE analysis_id = ? AND actor = ? AND team_id = ? AND conversation_key = ? AND project = ? AND state = 'preparing'`,
		now.UTC().Unix(), analysisID, scope.Actor, scope.TeamID, scope.ConversationKey, scope.Project); err != nil {
		return port.AnalysisRecord{}, wrapAnalysisCanceled(err, fmt.Errorf("%w: mark analysis running: %v", domain.ErrAnalysisUnavailable, err))
	}
	return s.Get(ctx, analysisID, scope)
}

type analysisRowScanner interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// analysisScannable is satisfied by both *sql.Row and *sql.Rows, so
// scanAnalysisRecordRow is the single field-order definition for decoding
// one result_analyses row, shared by a one-row lookup and a multi-row list.
type analysisScannable interface {
	Scan(dest ...any) error
}

func scanAnalysisRecordRow(row analysisScannable) (port.AnalysisRecord, error) {
	var (
		record               port.AnalysisRecord
		objectiveClass       string
		state, failureCode   string
		limitsJSON           string
		createdAt, updatedAt int64
	)
	err := row.Scan(&record.AnalysisID, &record.Identity.SourceResultID, &record.Identity.SourceSHA256, &objectiveClass, &record.Identity.ObjectiveDigest,
		&record.ObjectiveText, &record.Identity.SegmentationVersion, &record.Identity.PromptVersion, &record.Identity.ModelFingerprint, &record.Identity.LimitsDigest, &limitsJSON,
		&record.Scope.Actor, &record.Scope.TeamID, &record.Scope.ConversationKey, &record.Scope.Project, &record.WorkstreamID, &state, &failureCode, &createdAt, &updatedAt)
	if err != nil {
		return port.AnalysisRecord{}, err
	}
	record.Identity.ObjectiveClass = domain.AnalysisObjectiveClass(objectiveClass)
	record.State = domain.AnalysisState(state)
	record.FailureCode = domain.AnalysisFailureCode(failureCode)
	record.CreatedAt = time.Unix(createdAt, 0).UTC()
	record.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	// FIND-103: limits_json is the deterministic JSON encoding of the full
	// domain.AnalysisLimits snapshot, not only its digest. A row written
	// before this decode existed (there are none in production: v40 is
	// unreleased) or a row whose limits_json is unexpectedly malformed
	// decodes to the zero value rather than failing the whole read; a
	// worker that finds a zero-value Limits treats the row as unsafe to
	// drive (see Worker.processAnalysis) instead of running it under a
	// borrowed or unbounded configuration.
	_ = json.Unmarshal([]byte(limitsJSON), &record.Limits)
	return record, nil
}

func scanOneAnalysisRecord(ctx context.Context, q analysisRowScanner, whereClause string, args ...any) (port.AnalysisRecord, error) {
	row := q.QueryRowContext(ctx, `SELECT `+analysisRecordColumns+` FROM result_analyses `+whereClause, args...)
	record, err := scanAnalysisRecordRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return port.AnalysisRecord{}, fmt.Errorf("%w: analysis not found", domain.ErrAnalysisUnavailable)
	}
	if err != nil {
		return port.AnalysisRecord{}, wrapAnalysisCanceled(err, fmt.Errorf("%w: read analysis: %v", domain.ErrAnalysisUnavailable, err))
	}
	return record, nil
}

// wrapAnalysisCanceled makes sure a context cancellation or deadline is
// always reported through errors.Is(err, context.Canceled) /
// context.DeadlineExceeded, never flattened into the generic unavailable
// sentinel. fallback is the error to return when cause is not a context
// error; on a context error, fallback (which already wraps the applicable
// domain.ErrAnalysis* sentinel) is preserved alongside the context sentinel
// with errors.Join, so a caller checking either errors.Is(err,
// context.Canceled) or errors.Is(err, domain.ErrAnalysisUnavailable) still
// sees true. A caller that only wants to notice cancellation and treat
// everything else generically loses nothing; a caller that specifically
// branches on the domain sentinel is not silently broken by a cancellation
// racing its call, which is the failure mode FIND-094 exists to prevent.
func wrapAnalysisCanceled(cause error, fallback error) error {
	if errors.Is(cause, context.Canceled) {
		return errors.Join(context.Canceled, fallback)
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return errors.Join(context.DeadlineExceeded, fallback)
	}
	return fallback
}

func newAnalysisID() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// teamFromConversationKey extracts the team segment from a canonical
// "slack:{team}:..." conversation key. It returns "" for any key that does
// not match that shape, which matches no stored team_id.
func teamFromConversationKey(key domain.ConversationKey) string {
	parts := strings.SplitN(string(key), ":", 3)
	if len(parts) < 2 || parts[0] != "slack" || strings.TrimSpace(parts[1]) == "" {
		return ""
	}
	return parts[1]
}
