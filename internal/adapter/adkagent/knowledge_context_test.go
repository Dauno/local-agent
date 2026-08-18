package adkagent

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// knowledgePassCompiler is a deterministic test compiler: it selects whole
// cards under the request budget with the canonical renderer and attaches
// the block inside the current user turn, mirroring the production
// contract. It returns the same final facts the epoch recorder consumes.
type knowledgePassCompiler struct{}

func (knowledgePassCompiler) Compile(_ context.Context, req domain.CompileRequest) (domain.CompileResult, error) {
	selected, err := domain.FitKnowledgeFrameCards(req.Knowledge, req.KnowledgeBudgetTokens, nil)
	if err != nil {
		return domain.CompileResult{}, err
	}
	block := domain.RenderKnowledgeFrameCards(selected)
	contents := domain.CloneContents(req.Contents)
	if block != "" && len(contents) > 0 && contents[0].HasPlainText() {
		// Exactly one additional part on the current plain user input: the
		// block travels inside the user message and never as a separate
		// model or assistant content.
		contents[0].Parts = append(contents[0].Parts, domain.ContentPart{Text: block})
	}
	identities := make([]string, 0, len(selected))
	for _, card := range selected {
		if identity := card.Identity(); identity != "" {
			identities = append(identities, identity)
		}
	}
	sort.Strings(identities)
	return domain.CompileResult{
		Contents:               contents,
		KnowledgeIdentities:    identities,
		KnowledgeSelectedCount: len(selected),
		KnowledgeOmittedCount:  len(req.Knowledge) - len(selected),
		Diagnostics:            domain.CompileDiagnostics{RequestTokensAfter: 1234, RequestCodePointsAfter: 567},
	}, nil
}

type knowledgeFakeEpochStore struct {
	mu     sync.Mutex
	epochs []domain.ContextEpoch
}

func (s *knowledgeFakeEpochStore) Latest(context.Context, string, string, string) (domain.ContextEpoch, error) {
	return domain.ContextEpoch{}, port.ErrContextEpochNotFound
}

func (s *knowledgeFakeEpochStore) Append(_ context.Context, epoch domain.ContextEpoch, expectedPreviousEpoch int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expectedPreviousEpoch != int64(len(s.epochs)) {
		return errors.New("unexpected previous epoch number")
	}
	s.epochs = append(s.epochs, epoch)
	return nil
}

func (s *knowledgeFakeEpochStore) Range(context.Context, string, string, string, int64, int64) ([]domain.ContextEpoch, error) {
	return nil, nil
}

func (s *knowledgeFakeEpochStore) latest() domain.ContextEpoch {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.epochs) == 0 {
		return domain.ContextEpoch{}
	}
	return s.epochs[len(s.epochs)-1]
}

type knowledgeEpochSessionService struct {
	session.Service
	ordinal int64
}

func (s *knowledgeEpochSessionService) LatestEventOrdinal(context.Context, string, string, string) (int64, error) {
	return s.ordinal, nil
}

func knowledgeRuntimeCard() domain.KnowledgeFrameCard {
	return domain.KnowledgeFrameCard{
		Kind: domain.KnowledgeRetrievalClaim,
		Claim: &domain.KnowledgeCard{
			ClaimID:         "kclaim_000000000000000000000001",
			Subject:         "shared subject",
			Predicate:       "is",
			Value:           domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "value"},
			ScopeKind:       domain.KnowledgeScopeProject,
			ScopeID:         "workspace",
			SourceClass:     domain.KnowledgeSourceHuman,
			SourceRef:       "slack:T12345678:dm:D12345678:1710000000.000001",
			Status:          domain.KnowledgeClaimAsserted,
			ValidFrom:       time.Unix(1700000000, 0).UTC(),
			RetrievalReason: string(domain.KnowledgeRetrievalReasonExactSubject),
		},
	}
}

func TestRuntimeKnowledgeReachesProviderRequestButNeverDurableEvents(t *testing.T) {
	llm := &fakeLLM{response: func(*model.LLMRequest) string { return "answer" }}
	sessionService := &knowledgeEpochSessionService{Service: session.InMemoryService(), ordinal: 0}
	epochs := &knowledgeFakeEpochStore{}
	runtime, err := NewRuntime(RuntimeConfig{
		AgentName: "Dev Agent", Model: llm, SessionService: sessionService,
		ContextCompiler: knowledgePassCompiler{}, EpochStore: epochs,
		ContextBudget:         domain.RequestBudget{HardTokens: 100_000, TargetTokens: 100_000},
		KnowledgeBudgetTokens: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	cards := []domain.KnowledgeFrameCard{knowledgeRuntimeCard()}
	turn, err := runtime.Run(t.Context(), port.AgentRequest{
		ConversationKey:    "slack:T12345678:dm:D12345678",
		Messages:           []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: "hello"}},
		Knowledge:          cards,
		WorkstreamRevision: 7,
	})
	if err != nil || turn.Text != "answer" {
		t.Fatalf("Run() = %#v, %v", turn, err)
	}

	requests := llm.recorded()
	if len(requests) != 1 {
		t.Fatalf("model calls = %d", len(requests))
	}
	// The block is part of the single current user message: one content,
	// user role, carrying both the original text and the block, never a
	// separate model or assistant message.
	if len(requests[0].contents) != 1 {
		t.Fatalf("provider request contents = %#v, want the block inside the current user message without a new content", requests[0].contents)
	}
	current := requests[0].contents[0]
	if current.role != "user" {
		t.Fatalf("knowledge block role = %q, want the user role inside the current user turn", current.role)
	}
	if !strings.Contains(current.text, "hello") || !strings.Contains(current.text, "[KNOWLEDGE DATA]") || !strings.Contains(current.text, "shared subject") {
		t.Fatalf("current user message = %q, want the original text plus the knowledge block", current.text)
	}

	loaded, err := sessionService.Get(t.Context(), &session.GetRequest{AppName: applicationName, UserID: ephemeralUserID, SessionID: "adk:slack:T12345678:dm:D12345678"})
	if err != nil || loaded == nil || loaded.Session == nil {
		t.Fatalf("load durable session: %v", err)
	}
	events := loaded.Session.Events()
	for index := 0; index < events.Len(); index++ {
		event := events.At(index)
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if strings.Contains(part.Text, "[KNOWLEDGE DATA]") || strings.Contains(part.Text, "shared subject") {
				t.Fatalf("durable event %d carries the knowledge block", index)
			}
		}
	}

	epoch := epochs.latest()
	if len(epoch.KnowledgeIdentities) != 1 || epoch.KnowledgeIdentities[0] != "claim:kclaim_000000000000000000000001" {
		t.Fatalf("epoch identities = %#v", epoch.KnowledgeIdentities)
	}
	if epoch.SelectedSourceCount != 1 || epoch.OmittedSourceCount != 0 {
		t.Fatalf("epoch counts = %d/%d", epoch.SelectedSourceCount, epoch.OmittedSourceCount)
	}
	if epoch.WorkstreamRevision != 7 {
		t.Fatalf("epoch workstream revision = %d", epoch.WorkstreamRevision)
	}
	if epoch.FrameTokens != 1234 || epoch.FrameCodePoints <= 0 || epoch.SourceDigest == "" {
		t.Fatalf("epoch frame identity = tokens %d codePoints %d digest %q", epoch.FrameTokens, epoch.FrameCodePoints, epoch.SourceDigest)
	}
}

func TestRuntimeStreamingAndNonStreamingProduceIdenticalEpochFacts(t *testing.T) {
	cards := []domain.KnowledgeFrameCard{knowledgeRuntimeCard()}
	run := func(stream bool) (domain.ContextEpoch, bool) {
		llm := &fakeLLM{response: func(*model.LLMRequest) string { return "answer" }, stream: []string{"an", "swer"}}
		sessionService := &knowledgeEpochSessionService{Service: session.InMemoryService(), ordinal: 0}
		epochs := &knowledgeFakeEpochStore{}
		runtime, err := NewRuntime(RuntimeConfig{
			AgentName: "Dev Agent", Model: llm, SessionService: sessionService,
			ContextCompiler: knowledgePassCompiler{}, EpochStore: epochs,
			ContextBudget:         domain.RequestBudget{HardTokens: 100_000, TargetTokens: 100_000},
			KnowledgeBudgetTokens: 100_000,
		})
		if err != nil {
			t.Fatal(err)
		}
		request := port.AgentRequest{
			ConversationKey:    "slack:T12345678:dm:D12345678",
			Messages:           []domain.Message{{Role: domain.RoleUser, UserID: "U12345678", Content: "hello"}},
			Knowledge:          cards,
			WorkstreamRevision: 7,
		}
		if stream {
			var completed *port.AgentTurn
			runtime.Stream(t.Context(), request, func(event port.AgentStreamEvent) bool {
				if event.Kind == port.AgentStreamCompleted {
					completed = event.Turn
				}
				return true
			})
			if completed == nil || completed.Text != "answer" {
				t.Fatalf("streaming turn = %#v", completed)
			}
		} else {
			turn, err := runtime.Run(t.Context(), request)
			if err != nil || turn.Text != "answer" {
				t.Fatalf("Run() = %#v, %v", turn, err)
			}
		}
		epoch := epochs.latest()
		return epoch, requestsContainKnowledge(llm)
	}

	streamingEpoch, streamingHadBlock := run(true)
	nonStreamingEpoch, nonStreamingHadBlock := run(false)
	if !streamingHadBlock || !nonStreamingHadBlock {
		t.Fatalf("knowledge block missing: streaming=%t non-streaming=%t", streamingHadBlock, nonStreamingHadBlock)
	}
	if len(streamingEpoch.KnowledgeIdentities) != 1 || !equalStringSlices(streamingEpoch.KnowledgeIdentities, nonStreamingEpoch.KnowledgeIdentities) {
		t.Fatalf("epoch identities streaming=%#v non-streaming=%#v", streamingEpoch.KnowledgeIdentities, nonStreamingEpoch.KnowledgeIdentities)
	}
	if streamingEpoch.SelectedSourceCount != nonStreamingEpoch.SelectedSourceCount ||
		streamingEpoch.OmittedSourceCount != nonStreamingEpoch.OmittedSourceCount ||
		streamingEpoch.WorkstreamRevision != nonStreamingEpoch.WorkstreamRevision ||
		streamingEpoch.FrameTokens != nonStreamingEpoch.FrameTokens ||
		streamingEpoch.SourceDigest != nonStreamingEpoch.SourceDigest {
		t.Fatalf("epoch facts differ: streaming=%#v non-streaming=%#v", streamingEpoch, nonStreamingEpoch)
	}
}

func requestsContainKnowledge(llm *fakeLLM) bool {
	for _, request := range llm.recorded() {
		for _, content := range request.contents {
			if strings.Contains(content.text, "[KNOWLEDGE DATA]") {
				return true
			}
		}
	}
	return false
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
