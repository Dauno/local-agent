package adkagent

import (
	"testing"

	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

func TestExtractConfirmationPreservesHostHint(t *testing.T) {
	payload := map[string]any{
		"action":            "cancel_workstream",
		"expected_revision": 2,
		"payload_digest":    "digest-1",
		"workstream_id":     "ws-1",
	}
	call := &genai.FunctionCall{
		ID: "wrapper-1",
		Args: map[string]any{
			"originalFunctionCall": &genai.FunctionCall{ID: "original-1", Name: "workstream_transition", Args: map[string]any{"workstream_id": "ws-1", "action": "cancel_workstream"}},
			"toolConfirmation":     toolconfirmation.ToolConfirmation{Hint: "Approve workstream ws-1 at revision 2.", Payload: payload},
		},
	}

	confirmation := extractConfirmation(call)
	if confirmation == nil {
		t.Fatal("extractConfirmation returned nil")
	}
	if confirmation.Summary != "Approve workstream ws-1 at revision 2." {
		t.Fatalf("summary = %q", confirmation.Summary)
	}
	if confirmation.Payload != `{"action":"cancel_workstream","expected_revision":2,"payload_digest":"digest-1","workstream_id":"ws-1"}` {
		t.Fatalf("payload = %q", confirmation.Payload)
	}
}

func TestExtractConfirmationRejectsWorkstreamWithoutHostPayload(t *testing.T) {
	call := &genai.FunctionCall{ID: "wrapper-1", Args: map[string]any{
		"originalFunctionCall": &genai.FunctionCall{ID: "original-1", Name: "workstream_transition", Args: map[string]any{"action": "cancel_workstream"}},
		"toolConfirmation":     toolconfirmation.ToolConfirmation{Hint: "Approve cancellation."},
	}}
	if confirmation := extractConfirmation(call); confirmation != nil {
		t.Fatalf("confirmation = %#v, want fail-closed rejection", confirmation)
	}
}

// TestExtractConfirmationRendersPayloadForAnyHostIssuedHint pins hallazgo 9:
// confirmationPayload no longer name-matches "workstream_"; it renders the
// payload for any tool call that presented a custom, non-generic hint,
// which is exactly the signal that the tool called RequestConfirmation with
// a real host-issued payload (as the durable ACP delegation tool now does).
func TestExtractConfirmationRendersPayloadForAnyHostIssuedHint(t *testing.T) {
	payload := map[string]any{
		"project": "workspace", "task": "review the failing test", "workstream_id": "ws-1", "expected_revision": 4,
	}
	call := &genai.FunctionCall{
		ID: "wrapper-1",
		Args: map[string]any{
			"originalFunctionCall": &genai.FunctionCall{ID: "original-1", Name: "opencode_worker", Args: map[string]any{"project": "workspace", "task": "review the failing test"}},
			"toolConfirmation":     toolconfirmation.ToolConfirmation{Hint: `Approve delegating workstream "ws-1" task "review the failing test" (project "workspace") to an external agent at revision 4.`, Payload: payload},
		},
	}
	confirmation := extractConfirmation(call)
	if confirmation == nil {
		t.Fatal("extractConfirmation returned nil")
	}
	if confirmation.Payload != `{"expected_revision":4,"project":"workspace","task":"review the failing test","workstream_id":"ws-1"}` {
		t.Fatalf("payload = %q", confirmation.Payload)
	}
}

// TestExtractConfirmationRejectsCustomHintWithoutHostPayload pins the fail-
// closed side of the same fix: a non-generic hint without a payload is a
// defect for any tool, not just workstream_ ones.
func TestExtractConfirmationRejectsCustomHintWithoutHostPayload(t *testing.T) {
	call := &genai.FunctionCall{ID: "wrapper-1", Args: map[string]any{
		"originalFunctionCall": &genai.FunctionCall{ID: "original-1", Name: "opencode_worker", Args: map[string]any{"task": "review"}},
		"toolConfirmation":     toolconfirmation.ToolConfirmation{Hint: "Approve delegating task to an external agent."},
	}}
	if confirmation := extractConfirmation(call); confirmation != nil {
		t.Fatalf("confirmation = %#v, want fail-closed rejection", confirmation)
	}
}

// TestExtractConfirmationToleratesGenericADKHintWithoutPayload pins the
// unchanged side for tools that still rely on the bare RequireConfirmation
// flag (never call RequestConfirmation): ADK's generic default hint, which
// always mentions "FunctionResponse", must keep proceeding with an empty
// payload rather than failing closed.
func TestExtractConfirmationToleratesGenericADKHintWithoutPayload(t *testing.T) {
	call := &genai.FunctionCall{ID: "wrapper-1", Args: map[string]any{
		"originalFunctionCall": &genai.FunctionCall{ID: "original-1", Name: "create_canvas", Args: map[string]any{}},
		"toolConfirmation":     toolconfirmation.ToolConfirmation{Hint: `Please approve or reject the tool call create_canvas() by responding with a FunctionResponse.`},
	}}
	confirmation := extractConfirmation(call)
	if confirmation == nil {
		t.Fatal("extractConfirmation returned nil for a bare-flag tool")
	}
	if confirmation.Payload != "" {
		t.Fatalf("payload = %q, want empty for a bare-flag tool", confirmation.Payload)
	}
	if confirmation.Summary != `Tool "create_canvas" requires confirmation` {
		t.Fatalf("summary = %q", confirmation.Summary)
	}
}

func TestExtractConfirmationReadsSerializedHostPayload(t *testing.T) {
	call := &genai.FunctionCall{ID: "wrapper-1", Args: map[string]any{
		"originalFunctionCall": map[string]any{
			"id": "original-1", "name": "workstream_transition",
			"args": map[string]any{"action": "cancel_workstream"},
		},
		"toolConfirmation": map[string]any{
			"hint":    "Approve cancellation.",
			"payload": map[string]any{"action": "cancel_workstream", "payload_digest": "digest-1"},
		},
	}}
	confirmation := extractConfirmation(call)
	if confirmation == nil || confirmation.Payload != `{"action":"cancel_workstream","payload_digest":"digest-1"}` {
		t.Fatalf("confirmation = %#v", confirmation)
	}
}
