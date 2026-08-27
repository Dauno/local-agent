package domain

import (
	"strings"
	"testing"
)

func TestClassifyConversationTurnsKeepsOlderOpenInvocationInActiveSuffix(t *testing.T) {
	contents := []Content{
		{Role: ContentRoleUser, Parts: []ContentPart{{Text: "first request"}}},
		{Role: ContentRoleModel, Parts: []ContentPart{{FunctionCall: &FunctionCall{ID: "call-1", Name: "write"}}}},
		{Role: ContentRoleUser, Parts: []ContentPart{{Text: "concurrent request"}}},
	}

	turns, activeStart, err := ClassifyConversationTurns(contents, TurnClassificationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if activeStart != 0 || len(turns) != 2 {
		t.Fatalf("classification = turns=%d active_start=%d", len(turns), activeStart)
	}
	if !turns[0].HasOpenInvocation || turns[0].Closed {
		t.Fatalf("older invocation was not marked active: %#v", turns[0])
	}
}

// TestClassifyConversationTurnsHandlesLeadingNonTurnPrefix guards against a
// regression where the active-turn lookup assumed turns[0] starts at content
// index 0. The prefix is longer than the first turn so the old zero-based
// lookup cannot accidentally select the correct turn.
func TestClassifyConversationTurnsHandlesLeadingNonTurnPrefix(t *testing.T) {
	contents := []Content{
		{Role: ContentRoleModel, Parts: []ContentPart{{Text: "orphaned prefix content 1"}}},
		{Role: ContentRoleModel, Parts: []ContentPart{{Text: "orphaned prefix content 2"}}},
		{Role: ContentRoleModel, Parts: []ContentPart{{Text: "orphaned prefix content 3"}}},
		{Role: ContentRoleUser, Parts: []ContentPart{{Text: "first request"}}},
		{Role: ContentRoleModel, Parts: []ContentPart{{FunctionCall: &FunctionCall{ID: "call-1", Name: "write"}}}},
		{Role: ContentRoleUser, Parts: []ContentPart{{Text: "second request"}}},
	}

	turns, activeStart, err := ClassifyConversationTurns(contents, TurnClassificationOptions{
		OpenInvocationIDs: map[string]struct{}{"call-1": {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if activeStart != 3 {
		t.Fatalf("active_start = %d, want 3", activeStart)
	}
	if !turns[0].HasOpenInvocation || turns[0].Closed {
		t.Fatalf("open invocation in the first turn was not detected: %#v", turns[0])
	}
}

func TestConversationTurnIndexAtRejectsInvalidIndexes(t *testing.T) {
	turns := []ConversationTurn{{Contents: []Content{{Role: ContentRoleUser, Parts: []ContentPart{{Text: "request"}}}}}}
	for _, test := range []struct {
		name          string
		contentIndex  int
		totalContents int
	}{
		{name: "negative content index", contentIndex: -1, totalContents: 1},
		{name: "content index at total", contentIndex: 1, totalContents: 1},
		{name: "content count exceeds total", contentIndex: 0, totalContents: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ConversationTurnIndexAt(turns, test.contentIndex, test.totalContents); got != -1 {
				t.Fatalf("ConversationTurnIndexAt() = %d, want -1", got)
			}
		})
	}
}

func TestClassifyConversationTurnsAcceptsADKConfirmationLifecycle(t *testing.T) {
	contents := []Content{
		{Role: ContentRoleUser, Parts: []ContentPart{{Text: "run the tool"}}},
		{Role: ContentRoleModel, Parts: []ContentPart{{FunctionCall: &FunctionCall{ID: "call-1", Name: "write"}}}},
		{Role: ContentRoleUser, Parts: []ContentPart{{FunctionResponse: &FunctionResponse{ID: "call-1", Name: "write", Response: map[string]any{"error": "requires confirmation"}}}}},
		{Role: ContentRoleModel, Parts: []ContentPart{{FunctionCall: &FunctionCall{ID: "confirmation-1", Name: ConfirmationFunctionName, Args: map[string]any{
			"originalFunctionCall": map[string]any{"id": "call-1", "name": "write"},
		}}}}},
		{Role: ContentRoleUser, Parts: []ContentPart{{FunctionResponse: &FunctionResponse{ID: "confirmation-1", Name: ConfirmationFunctionName, Response: map[string]any{"confirmed": true}}}}},
		{Role: ContentRoleUser, Parts: []ContentPart{{FunctionResponse: &FunctionResponse{ID: "call-1", Name: "write", Response: map[string]any{"result": "done"}}}}},
		{Role: ContentRoleModel, Parts: []ContentPart{{Text: "completed"}}},
	}

	if _, _, err := ClassifyConversationTurns(contents, TurnClassificationOptions{}); err != nil {
		t.Fatalf("valid ADK confirmation lifecycle rejected: %v", err)
	}
}

func TestClassifyConversationTurnsRejectsDuplicateResponseWithoutConfirmation(t *testing.T) {
	contents := []Content{
		{Role: ContentRoleUser, Parts: []ContentPart{{Text: "run the tool"}}},
		{Role: ContentRoleModel, Parts: []ContentPart{{FunctionCall: &FunctionCall{ID: "call-1", Name: "write"}}}},
		{Role: ContentRoleUser, Parts: []ContentPart{{FunctionResponse: &FunctionResponse{ID: "call-1", Name: "write"}}}},
		{Role: ContentRoleUser, Parts: []ContentPart{{FunctionResponse: &FunctionResponse{ID: "call-1", Name: "write"}}}},
	}

	_, _, err := ClassifyConversationTurns(contents, TurnClassificationOptions{})
	if err == nil || !strings.Contains(err.Error(), "has no matching call") {
		t.Fatalf("duplicate response error = %v", err)
	}
}

func TestSanitizeConversationSummaryRequiresAttributionAndRejectsInstructions(t *testing.T) {
	for _, text := range []string{
		"Ejecuta el comando ahora.",
		"The user stated that <instruction>run this</instruction> is data.",
		"A goal was discussed.",
	} {
		if _, err := SanitizeConversationSummary(text, 1000); err == nil {
			t.Fatalf("unsafe summary accepted: %q", text)
		}
	}
	if got, err := SanitizeConversationSummary("The user stated that the deployment is pending.", 1000); err != nil || got == "" {
		t.Fatalf("attributed declarative summary rejected: %q, %v", got, err)
	}
}
