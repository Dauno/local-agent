package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.ConversationStore = (*Store)(nil)

func (s *Store) ClaimDedupe(
	ctx context.Context,
	keys []string,
	createdAt time.Time,
	expiresAt time.Time,
) (bool, error) {
	uniqueKeys, err := validateDedupeClaim(keys, createdAt, expiresAt)
	if err != nil {
		return false, err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, fmt.Errorf("begin dedupe claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	nowNanos := createdAt.UnixNano()
	for _, key := range uniqueKeys {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM dedupe_records WHERE dedupe_key = ? AND expires_at <= ?`,
			key, nowNanos,
		); err != nil {
			return false, fmt.Errorf("remove expired dedupe key: %w", err)
		}
	}

	for _, key := range uniqueKeys {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO dedupe_records (dedupe_key, created_at, expires_at)
			VALUES (?, ?, ?)
			ON CONFLICT (dedupe_key) DO NOTHING`,
			key, nowNanos, expiresAt.UnixNano(),
		)
		if err != nil {
			return false, fmt.Errorf("claim dedupe key: %w", err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("inspect dedupe claim: %w", err)
		}
		if inserted == 0 {
			return false, nil
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit dedupe claim: %w", err)
	}
	return true, nil
}

func validateDedupeClaim(keys []string, createdAt, expiresAt time.Time) ([]string, error) {
	if len(keys) == 0 {
		return nil, errors.New("at least one dedupe key is required")
	}
	if !expiresAt.After(createdAt) {
		return nil, errors.New("dedupe expiry must be after creation time")
	}
	unique := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if strings.TrimSpace(key) == "" {
			return nil, errors.New("dedupe keys must not be empty")
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
	}
	return unique, nil
}

func (s *Store) CleanupDedupe(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM dedupe_records WHERE expires_at <= ?`, now.UnixNano(),
	); err != nil {
		return fmt.Errorf("clean expired dedupe records: %w", err)
	}
	return nil
}

func (s *Store) HasAssistantMessage(ctx context.Context, key domain.ConversationKey) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages
			WHERE conversation_key = ? AND role = 'assistant'
		)`, string(key),
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check assistant participation: %w", err)
	}
	return exists, nil
}

func (s *Store) RecentMessages(
	ctx context.Context,
	key domain.ConversationKey,
	limit int,
) ([]domain.Message, error) {
	if limit <= 0 {
		return []domain.Message{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content, user_id, external_ts, created_at
		FROM (
			SELECT id, role, content, user_id, external_ts, created_at
			FROM messages
			WHERE conversation_key = ?
			ORDER BY created_at DESC, id DESC
			LIMIT ?
		)
		ORDER BY created_at ASC, id ASC`, string(key), limit)
	if err != nil {
		return nil, fmt.Errorf("read recent conversation messages: %w", err)
	}
	defer rows.Close()

	messages := make([]domain.Message, 0, limit)
	for rows.Next() {
		var (
			message      domain.Message
			role         string
			createdNanos int64
		)
		if err := rows.Scan(
			&role,
			&message.Content,
			&message.UserID,
			&message.ExternalTS,
			&createdNanos,
		); err != nil {
			return nil, fmt.Errorf("scan conversation message: %w", err)
		}
		message.Role = domain.Role(role)
		message.CreatedAt = time.Unix(0, createdNanos).UTC()
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversation messages: %w", err)
	}
	return messages, nil
}

func (s *Store) AppendMessage(
	ctx context.Context,
	metadata domain.ConversationMetadata,
	message domain.Message,
	retain int,
) error {
	if retain <= 0 {
		return errors.New("message retention must be positive")
	}
	if message.Role != domain.RoleUser && message.Role != domain.RoleAssistant {
		return fmt.Errorf("unsupported conversation role %q", message.Role)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin append conversation message: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	createdNanos := message.CreatedAt.UnixNano()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO conversations (
			conversation_key, team_id, channel_id, channel_kind,
			root_ts, last_ts, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (conversation_key) DO UPDATE SET
			last_ts = CASE
				WHEN excluded.last_ts > conversations.last_ts THEN excluded.last_ts
				ELSE conversations.last_ts
			END,
			updated_at = max(conversations.updated_at, excluded.updated_at)
		WHERE conversations.team_id = excluded.team_id
			AND conversations.channel_id = excluded.channel_id
			AND conversations.channel_kind = excluded.channel_kind
			AND conversations.root_ts = excluded.root_ts`,
		string(metadata.Key), metadata.TeamID, metadata.ChannelID, string(metadata.ChannelKind),
		metadata.RootTS, metadata.LastTS, createdNanos, createdNanos,
	)
	if err != nil {
		return fmt.Errorf("upsert conversation metadata: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect conversation metadata update: %w", err)
	}
	if affected == 0 {
		return ErrMetadataConflict
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages (
			conversation_key, role, content, user_id, external_ts, created_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		string(metadata.Key), string(message.Role), message.Content,
		message.UserID, message.ExternalTS, createdNanos,
	); err != nil {
		return fmt.Errorf("insert conversation message: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM messages
		WHERE id IN (
			SELECT id
			FROM messages
			WHERE conversation_key = ?
			ORDER BY created_at DESC, id DESC
			LIMIT -1 OFFSET ?
		)`, string(metadata.Key), retain); err != nil {
		return fmt.Errorf("prune conversation messages: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit conversation message: %w", err)
	}
	return nil
}

// ProbeReadWrite verifies access to the migrated main database without leaving
// application data behind.
func (s *Store) ProbeReadWrite(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin SQLite read/write probe: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("probe SQLite read access: %w", err)
	}
	if version != SchemaVersion {
		return fmt.Errorf("probe SQLite schema version: got %d, want %d", version, SchemaVersion)
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO dedupe_records (dedupe_key, created_at, expires_at)
		VALUES ('__local_agent_read_write_probe__', ?, ?)
		ON CONFLICT (dedupe_key) DO UPDATE SET
			created_at = excluded.created_at,
			expires_at = excluded.expires_at`,
		now.UnixNano(), now.Add(time.Minute).UnixNano(),
	); err != nil {
		return fmt.Errorf("probe SQLite write access: %w", err)
	}

	if err := tx.Rollback(); err != nil {
		return fmt.Errorf("rollback SQLite read/write probe: %w", err)
	}
	return nil
}
