package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestWorkstreamTaskOwnsExternalAgentJobAdmission(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "workstream-job.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	jobs := NewExternalAgentJobStore(store)
	now := time.Now().UTC()
	job := testExternalAgentJob(now)
	job.ID = "job-workstream-admission"
	job.Mode = domain.JobDetached
	job.CompletionPolicy = domain.ExternalAgentCompletionAutomaticRoot
	job.PrimaryProject = "workspace"
	job.Task = `{"version":"batch_v4","mode":"sequential"}`
	job.RequestSHA256 = "request-admission"
	job.OriginalCallID = "original-admission"
	job.WrapperCallID = "wrapper-admission"

	workstreamID := "ws-workstream-admission"
	taskID := "task-workstream-admission"
	conversationKey := job.ConversationKey
	if _, err := store.DB().ExecContext(t.Context(), `INSERT INTO workstreams (
		workstream_id, conversation_key, owner_actor, project, status, revision, objective, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'active', 0, 'admission objective', ?, ?)`,
		workstreamID, string(conversationKey), job.Actor, job.PrimaryProject, now.UnixNano(), now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `INSERT INTO workstream_tasks (
		workstream_id, task_id, project, description, status)
		VALUES (?, ?, ?, ?, 'proposed')`,
		workstreamID, taskID, job.PrimaryProject, "admit one durable batch"); err != nil {
		t.Fatal(err)
	}

	created, existing, err := jobs.CreateIfAbsentForWorkstream(t.Context(), job, domain.WorkstreamTaskAdmission{
		WorkstreamID: workstreamID, TaskID: taskID, ExpectedRevision: 0,
	})
	if err != nil || !created || existing != nil {
		t.Fatalf("admission created=%t existing=%#v err=%v", created, existing, err)
	}
	var status, associatedJobID, executionIdentity string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT status, job_id, execution_identity
		FROM workstream_tasks WHERE workstream_id = ? AND task_id = ?`, workstreamID, taskID).
		Scan(&status, &associatedJobID, &executionIdentity); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.TaskQueued) || associatedJobID != job.ID || executionIdentity != "" {
		t.Fatalf("task admission state = status:%q job:%q execution:%q", status, associatedJobID, executionIdentity)
	}
	var revision int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT revision FROM workstreams WHERE workstream_id = ?`, workstreamID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 1 {
		t.Fatalf("workstream revision = %d, want 1", revision)
	}
	var action string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT action FROM workstream_transitions
		WHERE workstream_id = ? AND to_revision = 1`, workstreamID).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if action != string(domain.WorkstreamActionQueueTask) {
		t.Fatalf("admission journal action = %q", action)
	}

	second := job
	second.ID = "job-workstream-admission-second"
	second.OriginalCallID = "original-admission-second"
	second.WrapperCallID = "wrapper-admission-second"
	created, existing, err = jobs.CreateIfAbsentForWorkstream(t.Context(), second, domain.WorkstreamTaskAdmission{
		WorkstreamID: workstreamID, TaskID: taskID, ExpectedRevision: 1,
	})
	if err == nil || created || existing != nil {
		t.Fatalf("duplicate admission created=%t existing=%#v err=%v", created, existing, err)
	}
	var count int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM external_agent_jobs
		WHERE job_id = ?`, second.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("duplicate job rows = %d, want 0", count)
	}
	if _, err := store.DB().ExecContext(t.Context(), `DELETE FROM external_agent_jobs WHERE job_id = ?`, job.ID); err == nil {
		t.Fatal("associated job deletion was accepted")
	}

	claimed, err := jobs.ClaimNext(t.Context(), now, "job-worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim admitted job = %#v, err=%v", claimed, err)
	}
	if err := store.DB().QueryRowContext(t.Context(), `SELECT status, execution_identity FROM workstream_tasks
		WHERE workstream_id = ? AND task_id = ?`, workstreamID, taskID).Scan(&status, &executionIdentity); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.TaskRunning) || executionIdentity != job.ID {
		t.Fatalf("task start state = status:%q execution:%q", status, executionIdentity)
	}
	if err := jobs.Transition(
		t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted,
		&domain.ExternalAgentInvocationResult{Text: "done"}, "", now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(t.Context(), `SELECT status FROM workstream_tasks
		WHERE workstream_id = ? AND task_id = ?`, workstreamID, taskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.TaskCompleted) {
		t.Fatalf("settled task status = %q, want completed", status)
	}
	if err := store.DB().QueryRowContext(t.Context(), `SELECT action FROM workstream_transitions
		WHERE workstream_id = ? AND to_revision = 3`, workstreamID).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if action != string(domain.WorkstreamActionSettleTask) {
		t.Fatalf("settlement journal action = %q", action)
	}
}

func TestMigrationV47BackfillsTaskAssociationBeforeDroppingJobColumns(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 46)
	jobID := "job-v46-bound"
	workstreamID := "ws-v46-bound"
	taskID := "task-v46-bound"
	executionIdentity := "exec-v46-bound"
	conversationKey := "slack:T12345678:dm:D12345678"
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO workstreams (
		workstream_id, conversation_key, owner_actor, project, status, revision, objective, created_at, updated_at)
		VALUES (?, ?, 'U12345678', 'workspace', 'active', 0, 'migration objective', 1, 1)`, workstreamID, conversationKey); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO workstream_tasks (
		workstream_id, task_id, project, description, status, execution_identity)
		VALUES (?, ?, 'workspace', 'legacy bound task', 'running', ?)`, workstreamID, taskID, executionIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO external_agent_jobs (
		job_id, mode, completion_policy, provider, profile, primary_project, additional_projects,
		registry_revision, task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
		conversation_key, workstream_id, task_id, execution_identity, admission_revision, status, timeout_at,
		created_at, updated_at)
		VALUES (?, 'detached', 'workstream_only', 'provider', 'profile', 'workspace', '[]', 'r1', 'task', 'request',
		'wrapper-v46', 'original-v46', 'U12345678', 'T12345678', ?, ?, ?, ?, 0, 'completed', 2, 1, 1)`,
		jobID, conversationKey, workstreamID, taskID, executionIdentity); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var associatedJobID, policy string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT job_id FROM workstream_tasks
		WHERE workstream_id = ? AND task_id = ?`, workstreamID, taskID).Scan(&associatedJobID); err != nil {
		t.Fatal(err)
	}
	if associatedJobID != jobID {
		t.Fatalf("backfilled task job = %q, want %q", associatedJobID, jobID)
	}
	if err := store.DB().QueryRowContext(t.Context(), `SELECT completion_policy FROM external_agent_jobs WHERE job_id = ?`, jobID).Scan(&policy); err != nil {
		t.Fatal(err)
	}
	if policy != string(domain.ExternalAgentCompletionDeliveryOnly) {
		t.Fatalf("migrated job policy = %q, want delivery_only", policy)
	}
	var columns int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_table_info('external_agent_jobs')
		WHERE name IN ('workstream_id', 'task_id', 'execution_identity', 'admission_revision')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatalf("migrated job still has workstream columns: %d", columns)
	}
}

func TestSchemaV47RemovesWorkstreamColumnsFromJobs(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "workstream-job-schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rows, err := store.DB().QueryContext(t.Context(), `PRAGMA table_info(external_agent_jobs)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "workstream_id", "task_id", "execution_identity", "admission_revision":
			t.Fatalf("job table still contains workstream column %q", name)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
