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
	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	"github.com/Dauno/slack-local-agent/internal/adapter/memorycurator"
	"github.com/Dauno/slack-local-agent/internal/adapter/memoryprojector"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	"github.com/Dauno/slack-local-agent/internal/adapter/opencodemanager"
	slackadapter "github.com/Dauno/slack-local-agent/internal/adapter/slack"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
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
	apiKey              string
	botToken            string
	appToken            string
	modelBaseURL        string
	redactor            secure.Redactor
	logger              *logging.Logger
	openCodeCoordinator *opencodeusecase.Coordinator
	artifactStore       port.ResultArtifactStore
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
	prepared := runtimeModels{rootFamily: domain.ProviderFamilyOpenAICompatible, openCodeCoordinator: opencodeusecase.NewCoordinator()}
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
	artifactStore, artifactErr := fsartifact.New(paths.ArtifactDir, int64(cfg.ACP.MaxResultArtifactBytes))
	if artifactErr != nil {
		return runtimeModels{}, prepared.redactor.Error(fmt.Errorf("initialize ACP result artifact store: %w", artifactErr))
	}
	prepared.artifactStore = artifactStore

	resolved, err := defs.ResolveModel(prepared.rootDef.Model)
	if err != nil {
		return runtimeModels{}, fmt.Errorf("resolve root agent model: %w", err)
	}
	builtRoot, rootSecret, err := newModelForResolved(ctx, resolved, values, cfg, paths, prepared.logger, prepared.redactor.String)
	if err != nil {
		if setup.legacy {
			return runtimeModels{}, fmt.Errorf("build model client: %w", err)
		}
		return runtimeModels{}, prepared.redactor.Error(fmt.Errorf("build root model client: %w", err))
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
	attachmentProc := adkartifact.NewProcessor(artifactSvc, models.attachmentModel, attachmentInstruction, attachmentTimeout, modelCalls)
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
	service         *botusecase.Service
	agentBuilderSvc port.AgentBuilderService
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
	var err error
	if !models.rootIsAgentCLI {
		var sandboxService *sandboxusecase.Service
		if cfg.Sandbox.Enabled {
			projects := paths.SandboxProjectRoots
			if len(projects) == 0 {
				projects = cfg.Sandbox.Projects
			}
			executor, err := fssandbox.New(projects, cfg.Sandbox.MaxOutputBytes)
			if err != nil {
				return nil, models.redactor.Error(fmt.Errorf("initialize filesystem sandbox: %w", err))
			}
			sandboxService, err = sandboxusecase.New(sandboxusecase.Config{
				AllowedCapabilities: []domain.Capability{domain.CapListRepos, domain.CapListDirectory, domain.CapReadFile, domain.CapListWorktrees},
				CommandTimeout:      time.Duration(cfg.Sandbox.CommandTimeoutSeconds) * time.Second,
				MaxOutputBytes:      cfg.Sandbox.MaxOutputBytes,
			}, sandboxusecase.Dependencies{AuditStore: adaptersqlite.NewSandboxAuditStore(infra.store), Executor: executor})
			if err != nil {
				return nil, models.redactor.Error(fmt.Errorf("initialize sandbox service: %w", err))
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
		factory := toolfactory.New(infra.store, sandboxService, canvasService, generatedFileService).WithAllowedUserIDs(cfg.Slack.AllowedUserIDs)
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
				slackadapter.NewBuilderLauncherPublisher(infra.api, infra.publisher, models.logger),
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
		if compositeFactory != nil {
			compositeFactory.setJobStarter(externalJobService)
		}
		bindForegroundRuntimes(models, externalJobService)
		if externalJobService != nil {
			go externalJobService.Run(ctx)
		}
		if notificationWorker != nil {
			go notificationWorker.Run(ctx)
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

	rtInstruction, rtGlobalInstruction := "", ""
	if models.rootDef != nil {
		rtInstruction, rtGlobalInstruction = models.rootDef.Instruction, models.rootDef.EffectiveRootGlobalInstruction()
	}
	runtime, err := adkagent.NewRuntime(adkagent.RuntimeConfig{AgentName: models.agentName, Instruction: rtInstruction, GlobalInstruction: rtGlobalInstruction, SessionService: infra.sessionSvc, Model: models.rootModel, ToolFactory: toolFactory, ProviderFamily: models.rootFamily})
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
	}, botusecase.Dependencies{Store: infra.store, Runtime: runtime, History: infra.history, Publisher: infra.publisher, Logger: models.logger, Exchange: infra.store, ModelCalls: infra.modelCalls, SanitizeContent: models.redactor.String, Enricher: infra.contextEnricher, ConfirmationStore: confirmationStore, ConfirmationPublisher: infra.confirmationPublisher, StructuredPublisher: infra.blockPublisher, FileLoader: infra.fileLoader, AttachmentProc: infra.attachmentProc, MaxAttachmentBytes: int64(cfg.Slack.Files.MaxBytesPerFile), MaxAttachmentChars: cfg.Slack.Files.MaxProcessedChars, StandardStore: infra.store, ProgressPublisher: infra.standardPublisher, PromptPublisher: infra.standardPublisher, StreamingRuntime: runtime, IncrementalPublisher: infra.standardPublisher})
	if err != nil {
		return nil, err
	}
	if err := confirmationStore.ExpireDeliveries(ctx, time.Now().UTC()); err != nil {
		models.logger.Warn("confirmation delivery expiry failed", "error", err)
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
	return &runtimeComposition{service: service, agentBuilderSvc: agentBuilderSvc}, nil
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

func (a *Application) startSlackRuntime(ctx context.Context, setup runtimeSetup, models runtimeModels, infra *runtimeInfrastructure, composition *runtimeComposition) error {
	cfg := setup.cfg
	socket := socketmode.New(infra.api, socketmode.OptionLog(infra.sdkLog))
	listener := slackadapter.NewListener(socket, slackadapter.NewRouter(infra.auth.UserID, cfg.Slack.StandardAgent.ThreadedDM), models.logger).WithAllowedUserIDs(cfg.Slack.AllowedUserIDs)
	if composition != nil && composition.agentBuilderSvc != nil && setup.defs != nil && infra.publisher != nil && infra.store != nil {
		var allowedProfiles []string
		for name, provider := range setup.defs.Providers {
			if provider.Type != agentdef.ProviderTypeOpenAICompatible {
				continue
			}
			for profileName := range provider.Profiles {
				allowedProfiles = append(allowedProfiles, name+"/"+profileName)
			}
		}
		if len(allowedProfiles) > 0 {
			sort.Strings(allowedProfiles)
			draftStore := adaptersqlite.NewAgentDraftStore(infra.store)
			presenter := slackadapter.NewBuilderModalPresenter(allowedProfiles)
			handler := slackadapter.NewBuilderSubmissionHandler(draftStore, composition.agentBuilderSvc, setup.defs, infra.publisher)
			listener = listener.WithBuilderPresenter(presenter).WithBuilderHandler(handler)
		}
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
	listener.SetInteractiveHandler(func(ictx context.Context, action domain.ConfirmationInteractiveAction) error {
		return composition.service.HandleConfirmationInteractive(ictx, action)
	})
	err := listener.Run(ctx, func(eventCtx context.Context, invocation domain.Invocation) {
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
