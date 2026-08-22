package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/secure"
	knowledgeusecase "github.com/Dauno/slack-local-agent/internal/usecase/knowledge"
)

// RebuildKnowledgeIndexes gives the operator a bounded path to reconstruct
// the TRD 06 reconstructible knowledge indexes (finding 12) without the
// destructive `init --reset-state`. It clears the lexical FTS index and
// re-enqueues every truth identity (`LexicalWorker.Rebuild`); the actual
// re-indexing then drains through the running agent's normal lexical
// worker poll loop, exactly like a fresh boot's reconciliation. It never
// touches knowledge_claims, knowledge_preferences, knowledge_documents, or
// any other truth table — only the reconstructible FTS index and its queue.
// It is cancelable: the enqueue loop checks ctx.Err() on every page. Vector
// (embedding) index rebuild is not yet wired to this entry point; embedding
// reconciliation still runs at every worker startup as before.
func (a *Application) RebuildKnowledgeIndexes(ctx context.Context) (domain.KnowledgeIndexRebuildResult, error) {
	configPath, err := config.ConfigPath(a.root)
	if err != nil {
		return domain.KnowledgeIndexRebuildResult{}, err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.KnowledgeIndexRebuildResult{}, errors.New("configuration not found")
		}
		return domain.KnowledgeIndexRebuildResult{}, err
	}
	if !cfg.Orchestration.Knowledge.Enabled {
		return domain.KnowledgeIndexRebuildResult{}, errors.New("orchestration.knowledge.enabled is false: there is no reconstructible knowledge index to rebuild")
	}
	paths, err := cfg.ResolvePaths(a.root)
	if err != nil {
		return domain.KnowledgeIndexRebuildResult{}, err
	}
	lock, err := a.schemaLock(paths.DatabaseFile)
	if err != nil {
		return domain.KnowledgeIndexRebuildResult{}, schemaLockFailure(err)
	}
	defer func() { _ = lock.Release() }()
	store, err := a.openCurrentTraced(ctx, paths.DatabaseFile)
	if err != nil {
		return domain.KnowledgeIndexRebuildResult{}, schemaOpenFailure(err)
	}
	defer store.Close()

	retrieval := cfg.Orchestration.Knowledge.Retrieval
	limits, err := knowledgeRetrievalLimits(cfg)
	if err != nil {
		return domain.KnowledgeIndexRebuildResult{}, err
	}
	logger := logging.New(a.logOutput, "info", secure.NewRedactor())
	redactor := secure.NewRedactor()

	queueStore := adaptersqlite.NewKnowledgeLexicalQueueStore(store)
	sourceStore := adaptersqlite.NewKnowledgeIndexSourceStore(store)
	resolver := adaptersqlite.NewKnowledgeDocumentResolver(store)
	indexStore := adaptersqlite.NewKnowledgeLexicalIndexStore(store, "")
	if queueStore == nil || sourceStore == nil || resolver == nil || indexStore == nil {
		return domain.KnowledgeIndexRebuildResult{}, errors.New("knowledge index adapters are not configured")
	}
	worker, err := knowledgeusecase.NewLexicalWorker(knowledgeusecase.LexicalWorkerConfig{
		Interval:   time.Duration(retrieval.WorkerIntervalSeconds) * time.Second,
		MaxRetries: retrieval.WorkerMaxRetries,
		BatchSize:  retrieval.WorkerBatchSize,
		Limits:     limits,
	}, knowledgeusecase.LexicalWorkerDependencies{
		Queue: queueStore, Source: sourceStore, Index: indexStore,
		Resolver: resolver, Lister: sourceStore,
		Logger: logger, Sanitize: redactor.String, Redact: redactor.String,
	})
	if err != nil {
		return domain.KnowledgeIndexRebuildResult{}, fmt.Errorf("initialize knowledge lexical worker: %w", err)
	}
	if err := worker.Rebuild(ctx); err != nil {
		return domain.KnowledgeIndexRebuildResult{}, fmt.Errorf("rebuild lexical index: %w", err)
	}
	result := domain.KnowledgeIndexRebuildResult{LexicalRebuilt: true}
	if retrieval.Embedding.Enabled {
		result.EmbeddingSkippedReason = "embedding index rebuild is not yet supported by this command; restart the agent so the embedding worker's normal startup reconciliation can repair queue state"
	} else {
		result.EmbeddingSkippedReason = "orchestration.knowledge.retrieval.embedding.enabled is false: there is no vector index to rebuild"
	}
	return result, nil
}
