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
	resultSHA := ""
	resultBytes := int64(0)
	if terminalStatus == "completed" {
		resultSHA = strings.Repeat("b", 64)
		resultBytes = 1
	}
	insertIdentityNotificationRowWithResult(t, db, id, notificationSHA, notificationBytes, terminalStatus, resultSHA, resultBytes)
}

func insertIdentityNotificationRowWithResult(t *testing.T, db *sql.DB, id, notificationSHA string, notificationBytes int64, terminalStatus, resultSHA string, resultBytes int64) {
	insertIdentityNotificationRowWithResultAtRevision(t, db, id, notificationSHA, notificationBytes, terminalStatus, resultSHA, resultBytes, 1)
}

// insertIdentityNotificationRowWithResultAtRevision is the revision-scoped
// variant used to prove that legacy provenance markers bind to the exact
// (job_id, status_revision) of a notification row.
func insertIdentityNotificationRowWithResultAtRevision(
	t *testing.T,
	db *sql.DB,
	id, notificationSHA string,
	notificationBytes int64,
	terminalStatus, resultSHA string,
	resultBytes int64,
	revision int,
) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO external_agent_job_notifications (
		job_id, status_revision, kind, terminal_status, canonical_markdown, content_sha256,
		renderer_version, channel_id, next_attempt_at, created_at, updated_at,
		delivery_mode, policy_version, artifact_ref, result_bytes, max_markdown_parts, upload_state,
		notification_sha256, notification_bytes, result_sha256, root_activation_required, published_at, publish_state)
		VALUES (?, ?, 'terminal', ?, 'OpenCode job `+id+` completed.', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
		'markdown_v1', 'D12345678', 1, 1, 1, 'markdown', 'legacy_v1', '', ?, 1, 'not_applicable',
		?, ?, ?, 0, 1, 'published')`,
		id, revision, terminalStatus, resultBytes, notificationSHA, notificationBytes, resultSHA); err != nil {
		t.Fatal(err)
	}
}

// insertIdentityLegacyEvent inserts one legacy provenance marker on the exact
// (job_id, status_revision) event key.
func insertIdentityLegacyEvent(t *testing.T, db *sql.DB, id string, revision int) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO external_agent_job_events
		(job_id, status_revision, event_kind, created_at)
		VALUES (?, ?, ?, 1)`, id, revision, legacyResultIdentityEvent); err != nil {
		t.Fatal(err)
	}
}

// insertIdentityActivationRow inserts one completed activation row owned by
// the given job with the given state, content byte count, and error code.
func insertIdentityActivationRow(t *testing.T, db *sql.DB, jobID, state string, contentBytes int64, errorCode string) {
	insertIdentityActivationRowWithTerminalStatus(t, db, jobID, state, contentBytes, errorCode, "completed")
}

func insertIdentityActivationRowWithTerminalStatus(t *testing.T, db *sql.DB, jobID, state string, contentBytes int64, errorCode, terminalStatus string) {
	insertIdentityActivationRowWithTerminalStatusAndSHA(t, db, jobID, state, contentBytes, errorCode, terminalStatus, strings.Repeat("a", 64))
}

func insertIdentityActivationRowWithTerminalStatusAndSHA(t *testing.T, db *sql.DB, jobID, state string, contentBytes int64, errorCode, terminalStatus, notificationSHA string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO external_agent_job_activations (
		job_id, status_revision, kind, activation_id, terminal_status, notification_sha256,
		actor, team_id, conversation_key, original_call_id, delivery_mode, content_bytes,
		slack_message_ts, published_at, state, attempt, lease_owner, lease_expiry, next_attempt_at,
		last_error_code, response_body, response_sha256, exchange_intent_id, correlation_id,
		response_slack_ts, created_at, updated_at)
		VALUES (?, 1, 'terminal', ?, ?, ?, 'U12345678', 'T12345678',
		'slack:T12345678:dm:D12345678', ?, 'markdown', ?, '1710000000.000002', 1, ?, 1, '', 0, 0,
		?, '', '', '', '', '', 1, 1)`,
		jobID, "activation_"+jobID, terminalStatus, notificationSHA, jobID+"-call", contentBytes, state, errorCode); err != nil {
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
	for _, terminalStatus := range []string{"failed", "cancelled", "completion_unknown", "abandoned"} {
		jobID := "job_detached_" + terminalStatus
		insertIdentityJobRow(t, store.DB(), jobID, "detached", terminalStatus, "", 0)
		insertIdentityActivationRowWithTerminalStatus(t, store.DB(), jobID, "failed", 0, "", terminalStatus)
	}

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

func TestIdentityHealthCompletedActivationWithoutContentIsCorrupt(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "completed-empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	insertIdentityJobRow(t, store.DB(), "job_completed_empty", "detached", "completed", strings.Repeat("a", 64), 1)
	insertIdentityActivationRow(t, store.DB(), "job_completed_empty", "failed", 0, "")

	health, err := jobs.IdentityHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.ActivationsWithoutContent != 1 {
		t.Fatalf("activations without content = %d, want 1", health.ActivationsWithoutContent)
	}
}

func TestIdentityHealthRejectsNonHexDigestShapes(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "hex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	cases := []struct {
		name string
		sha  string
	}{
		{name: "non-hex", sha: strings.Repeat("g", 64)},
		{name: "uppercase", sha: strings.Repeat("A", 64)},
		{name: "wrong length", sha: strings.Repeat("a", 63)},
	}
	for _, test := range cases {
		jobID := "job_hex_" + strings.ReplaceAll(test.name, " ", "_")
		insertIdentityJobRow(t, store.DB(), jobID, "detached", "completed", test.sha, 1)
		insertIdentityNotificationRow(t, store.DB(), jobID, strings.Repeat("c", 64), 1, "failed")
	}

	health, err := jobs.IdentityHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.JobsCompletedWithoutResultIdentity != len(cases) {
		t.Fatalf("current incomplete jobs = %d, want %d", health.JobsCompletedWithoutResultIdentity, len(cases))
	}
}

func TestIdentityHealthRejectsInvalidNotificationAndActivationDigests(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "delivery-hex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	invalidDigests := []string{strings.Repeat("G", 64), strings.Repeat("A", 64), strings.Repeat("a", 63)}
	for i, digest := range invalidDigests {
		notificationJobID := fmt.Sprintf("job_invalid_notification_%d", i)
		insertIdentityJobRow(t, store.DB(), notificationJobID, "detached", "failed", "", 0)
		insertIdentityNotificationRow(t, store.DB(), notificationJobID, digest, 1, "failed")
	}
	for i, digest := range invalidDigests[:2] {
		activationJobID := fmt.Sprintf("job_invalid_activation_%d", i)
		insertIdentityJobRow(t, store.DB(), activationJobID, "detached", "failed", "", 0)
		insertIdentityActivationRowWithTerminalStatusAndSHA(t, store.DB(), activationJobID, "failed", 0, "", "failed", digest)
	}
	insertIdentityJobRow(t, store.DB(), "job_invalid_notification_result", "detached", "failed", "", 0)
	insertIdentityNotificationRowWithResult(t, store.DB(), "job_invalid_notification_result", strings.Repeat("c", 64), 1, "failed", strings.Repeat("G", 64), 1)

	health, err := jobs.IdentityHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.NotificationsWithoutIdentity != len(invalidDigests)+1 {
		t.Fatalf("notifications without identity = %d, want %d", health.NotificationsWithoutIdentity, len(invalidDigests)+1)
	}
	if health.ActivationsWithoutIdentity != len(invalidDigests[:2]) {
		t.Fatalf("activations without identity = %d, want %d", health.ActivationsWithoutIdentity, len(invalidDigests[:2]))
	}
}

// TestIdentityHealthNotificationLegacyMarkerIsRevisionScoped proves a legacy
// provenance marker exonerates only the completed notification row of its own
// status revision: a completed notification of another revision with an
// incomplete result identity still counts as a defect, and the marker never
// hides a healthy row.
func TestIdentityHealthNotificationLegacyMarkerIsRevisionScoped(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "legacy-revision.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)

	completeSHA := strings.Repeat("f", 64)
	for _, id := range []string{
		"job_legacy_other_revision", "job_legacy_same_revision", "job_legacy_healthy",
	} {
		insertIdentityJobRow(t, store.DB(), id, "detached", "completed", strings.Repeat("e", 64), 5)
	}
	// Every notification has a complete notification identity but an empty
	// result identity: only the legacy marker decides their disposition.
	insertIdentityNotificationRowWithResult(t, store.DB(), "job_legacy_other_revision", completeSHA, 17, "completed", "", 0)
	insertIdentityNotificationRowWithResult(t, store.DB(), "job_legacy_same_revision", completeSHA, 17, "completed", "", 0)
	insertIdentityNotificationRowWithResult(t, store.DB(), "job_legacy_healthy", completeSHA, 17, "completed", strings.Repeat("b", 64), 1)

	// A marker on a different revision must never exonerate the revision-1 row.
	insertIdentityLegacyEvent(t, store.DB(), "job_legacy_other_revision", 2)
	// A marker on the exact row revision exonerates it; a healthy row stays
	// healthy regardless of the marker.
	insertIdentityLegacyEvent(t, store.DB(), "job_legacy_same_revision", 1)
	insertIdentityLegacyEvent(t, store.DB(), "job_legacy_healthy", 1)

	health, err := jobs.IdentityHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.NotificationsWithoutIdentity != 1 {
		t.Fatalf("notifications without identity = %d, want 1 (only the other-revision row)", health.NotificationsWithoutIdentity)
	}
	if health.JobsCompletedWithoutResultIdentity != 0 || health.JobsCompletedWithoutResultIdentityLegacy != 0 {
		t.Fatalf("job buckets must stay clean: %+v", health)
	}
}

// TestIdentityHealthRejectsNULSuffixedDigests proves the digest shape
// predicates are octet-safe: SQLite length() and GLOB stop at the first NUL
// byte, so a digest followed by a NUL suffix (or with a NUL inside) must still
// count as a defect exactly as the Go readers classify it. Clean lowercase
// digests stay healthy.
func TestIdentityHealthRejectsNULSuffixedDigests(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "nul.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := NewExternalAgentJobStore(store)
	clean := strings.Repeat("a", 64)
	nulSuffix := clean + "\x00x"
	nulInside := strings.Repeat("a", 32) + "\x00" + strings.Repeat("b", 31)
	for _, digest := range []string{nulSuffix, nulInside} {
		jobID := "job_nul_" + fmt.Sprintf("%x", len(digest))
		insertIdentityJobRow(t, store.DB(), jobID, "detached", "completed", digest, 1)
		insertIdentityNotificationRow(t, store.DB(), jobID, digest, 1, "completed")
	}
	// The v29 activation CHECK (length(notification_sha256) = 64) already
	// rejects a digest with a NUL inside (SQLite length stops at the NUL), so
	// only the NUL-suffixed digest is storable in an activation row; it must
	// still surface as an identity defect in doctor.
	insertIdentityActivationRowWithTerminalStatusAndSHA(t, store.DB(), "job_nul_"+fmt.Sprintf("%x", len(nulSuffix)), "failed", 0, "", "failed", nulSuffix)
	insertIdentityJobRow(t, store.DB(), "job_nul_clean", "detached", "completed", clean, 1)
	insertIdentityNotificationRow(t, store.DB(), "job_nul_clean", clean, 1, "completed")

	health, err := jobs.IdentityHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.JobsCompletedWithoutResultIdentity != 2 {
		t.Fatalf("current incomplete jobs = %d, want 2", health.JobsCompletedWithoutResultIdentity)
	}
	if health.NotificationsWithoutIdentity != 2 {
		t.Fatalf("notifications without identity = %d, want 2", health.NotificationsWithoutIdentity)
	}
	if health.ActivationsWithoutIdentity != 1 {
		t.Fatalf("activations without identity = %d, want 1", health.ActivationsWithoutIdentity)
	}
}

// TestV32RouteTriggerRejectsNULSuffixedDigest proves the completion-route
// trigger uses the same octet-safe digest length as the health predicates: a
// root activation requiring a NUL-suffixed notification digest is rejected,
// while a clean lowercase digest with a terminal status is accepted.
func TestV32RouteTriggerRejectsNULSuffixedDigest(t *testing.T) {
	store, err := Initialize(context.Background(), filepath.Join(t.TempDir(), "route-nul.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	insertIdentityJobRow(t, store.DB(), "route-nul-job", "detached", "completed", strings.Repeat("a", 64), 1)
	insert := func(digest string) error {
		_, err := store.DB().ExecContext(t.Context(), `INSERT INTO external_agent_job_notifications (
			job_id, status_revision, kind, terminal_status, canonical_markdown, content_sha256, renderer_version,
			channel_id, next_attempt_at, created_at, updated_at, root_activation_required,
			notification_sha256, notification_bytes)
			VALUES ('route-nul-job', 1, 'terminal', 'completed', 'OpenCode job route-nul-job completed.',
			'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'markdown_v1',
			'D12345678', 1, 1, 1, 1, ?, 1)`, digest)
		return err
	}
	if err := insert(strings.Repeat("a", 64) + "\x00x"); err == nil {
		t.Fatal("completion route accepted a NUL-suffixed notification digest")
	}
	if err := insert(strings.Repeat("a", 32) + "\x00" + strings.Repeat("b", 31)); err == nil {
		t.Fatal("completion route accepted a NUL-padded notification digest")
	}
	if err := insert(strings.Repeat("a", 64)); err != nil {
		t.Fatalf("completion route rejected a clean notification digest: %v", err)
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
