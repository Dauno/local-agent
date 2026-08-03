package sqlite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestActivationResponsePreparationAtomicallyStagesMemoryIneligibleExchange(t *testing.T) {
	store, jobs, now, activation := prepareActivationForExchangeTest(t)
	metadata := domain.ConversationMetadata{
		Key: activation.ConversationKey, TeamID: activation.TeamID, ChannelID: "D12345678",
		ChannelKind: domain.ChannelDM, LastTS: activation.SlackMessageTS,
	}
	message := domain.Message{
		Role: domain.RoleAssistant, Source: domain.MessageSourceAssistant,
		Content: "durable synthesis", CreatedAt: now.Add(8 * time.Second),
	}

	prepared, err := jobs.PrepareActivationResponseWithExchange(t.Context(), activation, metadata, message, 10, now.Add(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ID == "" || prepared.CorrelationID == "" {
		t.Fatalf("prepared exchange = %#v", prepared)
	}
	var state, body, intentID, correlation string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT state, response_body, exchange_intent_id, correlation_id
		FROM external_agent_job_activations WHERE activation_id = ?`, activation.ActivationID).Scan(&state, &body, &intentID, &correlation); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.ActivationResponsePrepared) || body != message.Content || intentID != prepared.ID || correlation != prepared.CorrelationID {
		t.Fatalf("activation response = state=%q body=%q intent=%q correlation=%q", state, body, intentID, correlation)
	}
	var sourceJSON string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT source_messages FROM memory_exchange_intents WHERE id = ?`, prepared.ID).Scan(&sourceJSON); err != nil {
		t.Fatal(err)
	}
	var source sourceMessagesWrapper
	if err := json.Unmarshal([]byte(sourceJSON), &source); err != nil {
		t.Fatal(err)
	}
	if source.MemoryEligible || len(source.Messages) != 2 {
		t.Fatalf("activation exchange memory source = %#v", source)
	}
	var messages, memoryOutbox int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE source = 'job_completion'`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM memory_outbox`).Scan(&memoryOutbox); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || memoryOutbox != 0 {
		t.Fatalf("activation durable side effects = messages=%d memory_outbox=%d", messages, memoryOutbox)
	}

	current, err := jobs.GetActivation(t.Context(), activation.ActivationID)
	if err != nil || current == nil {
		t.Fatalf("get prepared activation = %#v, err=%v", current, err)
	}
	retried, err := jobs.PrepareActivationResponseWithExchange(t.Context(), current, metadata, message, 10, now.Add(9*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if retried != prepared {
		t.Fatalf("retry prepared exchange = %#v, want %#v", retried, prepared)
	}
	var intentCount int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM memory_exchange_intents WHERE conversation_key = ?`, activation.ConversationKey).Scan(&intentCount); err != nil {
		t.Fatal(err)
	}
	if intentCount != 1 {
		t.Fatalf("exchange intents after retry = %d, want 1", intentCount)
	}
	if err := store.MarkAssistantExchangePublished(t.Context(), prepared.ID, "1710000000.000022"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeAssistantExchange(t.Context(), prepared.ID); err != nil {
		t.Fatal(err)
	}
	var assistantMessages, finalizedMemoryOutbox int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE role = 'assistant' AND source = 'assistant'`).Scan(&assistantMessages); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM memory_outbox`).Scan(&finalizedMemoryOutbox); err != nil {
		t.Fatal(err)
	}
	if assistantMessages != 1 || finalizedMemoryOutbox != 0 {
		t.Fatalf("finalized activation exchange = assistant_messages=%d memory_outbox=%d", assistantMessages, finalizedMemoryOutbox)
	}
	current, err = jobs.GetActivation(t.Context(), activation.ActivationID)
	if err != nil || current == nil {
		t.Fatalf("get activation before completion = %#v, err=%v", current, err)
	}
	if err := jobs.CompleteActivation(t.Context(), current, "1710000000.000022", now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestActivationResponsePreparationRollsBackExchangeOnActivationFailure(t *testing.T) {
	store, jobs, now, activation := prepareActivationForExchangeTest(t)
	metadata := domain.ConversationMetadata{
		Key: activation.ConversationKey, TeamID: activation.TeamID, ChannelID: "D12345678",
		ChannelKind: domain.ChannelDM, LastTS: activation.SlackMessageTS,
	}
	if _, err := store.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_activation_response_prepare
		BEFORE UPDATE OF state ON external_agent_job_activations
		WHEN NEW.state = 'response_prepared'
		BEGIN SELECT RAISE(ABORT, 'test response preparation failure'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.DB().ExecContext(context.Background(), `DROP TRIGGER fail_activation_response_prepare`)
	})

	_, err := jobs.PrepareActivationResponseWithExchange(t.Context(), activation, metadata, domain.Message{
		Role: domain.RoleAssistant, Source: domain.MessageSourceAssistant, Content: "must roll back", CreatedAt: now.Add(8 * time.Second),
	}, 10, now.Add(8*time.Second))
	if err == nil {
		t.Fatal("response preparation succeeded despite injected activation failure")
	}
	var state string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT state FROM external_agent_job_activations WHERE activation_id = ?`, activation.ActivationID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.ActivationModelStarted) {
		t.Fatalf("activation state after rollback = %q", state)
	}
	var intents int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM memory_exchange_intents`).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if intents != 0 {
		t.Fatalf("exchange intent survived rollback: %d", intents)
	}
}

func TestJobCompletionMessageAppendIsIdempotent(t *testing.T) {
	store, _, now := newActivationTestStore(t)
	metadata := domain.ConversationMetadata{
		Key: "slack:T12345678:dm:D12345678", TeamID: "T12345678", ChannelID: "D12345678",
		ChannelKind: domain.ChannelDM, LastTS: "1710000000.000020",
	}
	message := domain.Message{
		Role: domain.RoleUser, Source: domain.MessageSourceJobCompletion, Content: "completion envelope",
		ExternalTS: "activation-id", UserID: "U12345678", CreatedAt: now,
	}
	for range 2 {
		if err := store.AppendJobCompletionMessage(t.Context(), metadata, message, 10); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE external_ts = ?`, message.ExternalTS).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("job completion message count = %d, want 1", count)
	}
}

func prepareActivationForExchangeTest(t *testing.T) (*Store, *ExternalAgentJobStore, time.Time, *domain.ExternalAgentJobActivation) {
	t.Helper()
	store, jobs, now := newActivationTestStore(t)
	job := activationTestJob("activation-exchange", now)
	terminalizeActivationTestJob(t, jobs, job, now)
	notification := claimActivationTestNotification(t, jobs, now.Add(2*time.Second), "publisher")
	if err := jobs.MarkNotificationPublished(t.Context(), notification, "1710000000.000021", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	activation, err := jobs.ClaimNextActivation(t.Context(), now.Add(4*time.Second), "activation-worker", time.Minute)
	if err != nil || activation == nil {
		t.Fatalf("activation claim = %#v, err=%v", activation, err)
	}
	if err := store.AppendJobCompletionMessage(t.Context(), domain.ConversationMetadata{
		Key: activation.ConversationKey, TeamID: activation.TeamID, ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: activation.SlackMessageTS,
	}, domain.Message{
		Role: domain.RoleUser, Source: domain.MessageSourceJobCompletion,
		Content: "completion envelope", ExternalTS: activation.ActivationID, UserID: activation.Actor, CreatedAt: now.Add(5 * time.Second),
	}, 10); err != nil {
		t.Fatal(err)
	}
	if err := jobs.MarkActivationModelStarted(t.Context(), activation, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	return store, jobs, now, activation
}
