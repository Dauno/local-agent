package sqlite

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

// FileSchemaWriter implements rollout.SchemaWriter. RecordBaselineAndCutoff
// and RecordPostflight run on short-lived mode=rw connections that write
// runtime_state only and never migrate; Migrate is OpenExisting itself.
type FileSchemaWriter struct{}

// RecordBaselineAndCutoff commits baseline+cutoff (ON CONFLICT DO NOTHING:
// immutable once set) together with the five backup-identity keys (ON
// CONFLICT DO UPDATE: a row-2 resume that created a replacement must be able
// to overwrite them) in one small transaction. This is the first mode=rw
// open of any upgrade invocation that enters it; journal_mode flips only
// after a verified backup exists.
func (FileSchemaWriter) RecordBaselineAndCutoff(
	ctx context.Context,
	path string,
	baseline rollout.IdentityBaseline,
	cutoffUnixNanos int64,
	backup rollout.BackupIdentity,
) error {
	store, err := open(ctx, path, "rw", false)
	if err != nil {
		return fmt.Errorf("open database for rollout write: %w", err)
	}
	defer func() { _ = store.Close() }()
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin baseline/cutoff transaction: %w", err)
	}
	now := time.Now().UTC().UnixNano()
	const insertNothing = `INSERT INTO runtime_state (state_key, state_value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (state_key) DO NOTHING`
	const insertUpdate = `INSERT INTO runtime_state (state_key, state_value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (state_key) DO UPDATE SET state_value = excluded.state_value, updated_at = excluded.updated_at`
	entries := []struct {
		query string
		key   string
		value string
	}{
		{insertNothing, rollout.KeyBaseline, rollout.FormatBaseline(baseline)},
		{insertNothing, rollout.KeyCutoff, strconv.FormatInt(cutoffUnixNanos, 10)},
		{insertUpdate, rollout.KeyBackupPath, backup.Path},
		{insertUpdate, rollout.KeyBackupBytes, strconv.FormatInt(backup.Bytes, 10)},
		{insertUpdate, rollout.KeyBackupSHA256, backup.SHA256},
		{insertUpdate, rollout.KeyBackupSourceVersion, strconv.Itoa(backup.SourceVersion)},
		{insertUpdate, rollout.KeyBackupVerifiedAt, backup.VerifiedAt.UTC().Format(time.RFC3339)},
	}
	for _, entry := range entries {
		if _, err := tx.ExecContext(ctx, entry.query, entry.key, entry.value, now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("write runtime_state key %s: %w", entry.key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit baseline/cutoff transaction: %w", err)
	}
	return nil
}

func (FileSchemaWriter) Migrate(ctx context.Context, path string) error {
	store, err := OpenExisting(ctx, path)
	if err != nil {
		return err
	}
	return store.Close()
}

// RecordPostflight writes status and detail inside ONE transaction holding
// both upserts: they commit together or neither becomes durable, so no
// crash, cancellation, or injected failure between the statements can leave
// exactly one key behind (the partial shape the Corrupt table rejects).
func (FileSchemaWriter) RecordPostflight(ctx context.Context, path string, status rollout.PostflightStatus, detail string) error {
	store, err := open(ctx, path, "rw", false)
	if err != nil {
		return fmt.Errorf("open database for postflight write: %w", err)
	}
	defer func() { _ = store.Close() }()
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin postflight transaction: %w", err)
	}
	const upsert = `INSERT INTO runtime_state (state_key, state_value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (state_key) DO UPDATE SET state_value = excluded.state_value, updated_at = excluded.updated_at`
	now := time.Now().UTC().UnixNano()
	for _, entry := range []struct {
		key   string
		value string
	}{
		{rollout.KeyPostflightStatus, string(status)},
		{rollout.KeyPostflightDetail, detail},
	} {
		if _, err := tx.ExecContext(ctx, upsert, entry.key, entry.value, now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("write runtime_state key %s: %w", entry.key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit postflight transaction: %w", err)
	}
	return nil
}
