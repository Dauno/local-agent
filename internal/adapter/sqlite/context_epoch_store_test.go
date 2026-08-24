package sqlite

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestContextEpochStoreCASBoundedRangeAndReopen(t *testing.T) {
	path := t.TempDir() + "/epochs.db"
	store, err := Initialize(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err := NewAdkSessionService(store).Create(t.Context(), &session.CreateRequest{
		AppName: "app", UserID: "user", SessionID: "session",
	}); err != nil {
		t.Fatal(err)
	}
	sessions := NewAdkSessionService(store)
	created, err := sessions.Get(t.Context(), &session.GetRequest{AppName: "app", UserID: "user", SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := sessions.AppendEvent(t.Context(), created.Session, session.NewEvent(t.Context(), "epoch-test")); err != nil {
			t.Fatal(err)
		}
	}
	epochs := NewContextEpochStore(store)
	first := testContextEpoch(1, "epoch-1", time.Unix(10, 0).UTC())
	if err := epochs.Append(t.Context(), first, 0); err != nil {
		t.Fatalf("append first epoch: %v", err)
	}
	second := testContextEpoch(2, "epoch-2", time.Unix(20, 0).UTC())
	if err := epochs.Append(t.Context(), second, 0); !errors.Is(err, port.ErrContextEpochCASConflict) {
		t.Fatalf("stale append error = %v, want CAS conflict", err)
	}
	if err := epochs.Append(t.Context(), second, 1); err != nil {
		t.Fatalf("append second epoch: %v", err)
	}
	invalidCoverage := testContextEpoch(3, "epoch-3", time.Unix(30, 0).UTC())
	invalidCoverage.CoveredThroughOrdinal = 2
	if err := epochs.Append(t.Context(), invalidCoverage, 2); !errors.Is(err, port.ErrContextEpochValidation) {
		t.Fatalf("future coverage error = %v, want validation", err)
	}
	invalidCoverage.CoveredThroughOrdinal = 0
	if err := epochs.Append(t.Context(), invalidCoverage, 2); !errors.Is(err, port.ErrContextEpochValidation) {
		t.Fatalf("regressing coverage error = %v, want validation", err)
	}

	firstRange, err := epochs.Range(t.Context(), "app", "user", "session", 0, 1)
	if err != nil || len(firstRange) != 1 || firstRange[0].EpochNumber != 1 {
		t.Fatalf("first bounded range = %+v, %v", firstRange, err)
	}
	secondRange, err := epochs.Range(t.Context(), "app", "user", "session", 1, 1)
	if err != nil || len(secondRange) != 1 || secondRange[0].EpochNumber != 2 {
		t.Fatalf("second bounded range = %+v, %v", secondRange, err)
	}
	if _, err := epochs.Range(t.Context(), "app", "user", "session", 0, domain.MaxContextEpochRange+1); !errors.Is(err, port.ErrContextEpochValidation) {
		t.Fatalf("unbounded range error = %v", err)
	}

	latest, err := epochs.Latest(t.Context(), "app", "user", "session")
	if err != nil || latest.EpochID != second.EpochID || latest.SourceDigest != second.SourceDigest {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	reopenedLatest, err := NewContextEpochStore(reopened).Latest(t.Context(), "app", "user", "session")
	if err != nil || reopenedLatest.EpochNumber != 2 || reopenedLatest.ResultIdentities[0] != second.ResultIdentities[0] {
		t.Fatalf("reopened latest = %+v, %v", reopenedLatest, err)
	}
}

func TestContextEpochStoreRequiresExistingSessionAndValidIdentity(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/epochs.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	epochs := NewContextEpochStore(store)
	if err := epochs.Append(t.Context(), testContextEpoch(1, "epoch-1", time.Unix(10, 0).UTC()), 0); !errors.Is(err, port.ErrContextEpochSessionMissing) {
		t.Fatalf("missing session error = %v", err)
	}
	invalid := testContextEpoch(1, "epoch-1", time.Unix(10, 0).UTC())
	invalid.SourceDigest = strings.Repeat("z", 64)
	if err := epochs.Append(t.Context(), invalid, 0); !errors.Is(err, port.ErrContextEpochValidation) {
		t.Fatalf("invalid identity error = %v", err)
	}
}

func testContextEpoch(number int64, id string, createdAt time.Time) domain.ContextEpoch {
	return domain.ContextEpoch{
		EpochID:               id,
		AppName:               "app",
		UserID:                "user",
		SessionID:             "session",
		EpochNumber:           number,
		CoveredThroughOrdinal: number - 1,
		WorkstreamRevision:    number,
		SummaryIdentity:       "summary-" + id,
		KnowledgeIdentities:   []string{"knowledge-" + id},
		ResultIdentities:      []string{strings.Repeat(string(rune('a'+number-1)), 64)},
		CompilerVersion:       "compiler-v1",
		CounterVersion:        "counter-v1",
		SourceDigest:          strings.Repeat(string(rune('b'+number-1)), 64),
		FrameTokens:           int(number * 10),
		FrameCodePoints:       int(number * 20),
		SelectedSourceCount:   int(number),
		Reason:                "test",
		CreatedAt:             createdAt,
	}
}
