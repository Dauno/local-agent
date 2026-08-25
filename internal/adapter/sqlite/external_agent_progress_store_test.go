package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestJobProgressStoreRoundTrip(t *testing.T) {
	store := newProgressTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)

	_, err := store.ReadJobProgress(ctx, "missing-job")
	if err != nil {
		t.Fatalf("read missing progress: %v", err)
	}
	progress := domain.ExternalAgentJobProgress{
		JobID: "job-progress", Attempt: 1, Phase: domain.ExternalAgentPhaseToolRunning,
		LastEventKind: domain.ExternalAgentEventToolCallUpdate, LastTransportActivityAt: base.Add(time.Second),
		LastSessionUpdateAt: base.Add(time.Second), LastMeaningfulProgressAt: base.Add(time.Second),
		PromptStartedAt: base, ActiveToolCount: 1, LastToolCallID: "tool-9",
		LastToolKind: domain.ExternalAgentToolKindExecute, LastToolStatus: domain.ExternalAgentToolStatusRunning,
		PendingPermission: true, UpdatedAt: base.Add(time.Second),
	}
	if err := store.WriteJobProgress(ctx, "job-progress", "owner-1", 1, progress); err != nil {
		t.Fatalf("write progress: %v", err)
	}
	loaded, err := store.ReadJobProgress(ctx, "job-progress")
	if err != nil {
		t.Fatalf("read progress: %v", err)
	}
	if loaded == nil || loaded.Phase != domain.ExternalAgentPhaseToolRunning || loaded.LastToolCallID != "tool-9" ||
		!loaded.PendingPermission || loaded.Attempt != 1 || loaded.ActiveToolCount != 1 ||
		loaded.LastToolStatus != domain.ExternalAgentToolStatusRunning {
		t.Fatalf("loaded progress = %+v", loaded)
	}
}

func TestJobProgressStoreLeaseAttemptCAS(t *testing.T) {
	store := newProgressTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)

	progress := domain.ExternalAgentJobProgress{
		JobID: "job-cas", Attempt: 1, Phase: domain.ExternalAgentPhaseAgentProcessing, UpdatedAt: base,
	}
	if err := store.WriteJobProgress(ctx, "job-cas", "owner-1", 1, progress); err != nil {
		t.Fatalf("valid write: %v", err)
	}
	// A stale worker from an older attempt cannot overwrite the current attempt.
	stale := progress
	stale.Attempt = 0
	if err := store.WriteJobProgress(ctx, "job-cas", "owner-1", 0, stale); err == nil {
		t.Fatal("stale attempt write must be rejected")
	}
	// A wrong lease owner cannot write while the job is running.
	wrongOwner := progress
	wrongOwner.Phase = domain.ExternalAgentPhasePlanning
	if err := store.WriteJobProgress(ctx, "job-cas", "someone-else", 1, wrongOwner); err == nil {
		t.Fatal("stale lease owner write must be rejected")
	}
	// The current owner can still update.
	wrongOwner.Phase = domain.ExternalAgentPhasePlanning
	if err := store.WriteJobProgress(ctx, "job-cas", "owner-1", 1, wrongOwner); err != nil {
		t.Fatalf("current owner write: %v", err)
	}
}

func TestJobProgressStoreTerminalAcceptsFinalFlush(t *testing.T) {
	store := newProgressTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)

	progress := domain.ExternalAgentJobProgress{
		JobID: "job-terminal", Attempt: 1, Phase: domain.ExternalAgentPhaseAgentProcessing, UpdatedAt: base,
	}
	if err := store.WriteJobProgress(ctx, "job-terminal", "owner-1", 1, progress); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	// Transition the job to a terminal state and release its lease.
	now := base.Add(time.Minute)
	if err := store.Transition(ctx, "job-terminal", "owner-1", 1, domain.JobCompleted, nil, "", now); err != nil {
		t.Fatalf("transition job: %v", err)
	}
	// The final flush from the completing worker must still be accepted after
	// the lease is released, but only for the current attempt.
	terminal := progress
	terminal.Phase = domain.ExternalAgentPhaseCompleted
	terminal.StopReason = domain.ExternalAgentStopReasonEndTurn
	terminal.UpdatedAt = now
	if err := store.WriteJobProgress(ctx, "job-terminal", "owner-1", 1, terminal); err != nil {
		t.Fatalf("terminal final flush: %v", err)
	}
	stale := terminal
	stale.Attempt = 0
	if err := store.WriteJobProgress(ctx, "job-terminal", "owner-1", 0, stale); err == nil {
		t.Fatal("stale attempt final flush must be rejected")
	}
	loaded, err := store.ReadJobProgress(ctx, "job-terminal")
	if err != nil || loaded == nil || loaded.Phase != domain.ExternalAgentPhaseCompleted {
		t.Fatalf("terminal projection = %+v err=%v", loaded, err)
	}
}

func TestJobProgressStoreRejectsCrossJobWrite(t *testing.T) {
	store := newProgressTestStore(t)
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	// The caller authorizes against job-cas but writes job-inspect's payload.
	foreign := domain.ExternalAgentJobProgress{JobID: "job-inspect", Attempt: 1, Phase: domain.ExternalAgentPhaseAgentProcessing, UpdatedAt: base}
	if err := store.WriteJobProgress(context.Background(), "job-cas", "owner-1", 1, foreign); err == nil {
		t.Fatal("cross-job progress write must be rejected")
	}
}

func TestJobProgressStoreResetsAttemptScopedFields(t *testing.T) {
	store := newProgressTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	first := domain.ExternalAgentJobProgress{
		JobID: "job-cas", Attempt: 1, Phase: domain.ExternalAgentPhaseFailed,
		LastEventKind:           domain.ExternalAgentEventPromptResponse,
		LastTransportActivityAt: base, LastSessionUpdateAt: base,
		LastMeaningfulProgressAt: base, PromptStartedAt: base.Add(-time.Minute),
		ActiveToolCount: 1, LastToolCallID: "old-tool", LastToolKind: domain.ExternalAgentToolKindExecute,
		LastToolStatus: domain.ExternalAgentToolStatusRunning, ToolOverflow: true,
		PendingPermission: true, StopReason: domain.ExternalAgentStopReasonMaxTokens, UpdatedAt: base,
	}
	if err := store.WriteJobProgress(ctx, "job-cas", "owner-1", 1, first); err != nil {
		t.Fatalf("write first attempt: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE external_agent_jobs SET attempt = 2 WHERE job_id = 'job-cas'`); err != nil {
		t.Fatalf("advance job attempt: %v", err)
	}
	nextTime := base.Add(time.Hour)
	second := domain.ExternalAgentJobProgress{
		JobID: "job-cas", Attempt: 2, Phase: domain.ExternalAgentPhaseStarting,
		LastEventKind: domain.ExternalAgentEventProcessStarted, UpdatedAt: nextTime,
	}
	if err := store.WriteJobProgress(ctx, "job-cas", "owner-1", 2, second); err != nil {
		t.Fatalf("write second attempt: %v", err)
	}
	loaded, err := store.ReadJobProgress(ctx, "job-cas")
	if err != nil {
		t.Fatalf("read second attempt: %v", err)
	}
	if loaded == nil || loaded.Attempt != 2 || loaded.Phase != domain.ExternalAgentPhaseStarting ||
		!loaded.LastTransportActivityAt.IsZero() || !loaded.LastSessionUpdateAt.IsZero() ||
		!loaded.LastMeaningfulProgressAt.IsZero() || !loaded.PromptStartedAt.IsZero() ||
		loaded.ActiveToolCount != 0 || loaded.LastToolCallID != "" || loaded.LastToolKind != "" ||
		loaded.LastToolStatus != "" || loaded.ToolOverflow || loaded.PendingPermission || loaded.StopReason != "" {
		t.Fatalf("second attempt retained first-attempt state: %+v", loaded)
	}
	if loaded.CreatedAt != nextTime || loaded.UpdatedAt != nextTime {
		t.Fatalf("second attempt timestamps = %v/%v, want %v", loaded.CreatedAt, loaded.UpdatedAt, nextTime)
	}
}

func TestJobProgressStoreRejectsRecoveredJobWrites(t *testing.T) {
	store := newProgressTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Expire the lease so the job can be recovered (clears the lease owner).
	if _, err := store.db.ExecContext(ctx, `UPDATE external_agent_jobs SET lease_expiry = ? WHERE job_id = 'job-cas'`, now.Add(-time.Minute).UnixNano()); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if err := store.RecoverExpired(ctx, "job-cas", 1, 1, now, domain.JobCompletionUnknown, "completion_unknown"); err != nil {
		t.Fatalf("recover job: %v", err)
	}
	progress := domain.ExternalAgentJobProgress{JobID: "job-cas", Attempt: 1, Phase: domain.ExternalAgentPhaseFailed, UpdatedAt: now}
	if err := store.WriteJobProgress(ctx, "job-cas", "owner-1", 1, progress); err == nil {
		t.Fatal("progress write to a recovered job must be rejected")
	}
}

func TestJobProgressStoreRejectsUnknownJob(t *testing.T) {
	store := newProgressTestStore(t)
	progress := domain.ExternalAgentJobProgress{JobID: "ghost", Attempt: 1, Phase: domain.ExternalAgentPhaseStarting}
	if err := store.WriteJobProgress(context.Background(), "ghost", "owner-1", 1, progress); err == nil {
		t.Fatal("progress write for unknown job must fail")
	}
}

func TestJobInspectionIncludesSessionAndProgress(t *testing.T) {
	store := newProgressTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	const transcriptPath = "/home/operator/.codex/sessions/rollout-session.jsonl"
	if err := store.AssignExternalAgentSession(ctx, "job-inspect", "owner-1", 1, "ses_full_identity_0123456789"); err != nil {
		t.Fatalf("assign session: %v", err)
	}
	if err := store.AssignTranscriptPath(ctx, "job-inspect", "owner-1", 1, transcriptPath); err != nil {
		t.Fatalf("assign transcript: %v", err)
	}
	progress := domain.ExternalAgentJobProgress{
		JobID: "job-inspect", Attempt: 1, Phase: domain.ExternalAgentPhaseToolRunning,
		LastEventKind: domain.ExternalAgentEventToolCallUpdate, LastTransportActivityAt: base,
		LastMeaningfulProgressAt: base, PromptStartedAt: base.Add(-time.Minute),
		ActiveToolCount: 1, LastToolCallID: "tool-1", LastToolStatus: domain.ExternalAgentToolStatusRunning,
	}
	if err := store.WriteJobProgress(ctx, "job-inspect", "owner-1", 1, progress); err != nil {
		t.Fatalf("write progress: %v", err)
	}
	view, err := store.InspectJob(ctx, "job-inspect")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if view == nil || view.ExternalAgentSessionID != "ses_full_identity_0123456789" || view.TranscriptPath != transcriptPath {
		t.Fatalf("inspection session ID = %+v", view)
	}
	if view.Phase != domain.ExternalAgentPhaseToolRunning {
		t.Fatalf("inspection projection = %+v", view)
	}
}

func newProgressTestStore(t *testing.T) *ExternalAgentJobStore {
	t.Helper()
	store, _ := newTestStore(t)
	jobStore := NewExternalAgentJobStore(store)
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Minute)
	for _, jobID := range []string{"job-progress", "job-cas", "job-terminal", "job-inspect"} {
		job := domain.ExternalAgentJob{
			ID: jobID, Mode: domain.JobDetached, Provider: "opencode", Profile: "default",
			PrimaryProject: "workspace", Task: "task", Status: domain.JobRunning,
			Attempt: 1, LeaseOwner: "owner-1", LeaseExpiry: now.Add(5 * time.Minute),
			TimeoutAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
		}
		if _, err := jobStore.db.ExecContext(ctx, `INSERT INTO external_agent_jobs (
			job_id, mode, provider, profile, primary_project, additional_projects,
			registry_revision, task, request_sha256, wrapper_call_id, original_call_id,
			actor, slack_team_id, conversation_key, status, attempt, acp_session_id,
			side_effects_possible, lease_owner, lease_expiry, heartbeat_at, timeout_at,
			result_summary, result_artifact, result_sha256, result_bytes, error_code,
			status_revision, created_at, started_at, finished_at, updated_at)
			VALUES (?, ?, ?, ?, ?, '[]', 'r1', ?, 'x', '', ?, 'U1', 'T1', 'slack:T1:dm:D1', ?, 1, '',
			0, ?, ?, 0, ?, '', '', '', 0, '', 1, ?, 0, 0, ?)`,
			job.ID, job.Mode, job.Provider, job.Profile, job.PrimaryProject, job.Task, job.ID,
			job.Status, job.LeaseOwner, job.LeaseExpiry.UnixNano(), job.TimeoutAt.UnixNano(),
			now.UnixNano(), now.UnixNano()); err != nil {
			t.Fatalf("seed job %s: %v", jobID, err)
		}
	}
	return jobStore
}
