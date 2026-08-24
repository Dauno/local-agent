package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestConfirmationStorePreservesPresentationAndHandlesPublishedRejection(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	confirmations := NewConfirmationStore(store)
	delivery := port.ConfirmationDelivery{
		WrapperCallID:  "wrapper",
		OriginalCallID: "original",
		SessionID:      "session",
		Actor:          "U12345678",
		TeamID:         "T12345678",
		ChannelID:      "D12345678",
		Summary:        "Delete worktree",
		Payload:        `{"workstream_id":"ws-1","action":"cancel_workstream"}`,
		ParameterHash:  "hash",
		Expiry:         time.Now().Add(time.Hour),
	}
	if err := confirmations.CreateDelivery(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	// The durable event can be replayed after a crash; creating it again must
	// reconcile with the existing delivery rather than fail the recovery path.
	if err := confirmations.CreateDelivery(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	if err := confirmations.MarkPublished(ctx, delivery.WrapperCallID, "correlation", "1234.5678", "test_v1"); err != nil {
		t.Fatal(err)
	}
	if err := confirmations.RejectDelivery(ctx, delivery.WrapperCallID); err != nil {
		t.Fatal(err)
	}

	got, err := confirmations.GetByWrapperCallID(ctx, delivery.WrapperCallID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != port.ConfirmationRejected || got.Summary != delivery.Summary || got.Payload != delivery.Payload || got.ParameterHash != delivery.ParameterHash ||
		got.SlackMessageTS != "1234.5678" || got.RendererMode != "test_v1" {
		t.Fatalf("delivery = %#v", got)
	}
}

func TestConfirmationStoreDoesNotListExpiredDelivery(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	confirmations := NewConfirmationStore(store)
	if err := confirmations.CreateDelivery(ctx, port.ConfirmationDelivery{
		WrapperCallID:  "expired-wrapper",
		OriginalCallID: "original",
		SessionID:      "session",
		Actor:          "U12345678",
		TeamID:         "T12345678",
		ChannelID:      "D12345678",
		Expiry:         time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := confirmations.ListPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("ListPending() = %#v, want no expired deliveries", pending)
	}
	if err := confirmations.ExpireDeliveries(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := confirmations.GetByWrapperCallID(ctx, "expired-wrapper")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != port.ConfirmationExpired {
		t.Fatalf("delivery after expiry = %#v", got)
	}
}

func TestConfirmationStoreExpireDeliveryIsSingleUseAndRemovesListExpiredRow(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	confirmations := NewConfirmationStore(store)
	now := time.Unix(1710000000, 0).UTC()
	if err := confirmations.CreateDelivery(ctx, port.ConfirmationDelivery{
		WrapperCallID: "cas-expired-wrapper", OriginalCallID: "original", SessionID: "session",
		Actor: "U12345678", TeamID: "T12345678", ChannelID: "D12345678",
		Status: port.ConfirmationPublished, Expiry: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := confirmations.ListExpired(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("ListExpired() before CAS = %#v", listed)
	}
	first, err := confirmations.ExpireDelivery(ctx, "cas-expired-wrapper", now)
	if err != nil || !first {
		t.Fatalf("first ExpireDelivery() = first:%t err:%v", first, err)
	}
	listed, err = confirmations.ListExpired(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("ListExpired() after CAS = %#v", listed)
	}
	second, err := confirmations.ExpireDelivery(ctx, "cas-expired-wrapper", now)
	if err != nil || second {
		t.Fatalf("second ExpireDelivery() = second:%t err:%v", second, err)
	}
}

func TestConfirmationStoreRejectDeliveryIsSingleUse(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	confirmations := NewConfirmationStore(store)
	if err := confirmations.CreateDelivery(ctx, port.ConfirmationDelivery{
		WrapperCallID: "wrapper", OriginalCallID: "original", SessionID: "session",
		Actor: "U12345678", TeamID: "T12345678", ChannelID: "D12345678", Expiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := confirmations.RejectDelivery(ctx, "wrapper"); err != nil {
		t.Fatal(err)
	}
	if err := confirmations.RejectDelivery(ctx, "wrapper"); err == nil {
		t.Fatal("second RejectDelivery() unexpectedly succeeded")
	}
}

func TestConfirmationStoreRestoresThreadedDMConversationKey(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	confirmations := NewConfirmationStore(store)
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "threaded-wrapper", OriginalCallID: "original",
		SessionID: "adk:slack:T12345678:dm:D12345678:thread:1700000000.000001",
		Actor:     "U12345678", TeamID: "T12345678", ChannelID: "D12345678",
		ThreadTS: "1700000000.000001", Expiry: time.Now().Add(time.Hour),
	}
	if err := confirmations.CreateDelivery(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	got, err := confirmations.GetByWrapperCallID(ctx, delivery.WrapperCallID)
	if err != nil {
		t.Fatal(err)
	}
	want := "slack:T12345678:dm:D12345678:thread:1700000000.000001"
	if got == nil || got.ConversationKey != domain.ConversationKey(want) {
		t.Fatalf("ConversationKey = %q, want %q", got.ConversationKey, want)
	}
}
