package acpclient_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/acpclient"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestACPFakeAgent_Describe(t *testing.T) {
	client := acpclient.New("python3", []string{"-c", fakeACPAgentScript(false)})
	result, err := client.Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.ProtocolVersion != "1" || result.AgentInfo.Name != "fake-acp-agent" {
		t.Fatalf("description = %+v", result)
	}
	if !result.SessionCapabilities.Close {
		t.Fatalf("capabilities = %+v", result.SessionCapabilities)
	}
}

func TestACPFakeAgent_ProbeVerifiesFullConfigState(t *testing.T) {
	client := acpclient.New("python3", []string{"-c", fakeACPAgentScript(false)})
	err := client.Probe(t.Context(), t.TempDir(), []domain.ACPConfigOption{
		{ID: "model", Value: "test-model"},
		{ID: "mode", Value: "build"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestACPFakeAgent_RunCollectsOnlyAssistantTextAndHandlesPermission(t *testing.T) {
	client := acpclient.New("python3", []string{"-c", fakeACPAgentScript(true)})
	result, err := client.Run(t.Context(), domain.AcpInvocationRequest{
		PrimaryPath: t.TempDir(),
		ConfigOptions: []domain.ACPConfigOption{
			{ID: "model", Value: "test-model"},
			{ID: "mode", Value: "build"},
		},
		PermissionOptionKind: domain.ACPPermissionAllowOnce,
		GlobalInstruction:    "global",
		AgentInstruction:     "agent",
		Task:                 "task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "safe final text" {
		t.Fatalf("text = %q", result.Text)
	}
	for _, secret := range []string{"private thought", "terminal output", "raw input"} {
		if strings.Contains(result.Text, secret) {
			t.Fatalf("result leaked %q", secret)
		}
	}
}

func TestACPFakeAgentRunsDurableHooksBeforeUse(t *testing.T) {
	client := acpclient.New("python3", []string{"-c", fakeACPAgentScript(true)})
	var sessionID string
	sideEffectsPossible := false
	permissionChecked := false
	_, err := client.Run(t.Context(), domain.AcpInvocationRequest{
		PrimaryPath:          t.TempDir(),
		PermissionOptionKind: domain.ACPPermissionAllowOnce,
		Task:                 "durable hooks",
		OnSessionCreated: func(value string) error {
			sessionID = value
			return nil
		},
		OnSideEffectsPossible: func() error {
			sideEffectsPossible = true
			return nil
		},
		BeforePermission: func() error {
			permissionChecked = sessionID != "" && sideEffectsPossible
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != "session-real-1" || !sideEffectsPossible || !permissionChecked {
		t.Fatalf("session hook = %q, side-effects hook = %v, permission hook = %v", sessionID, sideEffectsPossible, permissionChecked)
	}
}

func TestACPFakeAgent_RunCollectsUpdateAfterPromptResponse(t *testing.T) {
	script := strings.Replace(fakeACPAgentScript(false), "import sys, json", "import sys, json, time", 1)
	script = strings.Replace(script,
		`notify({"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"safe final text"}})
        respond(req_id, {"stopReason":"end_turn"})`,
		`notify({"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"safe"}})
        respond(req_id, {"stopReason":"end_turn"})
        time.sleep(0.02)
        notify({"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":" final text"}})`, 1)
	client := acpclient.New("python3", []string{"-c", script})

	result, err := client.Run(t.Context(), domain.AcpInvocationRequest{
		PrimaryPath:          t.TempDir(),
		ConfigOptions:        []domain.ACPConfigOption{{ID: "model", Value: "test-model"}},
		PermissionOptionKind: domain.ACPPermissionRejectOnce,
		Task:                 "task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "safe final text" {
		t.Fatalf("text = %q", result.Text)
	}
}

func TestACPFakeAgent_RunAcceptsLargeJSONRPCMessage(t *testing.T) {
	script := strings.Replace(fakeACPAgentScript(false), `"text":"safe final text"`, `"text":"x" * 150000`, 1)
	client := acpclient.NewWithBounds("python3", []string{"-c", script}, acpclient.Bounds{MaxFrameBytes: 2 * 1024 * 1024, MaxInlineResultBytes: 200000})
	result, err := client.Run(t.Context(), domain.AcpInvocationRequest{
		PrimaryPath:          t.TempDir(),
		ConfigOptions:        []domain.ACPConfigOption{{ID: "model", Value: "test-model"}},
		PermissionOptionKind: domain.ACPPermissionRejectOnce,
		Task:                 "task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != strings.Repeat("x", 150000) {
		t.Fatalf("text length = %d, want 150000", len(result.Text))
	}
}

func TestACPFakeAgent_RunRejectsOversizedJSONRPCMessage(t *testing.T) {
	script := strings.Replace(fakeACPAgentScript(false), `"text":"safe final text"`, `"text":"x" * 1100000`, 1)
	client := acpclient.NewWithBounds("python3", []string{"-c", script}, acpclient.Bounds{MaxFrameBytes: 1024 * 1024})
	_, err := client.Run(t.Context(), domain.AcpInvocationRequest{
		PrimaryPath:          t.TempDir(),
		ConfigOptions:        []domain.ACPConfigOption{{ID: "model", Value: "test-model"}},
		PermissionOptionKind: domain.ACPPermissionRejectOnce,
		Task:                 "task",
	})
	if err == nil || !strings.Contains(err.Error(), "acp_frame_too_large") {
		t.Fatalf("error = %v", err)
	}
}

func TestACPFakeAgent_RejectsConfigFallback(t *testing.T) {
	script := strings.Replace(fakeACPAgentScript(false), `config[params["configId"]] = params["value"]`, `config[params["configId"]] = "fallback"`, 1)
	client := acpclient.New("python3", []string{"-c", script})
	err := client.Probe(t.Context(), t.TempDir(), []domain.ACPConfigOption{{ID: "model", Value: "selected"}})
	if err == nil || !strings.Contains(err.Error(), "was not retained") {
		t.Fatalf("error = %v", err)
	}
}

func TestACPFakeAgent_RejectsWrongProtocol(t *testing.T) {
	script := strings.Replace(fakeACPAgentScript(false), `"protocolVersion": 1`, `"protocolVersion": 2`, 1)
	_, err := acpclient.New("python3", []string{"-c", script}).Describe(t.Context())
	if err == nil {
		t.Fatal("expected protocol rejection")
	}
}

func TestACPFakeAgent_RejectsMalformedResponse(t *testing.T) {
	client := acpclient.New("python3", []string{"-c", `import sys
sys.stdin.readline()
sys.stdout.write("not json\n")
sys.stdout.flush()
`})
	_, err := client.Describe(t.Context())
	if err == nil {
		t.Fatal("expected malformed response rejection")
	}
}

func TestACPFakeAgent_CancellationKillsProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	client := acpclient.New("python3", []string{"-c", fakeACPBlockingScript()})
	started := time.Now()
	_, err := client.Run(ctx, domain.AcpInvocationRequest{
		PrimaryPath:          t.TempDir(),
		ConfigOptions:        []domain.ACPConfigOption{{ID: "model", Value: "test"}},
		PermissionOptionKind: domain.ACPPermissionRejectOnce,
		Task:                 "task",
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

func fakeACPAgentScript(permission bool) string {
	permissionBlock := ""
	if permission {
		permissionBlock = `
        notify({"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"private thought"}})
        notify({"sessionUpdate":"tool_call","toolCallId":"tool-1","kind":"execute","status":"pending","rawInput":{"secret":"raw input"}})
        send({"jsonrpc":"2.0","id":"permission-1","method":"session/request_permission","params":{"sessionId":session_id,"toolCall":{"toolCallId":"tool-1","rawOutput":"terminal output"},"options":[{"optionId":"yes","name":"yes","kind":"allow_once"},{"optionId":"no","name":"no","kind":"reject_once"}]}})
        permission_response = json.loads(sys.stdin.readline())
        if permission_response.get("result",{}).get("outcome",{}).get("optionId") != "yes":
            send({"jsonrpc":"2.0","id":req_id,"error":{"code":-32000,"message":"wrong permission"}})
            continue
`
	}
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
        if "additionalDirectories" in params:
            send({"jsonrpc":"2.0","id":req_id,"error":{"code":-32602,"message":"additionalDirectories forbidden"}})
        else:
            respond(req_id, {"sessionId":session_id,"configOptions":options()})
    elif method == "session/set_config_option":
        config[params["configId"]] = params["value"]
        respond(req_id, {"configOptions":options()})
    elif method == "session/prompt":` + permissionBlock + `
        notify({"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"safe final text"}})
        respond(req_id, {"stopReason":"end_turn"})
    elif method == "session/close":
        respond(req_id, {})
        break
    elif method == "session/cancel":
        continue
`
}

func fakeACPBlockingScript() string {
	return `import sys, json, time
config = {"model":"default"}
for line in sys.stdin:
    req = json.loads(line)
    method = req.get("method")
    req_id = req.get("id")
    if method == "initialize": result = {"protocolVersion":1,"agentInfo":{"name":"fake","version":"1"},"agentCapabilities":{"sessionCapabilities":{}}}
    elif method == "session/new": result = {"sessionId":"session-1","configOptions":[{"id":"model","name":"model","type":"select","currentValue":"default","options":[]}]}
    elif method == "session/set_config_option": result = {"configOptions":[{"id":"model","name":"model","type":"select","currentValue":req["params"]["value"],"options":[]}]}
    elif method == "session/prompt":
        time.sleep(30)
        continue
    else: continue
    sys.stdout.write(json.dumps({"jsonrpc":"2.0","id":req_id,"result":result})+"\n")
    sys.stdout.flush()
`
}

func init() {
	if _, err := exec.LookPath("python3"); err != nil {
		panic("python3 is required for ACP fake agent tests")
	}
}
