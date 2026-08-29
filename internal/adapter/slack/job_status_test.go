package slack

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/blockkit"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type fakeJobStatusEphemeralClient struct {
	calls []jobStatusEphemeralCall
	err   error
}

type jobStatusEphemeralCall struct {
	channelID string
	userID    string
	threadTS  string
	fallback  string
	blocks    []slackapi.Block
}

func (c *fakeJobStatusEphemeralClient) PostEphemeral(_ context.Context, channelID, userID, threadTS, fallbackText string, blocks []slackapi.Block) (string, error) {
	c.calls = append(c.calls, jobStatusEphemeralCall{
		channelID: channelID, userID: userID, threadTS: threadTS,
		fallback: fallbackText, blocks: blocks,
	})
	return "ephemeral-1", c.err
}

type fakeJobStatusConfirmationStore struct {
	delivery *port.ConfirmationDelivery
	err      error
	calls    int
}

func (s *fakeJobStatusConfirmationStore) CreateDelivery(context.Context, port.ConfirmationDelivery) error {
	return nil
}

func (s *fakeJobStatusConfirmationStore) MarkPublished(context.Context, string, string, string, string) error {
	return nil
}

func (s *fakeJobStatusConfirmationStore) MarkConsumed(context.Context, string) error { return nil }

func (s *fakeJobStatusConfirmationStore) RejectDelivery(context.Context, string) error { return nil }

func (s *fakeJobStatusConfirmationStore) GetByWrapperCallID(context.Context, string) (*port.ConfirmationDelivery, error) {
	s.calls++
	return s.delivery, s.err
}

func (s *fakeJobStatusConfirmationStore) ListPending(context.Context) ([]port.ConfirmationDelivery, error) {
	return nil, nil
}

func (s *fakeJobStatusConfirmationStore) ExpireDeliveries(context.Context, time.Time) error {
	return nil
}

var _ port.ConfirmationDeliveryStore = (*fakeJobStatusConfirmationStore)(nil)

type fakeJobStatusReader struct {
	job          *domain.ExternalAgentJob
	err          error
	calls        int
	wrapperCall  string
	actor        string
	conversation domain.ConversationKey
}

func (r *fakeJobStatusReader) StatusByWrapperCallID(_ context.Context, wrapperCallID, actor string, conversation domain.ConversationKey) (*domain.ExternalAgentJob, error) {
	r.calls++
	r.wrapperCall, r.actor, r.conversation = wrapperCallID, actor, conversation
	return r.job, r.err
}

var _ port.ExternalAgentJobWrapperReader = (*fakeJobStatusReader)(nil)

func statusTestDelivery() port.ConfirmationDelivery {
	return port.ConfirmationDelivery{
		WrapperCallID:   "wrapper-status",
		OriginalCallID:  "call-status",
		Actor:           "U12345678",
		TeamID:          "T12345678",
		ChannelID:       "C12345678",
		ThreadTS:        "1720000000.000001",
		ConversationKey: domain.ConversationKey("slack:T12345678:channel:C12345678:thread:1720000000.000001"),
		Status:          port.ConfirmationPublished,
		CorrelationID:   "confirmation:wrapper-status",
		SlackMessageTS:  "1720000001.000001",
		RendererMode:    confirmationRenderModeV2,
		Summary:         "Run a job",
		Expiry:          time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
}

func statusTestJob(delivery port.ConfirmationDelivery) *domain.ExternalAgentJob {
	return &domain.ExternalAgentJob{
		ID:              "job-status-1",
		WrapperCallID:   delivery.WrapperCallID,
		Actor:           delivery.Actor,
		TeamID:          delivery.TeamID,
		ConversationKey: delivery.ConversationKey,
		Status:          domain.JobRunning,
		Task:            "secret task text",
		ResultSummary:   "secret result text",
		ErrorCode:       "secret error code",
		CreatedAt:       time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 7, 21, 15, 5, 0, 0, time.UTC),
	}
}

func statusTestCallback(delivery port.ConfirmationDelivery, userID, channelID, threadTS string) slackapi.InteractionCallback {
	callback := newTestCallback(statusActionID, delivery.WrapperCallID, delivery.TeamID, userID, channelID, delivery.SlackMessageTS, threadTS)
	callback.Message.Metadata = confirmationMetadata(delivery)
	return callback
}

func TestNormalizeJobStatusActionPreservesWrapperCallID(t *testing.T) {
	delivery := statusTestDelivery()
	callback := statusTestCallback(delivery, delivery.Actor, delivery.ChannelID, delivery.ThreadTS)
	action, ok := normalizeJobStatusAction(&callback)
	if !ok {
		t.Fatal("normalizeJobStatusAction rejected a valid status action")
	}
	if action.WrapperCallID != delivery.WrapperCallID {
		t.Fatalf("wrapper call ID = %q, want %q", action.WrapperCallID, delivery.WrapperCallID)
	}
	if action.ConversationKey != delivery.ConversationKey || action.Actor != delivery.Actor {
		t.Fatalf("normalized action binding = %#v", action)
	}
}

func TestJobStatusHandlerPublishesAuthorizedEphemeralStatus(t *testing.T) {
	delivery := statusTestDelivery()
	client := &fakeJobStatusEphemeralClient{}
	reader := &fakeJobStatusReader{job: statusTestJob(delivery)}
	handler := newJobStatusHandler(client, time.Second, &fakeJobStatusConfirmationStore{delivery: &delivery}, reader)

	if err := handler.Handle(t.Context(), statusTestCallback(delivery, delivery.Actor, delivery.ChannelID, delivery.ThreadTS)); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(client.calls) != 1 {
		t.Fatalf("ephemeral calls = %d, want 1", len(client.calls))
	}
	call := client.calls[0]
	if call.channelID != delivery.ChannelID || call.userID != delivery.Actor || call.threadTS != delivery.ThreadTS {
		t.Fatalf("ephemeral target = %#v", call)
	}
	if !strings.Contains(call.fallback, "job-status-1") || !strings.Contains(call.fallback, "running") ||
		!strings.Contains(call.fallback, "2026-07-21T15:05:00Z") {
		t.Fatalf("ephemeral fallback = %q", call.fallback)
	}
	message := blockkit.Message{FallbackText: call.fallback, Blocks: call.blocks}
	for _, value := range []string{"job-status-1", "running", "2026-07-21T15:05:00Z", "The host is running the job."} {
		if !blockkit.Reachable(message, value) {
			t.Fatalf("status value %q did not reach the view", value)
		}
	}
	encoded, err := json.Marshal(call.blocks)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret task text", "secret result text", "secret error code"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("status response leaked %q", secret)
		}
	}
	if reader.calls != 1 || reader.wrapperCall != delivery.WrapperCallID || reader.actor != delivery.Actor || reader.conversation != delivery.ConversationKey {
		t.Fatalf("status reader call = %#v", reader)
	}
}

func TestJobStatusHandlerRejectsUnauthorizedUserWithoutReadingJob(t *testing.T) {
	delivery := statusTestDelivery()
	client := &fakeJobStatusEphemeralClient{}
	store := &fakeJobStatusConfirmationStore{delivery: &delivery}
	reader := &fakeJobStatusReader{job: statusTestJob(delivery)}
	handler := newJobStatusHandler(client, time.Second, store, reader)

	if err := handler.Handle(t.Context(), statusTestCallback(delivery, "U87654321", delivery.ChannelID, delivery.ThreadTS)); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	assertStatusAuthorizationFailure(t, client)
	if reader.calls != 0 {
		t.Fatalf("job reader calls = %d, want 0", reader.calls)
	}
}

func TestJobStatusHandlerRejectsWrongConversationWithoutReadingJob(t *testing.T) {
	delivery := statusTestDelivery()
	client := &fakeJobStatusEphemeralClient{}
	store := &fakeJobStatusConfirmationStore{delivery: &delivery}
	reader := &fakeJobStatusReader{job: statusTestJob(delivery)}
	handler := newJobStatusHandler(client, time.Second, store, reader)

	if err := handler.Handle(t.Context(), statusTestCallback(delivery, delivery.Actor, "C87654321", delivery.ThreadTS)); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	assertStatusAuthorizationFailure(t, client)
	if reader.calls != 0 {
		t.Fatalf("job reader calls = %d, want 0", reader.calls)
	}
}

func TestJobStatusHandlerRejectsUnknownJobWithoutSideEffects(t *testing.T) {
	delivery := statusTestDelivery()
	client := &fakeJobStatusEphemeralClient{}
	reader := &fakeJobStatusReader{err: errors.New("not found")}
	handler := newJobStatusHandler(client, time.Second, &fakeJobStatusConfirmationStore{delivery: &delivery}, reader)

	if err := handler.Handle(t.Context(), statusTestCallback(delivery, delivery.Actor, delivery.ChannelID, delivery.ThreadTS)); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	assertStatusAuthorizationFailure(t, client)
	if reader.calls != 1 {
		t.Fatalf("job reader calls = %d, want 1", reader.calls)
	}
}

func assertStatusAuthorizationFailure(t *testing.T, client *fakeJobStatusEphemeralClient) {
	t.Helper()
	if len(client.calls) != 1 {
		t.Fatalf("ephemeral calls = %d, want 1", len(client.calls))
	}
	if !strings.Contains(client.calls[0].fallback, "No se encontró") {
		t.Fatalf("authorization error = %q", client.calls[0].fallback)
	}
	if len(client.calls[0].blocks) == 0 {
		t.Fatal("authorization error has no blocks")
	}
}

func TestJobStatusResponseConstructionNeutralizesControlsAndIncludesRequiredFields(t *testing.T) {
	job := *statusTestJob(statusTestDelivery())
	job.ID = "job-<@U12345678>"
	job.Status = domain.JobCompleted
	fallback, blocks, err := compileJobStatusResponse(job)
	if err != nil {
		t.Fatalf("compileJobStatusResponse() error = %v", err)
	}
	message := blockkit.Message{FallbackText: fallback, Blocks: blocks}
	if !blockkit.Reachable(message, "completed") || !blockkit.Reachable(message, "2026-07-21T15:00:00Z") ||
		!blockkit.Reachable(message, "2026-07-21T15:05:00Z") {
		t.Fatal("status values did not reach the view")
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "<@U12345678>") || !strings.Contains(string(encoded), `\u0026lt;`) {
		t.Fatalf("unsafe Slack control was not safely rendered in %s", encoded)
	}
}

func TestJobStatusResponseRespectsBlockKitLimits(t *testing.T) {
	job := *statusTestJob(statusTestDelivery())
	job.ID = strings.Repeat("界", maxFallbackText*2)
	fallback, blocks, err := compileJobStatusResponse(job)
	if err != nil {
		t.Fatalf("compileJobStatusResponse() error = %v", err)
	}
	if len([]rune(fallback)) > maxFallbackText || len(blocks) > maxBlocksPerMessage {
		t.Fatalf("response exceeds limits: fallback=%d blocks=%d", len([]rune(fallback)), len(blocks))
	}
	message := blockkit.Message{FallbackText: fallback, Blocks: blocks}
	if !blockkit.Reachable(message, "界") {
		t.Fatal("long job ID did not reach the rendered view")
	}
	tooLong, _, err := compileJobStatusErrorResponse(strings.Repeat("x", maxContextText*2))
	if err != nil {
		t.Fatalf("compileJobStatusErrorResponse() error = %v", err)
	}
	if len([]rune(tooLong)) > maxFallbackText {
		t.Fatalf("error fallback exceeds limit: %d", len([]rune(tooLong)))
	}
}
