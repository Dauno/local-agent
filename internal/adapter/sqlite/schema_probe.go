package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

// FileSchemaProbe implements rollout.SchemaProbe. Every method takes only
// the database path and opens its own mode=ro connection; no connection
// handle crosses a method boundary, so the caller's lock is what makes a
// sequence of these reads trustworthy.
type FileSchemaProbe struct{}

func (FileSchemaProbe) CurrentVersion(ctx context.Context, path string) (int, error) {
	store, err := OpenReadOnly(ctx, path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = store.Close() }()
	return currentUserVersion(ctx, store.DB())
}

func currentUserVersion(ctx context.Context, db *sql.DB) (int, error) {
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return current, nil
}

// CaptureIdentityBaseline measures the two carve-out counters postflight
// compares against the durable baseline. It is a pure read.
func (p FileSchemaProbe) CaptureIdentityBaseline(ctx context.Context, path string) (rollout.IdentityBaseline, error) {
	health, err := p.IdentityHealth(ctx, path)
	if err != nil {
		return rollout.IdentityBaseline{}, err
	}
	return rollout.IdentityBaseline{
		JobsCompletedWithoutResultIdentity: health.JobsCompletedWithoutResultIdentity,
		ActivationsWithoutContent:          health.ActivationsWithoutContent,
	}, nil
}

func (FileSchemaProbe) IdentityHealth(ctx context.Context, path string) (domain.ExternalAgentJobIdentityHealth, error) {
	store, err := OpenReadOnly(ctx, path)
	if err != nil {
		return domain.ExternalAgentJobIdentityHealth{}, err
	}
	defer func() { _ = store.Close() }()
	return NewExternalAgentJobStore(store).IdentityHealth(ctx)
}

// ReadRolloutState returns the raw presence/validity/value reading of every
// durable rollout key. It performs no classification itself.
func (FileSchemaProbe) ReadRolloutState(ctx context.Context, path string) (rollout.RolloutState, error) {
	store, err := OpenReadOnly(ctx, path)
	if err != nil {
		return rollout.RolloutState{}, err
	}
	defer func() { _ = store.Close() }()

	reader := rolloutStateReader{db: store.DB(), ctx: ctx}
	state := rollout.RolloutState{}

	if raw, present, err := reader.value(rollout.KeyBaseline); err != nil {
		return rollout.RolloutState{}, err
	} else if present {
		state.BaselinePresent = true
		parsed, ok := rollout.ParseBaseline(raw)
		state.BaselineValid = ok
		if ok {
			state.Baseline = parsed
		}
	}
	rawCutoff, cutoffPresent, err := reader.value(rollout.KeyCutoff)
	if err != nil {
		return rollout.RolloutState{}, err
	}
	if cutoffPresent {
		state.CutoffPresent = true
		parsed, ok := rollout.ParseNonNegativeDecimal(rawCutoff)
		state.CutoffValid = ok
		if ok {
			state.CutoffUnixNanos = parsed
		}
	}
	if rawPath, present, err := reader.value(rollout.KeyBackupPath); err != nil {
		return rollout.RolloutState{}, err
	} else if present {
		state.BackupPathPresent = true
		state.BackupPathValid = filepath.IsAbs(rawPath)
		if state.BackupPathValid {
			state.BackupPath = rawPath
		}
	}
	rawBytes, bytesPresent, err := reader.value(rollout.KeyBackupBytes)
	if err != nil {
		return rollout.RolloutState{}, err
	}
	if bytesPresent {
		state.BackupBytesPresent = true
		parsed, ok := rollout.ParseNonNegativeDecimal(rawBytes)
		state.BackupBytesValid = ok
		if ok {
			state.BackupBytes = parsed
		}
	}
	if rawSHA, present, err := reader.value(rollout.KeyBackupSHA256); err != nil {
		return rollout.RolloutState{}, err
	} else if present {
		state.BackupSHA256Present = true
		parsed, ok := rollout.ParseSHA256Hex(rawSHA)
		state.BackupSHA256Valid = ok
		if ok {
			state.BackupSHA256 = parsed
		}
	}
	rawSourceVersion, sourcePresent, err := reader.value(rollout.KeyBackupSourceVersion)
	if err != nil {
		return rollout.RolloutState{}, err
	}
	if sourcePresent {
		state.BackupSourceVersionPresent = true
		parsed, ok := rollout.ParseBackupSourceVersion(rawSourceVersion)
		state.BackupSourceVersionValid = ok
		if ok {
			state.BackupSourceVersion = parsed
		}
	}
	rawVerifiedAt, verifiedPresent, err := reader.value(rollout.KeyBackupVerifiedAt)
	if err != nil {
		return rollout.RolloutState{}, err
	}
	if verifiedPresent {
		state.BackupVerifiedAtPresent = true
		parsed, ok := rollout.ParseRFC3339(rawVerifiedAt)
		state.BackupVerifiedAtValid = ok
		if ok {
			state.BackupVerifiedAt = parsed
		}
	}
	rawNotRequiredAt, notRequiredPresent, err := reader.value(rollout.KeyBackupNotRequiredAt)
	if err != nil {
		return rollout.RolloutState{}, err
	}
	if notRequiredPresent {
		state.BackupNotRequiredAtPresent = true
		parsed, ok := rollout.ParseRFC3339(rawNotRequiredAt)
		state.BackupNotRequiredAtValid = ok
		if ok {
			state.BackupNotRequiredAt = parsed
		}
	}
	rawStatus, statusPresent, err := reader.value(rollout.KeyPostflightStatus)
	if err != nil {
		return rollout.RolloutState{}, err
	}
	if statusPresent {
		state.PostflightPresent = true
		switch rollout.PostflightStatus(rawStatus) {
		case rollout.PostflightPassed, rollout.PostflightFailed:
			state.PostflightValid = true
			state.PostflightStatus = rollout.PostflightStatus(rawStatus)
		default:
			state.PostflightValid = false
		}
	}
	rawDetail, detailPresent, err := reader.value(rollout.KeyPostflightDetail)
	if err != nil {
		return rollout.RolloutState{}, err
	}
	if detailPresent {
		state.PostflightDetailPresent = true
		state.PostflightDetail = rawDetail
	}
	return state, nil
}

type rolloutStateReader struct {
	db  *sql.DB
	ctx context.Context
}

func (r rolloutStateReader) value(key string) (value string, present bool, err error) {
	err = r.db.QueryRowContext(r.ctx, `SELECT state_value FROM runtime_state WHERE state_key = ?`, key).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("read runtime_state key %s: %w", key, err)
	default:
		return value, true, nil
	}
}
