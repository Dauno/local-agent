package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/adapter/fsartifact"
	slackadapter "github.com/Dauno/slack-local-agent/internal/adapter/slack"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	externalagent "github.com/Dauno/slack-local-agent/internal/usecase/externalagent"
)

func TestDetachedJobCompletionPublishesDurableSlackDelivery(t *testing.T) {
	type requestRecord struct {
		Channel  string
		ThreadTS string
		Metadata map[string]any
	}
	var (
		mu       sync.Mutex
		requests []requestRecord
	)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat.postMessage" {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		if err := request.ParseForm(); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		var metadata map[string]any
		if err := json.Unmarshal([]byte(request.FormValue("metadata")), &metadata); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, requestRecord{Channel: request.FormValue("channel"), ThreadTS: request.FormValue("thread_ts"), Metadata: metadata})
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(response, `{"ok":true,"channel":"D12345678","ts":"1710000000.000001"}`)
	}))
	t.Cleanup(server.Close)

	database := filepath.Join(t.TempDir(), "local-agent.db")
	store, err := adaptersqlite.Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := adaptersqlite.NewExternalAgentJobStore(store)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	job := domain.ExternalAgentJob{
		ID: "job_integration_1", Mode: domain.JobDetached, Provider: "agentcli", Profile: "build",
		PrimaryProject: "workspace", RegistryRevision: "r1", Task: "task stays out of delivery logs",
		Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678:thread:1710000000.000001",
		Status: domain.JobQueued, TimeoutAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	job.RequestSHA256 = domain.ExternalAgentJobRequestDigest(domain.ExternalAgentJobRequest{
		Provider: job.Provider, Profile: job.Profile, PrimaryProject: job.PrimaryProject, RegistryRevision: job.RegistryRevision,
		Task: job.Task, Mode: job.Mode, Actor: job.Actor, TeamID: job.TeamID, ConversationKey: job.ConversationKey,
	})
	created, _, err := jobs.CreateIfAbsent(t.Context(), job)
	if err != nil || !created {
		t.Fatalf("create job = %v, err=%v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "execution-worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim job = %#v, err=%v", claimed, err)
	}
	content := "durable result without a user follow-up"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	result := &domain.ExternalAgentInvocationResult{
		Text: content, ResultSHA256: digest, ResultBytes: int64(len(content)), DeliveryMode: domain.JobResultDeliveryMarkdown,
		DeliveryPolicyVersion: domain.JobDeliveryPolicyV1, DeliveryContentSHA256: digest,
		DeliveryContentBytes: int64(len(content)), DeliveryMaxMarkdownParts: 6,
		DeliveryCanonicalMarkdown: "External-agent job `job_integration_1` completed.\n\n" + content,
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, result, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var outboxCount int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM external_agent_job_notifications WHERE job_id = ?`, job.ID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("outbox rows before publish = %d, want 1", outboxCount)
	}

	client := slackapi.New("xoxb-local-test", slackapi.OptionAPIURL(server.URL+"/"))
	publisher := slackadapter.NewPublisher(client, time.Second, integrationLogger{}, false)
	artifacts, err := fsartifact.New(filepath.Join(t.TempDir(), "artifacts"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	service, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute,
		PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1,
	}, externalagent.Dependencies{Store: jobs, Runtime: &integrationJobRuntime{}, Artifacts: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	history := slackadapter.NewHistoryReader(client, "B12345678", time.Second, integrationLogger{}, false)
	uploader := slackadapter.NewGeneratedFileUploader(client, time.Second)
	notificationPublisher := slackadapter.NewDurableJobNotificationPublisher(publisher, history, uploader, artifacts, jobs, client, false)
	worker, err := externalagent.NewNotificationWorker(externalagent.NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Minute}, externalagent.NotificationDependencies{
		Store: jobs, Publisher: notificationPublisher, HostCompleter: service,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}

	inspection, err := jobs.InspectJob(t.Context(), job.ID)
	if err != nil || inspection == nil || len(inspection.Deliveries) != 1 {
		t.Fatalf("inspection = %#v, err=%v", inspection, err)
	}
	delivery := inspection.Deliveries[0]
	if delivery.PublishState != domain.NotificationPublished || delivery.RecoveredSlackTS == "" || delivery.StatusRevision == 0 {
		t.Fatalf("published delivery = %#v", delivery)
	}
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM external_agent_job_notifications WHERE job_id = ?`, job.ID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("outbox rows after publish = %d, want 1", outboxCount)
	}
	if again, err := jobs.ClaimNextNotification(t.Context(), now.Add(time.Hour), "second-worker", time.Minute); err != nil || again != nil {
		t.Fatalf("published delivery was claimable: %#v, err=%v", again, err)
	}

	mu.Lock()
	gotRequests := append([]requestRecord(nil), requests...)
	mu.Unlock()
	if len(gotRequests) != 1 {
		t.Fatalf("Slack post requests = %d, want 1", len(gotRequests))
	}
	request := gotRequests[0]
	if request.Channel != "D12345678" || request.ThreadTS != "1710000000.000001" {
		t.Fatalf("Slack destination = %#v", request)
	}
	payload, ok := request.Metadata["event_payload"].(map[string]any)
	if !ok {
		t.Fatalf("Slack metadata payload = %#v", request.Metadata)
	}
	canonical := "External-agent job `job_integration_1` completed.\n\n" + content
	canonicalDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))
	if payload["job_id"] != job.ID || payload["status_revision"] != float64(delivery.StatusRevision) || payload["notification_sha256"] != canonicalDigest {
		t.Fatalf("Slack job metadata = %#v", payload)
	}
	if payload["result_sha256"] != digest || payload["result_bytes"] != float64(len(content)) {
		t.Fatalf("Slack result identity metadata = %#v", payload)
	}
	if payload["notification_sha256"] == payload["result_sha256"] {
		t.Fatalf("Slack notification and result identities collide: %#v", payload)
	}
}

type integrationLogger struct{}

func (integrationLogger) Debug(string, ...any) {}
func (integrationLogger) Info(string, ...any)  {}
func (integrationLogger) Warn(string, ...any)  {}
func (integrationLogger) Error(string, ...any) {}

var _ port.Logger = integrationLogger{}

type integrationJobRuntime struct{}

func (*integrationJobRuntime) Run(context.Context, domain.ExternalAgentJob) (domain.ExternalAgentInvocationResult, error) {
	return domain.ExternalAgentInvocationResult{}, context.Canceled
}

var _ port.ExternalAgentJobRuntime = (*integrationJobRuntime)(nil)

func TestDetachedJobNotificationRetriesHTTPFailureAndReclaimsExpiredPublishing(t *testing.T) {
	var (
		mu    sync.Mutex
		posts int
	)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/conversations.history":
			response.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(response, `{"ok":true,"messages":[]}`)
		case "/chat.postMessage":
			if err := request.ParseForm(); err != nil {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			posts++
			attempt := posts
			mu.Unlock()
			if attempt == 1 {
				response.WriteHeader(http.StatusInternalServerError)
				_, _ = fmt.Fprint(response, `{"ok":false,"error":"provider body must not persist"}`)
				return
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(response, `{"ok":true,"channel":"D12345678","ts":"1710000000.00000%d"}`, attempt)
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := adaptersqlite.NewExternalAgentJobStore(store)
	now := time.Now().UTC().Add(-time.Minute)
	job := integrationDetachedJob("job_retry_integration", now)
	created, _, err := jobs.CreateIfAbsent(t.Context(), job)
	if err != nil || !created {
		t.Fatalf("create job = %v, err=%v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "execution-worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim job = %#v, err=%v", claimed, err)
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, integrationMarkdownResult(job.ID, "retry result"), "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	artifacts, err := fsartifact.New(filepath.Join(t.TempDir(), "artifacts"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	client := slackapi.New("xoxb-local-test", slackapi.OptionAPIURL(server.URL+"/"))
	publisher := slackadapter.NewPublisher(client, time.Second, integrationLogger{}, false)
	history := slackadapter.NewHistoryReader(client, "B12345678", time.Second, integrationLogger{}, false)
	service, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute,
		PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1,
	}, externalagent.Dependencies{Store: jobs, Runtime: &integrationJobRuntime{}, Artifacts: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	notificationPublisher := slackadapter.NewDurableJobNotificationPublisher(publisher, history, slackadapter.NewGeneratedFileUploader(client, time.Second), artifacts, jobs, client, false)
	worker, err := externalagent.NewNotificationWorker(
		externalagent.NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Minute, RetryBase: time.Hour, RetryMax: time.Hour},
		externalagent.NotificationDependencies{Store: jobs, Publisher: notificationPublisher, HostCompleter: service},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	inspection, err := jobs.InspectJob(t.Context(), job.ID)
	if err != nil || inspection == nil || len(inspection.Deliveries) != 1 {
		t.Fatalf("retry inspection = %#v, err=%v", inspection, err)
	}
	if inspection.Deliveries[0].PublishState != domain.NotificationPending || inspection.Deliveries[0].Attempts != 1 || inspection.Deliveries[0].LastErrorCode != "notification_publish_ambiguous" {
		t.Fatalf("retry state = %#v", inspection.Deliveries[0])
	}
	if _, err := store.DB().ExecContext(
		t.Context(),
		`UPDATE external_agent_job_notifications SET publish_state = ?, lease_expiry = ?, next_attempt_at = ? WHERE job_id = ?`,
		domain.NotificationPublishing,
		time.Now().UTC().Add(-time.Minute).UnixNano(),
		time.Now().UTC().UnixNano(),
		job.ID,
	); err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	inspection, err = jobs.InspectJob(t.Context(), job.ID)
	if err != nil || inspection == nil || len(inspection.Deliveries) != 1 {
		t.Fatalf("recovered inspection = %#v, err=%v", inspection, err)
	}
	if inspection.Deliveries[0].PublishState != domain.NotificationPublished || inspection.Deliveries[0].Attempts != 2 || inspection.Deliveries[0].RecoveredSlackTS == "" ||
		inspection.Deliveries[0].LastErrorCode != "" {
		t.Fatalf("recovered state = %#v", inspection.Deliveries[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 2 {
		t.Fatalf("chat.postMessage calls = %d, want 2", posts)
	}
}

func TestDetachedJobNotificationPublishesMultipartMarkdownFromOutbox(t *testing.T) {
	var (
		mu       sync.Mutex
		requests int
	)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat.postMessage" {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		if err := request.ParseForm(); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests++
		attempt := requests
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(response, `{"ok":true,"channel":"D12345678","ts":"1710000000.00000%d"}`, attempt)
	}))
	t.Cleanup(server.Close)
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := adaptersqlite.NewExternalAgentJobStore(store)
	now := time.Now().UTC().Add(-time.Minute)
	job := integrationDetachedJob("job_multipart_integration", now)
	created, _, err := jobs.CreateIfAbsent(t.Context(), job)
	if err != nil || !created {
		t.Fatalf("create job = %v, err=%v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "execution-worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim job = %#v, err=%v", claimed, err)
	}
	content := strings.Repeat("m", domain.SlackMarkdownChunkRunes)
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, integrationMarkdownResult(job.ID, content), "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	artifacts, err := fsartifact.New(filepath.Join(t.TempDir(), "artifacts"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	client := slackapi.New("xoxb-local-test", slackapi.OptionAPIURL(server.URL+"/"))
	publisher := slackadapter.NewPublisher(client, time.Second, integrationLogger{}, false)
	service, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute,
		PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1,
	}, externalagent.Dependencies{Store: jobs, Runtime: &integrationJobRuntime{}, Artifacts: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	history := slackadapter.NewHistoryReader(client, "B12345678", time.Second, integrationLogger{}, false)
	worker, err := externalagent.NewNotificationWorker(externalagent.NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Minute}, externalagent.NotificationDependencies{
		Store:         jobs,
		Publisher:     slackadapter.NewDurableJobNotificationPublisher(publisher, history, slackadapter.NewGeneratedFileUploader(client, time.Second), artifacts, jobs, client, false),
		HostCompleter: service,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	inspection, err := jobs.InspectJob(t.Context(), job.ID)
	if err != nil || inspection == nil || len(inspection.Deliveries) != 1 || inspection.Deliveries[0].PublishState != domain.NotificationPublished {
		t.Fatalf("multipart inspection = %#v, err=%v", inspection, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 2 {
		t.Fatalf("multipart Slack posts = %d, want 2", requests)
	}
}

func TestDetachedJobFileDeliveryRecoversAcrossWorkerRestarts(t *testing.T) {
	content := "complete file-mode result after worker restarts"
	var (
		mu              sync.Mutex
		uploaded        []byte
		uploadURLCalls  int
		uploadCalls     int
		completeCalls   int
		postCalls       int
		completeVisible bool
		statusTS        string
		statusMetadata  map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/files.getUploadURLExternal":
			mu.Lock()
			uploadURLCalls++
			mu.Unlock()
			response.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(response, `{"ok":true,"file_id":"F123","upload_url":"http://%s/upload"}`, request.Host)
		case "/upload":
			if err := request.ParseMultipartForm(1024 * 1024); err != nil {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			file, _, err := request.FormFile("file")
			if err != nil {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			data, readErr := io.ReadAll(file)
			_ = file.Close()
			if readErr != nil {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			uploadCalls++
			uploaded = append([]byte(nil), data...)
			mu.Unlock()
			_, _ = response.Write([]byte("ok"))
		case "/files.completeUploadExternal":
			mu.Lock()
			completeCalls++
			completeVisible = true
			mu.Unlock()
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"ok":true,"files":[{"id":"F123","title":"External-agent result job_file_integration"}]}`))
		case "/files.info":
			mu.Lock()
			visible := completeVisible
			mu.Unlock()
			channels := "[]"
			if visible {
				channels = `["D12345678"]`
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(response, `{"ok":true,"file":{"id":"F123","name":"external-agent-job_file_integration.md","size":%d,"user":"B12345678","channels":%s}}`, len(content), channels)
		case "/conversations.replies", "/conversations.history":
			mu.Lock()
			gotStatusTS := statusTS
			gotStatusMetadata := statusMetadata
			mu.Unlock()
			response.Header().Set("Content-Type", "application/json")
			messages := make([]map[string]any, 0, 1)
			if gotStatusTS != "" {
				messages = append(messages, map[string]any{
					"type": "message", "user": "B12345678", "ts": gotStatusTS,
					"metadata": gotStatusMetadata,
				})
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"ok": true, "messages": messages})
		case "/chat.postMessage":
			var metadata map[string]any
			if err := json.Unmarshal([]byte(request.FormValue("metadata")), &metadata); err != nil {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			postCalls++
			attempt := postCalls
			statusTS = fmt.Sprintf("1710000000.00000%d", attempt)
			statusMetadata = metadata
			mu.Unlock()
			response.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(response, `{"ok":true,"channel":"D12345678","ts":"1710000000.00000%d"}`, attempt)
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	database := filepath.Join(t.TempDir(), "jobs.db")
	artifactDirectory := filepath.Join(t.TempDir(), "artifacts")
	store, err := adaptersqlite.Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	initialStore := store
	t.Cleanup(func() { _ = initialStore.Close() })
	jobs := adaptersqlite.NewExternalAgentJobStore(store)
	artifacts, err := fsartifact.New(artifactDirectory, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	job := integrationDetachedJob("job_file_integration", time.Now().UTC().Add(-time.Minute))
	artifact, err := artifacts.Put(t.Context(), job.ID+"-delivery", content)
	if err != nil {
		t.Fatal(err)
	}
	if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create job = %v, err=%v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), job.CreatedAt, "execution-worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim job = %#v, err=%v", claimed, err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	result := &domain.ExternalAgentInvocationResult{
		Text: "", ResultSHA256: digest, ResultBytes: int64(len(content)), ArtifactRef: artifact.Reference,
		DeliveryMode: domain.JobResultDeliveryFile, DeliveryPolicyVersion: domain.JobDeliveryPolicyV1,
		DeliveryArtifactRef: artifact.Reference, DeliveryContentSHA256: digest, DeliveryContentBytes: int64(len(content)), DeliveryMaxMarkdownParts: 6,
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, result, "", job.CreatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	newWorker := func(jobStore *adaptersqlite.ExternalAgentJobStore, artifactStore *fsartifact.Store) *externalagent.NotificationWorker {
		service, serviceErr := externalagent.New(externalagent.Config{
			DefaultTimeout: time.Minute, MaxTimeout: time.Hour, LeaseTTL: time.Minute,
			PollInterval: time.Millisecond, Concurrency: 1, MaxAttempts: 1,
		}, externalagent.Dependencies{Store: jobStore, Runtime: &integrationJobRuntime{}, Artifacts: artifactStore})
		if serviceErr != nil {
			t.Fatal(serviceErr)
		}
		client := slackapi.New("xoxb-local-test", slackapi.OptionAPIURL(server.URL+"/"))
		publisher := slackadapter.NewPublisher(client, time.Millisecond, integrationLogger{}, false)
		history := slackadapter.NewHistoryReader(client, "B12345678", time.Second, integrationLogger{}, false)
		uploader := slackadapter.NewGeneratedFileUploader(client, time.Second)
		notificationPublisher := slackadapter.NewDurableJobNotificationPublisher(publisher, history, uploader, artifactStore, jobStore, client, false)
		worker, workerErr := externalagent.NewNotificationWorker(externalagent.NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Minute}, externalagent.NotificationDependencies{
			Store: jobStore, Publisher: notificationPublisher, HostCompleter: service,
		})
		if workerErr != nil {
			t.Fatal(workerErr)
		}
		return worker
	}
	if _, err := store.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_notification_publish
		BEFORE UPDATE OF publish_state ON external_agent_job_notifications
		WHEN NEW.publish_state = 'published'
		BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}

	if err := newWorker(jobs, artifacts).ProcessOne(t.Context()); err == nil {
		t.Fatal("first worker unexpectedly completed after publication CAS was blocked")
	}
	inspection, err := jobs.InspectJob(t.Context(), job.ID)
	if err != nil || inspection == nil || len(inspection.Deliveries) != 1 {
		t.Fatalf("after bytes stage inspection = %#v, err=%v", inspection, err)
	}
	firstDelivery := inspection.Deliveries[0]
	if firstDelivery.PublishState != domain.NotificationPublishing || firstDelivery.UploadState != domain.JobResultUploadCompleted || !firstDelivery.SlackFileIDPresent {
		t.Fatalf("after interrupted publication delivery = %#v", firstDelivery)
	}
	if _, err := store.DB().ExecContext(t.Context(), `DROP TRIGGER reject_notification_publish`); err != nil {
		t.Fatal(err)
	}
	restartAt := time.Now().UTC()
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE external_agent_job_notifications
		SET lease_expiry = ?, next_attempt_at = ? WHERE job_id = ?`, restartAt.Add(-time.Second).UnixNano(), restartAt.UnixNano(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = adaptersqlite.Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	restartedJobs := adaptersqlite.NewExternalAgentJobStore(store)
	restartedArtifacts, err := fsartifact.New(artifactDirectory, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := newWorker(restartedJobs, restartedArtifacts).ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	inspection, err = restartedJobs.InspectJob(t.Context(), job.ID)
	if err != nil || inspection == nil || len(inspection.Deliveries) != 1 {
		t.Fatalf("final inspection = %#v, err=%v", inspection, err)
	}
	finalDelivery := inspection.Deliveries[0]
	if finalDelivery.PublishState != domain.NotificationPublished || finalDelivery.UploadState != domain.JobResultUploadCompleted || !finalDelivery.SlackFileIDPresent ||
		finalDelivery.RecoveredSlackTS != statusTS {
		t.Fatalf("final file delivery = %#v", finalDelivery)
	}
	mu.Lock()
	gotUploaded := append([]byte(nil), uploaded...)
	gotUploadURLCalls, gotUploadCalls := uploadURLCalls, uploadCalls
	gotCompleteCalls, gotPostCalls := completeCalls, postCalls
	mu.Unlock()
	if string(gotUploaded) != content || gotUploadURLCalls != 1 || gotUploadCalls != 1 || gotCompleteCalls != 1 || gotPostCalls != 1 {
		t.Fatalf("Slack file flow uploaded=%q upload_url_calls=%d upload_calls=%d complete_calls=%d post_calls=%d", gotUploaded, gotUploadURLCalls, gotUploadCalls, gotCompleteCalls, gotPostCalls)
	}
	if after, err := restartedJobs.ClaimNextNotification(t.Context(), time.Now().UTC().Add(time.Hour), "after-worker", time.Minute); err != nil || after != nil {
		t.Fatalf("published delivery was claimable: %#v, err=%v", after, err)
	}
}

func integrationDetachedJob(id string, now time.Time) domain.ExternalAgentJob {
	return domain.ExternalAgentJob{
		ID:               id,
		Mode:             domain.JobDetached,
		Provider:         "agentcli",
		Profile:          "build",
		PrimaryProject:   "workspace",
		RegistryRevision: "r1",
		Task:             "integration task",
		Actor:            "U12345678",
		TeamID:           "T12345678",
		ConversationKey:  "slack:T12345678:dm:D12345678",
		Status:           domain.JobQueued,
		TimeoutAt:        now.Add(time.Hour),
		CreatedAt:        now,
		UpdatedAt:        now,
		RequestSHA256: domain.ExternalAgentJobRequestDigest(
			domain.ExternalAgentJobRequest{
				Provider:         "agentcli",
				Profile:          "build",
				PrimaryProject:   "workspace",
				RegistryRevision: "r1",
				Task:             "integration task",
				Mode:             domain.JobDetached,
				Actor:            "U12345678",
				TeamID:           "T12345678",
				ConversationKey:  "slack:T12345678:dm:D12345678",
			},
		),
	}
}

func integrationMarkdownResult(jobID, content string) *domain.ExternalAgentInvocationResult {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	return &domain.ExternalAgentInvocationResult{
		Text: content, ResultSHA256: digest, ResultBytes: int64(len(content)), DeliveryMode: domain.JobResultDeliveryMarkdown,
		DeliveryPolicyVersion: domain.JobDeliveryPolicyV1, DeliveryContentSHA256: digest, DeliveryContentBytes: int64(len(content)),
		DeliveryMaxMarkdownParts: 6, DeliveryCanonicalMarkdown: "External-agent job `" + jobID + "` completed.\n\n" + content,
	}
}
