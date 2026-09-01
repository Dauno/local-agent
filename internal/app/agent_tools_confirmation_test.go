package app

import (
	"context"
	"strings"
	"testing"
)

func TestExternalAgentDelegationConfirmationKeepsCompleteTaskOutOfHint(t *testing.T) {
	task := strings.Repeat("review the README and report findings ", 20)
	hint, payload := externalAgentDelegationConfirmation(
		context.Background(), nil, "actor", "conversation", externalAgentArgs{Project: "local-agent", Task: task, FinalInstruction: "Present the result."},
	)

	if strings.Contains(hint, task) {
		t.Fatal("confirmation hint contains the complete delegated task")
	}
	if hint != `Approve external-agent delegation for project "local-agent".` {
		t.Fatalf("confirmation hint = %q", hint)
	}
	if payload["project"] != "local-agent" || payload["task"] != task {
		t.Fatalf("confirmation payload = %#v", payload)
	}
}
