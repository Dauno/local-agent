package sqlite

import (
	"strings"
	"testing"
	"time"
)

func TestMigrationV45BackfillsPoliciesAndActivationScopesWithoutCreatingRows(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 44)
	insertJob := func(id, mode, workstream, task, execution string) {
		t.Helper()
		_, err := raw.ExecContext(t.Context(), `INSERT INTO external_agent_jobs (
			job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
			task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
			conversation_key, workstream_id, task_id, execution_identity, admission_revision,
			status, timeout_at, created_at, updated_at)
			VALUES (?, ?, 'provider', 'profile', 'project', '[]', 'registry', 'task',
			'request', ?, ?, 'U12345678', 'T12345678', 'slack:T12345678:dm:D12345678',
			?, ?, ?, 2, 'completed', 2, 1, 1)`, id, mode, id+"-wrapper", id+"-call", workstream, task, execution)
		if err != nil {
			t.Fatal(err)
		}
	}
	insertJob("foreground", "foreground", "ws-fg", "task-fg", "exec-fg")
	insertJob("detached-bound", "detached", "ws-bound", "task-bound", "exec-bound")
	insertJob("detached-unbound", "detached", "", "", "")
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO external_agent_job_activations (
		job_id, status_revision, kind, activation_id, terminal_status, notification_sha256,
		result_sha256, actor, team_id, conversation_key, workstream_id, task_id, execution_identity, admission_revision,
		original_call_id, delivery_mode, content_bytes, slack_message_ts, published_at, state, next_attempt_at, created_at, updated_at)
		VALUES ('detached-bound', 1, 'terminal', 'activation-bound', 'completed', ?, ?, 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', 'ws-bound', 'task-bound', 'exec-bound', 2,
		'detached-bound-call', 'markdown', 1, '1710000000.000001', 1710000000000000000, 'pending', 0, 1, 1)`, strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO external_agent_job_activations (
		job_id, status_revision, kind, activation_id, terminal_status, notification_sha256,
		result_sha256, actor, team_id, conversation_key, original_call_id, delivery_mode,
		content_bytes, slack_message_ts, published_at, state, next_attempt_at, created_at, updated_at)
		VALUES ('detached-unbound', 1, 'terminal', 'activation-unbound', 'completed', ?, ?, 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', 'detached-unbound-call', 'markdown', 1, '1710000000.000002', 1710000000000000001, 'pending', 0, 1, 1)`, strings.Repeat("c", 64), strings.Repeat("d", 64)); err != nil {
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
	for _, want := range []struct {
		jobID  string
		policy string
	}{
		{"foreground", "delivery_only"},
		{"detached-bound", "workstream_only"},
		{"detached-unbound", "delivery_only"},
	} {
		var got string
		if err := upgraded.DB().QueryRowContext(t.Context(), `SELECT completion_policy FROM external_agent_jobs WHERE job_id = ?`, want.jobID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want.policy {
			t.Fatalf("job %q policy = %q, want %q", want.jobID, got, want.policy)
		}
	}
	for _, want := range []struct {
		id, scope string
	}{
		{"activation-bound", "workstream"},
		{"activation-unbound", "legacy"},
	} {
		var got string
		if err := upgraded.DB().QueryRowContext(t.Context(), `SELECT activation_scope FROM external_agent_job_activations WHERE activation_id = ?`, want.id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want.scope {
			t.Fatalf("activation %q scope = %q, want %q", want.id, got, want.scope)
		}
	}
	var activationCount int
	if err := upgraded.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM external_agent_job_activations`).Scan(&activationCount); err != nil {
		t.Fatal(err)
	}
	if activationCount != 2 {
		t.Fatalf("activation rows = %d, want 2", activationCount)
	}
	var version int
	if err := upgraded.DB().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema version = %d, err = %v, want %d", version, err, SchemaVersion)
	}
}

func TestMigrationV45CompletionPolicyAndActivationScopeAreImmutable(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/v45-immutable.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	job := testExternalAgentJob(time.Now().UTC())
	if created, _, err := NewExternalAgentJobStore(store).CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create = %v, err = %v", created, err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE external_agent_jobs SET completion_policy = 'automatic_root' WHERE job_id = ?`, job.ID); err == nil {
		t.Fatal("completion policy update bypassed immutable trigger")
	}
	if _, err := store.DB().ExecContext(t.Context(), `INSERT INTO external_agent_job_activations (
		job_id, status_revision, kind, activation_id, activation_scope, terminal_status, notification_sha256,
		result_sha256, actor, team_id, conversation_key, original_call_id, delivery_mode, content_bytes,
		slack_message_ts, published_at, state, next_attempt_at, created_at, updated_at)
		VALUES (?, 1, 'terminal', ?, 'conversation', 'completed', ?, ?, ?, ?, ?, ?, 'markdown', 1, '1710000000.000001', 1710000000000000000, 'pending', 0, 1, 1)`,
		job.ID, "activation-immutable", strings.Repeat("a", 64), strings.Repeat("b", 64), job.Actor, job.TeamID, string(job.ConversationKey), job.OriginalCallID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE external_agent_job_activations SET activation_scope = 'workstream' WHERE activation_id = ?`, "activation-immutable"); err == nil {
		t.Fatal("activation scope update bypassed immutable trigger")
	}
}
