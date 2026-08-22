package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
	"github.com/Dauno/slack-local-agent/internal/testutil"
	"github.com/Dauno/slack-local-agent/internal/usecase/doctor"
	knowledgeusecase "github.com/Dauno/slack-local-agent/internal/usecase/knowledge"
)

func TestLexicalRetrievalEnabledGate(t *testing.T) {
	cfg := config.Default()
	if lexicalRetrievalEnabled(cfg, domain.ProviderFamilyOpenAICompatible) {
		t.Fatal("gate enabled with default-off retrieval")
	}
	cfg.Orchestration.Knowledge.Enabled = true
	if lexicalRetrievalEnabled(cfg, domain.ProviderFamilyOpenAICompatible) {
		t.Fatal("gate enabled without retrieval")
	}
	cfg.Orchestration.Knowledge.Retrieval.Enabled = true
	if !lexicalRetrievalEnabled(cfg, domain.ProviderFamilyOpenAICompatible) {
		t.Fatal("gate disabled for a durable openai_compatible root")
	}
	if lexicalRetrievalEnabled(cfg, domain.ProviderFamilyAgentCLI) {
		t.Fatal("gate enabled for an agent_cli root")
	}
	cfg.Orchestration.Knowledge.Enabled = false
	if lexicalRetrievalEnabled(cfg, domain.ProviderFamilyOpenAICompatible) {
		t.Fatal("gate enabled with knowledge disabled")
	}
}

func TestComposeLexicalRetrievalDisabledCreatesNothing(t *testing.T) {
	cfg := config.Default()
	composition, err := composeLexicalRetrieval(cfg, runtimeModels{}, nil, nil)
	if err != nil || composition != nil {
		t.Fatalf("composeLexicalRetrieval(disabled) = %v, %v, want nothing", composition, err)
	}
}

func TestComposeLexicalRetrievalRejectsNonOpenAICompatibleRoot(t *testing.T) {
	cfg := config.Default()
	cfg.Orchestration.Knowledge.Enabled = true
	cfg.Orchestration.Knowledge.Retrieval.Enabled = true
	models := runtimeModels{rootFamily: domain.ProviderFamilyAgentCLI}
	if _, err := composeLexicalRetrieval(cfg, models, nil, nil); err == nil {
		t.Fatal("composeLexicalRetrieval(agent_cli root) succeeded")
	}
}

func TestComposeLexicalRetrievalRejectsMissingModelBudgetGate(t *testing.T) {
	cfg := config.Default()
	cfg.Orchestration.Knowledge.Enabled = true
	cfg.Orchestration.Knowledge.Retrieval.Enabled = true
	models := runtimeModels{rootFamily: domain.ProviderFamilyOpenAICompatible}
	if _, err := composeLexicalRetrieval(cfg, models, nil, nil); err == nil {
		t.Fatal("composeLexicalRetrieval without model-budget gate succeeded")
	}
}

func TestComposeLexicalRetrievalRejectsUnconfiguredAdapters(t *testing.T) {
	cfg := config.Default()
	cfg.Orchestration.Knowledge.Enabled = true
	cfg.Orchestration.Knowledge.Retrieval.Enabled = true
	features := config.ContextFeaturesConfig{ModelBudgetEnabled: true}
	cfg.Context.ContextFeatures = &features
	models := runtimeModels{rootFamily: domain.ProviderFamilyOpenAICompatible}
	if _, err := composeLexicalRetrieval(cfg, models, nil, nil); err == nil {
		t.Fatal("composeLexicalRetrieval without a store succeeded")
	}
}

func embeddingEnabledTestConfig() config.Config {
	cfg := config.Default()
	cfg.Orchestration.Knowledge.Enabled = true
	cfg.Orchestration.Knowledge.Retrieval.Enabled = true
	cfg.Orchestration.Knowledge.Retrieval.Embedding.Enabled = true
	cfg.Orchestration.Knowledge.Retrieval.Embedding.ProviderID = "acme"
	cfg.Orchestration.Knowledge.Retrieval.Embedding.BaseURL = "http://127.0.0.1:9"
	cfg.Orchestration.Knowledge.Retrieval.Embedding.APIKeyEnv = "EMBEDDING_API_KEY"
	cfg.Orchestration.Knowledge.Retrieval.Embedding.Model = "embed-3"
	cfg.Orchestration.Knowledge.Retrieval.Embedding.Dimensions = 4
	cfg.Orchestration.Knowledge.Retrieval.Embedding.MinSimilarityBasisPoints = 5000
	cfg.Orchestration.Knowledge.Retrieval.Embedding.TimeoutSeconds = 5
	features := config.ContextFeaturesConfig{ModelBudgetEnabled: true, RecoverableResultsEnabled: true}
	cfg.Context.ContextFeatures = &features
	return cfg
}

func embeddingEnabledTestModels() runtimeModels {
	return runtimeModels{
		rootFamily:      domain.ProviderFamilyOpenAICompatible,
		embeddingAPIKey: "sk-embedding-secret-value",
		redactor:        secure.NewRedactor("sk-embedding-secret-value"),
		logger:          logging.New(io.Discard, "error", secure.NewRedactor("sk-embedding-secret-value")),
		metrics:         nil,
	}
}

// embeddingEnabledTestLimiter returns a real single-slot limiter so the
// enabled path exercises the shared budget without any model call.
func embeddingEnabledTestLimiter() *modelcalllimiter.Limiter {
	return modelcalllimiter.New(1)
}

func openEmbeddingTestStore(t *testing.T) *adaptersqlite.Store {
	t.Helper()
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "embedding-composition.db"))
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestComposeLexicalRetrievalEmbeddingDisabledBuildsNoEmbeddingPieces(t *testing.T) {
	// The lexical-only configuration: embeddings remain default-off with
	// zero dimensions and threshold, so the composed retriever's semantic
	// channel stays inert and no embedding provider or worker exists.
	cfg := config.Default()
	cfg.Orchestration.Knowledge.Enabled = true
	cfg.Orchestration.Knowledge.Retrieval.Enabled = true
	features := config.ContextFeaturesConfig{ModelBudgetEnabled: true}
	cfg.Context.ContextFeatures = &features
	models := embeddingEnabledTestModels()
	store := openEmbeddingTestStore(t)
	composition, err := composeLexicalRetrieval(cfg, models, nil, store)
	if err != nil {
		t.Fatalf("composeLexicalRetrieval(embedding disabled) error = %v", err)
	}
	if composition == nil || composition.lexicalWorker == nil || composition.retriever == nil {
		t.Fatalf("lexical-only composition = %+v, want the lexical worker and retriever", composition)
	}
	if composition.embeddingWorker != nil {
		t.Fatal("embedding-disabled composition constructed an embedding worker")
	}
	if composition.limits.EmbeddingDimensions != 0 || composition.limits.MinSimilarityBasisPoints != 0 {
		t.Fatalf("embedding-disabled limits = %+v, want zero embedding dimensions and threshold", composition.limits)
	}
}

func TestComposeLexicalRetrievalEmbeddingEnabledConstructsEverything(t *testing.T) {
	cfg := embeddingEnabledTestConfig()
	wantFingerprint := domain.ModelFingerprint("acme", "embed-3", 4)
	if wantFingerprint == "" {
		t.Fatal("ModelFingerprint() returned empty")
	}

	// A minimal OpenAI-compatible embeddings endpoint returns one fixed
	// 4-dimension vector for every input, so the semantic channel can run
	// end to end without a real provider.
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/embeddings" {
			http.Error(writer, "not found", http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"object": "list",
			"data": []any{
				map[string]any{"object": "embedding", "index": 0, "embedding": []float64{1, 0, 0, 0}},
			},
			"model": "provider-model",
			"usage": map[string]any{"prompt_tokens": 2, "total_tokens": 2},
		})
	}))
	t.Cleanup(server.Close)
	cfg.Orchestration.Knowledge.Retrieval.Embedding.BaseURL = server.URL

	models := embeddingEnabledTestModels()
	store := openEmbeddingTestStore(t)
	composition, err := composeLexicalRetrieval(cfg, models, embeddingEnabledTestLimiter(), store)
	if err != nil {
		t.Fatalf("composeLexicalRetrieval(embedding enabled) error = %v", err)
	}
	if composition == nil || composition.lexicalWorker == nil || composition.retriever == nil {
		t.Fatalf("composition = %+v, want the lexical worker and retriever", composition)
	}
	if composition.embeddingWorker == nil {
		t.Fatal("embedding-enabled composition constructed no embedding worker")
	}
	if composition.limits.EmbeddingDimensions != 4 || composition.limits.MinSimilarityBasisPoints != 5000 {
		t.Fatalf("embedding-enabled limits = %+v, want dimensions 4 and threshold 5000", composition.limits)
	}

	// End-to-end proof that the composed retrieval surface is bound to the
	// fingerprint composeLexicalRetrieval computes: seed one claim plus a
	// matching vector row under exactly wantFingerprint, ask an unrelated
	// query (so exact/relation/lexical stay empty), and assert the claim
	// comes back through the semantic channel only.
	identity := "kclaim_00000000000000000000e2ee"
	subject, valueText := "semantic subject one", "value one"
	now := time.Now().UTC().UnixNano()
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, value_number,
			value_boolean, value_reference, scope_kind, scope_id, source_class, source_ref, author_id,
			status, valid_from, valid_until, supersedes_id, current_rev, created_at, updated_at)
		VALUES (?, ?, 'is', 'string', ?, 0, 0, '', 'project', 'my-project', 'human', ?, '', 'asserted', 0, 0, '', 1, ?, ?)`,
		identity, subject, valueText, "src:"+identity, now, now); err != nil {
		t.Fatalf("seed claim %s: %v", identity, err)
	}
	item := port.KnowledgeAuthoritativeItem{
		Kind: domain.KnowledgeRetrievalClaim, ID: identity,
		Claim: &domain.KnowledgeClaim{
			ID: domain.KnowledgeClaimID(identity), Subject: subject, Predicate: domain.KnowledgePredicateIs,
			Value:       domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: valueText},
			ScopeKind:   domain.KnowledgeScopeGlobal,
			SourceClass: domain.KnowledgeSourceRoot, SourceRef: "src:" + identity,
			Status: domain.KnowledgeClaimAsserted, Revision: 1,
		},
	}
	text, err := knowledgeusecase.BuildKnowledgeIndexText(domain.KnowledgeRetrievalClaim, item, "", nil)
	if err != nil {
		t.Fatalf("BuildKnowledgeIndexText() error = %v", err)
	}
	vectorBytes := make([]byte, 16)
	binary.LittleEndian.PutUint32(vectorBytes[0:], math.Float32bits(1))
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES ('claim', ?, 1, ?, ?, 4, ?, 100)`,
		identity, text.SourceDigest, wantFingerprint, vectorBytes); err != nil {
		t.Fatalf("seed vector row %s: %v", identity, err)
	}

	result, err := composition.retriever.Retrieve(t.Context(), domain.KnowledgeRetrievalRequest{
		Binding: domain.KnowledgeWriteBinding{
			Team: "T00000001", Actor: "U00000001",
			Conversation: domain.ConversationKey("slack:T00000001:dm:C00000001"),
			Project:      "my-project", WorkstreamID: "ws-e2e",
		},
		Workstream: &domain.WorkstreamSnapshot{
			ID: "ws-e2e", Project: "my-project", OwnerActor: "U00000001",
			ConversationKey: domain.ConversationKey("slack:T00000001:dm:C00000001"),
			Status:          domain.WorkstreamActive,
		},
		ExchangeTS:     "1700000000.000000",
		CurrentMessage: "totally unrelated query text",
		Now:            time.Now().UTC(),
		Limits:         composition.limits,
	})
	if err != nil {
		t.Fatalf("Retrieve() error = %v", err)
	}
	if len(result.Cards) != 1 || result.Cards[0].Identity() != "claim:"+identity {
		t.Fatalf("Retrieve() cards = %v failures = %v omitted = %d selected = %v, want the seeded claim through the semantic channel", result.Cards, result.Diagnostics.Failures, result.Diagnostics.OmittedCount, result.Diagnostics.SelectedIdentities)
	}
	if result.Cards[0].Claim.RetrievalReason != string(domain.KnowledgeRetrievalReasonSemantic) {
		t.Fatalf("card reason = %q, want semantic (exact/lexical must stay empty)", result.Cards[0].Claim.RetrievalReason)
	}
	if len(result.Diagnostics.Failures) != 0 {
		t.Fatalf("diagnostics failures = %v, want none", result.Diagnostics.Failures)
	}
}

func TestComposeLexicalRetrievalEmbeddingEnabledRequiresResolvedKeyAndLimiter(t *testing.T) {
	cfg := embeddingEnabledTestConfig()
	store := openEmbeddingTestStore(t)
	noKey := embeddingEnabledTestModels()
	noKey.embeddingAPIKey = ""
	if _, err := composeLexicalRetrieval(cfg, noKey, embeddingEnabledTestLimiter(), store); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("composeLexicalRetrieval(missing embedding key) error = %v, want an API key failure", err)
	}
	withKey := embeddingEnabledTestModels()
	if _, err := composeLexicalRetrieval(cfg, withKey, nil, store); err == nil || !strings.Contains(err.Error(), "limiter") {
		t.Fatalf("composeLexicalRetrieval(missing limiter) error = %v, want a limiter failure", err)
	}
}

func TestComposeLexicalRetrievalEmbeddingErrorsNeverLeakTheAPIKey(t *testing.T) {
	cfg := embeddingEnabledTestConfig()
	// An out-of-range dimension forces the provider constructor to fail;
	// the composed error must never carry the resolved API key.
	cfg.Orchestration.Knowledge.Retrieval.Embedding.Dimensions = 0
	models := embeddingEnabledTestModels()
	store := openEmbeddingTestStore(t)
	_, err := composeLexicalRetrieval(cfg, models, embeddingEnabledTestLimiter(), store)
	if err == nil {
		t.Fatal("composeLexicalRetrieval(invalid embedding dimensions) succeeded")
	}
	if strings.Contains(err.Error(), models.embeddingAPIKey) {
		t.Fatalf("composition error leaked the embedding API key: %q", err.Error())
	}
}

func TestRuntimeCompositionWaitEmbeddingNilWorkerIsNoOp(t *testing.T) {
	if err := (&runtimeComposition{}).WaitEmbedding(t.Context()); err != nil {
		t.Fatalf("WaitEmbedding(no worker) error = %v", err)
	}
	var nilComposition *runtimeComposition
	if err := nilComposition.WaitEmbedding(t.Context()); err != nil {
		t.Fatalf("WaitEmbedding(nil composition) error = %v", err)
	}
}

// lexicalOnlyKnowledgeTestConfig enables knowledge + retrieval with the
// model-budget safety gate and keeps embeddings default-off.
func lexicalOnlyKnowledgeTestConfig() config.Config {
	cfg := config.Default()
	cfg.Orchestration.Knowledge.Enabled = true
	cfg.Orchestration.Knowledge.Retrieval.Enabled = true
	features := config.ContextFeaturesConfig{ModelBudgetEnabled: true}
	cfg.Context.ContextFeatures = &features
	return cfg
}

// seedKnowledgeCompositionClaim inserts one authorized claim in the shape
// the real workers and the semantic SQL both accept: project scope, human
// source, asserted status, open validity, revision 1.
func seedKnowledgeCompositionClaim(t *testing.T, store *adaptersqlite.Store, identity, subject, valueText string) {
	t.Helper()
	now := time.Now().UTC().UnixNano()
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, value_number,
			value_boolean, value_reference, scope_kind, scope_id, source_class, source_ref, author_id,
			status, valid_from, valid_until, supersedes_id, current_rev, created_at, updated_at)
		VALUES (?, ?, 'is', 'string', ?, 0, 0, '', 'project', 'my-project', 'human', ?, '', 'asserted', 0, 0, '', 1, ?, ?)`,
		identity, subject, valueText, "src:"+identity, now, now); err != nil {
		t.Fatalf("seed claim %s: %v", identity, err)
	}
}

// knowledgeQueueRow reads the generation and status of one queue row. The
// table argument is one of the two fixed queue table names, never user
// input.
func knowledgeQueueRow(t *testing.T, store *adaptersqlite.Store, table, kind, id string) (generation int, status string) {
	t.Helper()
	if err := store.DB().QueryRowContext(t.Context(),
		"SELECT generation, status FROM "+table+" WHERE item_kind = ? AND item_id = ?", kind, id).
		Scan(&generation, &status); err != nil {
		t.Fatalf("queue row %s %s: %v", table, id, err)
	}
	return generation, status
}

func knowledgeFTSCount(t *testing.T, store *adaptersqlite.Store, kind, id string) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM knowledge_retrieval_fts WHERE item_kind = ? AND item_id = ?", kind, id).Scan(&count); err != nil {
		t.Fatalf("FTS count for %s: %v", id, err)
	}
	return count
}

func knowledgeEmbeddingCount(t *testing.T, store *adaptersqlite.Store, kind, id, fingerprint string) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM knowledge_embeddings WHERE item_kind = ? AND item_id = ? AND model_fingerprint = ?",
		kind, id, fingerprint).Scan(&count); err != nil {
		t.Fatalf("embedding count for %s: %v", id, err)
	}
	return count
}

func waitForKnowledgeCondition(t *testing.T, message string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", message)
}

// knowledgeRestartBinding is the trusted retrieval binding shared by every
// phase of the restart sequence.
func knowledgeRestartBinding() domain.KnowledgeWriteBinding {
	return domain.KnowledgeWriteBinding{
		Team: "T00000001", Actor: "U00000001",
		Conversation: domain.ConversationKey("slack:T00000001:dm:C00000001"),
		Project:      "my-project", WorkstreamID: "ws-restart",
	}
}

// TestKnowledgeRetrievalRestartRollbackSequence proves the checkpoint-6
// acceptance criteria at the composition/persistence level: one SQLite
// store survives six simulated process restarts (enabled → disabled →
// re-enabled → embeddings enabled with a fake-provider drain → rolled back
// to disabled → embeddings re-enabled), and every transition leaves the
// reconstructible state bounded and intact.
func TestKnowledgeRetrievalRestartRollbackSequence(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	claim1 := "kclaim_0000000000000000000000aa"
	claim2 := "kclaim_0000000000000000000000bb"
	models := embeddingEnabledTestModels()

	// Phase A: retrieval enabled, embeddings off. Drain the lexical queue
	// for one seeded claim with the real composed worker, then shut down.
	storeA, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seedKnowledgeCompositionClaim(t, storeA, claim1, "restart subject one", "value one")
	if _, err := adaptersqlite.NewKnowledgeLexicalQueueStore(storeA).Enqueue(ctx, domain.KnowledgeRetrievalClaim, claim1); err != nil {
		t.Fatalf("enqueue claim one: %v", err)
	}
	compositionA, err := composeLexicalRetrieval(lexicalOnlyKnowledgeTestConfig(), models, nil, storeA)
	if err != nil || compositionA == nil {
		t.Fatalf("phase A compose = %v, %v", compositionA, err)
	}
	if compositionA.embeddingWorker != nil {
		t.Fatal("phase A composed an embedding worker with embeddings disabled")
	}
	workerCtxA, cancelA := context.WithCancel(ctx)
	go compositionA.lexicalWorker.Run(workerCtxA)
	waitForKnowledgeCondition(t, "phase A lexical drain", func() bool {
		return knowledgeFTSCount(t, storeA, string(domain.KnowledgeRetrievalClaim), claim1) == 1
	})
	waitForKnowledgeCondition(t, "phase A queue completion", func() bool {
		_, status := knowledgeQueueRow(t, storeA, "knowledge_lexical_queue", string(domain.KnowledgeRetrievalClaim), claim1)
		return status == "done"
	})
	generationA, _ := knowledgeQueueRow(t, storeA, "knowledge_lexical_queue", string(domain.KnowledgeRetrievalClaim), claim1)
	if generationA < 2 {
		t.Fatalf("phase A queue generation = %d, want the startup reconcile bump above the initial enqueue", generationA)
	}
	cancelA()
	if err := compositionA.lexicalWorker.WaitStopped(ctx); err != nil {
		t.Fatalf("phase A WaitStopped error = %v", err)
	}
	if err := storeA.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase B: restart with retrieval fully disabled against a store that
	// already carries index rows and queue state. Composition returns
	// nothing, no worker exists to run, and the persisted state is
	// untouched by the disabled boot.
	storeB, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	generationB1, statusB1 := knowledgeQueueRow(t, storeB, "knowledge_lexical_queue", string(domain.KnowledgeRetrievalClaim), claim1)
	if statusB1 != "done" || generationB1 != generationA {
		t.Fatalf("phase B pre-boot queue state = gen %d status %s, want gen %d done", generationB1, statusB1, generationA)
	}
	if count := knowledgeFTSCount(t, storeB, string(domain.KnowledgeRetrievalClaim), claim1); count != 1 {
		t.Fatalf("phase B pre-boot FTS count = %d, want 1", count)
	}
	disabledCfg := lexicalOnlyKnowledgeTestConfig()
	disabledCfg.Orchestration.Knowledge.Retrieval.Enabled = false
	compositionB, err := composeLexicalRetrieval(disabledCfg, models, nil, storeB)
	if err != nil || compositionB != nil {
		t.Fatalf("phase B compose(disabled) = %v, %v, want nil, nil", compositionB, err)
	}
	generationB2, statusB2 := knowledgeQueueRow(t, storeB, "knowledge_lexical_queue", string(domain.KnowledgeRetrievalClaim), claim1)
	if statusB2 != "done" || generationB2 != generationB1 {
		t.Fatalf("phase B boot mutated queue state: gen %d -> %d, status %s -> %s", generationB1, generationB2, statusB1, statusB2)
	}
	if count := knowledgeFTSCount(t, storeB, string(domain.KnowledgeRetrievalClaim), claim1); count != 1 {
		t.Fatalf("phase B boot mutated FTS rows: count = %d", count)
	}
	if err := storeB.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase C: re-enable retrieval. The reconstructible queue survives the
	// restart: startup reconcile re-enqueues the existing truth, the worker
	// drains a newly seeded claim without losing the first, and no
	// duplicate FTS rows are produced.
	storeC, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seedKnowledgeCompositionClaim(t, storeC, claim2, "restart subject two", "value two")
	if _, err := adaptersqlite.NewKnowledgeLexicalQueueStore(storeC).Enqueue(ctx, domain.KnowledgeRetrievalClaim, claim2); err != nil {
		t.Fatalf("enqueue claim two: %v", err)
	}
	compositionC, err := composeLexicalRetrieval(lexicalOnlyKnowledgeTestConfig(), models, nil, storeC)
	if err != nil || compositionC == nil || compositionC.lexicalWorker == nil {
		t.Fatalf("phase C compose = %v, %v", compositionC, err)
	}
	workerCtxC, cancelC := context.WithCancel(ctx)
	go compositionC.lexicalWorker.Run(workerCtxC)
	waitForKnowledgeCondition(t, "phase C claim two drain", func() bool {
		return knowledgeFTSCount(t, storeC, string(domain.KnowledgeRetrievalClaim), claim2) == 1
	})
	waitForKnowledgeCondition(t, "phase C claim one re-reconcile", func() bool {
		generation, status := knowledgeQueueRow(t, storeC, "knowledge_lexical_queue", string(domain.KnowledgeRetrievalClaim), claim1)
		return status == "done" && generation > generationA
	})
	if count := knowledgeFTSCount(t, storeC, string(domain.KnowledgeRetrievalClaim), claim1); count != 1 {
		t.Fatalf("phase C duplicated claim one FTS rows: count = %d", count)
	}
	if count := knowledgeFTSCount(t, storeC, string(domain.KnowledgeRetrievalClaim), claim2); count != 1 {
		t.Fatalf("phase C claim two FTS count = %d, want 1", count)
	}
	cancelC()
	if err := compositionC.lexicalWorker.WaitStopped(ctx); err != nil {
		t.Fatalf("phase C WaitStopped error = %v", err)
	}
	if err := storeC.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase D: embeddings enabled with the same persisted store. The real
	// embedding worker (identical wiring to composeLexicalRetrieval but
	// with the deterministic fake provider — no live credential, no
	// network) drains the embedding queue and writes the vector row under
	// the composed fingerprint.
	storeD, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	embeddingCfg := embeddingEnabledTestConfig()
	compositionD, err := composeLexicalRetrieval(embeddingCfg, models, embeddingEnabledTestLimiter(), storeD)
	if err != nil || compositionD == nil {
		t.Fatalf("phase D compose = %v, %v", compositionD, err)
	}
	if compositionD.embeddingWorker == nil || compositionD.limits.EmbeddingDimensions != 4 {
		t.Fatalf("phase D composition = %+v, want an embedding worker and 4 dimensions", compositionD)
	}
	fingerprint := domain.ModelFingerprint(embeddingCfg.Orchestration.Knowledge.Retrieval.Embedding.ProviderID,
		embeddingCfg.Orchestration.Knowledge.Retrieval.Embedding.Model,
		embeddingCfg.Orchestration.Knowledge.Retrieval.Embedding.Dimensions)
	provider := testutil.NewFakeEmbeddingProvider(4)
	provider.SetVectors([][]float32{{1, 0, 0, 0}})
	embeddingQueue := adaptersqlite.NewKnowledgeEmbeddingQueueStore(storeD)
	if _, err := embeddingQueue.Enqueue(ctx, domain.KnowledgeRetrievalClaim, claim1); err != nil {
		t.Fatalf("enqueue embedding claim one: %v", err)
	}
	drainWorker, err := knowledgeusecase.NewEmbeddingWorker(knowledgeusecase.EmbeddingWorkerConfig{
		Interval:   time.Duration(embeddingCfg.Orchestration.Knowledge.Retrieval.WorkerIntervalSeconds) * time.Second,
		MaxRetries: embeddingCfg.Orchestration.Knowledge.Retrieval.WorkerMaxRetries,
		BatchSize:  1,
		Limits:     compositionD.limits,
		ProviderID: embeddingCfg.Orchestration.Knowledge.Retrieval.Embedding.ProviderID,
		Model:      embeddingCfg.Orchestration.Knowledge.Retrieval.Embedding.Model,
		Dimensions: embeddingCfg.Orchestration.Knowledge.Retrieval.Embedding.Dimensions,
	}, knowledgeusecase.EmbeddingWorkerDependencies{
		Queue:    embeddingQueue,
		Source:   adaptersqlite.NewKnowledgeIndexSourceStore(storeD),
		Index:    adaptersqlite.NewKnowledgeVectorIndexStore(storeD),
		Provider: provider,
		Resolver: adaptersqlite.NewKnowledgeDocumentResolver(storeD),
		Lister:   adaptersqlite.NewKnowledgeIndexSourceStore(storeD),
		Logger:   logging.New(io.Discard, "error", secure.NewRedactor("sk-embedding-secret-value")),
		Sanitize: func(value string) string { return value },
		Redact:   func(value string) string { return value },
	})
	if err != nil {
		t.Fatalf("phase D embedding worker: %v", err)
	}
	workerCtxD, cancelD := context.WithCancel(ctx)
	go drainWorker.Run(workerCtxD)
	waitForKnowledgeCondition(t, "phase D vector drain", func() bool {
		return knowledgeEmbeddingCount(t, storeD, string(domain.KnowledgeRetrievalClaim), claim1, fingerprint) == 1
	})
	waitForKnowledgeCondition(t, "phase D embedding queue completion", func() bool {
		_, status := knowledgeQueueRow(t, storeD, "knowledge_embedding_queue", string(domain.KnowledgeRetrievalClaim), claim1)
		return status == "done"
	})
	var vectorBefore []byte
	var digestBefore string
	if err := storeD.DB().QueryRowContext(ctx, `
		SELECT vector, source_digest FROM knowledge_embeddings WHERE item_kind = 'claim' AND item_id = ? AND model_fingerprint = ?`,
		claim1, fingerprint).Scan(&vectorBefore, &digestBefore); err != nil {
		t.Fatalf("read phase D vector row: %v", err)
	}
	if len(vectorBefore) != 16 {
		t.Fatalf("phase D vector bytes = %d, want 16 (4 float32 dimensions)", len(vectorBefore))
	}
	cancelD()
	if err := drainWorker.WaitStopped(ctx); err != nil {
		t.Fatalf("phase D WaitStopped error = %v", err)
	}
	if err := storeD.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase E: roll back to embeddings disabled. Composition builds the
	// lexical-only path with zero embedding dimensions, the surviving
	// vector row is left untouched (rollback is never destructive), and no
	// embedding call is possible or made.
	storeE, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rollbackCfg := embeddingEnabledTestConfig()
	rollbackCfg.Orchestration.Knowledge.Retrieval.Embedding.Enabled = false
	callsBefore := provider.CallCount()
	compositionE, err := composeLexicalRetrieval(rollbackCfg, models, embeddingEnabledTestLimiter(), storeE)
	if err != nil || compositionE == nil {
		t.Fatalf("phase E compose = %v, %v", compositionE, err)
	}
	if compositionE.embeddingWorker != nil {
		t.Fatal("phase E composed an embedding worker after rollback")
	}
	// The rollback flips only the enabled flag: the configured dimensions
	// are carried verbatim in the limits, but without a provider and with
	// an empty fingerprint the semantic channel is dead — proven
	// behaviorally below, not just by field inspection.
	phaseEResult, err := compositionE.retriever.Retrieve(ctx, domain.KnowledgeRetrievalRequest{
		Binding:        knowledgeRestartBinding(),
		Workstream:     &domain.WorkstreamSnapshot{ID: "ws-restart", Project: "my-project", OwnerActor: "U00000001", ConversationKey: domain.ConversationKey("slack:T00000001:dm:C00000001"), Status: domain.WorkstreamActive},
		ExchangeTS:     "1700000000.000000",
		CurrentMessage: "semantically related but lexically unrelated probe",
		Now:            time.Now().UTC(),
		Limits:         compositionE.limits,
	})
	if err != nil {
		t.Fatalf("phase E rollback Retrieve() error = %v", err)
	}
	for _, channel := range phaseEResult.Diagnostics.EnabledChannels {
		if channel == domain.KnowledgeRetrievalChannelSemantic {
			t.Fatal("phase E rollback retriever still enables the semantic channel")
		}
	}
	if len(phaseEResult.Cards) != 0 {
		t.Fatalf("phase E rollback returned %d cards, want none through the dead semantic channel", len(phaseEResult.Cards))
	}
	var vectorAfter []byte
	var digestAfter string
	if err := storeE.DB().QueryRowContext(ctx, `
		SELECT vector, source_digest FROM knowledge_embeddings WHERE item_kind = 'claim' AND item_id = ? AND model_fingerprint = ?`,
		claim1, fingerprint).Scan(&vectorAfter, &digestAfter); err != nil {
		t.Fatalf("phase E vector row was deleted by rollback: %v", err)
	}
	if !bytes.Equal(vectorBefore, vectorAfter) || digestBefore != digestAfter {
		t.Fatal("phase E rollback mutated the surviving vector row")
	}
	if provider.CallCount() != callsBefore {
		t.Fatalf("phase E made %d embedding calls, want none", provider.CallCount()-callsBefore)
	}
	if err := storeE.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase F: re-enable embeddings with the same fingerprint. The
	// surviving vector rows are immediately usable by the semantic search
	// surface without any rebuild.
	storeF, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	compositionF, err := composeLexicalRetrieval(embeddingEnabledTestConfig(), models, embeddingEnabledTestLimiter(), storeF)
	if err != nil || compositionF == nil || compositionF.embeddingWorker == nil {
		t.Fatalf("phase F compose = %v, %v", compositionF, err)
	}
	binding := knowledgeRestartBinding()
	indexStore := adaptersqlite.NewKnowledgeLexicalIndexStore(storeF, fingerprint)
	hits, err := indexStore.SearchSemantic(ctx, domain.KnowledgeReadableScopes(binding),
		domain.SlackOwnerKey(binding.Conversation, binding.Actor), []float32{1, 0, 0, 0}, 5000, 32)
	if err != nil {
		t.Fatalf("phase F SearchSemantic error = %v", err)
	}
	found := false
	for _, hit := range hits {
		if hit.Kind == domain.KnowledgeRetrievalClaim && hit.ID == claim1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("phase F semantic search over surviving vector rows missed claim one (hits %v)", hits)
	}
	if err := storeF.Close(); err != nil {
		t.Fatal(err)
	}
}

// doctorTestSecrets resolves the production-shaped secret set without any
// live credential: every key is a fixture value and the embedding key is a
// local placeholder that must never appear in the report.
type doctorTestSecrets struct{}

func (doctorTestSecrets) Resolve(keys ...string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		if key == "EMBEDDING_API_KEY" {
			values[key] = "sk-embedding-deployment-evidence-key"
			continue
		}
		values[key] = "fixture-value"
	}
	return values, nil
}

type doctorTestDatabase struct{}

func (doctorTestDatabase) CheckDatabase(context.Context, string) error { return nil }

// doctorStoreKnowledgeChecker adapts the real store health check to the
// doctor KnowledgeChecker contract over the database path.
type doctorStoreKnowledgeChecker struct {
	dbPath string
}

func (c doctorStoreKnowledgeChecker) CheckKnowledgeRetrievalState(ctx context.Context, path string) (domain.KnowledgeRetrievalHealth, error) {
	store, err := adaptersqlite.OpenExisting(ctx, path)
	if err != nil {
		return domain.KnowledgeRetrievalHealth{}, err
	}
	defer func() { _ = store.Close() }()
	return store.CheckKnowledgeRetrievalState(ctx)
}

func findKnowledgeDoctorResult(report doctor.Report, name string) (doctor.Result, bool) {
	for _, result := range report.Results {
		if result.Name == name {
			return result, true
		}
	}
	return doctor.Result{}, false
}

// TestKnowledgeRetrievalDoctorDeploymentEvidence is the deployment-evidence
// artifact: a production-shaped runtime (retrieval + embeddings enabled,
// resolved key, model-budget gate, real store) boots through composition,
// and doctor's offline structural pass reports exactly the state an
// operator checks before flipping the feature gate — embedding key
// present (masked), reconstructible queues counted — with no live check and
// no credential anywhere in the output.
func TestKnowledgeRetrievalDoctorDeploymentEvidence(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, ".local-agent")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(stateDir, "local-agent.db")
	store, err := adaptersqlite.Initialize(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := embeddingEnabledTestConfig()
	composition, err := composeLexicalRetrieval(cfg, embeddingEnabledTestModels(), embeddingEnabledTestLimiter(), store)
	if err != nil || composition == nil || composition.embeddingWorker == nil {
		t.Fatalf("production-shaped compose = %v, %v", composition, err)
	}
	// One pending embedding queue row: the exact reconstructible state the
	// composition operates on must be what doctor reports.
	if _, err := adaptersqlite.NewKnowledgeEmbeddingQueueStore(store).Enqueue(ctx, domain.KnowledgeRetrievalClaim, "kclaim_0000000000000000000000cc"); err != nil {
		t.Fatalf("enqueue deployment evidence row: %v", err)
	}

	deps := doctor.Dependencies{
		ConfigPath:    filepath.Join(stateDir, "config.yaml"),
		LoadConfig:    func(string) (config.Config, error) { return cfg, nil },
		Secrets:       doctorTestSecrets{},
		Database:      doctorTestDatabase{},
		Knowledge:     doctorStoreKnowledgeChecker{dbPath: dbPath},
		SQLiteRuntime: sqliteRuntimeChecker{},
	}
	service, err := doctor.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(ctx, false)

	keyResult, ok := findKnowledgeDoctorResult(report, "knowledge embedding API key")
	if !ok || keyResult.Status != doctor.StatusPass || !strings.Contains(keyResult.Detail, "configured") {
		t.Fatalf("doctor embedding key result = %+v, want a masked configured pass", keyResult)
	}
	stateResult, ok := findKnowledgeDoctorResult(report, "knowledge retrieval state")
	if !ok || stateResult.Status != doctor.StatusPass || !strings.Contains(stateResult.Detail, "embedding queue pending=1") {
		t.Fatalf("doctor retrieval state result = %+v, want a pass reporting the pending embedding queue row", stateResult)
	}
	if _, ok := findKnowledgeDoctorResult(report, "knowledge embedding endpoint"); ok {
		t.Fatal("doctor ran the live embedding endpoint check with includeLive=false")
	}
	for _, result := range report.Results {
		combined := result.Name + " " + result.Detail + " " + result.Remediation
		if strings.Contains(combined, "sk-embedding-deployment-evidence-key") {
			t.Fatalf("doctor output leaked the embedding key in result %q: %s", result.Name, combined)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeQueueRepairWakesOnlyAfterCommittedEnqueue(t *testing.T) {
	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "queue-wake.db"))
	if err != nil {
		t.Fatal(err)
	}
	wakes := 0
	queue := wakingKnowledgeQueue{
		KnowledgeQueueStore: adaptersqlite.NewKnowledgeLexicalQueueStore(store),
		wake:                func() { wakes++ },
	}
	if _, err := queue.Enqueue(t.Context(), domain.KnowledgeRetrievalClaim, "claim-wake"); err != nil {
		t.Fatal(err)
	}
	if wakes != 1 {
		t.Fatalf("queue wake count = %d, want 1", wakes)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(t.Context(), domain.KnowledgeRetrievalClaim, "claim-failed"); err == nil {
		t.Fatal("Enqueue() after store close succeeded")
	}
	if wakes != 1 {
		t.Fatalf("failed enqueue wake count = %d, want 1", wakes)
	}
}
