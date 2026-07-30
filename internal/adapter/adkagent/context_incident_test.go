package adkagent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// readableText returns n code points of safe, human-readable text for
// constructing large function responses without risking control characters.
func readableText(n int) string {
	var builder strings.Builder
	builder.Grow(n)
	const line = "Line content demonstrating a realistic file excerpt with technical prose about code structure, architecture patterns, and implementation details.\n"
	chars := 0
	for chars < n {
		written, _ := builder.WriteString(line)
		chars += utf8.RuneCountInString(line)
		_ = written
	}
	runes := []rune(builder.String())
	if len(runes) > n {
		runes = runes[:n]
	}
	return string(runes)
}

func TestOriginalIncidentReproducesContextOverflow(t *testing.T) {
	// Construct 2 completed turns + 1 active turn with 7 read_file responses.
	// Each response carries ~17,200 code points of readable text.
	// The active suffix totals ~121,222 code points.
	// budget (maxHistoryChars) = 120,000.
	// Expected: ActiveContextTooLargeError.

	perResponseText := 17_200

	// Completed turn 1.
	completed1 := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "hello"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "hi there, how can I help?"}}},
	}
	// Completed turn 2.
	completed2 := []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "analyze the project"}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "I'll take a look."}}},
	}

	// Active turn: user asks to read files, model emits 7 read_file calls,
	// then 7 large function responses arrive.
	activeUser := domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "read these project files"}}}

	// Model content carries 7 read_file function calls.
	modelCalls := make([]domain.ContentPart, 7)
	for i := 0; i < 7; i++ {
		modelCalls[i] = domain.ContentPart{
			FunctionCall: &domain.FunctionCall{
				ID:   idForIndex(i),
				Name: "read_file",
				Args: map[string]any{"path": fileForIndex(i)},
			},
		}
	}
	activeModel := domain.Content{Role: domain.ContentRoleModel, Parts: modelCalls}

	// 7 large function responses.
	responses := make([]domain.Content, 7)
	for i := 0; i < 7; i++ {
		responses[i] = domain.Content{
			Role: domain.ContentRoleUser,
			Parts: []domain.ContentPart{{
				FunctionResponse: &domain.FunctionResponse{
					ID:   idForIndex(i),
					Name: "read_file",
					Response: map[string]any{
						"text": readableText(perResponseText),
					},
				},
			}},
		}
	}

	// Build full history.
	contents := make([]domain.Content, 0, len(completed1)+len(completed2)+2+len(responses))
	contents = append(contents, completed1...)
	contents = append(contents, completed2...)
	contents = append(contents, activeUser)
	contents = append(contents, activeModel)
	for _, r := range responses {
		contents = append(contents, r)
	}

	// Measure the active suffix cost starting from the active user content.
	activeStart := len(completed1) + len(completed2)
	activeContents := contents[activeStart:]
	activeChars, err := domain.ContentCost(activeContents)
	if err != nil {
		t.Fatalf("ContentCost(active suffix): %v", err)
	}
	if activeChars < 120_000 {
		t.Fatalf("active suffix code points = %d, must exceed 120000 budget to reproduce incident", activeChars)
	}
	t.Logf("active suffix code points: %d (target ~121222)", activeChars)

	// Project with budget of 120,000.
	budget := 120_000
	projector, err := NewProjector(CompactionConfig{
		Enabled:         true,
		MaxHistoryChars: budget,
		RecentTurns:     8,
		SummaryEnabled:  false,
		SummaryMaxChars: budget / 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := projector.Project(context.Background(), domain.CompactionRequest{
		Contents:        contents,
		MaxHistoryChars: budget,
	})

	// The active suffix alone exceeds the budget; must return ActiveContextTooLargeError.
	if err == nil {
		t.Fatalf("expected ActiveContextTooLargeError, got result: %#v diagnostics=%#v", result.Contents, result.Diagnostics)
	}
	if !errors.Is(err, domain.ErrActiveContextTooLarge) {
		t.Fatalf("expected ActiveContextTooLargeError, got: %v", err)
	}
	var tooLarge *domain.ActiveContextTooLargeError
	if errors.As(err, &tooLarge) {
		t.Logf("got expected error: chars=%d budget=%d", tooLarge.Chars, tooLarge.Budget)
		if tooLarge.Chars <= tooLarge.Budget {
			t.Errorf("error reports chars=%d <= budget=%d", tooLarge.Chars, tooLarge.Budget)
		}
	}
}

func TestLongConversationContinuityFixture(t *testing.T) {
	// Generate hundreds of completed turns.
	// Verify the final compaction retains at least the most recent turn.

	const numCompleted = 300
	const budget = 120_000
	const recentTurns = 8

	contents := make([]domain.Content, 0, numCompleted*2+2)
	for i := 0; i < numCompleted; i++ {
		contents = append(contents, domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "message " + itoa(i)}}})
		contents = append(contents, domain.Content{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "response " + itoa(i)}}})
	}
	// Active (unclosed) turn.
	contents = append(contents, domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "current request"}}})

	projector, err := NewProjector(CompactionConfig{
		Enabled:         true,
		MaxHistoryChars: budget,
		RecentTurns:     recentTurns,
		SummaryEnabled:  false,
		SummaryMaxChars: budget / 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := projector.Project(context.Background(), domain.CompactionRequest{
		Contents: contents,
	})
	if err != nil {
		t.Fatalf("Project(): %v", err)
	}

	// Must retain the active turn.
	if len(result.Contents) < 1 {
		t.Fatal("projected contents must not be empty")
	}
	lastContent := result.Contents[len(result.Contents)-1]
	if lastContent.Parts[0].Text != "current request" {
		t.Fatalf("active turn was not retained: %#v", result.Contents)
	}

	// Most recent completed turns should be present.
	foundRecent := false
	for _, c := range result.Contents {
		for _, p := range c.Parts {
			if p.Text == "message "+itoa(numCompleted-1) {
				foundRecent = true
				break
			}
		}
	}
	if !foundRecent {
		t.Fatalf("most recent completed turn was evicted: projected %d contents", len(result.Contents))
	}

	// Verify retained count matches expectations.
	if result.Diagnostics.RecentTurnsRetained < 1 {
		t.Fatalf("expected at least 1 recent turn retained, got %d", result.Diagnostics.RecentTurnsRetained)
	}
	t.Logf("retained %d recent turns, history chars before=%d after=%d",
		result.Diagnostics.RecentTurnsRetained,
		result.Diagnostics.HistoryCharsBefore,
		result.Diagnostics.HistoryCharsAfter)
}

func idForIndex(i int) string {
	return "call-" + itoa(i+1)
}

func fileForIndex(i int) string {
	files := []string{
		"src/main.go", "src/config.go", "src/router.go",
		"src/handler.go", "src/models.go", "src/utils.go",
		"src/middleware.go",
	}
	return files[i]
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
