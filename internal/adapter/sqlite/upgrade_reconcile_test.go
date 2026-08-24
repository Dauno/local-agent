package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	slackadapter "github.com/Dauno/slack-local-agent/internal/adapter/slack"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

// insertV30CR1Notification seeds a v30 notification row exactly as the v30
// binary left it when Slack accepted the delivery but the local CAS never
// completed: the result digest is stored as content_sha256, the row is stuck
// in publishing (stale lease) or unknown, and the Slack evidence was already
// emitted under the v30 metadata contract.
func insertV30CR1Notification(t *testing.T, db *sql.DB, id, terminalStatus, markdown, contentSHA string, policy, deliveryMode, artifactRef string, resultBytes int64, publishState, slackFileID string) {
	t.Helper()
	uploadState := "not_applicable"
	if deliveryMode == "file" {
		uploadState = "completed"
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO external_agent_job_notifications (
		job_id, status_revision, kind, terminal_status, canonical_markdown, content_sha256,
		renderer_version, channel_id, next_attempt_at, created_at, updated_at,
		delivery_mode, policy_version, artifact_ref, result_bytes, max_markdown_parts, upload_state, published_at,
		publish_state, lease_owner, lease_expiry, attempts, last_error_code, slack_file_id)
		VALUES (?, 1, 'terminal', ?, ?, ?, 'markdown_v1', 'D12345678', 1, 1, 1, ?, ?, ?, ?, 6, ?, 0, ?, '', 1, 0, '', ?)`,
		id, terminalStatus, markdown, contentSHA, deliveryMode, policy, artifactRef, resultBytes, uploadState, publishState, slackFileID); err != nil {
		t.Fatal(err)
	}
}

// v30EraEvidencePayload reproduces the Slack metadata the v30 binary published:
// the result digest in both notification_sha256 and content_sha256, with none
// of the v32 metadata fields (notification_bytes, result_sha256).
func v30EraEvidencePayload(jobID string, revision int, markdown, resultDigest string, mode, fileID string) map[string]any {
	parts := slackadapter.RenderMarkdownParts(markdown, false)
	payload := map[string]any{
		"job_id": jobID, "status_revision": revision, "kind": domain.JobNotificationTerminal,
		"renderer_version": domain.JobNotificationRenderer, "delivery_mode": mode, "policy_version": domain.JobDeliveryPolicyV1,
		"notification_sha256": resultDigest, "content_sha256": resultDigest,
		"result_bytes": len([]byte("safe result")), "max_markdown_parts": 6,
		"upload_state": "not_applicable", "part_sha256": contentSHA256Hex(parts[0]),
		"part_index": 1, "part_count": 1,
	}
	if mode == "file" {
		payload["upload_state"] = "completed"
		payload["file_id"] = fileID
		payload["result_bytes"] = len([]byte("file bytes"))
	}
	return payload
}

// v32EraEvidencePayload reproduces the Slack metadata the v32 binary publishes:
// the canonical-Markdown digest as notification_sha256 plus the v32 metadata
// fields.
func v32EraEvidencePayload(jobID string, revision int, markdown, resultDigest, notificationDigest string, mode, fileID string) map[string]any {
	payload := v30EraEvidencePayload(jobID, revision, markdown, resultDigest, mode, fileID)
	payload["notification_sha256"] = notificationDigest
	payload["notification_bytes"] = int64(len([]byte(markdown)))
	payload["result_sha256"] = resultDigest
	return payload
}

func contentSHA256Hex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest)
}

// upgradeV30Database closes the v30 fixture database, applies the real
// v30 -> v31 -> v32 upgrade chain, and returns the opened store.
func upgradeV30Database(t *testing.T, path string, raw *sql.DB) *Store {
	t.Helper()
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("OpenExisting v30: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("upgraded schema version = %d, want %d", version, SchemaVersion)
	}
	return store
}

func countActivations(t *testing.T, store *Store, detached bool) int {
	t.Helper()
	var count int
	query := `SELECT COUNT(*) FROM external_agent_job_activations a JOIN external_agent_jobs j ON j.job_id = a.job_id`
	if detached {
		query += ` WHERE j.mode = 'detached'`
	} else {
		query += ` WHERE j.mode = 'foreground'`
	}
	if err := store.db.QueryRowContext(context.Background(), query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestUpgradeV30ToV32ReconcilesPreV32MarkdownEvidence reproduces the CR1
// scenario for Markdown delivery: a v30 database holds a detached delivery_v1
// notification stuck in publishing whose Slack message was accepted with the
// result digest as notification_sha256. After the real v30->v31->v32 upgrade,
// Reconcile must classify the delivery as published. The historical row has no
// v37 completion binding, so publication remains audit/delivery only.
func TestUpgradeV30ToV32ReconcilesPreV32MarkdownEvidence(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 30)
	markdown := "OpenCode job `upgrade-markdown` completed.\n\nsafe result"
	resultContent := "safe result"
	resultDigest := contentSHA256Hex(resultContent)
	insertV30JobRow(t, raw, "upgrade-markdown", "detached", "completed", "", "", resultDigest, int64(len(resultContent)))
	insertV30CR1Notification(t, raw, "upgrade-markdown", "completed", markdown, resultDigest, "delivery_v1", "markdown", "", int64(len(resultContent)), "publishing", "")

	store := upgradeV30Database(t, path, raw)
	jobStore := NewExternalAgentJobStore(store)
	evidence := slackapi.Message{Msg: slackapi.Msg{
		User: "B12345678", Timestamp: "1710000000.000001",
		Metadata: slackapi.SlackMetadata{EventType: "local_agent_external_agent_job", EventPayload: v30EraEvidencePayload("upgrade-markdown", 1, markdown, resultDigest, "markdown", "")},
	}}
	server := newSlackHistoryServer(t, evidence)

	claimed, err := jobStore.ClaimNextNotification(ctx, time.Now().UTC(), "test-worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim notification = %#v, err=%v", claimed, err)
	}
	if !claimed.NeedsReconciliation {
		t.Fatal("claimed pre-v32 delivery is not flagged for reconciliation")
	}
	ts, found, err := slackadapter.NewDurableJobNotificationPublisher(nil, slackadapter.NewHistoryReader(slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.BaseURL())), "B12345678", time.Second, nil, false), nil, nil, jobStore, nil).Reconcile(ctx, *claimed)
	if err != nil || !found || ts != evidence.Timestamp {
		t.Fatalf("pre-v32 Markdown reconcile = %q, found=%v, err=%v", ts, found, err)
	}
	if err := jobStore.MarkNotificationPublished(ctx, claimed, ts, time.Now().UTC()); err != nil {
		t.Fatalf("mark published: %v", err)
	}
	assertPublishedDelivery(t, store, "upgrade-markdown", ts)
	if got := countActivations(t, store, true); got != 0 {
		t.Fatalf("historical detached activations = %d, want 0", got)
	}
	if got := countActivations(t, store, false); got != 0 {
		t.Fatalf("foreground activations = %d, want 0", got)
	}
}

// TestUpgradeV30ToV32ReconcilesPreV32FileEvidence reproduces the CR1 scenario
// for file delivery: the v30 binary completed the external upload, posted the
// status message with the legacy metadata, and crashed before the local CAS.
// After the upgrade, Reconcile must recover the delivery without manufacturing
// a new activation for the unbound historical row.
func TestUpgradeV30ToV32ReconcilesPreV32FileEvidence(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 30)
	markdown := "OpenCode job `upgrade-file` completed. The complete result was attached."
	resultContent := "file bytes"
	resultDigest := contentSHA256Hex(resultContent)
	insertV30JobRow(t, raw, "upgrade-file", "detached", "completed", "", "", resultDigest, int64(len(resultContent)))
	insertV30CR1Notification(t, raw, "upgrade-file", "completed", markdown, resultDigest, "delivery_v1", "file", "upgrade-file-delivery.result", int64(len(resultContent)), "unknown", "F123")

	store := upgradeV30Database(t, path, raw)
	jobStore := NewExternalAgentJobStore(store)
	evidence := slackapi.Message{Msg: slackapi.Msg{
		User: "B12345678", Timestamp: "1710000000.000001",
		Metadata: slackapi.SlackMetadata{EventType: "local_agent_external_agent_job", EventPayload: v30EraEvidencePayload("upgrade-file", 1, markdown, resultDigest, "file", "F123")},
	}}
	server := newSlackHistoryServer(t, evidence)
	server.fileInfo = `{"ok":true,"file":{"id":"F123","name":"opencode-upgrade-file.md","size":` + fmt.Sprint(len(resultContent)) + `,"user":"B12345678","channels":["D12345678"]}}`
	fileClient := slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.BaseURL()))

	claimed, err := jobStore.ClaimNextNotification(ctx, time.Now().UTC(), "test-worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim notification = %#v, err=%v", claimed, err)
	}
	if !claimed.NeedsReconciliation {
		t.Fatal("claimed pre-v32 file delivery is not flagged for reconciliation")
	}
	ts, found, err := slackadapter.NewDurableJobNotificationPublisher(nil, slackadapter.NewHistoryReader(fileClient, "B12345678", time.Second, nil, false), nil, nil, jobStore, fileClient).Reconcile(ctx, *claimed)
	if err != nil || !found || ts != evidence.Timestamp {
		t.Fatalf("pre-v32 file reconcile = %q, found=%v, err=%v", ts, found, err)
	}
	if err := jobStore.MarkNotificationPublished(ctx, claimed, ts, time.Now().UTC()); err != nil {
		t.Fatalf("mark published: %v", err)
	}
	assertPublishedDelivery(t, store, "upgrade-file", ts)
	if got := countActivations(t, store, true); got != 0 {
		t.Fatalf("historical detached activations = %d, want 0", got)
	}
	if got := countActivations(t, store, false); got != 0 {
		t.Fatalf("foreground activations = %d, want 0", got)
	}
}

// TestUpgradeV30ToV32V32EvidenceStillVerifiedStrictly proves the new-contract
// verification is unchanged after the CR1 fix: a complete v32 metadata payload
// reconciles, while a v32 payload with a mismatched notification digest fails
// closed even when its content_sha256 matches the legacy content identity.
func TestUpgradeV30ToV32V32EvidenceStillVerifiedStrictly(t *testing.T) {
	markdown := "OpenCode job `upgrade-strict` completed.\n\nsafe result"
	resultContent := "safe result"
	resultDigest := contentSHA256Hex(resultContent)
	markdownDigest := contentSHA256Hex(markdown)
	for _, scenario := range []struct {
		name              string
		tampered          bool
		wantFound         bool
		wantError         bool
		wantOneActivation bool
	}{
		{name: "complete_v32_metadata", wantFound: true, wantOneActivation: false},
		{name: "tampered_v32_digest", tampered: true, wantError: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			path, raw := createSchemaAtVersion(t, 30)
			insertV30JobRow(t, raw, "upgrade-strict", "detached", "completed", "", "", resultDigest, int64(len(resultContent)))
			insertV30CR1Notification(t, raw, "upgrade-strict", "completed", markdown, resultDigest, "delivery_v1", "markdown", "", int64(len(resultContent)), "unknown", "")
			store := upgradeV30Database(t, path, raw)
			jobStore := NewExternalAgentJobStore(store)

			notificationDigest := markdownDigest
			if scenario.tampered {
				notificationDigest = strings.Repeat("a", 64)
			}
			evidence := slackapi.Message{Msg: slackapi.Msg{
				User: "B12345678", Timestamp: "1710000000.000001",
				Metadata: slackapi.SlackMetadata{EventType: "local_agent_external_agent_job", EventPayload: v32EraEvidencePayload("upgrade-strict", 1, markdown, resultDigest, notificationDigest, "markdown", "")},
			}}
			server := newSlackHistoryServer(t, evidence)

			claimed, err := jobStore.ClaimNextNotification(ctx, time.Now().UTC(), "test-worker", time.Minute)
			if err != nil || claimed == nil {
				t.Fatalf("claim notification = %#v, err=%v", claimed, err)
			}
			ts, found, err := slackadapter.NewDurableJobNotificationPublisher(nil, slackadapter.NewHistoryReader(slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.BaseURL())), "B12345678", time.Second, nil, false), nil, nil, jobStore, nil).Reconcile(ctx, *claimed)
			if scenario.wantError {
				if err == nil || found {
					t.Fatalf("tampered v32 evidence found=%v err=%v, want fail-closed", found, err)
				}
				if got := countActivations(t, store, true); got != 0 {
					t.Fatalf("activations after rejected evidence = %d, want 0", got)
				}
				return
			}
			if err != nil || !found || ts != evidence.Timestamp {
				t.Fatalf("strict v32 reconcile = %q, found=%v, err=%v", ts, found, err)
			}
			if err := jobStore.MarkNotificationPublished(ctx, claimed, ts, time.Now().UTC()); err != nil {
				t.Fatalf("mark published: %v", err)
			}
			assertPublishedDelivery(t, store, "upgrade-strict", ts)
			wantActivations := 0
			if scenario.wantOneActivation {
				wantActivations = 1
			}
			if got := countActivations(t, store, true); got != wantActivations {
				t.Fatalf("detached activations = %d, want %d", got, wantActivations)
			}
			if got := countActivations(t, store, false); got != 0 {
				t.Fatalf("foreground activations = %d, want 0", got)
			}
		})
	}
}

func newSlackHistoryServer(t *testing.T, messages ...slackapi.Message) *slackHistoryServer {
	t.Helper()
	server := &slackHistoryServer{messages: messages}
	server.server = httptest.NewServer(http.HandlerFunc(server.serve))
	t.Cleanup(server.server.Close)
	return server
}

type slackHistoryServer struct {
	server   *httptest.Server
	messages []slackapi.Message
	fileInfo string
}

func (s *slackHistoryServer) BaseURL() string {
	return s.server.URL + "/"
}

func (s *slackHistoryServer) serve(w http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/conversations.history":
		encoded, err := json.Marshal(s.messages)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"messages":%s}`, encoded)
	case "/files.info":
		if s.fileInfo == "" {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, s.fileInfo)
	default:
		http.NotFound(w, request)
	}
}

func assertPublishedDelivery(t *testing.T, store *Store, jobID, wantTS string) {
	t.Helper()
	var state, recovered string
	if err := store.db.QueryRowContext(context.Background(), `SELECT publish_state, recovered_slack_ts FROM external_agent_job_notifications WHERE job_id = ?`, jobID).Scan(&state, &recovered); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.NotificationPublished) || recovered != wantTS {
		t.Fatalf("delivery after publication = %q/%q", state, recovered)
	}
}

func assertActivationIdentity(t *testing.T, store *Store, jobID, deliveryMode, notificationDigest string, contentBytes int64) {
	t.Helper()
	var activationID, terminal, mode string
	var bytes int64
	if err := store.db.QueryRowContext(context.Background(), `SELECT activation_id, terminal_status, delivery_mode, content_bytes
		FROM external_agent_job_activations WHERE job_id = ?`, jobID).Scan(&activationID, &terminal, &mode, &bytes); err != nil {
		t.Fatal(err)
	}
	wantID := domain.ExternalAgentJobActivationID(jobID, 1, domain.JobNotificationTerminal)
	if activationID != wantID || terminal != "completed" || mode != deliveryMode || bytes != contentBytes {
		t.Fatalf("activation identity = %q/%q/%q/%d, want %q/completed/%q/%d", activationID, terminal, mode, bytes, wantID, deliveryMode, contentBytes)
	}
	if bytes <= 0 {
		t.Fatal("activation content bytes are not positive")
	}
}
