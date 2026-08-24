package adkagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// epochIdentitiesCompiler is a deterministic test compiler that reports the
// summary as included and appends one tool FunctionResponse carrying a
// well-formed result_id, so the epoch recorder has real material to extract
// SummaryIdentity and ResultIdentities from.
type epochIdentitiesCompiler struct {
	resultID        string
	summaryIncluded bool
}

func (c epochIdentitiesCompiler) Compile(_ context.Context, req domain.CompileRequest) (domain.CompileResult, error) {
	contents := domain.CloneContents(req.Contents)
	contents = append(contents, domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{
		FunctionResponse: &domain.FunctionResponse{ID: "call-1", Name: "workstream_result_handle", Response: map[string]any{"result_id": c.resultID}},
	}}})
	return domain.CompileResult{
		Contents: contents, SummaryIncluded: c.summaryIncluded,
		Diagnostics: domain.CompileDiagnostics{RequestTokensAfter: 42, RequestCodePointsAfter: 42},
	}, nil
}

type fakeSummaryStore struct{ record port.SummaryRecord }

func (s fakeSummaryStore) LatestSummary(context.Context, string) (port.SummaryRecord, error) {
	return s.record, nil
}
func (fakeSummaryStore) CommitSummary(context.Context, port.SummaryCommit) (bool, error) {
	return false, nil
}
func (fakeSummaryStore) ScheduleSummaryJob(context.Context, string, int64, time.Time) (bool, error) {
	return false, nil
}
func (fakeSummaryStore) ClaimSummaryJob(context.Context, time.Time) (port.SummaryJob, error) {
	return port.SummaryJob{}, nil
}
func (fakeSummaryStore) CompleteSummaryJob(context.Context, port.SummaryJob) error { return nil }
func (fakeSummaryStore) FailSummaryJob(context.Context, port.SummaryJob, time.Time) error {
	return nil
}

// TestRuntimeEpochRecordsSummaryAndResultIdentities pins hallazgo 7: the
// durable epoch fills SummaryIdentity from the summary record's durable
// source digest and covered ordinal (never its text), and ResultIdentities
// from the result_id values actually admitted into the final frame.
func TestRuntimeEpochRecordsSummaryAndResultIdentities(t *testing.T) {
	llm := &fakeLLM{response: func(*model.LLMRequest) string { return "answer" }}
	sessionService := &knowledgeEpochSessionService{Service: session.InMemoryService(), ordinal: 0}
	epochs := &knowledgeFakeEpochStore{}
	resultID := strings.Repeat("a", 64)
	summaries := fakeSummaryStore{record: port.SummaryRecord{
		SessionIdentity: "adk:slack:T12345678:dm:D12345678", CoveredThroughOrdinal: 9,
		SourceDigest: strings.Repeat("b", 64), SanitizedText: "the untrusted summary text must never reach the epoch",
	}}
	runtime, err := NewRuntime(RuntimeConfig{
		AgentName: "Dev Agent", Model: llm, SessionService: sessionService,
		ContextCompiler: epochIdentitiesCompiler{resultID: resultID, summaryIncluded: true},
		EpochStore:      epochs, SummaryStore: summaries,
		ContextBudget: domain.RequestBudget{HardTokens: 100_000, TargetTokens: 100_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey: "slack:T12345678:dm:D12345678",
		Messages:        []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: "hello"}},
	})
	if err != nil || turn.Text != "answer" {
		t.Fatalf("Run() = %#v, %v", turn, err)
	}
	epoch := epochs.latest()
	wantSummaryIdentity := summaries.record.SourceDigest + "@9"
	if epoch.SummaryIdentity != wantSummaryIdentity {
		t.Fatalf("epoch summary identity = %q, want %q", epoch.SummaryIdentity, wantSummaryIdentity)
	}
	if strings.Contains(epoch.SummaryIdentity, "untrusted summary text") {
		t.Fatal("epoch summary identity leaked summary text")
	}
	if len(epoch.ResultIdentities) != 1 || epoch.ResultIdentities[0] != resultID {
		t.Fatalf("epoch result identities = %#v, want [%q]", epoch.ResultIdentities, resultID)
	}
}

// TestRuntimeEpochLeavesSummaryIdentityEmptyWhenNotSelected pins the
// "empty by outcome, not by omission" restriction: when the compiler
// reports the summary was not included in the final frame, SummaryIdentity
// stays empty even though a summary record exists.
func TestRuntimeEpochLeavesSummaryIdentityEmptyWhenNotSelected(t *testing.T) {
	llm := &fakeLLM{response: func(*model.LLMRequest) string { return "answer" }}
	sessionService := &knowledgeEpochSessionService{Service: session.InMemoryService(), ordinal: 0}
	epochs := &knowledgeFakeEpochStore{}
	summaries := fakeSummaryStore{record: port.SummaryRecord{
		SessionIdentity: "adk:slack:T12345678:dm:D12345678", CoveredThroughOrdinal: 3,
		SourceDigest: strings.Repeat("c", 64), SanitizedText: "evicted summary",
	}}
	runtime, err := NewRuntime(RuntimeConfig{
		AgentName: "Dev Agent", Model: llm, SessionService: sessionService,
		ContextCompiler: epochIdentitiesCompiler{resultID: strings.Repeat("d", 64), summaryIncluded: false},
		EpochStore:      epochs, SummaryStore: summaries,
		ContextBudget: domain.RequestBudget{HardTokens: 100_000, TargetTokens: 100_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey: "slack:T12345678:dm:D12345678",
		Messages:        []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	epoch := epochs.latest()
	if epoch.SummaryIdentity != "" {
		t.Fatalf("epoch summary identity = %q, want empty when the compiler reports the summary was not selected", epoch.SummaryIdentity)
	}
}
