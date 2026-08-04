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
	"github.com/Dauno/slack-local-agent/internal/adapter/memorycurator"
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
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
	"github.com/Dauno/slack-local-agent/internal/usecase/agentbuilder"
	"github.com/Dauno/slack-local-agent/internal/usecase/bootstrap"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
	canvasusecase "github.com/Dauno/slack-local-agent/internal/usecase/canvas"
	contextcompilerusecase "github.com/Dauno/slack-local-agent/internal/usecase/contextcompiler"
	contextsummary "github.com/Dauno/slack-local-agent/internal/usecase/contextsummary"
	externalagentusecase "github.com/Dauno/slack-local-agent/internal/usecase/externalagent"
	generatedfileusecase "github.com/Dauno/slack-local-agent/internal/usecase/generatedfile"
	memoryusecase "github.com/Dauno/slack-local-agent/internal/usecase/memory"
	opencodeusecase "github.com/Dauno/slack-local-agent/internal/usecase/opencode"
	sandboxusecase "github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
)

const defaultAttachmentTimeout = 120 * time.Second

type runtimeSetup struct {
	cfg    config.Config
	paths  config.Paths
	defs   *agentdef.Definitions
	legacy bool
}

type runtimeModels struct {
	rootModel           model.LLM
	rootFamily          string
	rootIsAgentCLI      bool
	preparedAgentTools  []preparedAgentTool
	preparedWorkflows   []preparedWorkflowTool
	curatorLLM          memorycurator.LLM
	agentName           string
	rootDef             *agentdef.AgentDef
	curatorDef          *agentdef.AgentDef
	attachmentDef       *agentdef.AgentDef
	attachmentModel     model.LLM
	transcriber         port.AudioTranscriber
	apiKey              string
	botToken            string
	appToken            string
	modelBaseURL        string
	redactor            secure.Redactor
	logger              *logging.Logger
	openCodeCoordinator *opencodeusecase.Coordinator
	artifactStore       port.ResultArtifactStore
	requestTokenCounter port.RequestTokenCounter
	contextWindowTokens int
	metrics             port.MetricRecorder
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
	legacy := defs == nil
	if legacy {
		defs = agentdef.NormalizeLegacy(cfg.Agent.Name, cfg.Model.Name, cfg.Model.BaseURL, cfg.Model.APIKeyEnv, cfg.Model.ReasoningEffort, cfg.Model.Headers, cfg.Model.ExtraBody)
	}
	return runtimeSetup{cfg: cfg, paths: paths, defs: defs, legacy: legacy}, nil
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
	if cur, ok := defs.Agents["memory_curator"]; ok {
		prepared.curatorDef = &cur
	}
	if attachment, ok := defs.Agents["attachment_analyzer"]; ok {
		prepared.attachmentDef = &attachment
	}

	providerEnvs := defs.RequiredAPIKeyEnvs()
	allKeys := append(append(make([]string, 0, len(providerEnvs)+2), providerEnvs...), bootstrap.SlackBotTokenEnv, bootstrap.SlackAppTokenEnv)
	values, err := envfile.NewResolver(paths.EnvFile).Resolve(allKeys...)
	if err != nil {
		return runtimeModels{}, fmt.Errorf("load runtime secrets: %w", err)
	}
	prepared.botToken = values[bootstrap.SlackBotTokenEnv]
	prepared.appToken = values[bootstrap.SlackAppTokenEnv]
	if setup.legacy && strings.TrimSpace(values[cfg.Model.APIKeyEnv]) == "" {
		return runtimeModels{}, fmt.Errorf("%s is not configured. Run: local-agent init", cfg.Model.APIKeyEnv)
	}
	if err := requiredSlackTokens(prepared.botToken, prepared.appToken); err != nil {
		return runtimeModels{}, err
	}
	secrets := make([]string, 0, len(providerEnvs)+2)
	for _, environment := range providerEnvs {
		secrets = append(secrets, values[environment])
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

	resolved, err := defs.ResolveModel(prepared.rootDef.Model)
	if err != nil {
		return runtimeModels{}, fmt.Errorf("resolve root agent model: %w", err)
	}
	prepared.requestTokenCounter, err = composeRootTokenCounter(resolved)
	if err != nil {
		return runtimeModels{}, err
	}
	prepared.contextWindowTokens = resolved.ContextWindowTokens
	if err := config.ValidateADKCompaction(cfg, resolved.Type() == agentdef.ProviderTypeOpenAICompatible, resolved.Type() == agentdef.ProviderTypeOpenAICompatible); err != nil {
		return runtimeModels{}, err
	}
	builtRoot, rootSecret, err := newModelForResolved(ctx, resolved, values, cfg, paths, prepared.logger, prepared.redactor.String)
	if err != nil {
		if setup.legacy {
			return runtimeModels{}, fmt.Errorf("build model client: %w", err)
		}
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
		}, prepared.artifactStore)
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

	if cfg.Memory.Enabled && prepared.curatorDef == nil && !setup.legacy {
		return runtimeModels{}, errors.New("agent definition memory_curator is required when memory is enabled")
	}
	if cfg.Memory.Enabled && prepared.curatorDef != nil {
		curatorResolved, err := defs.ResolveModel(prepared.curatorDef.Model)
		if err != nil {
			return runtimeModels{}, fmt.Errorf("resolve curator model: %w", err)
		}
		curatorModel, _, err := newModelForResolved(ctx, curatorResolved, values, cfg, paths, prepared.logger, prepared.redactor.String)
		if err != nil {
			return runtimeModels{}, prepared.redactor.Error(fmt.Errorf("build curator model client: %w", err))
		}
		if err := handshakeSelectedAgentCLI(ctx, curatorResolved, curatorModel, describedCLIProviders); err != nil {
			return runtimeModels{}, prepared.redactor.Error(fmt.Errorf("validate curator model: %w", err))
		}
		prepared.curatorLLM = &memoryCuratorLLM{llm: curatorModel, generateContentConfig: curatorResolved.GenerateContentConfig, logger: prepared.logger, sanitize: prepared.redactor.String}
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

func (a *Application) openRuntimeInfrastructure(ctx context.Context, setup runtimeSetup, models runtimeModels) (*runtimeInfrastructure, error) {
	cfg, paths := setup.cfg, setup.paths
	store, err := adaptersqlite.OpenExisting(ctx, paths.DatabaseFile)
	if err != nil {
		if errors.Is(err, adaptersqlite.ErrDatabaseNotFound) {
			return nil, errors.New("Local state not found. Run: local-agent init")
		}
		if errors.Is(err, adaptersqlite.ErrFutureSchema) {
			return nil, models.redactor.Error(fmt.Errorf("%w. Install a local-agent version that supports this database or back up and remove only the configured database file", err))
		}
		if errors.Is(err, adaptersqlite.ErrStateResetNeeded) {
			return nil, models.redactor.Error(fmt.Errorf("%w. Run: local-agent init --reset-state", err))
		}
		return nil, models.redactor.Error(fmt.Errorf("open runtime database: %w", err))
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
	if cfg.Memory.Enabled {
		if err := store.CleanupOutbox(ctx, time.Now().UTC().AddDate(0, 0, -cfg.Memory.RetentionDays)); err != nil {
			return nil, models.redactor.Error(err)
		}
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
	standardPublisher := slackadapter.NewStandardPublisher(api, auth.UserID, slackTimeout)
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
	}
	ok = true
	return infra, nil
}

type runtimeComposition struct {
	service            *botusecase.Service
	agentBuilderSvc    port.AgentBuilderService
	externalJobService *externalagentusecase.Service
	notificationWorker *externalagentusecase.NotificationWorker
	activationWorker   *externalagentusecase.ActivationWorker
	notificationDone   chan struct{}
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
	var notificationDone chan struct{}
	var summaryScheduler port.SummaryScheduler
	var continuityStore port.ContinuityStore
	var resultStore *recoverableresult.Store
	features := cfg.Context.ContextFeatures
	var err error
	if !models.rootIsAgentCLI && features != nil && features.ContinuityCapsuleEnabled {
		continuityStore = adaptersqlite.NewContinuityStore(infra.store)
	}
	if !models.rootIsAgentCLI && features != nil && features.RecoverableResultsEnabled {
		resultsCfg := cfg.Context.RecoverableResults
		resultStore = recoverableresult.NewStore(infra.store.DB(), filepath.Join(paths.StateDir, "recoverable-results"), resultsCfg.MaxResultBytes, resultsCfg.ChunkMaxBytes, resultsCfg.RetentionDays, resultsCfg.CleanupBatchSize, models.metrics)
		resultStore.SetReferenceChecker(infra.store)
		if _, cleanupErr := resultStore.DeleteExpired(ctx, time.Now().UTC(), resultsCfg.CleanupBatchSize); cleanupErr != nil {
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
		factory := toolfactory.New(infra.store, sandboxService, canvasService, generatedFileService).WithAllowedUserIDs(cfg.Slack.AllowedUserIDs).WithRecoverableResults(resultStore).WithCodeReaders(codeReaders).WithSyntaxEngine(syntaxEngine).WithCodeIntelligence(codeIntelligence).WithMetrics(models.metrics)
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
			toolFactory = compositeFactory
		}
		externalJobService, notificationWorker, err = newExternalAgentJobService(cfg, models, infra)
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
			SummaryMaxChars: compaction.SummaryMaxChars,
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
	runtime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{AgentName: models.agentName, Instruction: rtInstruction, GlobalInstruction: rtGlobalInstruction, SessionService: infra.sessionSvc, Model: models.rootModel, ToolFactory: toolFactory, ContextProjector: projector, ContextCompiler: compiler, ContextBudget: contextBudget, ContextCompaction: contextCompaction, ContinuityStore: continuityStore, SummaryStore: infra.store, Metrics: models.metrics, ProviderFamily: models.rootFamily})
	if err != nil {
		return nil, models.redactor.Error(fmt.Errorf("initialize ADK runtime: %w", err))
	}
	confirmationStore := adaptersqlite.NewConfirmationStore(infra.store)
	service, err := botusecase.New(botusecase.Config{
		AccessPolicy:   domain.AccessPolicy{AllowAllUsers: cfg.Slack.AllowAllUsers, AllowedUserIDs: cfg.Slack.AllowedUserIDs, AllowedTeamIDs: cfg.Slack.AllowedTeamIDs, AllowedChannelIDs: cfg.Slack.AllowedChannelIDs},
		ContextLimits:  domain.ContextLimits{MaxMessages: cfg.Context.MaxMessages, MaxChars: cfg.Context.MaxChars},
		RetainMessages: cfg.Context.RetainMessagesPerConversation, MaxConcurrentCalls: cfg.Runtime.MaxConcurrentModelCalls,
		ModelTimeout: time.Duration(cfg.Runtime.ModelTimeoutSeconds) * time.Second, BusyMessage: cfg.Runtime.BusyMessage, ModelErrorMessage: cfg.Runtime.ModelErrorMessage, UnauthorizedMessage: cfg.Slack.UnauthorizedMessage,
		ProgressEnabled: cfg.Slack.StandardAgent.ProgressEnabled, PromptsEnabled: cfg.Slack.StandardAgent.PromptsEnabled, SuggestedPrompts: cfg.Slack.StandardAgent.SuggestedPrompts, StreamingEnabled: cfg.Slack.StandardAgent.StreamingEnabled, UpdateInterval: time.Duration(cfg.Slack.StandardAgent.UpdateIntervalSeconds) * time.Second, StreamingCarryRunes: models.redactor.StreamingCarryRunes(),
	}, botusecase.Dependencies{Store: infra.store, Runtime: runtime, ActivationStore: activationStore, History: infra.history, Publisher: infra.publisher, Logger: models.logger, Exchange: infra.store, ModelCalls: infra.modelCalls, SanitizeContent: models.redactor.String, Enricher: infra.contextEnricher, ConfirmationStore: confirmationStore, ConfirmationPublisher: infra.confirmationPublisher, StructuredPublisher: infra.blockPublisher, FileLoader: infra.fileLoader, AttachmentProc: infra.attachmentProc, MaxAttachmentBytes: int64(cfg.Slack.Files.MaxBytesPerFile), MaxAttachmentChars: cfg.Slack.Files.MaxProcessedChars, StandardStore: infra.store, OnboardingStore: infra.store, ProgressPublisher: infra.standardPublisher, PromptPublisher: infra.standardPublisher, OnboardingPublisher: infra.standardPublisher, StreamingRuntime: runtime, IncrementalPublisher: infra.standardPublisher, SummaryScheduler: summaryScheduler})
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
	if cfg.Memory.Enabled {
		if err := a.startMemoryCurator(ctx, setup, models, infra, service); err != nil {
			return nil, err
		}
	}
	if externalJobService != nil {
		activationWorker, err = externalagentusecase.NewActivationWorker(externalagentusecase.ActivationConfig{
			PollInterval: time.Second, LeaseTTL: 30 * time.Second, StuckThreshold: 5 * time.Minute,
		}, externalagentusecase.ActivationDependencies{
			Store: activationStore, Handler: service, Logger: models.logger, Metrics: models.metrics,
		})
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
	return &runtimeComposition{service: service, agentBuilderSvc: agentBuilderSvc, externalJobService: externalJobService, notificationWorker: notificationWorker, activationWorker: activationWorker, notificationDone: notificationDone}, nil
}

func (a *Application) startMemoryCurator(ctx context.Context, setup runtimeSetup, models runtimeModels, infra *runtimeInfrastructure, service *botusecase.Service) error {
	cfg, paths := setup.cfg, setup.paths
	memorySvc, memErr := memoryusecase.New(memoryusecase.Config{Recall: domain.MemoryRecallConfig{Enabled: true, MaxTopics: cfg.Memory.MaxTopicsRecall, MaxChars: cfg.Memory.MaxCharsRecall, Timeout: time.Duration(cfg.Memory.RecallTimeoutSeconds) * time.Second}, Limits: domain.MemoryLimits{MaxTopics: cfg.Memory.MaxTopics, MaxLinks: cfg.Memory.MaxLinks, MaxTopicChars: cfg.Memory.MaxTopicChars}, MaxPatchOps: cfg.Memory.MaxPatchOps}, memoryusecase.Dependencies{Store: infra.store, Logger: models.logger, SanitizeContent: models.redactor.String})
	if memErr != nil {
		return models.redactor.Error(fmt.Errorf("initialize memory service: %w", memErr))
	}
	curTimeout := time.Duration(cfg.Memory.CuratorTimeoutSeconds) * time.Second
	curatorInstruction := ""
	if models.curatorDef != nil {
		curatorInstruction = models.curatorDef.Instruction
		if models.curatorDef.TimeoutSeconds > 0 {
			curTimeout = time.Duration(models.curatorDef.TimeoutSeconds) * time.Second
		}
	}
	if models.curatorLLM == nil {
		models.curatorLLM = &memoryCuratorLLM{llm: models.rootModel, logger: models.logger, sanitize: models.redactor.String}
	}
	curator, curErr := memorycurator.New(models.curatorLLM, memorycurator.Config{Timeout: curTimeout, ModelCalls: infra.modelCalls, Instruction: curatorInstruction})
	if curErr != nil {
		return models.redactor.Error(fmt.Errorf("initialize memory curator: %w", curErr))
	}
	runner, runnerErr := memoryusecase.NewRunner(memoryusecase.RunnerConfig{
		Interval:      time.Duration(cfg.Memory.WorkerIntervalSeconds) * time.Second,
		MaxRetries:    cfg.Memory.CuratorMaxRetries,
		RetentionDays: cfg.Memory.RetentionDays,
		MemoryDir:     paths.MemoryDir,
	}, memoryusecase.RunnerDependencies{
		Store: infra.store, ExchangeFinder: infra.history, Curator: curator, Memory: memorySvc,
		Projector: memoryprojector.New(), ProjectionReader: infra.store,
		Logger: models.logger, Sanitize: models.redactor.String,
	})
	if runnerErr != nil {
		return models.redactor.Error(fmt.Errorf("initialize memory runner: %w", runnerErr))
	}
	go runner.Run(ctx)
	service.AddMemory(memorySvc, infra.store)
	models.logger.Info("memory service enabled", "directory", paths.MemoryDir, "max_topics_recall", cfg.Memory.MaxTopicsRecall, "max_chars_recall", cfg.Memory.MaxCharsRecall)
	return nil
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
	modelName := cfg.Model.Name
	if models.rootDef != nil {
		resolved, _ := setup.defs.ResolveModel(models.rootDef.Model)
		if resolved != nil {
			modelName = resolved.Model
		}
	}
	models.logger.Info("local-agent starting", "agent", models.agentName, "model", modelName, "model_base_url", models.modelBaseURL, "database", setup.paths.DatabaseFile, "allowed_users", len(cfg.Slack.AllowedUserIDs), "allow_all_users", cfg.Slack.AllowAllUsers, "max_concurrent_model_calls", cfg.Runtime.MaxConcurrentModelCalls)
	if setup.legacy {
		models.logger.Info("using legacy config.Model; migrate to .local-agent/providers/ and .local-agent/agents/ for declarative model configuration")
	} else {
		models.logger.Info("declarative agent definitions loaded", "providers", len(setup.defs.Providers), "agents", len(setup.defs.Agents))
	}
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
