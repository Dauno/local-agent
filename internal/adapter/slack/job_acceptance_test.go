package slack

import (
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

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
	fallback, blocks, err := compileJobAcceptedMessage(mustEmbeddedRenderer(t), acceptedTestJob())
	if err != nil {
		t.Fatalf("compileJobAcceptedMessage() error = %v", err)
	}
	if fallback == "" || !strings.Contains(fallback, "job_123") || !strings.Contains(fallback, "queued") {
		t.Fatalf("fallback = %q", fallback)
	}
	if !strings.Contains(fallback, "2026-07-21T20:30:00Z") || !strings.Contains(fallback, "2026-07-21T20:31:00Z") {
		t.Fatalf("fallback timestamps = %q", fallback)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	for _, block := range blocks {
		if _, ok := block.(*slackapi.ActionBlock); ok {
			t.Fatal("accepted receipt must not contain interactive elements")
		}
	}
	card := blocks[0].(*slackapi.CardBlock)
	if !strings.Contains(card.Subtitle.Text, "job_123") || !strings.Contains(card.Subtitle.Text, "queued") {
		t.Fatalf("card subtitle = %#v", card.Subtitle)
	}
	if card.Body == nil || card.Body.Text != jobAcceptedStatusSentence {
		t.Fatalf("card body = %#v", card.Body)
	}
	context := blocks[1].(*slackapi.ContextBlock)
	if len(context.ContextElements.Elements) != 2 {
		t.Fatalf("context elements = %#v", context.ContextElements.Elements)
	}
	created := context.ContextElements.Elements[0].(*slackapi.TextBlockObject)
	updated := context.ContextElements.Elements[1].(*slackapi.TextBlockObject)
	if !strings.Contains(created.Text, "2026-07-21T20:30:00Z") || !strings.Contains(updated.Text, "2026-07-21T20:31:00Z") {
		t.Fatalf("timestamp elements = %#v, %#v", created, updated)
	}
	if err := validateJobAcceptedMessageLimits(fallback, blocks); err != nil {
		t.Fatalf("validateJobAcceptedMessageLimits() error = %v", err)
	}
}

func TestCompileJobAcceptedMessageNeutralizesAndEscapesJobID(t *testing.T) {
	job := acceptedTestJob()
	job.ID = "job-<!channel>-<@U12345678>"
	_, blocks, err := compileJobAcceptedMessage(mustEmbeddedRenderer(t), job)
	if err != nil {
		t.Fatalf("compileJobAcceptedMessage() error = %v", err)
	}
	text := blocks[0].(*slackapi.CardBlock).Subtitle.Text
	if strings.Contains(text, "<!channel>") || strings.Contains(text, "<@U12345678>") {
		t.Fatalf("unsafe Slack controls remain in %q", text)
	}
	if !strings.Contains(text, "&amp;") || !strings.Contains(text, "&gt;") {
		t.Fatalf("job ID was not escaped as mrkdwn: %q", text)
	}
}

func TestCompileJobAcceptedMessageRespectsLimits(t *testing.T) {
	job := acceptedTestJob()
	job.ID = strings.Repeat("x", maxFallbackText*2)
	fallback, blocks, err := compileJobAcceptedMessage(mustEmbeddedRenderer(t), job)
	if err != nil {
		t.Fatalf("compileJobAcceptedMessage() error = %v", err)
	}
	if len([]rune(fallback)) > maxFallbackText || len(blocks) > maxBlocksPerMessage {
		t.Fatalf("compiled receipt exceeds limits: fallback=%d blocks=%d", len([]rune(fallback)), len(blocks))
	}
	card := blocks[0].(*slackapi.CardBlock)
	if card.Title == nil || len([]rune(card.Title.Text)) > maxRendererCardTitleLength {
		t.Fatalf("card title exceeds limit: %#v", card.Title)
	}
	if card.Subtitle == nil || len([]rune(card.Subtitle.Text)) > maxRendererCardSubtitleLength {
		t.Fatalf("card subtitle exceeds limit: %#v", card.Subtitle)
	}
	if card.Body == nil || len([]rune(card.Body.Text)) > maxRendererCardBodyLength {
		t.Fatalf("card body exceeds limit: %#v", card.Body)
	}
	for index, block := range blocks {
		section, ok := block.(*slackapi.SectionBlock)
		if !ok {
			continue
		}
		if section.Text != nil && len([]rune(section.Text.Text)) > maxRendererCompositionTextLength {
			t.Fatalf("block %d text exceeds composition limit", index)
		}
		for _, field := range section.Fields {
			if len([]rune(field.Text)) > maxRendererSectionFieldLength {
				t.Fatalf("block %d field exceeds section field limit", index)
			}
		}
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
			if _, _, err := compileJobAcceptedMessage(mustEmbeddedRenderer(t), job); err == nil {
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
