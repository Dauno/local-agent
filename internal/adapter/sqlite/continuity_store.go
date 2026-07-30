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

var ErrContinuityCAS = errors.New("continuity capsule revision conflict")

type ContinuityStore struct{ store *Store }

func NewContinuityStore(store *Store) *ContinuityStore { return &ContinuityStore{store: store} }

func (s *ContinuityStore) Latest(ctx context.Context, sessionID string) (domain.ContinuityCapsule, error) {
	if s == nil || s.store == nil || strings.TrimSpace(sessionID) == "" {
		return domain.ContinuityCapsule{}, errors.New("continuity store and session ID are required")
	}
	var raw string
	err := s.store.db.QueryRowContext(ctx, `SELECT capsule_json FROM continuity_capsules WHERE session_id = ?`, sessionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ContinuityCapsule{}, nil
	}
	if err != nil {
		return domain.ContinuityCapsule{}, fmt.Errorf("load continuity capsule: %w", err)
	}
	var capsule domain.ContinuityCapsule
	if err := json.Unmarshal([]byte(raw), &capsule); err != nil {
		return domain.ContinuityCapsule{}, errors.New("load continuity capsule: invalid stored data")
	}
	if err := validateContinuityCapsule(capsule); err != nil {
		return domain.ContinuityCapsule{}, fmt.Errorf("load continuity capsule: %w", err)
	}
	return capsule, nil
}

func (s *ContinuityStore) Commit(ctx context.Context, sessionID string, capsule domain.ContinuityCapsule, expectedRevision int64) error {
	if s == nil || s.store == nil || strings.TrimSpace(sessionID) == "" {
		return errors.New("continuity store and session ID are required")
	}
	if expectedRevision < 0 || capsule.Revision != expectedRevision+1 {
		return ErrContinuityCAS
	}
	if err := validateContinuityCapsule(capsule); err != nil {
		return err
	}
	raw, err := json.Marshal(capsule)
	if err != nil {
		return fmt.Errorf("encode continuity capsule: %w", err)
	}
	now := time.Now().UTC().Unix()
	var result sql.Result
	if expectedRevision == 0 {
		result, err = s.store.db.ExecContext(ctx, `INSERT INTO continuity_capsules
			(session_id, revision, capsule_json, source_digest, covered_through, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(session_id) DO NOTHING`,
			sessionID, capsule.Revision, string(raw), capsule.SourceDigest, capsule.CoveredThrough, now, now)
	} else {
		result, err = s.store.db.ExecContext(ctx, `UPDATE continuity_capsules
			SET revision = ?, capsule_json = ?, source_digest = ?, covered_through = ?, updated_at = ?
			WHERE session_id = ? AND revision = ?`, capsule.Revision, string(raw), capsule.SourceDigest,
			capsule.CoveredThrough, now, sessionID, expectedRevision)
	}
	if err != nil {
		return fmt.Errorf("commit continuity capsule: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("commit continuity capsule: %w", err)
	}
	if affected != 1 {
		return ErrContinuityCAS
	}
	return nil
}

func validateContinuityCapsule(capsule domain.ContinuityCapsule) error {
	items := make([]domain.ContinuityItem, 0)
	if capsule.Objective != nil {
		items = append(items, *capsule.Objective)
	}
	items = append(items, capsule.Constraints...)
	items = append(items, capsule.Decisions...)
	items = append(items, capsule.Completed...)
	items = append(items, capsule.Pending...)
	items = append(items, capsule.OpenQuestions...)
	items = append(items, capsule.Superseded...)
	for _, item := range items {
		if _, ok := domain.SanitizeContinuityItem(item); !ok {
			return errors.New("continuity capsule contains unsafe item")
		}
		if item.SourceEventOrdinal < 0 || item.SourceSessionRevision < 0 || strings.TrimSpace(item.SourceDigest) == "" {
			return errors.New("continuity capsule item provenance is invalid")
		}
	}
	return nil
}

func (s *ContinuityStore) IsRecoverableResultReferenced(ctx context.Context, ref string) (bool, error) {
	if s == nil || s.store == nil || strings.TrimSpace(ref) == "" {
		return false, errors.New("continuity reference check requires a store and reference")
	}
	rows, err := s.store.db.QueryContext(ctx, `SELECT capsule_json FROM continuity_capsules`)
	if err != nil {
		return false, fmt.Errorf("check continuity references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var capsule domain.ContinuityCapsule
		if err := json.Unmarshal([]byte(raw), &capsule); err != nil {
			return false, errors.New("check continuity references: invalid stored data")
		}
		for _, active := range capsule.ActiveResults {
			if active.ResultRef == ref {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

var _ port.ContinuityStore = (*ContinuityStore)(nil)
var _ port.RecoverableResultReferenceChecker = (*ContinuityStore)(nil)
