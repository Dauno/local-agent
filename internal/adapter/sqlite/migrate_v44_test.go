package sqlite

import "testing"

func TestMigrationV44AddsErrorClassWithoutInferringHistoricalRows(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 43)
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO external_agent_jobs (
		job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
		task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
		conversation_key, status, acp_session_id, timeout_at, created_at, updated_at)
		VALUES ('legacy-progress-job', 'detached', 'agentcli', 'build', 'workspace', '[]', 'r1',
		'task', 'request', 'wrapper', 'original', 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', 'completion_unknown', 'legacy-session', 2, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO external_agent_job_progress (
		job_id, attempt, phase, last_event_kind, created_at, updated_at)
		VALUES ('legacy-progress-job', 1, 'failed', 'process_failed', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upgraded.Close() }()
	var errorClass string
	if err := upgraded.DB().QueryRowContext(t.Context(), `SELECT error_class FROM external_agent_job_progress WHERE job_id = 'legacy-progress-job'`).Scan(&errorClass); err != nil {
		t.Fatal(err)
	}
	if errorClass != "" {
		t.Fatalf("historical error class = %q, want no inferred class", errorClass)
	}
	var version int
	if err := upgraded.DB().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
}
