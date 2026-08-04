package toolfactory_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/toolfactory"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func boolPtr(value bool) *bool { return &value }

// projectionExternalJobReader is a stub external-agent reader that also
// implements the optional status projection contract.
type projectionExternalJobReader struct {
	stubExternalJobReader
	view *domain.ExternalAgentJobStatusView
}

func (r projectionExternalJobReader) StatusProjection(ctx context.Context, jobID, actor string, key domain.ConversationKey) (*domain.ExternalAgentJobStatusView, error) {
	if r.view == nil {
		return nil, errors.New("external-agent job was not found")
	}
	return r.view, nil
}

func TestJobStatusJSONContract(t *testing.T) {
	alive := true
	base := time.Date(2026, 8, 4, 2, 41, 2, 0, time.UTC)
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	reader := &projectionExternalJobReader{
		stubExternalJobReader: stubExternalJobReader{job: &domain.ExternalAgentJob{ID: "job_bb3ed6", Status: domain.JobRunning, StatusRevision: 1, ACPSessionID: "ses_full_identity_0123456789"}},
		view: &domain.ExternalAgentJobStatusView{
			JobID: "job_bb3ed6", Status: domain.JobRunning, StatusRevision: 1,
			ACPSessionID: "ses_full_identity_0123456789",
			Phase:        domain.ACPPhaseToolRunning, Health: domain.ACPHealthPossiblyStalled,
			LastEventKind:            domain.ACPEventToolCallUpdate,
			LastTransportActivityAt:  base,
			LastSessionUpdateAt:      base,
			LastMeaningfulProgressAt: base.Add(-4 * time.Second),
			ActiveToolCount:          1, PromptElapsedSeconds: 3780,
			ProcessAlive: &alive,
		},
	}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	tools, err := factory.ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	var statusTool runnableFunctionTool
	for _, candidate := range tools {
		if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == "job_status" {
			statusTool, _ = candidate.(runnableFunctionTool)
		}
	}
	if statusTool == nil {
		t.Fatal("job_status tool is unavailable")
	}
	value, err := statusTool.Run(&stubToolContext{}, map[string]any{"job_id": "job_bb3ed6"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	raw, _ := json.Marshal(value)
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["acp_session_id"] != "ses_full_identity_0123456789" {
		t.Fatalf("acp_session_id = %v", result["acp_session_id"])
	}
	if result["phase"] != "tool_running" || result["health"] != "possibly_stalled" {
		t.Fatalf("phase/health = %v/%v", result["phase"], result["health"])
	}
	if result["last_event_kind"] != "tool_call_update" {
		t.Fatalf("last_event_kind = %v", result["last_event_kind"])
	}
	if result["last_transport_activity_at"] != "2026-08-04T02:41:02Z" {
		t.Fatalf("last_transport_activity_at = %v", result["last_transport_activity_at"])
	}
	if result["active_tool_count"] != float64(1) || result["pending_permission"] != false {
		t.Fatalf("tool count/permission = %v/%v", result["active_tool_count"], result["pending_permission"])
	}
	if result["prompt_elapsed_seconds"] != float64(3780) {
		t.Fatalf("prompt_elapsed_seconds = %v", result["prompt_elapsed_seconds"])
	}
	if result["stop_reason"] != "" {
		t.Fatalf("stop_reason = %v", result["stop_reason"])
	}
	if result["process_alive"] != true {
		t.Fatalf("process_alive = %v", result["process_alive"])
	}
}

func TestJobStatusQueuedSessionIsEmpty(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	reader := &stubExternalJobReader{job: &domain.ExternalAgentJob{ID: "job_queued", Status: domain.JobQueued, StatusRevision: 0, ACPSessionID: ""}}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	tools, err := factory.ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	var statusTool runnableFunctionTool
	for _, candidate := range tools {
		if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == "job_status" {
			statusTool, _ = candidate.(runnableFunctionTool)
		}
	}
	value, err := statusTool.Run(&stubToolContext{}, map[string]any{"job_id": "job_queued"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	raw, _ := json.Marshal(value)
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["acp_session_id"] != "" {
		t.Fatalf("queued acp_session_id = %q, want empty", result["acp_session_id"])
	}
	if result["phase"] != "" || result["health"] != "" {
		t.Fatalf("queued projection = %v/%v", result["phase"], result["health"])
	}
}

func TestJobStatusTerminalRetainsSessionID(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	reader := &stubExternalJobReader{job: &domain.ExternalAgentJob{
		ID: "job_terminal", Status: domain.JobCompletionUnknown, StatusRevision: 3, ACPSessionID: "ses_terminal_identity_9876543210",
	}}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	tools, err := factory.ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	var statusTool runnableFunctionTool
	for _, candidate := range tools {
		if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == "job_status" {
			statusTool, _ = candidate.(runnableFunctionTool)
		}
	}
	value, err := statusTool.Run(&stubToolContext{}, map[string]any{"job_id": "job_terminal"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	raw, _ := json.Marshal(value)
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["acp_session_id"] != "ses_terminal_identity_9876543210" {
		t.Fatalf("terminal acp_session_id = %v", result["acp_session_id"])
	}
}

func TestActivationStatusKeepsRevisionBoundSnapshot(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	// The current projection has drifted to a newer revision than the
	// activation; the merged live fields must be rejected so the response
	// stays bound to the activation revision.
	reader := &projectionExternalJobReader{
		stubExternalJobReader: stubExternalJobReader{job: &domain.ExternalAgentJob{
			ID: "job_act", Status: domain.JobCompleted, StatusRevision: 4, ACPSessionID: "ses_activation_identity",
		}},
		view: &domain.ExternalAgentJobStatusView{
			JobID: "job_act", Status: domain.JobCompleted, StatusRevision: 7,
			ACPSessionID: "ses_activation_identity", Phase: domain.ACPPhaseResponding,
			Health: domain.ACPHealthActive, ProcessAlive: boolPtr(true),
		},
	}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	activation := domain.ExternalAgentJobActivation{
		ActivationID: "activation-1", JobID: "job_act", StatusRevision: 4, Kind: "terminal",
		TerminalStatus: domain.JobCompleted, Actor: "U12345678", ConversationKey: key,
	}
	tools, err := factory.ToolsForActivation(activation.Actor, key, activation)
	if err != nil {
		t.Fatalf("activation tools: %v", err)
	}
	var statusTool runnableFunctionTool
	for _, candidate := range tools {
		if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == "job_status" {
			statusTool, _ = candidate.(runnableFunctionTool)
		}
	}
	if statusTool == nil {
		t.Fatal("activation job_status tool is unavailable")
	}
	value, err := statusTool.Run(&stubToolContext{}, map[string]any{"job_id": "job_act"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	raw, _ := json.Marshal(value)
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["status_revision"] != float64(4) {
		t.Fatalf("activation status revision = %v, want 4", result["status_revision"])
	}
	if result["phase"] != "" {
		t.Fatalf("drifted live phase leaked into revision-bound status: %v", result["phase"])
	}
	if result["acp_session_id"] != "ses_activation_identity" {
		t.Fatalf("activation session ID = %v", result["acp_session_id"])
	}
}
