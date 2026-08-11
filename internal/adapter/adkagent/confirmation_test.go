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
