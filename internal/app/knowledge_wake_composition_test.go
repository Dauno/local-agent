package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	"github.com/Dauno/slack-local-agent/internal/adapter/memoryprojector"
	metricsadapter "github.com/Dauno/slack-local-agent/internal/adapter/metrics"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/secure"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
	knowledgeusecase "github.com/Dauno/slack-local-agent/internal/usecase/knowledge"
)

// newWakeCompositionEmbeddingServer answers every request with a generic
// OpenAI-compatible embeddings response sized to the request's input count,
// so composeLexicalRetrieval's real embedding provider can complete a tick
// without a live embeddings endpoint.
func newWakeCompositionEmbeddingServer(t *testing.T, dimensions int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		data := make([]map[string]any, len(body.Input))
		for index := range body.Input {
			vector := make([]float64, dimensions)
			for value := range vector {
				vector[value] = float64(index+1) + float64(value)*0.5
			}
			data[index] = map[string]any{"object": "embedding", "index": index, "embedding": vector}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": "test-embedding-model", "usage": map[string]any{"prompt_tokens": 1, "total_tokens": 1}})
	}))
	t.Cleanup(server.Close)
	return server
}

// TestKnowledgeWakeCompositionFiresAllThreeSchedulers is FIND-122's repair
// for the knowledge wake class: it drives newKnowledgeWakeSchedules, the
// exact production function composeRuntime calls to build the projection,
// lexical, and embedding schedules and the wake list handed to
// composeKnowledgeService, instead of a hand-built stand-in for that list.
// All three consumers are enabled, matching composeRuntime's full gate. A
// regression that drops any one of the three wake appends inside
// newKnowledgeWakeSchedules (internal/app/knowledge_retrieval.go) makes
// this test hang until the go test binary's own -timeout, because that
// scheduler would then advance only through its own recovery timer, which
// this test never fires.
func TestKnowledgeWakeCompositionFiresAllThreeSchedulers(t *testing.T) {
	embeddingServer := newWakeCompositionEmbeddingServer(t, 3)

	cfg := config.Default()
	cfg.Orchestration.Knowledge.Enabled = true
	cfg.Orchestration.Knowledge.Retrieval.Enabled = true
	cfg.Orchestration.Knowledge.Retrieval.Embedding.Enabled = true
	cfg.Orchestration.Knowledge.Retrieval.Embedding.Dimensions = 3
	cfg.Orchestration.Knowledge.Retrieval.Embedding.Model = "test-embedding-model"
	cfg.Orchestration.Knowledge.Retrieval.Embedding.ProviderID = "test-provider"
	cfg.Orchestration.Knowledge.Retrieval.Embedding.BaseURL = embeddingServer.URL
	cfg.Context.ContextFeatures.ModelBudgetEnabled = true

	store, err := adaptersqlite.Initialize(t.Context(), filepath.Join(t.TempDir(), "knowledge-wake.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	logger := logging.New(io.Discard, "error", secure.NewRedactor())
	models := newRuntimeModels()
	models.redactor = secure.NewRedactor()
	models.logger = logger
	models.metrics = metricsadapter.NewRecorder()
	models.embeddingAPIKey = "sk-test-embedding-key"

	projectionScheduler, projectionTimers := newWakeCompositionScheduler(t)
	lexicalScheduler, lexicalTimers := newWakeCompositionScheduler(t)
	embeddingScheduler, embeddingTimers := newWakeCompositionScheduler(t)

	// newKnowledgeWakeSchedules is the production function under test: it
	// builds the wake list, not this test.
	schedules, wakes, err := newKnowledgeWakeSchedules(cfg, knowledgeWakeSchedules{
		projection: projectionScheduler,
		retrieval:  knowledgeRetrievalSchedules{lexical: lexicalScheduler, embedding: embeddingScheduler},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wakes) != 3 {
		t.Fatalf("newKnowledgeWakeSchedules() wakes = %d, want 3 (projection, lexical, embedding)", len(wakes))
	}

	retrievalComposition, err := composeLexicalRetrieval(cfg, models, modelcalllimiter.New(1), store, schedules.retrieval)
	if err != nil || retrievalComposition == nil || retrievalComposition.lexicalWorker == nil || retrievalComposition.embeddingWorker == nil {
		t.Fatalf("composeLexicalRetrieval() = %v, %v", retrievalComposition, err)
	}

	knowledgeStore := adaptersqlite.NewKnowledgeStore(store)
	coordinator := botusecase.NewLimiter(cfg.Runtime.MaxConcurrentModelCalls)
	knowledgeService, err := composeKnowledgeService(true, knowledgeStore, coordinator, wakes...)
	if err != nil {
		t.Fatal(err)
	}

	// composeRuntime constructs the projection worker inline over
	// schedules.projection (composition.go); this mirrors that exact
	// wiring, since there is no separate compose function for it.
	projectionWorker, err := knowledgeusecase.NewProjectionWorker(knowledgeusecase.ProjectionWorkerConfig{
		Interval: time.Duration(cfg.Orchestration.Knowledge.ProjectionIntervalSeconds) * time.Second, MaxRetries: cfg.Orchestration.Knowledge.ProjectionMaxRetries,
		RetentionDays: cfg.Orchestration.Knowledge.ProjectionRetentionDays, OutputDir: t.TempDir(),
	}, knowledgeusecase.ProjectionWorkerDependencies{
		Store: knowledgeStore, Reader: store, Projector: memoryprojector.New(),
		Logger: logger, Sanitize: models.redactor.String, Enabled: knowledgeService.Enabled, Scheduler: schedules.projection,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go projectionWorker.Run(ctx)
	go retrievalComposition.lexicalWorker.Run(ctx)
	go retrievalComposition.embeddingWorker.Run(ctx)
	waitCompositionPoll(t, projectionTimers) // initial polls: nothing pending.
	waitCompositionPoll(t, lexicalTimers)
	waitCompositionPoll(t, embeddingTimers)

	binding := domain.KnowledgeWriteBinding{Team: "T00000001", Actor: "U00000001", Conversation: domain.ConversationKey("slack:T00000001:channel:C00000001:thread:1234567890.123456")}
	command := knowledgeusecase.HumanCommandPrefix + `{"action":"remember","subject":"wake-composition","predicate":"is","value_kind":"string","value_text":"pg-01"}`
	if _, _, err := knowledgeService.Execute(t.Context(), binding, "evt-wake-composition", command); err != nil {
		t.Fatal(err)
	}
	// The single committed mutation must wake all three distinct scheduler
	// instances through newKnowledgeWakeSchedules's own wake list, never
	// through any scheduler's own recovery timer.
	waitCompositionPoll(t, projectionTimers)
	waitCompositionPoll(t, lexicalTimers)
	waitCompositionPoll(t, embeddingTimers)
}
