package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.AgentDraftStore = (*AgentDraftStore)(nil)

// AgentDraftStore implements port.AgentDraftStore backed by SQLite.
type AgentDraftStore struct {
	db *sql.DB
}

func NewAgentDraftStore(store *Store) *AgentDraftStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &AgentDraftStore{db: store.db}
}

func (s *AgentDraftStore) Create(ctx context.Context, draft *port.AgentDraft) error {
	if s == nil || s.db == nil {
		return errors.New("agent draft store is not configured")
	}
	if draft == nil {
		return errors.New("agent draft is required")
	}
	if err := validateDraft(*draft); err != nil {
		return err
	}
	status := draft.Status
	if status == "" {
		status = port.DraftStatusDraft
	}
	if !validDraftStatus(status) {
		return fmt.Errorf("invalid agent draft status %q", status)
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_drafts (
			draft_id, team_id, actor_id, conversation_key, name, description,
			instruction, model, definition_hash, catalog_revision, status,
			created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		draft.DraftID, draft.TeamID, draft.ActorID, draft.ConversationKey, draft.Name,
		draft.Description, draft.Instruction, draft.Model, draft.DefinitionHash,
		draft.CatalogRevision, status, draft.CreatedAt.UTC().UnixNano(), draft.ExpiresAt.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("create agent draft: %w", err)
	}
	return nil
}

func (s *AgentDraftStore) Get(ctx context.Context, draftID string) (*port.AgentDraft, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("agent draft store is not configured")
	}
	var (
		draft                     port.AgentDraft
		status                    string
		createdAtNanos, expiresAt int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT draft_id, team_id, actor_id, conversation_key, name, description,
			instruction, model, definition_hash, catalog_revision, status,
			created_at, expires_at
		FROM agent_drafts WHERE draft_id = ?`, draftID).Scan(
		&draft.DraftID, &draft.TeamID, &draft.ActorID, &draft.ConversationKey,
		&draft.Name, &draft.Description, &draft.Instruction, &draft.Model,
		&draft.DefinitionHash, &draft.CatalogRevision, &status,
		&createdAtNanos, &expiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get agent draft: %w", err)
	}
	draft.Status = port.AgentDraftStatus(status)
	draft.CreatedAt = time.Unix(0, createdAtNanos).UTC()
	draft.ExpiresAt = time.Unix(0, expiresAt).UTC()
	return &draft, nil
}

func (s *AgentDraftStore) FindByNameAndDefinitionHash(ctx context.Context, name, definitionHash string) (*port.AgentDraft, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("agent draft store is not configured")
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("agent draft name is required")
	}
	if strings.TrimSpace(definitionHash) == "" {
		return nil, errors.New("agent draft definition hash is required")
	}

	var (
		draft                     port.AgentDraft
		status                    string
		createdAtNanos, expiresAt int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT draft_id, team_id, actor_id, conversation_key, name, description,
			instruction, model, definition_hash, catalog_revision, status,
			created_at, expires_at
		FROM agent_drafts
		WHERE name = ? AND definition_hash = ?
		ORDER BY created_at DESC
		LIMIT 1`, name, definitionHash).Scan(
		&draft.DraftID, &draft.TeamID, &draft.ActorID, &draft.ConversationKey,
		&draft.Name, &draft.Description, &draft.Instruction, &draft.Model,
		&draft.DefinitionHash, &draft.CatalogRevision, &status,
		&createdAtNanos, &expiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find agent draft: %w", err)
	}
	draft.Status = port.AgentDraftStatus(status)
	draft.CreatedAt = time.Unix(0, createdAtNanos).UTC()
	draft.ExpiresAt = time.Unix(0, expiresAt).UTC()
	return &draft, nil
}

// MarkPreviewed atomically saves the preview result and transitions from Draft to Previewed.
func (s *AgentDraftStore) MarkPreviewed(ctx context.Context, draftID string, definitionHash string, catalogRevision int) error {
	if s == nil || s.db == nil {
		return errors.New("agent draft store is not configured")
	}
	if strings.TrimSpace(draftID) == "" {
		return errors.New("agent draft ID is required")
	}
	if strings.TrimSpace(definitionHash) == "" {
		return errors.New("agent draft definition hash is required")
	}
	if catalogRevision < 0 {
		return errors.New("agent draft catalog revision must not be negative")
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin agent draft preview update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE agent_drafts
		SET definition_hash = ?, catalog_revision = ?, status = ?
		WHERE draft_id = ? AND status = ?`,
		definitionHash, catalogRevision, port.DraftStatusPreviewed, draftID, port.DraftStatusDraft)
	if err != nil {
		return fmt.Errorf("update agent draft preview: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect agent draft preview update: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("agent draft %q not found or status is not %q", draftID, port.DraftStatusDraft)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent draft preview update: %w", err)
	}
	return nil
}

func (s *AgentDraftStore) UpdateStatus(ctx context.Context, draftID string, fromStatus, toStatus port.AgentDraftStatus) error {
	if s == nil || s.db == nil {
		return errors.New("agent draft store is not configured")
	}
	if strings.TrimSpace(draftID) == "" {
		return errors.New("agent draft ID is required")
	}
	if !validDraftStatus(fromStatus) || !validDraftStatus(toStatus) {
		return errors.New("invalid agent draft status transition")
	}
	if !isValidTransition(fromStatus, toStatus) {
		return fmt.Errorf("invalid agent draft status transition from %q to %q", fromStatus, toStatus)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin agent draft status update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE agent_drafts SET status = ?
		WHERE draft_id = ? AND status = ?`, toStatus, draftID, fromStatus)
	if err != nil {
		return fmt.Errorf("update agent draft status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect agent draft status update: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("agent draft %q not found or status is not %q", draftID, fromStatus)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent draft status update: %w", err)
	}
	return nil
}

func (s *AgentDraftStore) ExpireDrafts(ctx context.Context, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("agent draft store is not configured")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_drafts
		SET status = ?
		WHERE status IN (?, ?, ?) AND expires_at <= ?`,
		port.DraftStatusExpired,
		port.DraftStatusDraft, port.DraftStatusPreviewed, port.DraftStatusInstallRequested,
		now.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("expire agent drafts: %w", err)
	}
	return nil
}

func isValidTransition(from, to port.AgentDraftStatus) bool {
	switch from {
	case port.DraftStatusDraft:
		return to == port.DraftStatusPreviewed || to == port.DraftStatusCancelled || to == port.DraftStatusExpired
	case port.DraftStatusPreviewed:
		return to == port.DraftStatusInstallRequested || to == port.DraftStatusCancelled || to == port.DraftStatusExpired
	case port.DraftStatusInstallRequested:
		return to == port.DraftStatusInstalled || to == port.DraftStatusFailed || to == port.DraftStatusCancelled || to == port.DraftStatusExpired
	default:
		return false
	}
}

func validateDraft(draft port.AgentDraft) error {
	for _, required := range []struct {
		field string
		value string
	}{
		{field: "draft ID", value: draft.DraftID},
		{field: "team ID", value: draft.TeamID},
		{field: "actor ID", value: draft.ActorID},
		{field: "conversation key", value: draft.ConversationKey},
		{field: "name", value: draft.Name},
	} {
		if strings.TrimSpace(required.value) == "" {
			return fmt.Errorf("agent draft %s is required", required.field)
		}
	}
	if draft.CreatedAt.IsZero() || draft.ExpiresAt.IsZero() {
		return errors.New("agent draft creation and expiry times are required")
	}
	if !draft.ExpiresAt.After(draft.CreatedAt) {
		return errors.New("agent draft expiry must be after creation time")
	}
	if draft.CatalogRevision < 0 {
		return errors.New("agent draft catalog revision must not be negative")
	}
	return nil
}

func validDraftStatus(status port.AgentDraftStatus) bool {
	switch status {
	case port.DraftStatusDraft,
		port.DraftStatusPreviewed,
		port.DraftStatusInstallRequested,
		port.DraftStatusInstalled,
		port.DraftStatusCancelled,
		port.DraftStatusExpired,
		port.DraftStatusFailed:
		return true
	default:
		return false
	}
}
