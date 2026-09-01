package domain_test

import (
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestConversationActivationFrameCarriesProvenanceWithoutWorkstreamAuthority(t *testing.T) {
	const task = "inspect the current repository"
	digest := strings.Repeat("a", 64)
	frame := domain.ActivationFrame{
		ActivationID: "activation-conversation", JobID: "job-conversation",
		ActivationScope: domain.ExternalAgentActivationConversation,
		Actor:           "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678",
		TerminalStatus: domain.JobCompleted, PrimaryProject: "workspace",
		DelegatedTaskExcerpt: task, DelegatedTaskSHA256: digest,
		WorkstreamID: "stale-workstream", TaskID: "stale-task", ExecutionIdentity: "stale-execution", AdmissionRevision: 4,
		Representation: domain.ActivationResultUnavailable,
	}
	if err := frame.Validate(); err != nil {
		t.Fatalf("conversation frame with provenance failed validation: %v", err)
	}
	rendered, err := frame.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, `"activation_scope":"conversation"`) || !strings.Contains(rendered, `"workstream_id":"stale-workstream"`) {
		t.Fatalf("conversation frame omitted scope or provenance: %s", rendered)
	}
	if strings.Contains(rendered, `"workstream":`) || strings.Contains(rendered, `"task":`) {
		t.Fatalf("conversation frame carried workstream authority: %s", rendered)
	}
	if !strings.Contains(rendered, `"proposal_policy":"No proposal is allowed`) {
		t.Fatalf("conversation frame did not carry the no-proposal policy: %s", rendered)
	}
}
