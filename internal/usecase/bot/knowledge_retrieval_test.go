package bot

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type fakeKnowledgeRetriever struct {
	mu          sync.Mutex
	calls       int
	request     domain.KnowledgeRetrievalRequest
	cards       []domain.KnowledgeFrameCard
	err         error
	recordedCtx context.Context
	started     chan struct{}
	release     <-chan struct{}
}

func (r *fakeKnowledgeRetriever) Retrieve(ctx context.Context, request domain.KnowledgeRetrievalRequest) (domain.KnowledgeRetrievalResult, error) {
	r.mu.Lock()
	r.calls++
	r.request = request
	r.recordedCtx = ctx
	started := r.started
	release := r.release
	r.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return domain.KnowledgeRetrievalResult{}, ctx.Err()
		}
	}
	if r.err != nil {
		return domain.KnowledgeRetrievalResult{}, r.err
	}
	return domain.KnowledgeRetrievalResult{Cards: append([]domain.KnowledgeFrameCard(nil), r.cards...)}, nil
}

func (r *fakeKnowledgeRetriever) snapshot() (int, domain.KnowledgeRetrievalRequest, context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.request, r.recordedCtx
}

type fakeRetrievalBindingResolver struct {
	mu      sync.Mutex
	calls   int
	binding port.KnowledgeRetrievalBinding
	err     error
}

func (r *fakeRetrievalBindingResolver) ResolveRetrievalBinding(ctx context.Context, team, actor string, conversation domain.ConversationKey, exchangeTS string) (port.KnowledgeRetrievalBinding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return port.KnowledgeRetrievalBinding{}, r.err
	}
	return r.binding, nil
}

type capturingStreamingRuntime struct {
	request port.AgentRequest
}

func (r *capturingStreamingRuntime) Stream(_ context.Context, request port.AgentRequest, yield func(port.AgentStreamEvent) bool) {
	r.request = request
	yield(port.AgentStreamEvent{Kind: port.AgentStreamTextDelta, TextDelta: "streamed"})
	yield(port.AgentStreamEvent{Kind: port.AgentStreamCompleted, Turn: &port.AgentTurn{Text: "streamed"}})
}

func knowledgeRetrievalLimits() domain.KnowledgeRetrievalLimits {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	limits.TimeoutSeconds = 1
	return limits
}

func knowledgeRetrievalCard() domain.KnowledgeFrameCard {
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

func retrievalBindingFixture(conversation domain.ConversationKey) port.KnowledgeRetrievalBinding {
	snapshot := domain.WorkstreamSnapshot{
		ID: "ws-1", ConversationKey: conversation, OwnerActor: "U12345678",
		Project: "workspace", Status: domain.WorkstreamActive, Revision: 7,
	}
	return port.KnowledgeRetrievalBinding{
		Binding:    domain.KnowledgeWriteBinding{Team: "T12345678", Actor: "U12345678", Conversation: conversation, Project: "workspace", WorkstreamID: "ws-1"},
		Workstream: &snapshot,
		ExchangeTS: "1700000000.000001",
	}
}

func TestKnowledgeRetrievalRunsExactlyOnceWithHostBoundRequest(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	publisher := &fakePublisher{}
	retriever := &fakeKnowledgeRetriever{cards: []domain.KnowledgeFrameCard{knowledgeRetrievalCard()}}
	resolver := &fakeRetrievalBindingResolver{binding: retrievalBindingFixture(func() domain.ConversationKey { key, _ := botInvocation().ConversationKey(); return key }())}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, func(cfg *Config) {
		cfg.KnowledgeRetrievalLimits = knowledgeRetrievalLimits()
		cfg.WorkstreamsEnabled = true
	})
	service.knowledgeRetriever = retriever
	service.retrievalBindings = resolver

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	calls, request, recordedCtx := retriever.snapshot()
	if calls != 1 {
		t.Fatalf("retriever calls = %d, want exactly one", calls)
	}
	if resolver.calls != 1 {
		t.Fatalf("binding resolver calls = %d, want exactly one", resolver.calls)
	}
	conversation, _ := botInvocation().ConversationKey()
	if request.Binding.Team != "T12345678" || request.Binding.Actor != "U12345678" ||
		request.Binding.Conversation != conversation ||
		request.Binding.Project != "workspace" || request.Binding.WorkstreamID != "ws-1" {
		t.Fatalf("retrieval binding = %#v", request.Binding)
	}
	if request.ExchangeTS != "1700000000.000001" || request.CurrentMessage != "hello" {
		t.Fatalf("retrieval turn identity = %q %q", request.ExchangeTS, request.CurrentMessage)
	}
	if request.Now != time.Unix(1700000000, 0).UTC() {
		t.Fatalf("retrieval clock = %v, want injected UTC clock", request.Now)
	}
	if request.Workstream == nil || request.Workstream.Revision != 7 || request.Workstream.Project != "workspace" {
		t.Fatalf("retrieval snapshot = %#v", request.Workstream)
	}
	if request.Limits != knowledgeRetrievalLimits() {
		t.Fatalf("retrieval limits = %#v", request.Limits)
	}
	if deadline, ok := recordedCtx.Deadline(); !ok || time.Until(deadline) > time.Second || time.Until(deadline) < 0 {
		t.Fatalf("retrieval context deadline = %v, ok=%t, want ~1s", deadline, ok)
	}
	if len(runtime.runRequest.Knowledge) != 1 || runtime.runRequest.Knowledge[0].Identity() != "claim:kclaim_000000000000000000000001" {
		t.Fatalf("runtime knowledge = %#v", runtime.runRequest.Knowledge)
	}
	if runtime.runRequest.WorkstreamRevision != 7 {
		t.Fatalf("runtime workstream revision = %d, want 7", runtime.runRequest.WorkstreamRevision)
	}
	if runtime.runRequest.WorkstreamSnapshot == nil || runtime.runRequest.WorkstreamSnapshot.Revision != 7 || runtime.runRequest.WorkstreamSnapshot.Project != "workspace" {
		t.Fatalf("runtime workstream snapshot = %#v", runtime.runRequest.WorkstreamSnapshot)
	}
}

func TestKnowledgeRetrievalTimeoutContinuesTurnWithoutCards(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	publisher := &fakePublisher{}
	retriever := &fakeKnowledgeRetriever{err: context.DeadlineExceeded}
	resolver := &fakeRetrievalBindingResolver{binding: retrievalBindingFixture(func() domain.ConversationKey { key, _ := botInvocation().ConversationKey(); return key }())}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, func(cfg *Config) {
		cfg.KnowledgeRetrievalLimits = knowledgeRetrievalLimits()
		cfg.WorkstreamsEnabled = true
	})
	service.knowledgeRetriever = retriever
	service.retrievalBindings = resolver

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if runtime.runCalls != 1 || len(runtime.runRequest.Knowledge) != 0 || runtime.runRequest.WorkstreamRevision != 7 {
		t.Fatalf("turn continued with knowledge = %#v revision = %d", runtime.runRequest.Knowledge, runtime.runRequest.WorkstreamRevision)
	}
}

func TestKnowledgeRetrievalBindingFailureContinuesTurnWithoutCards(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	publisher := &fakePublisher{}
	retriever := &fakeKnowledgeRetriever{cards: []domain.KnowledgeFrameCard{knowledgeRetrievalCard()}}
	resolver := &fakeRetrievalBindingResolver{err: errors.New("workstream store failed")}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, func(cfg *Config) {
		cfg.KnowledgeRetrievalLimits = knowledgeRetrievalLimits()
	})
	service.knowledgeRetriever = retriever
	service.retrievalBindings = resolver

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if calls, _, _ := retriever.snapshot(); calls != 0 {
		t.Fatalf("retriever calls = %d, want zero after binding failure", calls)
	}
	if runtime.runCalls != 1 || len(runtime.runRequest.Knowledge) != 0 {
		t.Fatalf("turn continued with knowledge = %#v", runtime.runRequest.Knowledge)
	}
}

func TestKnowledgeRetrievalFailureContinuesTurnWithoutCards(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	publisher := &fakePublisher{}
	retriever := &fakeKnowledgeRetriever{err: errors.New("authoritative reader unavailable")}
	resolver := &fakeRetrievalBindingResolver{binding: retrievalBindingFixture(func() domain.ConversationKey { key, _ := botInvocation().ConversationKey(); return key }())}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, func(cfg *Config) {
		cfg.KnowledgeRetrievalLimits = knowledgeRetrievalLimits()
	})
	service.knowledgeRetriever = retriever
	service.retrievalBindings = resolver

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if runtime.runCalls != 1 || len(runtime.runRequest.Knowledge) != 0 {
		t.Fatalf("turn continued with knowledge = %#v", runtime.runRequest.Knowledge)
	}
}

// TestKnowledgeRetrievalDisabledStillResolvesWorkstreamRevision pins the
// hallazgo-3 fix: with the knowledge retriever gate disabled (the default),
// binding resolution still runs so the active workstream revision reaches
// the epoch independently of retrieval. The retriever itself makes zero
// calls. With orchestration.workstreams.enabled false the revision and
// snapshot stay zero/nil even though the resolved binding carries an active
// workstream, because the frame source is gated separately.
func TestKnowledgeRetrievalDisabledStillResolvesWorkstreamRevision(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	publisher := &fakePublisher{}
	retriever := &fakeKnowledgeRetriever{}
	resolver := &fakeRetrievalBindingResolver{binding: retrievalBindingFixture(func() domain.ConversationKey { key, _ := botInvocation().ConversationKey(); return key }())}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, nil)
	service.retrievalBindings = resolver

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if calls, _, _ := retriever.snapshot(); calls != 0 {
		t.Fatalf("retriever calls = %d with the gate disabled", calls)
	}
	if resolver.calls != 1 {
		t.Fatalf("binding resolver calls = %d, want exactly one even with retrieval disabled", resolver.calls)
	}
	if runtime.runRequest.WorkstreamRevision != 0 || runtime.runRequest.WorkstreamSnapshot != nil {
		t.Fatalf("runtime workstream revision/snapshot = %d/%#v, want zero/nil with orchestration.workstreams.enabled false",
			runtime.runRequest.WorkstreamRevision, runtime.runRequest.WorkstreamSnapshot)
	}
}

// TestKnowledgeRetrievalDisabledWithWorkstreamsEnabledRecordsRevision pins
// the other half of hallazgo 3: once orchestration.workstreams.enabled is
// true, the revision and snapshot flow through even with the knowledge
// retriever gate disabled.
func TestKnowledgeRetrievalDisabledWithWorkstreamsEnabledRecordsRevision(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	publisher := &fakePublisher{}
	retriever := &fakeKnowledgeRetriever{}
	resolver := &fakeRetrievalBindingResolver{binding: retrievalBindingFixture(func() domain.ConversationKey { key, _ := botInvocation().ConversationKey(); return key }())}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, func(cfg *Config) {
		cfg.WorkstreamsEnabled = true
	})
	service.retrievalBindings = resolver

	outcome, err := service.Handle(t.Context(), botInvocation())
	if err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	if calls, _, _ := retriever.snapshot(); calls != 0 {
		t.Fatalf("retriever calls = %d with the retrieval gate disabled", calls)
	}
	if resolver.calls != 1 {
		t.Fatalf("binding resolver calls = %d, want exactly one", resolver.calls)
	}
	if runtime.runRequest.WorkstreamRevision != 7 || runtime.runRequest.WorkstreamSnapshot == nil {
		t.Fatalf("runtime workstream revision/snapshot = %d/%#v, want 7/non-nil",
			runtime.runRequest.WorkstreamRevision, runtime.runRequest.WorkstreamSnapshot)
	}
}

func TestKnowledgeRetrievalStreamingAndNonStreamingReceiveIdenticalData(t *testing.T) {
	cards := []domain.KnowledgeFrameCard{knowledgeRetrievalCard()}
	runStreaming := func() port.AgentRequest {
		store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
		standard := &fakeStandardExperience{}
		stream := &capturingStreamingRuntime{}
		retriever := &fakeKnowledgeRetriever{cards: cards}
		resolver := &fakeRetrievalBindingResolver{binding: retrievalBindingFixture(func() domain.ConversationKey { key, _ := botInvocation().ConversationKey(); return key }())}
		service := newTestService(t, store, &fakeRuntime{}, &fakeHistory{}, &fakePublisher{}, func(cfg *Config) {
			cfg.KnowledgeRetrievalLimits = knowledgeRetrievalLimits()
			cfg.WorkstreamsEnabled = true
		})
		service.cfg.StreamingEnabled = true
		service.cfg.UpdateInterval = 3 * time.Second
		service.cfg.StreamingCarryRunes = 128
		service.streamingRuntime = stream
		service.standardStore = standard
		service.incrementalPublisher = standard
		service.knowledgeRetriever = retriever
		service.retrievalBindings = resolver
		if outcome, err := service.Handle(t.Context(), botInvocation()); err != nil || outcome != OutcomeResponded {
			t.Fatalf("streaming outcome = %q, err = %v", outcome, err)
		}
		return stream.request
	}
	runNonStreaming := func() port.AgentRequest {
		store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
		runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
		retriever := &fakeKnowledgeRetriever{cards: cards}
		resolver := &fakeRetrievalBindingResolver{binding: retrievalBindingFixture(func() domain.ConversationKey { key, _ := botInvocation().ConversationKey(); return key }())}
		service := newTestService(t, store, runtime, &fakeHistory{}, &fakePublisher{}, func(cfg *Config) {
			cfg.KnowledgeRetrievalLimits = knowledgeRetrievalLimits()
			cfg.WorkstreamsEnabled = true
		})
		service.knowledgeRetriever = retriever
		service.retrievalBindings = resolver
		if outcome, err := service.Handle(t.Context(), botInvocation()); err != nil || outcome != OutcomeResponded {
			t.Fatalf("non-streaming outcome = %q, err = %v", outcome, err)
		}
		return runtime.runRequest
	}
	streamingRequest := runStreaming()
	nonStreamingRequest := runNonStreaming()
	if !reflect.DeepEqual(streamingRequest.Knowledge, nonStreamingRequest.Knowledge) {
		t.Fatalf("streaming knowledge = %#v, non-streaming = %#v", streamingRequest.Knowledge, nonStreamingRequest.Knowledge)
	}
	if streamingRequest.WorkstreamRevision != nonStreamingRequest.WorkstreamRevision || streamingRequest.WorkstreamRevision != 7 {
		t.Fatalf("streaming revision = %d, non-streaming = %d", streamingRequest.WorkstreamRevision, nonStreamingRequest.WorkstreamRevision)
	}
	if !reflect.DeepEqual(streamingRequest.WorkstreamSnapshot, nonStreamingRequest.WorkstreamSnapshot) || streamingRequest.WorkstreamSnapshot == nil {
		t.Fatalf("streaming snapshot = %#v, non-streaming = %#v", streamingRequest.WorkstreamSnapshot, nonStreamingRequest.WorkstreamSnapshot)
	}
}

// TestKnowledgeRetrievalLogsBoundedDiagnostics pins hallazgo 13: the
// per-turn TRD 06 diagnostics that metrics alone do not carry (ranking
// policy, fingerprint version, enabled channels, omission categories) now
// reach an observable destination — a bounded, sanitized log line — with
// no query, card content, vector, handle, digest, or actor/conversation
// identity ever logged. Selected card identities are reduced to a count.
func TestKnowledgeRetrievalLogsBoundedDiagnostics(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	publisher := &fakePublisher{}
	card := knowledgeRetrievalCard()
	retriever := &fakeKnowledgeRetriever{cards: []domain.KnowledgeFrameCard{card}}
	resolver := &fakeRetrievalBindingResolver{binding: retrievalBindingFixture(func() domain.ConversationKey { key, _ := botInvocation().ConversationKey(); return key }())}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, func(cfg *Config) {
		cfg.KnowledgeRetrievalLimits = knowledgeRetrievalLimits()
	})
	service.knowledgeRetriever = &diagnosticsKnowledgeRetriever{fakeKnowledgeRetriever: retriever, diagnostics: domain.KnowledgeRetrievalDiagnostics{
		RankingPolicy: "knowledge-rank-v1", IndexFingerprintVersion: "fp-v1",
		EnabledChannels: []domain.KnowledgeRetrievalChannel{domain.KnowledgeRetrievalChannelExact, domain.KnowledgeRetrievalChannelLexical},
		CandidateCount:  3, SelectedCount: 1, OmittedCount: 2,
		SelectedIdentities: []string{card.Identity()},
		Failures:           []domain.KnowledgeRetrievalFailure{domain.KnowledgeRetrievalSemanticUnavailable},
		Omissions:          []domain.KnowledgeRetrievalOmission{domain.KnowledgeRetrievalOmissionCardOverBudget},
		Elapsed:            42 * time.Millisecond,
	}}
	service.retrievalBindings = resolver

	var logOutput strings.Builder
	service.logger = slog.New(slog.NewTextHandler(&logOutput, nil))

	if outcome, err := service.Handle(t.Context(), botInvocation()); err != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	var diagnosticsLine string
	for _, line := range strings.Split(logOutput.String(), "\n") {
		if strings.Contains(line, "knowledge retrieval diagnostics") {
			diagnosticsLine = line
			break
		}
	}
	if diagnosticsLine == "" {
		t.Fatalf("diagnostics log line not found: %s", logOutput.String())
	}
	for _, want := range []string{
		"ranking_policy=knowledge-rank-v1", "fingerprint_version=fp-v1",
		"candidate_count=3", "selected_count=1", "omitted_count=2", "selected_identity_count=1",
		"semantic_unavailable", "card_over_budget", "elapsed_ms=42",
	} {
		if !strings.Contains(diagnosticsLine, want) {
			t.Fatalf("diagnostics log line missing %q: %s", want, diagnosticsLine)
		}
	}
	for _, forbidden := range []string{"hello", card.Identity(), "U12345678", "T12345678", "D12345678"} {
		if strings.Contains(diagnosticsLine, forbidden) {
			t.Fatalf("diagnostics log line leaked %q: %s", forbidden, diagnosticsLine)
		}
	}
}

// diagnosticsKnowledgeRetriever wraps fakeKnowledgeRetriever to attach
// fixed diagnostics to a successful result, since the base fake always
// returns a zero-value KnowledgeRetrievalDiagnostics.
type diagnosticsKnowledgeRetriever struct {
	*fakeKnowledgeRetriever
	diagnostics domain.KnowledgeRetrievalDiagnostics
}

func (r *diagnosticsKnowledgeRetriever) Retrieve(ctx context.Context, request domain.KnowledgeRetrievalRequest) (domain.KnowledgeRetrievalResult, error) {
	result, err := r.fakeKnowledgeRetriever.Retrieve(ctx, request)
	if err != nil {
		return result, err
	}
	result.Diagnostics = r.diagnostics
	return result, nil
}

// TestKnowledgeRetrievalLateSuccessAfterCancellationIsRejected pins
// FIND-094: a retriever that returns a successful result after its context
// was cancelled must never admit cards; the turn continues without
// knowledge and the runtime request stays empty.
func TestKnowledgeRetrievalLateSuccessAfterCancellationIsRejected(t *testing.T) {
	store := &fakeStore{recent: make(map[domain.ConversationKey][]domain.Message)}
	runtime := &fakeRuntime{runTurn: port.AgentTurn{Text: "answer"}}
	publisher := &fakePublisher{}
	started := make(chan struct{})
	release := make(chan struct{})
	retriever := &fakeKnowledgeRetriever{cards: []domain.KnowledgeFrameCard{knowledgeRetrievalCard()}, started: started, release: release}
	resolver := &fakeRetrievalBindingResolver{binding: retrievalBindingFixture(func() domain.ConversationKey { key, _ := botInvocation().ConversationKey(); return key }())}
	service := newTestService(t, store, runtime, &fakeHistory{}, publisher, func(cfg *Config) {
		cfg.KnowledgeRetrievalLimits = knowledgeRetrievalLimits()
	})
	service.knowledgeRetriever = retriever
	service.retrievalBindings = resolver

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var outcome Outcome
	var handleErr error
	go func() {
		defer close(done)
		outcome, handleErr = service.Handle(ctx, botInvocation())
	}()
	<-started
	cancel()
	close(release)
	<-done
	if handleErr != nil || outcome != OutcomeResponded {
		t.Fatalf("outcome = %q, err = %v", outcome, handleErr)
	}
	if runtime.runCalls != 1 || len(runtime.runRequest.Knowledge) != 0 {
		t.Fatalf("late cards were admitted: %#v", runtime.runRequest.Knowledge)
	}
}
