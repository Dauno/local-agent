package cliprotocol

import (
	"strings"
	"testing"
)

func validWorkspace() Workspace {
	return Workspace{
		WorkingDirectory: "/home/dm/projects/local-agent",
		Projects: []Project{
			{Name: "workspace", Path: "/home/dm/projects/local-agent"},
			{Name: "api", Path: "/home/dm/projects/api"},
		},
	}
}

func validRunRequest() Request {
	req := NewRequest("run-1", MethodRun)
	req.Profile = &Profile{Model: "anthropic/model-name", Agent: "build", Approval: ApprovalAuto, Variant: "high"}
	req.SystemInstruction = "You are Dev Agent."
	req.Messages = []Message{
		{Role: RoleUser, Text: "Review the API contract."},
		{Role: RoleAssistant, Text: "Which endpoint?"},
		{Role: RoleUser, Text: "Authentication."},
	}
	ws := validWorkspace()
	req.Workspace = &ws
	return req
}

func TestRequestRoundTrip(t *testing.T) {
	original := validRunRequest()
	line, err := EncodeLine(original)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.HasSuffix(string(line), "\n") {
		t.Fatalf("encoded line must end with newline")
	}
	decoded, err := DecodeRequest(line)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Method != MethodRun || decoded.Profile.Model != "anthropic/model-name" {
		t.Fatalf("unexpected decoded request: %+v", decoded)
	}
	if len(decoded.Messages) != 3 || decoded.Messages[2].Text != "Authentication." {
		t.Fatalf("messages not preserved: %+v", decoded.Messages)
	}
	if len(decoded.Workspace.Projects) != 2 {
		t.Fatalf("projects not preserved: %+v", decoded.Workspace)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	cases := []Response{
		NewDescription("d-1", "opencode", "v0.1.0", "1.17.20", []string{"text", "native_tools"}),
		NewValidated("v-1"),
		NewActivity("run-1", "tool", "bash", "completed"),
		NewResult("run-1", "final answer"),
		NewError("run-1", CodeProcessFailed, "exited early", false),
	}
	for _, original := range cases {
		line, err := EncodeLine(original)
		if err != nil {
			t.Fatalf("encode %s: %v", original.Type, err)
		}
		decoded, err := DecodeResponse(line)
		if err != nil {
			t.Fatalf("decode %s: %v", original.Type, err)
		}
		if decoded.Type != original.Type || decoded.ID != original.ID {
			t.Fatalf("round-trip mismatch: %+v vs %+v", decoded, original)
		}
	}
}

func TestResultCarriesStopFinishReason(t *testing.T) {
	if got := NewResult("x", "hi").FinishReason; got != FinishReasonStop {
		t.Fatalf("finish reason = %q, want %q", got, FinishReasonStop)
	}
}

func TestValidateRequestRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{"wrong protocol", func(r *Request) { r.Protocol = "other" }},
		{"wrong version", func(r *Request) { r.Version = 2 }},
		{"empty id", func(r *Request) { r.ID = "" }},
		{"unknown method", func(r *Request) { r.Method = "explode" }},
		{"missing profile", func(r *Request) { r.Profile = nil }},
		{"empty model", func(r *Request) { r.Profile.Model = "" }},
		{"bad approval", func(r *Request) { r.Profile.Approval = "maybe" }},
		{"empty messages", func(r *Request) { r.Messages = nil }},
		{"bad role", func(r *Request) { r.Messages[0].Role = "system" }},
		{"empty message text", func(r *Request) { r.Messages[2].Text = "  " }},
		{"final not user", func(r *Request) { r.Messages = r.Messages[:2] }},
		{"missing workspace", func(r *Request) { r.Workspace = nil }},
		{"duplicate project", func(r *Request) {
			r.Workspace.Projects[1].Name = "workspace"
		}},
		{"relative project path", func(r *Request) {
			r.Workspace.Projects[1].Path = "relative/api"
		}},
		{"non-canonical path", func(r *Request) {
			r.Workspace.Projects[1].Path = "/home/dm/projects/../projects/api"
		}},
		{"working dir not registered", func(r *Request) {
			r.Workspace.WorkingDirectory = "/home/dm/projects/elsewhere"
		}},
		{"empty project name", func(r *Request) { r.Workspace.Projects[0].Name = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := validRunRequest()
			tc.mutate(&req)
			if err := ValidateRequest(req); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestValidateDescribeRequest(t *testing.T) {
	req := NewRequest("d-1", MethodDescribe)
	if err := ValidateRequest(req); err != nil {
		t.Fatalf("describe request should validate: %v", err)
	}
}

func TestValidateValidateRequestRequiresWorkspace(t *testing.T) {
	req := NewRequest("v-1", MethodValidate)
	req.Profile = &Profile{Model: "m"}
	if err := ValidateRequest(req); err == nil {
		t.Fatalf("validate request without workspace should fail")
	}
	ws := validWorkspace()
	req.Workspace = &ws
	if err := ValidateRequest(req); err != nil {
		t.Fatalf("validate request should pass: %v", err)
	}
}

func TestValidateResponseRejects(t *testing.T) {
	tests := []struct {
		name string
		resp Response
	}{
		{"unknown type", Response{Protocol: Protocol, Version: Version, ID: "x", Type: "mystery"}},
		{"description without name", Response{Protocol: Protocol, Version: Version, ID: "x", Type: TypeDescription}},
		{"description without shim version", NewDescription("x", "shim", "", "1.0.0", []string{CapabilityText})},
		{"description without CLI version", NewDescription("x", "shim", "v1", "", []string{CapabilityText})},
		{"description without capabilities", NewDescription("x", "shim", "v1", "1.0.0", nil)},
		{"description without text capability", NewDescription("x", "shim", "v1", "1.0.0", []string{"native_tools"})},
		{"description with duplicate capability", NewDescription("x", "shim", "v1", "1.0.0", []string{CapabilityText, CapabilityText})},
		{"activity without kind", Response{Protocol: Protocol, Version: Version, ID: "x", Type: TypeActivity, Name: "bash", Status: ActivityStatusCompleted}},
		{"activity without name", NewActivity("x", ActivityKindTool, "", ActivityStatusCompleted)},
		{"activity with invalid status", NewActivity("x", ActivityKindTool, "bash", "running")},
		{"result without text", Response{Protocol: Protocol, Version: Version, ID: "x", Type: TypeResult}},
		{"result without stop finish reason", Response{Protocol: Protocol, Version: Version, ID: "x", Type: TypeResult, Text: "ok"}},
		{"error without code", Response{Protocol: Protocol, Version: Version, ID: "x", Type: TypeError, Message: "failed"}},
		{"error with unknown code", NewError("x", "new_code", "failed", false)},
		{"error without message", NewError("x", CodeProcessFailed, "", false)},
		{"wrong version", Response{Protocol: Protocol, Version: 99, ID: "x", Type: TypeValidated}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateResponse(tc.resp); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := []string{TypeDescription, TypeValidated, TypeResult, TypeError}
	for _, ty := range terminal {
		if !IsTerminal(ty) {
			t.Errorf("%s should be terminal", ty)
		}
	}
	if IsTerminal(TypeActivity) {
		t.Errorf("activity must not be terminal")
	}
}

func TestTerminalTypeForMethod(t *testing.T) {
	cases := map[string]string{
		MethodDescribe: TypeDescription,
		MethodValidate: TypeValidated,
		MethodRun:      TypeResult,
	}
	for method, want := range cases {
		got, ok := TerminalTypeForMethod(method)
		if !ok || got != want {
			t.Errorf("method %s => %s (%v), want %s", method, got, ok, want)
		}
	}
	if _, ok := TerminalTypeForMethod("nope"); ok {
		t.Errorf("unknown method should not resolve a terminal type")
	}
}
