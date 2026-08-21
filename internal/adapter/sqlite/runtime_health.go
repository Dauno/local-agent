package sqlite

import (
	"context"
	"errors"
	"fmt"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// CheckSQLiteRuntime is the offline doctor check for the connection model
// (TRD 08 checkpoint 6). It reads the pragmas the current connection
// actually carries; it never asserts a value the driver does not expose,
// so it does not report on _txlock (see checkpoint 6 worker prompt).
func (s *Store) CheckSQLiteRuntime(ctx context.Context) (domain.SQLiteRuntimeHealth, error) {
	if s == nil || s.db == nil {
		return domain.SQLiteRuntimeHealth{}, errors.New("SQLite store is not configured")
	}
	health := domain.SQLiteRuntimeHealth{}
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&health.SchemaVersion); err != nil {
		return health, fmt.Errorf("read schema version: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&health.JournalMode); err != nil {
		return health, fmt.Errorf("read journal mode: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&health.Synchronous); err != nil {
		return health, fmt.Errorf("read synchronous mode: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&health.BusyTimeoutMillis); err != nil {
		return health, fmt.Errorf("read busy timeout: %w", err)
	}
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return health, fmt.Errorf("read foreign key enforcement: %w", err)
	}
	health.ForeignKeys = foreignKeys != 0
	health.MaxOpenConnections = s.db.Stats().MaxOpenConnections
	return health, nil
}
