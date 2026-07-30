package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
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
		Kind:            "llm",
		ExecutionMode:   "foreground",
		CanonicalYAML:   "agent_class: LlmAgent\n",
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

func TestAgentDraftStoreACPModelIsNullable(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	drafts := NewAgentDraftStore(store)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	draft := &port.AgentDraft{
		DraftID: "draft-acp", TeamID: "T12345678", ActorID: "U12345678",
		ConversationKey: "slack:T12345678:dm:D12345678", Name: "acp_worker",
		DefinitionHash: "hash", CatalogRevision: 1, Kind: "acp",
		ExecutionMode: "foreground", TimeoutSeconds: 7200,
		CanonicalYAML: "agent_class: AcpAgent\n", Status: port.DraftStatusPreviewed,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := drafts.Create(ctx, draft); err != nil {
		t.Fatal(err)
	}
	got, err := drafts.Get(ctx, draft.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Model != "" || got.Kind != "acp" || got.TimeoutSeconds != 7200 {
		t.Fatalf("ACP draft = %#v", got)
	}
	var isNull bool
	if err := store.db.QueryRowContext(ctx, "SELECT model IS NULL FROM agent_drafts WHERE draft_id = ?", draft.DraftID).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Fatal("ACP model should be stored as NULL")
	}
}

func TestAgentDraftStoreRejectsInvalidV2Policies(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	drafts := NewAgentDraftStore(store)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	base := port.AgentDraft{
		DraftID: "draft-policy", TeamID: "T12345678", ActorID: "U12345678",
		ConversationKey: "slack:T12345678:dm:D12345678", Name: "policy_worker",
		DefinitionHash: "hash", CatalogRevision: 1, Kind: "llm", ExecutionMode: "foreground",
		Status: port.DraftStatusDraft, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	tests := []struct {
		name   string
		mutate func(*port.AgentDraft)
	}{
		{name: "invalid kind", mutate: func(d *port.AgentDraft) { d.Kind = "other" }},
		{name: "ACP timeout too high", mutate: func(d *port.AgentDraft) { d.Kind = "acp"; d.ExecutionMode = "foreground"; d.TimeoutSeconds = 86401 }},
		{name: "LLM durable job", mutate: func(d *port.AgentDraft) { d.ExecutionMode = "durable_job" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			draft := base
			draft.DraftID = "draft-policy-" + tt.name
			tt.mutate(&draft)
			if err := drafts.Create(ctx, &draft); err == nil {
				t.Fatal("Create() unexpectedly succeeded")
			}
		})
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

func TestOpenExistingUpgradesDraftSchemaFromV19ThroughV22(t *testing.T) {
	ctx := context.Background()
	for _, version := range []int{19, 20, 21, 22} {
		t.Run("v"+strconv.Itoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
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
				_ = raw.Close()
				t.Fatal(err)
			}
			for migration := 1; migration <= version; migration++ {
				if err := migrations[migration](ctx, tx); err != nil {
					_ = tx.Rollback()
					_ = raw.Close()
					t.Fatalf("migration %d: %v", migration, err)
				}
			}
			if version >= 20 {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO agent_drafts
					(draft_id, team_id, actor_id, conversation_key, name, description, instruction,
					 model, definition_hash, catalog_revision, status, created_at, expires_at)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					"legacy-draft", "T12345678", "U12345678", "slack:T12345678:dm:D12345678",
					"legacy_worker", "legacy", "legacy instruction", "legacy/model", "hash", 1,
					"draft", 100, 200); err != nil {
					_ = tx.Rollback()
					_ = raw.Close()
					t.Fatal(err)
				}
			}
			if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(version)); err != nil {
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
			var gotVersion int
			if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&gotVersion); err != nil {
				t.Fatal(err)
			}
			if gotVersion != SchemaVersion {
				t.Fatalf("schema version = %d, want %d", gotVersion, SchemaVersion)
			}
			if version >= 20 {
				var kind, mode string
				var timeout int
				if err := store.db.QueryRowContext(ctx, "SELECT kind, execution_mode, timeout_seconds FROM agent_drafts WHERE draft_id = ?", "legacy-draft").Scan(&kind, &mode, &timeout); err != nil {
					t.Fatal(err)
				}
				if kind != "llm" || mode != "foreground" || timeout != 0 {
					t.Fatalf("legacy draft policy = %q/%q/%d", kind, mode, timeout)
				}
			}
		})
	}
}
