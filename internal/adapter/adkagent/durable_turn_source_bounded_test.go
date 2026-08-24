package adkagent

// TRD 08 checkpoint 2, item 5 and acceptance criterion 5
// (docs/root-orchestrator-v2/hallazgos/worker-prompt-trd08-checkpoint2.md):
// DurableTurnSource.ClosedTurns must stop loading sessions through one
// unbounded Get. This test seeds a real sqlite-backed session past
// domain.MaxContextEpochRange events (forcing the paginated LoadEventRange
// path to run more than one page), and checks that the paginated read
// returns exactly the same turns as the pre-checkpoint unbounded Get did,
// while never calling Get at all.
//
// The architecture test excludes _test.go files, so this file may import
// internal/adapter/sqlite even though production adkagent code must not.

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func appendClosedTurn(t *testing.T, service *adaptersqlite.AdkSessionService, sess adksessionRef, index int) {
	t.Helper()
	userEvent := session.NewEvent(context.Background(), "invocation")
	userEvent.ID = fmt.Sprintf("turn-%d-user", index)
	userEvent.Content = genai.NewContentFromText(fmt.Sprintf("user message %d", index), genai.RoleUser)
	userEvent.TurnComplete = false
	if err := service.AppendEvent(context.Background(), sess.session, userEvent); err != nil {
		t.Fatalf("append user event %d: %v", index, err)
	}

	modelEvent := session.NewEvent(context.Background(), "invocation")
	modelEvent.ID = fmt.Sprintf("turn-%d-model", index)
	modelEvent.Content = genai.NewContentFromText(fmt.Sprintf("model reply %d", index), genai.RoleModel)
	modelEvent.TurnComplete = true
	if err := service.AppendEvent(context.Background(), sess.session, modelEvent); err != nil {
		t.Fatalf("append model event %d: %v", index, err)
	}
}

// adksessionRef carries the live *localSession forward across AppendEvent
// calls, since AppendEvent mutates it in place (revision, events) and every
// call after the first must see that mutation to pass its CAS check.
type adksessionRef struct{ session session.Session }

func TestDurableTurnSourcePaginatesInsteadOfUnboundedGet(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := adaptersqlite.Initialize(ctx, filepath.Join(dir, "durable-turns.db"))
	if err != nil {
		t.Fatalf("initialize store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := adaptersqlite.NewAdkSessionService(store)

	const appName, userID, sessionID = "app", "user", "session"
	created, err := service.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	ref := adksessionRef{session: created.Session}

	// 70 turns x 2 events = 140 events, past domain.MaxContextEpochRange (128),
	// so the paginated loader must run at least two pages.
	const turnCount = 70
	if 2*turnCount <= domain.MaxContextEpochRange {
		t.Fatalf("test setup does not exceed the page size: 2*%d events <= %d", turnCount, domain.MaxContextEpochRange)
	}
	for i := 1; i <= turnCount; i++ {
		appendClosedTurn(t, service, ref, i)
	}

	// Reference: the pre-checkpoint behavior, an unbounded Get plus in-memory
	// classification, called directly against the bare service (no
	// durableEventRangeLoader on session.Service alone).
	var bareService session.Service = service
	wantSource := DurableTurnSource{Service: unboundedOnlyService{bareService}, AppName: appName, UserID: userID}
	want, err := wantSource.ClosedTurns(ctx, sessionID, 0, int64(^uint64(0)>>1))
	if err != nil {
		t.Fatalf("reference ClosedTurns: %v", err)
	}
	if len(want) != turnCount {
		t.Fatalf("reference turn count = %d, want %d", len(want), turnCount)
	}

	// Paginated: the recording wrapper exposes LoadEventRange, so
	// loadAllEvents must page instead of calling Get.
	recorder := &recordingSessionService{inner: service}
	gotSource := DurableTurnSource{Service: recorder, AppName: appName, UserID: userID}
	got, err := gotSource.ClosedTurns(ctx, sessionID, 0, int64(^uint64(0)>>1))
	if err != nil {
		t.Fatalf("paginated ClosedTurns: %v", err)
	}

	if !reflect.DeepEqual(want, got) {
		t.Fatalf("paginated ClosedTurns diverged from the unbounded-Get reference:\nwant=%#v\ngot=%#v", want, got)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.totalGets != 0 {
		t.Fatalf("paginated ClosedTurns called Get %d times, want 0", recorder.totalGets)
	}
}

// unboundedOnlyService strips any durableEventRangeLoader/epochEventHeadReader
// methods a wrapped service might expose, forcing DurableTurnSource onto its
// pre-checkpoint unbounded-Get fallback, so this test has a ground truth that
// is independent of the paginated code path it verifies.
type unboundedOnlyService struct{ session.Service }
