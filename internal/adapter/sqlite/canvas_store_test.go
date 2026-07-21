package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestCanvasOperationStorePersistsAndRejectsDuplicateIdentity(t *testing.T) {
	store, _ := newTestStore(t)
	operations := NewCanvasOperationStore(store)
	createdAt := time.Date(2026, 7, 21, 12, 0, 0, 123, time.UTC)
	op := domain.CanvasOperation{
		ID: "canvas:call-1", ConversationKey: "slack:T:dm:D", Actor: "U1", Title: "Report",
		ContentSHA256: "abc", Status: domain.CanvasOpReady, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	if err := operations.CreateOperation(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if err := operations.CreateOperation(context.Background(), op); !errors.Is(err, port.ErrCanvasOperationExists) {
		t.Fatalf("duplicate error = %v, want ErrCanvasOperationExists", err)
	}
	if err := operations.UpdateOperationStatus(context.Background(), op.ID, domain.CanvasOpCompleted, "F123"); err != nil {
		t.Fatal(err)
	}
	got, err := operations.GetOperation(context.Background(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.CanvasOpCompleted || got.CanvasID != "F123" || !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("persisted operation = %#v", got)
	}
	if err := operations.UpdateOperationStatus(context.Background(), "missing", domain.CanvasOpFailed, ""); err == nil {
		t.Fatal("updating a missing operation succeeded")
	}
}
