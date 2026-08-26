package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestMigrationV37AddsCompletionBindingColumns(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/completion-v37.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	var version int
	if err := store.DB().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, SchemaVersion)
	}

	wanted := map[string][]string{
		"external_agent_jobs": {
			"workstream_id", "task_id", "execution_identity", "admission_revision",
		},
		"external_agent_job_notifications": {
			"workstream_id", "task_id", "execution_identity", "admission_revision",
		},
		"external_agent_job_activations": {
			"workstream_id", "task_id", "execution_identity", "admission_revision",
			"fallback_required", "fallback_slack_ts",
		},
	}
	for table, columns := range wanted {
		rows, err := store.DB().QueryContext(t.Context(), `PRAGMA table_info(`+table+`)`)
		if err != nil {
			t.Fatalf("table_info(%s): %v", table, err)
		}
		found := make(map[string]bool)
		for rows.Next() {
			var cid int
			var name, columnType string
			var notNull, primaryKey int
			var defaultValue sql.NullString
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				_ = rows.Close()
				t.Fatalf("scan table_info(%s): %v", table, err)
			}
			found[name] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("iterate table_info(%s): %v", table, err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close table_info(%s): %v", table, err)
		}
		for _, column := range columns {
			if !found[column] {
				t.Fatalf("table %s is missing v37 column %q", table, column)
			}
		}
	}
	var triggerSQL string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'external_agent_job_notifications_completion_binding_immutable'`).Scan(&triggerSQL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(triggerSQL, "result_sha256") {
		t.Fatalf("notification completion trigger does not protect result identity: %s", triggerSQL)
	}
}

func TestMigrationV37PreservesJournalAndExtendsStartTask(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 36)
	seedV34WorkstreamJournal(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	if _, err := store.DB().ExecContext(t.Context(), `INSERT INTO workstream_transitions
		(workstream_id, from_revision, to_revision, source, source_id, actor, action,
		payload_digest, payload_json, state_digest, state_json, committed_at)
		VALUES ('ws-v35', 1, 2, 'human', 'start-v37', 'U1', 'start_task',
		'start-digest', 'start-payload', 'start-state-digest', 'start-state', 3)`); err != nil {
		t.Fatalf("v37 rejected start_task journal action: %v", err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE workstream_transitions SET actor = 'U2' WHERE workstream_id = 'ws-v35' AND to_revision = 2`); err == nil {
		t.Fatal("v37 journal update was accepted")
	}
	if _, err := store.DB().ExecContext(t.Context(), `DELETE FROM workstream_transitions WHERE workstream_id = 'ws-v35' AND to_revision = 2`); err == nil {
		t.Fatal("v37 journal delete was accepted")
	}
}

func TestMigrationV37FailureRollsBackWithoutPartialSchema(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 36)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	original, registered := migrations[37]
	if registered {
		defer func() { migrations[37] = original }()
	} else {
		defer delete(migrations, 37)
	}
	migrations[37] = func(ctx context.Context, tx *sql.Tx) error {
		if original == nil {
			return errors.New("v37 migration is not registered")
		}
		if err := original(ctx, tx); err != nil {
			return err
		}
		return errors.New("injected v37 failure")
	}

	store, err := OpenExisting(t.Context(), path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting succeeded after injected v37 failure")
	}
	if err == nil || !strings.Contains(err.Error(), "injected v37 failure") {
		t.Fatalf("OpenExisting error = %v, want injected v37 failure", err)
	}

	check, err := sql.Open("sqlite", mustDataSourceName(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = check.Close() }()
	var version, columns int
	if err := check.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_table_info('external_agent_jobs') WHERE name IN ('workstream_id', 'task_id', 'execution_identity', 'admission_revision')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if version != 36 || columns != 0 {
		t.Fatalf("rolled-back v37 state = version %d, binding columns %d", version, columns)
	}
}
