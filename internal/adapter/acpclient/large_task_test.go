package acpclient_test

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/adapter/acpclient"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestACPFramesDoNotUseCumulativeStdoutBudget(t *testing.T) {
	const frames = 10_000
	script := fakeManyFramesScript(frames, 12*1024)
	client := acpclient.NewWithBounds("python3", []string{"-c", script}, acpclient.Bounds{
		MaxFrameBytes:        32 * 1024,
		MaxInlineResultBytes: 64 * 1024,
		StderrTailBytes:      1024,
	})

	result, err := client.Run(t.Context(), domain.AcpInvocationRequest{
		PrimaryPath:          t.TempDir(),
		PermissionOptionKind: domain.ACPPermissionRejectOnce,
		Task:                 "large valid task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "final" {
		t.Fatalf("result text = %q", result.Text)
	}
}

func TestACPFrameOneByteOverLimitReturnsTypedError(t *testing.T) {
	script := fakeACPAgentScript(true, false)
	client := acpclient.NewWithBounds("python3", []string{"-c", script}, acpclient.Bounds{
		MaxFrameBytes:        256,
		MaxInlineResultBytes: 128,
		StderrTailBytes:      128,
	})

	_, err := client.Run(t.Context(), domain.AcpInvocationRequest{
		PrimaryPath:          t.TempDir(),
		PermissionOptionKind: domain.ACPPermissionRejectOnce,
		Task:                 strings.Repeat("x", 400),
	})
	if err == nil || !strings.Contains(err.Error(), string(domain.ACPErrorFrameTooLarge)) {
		t.Fatalf("error = %v, want %s", err, domain.ACPErrorFrameTooLarge)
	}
}

func TestACPCollectorUsesFinalAssistantMessageID(t *testing.T) {
	client := acpclient.New("python3", []string{"-c", fakeMessageIDScript()})
	result, err := client.Run(t.Context(), domain.AcpInvocationRequest{
		PrimaryPath:          t.TempDir(),
		PermissionOptionKind: domain.ACPPermissionRejectOnce,
		Task:                 "message ids",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "final answer" {
		t.Fatalf("result text = %q, progress leaked", result.Text)
	}
}

func TestACPLargeFinalResultReturnsCompleteBoundedText(t *testing.T) {
	client := acpclient.NewWithBounds("python3", []string{"-c", fakeACPAgentScript(true, false)}, acpclient.Bounds{
		MaxFrameBytes:          128 * 1024,
		MaxInlineResultBytes:   4,
		MaxResultArtifactBytes: 4096,
		StderrTailBytes:        1024,
	})

	result, err := client.Run(t.Context(), domain.AcpInvocationRequest{
		PrimaryPath:          t.TempDir(),
		PermissionOptionKind: domain.ACPPermissionRejectOnce,
		Task:                 "artifact result",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Inline || result.ArtifactRef != "" || result.Text != "safe final text" || result.ResultBytes != int64(len(result.Text)) {
		t.Fatalf("result metadata = %+v", result)
	}
}

func TestACPResultKeepsUTF8TextForHostMaterialization(t *testing.T) {
	script := strings.Replace(fakeACPAgentScript(true, false), `"text":"safe final text"`, `"text":"🚀🚀"`, 1)
	client := acpclient.NewWithBounds("python3", []string{"-c", script}, acpclient.Bounds{
		MaxFrameBytes: 64 * 1024, MaxInlineResultBytes: 5, MaxResultArtifactBytes: 4096, StderrTailBytes: 1024,
	})
	result, err := client.Run(t.Context(), domain.AcpInvocationRequest{PrimaryPath: t.TempDir(), Task: "byte preview"})
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(result.Text) || result.Text != "🚀🚀" {
		t.Fatalf("result = %q, valid UTF-8 = %v", result.Text, utf8.ValidString(result.Text))
	}
}

func fakeManyFramesScript(frames, payloadBytes int) string {
	return `import sys, json
session_id = "session-large"
payload = "x" * ` + strconv.Itoa(payloadBytes) + `
def send(value):
    sys.stdout.write(json.dumps(value) + "\n")
    sys.stdout.flush()
for line in sys.stdin:
    req = json.loads(line)
    method = req.get("method")
    req_id = req.get("id")
    if method == "initialize":
        send({"jsonrpc":"2.0","id":req_id,"result":{"protocolVersion":1,"agentInfo":{"name":"large","version":"1"},"agentCapabilities":{"sessionCapabilities":{}}}})
    elif method == "session/new":
        send({"jsonrpc":"2.0","id":req_id,"result":{"sessionId":session_id,"configOptions":[]}})
    elif method == "session/prompt":
        for _ in range(` + strconv.Itoa(frames) + `):
            send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session_id,"update":{"sessionUpdate":"usage_update","usage":{"inputTokens":1,"outputTokens":1,"payload":payload}}}})
        send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session_id,"update":{"sessionUpdate":"agent_message_chunk","messageId":"final","content":{"type":"text","text":"final"}}}})
        send({"jsonrpc":"2.0","id":req_id,"result":{"stopReason":"end_turn"}})
    elif method == "session/close":
        send({"jsonrpc":"2.0","id":req_id,"result":{}})
        break
`
}

func fakeMessageIDScript() string {
	return `import sys, json
session_id = "session-messages"
def send(value):
    sys.stdout.write(json.dumps(value) + "\n")
    sys.stdout.flush()
for line in sys.stdin:
    req = json.loads(line)
    method = req.get("method")
    req_id = req.get("id")
    if method == "initialize":
        send({"jsonrpc":"2.0","id":req_id,"result":{"protocolVersion":1,"agentInfo":{"name":"messages","version":"1"},"agentCapabilities":{"sessionCapabilities":{}}}})
    elif method == "session/new":
        send({"jsonrpc":"2.0","id":req_id,"result":{"sessionId":session_id,"configOptions":[]}})
    elif method == "session/prompt":
        for message_id, text in [("progress-1", "progress"), ("progress-2", "more progress"), ("final-1", "final answer")]:
            send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session_id,"update":{"sessionUpdate":"agent_message_chunk","messageId":message_id,"content":{"type":"text","text":text}}}})
        send({"jsonrpc":"2.0","id":req_id,"result":{"stopReason":"end_turn"}})
    elif method == "session/close":
        send({"jsonrpc":"2.0","id":req_id,"result":{}})
        break
`
}
