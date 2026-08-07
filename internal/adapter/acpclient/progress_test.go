package acpclient_test

import (
	"context"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/adapter/acpclient"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestACPFakeAgent_EmitsContentFreeProgressEvents(t *testing.T) {
	client := acpclient.New("python3", []string{"-c", progressScript()})
	var events []domain.ACPProgressEvent
	result, err := client.Run(context.Background(), domain.AcpInvocationRequest{
		PrimaryPath:          t.TempDir(),
		ConfigOptions:        []domain.ACPConfigOption{{ID: "model", Value: "test"}},
		PermissionOptionKind: domain.ACPPermissionRejectOnce,
		Task:                 "task",
		OnProgress: func(event domain.ACPProgressEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "safe final text" {
		t.Fatalf("result text = %q", result.Text)
	}
	wantKinds := []domain.ACPEventKind{
		domain.ACPEventProcessStarted,
		domain.ACPEventInitializeResponse,
		domain.ACPEventSessionNew,
		domain.ACPEventTransportActivity,
		domain.ACPEventPromptSent,
		domain.ACPEventThoughtChunk,
		domain.ACPEventToolCall,
		domain.ACPEventPermissionRequested,
		domain.ACPEventPermissionResponded,
		domain.ACPEventToolCallUpdate,
		domain.ACPEventToolCallUpdate,
		domain.ACPEventUsageUpdate,
		domain.ACPEventUsageUpdate,
		domain.ACPEventMessageChunk,
		domain.ACPEventPromptResponse,
		domain.ACPEventTransportActivity,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(wantKinds), events)
	}
	for index, kind := range wantKinds {
		if events[index].Kind != kind {
			t.Fatalf("event %d kind = %s, want %s", index, events[index].Kind, kind)
		}
	}
	if events[0].PID <= 0 {
		t.Fatalf("process start must carry a PID, got %d", events[0].PID)
	}
	// Bounded tool metadata only: call ID and status, never input or output.
	toolEvent := events[6]
	if toolEvent.Tool == nil || toolEvent.Tool.CallID != "tool-1" || toolEvent.Tool.Status != domain.ACPToolStatusPending {
		t.Fatalf("tool event = %+v", toolEvent.Tool)
	}
	if toolEvent.Tool.Kind != domain.ACPToolKindExecute {
		t.Fatalf("tool kind = %q, want execute", toolEvent.Tool.Kind)
	}
	runningEvent := events[9]
	if runningEvent.Tool == nil || runningEvent.Tool.Status != domain.ACPToolStatusRunning {
		t.Fatalf("tool running event = %+v", runningEvent.Tool)
	}
	// A completed tool maps to the internal terminal status.
	terminalEvent := events[10]
	if terminalEvent.Tool == nil || terminalEvent.Tool.Status != domain.ACPToolStatusTerminal {
		t.Fatalf("tool terminal event = %+v", terminalEvent.Tool)
	}
	// Usage updates gate meaningful progress on increasing counters.
	if events[12].UsageIncreased {
		t.Fatal("second non-increasing usage update must not claim an increase")
	}
	// Permission transitions carry the pending boolean.
	if !events[7].PermissionPending || events[8].PermissionPending {
		t.Fatalf("permission pending flags = %t/%t", events[7].PermissionPending, events[8].PermissionPending)
	}
	// Terminal prompt response carries the stop reason.
	if events[14].StopReason != domain.ACPStopReasonEndTurn {
		t.Fatalf("prompt response stop reason = %q", events[14].StopReason)
	}
}

func TestACPFakeAgent_ProgressCanaryNeverLeaks(t *testing.T) {
	client := acpclient.New("python3", []string{"-c", canaryProgressScript()})
	var events []domain.ACPProgressEvent
	_, err := client.Run(context.Background(), domain.AcpInvocationRequest{
		PrimaryPath:          t.TempDir(),
		ConfigOptions:        []domain.ACPConfigOption{{ID: "model", Value: "test"}},
		PermissionOptionKind: domain.ACPPermissionRejectOnce,
		Task:                 "task",
		OnProgress: func(event domain.ACPProgressEvent) {
			events = append(events, event)
		},
	})
	if err == nil {
		t.Fatal("expected unknown stop reason to fail the run")
	}
	for _, event := range events {
		if event.StopReason != "" && event.StopReason != domain.ACPStopReasonEndTurn && event.StopReason != domain.ACPStopReasonCancelled {
			t.Fatalf("unbounded stop reason leaked into progress event: %q", event.StopReason)
		}
		switch event.ErrorClass {
		case "", "acp_prompt_failed", "acp_frame_too_large", "acp_malformed_frame",
			"acp_protocol_violation", "acp_process_exit", "acp_config_drift",
			"acp_idle_timeout", "acp_result_too_large", "acp_permission_unavailable",
			"acp_completed_without_final_message", "acp_invalid_input",
			"acp_session_recovery_unsupported", "result_artifact_invalid",
			"result_delivery_failed", "acp_job_timeout":
		default:
			t.Fatalf("unbounded error class leaked into progress event: %q", event.ErrorClass)
		}
	}
}

func TestACPFakeAgent_MismatchedSessionCannotUpdateProgress(t *testing.T) {
	client := acpclient.New("python3", []string{"-c", mismatchedSessionScript()})
	var events []domain.ACPProgressEvent
	_, err := client.Run(context.Background(), domain.AcpInvocationRequest{
		PrimaryPath:          t.TempDir(),
		ConfigOptions:        []domain.ACPConfigOption{{ID: "model", Value: "test"}},
		PermissionOptionKind: domain.ACPPermissionRejectOnce,
		Task:                 "task",
		OnProgress: func(event domain.ACPProgressEvent) {
			events = append(events, event)
		},
	})
	if err == nil {
		t.Fatal("mismatched session update must fail the prompt")
	}
	for _, event := range events {
		if event.Kind == domain.ACPEventMessageChunk || event.Kind == domain.ACPEventThoughtChunk {
			t.Fatalf("mismatched session leaked a progress event: %+v", event)
		}
	}
}

func mismatchedSessionScript() string {
	return `import sys, json

config = {"model":"default"}
session_id = "session-real-1"

def send(value):
    sys.stdout.write(json.dumps(value) + "\n")
    sys.stdout.flush()

def respond(req_id, result):
    send({"jsonrpc":"2.0","id":req_id,"result":result})

for line in sys.stdin:
    req = json.loads(line)
    method = req.get("method", "")
    req_id = req.get("id")
    params = req.get("params", {})
    if method == "initialize":
        respond(req_id, {"protocolVersion": 1, "agentInfo":{"name":"fake","version":"1"}, "agentCapabilities":{"sessionCapabilities":{"close":{}}}})
    elif method == "session/new":
        respond(req_id, {"sessionId":session_id,"configOptions":[{"id":"model","name":"model","type":"select","currentValue":"default","options":[]}]})
    elif method == "session/set_config_option":
        respond(req_id, {"configOptions":[{"id":"model","name":"model","type":"select","currentValue":req["params"]["value"],"options":[]}]})
    elif method == "session/prompt":
        send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-wrong","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"leaked"}}}})
        respond(req_id, {"stopReason":"end_turn"})
    elif method == "session/close":
        respond(req_id, {})
        break
`
}

func progressScript() string {
	return `import sys, json

config = {"model":"default-model", "mode":"ask"}
session_id = "session-real-1"

def send(value):
    sys.stdout.write(json.dumps(value) + "\n")
    sys.stdout.flush()

def respond(req_id, result):
    send({"jsonrpc":"2.0","id":req_id,"result":result})

def options():
    return [{"id":key,"name":key,"type":"select","currentValue":value,"options":[{"value":value,"name":str(value)}]} for key,value in config.items()]

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
        respond(req_id, {"sessionId":session_id,"configOptions":options()})
    elif method == "session/set_config_option":
        config[params["configId"]] = params["value"]
        respond(req_id, {"configOptions":options()})
    elif method == "session/prompt":
        notify({"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"private thought"}})
        notify({"sessionUpdate":"tool_call","toolCallId":"tool-1","kind":"execute","rawInput":{"secret":"raw input"}})
        send({"jsonrpc":"2.0","id":"permission-1","method":"session/request_permission","params":{"sessionId":session_id,"toolCall":{"toolCallId":"tool-1","rawOutput":"terminal output"},"options":[{"optionId":"no","name":"no","kind":"reject_once"},{"optionId":"yes","name":"yes","kind":"allow_once"}]}})
        permission_response = json.loads(sys.stdin.readline())
        notify({"sessionUpdate":"tool_call_update","toolCallId":"tool-1","kind":"execute","status":"in_progress"})
        notify({"sessionUpdate":"tool_call_update","toolCallId":"tool-1","kind":"execute","status":"completed"})
        notify({"sessionUpdate":"usage_update","usage":{"inputTokens":1,"outputTokens":1}})
        notify({"sessionUpdate":"usage_update","usage":{"inputTokens":1,"outputTokens":1}})
        notify({"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"safe final text"}})
        respond(req_id, {"stopReason":"end_turn"})
    elif method == "session/close":
        respond(req_id, {})
        break
    elif method == "session/cancel":
        continue
`
}

func canaryProgressScript() string {
	return `import sys, json

config = {"model":"default"}
session_id = "session-canary-1"

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
        respond(req_id, {"protocolVersion": 1, "agentInfo":{"name":"fake","version":"1"}, "agentCapabilities":{"sessionCapabilities":{"close":{}}}})
    elif method == "session/new":
        respond(req_id, {"sessionId":session_id,"configOptions":[{"id":"model","name":"model","type":"select","currentValue":"default","options":[]}]})
    elif method == "session/set_config_option":
        respond(req_id, {"configOptions":[{"id":"model","name":"model","type":"select","currentValue":req["params"]["value"],"options":[]}]})
    elif method == "session/prompt":
        notify({"sessionUpdate":"tool_call","toolCallId":"raw secret","kind":"execute","status":"pending","rawInput":{"secret":"CANARY-INPUT"}})
        notify({"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"CANARY-TEXT final"}})
        respond(req_id, {"stopReason":"CANARY-STOP"})
    elif method == "session/close":
        respond(req_id, {})
        break
`
}
