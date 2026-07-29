package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestAgentDraftStoreRoundTripAndCompareAndSet(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	drafts := NewAgentDraftStore(store)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	want := &port.AgentDraft{
		DraftID:         "draft-1",
		TeamID:          "T12345678",
		ActorID:         "U12345678",
		ConversationKey: "slack:T12345678:dm:D12345678",
		Name:            "release-notes",
		Description:     "Writes release notes",
		Instruction:     "Summarize merged changes",
		Model:           "provider/profile",
		DefinitionHash:  "hash",
		CatalogRevision: 3,
		Status:          port.DraftStatusDraft,
		CreatedAt:       now,
		ExpiresAt:       now.Add(time.Hour),
	}
	if err := drafts.Create(ctx, want); err != nil {
		t.Fatal(err)
	}

	got, err := drafts.Get(ctx, want.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != *want {
		t.Fatalf("Get() = %#v, want %#v", got, want)
	}
	if err := drafts.MarkPreviewed(ctx, want.DraftID, "preview-hash", 4); err != nil {
		t.Fatal(err)
	}
	got, err = drafts.Get(ctx, want.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != port.DraftStatusPreviewed || got.DefinitionHash != "preview-hash" || got.CatalogRevision != 4 {
		t.Fatalf("status after transition = %#v, want %q", got, port.DraftStatusPreviewed)
	}
	found, err := drafts.FindByNameAndDefinitionHash(ctx, want.Name, "preview-hash")
	if err != nil {
		t.Fatal(err)
	}
	if found == nil || found.DraftID != want.DraftID {
		t.Fatalf("FindByNameAndDefinitionHash() = %#v, want draft %q", found, want.DraftID)
	}
	if err := drafts.UpdateStatus(ctx, want.DraftID, port.DraftStatusPreviewed, port.DraftStatusFailed); err == nil {
		t.Fatal("invalid previewed to failed transition unexpectedly succeeded")
	}
	if err := drafts.UpdateStatus(ctx, want.DraftID, port.DraftStatusPreviewed, port.DraftStatusInstallRequested); err != nil {
		t.Fatal(err)
	}
	if err := drafts.UpdateStatus(ctx, want.DraftID, port.DraftStatusInstallRequested, port.DraftStatusInstalled); err != nil {
		t.Fatal(err)
	}
	if err := drafts.UpdateStatus(ctx, want.DraftID, port.DraftStatusDraft, port.DraftStatusInstallRequested); err == nil {
		t.Fatal("stale status transition unexpectedly succeeded")
	}
	got, err = drafts.Get(ctx, want.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != port.DraftStatusInstalled {
		t.Fatalf("status after CAS = %#v, want %q", got, port.DraftStatusInstalled)
	}

	expiring := *want
	expiring.DraftID = "draft-2"
	expiring.Status = port.DraftStatusDraft
	if err := drafts.Create(ctx, &expiring); err != nil {
		t.Fatal(err)
	}
	if err := drafts.ExpireDrafts(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err = drafts.Get(ctx, expiring.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != port.DraftStatusExpired {
		t.Fatalf("expired draft = %#v, want status %q", got, port.DraftStatusExpired)
	}
}

func TestOpenExistingUpgradesV19WithAgentDrafts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v19.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dsn, err := dataSourceName(path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	for version := 1; version <= 19; version++ {
		if err := migrations[version](ctx, tx); err != nil {
			_ = tx.Rollback()
			_ = raw.Close()
			t.Fatalf("migration %d: %v", version, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 19"); err != nil {
		_ = tx.Rollback()
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, SchemaVersion)
	}
	var table string
	if err := store.db.QueryRowContext(ctx, "SELECT name FROM sqlite_schema WHERE type = 'table' AND name = 'agent_drafts'").Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table != "agent_drafts" {
		t.Fatalf("agent drafts table = %q", table)
	}
}
