package contextcompiler

// TRD 08 item 4 (docs/root-orchestrator-v2/hallazgos/worker-prompt-trd08-matriz-benchmarks.md):
// how much of the per-turn cost is repeated frame serialization
// (FitKnowledgeFrameCards calling the cost function once per candidate
// card, each call re-serializing the whole candidate frame) versus deep
// clone (domain.CloneContents, which recursively clones maps and runs
// seven times per compilation per the TRD's Current Risks table).
//
// Repo convention: no Test functions in this file, so `go test ./...` pays
// only the compile cost. Reuses knowledgeFrameCard/knowledgeContentsFixture
// from knowledge_test.go and fakeResultStore/fakeTokenCounter from
// compiler_test.go: same package, same conventions.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// serializingFrameCounter pays the same shape of cost a real provider-shaped
// counter pays: it serializes the whole candidate frame on every call,
// rather than the fixed-shape knowledgeDeltaFrameCounter used by the
// correctness tests in knowledge_test.go, which only inspects the
// [KNOWLEDGE DATA] block and is therefore insensitive to frame size. This
// is what makes the frame-bytes axis of this benchmark mean something.
type serializingFrameCounter struct{ calls int }

func (c *serializingFrameCounter) CountContextFrame(_ context.Context, contents []domain.Content) (port.TokenCount, error) {
	c.calls++
	encoded, err := json.Marshal(contents)
	if err != nil {
		return port.TokenCount{}, err
	}
	return port.TokenCount{Tokens: len(encoded), Strategy: "provider", Exact: true}, nil
}

// compileBenchCardCounts is the candidate knowledge-card pool size offered
// to FitKnowledgeFrameCards per compilation. Retrieval bounds cap the
// realistic candidate count far below the 10,000-unit corpus scale that
// item 5 measures; this varies the admitted-candidate count itself.
var compileBenchCardCounts = []int{10, 50, 200}

// compileBenchFrameBytes is the size of the pre-existing frame (summary,
// recent turns, and the current active turn) that every candidate re-
// serialization must pay for, per the worker prompt's request to vary
// "the frame size" alongside the card count.
var compileBenchFrameBytes = []int{2_000, 20_000, 100_000}

func compileBenchKnowledgeCards(n int) []domain.KnowledgeFrameCard {
	cards := make([]domain.KnowledgeFrameCard, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("kclaim_%024x", i)
		cards[i] = knowledgeFrameCard(id, fmt.Sprintf("subject %05d", i), "exact_subject",
			fmt.Sprintf("value-%05d", i))
	}
	return cards
}

func compileBenchContents(frameBytes int) []domain.Content {
	filler := strings.Repeat("h", frameBytes)
	return []domain.Content{
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "old request " + filler}}},
		{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: "old answer"}}},
		{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "current request"}}},
	}
}

// BenchmarkCompileKnowledgeSelection isolates FitKnowledgeFrameCards's cost
// as N+1 full-candidate-frame serializations for N admitted cards, by
// running a full Compile with a knowledge candidate pool and a frame
// counter that reports its call count.
func BenchmarkCompileKnowledgeSelection(b *testing.B) {
	for _, frameBytes := range compileBenchFrameBytes {
		for _, cardCount := range compileBenchCardCounts {
			name := fmt.Sprintf("frameBytes=%d/cards=%d", frameBytes, cardCount)
			b.Run(name, func(b *testing.B) {
				contents := compileBenchContents(frameBytes)
				cards := compileBenchKnowledgeCards(cardCount)
				// A budget wide enough to admit every candidate card, so the
				// selection loop always runs the full N+1 cost-function calls
				// rather than stopping early on the first rejection. The
				// counter's tokens are serialized bytes, not code points, so
				// this stays deliberately generous.
				const budget = 100_000_000

				var frameCalls int
				b.ResetTimer()
				for b.Loop() {
					counter := &serializingFrameCounter{}
					compiler := New(newFakeResultStore(), fakeTokenCounter{})
					result, err := compiler.CompileFrame(context.Background(), domain.CompileRequest{
						Contents:              contents,
						ModelBudget:           domain.RequestBudget{HardTokens: 1_000_000, TargetTokens: 1_000_000},
						Knowledge:             cards,
						KnowledgeBudgetTokens: budget,
					}, counter)
					if err != nil {
						b.Fatal(err)
					}
					if result.KnowledgeSelectedCount != cardCount {
						b.Fatalf("selected %d of %d candidate cards: benchmark did not admit the full pool it was measuring",
							result.KnowledgeSelectedCount, cardCount)
					}
					frameCalls = counter.calls
				}
				b.ReportMetric(float64(frameCalls), "frame-counter-calls")
				b.ReportMetric(float64(cardCount), "candidate-cards")
				b.ReportMetric(float64(frameBytes), "frame-bytes")
			})
		}
	}
}
