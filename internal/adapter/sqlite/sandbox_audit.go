package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// SandboxAuditStore implements the sandbox use case audit interface using the
// tool_execution_audit table created in migration v10.
type SandboxAuditStore struct {
	db *sql.DB
}

func NewSandboxAuditStore(store *Store) *SandboxAuditStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &SandboxAuditStore{db: store.db}
}

func (s *SandboxAuditStore) InsertAudit(ctx context.Context, record domain.ToolAuditRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tool_execution_audit
		 (original_call_id, capability, actor, authorization_result,
		  idempotency_key, lifecycle_state, created_at, completed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		record.OriginalCallID,
		string(record.Capability),
		record.Actor,
		record.AuthorizationResult,
		record.IdempotencyKey,
		string(record.LifecycleState),
		record.CreatedAt.Unix(),
		record.CompletedAt.Unix(),
	)
	return err
}

func (s *SandboxAuditStore) UpdateAuditState(ctx context.Context, callID string, state domain.ToolLifecycleState, completedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tool_execution_audit
		 SET lifecycle_state = ?, completed_at = ?
		 WHERE original_call_id = ?`,
		string(state), completedAt.Unix(), callID,
	)
	return err
}

func (s *SandboxAuditStore) GetAuditByCallID(ctx context.Context, callID string) (*domain.ToolAuditRecord, error) {
	var (
		capability, authResult, idempotencyKey, lifecycleState string
		actor                                                    string
		createdAt, completedAt                                   int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT capability, actor, authorization_result, idempotency_key,
		 lifecycle_state, created_at, completed_at
		 FROM tool_execution_audit
		 WHERE original_call_id = ?`,
		callID,
	).Scan(&capability, &actor, &authResult, &idempotencyKey, &lifecycleState, &createdAt, &completedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get audit record: %w", err)
	}
	return &domain.ToolAuditRecord{
		OriginalCallID:      callID,
		Capability:          domain.Capability(capability),
		Actor:               actor,
		AuthorizationResult:  authResult,
		IdempotencyKey:      idempotencyKey,
		LifecycleState:      domain.ToolLifecycleState(lifecycleState),
		CreatedAt:           time.Unix(createdAt, 0),
		CompletedAt:         time.Unix(completedAt, 0),
	}, nil
}
