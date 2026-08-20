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

var ErrContinuityCAS = port.ErrContinuityCASConflict

type ContinuityStore struct{ store *Store }

func NewContinuityStore(store *Store) *ContinuityStore { return &ContinuityStore{store: store} }

func (s *ContinuityStore) Latest(ctx context.Context, sessionID string) (domain.ContinuityCapsule, error) {
	if s == nil || s.store == nil || strings.TrimSpace(sessionID) == "" {
		return domain.ContinuityCapsule{}, fmt.Errorf("%w: continuity store and session ID are required", port.ErrContinuityUnavailable)
	}
	var raw string
	err := s.store.db.QueryRowContext(ctx, `SELECT capsule_json FROM continuity_capsules WHERE session_id = ?`, sessionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ContinuityCapsule{}, nil
	}
	if err != nil {
		return domain.ContinuityCapsule{}, fmt.Errorf("%w: load continuity capsule", port.ErrContinuityUnavailable)
	}
	var capsule domain.ContinuityCapsule
	if err := json.Unmarshal([]byte(raw), &capsule); err != nil {
		return domain.ContinuityCapsule{}, fmt.Errorf("%w: invalid stored data", port.ErrContinuityValidation)
	}
	if err := validateContinuityCapsule(capsule); err != nil {
		return domain.ContinuityCapsule{}, fmt.Errorf("load continuity capsule: %w", err)
	}
	return capsule, nil
}

func (s *ContinuityStore) Commit(ctx context.Context, sessionID string, capsule domain.ContinuityCapsule, expectedRevision int64) error {
	if s == nil || s.store == nil || strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("%w: continuity store and session ID are required", port.ErrContinuityUnavailable)
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

	// The CAS write and the ref index rewrite must commit together: a
	// capsule revision that drops a ref has to drop that ref's index row in
	// the same transaction, or a crash between the two either protects a
	// result forever (index row survives, safe but leaked) or exposes one
	// (index row missing while the capsule still names it, unsafe). Both
	// statements below run on the same tx, so a failure at either point
	// rolls back both.
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: begin continuity commit", port.ErrContinuityUnavailable)
	}
	defer func() { _ = tx.Rollback() }()

	var result sql.Result
	if expectedRevision == 0 {
		result, err = tx.ExecContext(ctx, `INSERT INTO continuity_capsules
			(session_id, revision, capsule_json, source_digest, covered_through, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(session_id) DO NOTHING`,
			sessionID, capsule.Revision, string(raw), capsule.SourceDigest, capsule.CoveredThrough, now, now)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE continuity_capsules
			SET revision = ?, capsule_json = ?, source_digest = ?, covered_through = ?, updated_at = ?
			WHERE session_id = ? AND revision = ?`, capsule.Revision, string(raw), capsule.SourceDigest,
			capsule.CoveredThrough, now, sessionID, expectedRevision)
	}
	if err != nil {
		return fmt.Errorf("%w: commit continuity capsule", port.ErrContinuityUnavailable)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%w: commit continuity capsule", port.ErrContinuityUnavailable)
	}
	if affected != 1 {
		return ErrContinuityCAS
	}

	// A capsule is rewritten in place, one row per session_id: replace,
	// not append, so a ref this revision no longer names loses its index
	// row instead of staying protected forever.
	if err := replaceRecoverableResultRefs(ctx, tx, recoverableRefOwnerKindCapsule, sessionID, string(raw), now); err != nil {
		return fmt.Errorf("%w: index continuity capsule refs", port.ErrContinuityUnavailable)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit continuity capsule", port.ErrContinuityUnavailable)
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
			return fmt.Errorf("%w: continuity capsule contains unsafe item", port.ErrContinuityValidation)
		}
		if item.SourceEventOrdinal < 0 || item.SourceSessionRevision < 0 || strings.TrimSpace(item.SourceDigest) == "" {
			return fmt.Errorf("%w: continuity capsule item provenance is invalid", port.ErrContinuityValidation)
		}
	}
	return nil
}

var _ port.ContinuityStore = (*ContinuityStore)(nil)
