package results

import (
	"context"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestMaterializeRedactsAndSanitizesBeforeStoreReceivesPayload(t *testing.T) {
	store := &captureResultStore{}
	service, err := New(store, func(value string) string {
		return "safe " + value[7:]
	})
	if err != nil {
		t.Fatal(err)
	}
	request := port.ResultMaterialization{Payload: "secret:<tag>"}
	if _, err := service.Materialize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if store.request.Payload != "safe &lt;tag>" {
		t.Fatalf("store payload = %q", store.request.Payload)
	}
}

func TestRetentionPolicyFailsClosedForReferencesReservationsAndHolds(t *testing.T) {
	policy := RetentionPolicy{
		Context: time.Hour, Conversation: 2 * time.Hour, Workstream: 3 * time.Hour, Exported: 4 * time.Hour,
	}
	identity := domain.ResultIdentity{
		ResultID:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Producer:  domain.ResultProducer{Kind: domain.ResultProducerToolOperation, ID: "operation", Revision: 1},
		Storage:   domain.ResultStorage{Kind: domain.ResultStorageRecoverable, Key: "private-key"},
		SHA256:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Bytes:     7,
		MediaType: "text/plain",
		Scope:     domain.ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:D1", Project: "app"},
		Retention: domain.ResultRetentionContext, State: domain.ResultAvailable,
		CreatedAt: time.Unix(1, 0).UTC(),
	}
	clear := RetentionEvidence{ReferencesChecked: true, MaterializationsChecked: true, BackendChecked: true}
	if policy.Eligible(identity, identity.CreatedAt.Add(time.Hour-time.Nanosecond), clear) {
		t.Fatal("result was eligible before its class age")
	}
	if !policy.Eligible(identity, identity.CreatedAt.Add(time.Hour), clear) {
		t.Fatal("unreferenced result was not eligible at its class age")
	}
	withReference := clear
	withReference.HasLiveReference = true
	if policy.Eligible(identity, identity.CreatedAt.Add(time.Hour), withReference) {
		t.Fatal("live reference allowed cleanup")
	}
	withReservation := clear
	withReservation.HasPendingMaterialization = true
	if policy.Eligible(identity, identity.CreatedAt.Add(time.Hour), withReservation) {
		t.Fatal("pending materialization allowed cleanup")
	}
	withHold := clear
	withHold.BackendHeld = true
	if policy.Eligible(identity, identity.CreatedAt.Add(time.Hour), withHold) {
		t.Fatal("backend hold allowed cleanup")
	}
	for name, incomplete := range map[string]RetentionEvidence{
		"references":       {MaterializationsChecked: true, BackendChecked: true},
		"materializations": {ReferencesChecked: true, BackendChecked: true},
		"backend":          {ReferencesChecked: true, MaterializationsChecked: true},
	} {
		if policy.Eligible(identity, identity.CreatedAt.Add(time.Hour), incomplete) {
			t.Fatalf("incomplete %s evidence allowed cleanup", name)
		}
	}
}

func TestRetentionPolicyAnchorsExportedResultsAtVerifiedPublication(t *testing.T) {
	policy := RetentionPolicy{Context: time.Hour, Conversation: time.Hour, Workstream: time.Hour, Exported: 24 * time.Hour}
	identity := domain.ResultIdentity{
		ResultID:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Producer:  domain.ResultProducer{Kind: domain.ResultProducerToolOperation, ID: "operation", Revision: 1},
		Storage:   domain.ResultStorage{Kind: domain.ResultStorageRecoverable, Key: "private-key"},
		SHA256:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Bytes:     7,
		MediaType: "text/plain",
		Scope:     domain.ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:D1", Project: "app"},
		Retention: domain.ResultRetentionExported, State: domain.ResultAvailable,
		CreatedAt: time.Unix(1, 0).UTC(),
	}
	publication := identity.CreatedAt.Add(30 * 24 * time.Hour)
	evidence := RetentionEvidence{
		ReferencesChecked: true, MaterializationsChecked: true, BackendChecked: true,
		VerifiedPublicationAt: publication,
	}
	if policy.Eligible(identity, publication.Add(24*time.Hour-time.Nanosecond), evidence) {
		t.Fatal("exported result was eligible before publication retention age")
	}
	if !policy.Eligible(identity, publication.Add(24*time.Hour), evidence) {
		t.Fatal("exported result was not eligible at publication retention age")
	}
	evidence.VerifiedPublicationAt = time.Time{}
	if policy.Eligible(identity, publication.Add(24*time.Hour), evidence) {
		t.Fatal("unpublished result was eligible for cleanup")
	}
}

type captureResultStore struct{ request port.ResultMaterialization }

func (s *captureResultStore) Materialize(_ context.Context, request port.ResultMaterialization) (domain.ResultHandle, error) {
	s.request = request
	return domain.ResultHandle{}, nil
}

func (*captureResultStore) Resolve(context.Context, string, domain.ResultScope) (domain.ResultIdentity, domain.ResultHandle, error) {
	return domain.ResultIdentity{}, domain.ResultHandle{}, nil
}

func (*captureResultStore) ReadRange(context.Context, string, domain.ResultScope, int64, int64) (domain.ResultChunk, error) {
	return domain.ResultChunk{}, nil
}

var _ port.TrustedResultStore = (*captureResultStore)(nil)
