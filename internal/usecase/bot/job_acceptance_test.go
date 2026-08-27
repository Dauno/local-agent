package bot

import (
	"context"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type fakeAcceptedJobReader struct {
	job           *domain.ExternalAgentJob
	wrapperCallID string
	actor         string
	conversation  domain.ConversationKey
}

func (r *fakeAcceptedJobReader) StatusByWrapperCallID(_ context.Context, wrapperCallID, actor string, conversation domain.ConversationKey) (*domain.ExternalAgentJob, error) {
	r.wrapperCallID = wrapperCallID
	r.actor = actor
	r.conversation = conversation
	return r.job, nil
}

type fakeJobAcceptancePublisher struct {
	jobs []domain.ExternalAgentJob
	err  error
}

func (p *fakeJobAcceptancePublisher) PublishJobAccepted(_ context.Context, job domain.ExternalAgentJob) error {
	p.jobs = append(p.jobs, job)
	return p.err
}

func TestHandleInteractiveConfirmationPublishesAcceptedJobReceipt(t *testing.T) {
	delivery := richConfirmationDelivery(t)
	confirmations := &fakeConfirmationStore{delivery: &delivery}
	runtime := &fakeRuntime{resumeTurn: port.AgentTurn{Text: "model acceptance text must not be the receipt"}}
	publisher := &fakePublisher{}
	richPublisher := &fakeConfirmationPublisher{}
	job := &domain.ExternalAgentJob{
		ID:              "job_123",
		WrapperCallID:   delivery.WrapperCallID,
		Actor:           delivery.Actor,
		ConversationKey: delivery.ConversationKey,
		Status:          domain.JobQueued,
		CreatedAt:       time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 7, 21, 15, 31, 0, 0, time.UTC),
	}
	reader := &fakeAcceptedJobReader{job: job}
	acceptancePublisher := &fakeJobAcceptancePublisher{}
	service := newTestServiceWithConfirmations(t, &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}, runtime, &fakeHistory{}, publisher, confirmations, nil)
	service.confirmationPublisher = richPublisher
	service.jobReader = reader
	service.jobAcceptancePublisher = acceptancePublisher

	if err := service.HandleConfirmationInteractive(t.Context(), richConfirmationAction(delivery)); err != nil {
		t.Fatal(err)
	}
	if len(acceptancePublisher.jobs) != 1 || acceptancePublisher.jobs[0].ID != job.ID {
		t.Fatalf("accepted jobs = %#v", acceptancePublisher.jobs)
	}
	if reader.wrapperCallID != delivery.WrapperCallID || reader.actor != delivery.Actor || reader.conversation != delivery.ConversationKey {
		t.Fatalf("job lookup binding = %#v", reader)
	}
	if len(richPublisher.updated) != 1 || richPublisher.updated[0].Status != port.ConfirmationConsumed {
		t.Fatalf("confirmation updates = %#v", richPublisher.updated)
	}
	if len(publisher.calls) != 0 {
		t.Fatalf("model acceptance text was published instead of the host receipt: %#v", publisher.calls)
	}
}
