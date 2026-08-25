package sqlite

import (
	"strings"
	"testing"
	"time"
)

func TestMigrationV43AddsTranscriptWithoutInferringHistoricalRows(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 42)
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO external_agent_jobs (
		job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
		task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
		conversation_key, status, acp_session_id, timeout_at, created_at, updated_at)
		VALUES ('legacy-session-job', 'detached', 'opencode', 'build', 'workspace', '[]', 'r1',
		'task', 'request', 'wrapper', 'original', 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', 'completion_unknown', 'legacy-session', 2, 1, 1)`); err != nil {
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
	var transcript string
	if err := upgraded.DB().QueryRowContext(t.Context(), `SELECT transcript_path FROM external_agent_jobs WHERE job_id = 'legacy-session-job'`).Scan(&transcript); err != nil {
		t.Fatal(err)
	}
	if transcript != "" {
		t.Fatalf("historical transcript = %q, want no inferred path", transcript)
	}
	var version int
	if err := upgraded.DB().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
}

func TestTranscriptAssignmentUsesSessionLeaseCAS(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/transcript.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	jobs := NewExternalAgentJobStore(store)
	job := testExternalAgentJob(time.Now().UTC())
	if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create = %v, err = %v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), job.CreatedAt, "worker-transcript", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, err = %v", claimed, err)
	}
	path := "/home/operator/.codex/sessions/rollout-session-1.jsonl"
	if err := jobs.AssignExternalAgentSession(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, "session-1"); err != nil {
		t.Fatal(err)
	}
	if err := jobs.AssignTranscriptPath(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, path); err != nil {
		t.Fatal(err)
	}
	loaded, err := jobs.GetJob(t.Context(), job.ID)
	if err != nil || loaded == nil || loaded.TranscriptPath != path {
		t.Fatalf("loaded = %#v, err = %v", loaded, err)
	}
	if err := jobs.AssignExternalAgentSession(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, "session-2"); err == nil {
		t.Fatal("a second session assignment bypassed the CAS")
	}
	if err := jobs.AssignTranscriptPath(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, strings.Replace(path, "1", "2", 1)); err == nil {
		t.Fatal("a second transcript assignment bypassed the CAS")
	}
}
