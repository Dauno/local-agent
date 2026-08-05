package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// insertIdentityJobRow inserts one job row with exactly the given result
// identity shape so the aggregate queries can be exercised content-free.
func insertIdentityJobRow(t *testing.T, db *sql.DB, id, mode, status, sha string, bytes int64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO external_agent_jobs (
		job_id, mode, provider, profile, primary_project, additional_projects, registry_revision,
		task, request_sha256, wrapper_call_id, original_call_id, actor, slack_team_id,
		conversation_key, status, result_summary, result_artifact, result_sha256, result_bytes,
		timeout_at, created_at, updated_at)
		VALUES (?, ?, 'opencode', 'build', 'workspace', '[]', 'r1',
		'task', 'request', 'wrapper', ?, 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', ?, 'result summary', '', ?, ?, 2, 1, 1)`,
		id, mode, id+"-call", status, sha, bytes); err != nil {
		t.Fatal(err)
	}
}

// insertIdentityNotificationRow inserts one notification row with the given
// notification identity shape. Empty notificationSHA yields an incomplete
// identity (legacy/unclassifiable rows keep it after v32 backfill).
func insertIdentityNotificationRow(t *testing.T, db *sql.DB, id, notificationSHA string, notificationBytes int64, terminalStatus string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO external_agent_job_notifications (
		job_id, status_revision, kind, terminal_status, canonical_markdown, content_sha256,
		renderer_version, channel_id, next_attempt_at, created_at, updated_at,
		delivery_mode, policy_version, artifact_ref, result_bytes, max_markdown_parts, upload_state,
		notification_sha256, notification_bytes, result_sha256, root_activation_required, published_at, publish_state)
		VALUES (?, 1, 'terminal', ?, 'OpenCode job `+id+` completed.', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
		'markdown_v1', 'D12345678', 1, 1, 1, 'markdown', 'legacy_v1', '', 0, 1, 'not_applicable',
		?, ?, '', 0, 1, 'published')`,
		id, terminalStatus, notificationSHA, notificationBytes); err != nil {
		t.Fatal(err)
	}
}

// insertIdentityActivationRow inserts one activation row owned by the given
// job with the given state, content byte count, and error code.
func insertIdentityActivationRow(t *testing.T, db *sql.DB, jobID, state string, contentBytes int64, errorCode string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO external_agent_job_activations (
		job_id, status_revision, kind, activation_id, terminal_status, notification_sha256,
		actor, team_id, conversation_key, original_call_id, delivery_mode, content_bytes,
		slack_message_ts, published_at, state, attempt, lease_owner, lease_expiry, next_attempt_at,
		last_error_code, response_body, response_sha256, exchange_intent_id, correlation_id,
		response_slack_ts, created_at, updated_at)
		VALUES (?, 1, 'terminal', ?, 'completed', ?, 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', ?, 'markdown', ?, '1710000000.000002', 1, ?, 1, '', 0, 0,
		?, '', '', '', '', '', 1, 1)`,
		jobID, "activation_"+jobID, strings.Repeat("a", 64), jobID+"-call", contentBytes, state, errorCode); err != nil {
		t.Fatal(err)
	}
}

// TestIdentityHealthDetectsIncompleteIdentityContentFree proves every
// aggregate field is detected from a real v32 schema without reading any
// result or notification content.
func TestIdentityHealthDetectsIncompleteIdentityContentFree(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)

	insertIdentityJobRow(t, store.DB(), "job_no_result_identity", "foreground", "completed", "", 0)
	insertIdentityJobRow(t, store.DB(), "job_healthy_inline", "detached", "completed", strings.Repeat("b", 64), 5)
	insertIdentityJobRow(t, store.DB(), "job_foreground_active", "foreground", "completed", strings.Repeat("c", 64), 5)
	insertIdentityJobRow(t, store.DB(), "job_retired_owner", "foreground", "completed", strings.Repeat("d", 64), 5)
	insertIdentityJobRow(t, store.DB(), "job_healthy_activation", "detached", "completed", strings.Repeat("e", 64), 5)
	insertIdentityJobRow(t, store.DB(), "job_not_terminal", "foreground", "running", "", 0)

	insertIdentityNotificationRow(t, store.DB(), "job_healthy_inline", strings.Repeat("f", 64), 17, "completed")
	insertIdentityNotificationRow(t, store.DB(), "job_no_result_identity", "", 0, "completed")

	insertIdentityActivationRow(t, store.DB(), "job_foreground_active", "pending", 12, "")
	insertIdentityActivationRow(t, store.DB(), "job_retired_owner", "failed", 12, domain.ActivationForegroundRetiredCode)
	insertIdentityActivationRow(t, store.DB(), "job_healthy_activation", "pending", 0, "")

	health, err := jobs.IdentityHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ExternalAgentJobIdentityHealth{
		JobsCompletedWithoutResultIdentity: 1,
		NotificationsWithoutIdentity:       1,
		ActivationsWithoutContent:          1,
		ForegroundActivationsActive:        1,
		RetiredForegroundActivations:       1,
	}
	if health != want {
		t.Fatalf("identity health = %+v, want %+v", health, want)
	}

	// The retired and healthy rows must never surface as content-free counts
	// from a different bucket: only the seeded defect rows are counted.
	if health.RetiredForegroundActivations != 1 || health.ActivationsWithoutContent != 1 {
		t.Fatalf("buckets misclassified: %+v", health)
	}
}

// TestActivationHealthStuckExcludesRetiredForegroundActivations proves the
// stuck count never includes terminal retired rows and keeps counting
// genuinely overdue non-terminal rows.
func TestActivationHealthStuckExcludesRetiredForegroundActivations(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "stuck.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)

	now := time.Now().UTC().Truncate(time.Second)
	insertIdentityJobRow(t, store.DB(), "job_overdue", "foreground", "completed", strings.Repeat("a", 64), 5)
	insertIdentityJobRow(t, store.DB(), "job_retired", "foreground", "completed", strings.Repeat("b", 64), 5)
	insertIdentityActivationRow(t, store.DB(), "job_overdue", "pending", 5, "")
	insertIdentityActivationRow(t, store.DB(), "job_retired", "failed", 5, domain.ActivationForegroundRetiredCode)
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE external_agent_job_activations
		SET next_attempt_at = ? WHERE job_id = ?`, now.UnixNano(), "job_overdue"); err != nil {
		t.Fatal(err)
	}

	health, err := jobs.ActivationHealth(t.Context(), now.Add(10*time.Minute), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if health.Stuck != 1 {
		t.Fatalf("stuck = %d, want 1 (only the overdue non-terminal activation)", health.Stuck)
	}
	identity, err := jobs.IdentityHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if identity.RetiredForegroundActivations != 1 {
		t.Fatalf("retired = %d, want 1", identity.RetiredForegroundActivations)
	}
	if identity.ForegroundActivationsActive != 1 {
		t.Fatalf("active foreground = %d, want 1", identity.ForegroundActivationsActive)
	}
}

// TestIdentityHealthNeverExposesRowValues pins the content-free contract: the
// aggregate only ever returns counts, never job IDs, actor, conversation,
// digests, or content.
func TestIdentityHealthNeverExposesRowValues(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "values.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)

	insertIdentityJobRow(t, store.DB(), "job_leak_1", "detached", "completed", "", 0)
	insertIdentityNotificationRow(t, store.DB(), "job_leak_1", "", 0, "completed")
	insertIdentityActivationRow(t, store.DB(), "job_leak_1", "pending", 0, "")

	health, err := jobs.IdentityHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rendered := fmt.Sprintf("%+v", health)
	for _, forbidden := range []string{
		"job_leak_1", "U12345678", "T12345678", "D12345678", "slack:", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "result summary",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("identity health leaked %q in %q", forbidden, rendered)
		}
	}
}
