package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.ContextEpochStore = (*ContextEpochStore)(nil)

type ContextEpochStore struct {
	db *sql.DB
}

func NewContextEpochStore(store *Store) *ContextEpochStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &ContextEpochStore{db: store.db}
}

func (s *ContextEpochStore) Append(ctx context.Context, epoch domain.ContextEpoch, expectedPreviousEpoch int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: context epoch store is not configured", port.ErrContextEpochUnavailable)
	}
	if err := epoch.Validate(); err != nil {
		return fmt.Errorf("%w: %v", port.ErrContextEpochValidation, err)
	}
	if expectedPreviousEpoch < 0 {
		return fmt.Errorf("%w: expected previous epoch cannot be negative", port.ErrContextEpochValidation)
	}
	knowledgeJSON, err := json.Marshal(epoch.KnowledgeIdentities)
	if err != nil {
		return fmt.Errorf("%w: encode knowledge identities: %v", port.ErrContextEpochUnavailable, err)
	}
	resultJSON, err := json.Marshal(epoch.ResultIdentities)
	if err != nil {
		return fmt.Errorf("%w: encode result identities: %v", port.ErrContextEpochUnavailable, err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("%w: begin epoch append: %v", port.ErrContextEpochUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	var sessionExists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM adk_sessions WHERE app_name = ? AND user_id = ? AND session_id = ?`, epoch.AppName, epoch.UserID, epoch.SessionID).Scan(&sessionExists)
	if errors.Is(err, sql.ErrNoRows) {
		return port.ErrContextEpochSessionMissing
	}
	if err != nil {
		return fmt.Errorf("%w: inspect ADK session: %v", port.ErrContextEpochUnavailable, err)
	}

	var current int64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COALESCE(MAX(epoch_number), 0) FROM context_epochs WHERE app_name = ? AND user_id = ? AND session_id = ?`,
		epoch.AppName,
		epoch.UserID,
		epoch.SessionID,
	).Scan(
		&current,
	); err != nil {
		return fmt.Errorf("%w: inspect epoch head: %v", port.ErrContextEpochUnavailable, err)
	}
	if current != expectedPreviousEpoch {
		return fmt.Errorf("%w: current epoch %d differs from expected %d", port.ErrContextEpochCASConflict, current, expectedPreviousEpoch)
	}
	if epoch.EpochNumber != expectedPreviousEpoch+1 {
		return fmt.Errorf("%w: epoch number %d does not follow %d", port.ErrContextEpochValidation, epoch.EpochNumber, expectedPreviousEpoch)
	}
	var eventHead int64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COALESCE(MAX(ordinal), -1) FROM adk_events WHERE app_name = ? AND user_id = ? AND session_id = ?`,
		epoch.AppName,
		epoch.UserID,
		epoch.SessionID,
	).Scan(
		&eventHead,
	); err != nil {
		return fmt.Errorf("%w: inspect event head: %v", port.ErrContextEpochUnavailable, err)
	}
	if epoch.CoveredThroughOrdinal > eventHead {
		return fmt.Errorf("%w: covered ordinal %d exceeds event head %d", port.ErrContextEpochValidation, epoch.CoveredThroughOrdinal, eventHead)
	}
	if current > 0 {
		var previousCovered int64
		if err := tx.QueryRowContext(
			ctx,
			`SELECT covered_through_ordinal FROM context_epochs WHERE app_name = ? AND user_id = ? AND session_id = ? ORDER BY epoch_number DESC LIMIT 1`,
			epoch.AppName,
			epoch.UserID,
			epoch.SessionID,
		).Scan(
			&previousCovered,
		); err != nil {
			return fmt.Errorf("%w: inspect previous coverage: %v", port.ErrContextEpochUnavailable, err)
		}
		if epoch.CoveredThroughOrdinal < previousCovered {
			return fmt.Errorf("%w: covered ordinal %d precedes previous coverage %d", port.ErrContextEpochValidation, epoch.CoveredThroughOrdinal, previousCovered)
		}
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO context_epochs (
		app_name, user_id, session_id, epoch_id, epoch_number, covered_through_ordinal,
		workstream_revision, summary_identity, knowledge_identities, result_identities,
		compiler_version, counter_version, source_digest, frame_tokens, frame_code_points,
		selected_source_count, omitted_source_count, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		epoch.AppName, epoch.UserID, epoch.SessionID, epoch.EpochID, epoch.EpochNumber,
		epoch.CoveredThroughOrdinal, epoch.WorkstreamRevision, epoch.SummaryIdentity,
		string(knowledgeJSON), string(resultJSON), epoch.CompilerVersion, epoch.CounterVersion,
		epoch.SourceDigest, epoch.FrameTokens, epoch.FrameCodePoints, epoch.SelectedSourceCount,
		epoch.OmittedSourceCount, epoch.Reason, epoch.CreatedAt.UTC().UnixNano())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return fmt.Errorf("%w: insert identity conflicts with existing epoch", port.ErrContextEpochCASConflict)
		}
		return fmt.Errorf("%w: insert epoch: %v", port.ErrContextEpochUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit epoch append: %v", port.ErrContextEpochUnavailable, err)
	}
	return nil
}

func (s *ContextEpochStore) Latest(ctx context.Context, appName, userID, sessionID string) (domain.ContextEpoch, error) {
	if err := validateEpochSessionKey(appName, userID, sessionID); err != nil {
		return domain.ContextEpoch{}, err
	}
	epoch, err := s.load(ctx, `WHERE app_name = ? AND user_id = ? AND session_id = ? ORDER BY epoch_number DESC LIMIT 1`, appName, userID, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ContextEpoch{}, port.ErrContextEpochNotFound
	}
	if err != nil {
		return domain.ContextEpoch{}, fmt.Errorf("%w: load latest epoch: %v", port.ErrContextEpochUnavailable, err)
	}
	return epoch, nil
}

func (s *ContextEpochStore) Range(ctx context.Context, appName, userID, sessionID string, afterEpoch, limit int64) ([]domain.ContextEpoch, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("%w: context epoch store is not configured", port.ErrContextEpochUnavailable)
	}
	if err := validateEpochSessionKey(appName, userID, sessionID); err != nil {
		return nil, err
	}
	if afterEpoch < 0 || limit <= 0 || limit > domain.MaxContextEpochRange {
		return nil, fmt.Errorf("%w: epoch range is outside bounded limits", port.ErrContextEpochValidation)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT app_name, user_id, session_id, epoch_id, epoch_number,
		covered_through_ordinal, workstream_revision, summary_identity, knowledge_identities,
		result_identities, compiler_version, counter_version, source_digest, frame_tokens,
		frame_code_points, selected_source_count, omitted_source_count, reason, created_at
		FROM context_epochs
		WHERE app_name = ? AND user_id = ? AND session_id = ? AND epoch_number > ?
		ORDER BY epoch_number ASC LIMIT ?`, appName, userID, sessionID, afterEpoch, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: query epoch range: %v", port.ErrContextEpochUnavailable, err)
	}
	defer func() { _ = rows.Close() }()

	var epochs []domain.ContextEpoch
	for rows.Next() {
		epoch, err := scanContextEpoch(rows)
		if err != nil {
			return nil, err
		}
		epochs = append(epochs, epoch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate epoch range: %v", port.ErrContextEpochUnavailable, err)
	}
	return epochs, nil
}

type contextEpochScanner interface {
	Scan(dest ...any) error
}

func (s *ContextEpochStore) load(ctx context.Context, suffix string, args ...any) (domain.ContextEpoch, error) {
	if s == nil || s.db == nil {
		return domain.ContextEpoch{}, fmt.Errorf("%w: context epoch store is not configured", port.ErrContextEpochUnavailable)
	}
	query := `SELECT app_name, user_id, session_id, epoch_id, epoch_number,
		covered_through_ordinal, workstream_revision, summary_identity, knowledge_identities,
		result_identities, compiler_version, counter_version, source_digest, frame_tokens,
		frame_code_points, selected_source_count, omitted_source_count, reason, created_at
		FROM context_epochs ` + suffix
	return scanContextEpoch(s.db.QueryRowContext(ctx, query, args...))
}

func scanContextEpoch(scanner contextEpochScanner) (domain.ContextEpoch, error) {
	var epoch domain.ContextEpoch
	var knowledgeJSON, resultJSON string
	var createdAt int64
	if err := scanner.Scan(
		&epoch.AppName, &epoch.UserID, &epoch.SessionID, &epoch.EpochID, &epoch.EpochNumber,
		&epoch.CoveredThroughOrdinal, &epoch.WorkstreamRevision, &epoch.SummaryIdentity,
		&knowledgeJSON, &resultJSON, &epoch.CompilerVersion, &epoch.CounterVersion,
		&epoch.SourceDigest, &epoch.FrameTokens, &epoch.FrameCodePoints,
		&epoch.SelectedSourceCount, &epoch.OmittedSourceCount, &epoch.Reason, &createdAt,
	); err != nil {
		return domain.ContextEpoch{}, err
	}
	if err := json.Unmarshal([]byte(knowledgeJSON), &epoch.KnowledgeIdentities); err != nil {
		return domain.ContextEpoch{}, fmt.Errorf("%w: decode knowledge identities: %v", port.ErrContextEpochValidation, err)
	}
	if err := json.Unmarshal([]byte(resultJSON), &epoch.ResultIdentities); err != nil {
		return domain.ContextEpoch{}, fmt.Errorf("%w: decode result identities: %v", port.ErrContextEpochValidation, err)
	}
	epoch.CreatedAt = time.Unix(0, createdAt).UTC()
	if err := epoch.Validate(); err != nil {
		return domain.ContextEpoch{}, fmt.Errorf("%w: stored epoch: %v", port.ErrContextEpochValidation, err)
	}
	return epoch, nil
}

func validateEpochSessionKey(appName, userID, sessionID string) error {
	if strings.TrimSpace(appName) == "" || strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("%w: app name, user ID, and session ID are required", port.ErrContextEpochValidation)
	}
	return nil
}
