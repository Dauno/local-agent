package contextcompiler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// workstreamDeltaFrameCounter mirrors knowledgeDeltaFrameCounter for the
// [WORKSTREAM DATA] block: every part whose text carries that preamble
// contributes its code points, everything else contributes zero, so the
// source delta equals the exact rendered block cost.
type workstreamDeltaFrameCounter struct {
	err         error
	failOnBlock bool
}

func (c *workstreamDeltaFrameCounter) CountContextFrame(_ context.Context, contents []domain.Content) (port.TokenCount, error) {
	hasBlock := false
	tokens := 0
	for _, content := range contents {
		for _, part := range content.Parts {
			if strings.HasPrefix(part.Text, "[WORKSTREAM DATA]") {
				hasBlock = true
				tokens += utf8.RuneCountInString(part.Text)
			}
		}
	}
	if hasBlock && c.failOnBlock {
		return port.TokenCount{}, c.err
	}
	if !c.failOnBlock && c.err != nil {
		return port.TokenCount{}, c.err
	}
	return port.TokenCount{Tokens: tokens, Strategy: "provider"}, nil
}

func workstreamSnapshotFixture() domain.WorkstreamSnapshot {
	return domain.WorkstreamSnapshot{
		ID: "ws_000000000000000000000001", ConversationKey: "slack:T12345678:dm:D12345678",
		OwnerActor: "U12345678", Project: "workspace", Status: domain.WorkstreamActive, Revision: 3,
		Objective: "ship the audit repairs", CurrentPhase: "implementation",
	}
}

func workstreamBlockText(contents []domain.Content) (string, bool) {
	for _, content := range contents {
		for _, part := range content.Parts {
			if strings.HasPrefix(part.Text, "[WORKSTREAM DATA]") {
				return part.Text, true
			}
		}
	}
	return "", false
}

func TestWorkstreamSnapshotAdmittedWithinBudget(t *testing.T) {
	snapshot := workstreamSnapshotFixture()
	rendered, err := domain.RenderWorkstreamSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	exactBudget := utf8.RuneCountInString(rendered)
	result, err := New(newFakeResultStore(), fakeTokenCounter{}).CompileFrame(t.Context(), domain.CompileRequest{
		Contents: knowledgeContentsFixture(), ModelBudget: domain.RequestBudget{HardTokens: 10_000, TargetTokens: 10_000},
		Workstream: &snapshot, WorkstreamBudgetTokens: exactBudget,
	}, &workstreamDeltaFrameCounter{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.WorkstreamSnapshotIncluded || result.WorkstreamOmissionReason != "" {
		t.Fatalf("included=%t reason=%q, want included with no omission", result.WorkstreamSnapshotIncluded, result.WorkstreamOmissionReason)
	}
	text, found := workstreamBlockText(result.Contents)
	if !found || text != rendered {
		t.Fatalf("rendered block mismatch: found=%t", found)
	}
}

func TestWorkstreamSnapshotOmittedOverBudget(t *testing.T) {
	snapshot := workstreamSnapshotFixture()
	rendered, err := domain.RenderWorkstreamSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	exactBudget := utf8.RuneCountInString(rendered)
	result, err := New(newFakeResultStore(), fakeTokenCounter{}).CompileFrame(t.Context(), domain.CompileRequest{
		Contents: knowledgeContentsFixture(), ModelBudget: domain.RequestBudget{HardTokens: 10_000, TargetTokens: 10_000},
		Workstream: &snapshot, WorkstreamBudgetTokens: exactBudget - 1,
	}, &workstreamDeltaFrameCounter{})
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkstreamSnapshotIncluded || result.WorkstreamOmissionReason != "source_budget" {
		t.Fatalf("included=%t reason=%q, want omitted with source_budget", result.WorkstreamSnapshotIncluded, result.WorkstreamOmissionReason)
	}
	if _, found := workstreamBlockText(result.Contents); found {
		t.Fatal("workstream block present after exceeding its budget")
	}
}

func TestWorkstreamSnapshotNilOmitted(t *testing.T) {
	result, err := New(newFakeResultStore(), fakeTokenCounter{}).CompileFrame(t.Context(), domain.CompileRequest{
		Contents: knowledgeContentsFixture(), ModelBudget: domain.RequestBudget{HardTokens: 10_000, TargetTokens: 10_000},
		WorkstreamBudgetTokens: 1000,
	}, &workstreamDeltaFrameCounter{})
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkstreamSnapshotIncluded || result.WorkstreamOmissionReason != "no_snapshot" {
		t.Fatalf("included=%t reason=%q, want no_snapshot with no active workstream", result.WorkstreamSnapshotIncluded, result.WorkstreamOmissionReason)
	}
}

func TestWorkstreamSnapshotDisabledByZeroBudget(t *testing.T) {
	snapshot := workstreamSnapshotFixture()
	result, err := New(newFakeResultStore(), fakeTokenCounter{}).CompileFrame(t.Context(), domain.CompileRequest{
		Contents: knowledgeContentsFixture(), ModelBudget: domain.RequestBudget{HardTokens: 10_000, TargetTokens: 10_000},
		Workstream: &snapshot, WorkstreamBudgetTokens: 0,
	}, &workstreamDeltaFrameCounter{})
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkstreamSnapshotIncluded || result.WorkstreamOmissionReason != "disabled" {
		t.Fatalf("included=%t reason=%q, want disabled with zero budget", result.WorkstreamSnapshotIncluded, result.WorkstreamOmissionReason)
	}
}

func TestWorkstreamNegativeBudgetRejectsCompile(t *testing.T) {
	snapshot := workstreamSnapshotFixture()
	_, err := New(newFakeResultStore(), fakeTokenCounter{}).Compile(t.Context(), domain.CompileRequest{
		Contents: knowledgeContentsFixture(), ModelBudget: domain.RequestBudget{HardTokens: 1000, TargetTokens: 1000},
		Workstream: &snapshot, WorkstreamBudgetTokens: -1,
	})
	if err == nil {
		t.Fatal("Compile() with a negative workstream budget succeeded")
	}
}

func TestWorkstreamEmptyRequestOmitsSnapshot(t *testing.T) {
	snapshot := workstreamSnapshotFixture()
	result, err := New(newFakeResultStore(), fakeTokenCounter{}).Compile(t.Context(), domain.CompileRequest{
		Contents: nil, ModelBudget: domain.RequestBudget{HardTokens: 1000, TargetTokens: 1000},
		Workstream: &snapshot, WorkstreamBudgetTokens: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkstreamSnapshotIncluded {
		t.Fatal("workstream snapshot included with no turn to attach it to")
	}
}

// runeCountFrameCounter is a real, deterministic provider-shaped counter: the
// token count is the exact sum of every content part's rendered text length.
// Unlike the delta-only fakes above, it measures the whole assembled frame,
// so it drives both per-source budgeting and the overall admission count
// consistently in the priority test below.
type runeCountFrameCounter struct{}

func (runeCountFrameCounter) CountContextFrame(_ context.Context, contents []domain.Content) (port.TokenCount, error) {
	total := 0
	for _, content := range contents {
		for _, part := range content.Parts {
			total += utf8.RuneCountInString(part.Text)
		}
	}
	return port.TokenCount{Tokens: total, Strategy: "provider"}, nil
}

// TestWorkstreamOutranksSummaryAndKnowledgeUnderPressure pins the TRD 03
// selection-priority requirement from hallazgo 2: the active workstream
// snapshot ranks above the rolling summary and knowledge cards. Under total
// pressure, summary and knowledge are evicted first while the workstream
// block survives; only further pressure evicts it, last among the optional
// sources and strictly before protected active content.
func TestWorkstreamOutranksSummaryAndKnowledgeUnderPressure(t *testing.T) {
	snapshot := workstreamSnapshotFixture()
	workstreamRendered, err := domain.RenderWorkstreamSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	cards := []domain.KnowledgeFrameCard{knowledgeFrameCard("kclaim_000000000000000000000099", "subject", "exact_subject", "")}
	knowledgeRendered := domain.RenderKnowledgeFrameCards(cards)
	summaryText := "summary"
	summaryRendered := summaryReference(summaryText)

	activeCost := utf8.RuneCountInString("current request")
	recentCost := utf8.RuneCountInString("old request") + utf8.RuneCountInString("old answer")
	summaryCost := utf8.RuneCountInString(summaryRendered)
	knowledgeCost := utf8.RuneCountInString(knowledgeRendered)
	workstreamCost := utf8.RuneCountInString(workstreamRendered)
	total := activeCost + recentCost + summaryCost + knowledgeCost + workstreamCost

	newBase := func() domain.CompileRequest {
		return domain.CompileRequest{
			Contents: knowledgeContentsFixture(), ExistingSummary: summaryText,
			Knowledge:              cards,
			KnowledgeBudgetTokens:  knowledgeCost,
			Workstream:             &snapshot,
			WorkstreamBudgetTokens: workstreamCost,
		}
	}

	t.Run("summary and knowledge evicted, workstream survives", func(t *testing.T) {
		req := newBase()
		target := activeCost + workstreamCost
		req.ModelBudget = domain.RequestBudget{HardTokens: total + 100, TriggerTokens: target, TargetTokens: target}
		result, err := New(newFakeResultStore(), fakeTokenCounter{}).CompileFrame(t.Context(), req, runeCountFrameCounter{})
		if err != nil {
			t.Fatal(err)
		}
		if !result.WorkstreamSnapshotIncluded || result.WorkstreamOmissionReason != "" {
			t.Fatalf("included=%t reason=%q, want the workstream block retained", result.WorkstreamSnapshotIncluded, result.WorkstreamOmissionReason)
		}
		if _, found := workstreamBlockText(result.Contents); !found {
			t.Fatal("workstream block was evicted before summary and knowledge")
		}
		if result.KnowledgeSelectedCount != 0 {
			t.Fatalf("knowledge selected=%d, want evicted under pressure", result.KnowledgeSelectedCount)
		}
	})

	t.Run("workstream evicted last under sustained pressure", func(t *testing.T) {
		req := newBase()
		target := activeCost
		req.ModelBudget = domain.RequestBudget{HardTokens: total + 100, TriggerTokens: target, TargetTokens: target}
		result, err := New(newFakeResultStore(), fakeTokenCounter{}).CompileFrame(t.Context(), req, runeCountFrameCounter{})
		if err != nil {
			t.Fatal(err)
		}
		if result.WorkstreamSnapshotIncluded || result.WorkstreamOmissionReason != "total_pressure" {
			t.Fatalf("included=%t reason=%q, want evicted under total pressure", result.WorkstreamSnapshotIncluded, result.WorkstreamOmissionReason)
		}
		if _, found := workstreamBlockText(result.Contents); found {
			t.Fatal("workstream block survived total-pressure eviction")
		}
	})

	t.Run("workstream never makes request irreducible", func(t *testing.T) {
		req := newBase()
		target := activeCost - 1
		req.ModelBudget = domain.RequestBudget{HardTokens: target, TriggerTokens: target, TargetTokens: target}
		result, err := New(newFakeResultStore(), fakeTokenCounter{}).CompileFrame(t.Context(), req, runeCountFrameCounter{})
		if !errors.Is(err, domain.ErrIrreducibleContext) {
			t.Fatalf("error = %v, want irreducible", err)
		}
		if result.WorkstreamSnapshotIncluded {
			t.Fatal("workstream snapshot included in the irreducible result")
		}
	})
}
