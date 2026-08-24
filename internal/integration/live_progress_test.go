package integration_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/acpclient"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	externalagent "github.com/Dauno/slack-local-agent/internal/usecase/externalagent"
)

// liveProgressTestRuntime is a provider-neutral runtime that executes the
// scripted ACP agent through the real adapter and installs a progress
// recorder, mirroring the composed dispatcher.
type liveProgressTestRuntime struct {
	client        *acpclient.Client
	store         port.ExternalAgentJobStore
	registry      port.ACPProcessRegistry
	progressStore port.ExternalAgentJobProgressStore
	warnAfter     time.Duration
	logger        port.Logger
	metrics       port.MetricRecorder
	global        string
}

func (r *liveProgressTestRuntime) Run(ctx context.Context, job domain.ExternalAgentJob) (domain.AcpInvocationResult, error) {
	recorder := externalagent.NewProgressRecorder(r.progressStore, r.registry, nil, r.logger, r.metrics, nil, r.warnAfter, job.ID, job.LeaseOwner, job.Attempt)
	recorder.Start(ctx)
	defer recorder.Close()
	result, err := r.client.Run(ctx, domain.AcpInvocationRequest{
		JobID: job.ID, PrimaryProject: job.PrimaryProject, PrimaryPath: job.PrimaryProject,
		ProfileName: job.Profile, PermissionOptionKind: domain.ACPPermissionRejectOnce,
		GlobalInstruction: r.global, Task: job.Task, Timeout: 2 * time.Minute,
		OnSessionCreated: func(sessionID string) error {
			recorder.SetSessionID(sessionID)
			return r.store.AssignACPSession(ctx, job.ID, job.LeaseOwner, job.Attempt, sessionID)
		},
		OnProgress: recorder.Record,
	})
	if err != nil {
		return domain.AcpInvocationResult{}, err
	}
	return result, nil
}

func TestDurableJobLiveProjectionWithScriptedACPProcess(t *testing.T) {
	database := filepath.Join(t.TempDir(), "local-agent.db")
	store, err := adaptersqlite.Initialize(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobStore := adaptersqlite.NewExternalAgentJobStore(store)
	workDir := t.TempDir()
	registry := newIntegrationRegistry()
	runtime := &liveProgressTestRuntime{
		client: acpclient.New("python3", []string{"-c", liveProgressAgentScript()}),
		store:  jobStore, registry: registry, progressStore: jobStore,
		warnAfter: 10 * time.Second, logger: integrationLogger{}, metrics: port.NoopMetricRecorder{},
	}
	service, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Minute, MaxTimeout: 2 * time.Minute, LeaseTTL: 30 * time.Second,
		PollInterval: 10 * time.Millisecond, Concurrency: 1, MaxAttempts: 1,
	}, externalagent.Dependencies{
		Store: jobStore, Runtime: runtime, ProgressStore: jobStore, ProcessRegistry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Start(t.Context(), domain.ExternalAgentJobRequest{
		Provider: "opencode", Profile: "default", PrimaryProject: workDir,
		Task: "inspect and report", Mode: domain.JobDetached, Actor: "U1",
		TeamID: "T1", ConversationKey: domain.ConversationKey("slack:T1:dm:D1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go service.Run(ctx)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		current, _ := jobStore.GetJob(t.Context(), job.ID)
		if current != nil && current.Status == domain.JobCompleted {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	view, err := service.StatusProjection(t.Context(), job.ID, "U1", domain.ConversationKey("slack:T1:dm:D1"))
	if err != nil {
		t.Fatalf("status projection: %v", err)
	}
	if view.ACPSessionID == "" {
		t.Fatal("projection must retain the complete ACP session ID")
	}
	if view.Phase != domain.ACPPhaseCompleted {
		t.Fatalf("projected phase = %s, want completed", view.Phase)
	}
	if view.Health != domain.ACPHealthTerminal {
		t.Fatalf("projected health = %s, want terminal", view.Health)
	}
	if view.StopReason != domain.ACPStopReasonEndTurn {
		t.Fatalf("projected stop reason = %q", view.StopReason)
	}
	// The durable projection survives a restart without opening ACP.
	restarted, err := adaptersqlite.OpenExisting(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	persisted, err := adaptersqlite.NewExternalAgentJobStore(restarted).ReadJobProgress(t.Context(), job.ID)
	if err != nil || persisted == nil {
		t.Fatalf("persisted projection after restart: %v %v", persisted, err)
	}
	if persisted.Phase != domain.ACPPhaseCompleted || persisted.StopReason != domain.ACPStopReasonEndTurn {
		t.Fatalf("restarted projection = %+v", persisted)
	}
	if !domain.ValidACPEventKind(persisted.LastEventKind) {
		t.Fatalf("restarted projection has invalid last event kind: %q", persisted.LastEventKind)
	}
}

// integrationRegistry is the test process registry; liveness is always
// reported as alive once registered.
type integrationRegistry struct {
	procs map[string]int
}

func newIntegrationRegistry() *integrationRegistry {
	return &integrationRegistry{procs: make(map[string]int)}
}

func (r *integrationRegistry) Register(jobID string, attempt int, pid int) {
	r.procs[jobID] = pid
}

func (r *integrationRegistry) ProcessAlive(jobID string, attempt int) *bool {
	_, exists := r.procs[jobID]
	if !exists {
		return nil
	}
	alive := true
	return &alive
}

func liveProgressAgentScript() string {
	return `import sys, json, time

config = {"model":"default"}
session_id = "session-live-1"

def send(value):
    sys.stdout.write(json.dumps(value) + "\n")
    sys.stdout.flush()

def respond(req_id, result):
    send({"jsonrpc":"2.0","id":req_id,"result":result})

def notify(update):
    send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session_id,"update":update}})

for line in sys.stdin:
    req = json.loads(line)
    method = req.get("method", "")
    req_id = req.get("id")
    params = req.get("params", {})
    if method == "initialize":
        respond(req_id, {"protocolVersion": 1, "agentInfo":{"name":"fake-acp-agent","version":"1.0.0"}, "agentCapabilities":{"sessionCapabilities":{"close":{}}}})
    elif method == "session/new":
        respond(req_id, {"sessionId":session_id,"configOptions":[{"id":"model","name":"model","type":"select","currentValue":"default","options":[]}]})
    elif method == "session/set_config_option":
        respond(req_id, {"configOptions":[{"id":"model","name":"model","type":"select","currentValue":req["params"]["value"],"options":[]}]})
    elif method == "session/prompt":
        notify({"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}})
        notify({"sessionUpdate":"tool_call","toolCallId":"tool-live-1","kind":"execute","status":"pending"})
        notify({"sessionUpdate":"tool_call_update","toolCallId":"tool-live-1","kind":"execute","status":"in_progress"})
        time.sleep(0.5)
        notify({"sessionUpdate":"tool_call_update","toolCallId":"tool-live-1","kind":"execute","status":"completed"})
        notify({"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"integration result text"}})
        respond(req_id, {"stopReason":"end_turn"})
    elif method == "session/close":
        respond(req_id, {})
        break
`
}
