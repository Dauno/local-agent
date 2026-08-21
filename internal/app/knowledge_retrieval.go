package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	"github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	knowledgeusecase "github.com/Dauno/slack-local-agent/internal/usecase/knowledge"
	"github.com/Dauno/slack-local-agent/internal/usecase/workpoll"
)

// lexicalRetrievalEnabled is the pure composition gate: retrieval workers
// exist only when knowledge is enabled, retrieval is enabled, and the root
// is a durable openai_compatible provider. The model-budget safety gate is
// enforced statically by config validation and re-asserted during
// composition.
func lexicalRetrievalEnabled(cfg config.Config, rootFamily string) bool {
	return cfg.Orchestration.Knowledge.Enabled &&
		cfg.Orchestration.Knowledge.Retrieval.Enabled &&
		rootFamily == domain.ProviderFamilyOpenAICompatible
}

// knowledgeRetrievalLimits builds the validated retrieval limits from
// orchestration.knowledge for both the retriever and the bot. Embedding
// dimensions and the similarity threshold come from the embedding config;
// they stay zero when embeddings are disabled and fail the semantic
// channel closed through the retriever's normalization validation.
func knowledgeRetrievalLimits(cfg config.Config) (domain.KnowledgeRetrievalLimits, error) {
	retrieval := cfg.Orchestration.Knowledge.Retrieval
	limits := domain.KnowledgeRetrievalLimits{
		TimeoutSeconds:           retrieval.TimeoutSeconds,
		MaxQueryRunes:            retrieval.MaxQueryRunes,
		MaxCandidatesPerChannel:  retrieval.MaxCandidatesPerChannel,
		MaxCards:                 retrieval.MaxCards,
		MaxCardTokens:            cfg.Orchestration.Knowledge.MaxCardTokens,
		MaxDocumentBytes:         retrieval.MaxDocumentBytes,
		WorkerIntervalSeconds:    retrieval.WorkerIntervalSeconds,
		WorkerMaxRetries:         retrieval.WorkerMaxRetries,
		WorkerBatchSize:          retrieval.WorkerBatchSize,
		EmbeddingTimeoutSeconds:  retrieval.Embedding.TimeoutSeconds,
		EmbeddingDimensions:      retrieval.Embedding.Dimensions,
		MinSimilarityBasisPoints: retrieval.Embedding.MinSimilarityBasisPoints,
	}
	if err := limits.Validate(); err != nil {
		return domain.KnowledgeRetrievalLimits{}, fmt.Errorf("knowledge retrieval limits are invalid: %w", err)
	}
	return limits, nil
}

// knowledgeRetrievalComposition carries the composed retrieval pieces: the
// retriever, the validated limits, and the awaitable workers. All fields
// stay nil when the retrieval gate is off.
type knowledgeRetrievalComposition struct {
	lexicalWorker   *knowledgeusecase.LexicalWorker
	embeddingWorker *knowledgeusecase.EmbeddingWorker
	retriever       *knowledgeusecase.Retriever
	limits          domain.KnowledgeRetrievalLimits
}

type knowledgeRetrievalSchedules struct {
	lexical   *workpoll.Scheduler
	embedding *workpoll.Scheduler
}

type wakingKnowledgeQueue struct {
	port.KnowledgeQueueStore
	wake func()
}

// knowledgeWakeSchedules bundles the projection scheduler with the lexical
// and embedding retrieval schedules newKnowledgeWakeSchedules produces
// together, so a caller (production or test) drives every consumer over the
// exact same scheduler instances the wake list wakes.
type knowledgeWakeSchedules struct {
	projection *workpoll.Scheduler
	retrieval  knowledgeRetrievalSchedules
}

// newKnowledgeWakeSchedules is the single production source of which
// schedulers one committed knowledge mutation must wake. It builds the
// projection scheduler and, when retrieval is enabled, the lexical and
// (when embeddings are enabled) embedding schedulers, and returns the
// flattened wake list composeRuntime hands to composeKnowledgeService. A
// composition test calls this function directly instead of reproducing its
// wake list, so dropping an append here is caught by that test.
//
// supplied lets a caller (only tests use this) pre-build the schedulers
// with fake timers; composeRuntime always calls this with no argument, so
// production keeps building its own real-interval schedulers exactly as
// before this function existed.
func newKnowledgeWakeSchedules(cfg config.Config, supplied ...knowledgeWakeSchedules) (knowledgeWakeSchedules, []func(), error) {
	if !cfg.Orchestration.Knowledge.Enabled {
		return knowledgeWakeSchedules{}, nil, nil
	}
	var schedules knowledgeWakeSchedules
	if len(supplied) > 0 {
		schedules = supplied[0]
	}
	var wakes []func()
	var err error
	if schedules.projection == nil {
		schedules.projection, err = workpoll.New(time.Duration(cfg.Orchestration.Knowledge.ProjectionIntervalSeconds)*time.Second, workpoll.Options{})
		if err != nil {
			return knowledgeWakeSchedules{}, nil, fmt.Errorf("initialize knowledge projection scheduler: %w", err)
		}
	}
	wakes = append(wakes, schedules.projection.Wake)
	if cfg.Orchestration.Knowledge.Retrieval.Enabled {
		interval := time.Duration(cfg.Orchestration.Knowledge.Retrieval.WorkerIntervalSeconds) * time.Second
		if schedules.retrieval.lexical == nil {
			schedules.retrieval.lexical, err = workpoll.New(interval, workpoll.Options{})
			if err != nil {
				return knowledgeWakeSchedules{}, nil, fmt.Errorf("initialize knowledge lexical scheduler: %w", err)
			}
		}
		wakes = append(wakes, schedules.retrieval.lexical.Wake)
		if cfg.Orchestration.Knowledge.Retrieval.Embedding.Enabled {
			if schedules.retrieval.embedding == nil {
				schedules.retrieval.embedding, err = workpoll.New(interval, workpoll.Options{})
				if err != nil {
					return knowledgeWakeSchedules{}, nil, fmt.Errorf("initialize knowledge embedding scheduler: %w", err)
				}
			}
			wakes = append(wakes, schedules.retrieval.embedding.Wake)
		}
	}
	return schedules, wakes, nil
}

func (q wakingKnowledgeQueue) Enqueue(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, itemID string) (domain.KnowledgeQueueItem, error) {
	item, err := q.KnowledgeQueueStore.Enqueue(ctx, kind, itemID)
	if err == nil && q.wake != nil {
		q.wake()
	}
	return item, err
}

// composeLexicalRetrieval builds the retriever, the awaitable lexical
// worker, and — when embeddings are enabled — the OpenAI-compatible
// embedding provider, the embedding worker, and the fingerprint-bound
// semantic search surface. While retrieval is disabled nothing is created:
// workers never run, FTS and vectors are never touched, and queues never
// drain. Embedding-disabled operation stays byte-for-byte the lexical-only
// path: no provider, no embedding worker, an empty fingerprint keeps
// SearchSemantic fail-closed, and the retriever's Provider stays nil. The
// embedding API key was resolved inside prepareRuntimeModels and must be
// present when embeddings are enabled.
func composeLexicalRetrieval(cfg config.Config, models runtimeModels, modelCalls port.ModelCallLimiter, store *sqlite.Store, supplied ...knowledgeRetrievalSchedules) (*knowledgeRetrievalComposition, error) {
	retrieval := cfg.Orchestration.Knowledge.Retrieval
	if !cfg.Orchestration.Knowledge.Enabled || !retrieval.Enabled {
		return nil, nil
	}
	if models.rootFamily != domain.ProviderFamilyOpenAICompatible {
		return nil, errors.New("orchestration.knowledge.retrieval.enabled requires an openai_compatible root agent")
	}
	features := cfg.Context.ContextFeatures
	if features == nil || !features.ModelBudgetEnabled {
		return nil, errors.New("orchestration.knowledge.retrieval.enabled requires the model-budget safety gate")
	}
	limits, err := knowledgeRetrievalLimits(cfg)
	if err != nil {
		return nil, err
	}
	var schedules knowledgeRetrievalSchedules
	if len(supplied) > 0 {
		schedules = supplied[0]
	}
	interval := time.Duration(retrieval.WorkerIntervalSeconds) * time.Second
	if schedules.lexical == nil {
		schedules.lexical, _ = workpoll.New(interval, workpoll.Options{})
	}
	if retrieval.Embedding.Enabled && schedules.embedding == nil {
		schedules.embedding, _ = workpoll.New(interval, workpoll.Options{})
	}
	queueStore := wakingKnowledgeQueue{KnowledgeQueueStore: sqlite.NewKnowledgeLexicalQueueStore(store), wake: schedules.lexical.Wake}
	sourceStore := sqlite.NewKnowledgeIndexSourceStore(store)
	candidateReader := sqlite.NewKnowledgeCandidateReader(store)
	resolver := sqlite.NewKnowledgeDocumentResolver(store)
	if queueStore.KnowledgeQueueStore == nil || sourceStore == nil || candidateReader == nil || resolver == nil {
		return nil, errors.New("knowledge retrieval adapters are not configured")
	}
	embeddingEnabled := retrieval.Embedding.Enabled
	var (
		provider        port.EmbeddingProvider
		embeddingWorker *knowledgeusecase.EmbeddingWorker
		fingerprint     string
	)
	if embeddingEnabled {
		if models.embeddingAPIKey == "" {
			return nil, errors.New("orchestration.knowledge.retrieval.embedding.enabled requires a resolved embedding API key")
		}
		if modelCalls == nil {
			return nil, errors.New("orchestration.knowledge.retrieval.embedding.enabled requires the shared model-call limiter")
		}
		provider, err = openaillm.NewEmbeddingProvider(openaillm.EmbeddingProviderConfig{
			APIKey:     models.embeddingAPIKey,
			BaseURL:    retrieval.Embedding.BaseURL,
			Model:      retrieval.Embedding.Model,
			Dimensions: retrieval.Embedding.Dimensions,
			Timeout:    time.Duration(retrieval.Embedding.TimeoutSeconds) * time.Second,
			MaxBatch:   retrieval.WorkerBatchSize,
			Limiter:    modelCalls,
			Redact:     models.redactor.String,
		})
		if err != nil {
			return nil, fmt.Errorf("initialize knowledge embedding provider: %w", err)
		}
		fingerprint = domain.ModelFingerprint(retrieval.Embedding.ProviderID, retrieval.Embedding.Model, retrieval.Embedding.Dimensions)
		embeddingWorker, err = knowledgeusecase.NewEmbeddingWorker(knowledgeusecase.EmbeddingWorkerConfig{
			Interval:   time.Duration(retrieval.WorkerIntervalSeconds) * time.Second,
			MaxRetries: retrieval.WorkerMaxRetries,
			BatchSize:  retrieval.WorkerBatchSize,
			Limits:     limits,
			ProviderID: retrieval.Embedding.ProviderID,
			Model:      retrieval.Embedding.Model,
			Dimensions: retrieval.Embedding.Dimensions,
		}, knowledgeusecase.EmbeddingWorkerDependencies{
			Queue: wakingKnowledgeQueue{KnowledgeQueueStore: sqlite.NewKnowledgeEmbeddingQueueStore(store), wake: schedules.embedding.Wake}, Source: sourceStore,
			Index:    sqlite.NewKnowledgeVectorIndexStore(store),
			Provider: provider, Resolver: resolver, Lister: sourceStore,
			Logger: models.logger, Sanitize: models.redactor.String, Redact: models.redactor.String,
			Metrics: models.metrics, Scheduler: schedules.embedding,
		})
		if err != nil {
			return nil, fmt.Errorf("initialize knowledge embedding worker: %w", err)
		}
	}
	// The retrieval-facing index store binds the real fingerprint when
	// embeddings are enabled so SearchSemantic serves the current provider
	// configuration; with embeddings disabled the fingerprint stays empty
	// and semantic search fails closed.
	indexStore := sqlite.NewKnowledgeLexicalIndexStore(store, fingerprint)
	if indexStore == nil {
		return nil, errors.New("knowledge retrieval adapters are not configured")
	}
	retriever, err := knowledgeusecase.NewRetriever(knowledgeusecase.RetrieverDependencies{
		Reader: candidateReader, Index: indexStore, Resolver: resolver,
		Queue: queueStore, Provider: provider, Redact: models.redactor.String, Metrics: models.metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize knowledge retriever: %w", err)
	}
	worker, err := knowledgeusecase.NewLexicalWorker(knowledgeusecase.LexicalWorkerConfig{
		Interval:   time.Duration(retrieval.WorkerIntervalSeconds) * time.Second,
		MaxRetries: retrieval.WorkerMaxRetries,
		BatchSize:  retrieval.WorkerBatchSize,
		Limits:     limits,
	}, knowledgeusecase.LexicalWorkerDependencies{
		Queue: queueStore, Source: sourceStore, Index: indexStore,
		Resolver: resolver, Lister: sourceStore,
		Logger: models.logger, Sanitize: models.redactor.String, Redact: models.redactor.String,
		Metrics: models.metrics, Scheduler: schedules.lexical,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize knowledge lexical worker: %w", err)
	}
	return &knowledgeRetrievalComposition{lexicalWorker: worker, embeddingWorker: embeddingWorker, retriever: retriever, limits: limits}, nil
}
