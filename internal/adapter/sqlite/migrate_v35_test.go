package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestMigrationV35PreservesJournalAndExtendsActionConstraint(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 34)
	seedV34WorkstreamJournal(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	assertV35Journal(t, store.DB())
	if _, err := store.DB().ExecContext(t.Context(), `INSERT INTO workstream_transitions
		(workstream_id, from_revision, to_revision, source, source_id, actor, action,
		payload_digest, payload_json, state_digest, state_json, committed_at)
		VALUES ('ws-v35', 1, 2, 'root', 'link-v35', 'U1', 'link_completed_result',
		'link-digest', 'link-payload', 'link-state-digest', 'link-state', 3)`); err != nil {
		t.Fatalf("v35 rejected link_completed_result: %v", err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE workstream_transitions SET actor = 'U2' WHERE workstream_id = 'ws-v35' AND to_revision = 1`); err == nil {
		t.Fatal("v35 journal update was accepted")
	}
	if _, err := store.DB().ExecContext(t.Context(), `DELETE FROM workstream_transitions WHERE workstream_id = 'ws-v35' AND to_revision = 1`); err == nil {
		t.Fatal("v35 journal delete was accepted")
	}
	for _, name := range []string{"workstream_transitions_by_workstream", "workstream_transitions_by_source", "workstream_transitions_immutable_update", "workstream_transitions_immutable_delete"} {
		var found string
		if err := store.DB().QueryRowContext(t.Context(), `SELECT name FROM sqlite_schema WHERE name = ?`, name).Scan(&found); err != nil || found != name {
			t.Fatalf("v35 schema object %q = %q, %v", name, found, err)
		}
	}
}

func TestMigrationV35CrashRollsBackJournalRebuild(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 34)
	seedV34WorkstreamJournal(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	original := migrations[35]
	migrations[35] = func(ctx context.Context, tx *sql.Tx) error {
		if err := migrateV35(ctx, tx); err != nil {
			return err
		}
		return errors.New("injected v35 crash")
	}
	defer func() { migrations[35] = original }()

	store, err := OpenExisting(t.Context(), path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting succeeded after injected v35 crash")
	}
	if err == nil || !strings.Contains(err.Error(), "injected v35 crash") {
		t.Fatalf("OpenExisting error = %v", err)
	}
	raw, err = sql.Open("sqlite", mustDataSourceName(t, path))
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := raw.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if version != 34 {
		_ = raw.Close()
		t.Fatalf("schema version after v35 crash = %d, want 34", version)
	}
	assertV35Journal(t, raw)
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO workstream_transitions
		(workstream_id, from_revision, to_revision, source, source_id, actor, action,
		payload_digest, payload_json, state_digest, state_json, committed_at)
		VALUES ('ws-v35', 1, 2, 'root', 'link-before-v35', 'U1', 'link_completed_result',
		'digest', 'payload', 'state-digest', 'state', 3)`); err == nil {
		_ = raw.Close()
		t.Fatal("rolled-back v34 journal accepted v35 action")
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrations[35] = original
	store, err = OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen after v35 crash: %v", err)
	}
	defer store.Close()
	assertV35Journal(t, store.DB())
}

func seedV34WorkstreamJournal(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO workstreams
		(workstream_id, conversation_key, owner_actor, project, status, revision, objective, created_at, updated_at)
		VALUES ('ws-v35', 'slack:T1:dm:D1', 'U1', 'app', 'active', 1, 'preserve journal', 1, 2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO workstream_transitions
		(workstream_id, from_revision, to_revision, source, source_id, actor, action,
		payload_digest, payload_json, state_digest, state_json, committed_at) VALUES
		('ws-v35', 0, 0, 'human', 'create-v35', 'U1', 'create_workstream', 'create-digest', 'create-payload', 'state-0-digest', 'state-0', 1),
		('ws-v35', 0, 1, 'root', 'revise-v35', 'U1', 'revise_plan', 'revise-digest', 'revise-payload', 'state-1-digest', 'state-1', 2)`); err != nil {
		t.Fatal(err)
	}
}

func assertV35Journal(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT source_id, payload_digest, payload_json, state_digest, state_json
		FROM workstream_transitions WHERE workstream_id = 'ws-v35' ORDER BY to_revision`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := [][5]string{
		{"create-v35", "create-digest", "create-payload", "state-0-digest", "state-0"},
		{"revise-v35", "revise-digest", "revise-payload", "state-1-digest", "state-1"},
	}
	var got [][5]string
	for rows.Next() {
		var row [5]string
		if err := rows.Scan(&row[0], &row[1], &row[2], &row[3], &row[4]); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("journal rows = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("journal row %d = %#v, want %#v", index, got[index], want[index])
		}
	}
}
