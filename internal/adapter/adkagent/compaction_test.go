package adkagent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func testProjector(t *testing.T, max, recent int) *Projector {
	t.Helper()
	projector, err := NewProjector(CompactionConfig{Enabled: true, MaxHistoryChars: max, RecentTurns: recent, SummaryEnabled: false, SummaryMaxChars: max / 2})
	if err != nil {
		t.Fatal(err)
	}
	return projector
}

func userText(text string) domain.Content {
	return domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: text}}}
}

func modelText(text string) domain.Content {
	return domain.Content{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: text}}}
}

func TestProjectorEmptyAndShortHistory(t *testing.T) {
	projector := testProjector(t, 100, 8)
	result, err := projector.Project(context.Background(), domain.CompactionRequest{})
	if err != nil || len(result.Contents) != 0 || result.Diagnostics.HistoryCharsAfter != 0 {
		t.Fatalf("empty projection = %#v, %v", result, err)
	}
	result, err = projector.Project(context.Background(), domain.CompactionRequest{Contents: []domain.Content{userText("hi")}})
	if err != nil || len(result.Contents) != 1 || result.Contents[0].Parts[0].Text != "hi" {
		t.Fatalf("short projection = %#v, %v", result, err)
	}
}

func TestProjectorExactlyAtBudgetAndUnicodeUseCodePoints(t *testing.T) {
	contents := []domain.Content{userText("old"), modelText("ok"), userText("now")}
	cost, err := domain.ContentCost(contents)
	if err != nil {
		t.Fatal(err)
	}
	result, err := testProjector(t, cost, 8).Project(context.Background(), domain.CompactionRequest{Contents: contents})
	if err != nil || len(result.Contents) != len(contents) || result.Diagnostics.HistoryCharsAfter != cost {
		t.Fatalf("exact budget projection = %#v, %v", result, err)
	}
	unicodeResult, err := testProjector(t, 2, 8).Project(context.Background(), domain.CompactionRequest{Contents: []domain.Content{userText("🙂🙂")}})
	if err != nil || unicodeResult.Diagnostics.HistoryCharsAfter != 2 {
		t.Fatalf("Unicode code-point accounting = %#v, %v", unicodeResult, err)
	}
}

func TestProjectorCountsStructuredPartsAndRetainsRecentTurnsInOrder(t *testing.T) {
	call := domain.Content{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{FunctionCall: &domain.FunctionCall{ID: "call-1", Name: "lookup", Args: map[string]any{"query": "one"}}}}}
	response := domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{FunctionResponse: &domain.FunctionResponse{ID: "call-1", Name: "lookup", Response: map[string]any{"result": "one"}}}}}
	contents := []domain.Content{userText("first"), modelText("answer-1"), userText("second"), call, response, modelText("answer-2"), userText("current")}
	structuredCost, err := domain.ContentCost([]domain.Content{call, response})
	if err != nil || structuredCost == 0 {
		t.Fatalf("structured cost = %d, %v", structuredCost, err)
	}
	result, err := testProjector(t, 1000, 1).Project(context.Background(), domain.CompactionRequest{Contents: contents})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Contents) != 5 || result.Contents[0].Parts[0].Text != "second" || result.Contents[1].Parts[0].FunctionCall == nil || result.Contents[2].Parts[0].FunctionResponse == nil || result.Contents[4].Parts[0].Text != "current" {
		t.Fatalf("recent projection = %#v", result.Contents)
	}
}

func TestProjectorNeverSeparatesProtocolPairs(t *testing.T) {
	confirmationCall := domain.Content{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{FunctionCall: &domain.FunctionCall{ID: "wrapper-1", Name: domain.ConfirmationFunctionName, Args: map[string]any{"hint": "write"}}}}}
	confirmationResponse := domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{FunctionResponse: &domain.FunctionResponse{ID: "wrapper-1", Name: domain.ConfirmationFunctionName, Response: map[string]any{"confirmed": true}}}}}
	contents := []domain.Content{userText("request"), confirmationCall, confirmationResponse, modelText("completed"), userText("next")}
	result, err := testProjector(t, 1000, 8).Project(context.Background(), domain.CompactionRequest{Contents: contents})
	if err != nil || len(result.Contents) != len(contents) {
		t.Fatalf("confirmation pair projection = %#v, %v", result.Contents, err)
	}

	_, err = testProjector(t, 10, 8).Project(context.Background(), domain.CompactionRequest{Contents: []domain.Content{userText("current request is too large")}})
	if !errors.Is(err, domain.ErrActiveContextTooLarge) {
		t.Fatalf("active oversized error = %v", err)
	}
}

func TestProjectorEvictsOldestTurnFirstAndLeavesInputUntouched(t *testing.T) {
	contents := []domain.Content{userText("oldest"), modelText("one"), userText("middle"), modelText("two"), userText("newer"), modelText("three"), userText("active")}
	original := contents[0].Parts[0].Text
	result, err := testProjector(t, 30, 2).Project(context.Background(), domain.CompactionRequest{Contents: contents})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Contents) < 5 || result.Contents[0].Parts[0].Text != "middle" {
		t.Fatalf("oldest turn was not evicted: %#v", result.Contents)
	}
	if contents[0].Parts[0].Text != original {
		t.Fatal("projection mutated caller contents")
	}
}

func TestProjectorSummaryNeverEvictsRecentRawTurn(t *testing.T) {
	contents := []domain.Content{userText("old"), modelText("old answer"), userText("recent"), modelText("recent answer"), userText("active")}
	activeAndRecent, err := domain.ContentCost(contents[2:])
	if err != nil {
		t.Fatal(err)
	}
	projector, err := NewProjector(CompactionConfig{Enabled: true, MaxHistoryChars: 100, RecentTurns: 1, SummaryEnabled: true, SummaryMaxChars: 50})
	if err != nil {
		t.Fatal(err)
	}
	result, err := projector.Project(context.Background(), domain.CompactionRequest{Contents: contents, MaxHistoryChars: activeAndRecent, ExistingSummary: "The user stated that old context exists."})
	if err != nil {
		t.Fatal(err)
	}
	if result.Diagnostics.SummaryPresent || len(result.Contents) != 3 || result.Contents[0].Parts[0].Text != "recent" || result.Contents[2].Parts[0].Text != "active" {
		t.Fatalf("summary evicted raw recent turn: %#v diagnostics=%#v", result.Contents, result.Diagnostics)
	}
}

func TestADKFunctionPartsKeepNumericTypesAcrossNonActiveProjection(t *testing.T) {
	original := []*genai.Content{{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call", Name: "tool", Args: map[string]any{"integer": int64(7), "fraction": float32(1.5)}}}}}}
	domainContents, err := toDomainContents(original)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := fromDomainContents(domainContents)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, projected) {
		t.Fatalf("function part changed during projection:\noriginal=%#v\nprojected=%#v", original, projected)
	}
}

func TestProjectorIgnoresSummaryBeyondCurrentLedger(t *testing.T) {
	projector, err := NewProjector(CompactionConfig{Enabled: true, MaxHistoryChars: 1000, RecentTurns: 1, SummaryEnabled: true, SummaryMaxChars: 200})
	if err != nil {
		t.Fatal(err)
	}
	result, err := projector.Project(context.Background(), domain.CompactionRequest{
		Contents:        []domain.Content{userText("old"), modelText("done"), userText("active")},
		ExistingSummary: "The user stated that stale data exists.", ExistingSummaryCoveredOrdinal: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Diagnostics.SummaryPresent {
		t.Fatalf("stale summary was projected: %#v", result.Diagnostics)
	}
}
