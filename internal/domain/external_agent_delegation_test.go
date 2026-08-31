package domain_test

import (
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestExternalAgentDelegationRoundTrip(t *testing.T) {
	t.Parallel()
	encoded, err := domain.EncodeExternalAgentDelegation(
		"Inspect the repository.",
		"Presenta el resultado en español.",
	)
	if err != nil {
		t.Fatal(err)
	}
	delegation, versioned, err := domain.DecodeExternalAgentDelegation(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !versioned || delegation.Version != "delegation_v1" || delegation.Task != "Inspect the repository." ||
		delegation.FinalInstruction != "Presenta el resultado en español." {
		t.Fatalf("delegation = %#v, versioned = %v", delegation, versioned)
	}
	executionTask, err := domain.ExternalAgentExecutionTask(encoded)
	if err != nil || executionTask != delegation.Task {
		t.Fatalf("execution task = %q, err = %v", executionTask, err)
	}
}

func TestExternalAgentDelegationKeepsLegacyTask(t *testing.T) {
	t.Parallel()
	const task = `{"version":"ordinary-user-json","operation":"inspect"}`
	delegation, versioned, err := domain.DecodeExternalAgentDelegation(task)
	if err != nil {
		t.Fatal(err)
	}
	if versioned || delegation.Version != "legacy" || delegation.Task != task || delegation.FinalInstruction != "" {
		t.Fatalf("legacy delegation = %#v, versioned = %v", delegation, versioned)
	}
}

func TestExternalAgentDelegationRejectsInvalidEnvelope(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"local-agent-delegation:v1\n{",
		`local-agent-delegation:v1
{"version":"delegation_v1","task":"task","final_instruction":""}`,
	} {
		if _, _, err := domain.DecodeExternalAgentDelegation(value); err == nil {
			t.Fatalf("invalid envelope was accepted: %q", value)
		}
	}
}

func TestExternalAgentDelegationRejectsMissingFieldsAndOversize(t *testing.T) {
	t.Parallel()
	if _, err := domain.EncodeExternalAgentDelegation("", "Present result."); err == nil {
		t.Fatal("empty task was accepted")
	}
	if _, err := domain.EncodeExternalAgentDelegation("task", ""); err == nil {
		t.Fatal("empty final instruction was accepted")
	}
	if _, err := domain.EncodeExternalAgentDelegation(strings.Repeat("x", domain.MaxExternalAgentTaskRunes), "Present result."); err == nil {
		t.Fatal("oversize envelope was accepted")
	}
}
