package app

import (
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	"github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	knowledgeusecase "github.com/Dauno/slack-local-agent/internal/usecase/knowledge"
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
func composeLexicalRetrieval(cfg config.Config, models runtimeModels, modelCalls port.ModelCallLimiter, store *sqlite.Store) (*knowledgeRetrievalComposition, error) {
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
	queueStore := sqlite.NewKnowledgeLexicalQueueStore(store)
	sourceStore := sqlite.NewKnowledgeIndexSourceStore(store)
	candidateReader := sqlite.NewKnowledgeCandidateReader(store)
	resolver := sqlite.NewKnowledgeDocumentResolver(store)
	if queueStore == nil || sourceStore == nil || candidateReader == nil || resolver == nil {
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
			Queue: sqlite.NewKnowledgeEmbeddingQueueStore(store), Source: sourceStore,
			Index:    sqlite.NewKnowledgeVectorIndexStore(store),
			Provider: provider, Resolver: resolver, Lister: sourceStore,
			Logger: models.logger, Sanitize: models.redactor.String, Redact: models.redactor.String,
			Metrics: models.metrics,
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
		Metrics: models.metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize knowledge lexical worker: %w", err)
	}
	return &knowledgeRetrievalComposition{lexicalWorker: worker, embeddingWorker: embeddingWorker, retriever: retriever, limits: limits}, nil
}
