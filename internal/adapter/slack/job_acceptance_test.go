package slack

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/blockkit"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func mustJobEngine(t *testing.T) *blockkit.Engine {
	t.Helper()
	engine, err := newJobEngine()
	if err != nil {
		t.Fatalf("new job view engine: %v", err)
	}
	return engine
}

func acceptedTestJob() domain.ExternalAgentJob {
	return domain.ExternalAgentJob{
		ID:              "job_123",
		Status:          domain.JobQueued,
		ConversationKey: domain.ConversationKey("slack:T12345678:channel:C12345678:thread:1700000000.000001"),
		CreatedAt:       time.Date(2026, 7, 21, 15, 30, 0, 0, time.FixedZone("local", -5*60*60)),
		UpdatedAt:       time.Date(2026, 7, 21, 15, 31, 0, 0, time.FixedZone("local", -5*60*60)),
	}
}

func TestCompileJobAcceptedMessageIncludesHostReceiptFields(t *testing.T) {
	fallback, blocks, err := compileJobAcceptedMessage(mustJobEngine(t), acceptedTestJob())
	if err != nil {
		t.Fatalf("compileJobAcceptedMessage() error = %v", err)
	}
	message := blockkit.Message{FallbackText: fallback, Blocks: blocks}
	for _, value := range []string{"job_123", "queued", "2026-07-21T20:30:00Z", "2026-07-21T20:31:00Z", jobAcceptedStatusSentence} {
		if !blockkit.Reachable(message, value) {
			t.Fatalf("accepted value %q did not reach the view", value)
		}
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "action_id") {
		t.Fatalf("accepted receipt contains interactive elements: %s", encoded)
	}
	if err := validateJobAcceptedMessageLimits(fallback, blocks); err != nil {
		t.Fatalf("validateJobAcceptedMessageLimits() error = %v", err)
	}
}

func TestCompileJobAcceptedMessageNeutralizesAndEscapesJobID(t *testing.T) {
	job := acceptedTestJob()
	job.ID = "job-<!channel>-<@U12345678>"
	_, blocks, err := compileJobAcceptedMessage(mustJobEngine(t), job)
	if err != nil {
		t.Fatalf("compileJobAcceptedMessage() error = %v", err)
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "<!channel>") || strings.Contains(string(encoded), "<@U12345678>") || !strings.Contains(string(encoded), `\u0026lt;`) {
		t.Fatalf("unsafe Slack controls were not safely rendered: %q", encoded)
	}
}

func TestCompileJobAcceptedMessageRespectsLimits(t *testing.T) {
	job := acceptedTestJob()
	job.ID = strings.Repeat("x", maxFallbackText*2)
	fallback, blocks, err := compileJobAcceptedMessage(mustJobEngine(t), job)
	if err != nil {
		t.Fatalf("compileJobAcceptedMessage() error = %v", err)
	}
	if len([]rune(fallback)) > maxFallbackText || len(blocks) > maxBlocksPerMessage {
		t.Fatalf("compiled receipt exceeds limits: fallback=%d blocks=%d", len([]rune(fallback)), len(blocks))
	}
	message := blockkit.Message{FallbackText: fallback, Blocks: blocks}
	if !blockkit.Reachable(message, strings.Repeat("x", 10)) {
		t.Fatal("truncated job ID did not reach the rendered view")
	}
}

func TestCompileJobAcceptedMessageRequiresReceiptFields(t *testing.T) {
	tests := []struct {
		name string
		edit func(*domain.ExternalAgentJob)
	}{
		{name: "job ID", edit: func(job *domain.ExternalAgentJob) { job.ID = "" }},
		{name: "status", edit: func(job *domain.ExternalAgentJob) { job.Status = "not-a-status" }},
		{name: "created timestamp", edit: func(job *domain.ExternalAgentJob) { job.CreatedAt = time.Time{} }},
		{name: "updated timestamp", edit: func(job *domain.ExternalAgentJob) { job.UpdatedAt = time.Time{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := acceptedTestJob()
			tt.edit(&job)
			if _, _, err := compileJobAcceptedMessage(mustJobEngine(t), job); err == nil {
				t.Fatal("compileJobAcceptedMessage() accepted incomplete receipt")
			}
		})
	}
}

func TestConfirmationPublisherPublishesJobAcceptanceAsNewMessage(t *testing.T) {
	client := &fakeConfirmationBlockClient{}
	publisher := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)
	job := acceptedTestJob()

	if err := publisher.PublishJobAccepted(t.Context(), job); err != nil {
		t.Fatalf("PublishJobAccepted() error = %v", err)
	}
	if len(client.postedBlocks) != 1 || len(client.updatedBlocks) != 0 {
		t.Fatalf("posted blocks = %d, updated blocks = %d", len(client.postedBlocks), len(client.updatedBlocks))
	}
	if client.postedChans[0] != "C12345678" || client.postedThreads[0] != "1700000000.000001" {
		t.Fatalf("posted target = %q thread %q", client.postedChans[0], client.postedThreads[0])
	}
}
