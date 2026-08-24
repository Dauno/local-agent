package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

func TestCreateRecordsAdoptionAtCreationState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fresh.db")
	store, err := Create(ctx, path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = store.Close() }()

	state, err := FileSchemaProbe{}.ReadRolloutState(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !state.BaselinePresent || !state.BaselineValid || state.Baseline.JobsCompletedWithoutResultIdentity != 0 || state.Baseline.ActivationsWithoutContent != 0 {
		t.Fatalf("baseline = %+v, want fixed zero", state.Baseline)
	}
	if !state.CutoffPresent || !state.CutoffValid || state.CutoffUnixNanos <= 0 {
		t.Fatalf("cutoff = %d, want a real captured timestamp", state.CutoffUnixNanos)
	}
	if !state.PostflightPresent || state.PostflightStatus != rollout.PostflightPassed {
		t.Fatalf("postflight status = %q, want passed", state.PostflightStatus)
	}
	if !state.PostflightDetailPresent || state.PostflightDetail != createAdoptionPostflightDetail {
		t.Fatalf("detail = %q, want the fixed created-at-v42 string", state.PostflightDetail)
	}
	if !state.BackupNotRequiredAtPresent || !state.BackupNotRequiredAtValid {
		t.Fatalf("not-required marker present=%v valid=%v, want a valid RFC 3339 marker",
			state.BackupNotRequiredAtPresent, state.BackupNotRequiredAtValid)
	}
	shape, err := rollout.ClassifyBackupIdentity(state)
	if err != nil || shape != rollout.BackupIdentityNotRequired {
		t.Fatalf("backup identity shape=%d err=%v, want NotRequired (FIND-166)", shape, err)
	}
	row, classifyErr := rollout.ClassifyRollout(rollout.TargetVersion, state)
	if classifyErr != nil || row != rollout.RolloutRowAlreadyComplete {
		t.Fatalf("row=%d err=%v, want AlreadyComplete for a fresh Create file", row, classifyErr)
	}

	var quarantineMarker string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT state_value FROM runtime_state WHERE state_key = ?`, rollout.KeyLegacyQuarantineAt).Scan(&quarantineMarker); err != nil {
		t.Fatalf("quarantine marker missing from adoption transaction 2: %v", err)
	}
	if _, parseErr := time.Parse(time.RFC3339, quarantineMarker); parseErr != nil {
		t.Fatalf("quarantine marker %q is not RFC 3339: %v", quarantineMarker, parseErr)
	}
}

// TestCrashWindowBetweenCreateTransactionsClassifiesAdoption proves the two
// transactions are distinct durable steps: a file stopped after transaction
// 1 (bare v42, zero rollout keys) classifies exactly as Recovery Table
// row 3, with no special-cased recovery code.
func TestCrashWindowBetweenCreateTransactionsClassifiesAdoption(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, rollout.TargetVersion)
	defer func() { _ = raw.Close() }()

	state, err := FileSchemaProbe{}.ReadRolloutState(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	shape, err := rollout.ClassifyBackupIdentity(state)
	if err != nil || shape != rollout.BackupIdentityAbsent {
		t.Fatalf("shape=%d err=%v, want Absent in the crash window", shape, err)
	}
	row, classifyErr := rollout.ClassifyRollout(rollout.TargetVersion, state)
	if classifyErr != nil || row != rollout.RolloutRowAdoption {
		t.Fatalf("row=%d err=%v, want Adoption in the crash window", row, classifyErr)
	}
}

// TestAdoptionTransactionNeverRewritesExistingKeys pins the DO NOTHING
// semantics of transaction 2 and its separation from the migration commit:
// replaying it against an already-stamped file leaves every value untouched.
func TestAdoptionTransactionNeverRewritesExistingKeys(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "replayed.db")
	store, err := Create(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	type snapshot struct {
		value     string
		updatedAt int64
	}
	keys := []string{rollout.KeyCutoff, rollout.KeyBackupNotRequiredAt, rollout.KeyLegacyQuarantineAt}
	before := map[string]snapshot{}
	for _, key := range keys {
		var snap snapshot
		if err := store.DB().QueryRowContext(ctx,
			`SELECT state_value, updated_at FROM runtime_state WHERE state_key = ?`, key).Scan(&snap.value, &snap.updatedAt); err != nil {
			t.Fatal(err)
		}
		before[key] = snap
	}

	if err := recordAdoptionAtCreation(ctx, store.DB()); err != nil {
		t.Fatalf("replay transaction 2: %v", err)
	}
	for _, key := range keys {
		var snap snapshot
		if err := store.DB().QueryRowContext(ctx,
			`SELECT state_value, updated_at FROM runtime_state WHERE state_key = ?`, key).Scan(&snap.value, &snap.updatedAt); err != nil {
			t.Fatal(err)
		}
		if snap != before[key] {
			t.Fatalf("%s changed on replay: %+v -> %+v", key, before[key], snap)
		}
	}
}
