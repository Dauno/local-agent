package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"

	"github.com/Dauno/slack-local-agent/internal/adapter/acpclient"
	"github.com/Dauno/slack-local-agent/internal/adapter/adkagent"
	"github.com/Dauno/slack-local-agent/internal/adapter/adkartifact"
	"github.com/Dauno/slack-local-agent/internal/adapter/envfile"
	"github.com/Dauno/slack-local-agent/internal/adapter/filesystem"
	"github.com/Dauno/slack-local-agent/internal/adapter/fsartifact"
	"github.com/Dauno/slack-local-agent/internal/adapter/fssandbox"
	"github.com/Dauno/slack-local-agent/internal/adapter/goast"
	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	"github.com/Dauno/slack-local-agent/internal/adapter/lspclient"
	"github.com/Dauno/slack-local-agent/internal/adapter/lspdiscovery"
	"github.com/Dauno/slack-local-agent/internal/adapter/memoryprojector"
	metricsadapter "github.com/Dauno/slack-local-agent/internal/adapter/metrics"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	"github.com/Dauno/slack-local-agent/internal/adapter/openaistt"
	"github.com/Dauno/slack-local-agent/internal/adapter/opencodemanager"
	"github.com/Dauno/slack-local-agent/internal/adapter/rangedreader"
	"github.com/Dauno/slack-local-agent/internal/adapter/recoverableresult"
	slackadapter "github.com/Dauno/slack-local-agent/internal/adapter/slack"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/adapter/tokencounter"
	"github.com/Dauno/slack-local-agent/internal/adapter/toolfactory"
	"github.com/Dauno/slack-local-agent/internal/adapter/toolrunner"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
	"github.com/Dauno/slack-local-agent/internal/tooldef"
	"github.com/Dauno/slack-local-agent/internal/usecase/agentbuilder"
	"github.com/Dauno/slack-local-agent/internal/usecase/bootstrap"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
	canvasusecase "github.com/Dauno/slack-local-agent/internal/usecase/canvas"
	contextcompilerusecase "github.com/Dauno/slack-local-agent/internal/usecase/contextcompiler"
	contextsummary "github.com/Dauno/slack-local-agent/internal/usecase/contextsummary"
	externalagentusecase "github.com/Dauno/slack-local-agent/internal/usecase/externalagent"
	generatedfileusecase "github.com/Dauno/slack-local-agent/internal/usecase/generatedfile"
	knowledgeusecase "github.com/Dauno/slack-local-agent/internal/usecase/knowledge"
	opencodeusecase "github.com/Dauno/slack-local-agent/internal/usecase/opencode"
	resultanalysisusecase "github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
	resultsusecase "github.com/Dauno/slack-local-agent/internal/usecase/results"
	sandboxusecase "github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
	workstreamusecase "github.com/Dauno/slack-local-agent/internal/usecase/workstream"
)

const defaultAttachmentTimeout = 120 * time.Second

type runtimeSetup struct {
	cfg   config.Config
	paths config.Paths
	defs  *agentdef.Definitions
}

type runtimeModels struct {
	rootModel             model.LLM
	rootFamily            string
	rootIsAgentCLI        bool
	preparedAgentTools    []preparedAgentTool
	preparedWorkflows     []preparedWorkflowTool
	agentName             string
	rootDef               *agentdef.AgentDef
	attachmentDef         *agentdef.AgentDef
	rootProviderID        string
	rootModelName         string
	rootReasoningEffort   string
	attachmentModel       model.LLM
	transcriber           port.AudioTranscriber
	apiKey                string
	botToken              string
	appToken              string
	embeddingAPIKey       string
	resultAnalysisAPIKey  string
	modelBaseURL          string
	redactor              secure.Redactor
	logger                *logging.Logger
	openCodeCoordinator   *opencodeusecase.Coordinator
	artifactStore         port.ResultArtifactStore
	resultPayloadStore    port.ResultPayloadStore
	requestTokenCounter   port.RequestTokenCounter
	contextWindowTokens   int
	rootDirectInlineBytes int64
	metrics               port.MetricRecorder
}

func newRuntimeModels() runtimeModels {
	return runtimeModels{
		rootFamily:          domain.ProviderFamilyOpenAICompatible,
		openCodeCoordinator: opencodeusecase.NewCoordinator(),
		metrics:             metricsadapter.NewRecorder(),
	}
}

func bindForegroundRuntimes(models runtimeModels, jobs synchronousExternalAgentJobs) {
	for _, child := range models.preparedAgentTools {
		if runtime, ok := child.acpRuntime.(*foregroundExternalAgentRuntime); ok {
			runtime.setJobRunner(jobs)
		}
	}
	for _, workflow := range models.preparedWorkflows {
		for _, runtime := range workflow.acpRuntimes {
			if facade, ok := runtime.(*foregroundExternalAgentRuntime); ok {
				facade.setJobRunner(jobs)
			}
		}
	}
}

type runtimeInfrastructure struct {
	store                 *adaptersqlite.Store
	modelCalls            *modelcalllimiter.Limiter
	sdkLog                *log.Logger
	api                   *slackapi.Client
	auth                  *slackapi.AuthTestResponse
	grantedSlackScopes    string
	slackTimeout          time.Duration
	publisher             *slackadapter.Publisher
	history               *slackadapter.HistoryReader
	fileLoader            *slackadapter.FileLoader
	confirmationPublisher *slackadapter.ConfirmationPublisher
	blockPublisher        *slackadapter.BlockPublisher
	standardPublisher     *slackadapter.StandardPublisher
	attachmentProc        *adkartifact.Processor
	contextEnricher       *slackadapter.ContextEnricher
	sessionSvc            *adaptersqlite.AdkSessionService
	processRegistry       *inProcessRegistry
}

func (a *Application) loadRuntimeSetup() (runtimeSetup, error) {
	configPath, err := config.ConfigPath(a.root)
	if err != nil {
		return runtimeSetup{}, err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return runtimeSetup{}, errors.New("Configuration not found. Run: local-agent init")
		}
		return runtimeSetup{}, fmt.Errorf("load runtime configuration: %w", err)
	}
	paths, err := cfg.ResolvePaths(a.root)
	if err != nil {
		return runtimeSetup{}, err
	}
	info, statErr := os.Stat(paths.StateDir)
	if errors.Is(statErr, os.ErrNotExist) {
		return runtimeSetup{}, errors.New("Local state not found. Run: local-agent init")
	}
	if statErr != nil {
		return runtimeSetup{}, fmt.Errorf("inspect configured state directory: %w. Run: local-agent doctor", statErr)
	}
	if !info.IsDir() {
		return runtimeSetup{}, errors.New("Configured state.dir is not a directory. Run: local-agent doctor")
	}
	defs, err := agentdef.Load(paths.StateDir)
	if err != nil {
		return runtimeSetup{}, fmt.Errorf("load agent definitions: %w", err)
	}
	if defs == nil {
		return runtimeSetup{}, errors.New("declarative agent definitions are required; check " + filepath.Join(paths.StateDir, "agents") + " and " + filepath.Join(paths.StateDir, "providers"))
	}
	return runtimeSetup{cfg: cfg, paths: paths, defs: defs}, nil
}

func (a *Application) prepareRuntimeModels(ctx context.Context, setup runtimeSetup) (runtimeModels, error) {
	cfg, paths, defs := setup.cfg, setup.paths, setup.defs
	prepared := newRuntimeModels()
	describedCLIProviders := make(map[string]bool)
	rootDefCandidate, ok := defs.Agents["root_agent"]
	if !ok {
		return runtimeModels{}, errors.New("agent definition root_agent is required when declarative agents are configured")
	}
	prepared.rootDef = &rootDefCandidate
	if attachment, ok := defs.Agents["attachment_analyzer"]; ok {
		prepared.attachmentDef = &attachment
	}

	providerEnvs := defs.RequiredAPIKeyEnvs()
	// The embedding API key must resolve inside this same secret-resolution
	// pass: the redactor is immutable once constructed and every resolved
	// credential feeds it here, so a key resolved later would silently
	// never be redacted from logs and errors. It is requested only when
	// embeddings are actually enabled.
	embeddingEnabled := cfg.Orchestration.Knowledge.Enabled && cfg.Orchestration.Knowledge.Retrieval.Enabled && cfg.Orchestration.Knowledge.Retrieval.Embedding.Enabled
	embeddingKeyEnv := cfg.Orchestration.Knowledge.Retrieval.Embedding.APIKeyEnv
	// The result analysis model's API key must resolve in this same pass
	// for the same reason as the embedding key above: the redactor is
	// immutable once constructed below, so a key resolved later would
	// never be redacted from logs and errors. It is requested only when
	// the gate and the dedicated model profile are both enabled.
	resultAnalysisModelEnabled := cfg.Orchestration.ResultAnalysis.Enabled && cfg.Orchestration.ResultAnalysis.Model.Enabled
	resultAnalysisKeyEnv := cfg.Orchestration.ResultAnalysis.Model.APIKeyEnv
	allKeys := append(append(make([]string, 0, len(providerEnvs)+3), providerEnvs...), bootstrap.SlackBotTokenEnv, bootstrap.SlackAppTokenEnv)
	if embeddingEnabled && strings.TrimSpace(embeddingKeyEnv) != "" {
		allKeys = append(allKeys, embeddingKeyEnv)
	}
	if resultAnalysisModelEnabled && strings.TrimSpace(resultAnalysisKeyEnv) != "" {
		allKeys = append(allKeys, resultAnalysisKeyEnv)
	}
	values, err := envfile.NewResolver(paths.EnvFile).Resolve(allKeys...)
	if err != nil {
		return runtimeModels{}, fmt.Errorf("load runtime secrets: %w", err)
	}
	prepared.botToken = values[bootstrap.SlackBotTokenEnv]
	prepared.appToken = values[bootstrap.SlackAppTokenEnv]
	prepared.embeddingAPIKey = values[embeddingKeyEnv]
	prepared.resultAnalysisAPIKey = values[resultAnalysisKeyEnv]
	if err := requiredSlackTokens(prepared.botToken, prepared.appToken); err != nil {
		return runtimeModels{}, err
	}
	secrets := make([]string, 0, len(providerEnvs)+3)
	for _, environment := range providerEnvs {
		secrets = append(secrets, values[environment])
	}
	if embeddingEnabled {
		secrets = append(secrets, prepared.embeddingAPIKey)
	}
	if resultAnalysisModelEnabled {
		secrets = append(secrets, prepared.resultAnalysisAPIKey)
	}
	prepared.redactor = secure.NewRedactor(append(secrets, prepared.botToken, prepared.appToken)...)
	prepared.logger = logging.New(a.logOutput, cfg.Runtime.LogLevel, prepared.redactor)
	if cfg.Context.ModelBudget != nil && cfg.Context.ModelBudget.MaxRequestPercent > 70 {
		prepared.logger.Warn("model request ceiling is above the recommended maximum", "max_request_percent", cfg.Context.ModelBudget.MaxRequestPercent)
	}
	artifactStore, artifactErr := fsartifact.New(paths.ArtifactDir, int64(cfg.ACP.MaxResultArtifactBytes))
	if artifactErr != nil {
		return runtimeModels{}, prepared.redactor.Error(fmt.Errorf("initialize ACP result artifact store: %w", artifactErr))
	}
	prepared.artifactStore = artifactStore
	prepared.resultPayloadStore, err = fsartifact.NewTypedStore(filepath.Join(paths.ArtifactDir, "v2-results"), int64(cfg.ACP.MaxResultArtifactBytes))
	if err != nil {
		return runtimeModels{}, prepared.redactor.Error(fmt.Errorf("initialize typed result artifact store: %w", err))
	}

	resolved, err := defs.ResolveModel(prepared.rootDef.Model)
	if err != nil {
		return runtimeModels{}, fmt.Errorf("resolve root agent model: %w", err)
	}
	prepared.requestTokenCounter, err = composeRootTokenCounter(resolved)
	if err != nil {
		return runtimeModels{}, err
	}
	prepared.rootDirectInlineBytes = int64(resolved.MaxDirectInlineBytes)
	prepared.contextWindowTokens = resolved.ContextWindowTokens
	if err := config.ValidateADKCompaction(cfg, resolved.Type() == agentdef.ProviderTypeOpenAICompatible, resolved.Type() == agentdef.ProviderTypeOpenAICompatible); err != nil {
		return runtimeModels{}, err
	}
	prepared.rootProviderID = resolved.Provider.Name
	prepared.rootModelName = resolved.Model
	prepared.rootReasoningEffort = resolved.ReasoningEffort
	builtRoot, rootSecret, err := newModelForResolved(ctx, resolved, values, cfg, paths, prepared.logger, prepared.redactor.String)
	if err != nil {
		return runtimeModels{}, prepared.redactor.Error(fmt.Errorf("build root model client: %w", err))
	}
	if httpModel, ok := builtRoot.(*openaillm.OpenAICompatibleLLM); ok {
		budget, budgetErr := domain.NewRequestBudget(resolved.ContextWindowTokens, domain.RequestBudgetPolicy{
			MaxRequestPercent: cfg.Context.ModelBudget.MaxRequestPercent,
		})
		if budgetErr != nil {
			return runtimeModels{}, fmt.Errorf("compose root model request budget: %w", budgetErr)
		}
		profileID := resolved.Provider.Name + "/" + resolved.Model
		if guardErr := httpModel.ConfigureRequestGuard(prepared.requestTokenCounter, budget, profileID, prepared.metrics); guardErr != nil {
			return runtimeModels{}, fmt.Errorf("configure root model final request guard: %w", guardErr)
		}
	}
	if err := handshakeSelectedAgentCLI(ctx, resolved, builtRoot, describedCLIProviders); err != nil {
		return runtimeModels{}, prepared.redactor.Error(fmt.Errorf("validate root agent model: %w", err))
	}
	prepared.rootModel = builtRoot
	if resolved.IsAgentCLI() {
		prepared.rootIsAgentCLI = true
		prepared.rootFamily = domain.ProviderFamilyAgentCLI
		prepared.modelBaseURL = "agent_cli:" + resolved.Provider.Name
	} else {
		prepared.apiKey = rootSecret
		prepared.modelBaseURL = resolved.BaseURL
	}
	acpRuntimeFactory := func(resolved *agentdef.ResolvedModel) (port.ExternalAgentRuntime, error) {
		direct := acpclient.NewWithCoordinatorAndBounds(resolved.Command, resolved.Args, prepared.openCodeCoordinator, acpclient.Bounds{
			MaxFrameBytes: cfg.ACP.MaxFrameBytes, MaxInlineResultBytes: cfg.ACP.MaxInlineResultBytes,
			MaxResultArtifactBytes: cfg.ACP.MaxResultArtifactBytes, StderrTailBytes: cfg.ACP.StderrTailBytes,
		})
		return newForegroundExternalAgentRuntime(direct, nil), nil
	}
	prepared.preparedAgentTools, err = prepareRootAgentTools(ctx, defs, *prepared.rootDef, values, cfg, paths, prepared.logger, prepared.redactor.String, describedCLIProviders, acpRuntimeFactory)
	if err != nil {
		return runtimeModels{}, prepared.redactor.Error(err)
	}
	prepared.preparedWorkflows, err = prepareRootWorkflowTools(ctx, defs, *prepared.rootDef, values, cfg, paths, prepared.logger, prepared.redactor.String, describedCLIProviders, paths.StateDir, acpRuntimeFactory, prepared.openCodeCoordinator)
	if err != nil {
		return runtimeModels{}, prepared.redactor.Error(err)
	}

	if prepared.attachmentDef != nil {
		attachmentResolved, err := defs.ResolveModel(prepared.attachmentDef.Model)
		if err != nil {
			return runtimeModels{}, fmt.Errorf("resolve attachment analyzer model: %w", err)
		}
		if err := validateAttachmentModel(attachmentResolved); err != nil {
			return runtimeModels{}, err
		}
		prepared.attachmentModel, _, err = newModelForResolved(ctx, attachmentResolved, values, cfg, paths, prepared.logger, prepared.redactor.String)
		if err != nil {
			return runtimeModels{}, prepared.redactor.Error(fmt.Errorf("build attachment analyzer model client: %w", err))
		}
	}
	if profile := strings.TrimSpace(cfg.Slack.Files.TranscriptionProfile); profile != "" {
		transcriptionResolved, err := defs.ResolveModel(profile)
		if err != nil {
			return runtimeModels{}, fmt.Errorf("resolve transcription profile %q: %w", profile, err)
		}
		if err := validateTranscriptionModel(transcriptionResolved); err != nil {
			return runtimeModels{}, err
		}
		apiKey := values[transcriptionResolved.APIKeyEnv]
		if strings.TrimSpace(apiKey) == "" {
			return runtimeModels{}, fmt.Errorf("%s is not configured for transcription profile %q", transcriptionResolved.APIKeyEnv, profile)
		}
		prepared.transcriber, err = openaistt.New(openaistt.Config{
			BaseURL: transcriptionResolved.BaseURL,
			APIKey:  apiKey,
			Model:   transcriptionResolved.Model,
			Headers: transcriptionResolved.Headers,
		})
		if err != nil {
			return runtimeModels{}, prepared.redactor.Error(fmt.Errorf("build transcription client: %w", err))
		}
	}
	prepared.agentName = prepared.rootDef.Name

	if prepared.rootModel == nil {
		return runtimeModels{}, prepared.redactor.Error(errors.New("model client not initialized"))
	}
	if cfg.Slack.StandardAgent.StreamingEnabled {
		capability, ok := prepared.rootModel.(interface{ SupportsStreaming() bool })
		if !ok || !capability.SupportsStreaming() {
			return runtimeModels{}, errors.New("standard agent streaming is enabled but the selected root provider does not support true incremental output")
		}
	}
	return prepared, nil
}

func composeRootTokenCounter(resolved *agentdef.ResolvedModel) (port.RequestTokenCounter, error) {
	if problems := agentdef.ValidateProfileCapability(resolved); len(problems) > 0 {
		return nil, fmt.Errorf("validate root model context capability: %s", strings.Join(problems, "; "))
	}
	if resolved.Type() != agentdef.ProviderTypeOpenAICompatible {
		return nil, nil
	}
	counter, err := tokencounter.New(resolved.CounterStrategy, resolved.CounterID)
	if err != nil {
		return nil, fmt.Errorf("compose root model token counter strategy %q id %q: %w", resolved.CounterStrategy, resolved.CounterID, err)
	}
	return counter, nil
}

func composeModelContextAdmission(resolved *agentdef.ResolvedModel, cfg config.Config, requestPercentOverride ...int) (port.RequestTokenCounter, domain.RequestBudget, error) {
	if resolved == nil || resolved.Type() != agentdef.ProviderTypeOpenAICompatible {
		return nil, domain.RequestBudget{}, nil
	}
	counter, err := composeRootTokenCounter(resolved)
	if err != nil {
		return nil, domain.RequestBudget{}, err
	}
	requestPercent := cfg.Context.ModelBudget.MaxRequestPercent
	if len(requestPercentOverride) > 0 && requestPercentOverride[0] > 0 {
		requestPercent = requestPercentOverride[0]
	}
	budget, err := domain.NewRequestBudget(resolved.ContextWindowTokens, domain.RequestBudgetPolicy{
		MaxRequestPercent: requestPercent,
	})
	if err != nil {
		return nil, domain.RequestBudget{}, err
	}
	return counter, budget, nil
}

func (a *Application) openRuntimeInfrastructure(ctx context.Context, setup runtimeSetup, models runtimeModels) (*runtimeInfrastructure, error) {
	cfg, paths := setup.cfg, setup.paths
	// Rollout-completeness half of run's preflight (checkpoint 4): read-only
	// reads under the lock acquired in Run, strictly before OpenCurrent's
	// own mode=rw open. The disposition-marker half is checkpoint 5.
	a.traceSchemaEvent("preflight")
	if err := a.requireRolloutComplete(ctx, paths.DatabaseFile); err != nil {
		if errors.Is(err, adaptersqlite.ErrDatabaseNotFound) {
			return nil, errors.New("Local state not found. Run: local-agent init")
		}
		return nil, rolloutPreflightFailure(err)
	}
	// Legacy-disposition half of run's preflight (checkpoint 5, closes
	// FIND-168): one more read-only marker read in the same lock-held pass,
	// still strictly before OpenCurrent.
	a.traceSchemaEvent("disposition")
	if err := a.requireLegacyIdentityDispositionComplete(ctx, paths.DatabaseFile); err != nil {
		return nil, rolloutPreflightFailure(err)
	}
	store, err := a.openCurrentTraced(ctx, paths.DatabaseFile)
	if err != nil {
		if errors.Is(err, adaptersqlite.ErrDatabaseNotFound) {
			return nil, errors.New("Local state not found. Run: local-agent init")
		}
		return nil, models.redactor.Error(schemaOpenFailure(err))
	}
	ok := false
	defer func() {
		if !ok {
			_ = store.Close()
		}
	}()
	if err := store.CleanupDedupe(ctx, time.Now().UTC()); err != nil {
		return nil, models.redactor.Error(err)
	}
	if err := store.EnsureDMIdentityMode(ctx, cfg.Slack.StandardAgent.ThreadedDM); err != nil {
		if errors.Is(err, adaptersqlite.ErrDMIdentityModeMismatch) {
			return nil, models.redactor.Error(fmt.Errorf("%w. Back up local state, then run: local-agent init --reset-state", err))
		}
		return nil, models.redactor.Error(fmt.Errorf("validate durable DM identity mode: %w", err))
	}
	if retention, ok := models.artifactStore.(interface {
		Cleanup(context.Context, time.Time) (int, error)
	}); ok {
		if configured, ok := models.artifactStore.(*fsartifact.Store); ok {
			configured.SetReferenceChecker(adaptersqlite.NewExternalAgentJobStore(store))
		}
		if _, err := retention.Cleanup(ctx, time.Now().UTC().AddDate(0, 0, -cfg.ACP.ArtifactRetentionDays)); err != nil {
			return nil, models.redactor.Error(fmt.Errorf("cleanup ACP result artifacts: %w", err))
		}
	}

	modelCalls := modelcalllimiter.New(cfg.Runtime.MaxConcurrentModelCalls)
	sdkLog := log.New(&redactingWriter{target: a.logOutput, redactor: models.redactor}, "slack: ", log.LstdFlags)
	var grantedSlackScopes string
	api := slackapi.New(
		models.botToken,
		slackapi.OptionAppLevelToken(models.appToken),
		slackapi.OptionLog(sdkLog),
		slackapi.OptionOnResponseHeaders(func(path string, headers http.Header) {
			if path == "auth.test" {
				grantedSlackScopes = headers.Get("X-OAuth-Scopes")
			}
		}),
	)
	authCtx, cancelAuth := optionalTimeout(ctx, time.Duration(cfg.Runtime.SlackAPITimeoutSeconds)*time.Second)
	auth, err := api.AuthTestContext(authCtx)
	cancelAuth()
	if err != nil {
		return nil, models.redactor.Error(fmt.Errorf("authenticate Slack bot: %w", err))
	}
	if auth == nil || auth.UserID == "" {
		return nil, errors.New("authenticate Slack bot: Slack returned no bot user ID")
	}
	if cfg.Canvases.Enabled && !hasSlackScope(grantedSlackScopes, "canvases:write") {
		return nil, errors.New("initialize Canvas support: Slack bot token is missing canvases:write; regenerate the manifest and reinstall the app")
	}
	if cfg.Exports.Enabled && !hasSlackScope(grantedSlackScopes, "files:write") {
		return nil, errors.New("initialize generated file exports: Slack bot token is missing files:write; regenerate the manifest and reinstall the app")
	}
	if durableACPConfigured(models) && !hasSlackScope(grantedSlackScopes, "files:write") {
		return nil, errors.New("initialize durable ACP result delivery: Slack bot token is missing files:write; regenerate the manifest and reinstall the app")
	}

	slackTimeout := time.Duration(cfg.Runtime.SlackAPITimeoutSeconds) * time.Second
	publisher := slackadapter.NewPublisher(api, slackTimeout, models.logger, cfg.Slack.PartLabels)
	history := slackadapter.NewHistoryReader(api, auth.UserID, slackTimeout, models.logger, cfg.Slack.PartLabels)
	fileLoader := slackadapter.NewFileLoader(api, models.botToken, slackTimeout)
	confirmationPublisher := slackadapter.NewConfirmationPublisher(api, auth.UserID, slackTimeout, models.logger)
	blockPublisher := slackadapter.NewBlockPublisher(api, slackTimeout, models.logger)
	standardPublisher := slackadapter.NewStandardPublisher(api, auth.UserID, slackTimeout, slackadapter.ResolveProgressLabels(cfg.Slack.StandardAgent.ProgressLabels))
	artifactSvc := artifact.InMemoryService()
	attachmentInstruction := ""
	attachmentTimeout := defaultAttachmentTimeout
	if models.attachmentDef != nil {
		attachmentInstruction = models.attachmentDef.Instruction
		if models.attachmentDef.TimeoutSeconds > 0 {
			attachmentTimeout = time.Duration(models.attachmentDef.TimeoutSeconds) * time.Second
		}
	}
	transcriptionTimeout := time.Duration(cfg.Slack.Files.TranscriptionTimeoutSeconds) * time.Second
	attachmentProc := adkartifact.NewProcessorWithTranscription(artifactSvc, models.attachmentModel, attachmentInstruction, attachmentTimeout, models.transcriber, transcriptionTimeout, modelCalls)
	if err := store.ReconcileAssistantExchanges(ctx, history); err != nil {
		return nil, models.redactor.Error(fmt.Errorf("reconcile assistant exchanges: %w", err))
	}
	contextEnricher := slackadapter.NewContextEnricherFromSDK(models.logger, api, slackadapter.ContextEnricherConfig{
		Enabled: cfg.Slack.Context.Enabled, MaxChars: cfg.Slack.Context.MaxChars,
		Timeout:              time.Duration(cfg.Slack.Context.TimeoutSeconds) * time.Second,
		ProfileCacheTTL:      time.Duration(cfg.Slack.Context.ProfileCacheTTLMinutes) * time.Minute,
		ConversationCacheTTL: time.Duration(cfg.Slack.Context.ConversationCacheTTLMinutes) * time.Minute,
	})

	sessionSvc := adaptersqlite.NewAdkSessionService(store)
	if sessionSvc == nil {
		return nil, errors.New("initialize ADK session service: SQLite store is unavailable")
	}
	families, err := sessionSvc.RootSessionProviderFamilies(ctx)
	if err != nil {
		return nil, models.redactor.Error(fmt.Errorf("inspect durable session provider families: %w", err))
	}
	if err := enforceProviderFamily(families, models.rootFamily); err != nil {
		return nil, models.redactor.Error(err)
	}
	infra := &runtimeInfrastructure{
		store: store, modelCalls: modelCalls, sdkLog: sdkLog, api: api, auth: auth,
		grantedSlackScopes: grantedSlackScopes, slackTimeout: slackTimeout,
		publisher: publisher, history: history, fileLoader: fileLoader,
		confirmationPublisher: confirmationPublisher, blockPublisher: blockPublisher,
		standardPublisher: standardPublisher, attachmentProc: attachmentProc,
		contextEnricher: contextEnricher, sessionSvc: sessionSvc,
		processRegistry: newInProcessRegistry(),
	}
	ok = true
	return infra, nil
}

type runtimeComposition struct {
	service              *botusecase.Service
	agentBuilderSvc      port.AgentBuilderService
	externalJobService   *externalagentusecase.Service
	notificationWorker   *externalagentusecase.NotificationWorker
	activationWorker     *externalagentusecase.ActivationWorker
	knowledgeRetriever   port.KnowledgeRetriever
	lexicalWorker        *knowledgeusecase.LexicalWorker
	embeddingWorker      *knowledgeusecase.EmbeddingWorker
	resultAnalysisWorker *resultanalysisusecase.Worker
	notificationDone     chan struct{}
}

func (c *runtimeComposition) StopExternalAdmission() {
	if c == nil {
		return
	}
	if c.externalJobService != nil {
		c.externalJobService.StopAdmission()
	}
	if c.notificationWorker != nil {
		c.notificationWorker.StopAdmission()
	}
	if c.activationWorker != nil {
		c.activationWorker.StopAdmission()
	}
}

// WaitKnowledge waits for the lexical worker to stop. A nil worker means
// retrieval was disabled and nothing needs draining.
func (c *runtimeComposition) WaitKnowledge(ctx context.Context) error {
	if c == nil || c.lexicalWorker == nil {
		return nil
	}
	return c.lexicalWorker.WaitStopped(ctx)
}

// WaitEmbedding waits for the embedding worker to stop. A nil worker means
// embeddings were disabled and nothing needs draining.
func (c *runtimeComposition) WaitEmbedding(ctx context.Context) error {
	if c == nil || c.embeddingWorker == nil {
		return nil
	}
	return c.embeddingWorker.WaitStopped(ctx)
}

// WaitResultAnalysis waits for the TRD 07 result analysis worker to stop.
// A nil worker means the gate was disabled and nothing needs draining.
func (c *runtimeComposition) WaitResultAnalysis(ctx context.Context) error {
	if c == nil || c.resultAnalysisWorker == nil {
		return nil
	}
	return c.resultAnalysisWorker.WaitStopped(ctx)
}

func (c *runtimeComposition) WaitExternal(ctx context.Context) error {
	if c == nil {
		return nil
	}
	waiters := 0
	waitDone := make(chan error, 2)
	if c.externalJobService != nil {
		waiters++
		go func() { waitDone <- c.externalJobService.WaitStopped(ctx) }()
	}
	if c.activationWorker != nil {
		waiters++
		go func() { waitDone <- c.activationWorker.WaitStopped(ctx) }()
	}
	var waitErrs []error
	for range waiters {
		if err := <-waitDone; err != nil {
			waitErrs = append(waitErrs, err)
		}
	}
	return errors.Join(waitErrs...)
}

func (c *runtimeComposition) ExternalShutdownStats(ctx context.Context) (domain.ExternalAgentJobShutdownStats, error) {
	if c == nil || c.externalJobService == nil {
		return domain.ExternalAgentJobShutdownStats{}, nil
	}
	return c.externalJobService.ShutdownStats(ctx)
}

func (c *runtimeComposition) WaitNotification(ctx context.Context) error {
	if c != nil && c.notificationWorker != nil {
		return c.notificationWorker.WaitStopped(ctx)
	}
	if c == nil || c.notificationDone == nil {
		return nil
	}
	select {
	case <-c.notificationDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *runtimeComposition) ActivationHealth(ctx context.Context) (domain.ExternalAgentJobActivationHealth, error) {
	if c == nil || c.activationWorker == nil {
		return domain.ExternalAgentJobActivationHealth{}, nil
	}
	return c.activationWorker.SnapshotHealth(ctx, time.Now().UTC())
}

// bindAndCleanupRecoverableResults binds the recoverable-result store's
// reference checker to the SQLite-backed index before the first cleanup pass
// runs, so the two calls can never land in composition separately. A test
// that only exercises DeleteExpired or IsRecoverableResultReferenced on
// their own cannot catch a build that drops SetReferenceChecker; a test that
// calls this same function can (TRD 08 checkpoint 6).
func bindAndCleanupRecoverableResults(ctx context.Context, store *adaptersqlite.Store, resultStore *recoverableresult.Store, cutoff time.Time, batchSize int) (int, error) {
	resultStore.SetReferenceChecker(store)
	return resultStore.DeleteExpired(ctx, cutoff, batchSize)
}

func (a *Application) composeRuntime(ctx context.Context, setup runtimeSetup, models runtimeModels, infra *runtimeInfrastructure) (*runtimeComposition, error) {
	cfg, paths := setup.cfg, setup.paths
	defs := setup.defs
	var agentBuilderSvc port.AgentBuilderService
	if defs != nil {
		agentBuilderSvc = agentbuilder.New()
	}
	var toolFactory port.AgentToolFactory
	var compositeFactory *compositeAgentToolFactory
	var externalJobService *externalagentusecase.Service
	var notificationWorker *externalagentusecase.NotificationWorker
	var activationStore port.ExternalAgentJobActivationStore
	var activationWorker *externalagentusecase.ActivationWorker
	var externalSchedules externalAgentSchedules
	var notificationDone chan struct{}
	var summaryScheduler port.SummaryScheduler
	var continuityStore port.ContinuityStore
	var resultStore *recoverableresult.Store
	var workstreamService *workstreamusecase.Service
	var knowledgeBindings port.KnowledgeBindingResolver
	var knowledgeRetriever port.KnowledgeRetriever
	var knowledgeLexicalWorker *knowledgeusecase.LexicalWorker
	var knowledgeEmbeddingWorker *knowledgeusecase.EmbeddingWorker
	var resultAnalysisWorker *resultanalysisusecase.Worker
	var knowledgeRetrievalLimits domain.KnowledgeRetrievalLimits
	features := cfg.Context.ContextFeatures
	var err error
	// The TRD 02 V2 retention policy is composed and validated at startup
	// even though no deletion worker consumes it yet (see TRD 02 §Retention,
	// finding 6): failing closed here catches a misconfigured age before the
	// offline doctor check ever runs against it.
	resultRetentionPolicy := resultsusecase.RetentionPolicy{
		Context:      time.Duration(cfg.Orchestration.ResultHandles.Retention.ContextDays) * 24 * time.Hour,
		Conversation: time.Duration(cfg.Orchestration.ResultHandles.Retention.ConversationDays) * 24 * time.Hour,
		Workstream:   time.Duration(cfg.Orchestration.ResultHandles.Retention.WorkstreamDays) * 24 * time.Hour,
		Exported:     time.Duration(cfg.Orchestration.ResultHandles.Retention.ExportedDays) * 24 * time.Hour,
	}
	if err := resultRetentionPolicy.Validate(); err != nil {
		return nil, fmt.Errorf("initialize result retention policy: %w", err)
	}
	if models.rootIsAgentCLI && cfg.Orchestration.Workstreams.Enabled {
		return nil, errors.New("orchestration.workstreams.enabled requires an openai_compatible root agent")
	}
	if !models.rootIsAgentCLI && features != nil && features.ContinuityCapsuleEnabled {
		continuityStore = adaptersqlite.NewContinuityStore(infra.store)
	}
	if !models.rootIsAgentCLI && features != nil && features.RecoverableResultsEnabled {
		resultsCfg := cfg.Context.RecoverableResults
		resultStore = recoverableresult.NewStore(infra.store.DB(), filepath.Join(paths.StateDir, "recoverable-results"), resultsCfg.MaxResultBytes, resultsCfg.ChunkMaxBytes, resultsCfg.RetentionDays, resultsCfg.CleanupBatchSize, models.metrics)
		if _, cleanupErr := bindAndCleanupRecoverableResults(ctx, infra.store, resultStore, time.Now().UTC(), resultsCfg.CleanupBatchSize); cleanupErr != nil {
			return nil, models.redactor.Error(cleanupErr)
		}
	}
	if !models.rootIsAgentCLI {
		var sandboxService *sandboxusecase.Service
		var codeReaders map[string]port.CodeReader
		var syntaxEngine port.SyntaxEngine
		var codeIntelligence port.CodeIntelligence
		if cfg.Sandbox.Enabled {
			projects := paths.SandboxProjectRoots
			if len(projects) == 0 {
				projects = cfg.Sandbox.Projects
			}
			executor, err := fssandbox.New(projects, cfg.Sandbox.MaxOutputBytes)
			if err != nil {
				return nil, models.redactor.Error(fmt.Errorf("initialize filesystem sandbox: %w", err))
			}
			executor.WithMetrics(models.metrics)
			sandboxService, err = sandboxusecase.New(sandboxusecase.Config{
				AllowedCapabilities: []domain.Capability{domain.CapListRepos, domain.CapListDirectory, domain.CapReadFile, domain.CapListWorktrees},
				CommandTimeout:      time.Duration(cfg.Sandbox.CommandTimeoutSeconds) * time.Second,
				MaxOutputBytes:      cfg.Sandbox.MaxOutputBytes,
			}, sandboxusecase.Dependencies{AuditStore: adaptersqlite.NewSandboxAuditStore(infra.store), Executor: executor})
			if err != nil {
				return nil, models.redactor.Error(fmt.Errorf("initialize sandbox service: %w", err))
			}
			if resultStore != nil && cfg.CodeIntelligence != nil && cfg.CodeIntelligence.Enabled {
				codeReaders = make(map[string]port.CodeReader, len(paths.SandboxProjectRoots))
				syntaxReaders := make(map[string]port.CodeReader, len(paths.SandboxProjectRoots))
				for name, root := range paths.SandboxProjectRoots {
					codeReaders[name] = rangedreader.NewReader(root, cfg.Sandbox.MaxOutputBytes, cfg.Context.RecoverableResults.ChunkMaxBytes, cfg.Context.RecoverableResults.MaxResultBytes).WithResultStore(resultStore).WithMetrics(models.metrics)
					syntaxReaders[name] = rangedreader.NewReader(root, 1<<20, cfg.Context.RecoverableResults.ChunkMaxBytes, cfg.Context.RecoverableResults.MaxResultBytes).WithResultStore(resultStore).WithPreservedLineEndings().WithMetrics(models.metrics)
				}
				syntaxEngine = goast.New(syntaxReaders).WithMetrics(models.metrics)
				if len(cfg.CodeIntelligence.LSPServers) > 0 {
					candidates := make([]port.ServerCandidate, 0, len(cfg.CodeIntelligence.LSPServers))
					for _, server := range cfg.CodeIntelligence.LSPServers {
						candidates = append(candidates, port.ServerCandidate{ID: server.ID, Command: server.Command, Args: server.Args, Languages: server.Languages})
					}
					rootList := make([]string, 0, len(paths.SandboxProjectRoots))
					for _, root := range paths.SandboxProjectRoots {
						rootList = append(rootList, root)
					}
					descriptors, discoveryErr := lspdiscovery.New(rootList).Discover(ctx, candidates)
					if discoveryErr != nil {
						return nil, models.redactor.Error(fmt.Errorf("discover language servers: %w", discoveryErr))
					}
					definitions := make(map[string]config.LSPServerConfig, len(cfg.CodeIntelligence.LSPServers))
					for _, server := range cfg.CodeIntelligence.LSPServers {
						definitions[server.ID] = server
					}
					servers := make([]lspclient.Server, 0, len(descriptors))
					for _, descriptor := range descriptors {
						if descriptor.Status != "available" {
							continue
						}
						definition := definitions[descriptor.ID]
						servers = append(servers, lspclient.Server{ID: descriptor.ID, Path: descriptor.Path, SHA256: descriptor.BinarySHA256, Args: definition.Args, Languages: definition.Languages})
					}
					if len(servers) > 0 {
						routes := make(map[string][]string, len(cfg.CodeIntelligence.LSPRoutes))
						for language, route := range cfg.CodeIntelligence.LSPRoutes {
							routes[language] = append([]string(nil), route.Priority...)
						}
						codeIntelligence, err = lspclient.New(lspclient.Config{Servers: servers, Routes: routes,
							ProjectRoots: paths.SandboxProjectRoots, Readers: syntaxReaders, ResultStore: resultStore, MaxProcesses: cfg.CodeIntelligence.MaxProcesses,
							InitTimeout:    time.Duration(cfg.CodeIntelligence.InitTimeoutSeconds) * time.Second,
							RequestTimeout: time.Duration(cfg.CodeIntelligence.RequestTimeoutSeconds) * time.Second})
						if err != nil {
							return nil, models.redactor.Error(fmt.Errorf("initialize language server runtime: %w", err))
						}
						if concrete, ok := codeIntelligence.(*lspclient.Client); ok {
							concrete.WithMetrics(models.metrics)
						}
					}
				}
			}
		}
		var canvasService *canvasusecase.Service
		if cfg.Canvases.Enabled {
			canvasCreator := slackadapter.NewCanvasCreator(infra.api, time.Duration(cfg.Canvases.TimeoutSeconds)*time.Second)
			canvasService, err = canvasusecase.New(canvasusecase.Config{MaxTitleChars: cfg.Canvases.MaxTitleChars, MaxContentChars: cfg.Canvases.MaxContentChars, MaxContentBytes: cfg.Canvases.MaxContentBytes}, canvasusecase.Dependencies{Creator: canvasCreator, Store: adaptersqlite.NewCanvasOperationStore(infra.store), Logger: models.logger, SanitizeContent: models.redactor.String})
			if err != nil {
				return nil, models.redactor.Error(fmt.Errorf("initialize canvas service: %w", err))
			}
		}
		var generatedFileService *generatedfileusecase.Service
		if cfg.Exports.Enabled {
			uploader := slackadapter.NewGeneratedFileUploader(infra.api, time.Duration(cfg.Exports.TimeoutSeconds)*time.Second)
			generatedFileService, err = generatedfileusecase.New(generatedfileusecase.Config{MaxFilenameChars: cfg.Exports.MaxFilenameChars, MaxContentBytes: cfg.Exports.MaxContentBytes}, generatedfileusecase.Dependencies{Uploader: uploader, Store: adaptersqlite.NewGeneratedFileOperationStore(infra.store), Logger: models.logger, SanitizeContent: models.redactor.String})
			if err != nil {
				return nil, models.redactor.Error(fmt.Errorf("initialize generated file export service: %w", err))
			}
		}
		// The TRD 07 result analysis gate is positive: with the feature
		// disabled, composeResultAnalysis returns (nil, nil) and never
		// constructs a v40 adapter, so no v40 table is opened or written
		// and the worker is never created. This must compose before the
		// workstream service below, because an enabled analysis feature
		// injects resultAnalysisComp.gate into the workstream service's
		// dependent-dispatch check.
		resultAnalysisComp, resultAnalysisErr := composeResultAnalysis(cfg, models, infra.modelCalls, infra.store)
		if resultAnalysisErr != nil {
			return nil, models.redactor.Error(fmt.Errorf("initialize result analysis: %w", resultAnalysisErr))
		}
		if resultAnalysisComp != nil {
			resultAnalysisWorker = resultAnalysisComp.worker
			go resultAnalysisWorker.Run(ctx)
		}
		factory := toolfactory.New(infra.store, sandboxService, canvasService, generatedFileService).WithAllowedUserIDs(cfg.Slack.AllowedUserIDs).WithRecoverableResults(resultStore).WithCodeReaders(codeReaders).WithSyntaxEngine(syntaxEngine).WithCodeIntelligence(codeIntelligence).WithMetrics(models.metrics)
		if resultAnalysisComp != nil {
			factory = factory.WithResultAnalysis(resultAnalysisComp.service)
		}
		if cfg.Orchestration.Workstreams.Enabled {
			var analysisGate port.AnalysesByWorkstream
			if resultAnalysisComp != nil {
				analysisGate = resultAnalysisComp.gate
			}
			var workstreamErr error
			workstreamService, workstreamErr = composeWorkstream(cfg, paths, infra.store, models.resultPayloadStore, analysisGate)
			if workstreamErr != nil {
				return nil, models.redactor.Error(workstreamErr)
			}
			factory.WithWorkstreams(workstreamService).WithResultLinksEnabled(cfg.Orchestration.ResultHandles.Enabled)
		}
		// Configurar Agent Builder (preview + install tools).
		if agentBuilderSvc != nil && defs != nil {
			agentsDir := filepath.Join(paths.StateDir, "agents")
			if info, statErr := os.Stat(agentsDir); statErr == nil && info.IsDir() {
				writer, writerErr := filesystem.NewAgentWriter(agentsDir)
				if writerErr == nil {
					factory = factory.
						WithAgentBuilder(agentBuilderSvc).
						WithAgentWriter(writer).
						WithCurrentDefinitions(defs).
						WithDraftStore(adaptersqlite.NewAgentDraftStore(infra.store))
				}
			}
		}
		if infra.publisher != nil && infra.api != nil {
			factory = factory.WithBuilderLauncher(
				slackadapter.NewBuilderLauncherPublisherWithStore(infra.api, infra.publisher, models.logger, infra.store, infra.auth.UserID),
			)
		}
		toolFactory = factory
		if len(models.preparedAgentTools) > 0 || len(models.preparedWorkflows) > 0 {
			delegatedGlobalInstruction := ""
			if models.rootDef != nil {
				delegatedGlobalInstruction = models.rootDef.EffectiveDelegatedGlobalInstruction()
			}
			compositeFactory = newCompositeAgentToolFactory(toolFactory, models.preparedAgentTools, models.preparedWorkflows, delegatedGlobalInstruction)
			compositeFactory.setChildContextResultStore(resultStore)
			toolFactory = compositeFactory
		}
		// Declarative tools must be wired after the composite factory exists so
		// children and workflow steps can reference them.
		if err := wireDeclarativeTools(factory, models, paths, compositeFactory); err != nil {
			return nil, models.redactor.Error(err)
		}
		externalSchedules, err = newExternalAgentSchedules()
		if err != nil {
			return nil, models.redactor.Error(fmt.Errorf("initialize external-agent schedulers: %w", err))
		}
		externalJobService, notificationWorker, err = newExternalAgentJobService(cfg, models, infra, externalSchedules)
		if err != nil {
			return nil, models.redactor.Error(fmt.Errorf("initialize external-agent jobs: %w", err))
		}
		if externalJobService != nil {
			factory.WithExternalAgentJobs(externalJobService)
			activationStore = adaptersqlite.NewExternalAgentJobStore(infra.store)
			if activationStore == nil {
				return nil, errors.New("initialize external-agent activation store")
			}
		}
		if compositeFactory != nil && externalJobService != nil {
			compositeFactory.setJobStarter(externalJobService)
			if workstreamService != nil {
				compositeFactory.setCompletionBindingResolver(workstreamService)
			}
		}
		if setup.defs != nil {
			if provider, exists := setup.defs.Providers["opencode"]; exists && provider.Type == agentdef.ProviderTypeACP {
				resolved, resolveErr := setup.defs.ResolveModel("opencode/smoke")
				if resolveErr != nil {
					return nil, models.redactor.Error(fmt.Errorf("resolve OpenCode management profile: %w", resolveErr))
				}
				primaryPath, pathErr := managementProbePath(paths.SandboxProjectRoots)
				if pathErr != nil {
					return nil, models.redactor.Error(pathErr)
				}
				toolFactory = &openCodeManagementToolFactory{base: toolFactory, runtime: acpclient.NewWithCoordinator(resolved.Command, resolved.Args, models.openCodeCoordinator), manager: opencodemanager.New(resolved.Command), allowedIDs: cfg.OpenCode.Management.AllowedUserIDs, primaryPath: primaryPath, configOptions: domainConfigOptions(resolved), coordinator: models.openCodeCoordinator}
			}
		}
	}
	if !models.rootIsAgentCLI && cfg.Context.ADKCompaction != nil && cfg.Context.ADKCompaction.SummaryEnabled {
		summarizer, summaryErr := openaillm.NewSummarizer(models.rootModel, openaillm.SummarizerConfig{
			MaxChars: cfg.Context.ADKCompaction.SummaryMaxChars, Timeout: 30 * time.Second,
			ModelCalls: infra.modelCalls, Redact: models.redactor.String,
		})
		if summaryErr != nil {
			return nil, models.redactor.Error(fmt.Errorf("initialize ADK summarizer: %w", summaryErr))
		}
		worker, workerErr := contextsummary.New(contextsummary.Config{MaxChars: cfg.Context.ADKCompaction.SummaryMaxChars, RecentTurns: cfg.Context.ADKCompaction.RecentTurns, WorkerInterval: time.Second}, contextsummary.Dependencies{
			Store: infra.store, Summarizer: summarizer,
			TurnSource: adkagent.DurableTurnSource{Service: infra.sessionSvc, AppName: "local-agent", UserID: "local_user", Redact: models.redactor.String},
		})
		if workerErr != nil {
			return nil, models.redactor.Error(fmt.Errorf("initialize ADK summary worker: %w", workerErr))
		}
		summaryScheduler = worker
		go worker.Run(ctx)
	}

	rtInstruction, rtGlobalInstruction := "", ""
	if models.rootDef != nil {
		rtInstruction, rtGlobalInstruction = models.rootDef.Instruction, models.rootDef.EffectiveRootGlobalInstruction()
	}
	var projector port.ContextProjector
	var compiler port.ContextCompiler
	var contextBudget domain.RequestBudget
	var contextCompaction domain.ContextCompactionSettings
	if !models.rootIsAgentCLI && features != nil && features.ModelBudgetEnabled {
		contextBudget, err = domain.NewRequestBudget(models.contextWindowTokens, domain.RequestBudgetPolicy{MaxRequestPercent: cfg.Context.ModelBudget.MaxRequestPercent})
		if err != nil {
			return nil, models.redactor.Error(fmt.Errorf("initialize context budget: %w", err))
		}
		compiler = contextcompilerusecase.New(resultStore, models.requestTokenCounter, models.metrics)
	}
	if !models.rootIsAgentCLI {
		compaction := cfg.Context.ADKCompaction
		if compaction == nil {
			return nil, errors.New("context.adk_compaction is required for durable model runtime")
		}
		contextCompaction = domain.ContextCompactionSettings{
			Enabled: compaction.Enabled, MaxHistoryChars: compaction.MaxHistoryChars,
			RecentTurns: compaction.RecentTurns, SummaryEnabled: compaction.SummaryEnabled,
			SummaryMaxChars: compaction.SummaryMaxChars, SummaryBudgetTokens: compaction.SummaryBudgetTokens,
		}
		if compiler == nil {
			projector, err = adkagent.NewProjector(adkagent.CompactionConfig{
				Enabled: compaction.Enabled, MaxHistoryChars: compaction.MaxHistoryChars,
				RecentTurns: compaction.RecentTurns, SummaryEnabled: compaction.SummaryEnabled,
				SummaryMaxChars: compaction.SummaryMaxChars,
			})
			if err != nil {
				return nil, models.redactor.Error(fmt.Errorf("initialize ADK context projector: %w", err))
			}
			if concrete, ok := projector.(*adkagent.Projector); ok {
				concrete.SetSummaryStore(infra.store)
				concrete.SetLogger(models.logger)
			}
		}
	}
	resultProducingTools := make([]string, 0, len(models.preparedAgentTools))
	if cfg.Orchestration.ResultHandles.Enabled {
		for _, child := range models.preparedAgentTools {
			if child.acpRuntime != nil {
				resultProducingTools = append(resultProducingTools, child.definition.Name)
			}
		}
	}
	runtime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{AgentName: models.agentName, Instruction: rtInstruction, GlobalInstruction: rtGlobalInstruction, SessionService: infra.sessionSvc, Model: models.rootModel, ToolFactory: toolFactory, ContextProjector: projector, ContextCompiler: compiler, ContextBudget: contextBudget, ContextCompaction: contextCompaction, ContinuityStore: continuityStore, SummaryStore: infra.store, EpochStore: adaptersqlite.NewContextEpochStore(infra.store), Metrics: models.metrics, ProviderFamily: models.rootFamily, ResultProducingToolNames: resultProducingTools, ResultProducingCallsPerStep: cfg.Orchestration.ResultHandles.MaxProducingCallsPerStep, ResultProducingCallReserveTokens: cfg.Orchestration.ResultHandles.ProducingCallReserveTokens, KnowledgeBudgetTokens: cfg.Orchestration.Knowledge.MaxCardTokens, WorkstreamBudgetTokens: cfg.Orchestration.Workstreams.SnapshotBudgetTokens})
	if err != nil {
		return nil, models.redactor.Error(fmt.Errorf("initialize ADK runtime: %w", err))
	}
	confirmationStore := adaptersqlite.NewConfirmationStore(infra.store)
	// One coordinator instance is shared by root turns, activations,
	// confirmations, workstream commands, and knowledge commands so a busy
	// conversation never mutates knowledge state while another operation
	// holds the conversation. The knowledge service is always wired: while the
	// gate is disabled its executor answers memory-human commands with a
	// deterministic disabled response instead of mutating or reaching the
	// model.
	coordinator := botusecase.NewLimiter(cfg.Runtime.MaxConcurrentModelCalls)
	// The OKF projector is a single shared, serialized instance used by the
	// knowledge projection worker, so concurrent promotions never corrupt
	// staging, backup, or the live bundle.
	okfProjector := memoryprojector.New()
	knowledgeStore := adaptersqlite.NewKnowledgeStore(infra.store)
	knowledgeSchedules, knowledgeWakes, err := newKnowledgeWakeSchedules(cfg)
	if err != nil {
		return nil, models.redactor.Error(err)
	}
	projectionScheduler := knowledgeSchedules.projection
	retrievalSchedules := knowledgeSchedules.retrieval
	knowledgeService, err := composeKnowledgeService(cfg.Orchestration.Knowledge.Enabled, knowledgeStore, coordinator, knowledgeWakes...)
	if err != nil {
		return nil, models.redactor.Error(fmt.Errorf("initialize knowledge service: %w", err))
	}
	// The knowledge projection worker starts only when the knowledge gate is on. While disabled it never
	// claims outbox rows or writes files; pending triggers survive and drain
	// immediately when the gate is re-enabled.
	if cfg.Orchestration.Knowledge.Enabled {
		projectionWorker, workerErr := knowledgeusecase.NewProjectionWorker(knowledgeusecase.ProjectionWorkerConfig{
			Interval:      time.Duration(cfg.Orchestration.Knowledge.ProjectionIntervalSeconds) * time.Second,
			MaxRetries:    cfg.Orchestration.Knowledge.ProjectionMaxRetries,
			RetentionDays: cfg.Orchestration.Knowledge.ProjectionRetentionDays,
			OutputDir:     paths.MemoryDir,
		}, knowledgeusecase.ProjectionWorkerDependencies{
			Store: knowledgeStore, Reader: infra.store, Projector: okfProjector,
			Logger: models.logger, Sanitize: models.redactor.String, Enabled: knowledgeService.Enabled, Scheduler: projectionScheduler,
		})
		if workerErr != nil {
			return nil, models.redactor.Error(fmt.Errorf("initialize knowledge projection worker: %w", workerErr))
		}
		go projectionWorker.Run(ctx)
		// The lexical index worker runs only when retrieval is enabled on
		// a durable openai_compatible root with the model-budget safety
		// gate (config validation enforces the gate statically; composition
		// re-asserts both fail closed). While disabled the worker is never
		// created, FTS is never touched, and queues never drain. The worker
		// owns startup reconciliation and an awaitable shutdown path. When
		// embeddings are enabled the embedding provider, vector index, and
		// embedding worker are composed alongside it and awaited on
		// shutdown the same way.
		retrievalComposition, composeErr := composeLexicalRetrieval(cfg, models, infra.modelCalls, infra.store, retrievalSchedules)
		if composeErr != nil {
			return nil, models.redactor.Error(composeErr)
		}
		if retrievalComposition != nil && retrievalComposition.lexicalWorker != nil {
			go retrievalComposition.lexicalWorker.Run(ctx)
			knowledgeRetriever = retrievalComposition.retriever
			knowledgeLexicalWorker = retrievalComposition.lexicalWorker
			knowledgeRetrievalLimits = retrievalComposition.limits
			if retrievalComposition.embeddingWorker != nil {
				go retrievalComposition.embeddingWorker.Run(ctx)
				knowledgeEmbeddingWorker = retrievalComposition.embeddingWorker
			}
		}
	}
	// Binding resolution is read-only host state and must never depend on the
	// workstream command feature gate: durable active workstreams keep their
	// project scope for knowledge even when workstream commands are disabled.
	registeredProjects := paths.SandboxProjectRoots
	if len(registeredProjects) == 0 {
		registeredProjects = cfg.Sandbox.Projects
	}
	knowledgeAllowedProjects := make(map[string]struct{}, len(registeredProjects))
	for project := range registeredProjects {
		knowledgeAllowedProjects[project] = struct{}{}
	}
	bindingsResolver := workstreamKnowledgeBindingResolver{
		store:   adaptersqlite.NewWorkstreamStore(infra.store),
		allowed: knowledgeAllowedProjects,
	}
	knowledgeBindings = bindingsResolver
	// The consuming root profile must declare a positive
	// result_handles.max_direct_inline_bytes to opt in to direct-inline
	// completion frames. The resolved declarative root profile is authoritative;
	// absence is zero and never transformed into a default.
	service, err := botusecase.New(botusecase.Config{
		AccessPolicy:   domain.AccessPolicy{AllowAllUsers: cfg.Slack.AllowAllUsers, AllowedUserIDs: cfg.Slack.AllowedUserIDs, AllowedTeamIDs: cfg.Slack.AllowedTeamIDs, AllowedChannelIDs: cfg.Slack.AllowedChannelIDs},
		ContextLimits:  domain.ContextLimits{MaxMessages: cfg.Context.MaxMessages, MaxChars: cfg.Context.MaxChars},
		RetainMessages: cfg.Context.RetainMessagesPerConversation, MaxConcurrentCalls: cfg.Runtime.MaxConcurrentModelCalls,
		ModelTimeout: time.Duration(cfg.Runtime.ModelTimeoutSeconds) * time.Second, BusyMessage: cfg.Runtime.BusyMessage, ModelErrorMessage: cfg.Runtime.ModelErrorMessage, UnauthorizedMessage: cfg.Slack.UnauthorizedMessage,
		ProgressEnabled: cfg.Slack.StandardAgent.ProgressEnabled, PromptsEnabled: cfg.Slack.StandardAgent.PromptsEnabled, SuggestedPrompts: cfg.Slack.StandardAgent.SuggestedPrompts, StreamingEnabled: cfg.Slack.StandardAgent.StreamingEnabled, UpdateInterval: time.Duration(cfg.Slack.StandardAgent.UpdateIntervalSeconds) * time.Second, StreamingCarryRunes: models.redactor.StreamingCarryRunes(),
		ResultHandlesEnabled:     cfg.Orchestration.ResultHandles.Enabled,
		MaxDirectInlineBytes:     models.rootDirectInlineBytes,
		KnowledgeRetrievalLimits: knowledgeRetrievalLimits,
		WorkstreamsEnabled:       cfg.Orchestration.Workstreams.Enabled,
		KnowledgeGateEnabled:     cfg.Orchestration.Knowledge.Enabled,
	}, botusecase.Dependencies{Store: infra.store, Runtime: runtime, ActivationStore: activationStore, CompletionReader: externalJobService, History: infra.history, Publisher: infra.publisher, Logger: models.logger, Exchange: infra.store, ModelCalls: infra.modelCalls, SanitizeContent: models.redactor.String, Enricher: infra.contextEnricher, ConfirmationStore: confirmationStore, ConfirmationPublisher: infra.confirmationPublisher, StructuredPublisher: infra.blockPublisher, FileLoader: infra.fileLoader, AttachmentProc: infra.attachmentProc, MaxAttachmentBytes: int64(cfg.Slack.Files.MaxBytesPerFile), MaxAttachmentChars: cfg.Slack.Files.MaxProcessedChars, StandardStore: infra.store, OnboardingStore: infra.store, ProgressPublisher: infra.standardPublisher, PromptPublisher: infra.standardPublisher, OnboardingPublisher: infra.standardPublisher, StreamingRuntime: runtime, IncrementalPublisher: infra.standardPublisher, SummaryScheduler: summaryScheduler, Workstreams: workstreamService, Coordinator: coordinator, Knowledge: knowledgeService, KnowledgeBindings: knowledgeBindings, KnowledgeRetriever: knowledgeRetriever, KnowledgeRetrievalBindings: bindingsResolver})
	if err != nil {
		return nil, err
	}
	if err := service.ReconcileConfirmations(ctx, infra.history); err != nil {
		return nil, models.redactor.Error(fmt.Errorf("reconcile confirmation deliveries: %w", err))
	}
	if err := service.ReconcileProgress(ctx); err != nil {
		return nil, models.redactor.Error(fmt.Errorf("reconcile standard progress: %w", err))
	}
	if err := service.ReconcileIncremental(ctx); err != nil {
		return nil, models.redactor.Error(fmt.Errorf("reconcile standard incremental delivery: %w", err))
	}
	models.logger.Info("ADK durable runtime enabled", "session_service", "sqlite")
	if externalJobService != nil {
		activationWorker, err = newExternalAgentActivationWorker(activationStore, service, models.logger, models.metrics, externalSchedules)
		if err != nil {
			return nil, models.redactor.Error(fmt.Errorf("initialize external-agent activation worker: %w", err))
		}
		bindForegroundRuntimes(models, externalJobService)
		go externalJobService.Run(ctx)
	}
	if notificationWorker != nil {
		notificationDone = make(chan struct{})
		go func() {
			defer close(notificationDone)
			notificationWorker.Run(ctx)
		}()
	}
	if activationWorker != nil {
		go activationWorker.Run(ctx)
	}
	return &runtimeComposition{service: service, agentBuilderSvc: agentBuilderSvc, externalJobService: externalJobService, notificationWorker: notificationWorker, activationWorker: activationWorker, knowledgeRetriever: knowledgeRetriever, lexicalWorker: knowledgeLexicalWorker, embeddingWorker: knowledgeEmbeddingWorker, resultAnalysisWorker: resultAnalysisWorker, notificationDone: notificationDone}, nil
}

func (a *Application) startSlackRuntime(intakeCtx, handlerCtx context.Context, setup runtimeSetup, models runtimeModels, infra *runtimeInfrastructure, composition *runtimeComposition) error {
	cfg := setup.cfg
	socket := socketmode.New(infra.api, socketmode.OptionLog(infra.sdkLog))
	listener := slackadapter.NewListener(socket, slackadapter.NewRouter(infra.auth.UserID, cfg.Slack.StandardAgent.ThreadedDM), models.logger).WithAllowedUserIDs(cfg.Slack.AllowedUserIDs)
	if composition != nil && composition.agentBuilderSvc != nil && setup.defs != nil && infra.publisher != nil && infra.store != nil {
		var providerNames []string
		for name := range setup.defs.Providers {
			providerNames = append(providerNames, name)
		}
		sort.Strings(providerNames)
		var allowedProfiles []slackadapter.BuilderProviderProfile
		for _, name := range providerNames {
			provider := setup.defs.Providers[name]
			var profileNames []string
			for profileName := range provider.Profiles {
				profileNames = append(profileNames, profileName)
			}
			sort.Strings(profileNames)
			for _, profileName := range profileNames {
				allowedProfiles = append(allowedProfiles, slackadapter.BuilderProviderProfile{
					Reference:    name + "/" + profileName,
					ProviderType: provider.Type,
				})
			}
		}
		if len(allowedProfiles) > 0 {
			draftStore := adaptersqlite.NewAgentDraftStore(infra.store)
			presenter := slackadapter.NewBuilderModalPresenterWithProviders(allowedProfiles)
			if err := presenter.InitializationError(); err != nil {
				return models.redactor.Error(fmt.Errorf("initialize builder modal template renderer: %w", err))
			}
			handler := slackadapter.NewBuilderSubmissionHandler(draftStore, composition.agentBuilderSvc, setup.defs, infra.publisher)
			listener = listener.WithBuilderPresenter(presenter).WithBuilderHandler(handler)
		}
	}
	dispatcher, dispatcherErr := listener.BuildInteractiveDispatcher()
	if dispatcherErr != nil {
		return models.redactor.Error(fmt.Errorf("initialize Slack interactive dispatcher: %w", dispatcherErr))
	}
	listener = listener.WithDispatcher(dispatcher)
	catalog, catalogErr := slackadapter.EmbeddedTemplateCatalog()
	if catalogErr != nil {
		return models.redactor.Error(fmt.Errorf("validate embedded Slack UI templates: %w", catalogErr))
	}
	if err := listener.ValidateTemplateCatalog(catalog); err != nil {
		return models.redactor.Error(fmt.Errorf("validate Slack interactive dispatcher: %w", err))
	}
	modelName := models.rootModelName
	models.logger.Info("local-agent starting", "agent", models.agentName, "model", modelName, "model_base_url", models.modelBaseURL, "database", setup.paths.DatabaseFile, "allowed_users", len(cfg.Slack.AllowedUserIDs), "allow_all_users", cfg.Slack.AllowAllUsers, "max_concurrent_model_calls", cfg.Runtime.MaxConcurrentModelCalls)
	models.logger.Info("declarative agent definitions loaded", "providers", len(setup.defs.Providers), "agents", len(setup.defs.Agents))
	listener = listener.WithResponsePublisher(infra.publisher)
	listener.SetInteractiveHandler(func(ictx context.Context, action domain.ConfirmationInteractiveAction) error {
		return composition.service.HandleConfirmationInteractive(ictx, action)
	})
	err := listener.RunWithHandlerContext(intakeCtx, handlerCtx, func(eventCtx context.Context, invocation domain.Invocation) {
		if _, handleErr := composition.service.Handle(eventCtx, invocation); handleErr != nil {
			models.logger.Error("Slack invocation processing failed", "event_id", invocation.EventID, "error", handleErr)
		}
	})
	if err != nil {
		return models.redactor.Error(err)
	}
	models.logger.Info("local-agent stopped")
	return nil
}

// wireDeclarativeTools loads .local-agent/tools/, resolves every executable at
// startup, and exposes to the root agent only the tools it declared in
// tool_scope. Child agents and workflow steps may reference the same active
// set through their own declarations. Any reference without a matching
// declaration fails loudly before Slack opens.
func wireDeclarativeTools(factory *toolfactory.Factory, models runtimeModels, paths config.Paths, compositeFactory *compositeAgentToolFactory) error {
	rootDef := models.rootDef
	if factory == nil || rootDef == nil || len(paths.SandboxProjectRoots) == 0 {
		return nil
	}
	loaded, err := tooldef.Load(paths.ToolsDir)
	if err != nil {
		return fmt.Errorf("load declarative tools: %w", err)
	}
	if len(loaded) == 0 {
		return nil
	}
	selected := make(map[string]tooldef.ToolDef)
	for _, scope := range rootDef.ToolScope {
		if scope == "invocation_scoped" {
			continue
		}
		def, ok := loaded[scope]
		if !ok {
			return fmt.Errorf("tool_scope references undeclared tool %q; check %s", scope, paths.ToolsDir)
		}
		if agentdef.IsDirectToolName(scope) || scope == "exit_loop" {
			return fmt.Errorf("declarative tool %q collides with a direct application tool", scope)
		}
		selected[scope] = def
	}
	for _, child := range models.preparedAgentTools {
		for _, scope := range child.definition.ToolScope {
			if scope == "invocation_scoped" || isReadOnlyChildTool(scope) {
				continue
			}
			if _, active := selected[scope]; !active {
				return fmt.Errorf("agent tool %q: tool_scope references undeclared tool %q; add it to root_agent tool_scope or check %s", child.definition.Name, scope, paths.ToolsDir)
			}
		}
	}
	for _, workflow := range models.preparedWorkflows {
		for _, doc := range workflow.blueprint.OrderedDocuments() {
			if doc.LLM == nil {
				continue
			}
			for _, ref := range doc.LLM.Tools {
				if ref.Name == "exit_loop" || isReadOnlyChildTool(ref.Name) {
					continue
				}
				if _, active := selected[ref.Name]; !active {
					return fmt.Errorf("workflow %q agent %q: tool %q is not an active declarative tool; add it to root_agent tool_scope or check %s", workflow.blueprint.ID, doc.Name, ref.Name, paths.ToolsDir)
				}
			}
		}
	}
	if len(selected) == 0 {
		return nil
	}
	executor, err := toolrunner.New(selected, paths.SandboxProjectRoots)
	if err != nil {
		return fmt.Errorf("initialize declarative tool executor: %w", err)
	}
	factory.WithDeclarativeTools(selected, executor)
	if compositeFactory != nil {
		compositeFactory.setDeclarativeTools(selected)
	}
	return nil
}
