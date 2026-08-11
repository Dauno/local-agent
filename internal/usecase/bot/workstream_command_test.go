package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestParseHumanWorkstreamCommandBuildsTypedTransition(t *testing.T) {
	command, handled, err := parseHumanWorkstreamCommand(`workstream-human {"project":"workspace","workstream_id":"ws-1","expected_revision":4,"action":"resolve_question","question_id":"q-1","question_resolution":"approved by owner"}`)
	if err != nil || !handled {
		t.Fatalf("parse command: handled=%v err=%v", handled, err)
	}
	if command.Transition.Action != domain.WorkstreamActionResolveQuestion || command.Transition.QuestionID != "q-1" || command.Transition.QuestionResolution != "approved by owner" {
		t.Fatalf("transition = %+v", command.Transition)
	}
}

func TestConfirmationPromptIncludesPayloadBeforeApprovalInstructions(t *testing.T) {
	payload := `{"action":"cancel_workstream","workstream_id":"ws-1"}`
	prompt := confirmationPrompt("Approve cancellation", payload, "call-1", "wrapper-1", time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC))
	payloadIndex := strings.Index(prompt, payload)
	approvalIndex := strings.Index(prompt, "Reply `approve wrapper-1`")
	if payloadIndex < 0 || approvalIndex < 0 || payloadIndex > approvalIndex {
		t.Fatalf("confirmation prompt does not present payload before approval instructions: %q", prompt)
	}
}

func TestParseHumanWorkstreamCommandRejectsUnknownFields(t *testing.T) {
	_, handled, err := parseHumanWorkstreamCommand(`workstream-human {"project":"workspace","workstream_id":"ws-1","action":"pause_workstream","unknown":true}`)
	if !handled || err == nil {
		t.Fatalf("handled=%v err=%v, want unknown field error", handled, err)
	}
}
