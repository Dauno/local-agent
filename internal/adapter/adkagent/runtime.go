package adkagent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// RuntimeConfig holds the dependencies for a durable ADK agent runtime.
type RuntimeConfig struct {
	AgentName         string
	Instruction       string
	GlobalInstruction string
	SessionService    session.Service
	Model             model.LLM
	ToolFactory       port.AgentToolFactory
	ContextProjector  port.ContextProjector
	ContextCompiler   port.ContextCompiler
	ContextBudget     domain.RequestBudget
	ContextCompaction domain.ContextCompactionSettings
	ContinuityStore   port.ContinuityStore
	SummaryStore      port.SummaryStore
	EpochStore        port.ContextEpochStore
	Metrics           port.MetricRecorder
	// Result-producing ACP tools are limited per model step. The input frame
	// reserves their bounded result capacity before each provider call.
	ResultProducingToolNames         []string
	ResultProducingCallsPerStep      int
	ResultProducingCallReserveTokens int
	// StaticTools are reusable ADK tools composed at startup, such as AgentTool
	// wrappers. Invocation-scoped tools continue to come from ToolFactory.
	StaticTools []tool.Tool
	// ProviderFamily is stamped onto new durable sessions and compared
	// defensively before each turn. Empty defaults to openai_compatible.
	ProviderFamily string
	// KnowledgeBudgetTokens is the validated per-turn source budget for
	// knowledge frame cards from orchestration.knowledge.max_card_tokens.
	// The turn payload never controls this budget; zero disables selection.
	KnowledgeBudgetTokens int
	// WorkstreamBudgetTokens is the validated per-turn source budget for the
	// active workstream snapshot from
	// orchestration.workstreams.snapshot_budget_tokens. The turn payload
	// never controls this budget; zero disables selection.
	WorkstreamBudgetTokens int
}

// Runtime adapts ADK's llmagent + durable session service into the
// application's port.AgentRuntime boundary.
type Runtime struct {
	agentName                        string
	instruction                      string
	globalInstruction                string
	sessionService                   session.Service
	model                            model.LLM
	toolFactory                      port.AgentToolFactory
	contextProjector                 port.ContextProjector
	contextCompiler                  port.ContextCompiler
	contextBudget                    domain.RequestBudget
	contextCompaction                domain.ContextCompactionSettings
	continuityStore                  port.ContinuityStore
	summaryStore                     port.SummaryStore
	epochStore                       port.ContextEpochStore
	staticTools                      []tool.Tool
	providerFamily                   string
	metrics                          port.MetricRecorder
	resultProducingToolNames         map[string]struct{}
	resultProducingCallsPerStep      int
	resultProducingCallReserveTokens int
	knowledgeBudgetTokens            int
	workstreamBudgetTokens           int
}

var _ port.AgentRuntime = (*Runtime)(nil)
var _ port.AgentActivationRecovery = (*Runtime)(nil)
var _ port.StreamingAgentRuntime = (*Runtime)(nil)

// NewRuntime creates an ADK-backed agent runtime.
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	if strings.TrimSpace(cfg.AgentName) == "" {
		return nil, errors.New("agent name is required")
	}
	if strings.ContainsAny(cfg.AgentName, "\r\n\x00") {
		return nil, errors.New("agent name must be a single line")
	}
	if cfg.Model == nil {
		return nil, errors.New("ADK model is required")
	}
	if cfg.SessionService == nil {
		return nil, errors.New("session service is required")
	}
	resultProducingTools := make(map[string]struct{}, len(cfg.ResultProducingToolNames))
	for _, name := range cfg.ResultProducingToolNames {
		if name = strings.TrimSpace(name); name != "" {
			resultProducingTools[name] = struct{}{}
		}
	}
	if len(resultProducingTools) > 0 {
		if cfg.ResultProducingCallsPerStep != 1 || cfg.ResultProducingCallReserveTokens <= 0 {
			return nil, errors.New("result-producing tool reservation requires one call and a positive token reserve")
		}
		if cfg.ContextCompiler == nil {
			return nil, errors.New("result-producing tool reservation requires an admissible context compiler budget")
		}
		if _, err := reserveResultCallTokens(cfg.ContextBudget, cfg.ResultProducingCallReserveTokens); err != nil {
			return nil, fmt.Errorf("result-producing tool reservation requires an admissible context compiler budget: %w", err)
		}
	}
	providerFamily := cfg.ProviderFamily
	if providerFamily == "" {
		providerFamily = domain.ProviderFamilyOpenAICompatible
	}
	return &Runtime{
		agentName:                        cfg.AgentName,
		instruction:                      cfg.Instruction,
		globalInstruction:                cfg.GlobalInstruction,
		sessionService:                   cfg.SessionService,
		model:                            cfg.Model,
		toolFactory:                      cfg.ToolFactory,
		contextProjector:                 cfg.ContextProjector,
		contextCompiler:                  cfg.ContextCompiler,
		contextBudget:                    cfg.ContextBudget,
		contextCompaction:                cfg.ContextCompaction,
		continuityStore:                  cfg.ContinuityStore,
		summaryStore:                     cfg.SummaryStore,
		epochStore:                       cfg.EpochStore,
		metrics:                          cfg.Metrics,
		staticTools:                      append([]tool.Tool(nil), cfg.StaticTools...),
		providerFamily:                   providerFamily,
		resultProducingToolNames:         resultProducingTools,
		resultProducingCallsPerStep:      cfg.ResultProducingCallsPerStep,
		resultProducingCallReserveTokens: cfg.ResultProducingCallReserveTokens,
		knowledgeBudgetTokens:            cfg.KnowledgeBudgetTokens,
		workstreamBudgetTokens:           cfg.WorkstreamBudgetTokens,
	}, nil
}

// adkSessionID derives a deterministic ADK session ID from a conversation key.
func adkSessionID(key domain.ConversationKey) string {
	return "adk:" + string(key)
}

func adkActivationSessionID(activationID string) string {
	return "adk:activation:" + activationID
}

func sessionIDForOrigin(key domain.ConversationKey, origin port.AgentTurnOrigin) string {
	if origin.Kind == port.AgentTurnOriginJobCompletion {
		return adkActivationSessionID(origin.ActivationID)
	}
	return adkSessionID(key)
}

// buildAgent constructs a per-turn llmagent with tools and before-model callback.
func (r *Runtime) buildAgent(tools []tool.Tool, ephemeral beforeModelData, stream bool) (agent.Agent, error) {
	instruction := r.instruction
	if instruction == "" {
		instruction = BaseInstruction(r.agentName)
		if len(tools) > 0 {
			instruction += " You may use only the registered function tools when they are relevant. Their arguments and results remain subject to application policy."
		}
	}
	instruction = instructionForOrigin(instruction, ephemeral.origin)

	agentCfg := llmagent.Config{
		Name:              technicalName,
		Description:       "Slack conversational assistant with tools",
		Model:             r.model,
		Mode:              llmagent.ModeChat,
		IncludeContents:   includeContentsForOrigin(ephemeral.origin),
		GlobalInstruction: r.globalInstruction,
		InstructionProvider: func(agent.ReadonlyContext) (string, error) {
			return instruction, nil
		},
	}
	reservation := newResultCallReservation(tools, r.resultProducingToolNames, r.resultProducingCallsPerStep)
	budget := r.contextBudget
	if reservation != nil {
		var err error
		budget, err = reserveResultCallTokens(budget, r.resultProducingCallReserveTokens)
		if err != nil {
			return nil, err
		}
		agentCfg.BeforeModelCallbacks = append(agentCfg.BeforeModelCallbacks, func(agent.Context, *model.LLMRequest) (*model.LLMResponse, error) {
			reservation.Reset()
			return nil, nil
		})
		agentCfg.AfterModelCallbacks = append(agentCfg.AfterModelCallbacks, reservation.AfterModel)
		agentCfg.BeforeToolCallbacks = append(agentCfg.BeforeToolCallbacks, reservation.BeforeTool)
	}

	if len(tools) > 0 {
		agentCfg.Tools = tools
	}
	if r.contextProjector != nil || r.contextCompiler != nil || ephemeral.reference() != "" {
		if r.contextProjector != nil && r.contextCompiler == nil {
			agentCfg.BeforeModelCallbacks = append(agentCfg.BeforeModelCallbacks, BeforeModelCallback(r.contextProjector))
		}
		if reference := ephemeral.reference(); reference != "" {
			agentCfg.BeforeModelCallbacks = append(agentCfg.BeforeModelCallbacks, injectEphemeralReference(reference))
		}
		if r.contextCompiler != nil {
			recorder := &epochRecorder{}
			agentCfg.BeforeModelCallbacks = append(agentCfg.BeforeModelCallbacks, CompilerBeforeModelCallback(CompilerCallbackConfig{
				Compiler: r.contextCompiler, RequestModel: r.model, Stream: stream, Budget: budget,
				Continuity: r.continuityStore, Summaries: r.summaryStore, Compaction: r.contextCompaction, Actor: ephemeral.actor,
				Knowledge:              ephemeral.knowledge,
				KnowledgeBudgetTokens:  r.knowledgeBudgetTokens,
				Workstream:             ephemeral.workstreamSnapshot,
				WorkstreamBudgetTokens: r.workstreamBudgetTokens,
				WorkstreamRevision:     ephemeral.workstreamRevision,
				EpochSink:              recorder,
			}))
			if r.epochStore != nil {
				agentCfg.BeforeModelCallbacks = append(agentCfg.BeforeModelCallbacks, recorder.capture)
				agentCfg.AfterModelCallbacks = append(agentCfg.AfterModelCallbacks, r.recordEpoch(recorder))
			}
		}
	}
	if ephemeral.beforeModel != nil {
		var once sync.Once
		var callbackErr error
		agentCfg.BeforeModelCallbacks = append(agentCfg.BeforeModelCallbacks, func(ctx agent.Context, _ *model.LLMRequest) (*model.LLMResponse, error) {
			once.Do(func() { callbackErr = ephemeral.beforeModel(ctx) })
			return nil, callbackErr
		})
	}

	return llmagent.New(agentCfg)
}

type epochEventHeadReader interface {
	LatestEventOrdinal(context.Context, string, string, string) (int64, error)
}

type epochRecorder struct {
	mu                  sync.Mutex
	sourceDigest        string
	codePoints          int
	knowledgeIdentities []string
	selectedCount       int
	omittedCount        int
	workstreamRevision  int64
	summaryIdentity     string
	resultIdentities    []string
	frameTokens         int
	compiled            bool
}

// setCompileFacts receives the final CompileResult of the same model step
// from the compiler callback. Only content-free facts are retained: sorted
// identities, bounded counts, the host-trusted workstream revision, and the
// final provider count. Cards, queries, and rendered knowledge never reach
// the recorder. summaryIdentity is already the durable source-digest/ordinal
// form (never text), and resultIdentities are already sorted, deduplicated,
// and bounded.
func (r *epochRecorder) setCompileFacts(result domain.CompileResult, workstreamRevision int64, summaryIdentity string, resultIdentities []string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.knowledgeIdentities = append([]string(nil), result.KnowledgeIdentities...)
	r.selectedCount = result.KnowledgeSelectedCount
	r.omittedCount = result.KnowledgeOmittedCount
	r.workstreamRevision = workstreamRevision
	r.summaryIdentity = summaryIdentity
	r.resultIdentities = append([]string(nil), resultIdentities...)
	r.frameTokens = result.Diagnostics.RequestTokensAfter
	r.compiled = true
}

func (r *epochRecorder) capture(_ agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
	if request == nil {
		return nil, errors.New("epoch frame request is required")
	}
	contents, err := toDomainContents(request.Contents)
	if err != nil {
		return nil, err
	}
	encoded, err := domain.CanonicalJSON(contents)
	if err != nil {
		return nil, err
	}
	codePoints, err := domain.ContentCost(contents)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	r.mu.Lock()
	r.sourceDigest = fmt.Sprintf("%x", digest)
	r.codePoints = codePoints
	r.mu.Unlock()
	return nil, nil
}

func (r *Runtime) recordEpoch(recorder *epochRecorder) llmagent.AfterModelCallback {
	return func(ctx agent.Context, response *model.LLMResponse, responseErr error) (*model.LLMResponse, error) {
		if responseErr != nil || response == nil || response.Partial || ctx == nil {
			return nil, nil
		}
		headReader, ok := r.sessionService.(epochEventHeadReader)
		if !ok {
			return nil, nil
		}
		recorder.mu.Lock()
		digest, codePoints := recorder.sourceDigest, recorder.codePoints
		identities := append([]string(nil), recorder.knowledgeIdentities...)
		selected, omitted := recorder.selectedCount, recorder.omittedCount
		workstreamRevision, frameTokens, compiled := recorder.workstreamRevision, recorder.frameTokens, recorder.compiled
		summaryIdentity := recorder.summaryIdentity
		resultIdentities := append([]string(nil), recorder.resultIdentities...)
		recorder.mu.Unlock()
		if digest == "" {
			return nil, errors.New("epoch frame was not captured")
		}
		head, err := headReader.LatestEventOrdinal(ctx, applicationName, ephemeralUserID, ctx.SessionID())
		if err != nil {
			return nil, fmt.Errorf("read epoch event head: %w", err)
		}
		latest, err := r.epochStore.Latest(ctx, applicationName, ephemeralUserID, ctx.SessionID())
		expected, number := int64(0), int64(1)
		if err == nil {
			expected, number = latest.EpochNumber, latest.EpochNumber+1
		} else if !errors.Is(err, port.ErrContextEpochNotFound) {
			return nil, fmt.Errorf("read epoch head: %w", err)
		}
		idDigest := sha256.Sum256([]byte(ctx.SessionID() + "\x00" + strconv.FormatInt(number, 10) + "\x00" + digest))
		epoch := domain.ContextEpoch{EpochID: "epoch-" + fmt.Sprintf("%x", idDigest[:16]), AppName: applicationName, UserID: ephemeralUserID, SessionID: ctx.SessionID(), EpochNumber: number, CoveredThroughOrdinal: head, CompilerVersion: "context-compiler-v1", CounterVersion: "provider-shaped-v1", SourceDigest: digest, FrameCodePoints: codePoints, Reason: "model_step", CreatedAt: time.Now().UTC()}
		if compiled {
			epoch.WorkstreamRevision = workstreamRevision
			epoch.KnowledgeIdentities = identities
			epoch.SummaryIdentity = summaryIdentity
			epoch.ResultIdentities = resultIdentities
			epoch.SelectedSourceCount = selected
			epoch.OmittedSourceCount = omitted
			epoch.FrameTokens = frameTokens
		}
		if err := r.epochStore.Append(ctx, epoch, expected); err != nil {
			return nil, fmt.Errorf("append context epoch: %w", err)
		}
		return nil, nil
	}
}

type resultCallReservation struct {
	producerNames map[string]struct{}
	limit         int
	mu            sync.Mutex
	calls         int
	allowedCallID string
}

func newResultCallReservation(tools []tool.Tool, producerNames map[string]struct{}, limit int) *resultCallReservation {
	if len(producerNames) == 0 || limit <= 0 {
		return nil
	}
	for _, candidate := range tools {
		if candidate != nil {
			if _, ok := producerNames[candidate.Name()]; ok {
				return &resultCallReservation{producerNames: producerNames, limit: limit}
			}
		}
	}
	return nil
}

func (r *resultCallReservation) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.calls = 0
	r.allowedCallID = ""
	r.mu.Unlock()
}

func (r *resultCallReservation) AfterModel(_ agent.Context, response *model.LLMResponse, responseErr error) (*model.LLMResponse, error) {
	if r == nil || responseErr != nil || response == nil || response.Content == nil {
		return nil, nil
	}
	allowedCallID := ""
	for _, part := range response.Content.Parts {
		if part == nil || part.FunctionCall == nil {
			continue
		}
		if _, producer := r.producerNames[part.FunctionCall.Name]; producer {
			allowedCallID = part.FunctionCall.ID
			break
		}
	}
	r.mu.Lock()
	r.allowedCallID = allowedCallID
	r.mu.Unlock()
	return nil, nil
}

func (r *resultCallReservation) BeforeTool(ctx agent.Context, candidate tool.Tool, _ map[string]any) (map[string]any, error) {
	if r == nil || candidate == nil {
		return nil, nil
	}
	if _, ok := r.producerNames[candidate.Name()]; !ok {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.allowedCallID != "" && (ctx == nil || ctx.FunctionCallID() != r.allowedCallID) {
		return nil, errors.New("result-producing call limit reached for model step")
	}
	if r.calls >= r.limit {
		return nil, errors.New("result-producing call limit reached for model step")
	}
	r.calls++
	return nil, nil
}

func reserveResultCallTokens(budget domain.RequestBudget, reserve int) (domain.RequestBudget, error) {
	if reserve <= 0 || budget.HardTokens <= reserve {
		return domain.RequestBudget{}, errors.New("result-producing call reserve exceeds the context budget")
	}
	budget.HardTokens -= reserve
	if budget.TriggerTokens > 0 {
		if budget.TriggerTokens <= reserve {
			return domain.RequestBudget{}, errors.New("result-producing call reserve exhausts the context trigger")
		}
		budget.TriggerTokens -= reserve
	}
	if budget.TargetTokens > 0 {
		if budget.TargetTokens <= reserve {
			return domain.RequestBudget{}, errors.New("result-producing call reserve exhausts the context target")
		}
		budget.TargetTokens -= reserve
	}
	if err := domain.ValidateRequestBudget(budget); err != nil {
		return domain.RequestBudget{}, fmt.Errorf("validate result-producing call budget: %w", err)
	}
	return budget, nil
}

func (r *Runtime) toolsForInvocation(origin port.AgentTurnOrigin, key domain.ConversationKey, activation *domain.ExternalAgentJobActivation) ([]tool.Tool, error) {
	if origin.Kind == port.AgentTurnOriginJobCompletion {
		if r.toolFactory == nil {
			return nil, nil
		}
		if activation == nil || activation.ActivationID != origin.ActivationID || activation.Actor != origin.Actor || activation.ConversationKey != key {
			return nil, errors.New("job-completion activation binding is incomplete")
		}
		factory, ok := r.toolFactory.(port.ActivationAgentToolFactory)
		if !ok {
			return nil, errors.New("job-completion host-only tool factory is unavailable")
		}
		rawTools, err := factory.ToolsForActivation(origin.Actor, key, *activation)
		if err != nil {
			return nil, err
		}
		tools := make([]tool.Tool, 0, len(rawTools))
		for index, raw := range rawTools {
			candidate, ok := raw.(tool.Tool)
			if !ok || candidate == nil {
				return nil, fmt.Errorf("activation tool %d is not an ADK tool: %T", index, raw)
			}
			tools = append(tools, candidate)
		}
		return tools, nil
	}

	tools := append([]tool.Tool(nil), r.staticTools...)
	if r.toolFactory == nil {
		return tools, nil
	}
	rawTools, err := r.toolFactory.ToolsForInvocation(origin.Actor, key)
	if err != nil {
		return nil, err
	}
	for _, raw := range rawTools {
		if candidate, ok := raw.(tool.Tool); ok {
			tools = append(tools, candidate)
		}
	}
	return tools, nil
}

// Run executes one agent turn against the durable session.
func (r *Runtime) Run(ctx context.Context, req port.AgentRequest) (port.AgentTurn, error) {
	if strings.TrimSpace(string(req.ConversationKey)) == "" {
		return port.AgentTurn{}, errors.New("conversation key is required")
	}
	if len(req.Messages) == 0 {
		return port.AgentTurn{}, fmt.Errorf("%w: at least one message is required", ErrInvalidHistory)
	}
	if err := validateMessages(req.Messages); err != nil {
		return port.AgentTurn{}, err
	}
	current := req.Messages[len(req.Messages)-1]
	if current.Role != domain.RoleUser {
		return port.AgentTurn{}, fmt.Errorf("%w: final message must have user role", ErrInvalidHistory)
	}
	origin, err := resolveTurnOrigin(req, current)
	if err != nil {
		return port.AgentTurn{}, err
	}
	turnCtx := port.WithAgentTurnContext(ctx, port.AgentTurnContext{ConversationKey: req.ConversationKey, Origin: origin})

	sessionID := sessionIDForOrigin(req.ConversationKey, origin)

	// Ensure session exists (idempotent).
	_, err = r.ensureSession(turnCtx, sessionID)
	if err != nil {
		return port.AgentTurn{}, fmt.Errorf("ensure ADK session: %w", err)
	}

	// Preload ephemeral context (memory + Slack data) into the current
	// model call via before-model callback. They must not become durable events.
	ephemeralCtx := buildBeforeModelContext(req)
	ephemeralCtx.actor = origin.Actor
	ephemeralCtx.origin = origin

	// Build tools for this turn. Host-originated turns use a dedicated factory;
	// they never filter the general factory by a potentially colliding name.
	tools, toolErr := r.toolsForInvocation(origin, req.ConversationKey, req.Activation)
	if toolErr != nil {
		return port.AgentTurn{}, fmt.Errorf("build invocation tools: %w", toolErr)
	}

	agent, err := r.buildAgent(tools, ephemeralCtx, false)
	if err != nil {
		return port.AgentTurn{}, fmt.Errorf("build agent: %w", err)
	}

	adkRunner, err := runner.New(runner.Config{
		AppName:        applicationName,
		Agent:          agent,
		SessionService: newTurnSessionService(boundedSessions(r.sessionService), origin),
	})
	if err != nil {
		return port.AgentTurn{}, fmt.Errorf("create runner: %w", err)
	}

	input := genai.NewContentFromText(current.Content, genai.RoleUser)

	turn, err := runTurn(turnCtx, adkRunner, input, sessionID, origin.Actor, req.ConversationKey)
	if err != nil {
		return port.AgentTurn{}, err
	}
	if turn.PendingConfirmation == nil {
		r.updateContinuity(ctx, sessionID, current.Content, turn.Text, origin.Kind != port.AgentTurnOriginJobCompletion)
	}
	return turn, nil
}

// Stream executes one turn with ADK SSE mode and exposes only typed model text
// deltas. Function calls and arguments remain inside ADK.
func (r *Runtime) Stream(ctx context.Context, req port.AgentRequest, yield func(port.AgentStreamEvent) bool) {
	terminalError := func(err error) {
		yield(port.AgentStreamEvent{Kind: port.AgentStreamError, Err: err})
	}
	if yield == nil {
		return
	}
	if strings.TrimSpace(string(req.ConversationKey)) == "" {
		terminalError(errors.New("conversation key is required"))
		return
	}
	if len(req.Messages) == 0 {
		terminalError(fmt.Errorf("%w: at least one message is required", ErrInvalidHistory))
		return
	}
	if err := validateMessages(req.Messages); err != nil {
		terminalError(err)
		return
	}
	current := req.Messages[len(req.Messages)-1]
	if current.Role != domain.RoleUser {
		terminalError(fmt.Errorf("%w: final message must have user role", ErrInvalidHistory))
		return
	}
	origin, err := resolveTurnOrigin(req, current)
	if err != nil {
		terminalError(err)
		return
	}
	turnCtx := port.WithAgentTurnContext(ctx, port.AgentTurnContext{ConversationKey: req.ConversationKey, Origin: origin})
	sessionID := sessionIDForOrigin(req.ConversationKey, origin)
	if _, err := r.ensureSession(turnCtx, sessionID); err != nil {
		terminalError(fmt.Errorf("ensure ADK session: %w", err))
		return
	}
	ephemeralCtx := buildBeforeModelContext(req)
	ephemeralCtx.actor = origin.Actor
	ephemeralCtx.origin = origin
	tools, toolErr := r.toolsForInvocation(origin, req.ConversationKey, req.Activation)
	if toolErr != nil {
		terminalError(fmt.Errorf("build invocation tools: %w", toolErr))
		return
	}
	agent, err := r.buildAgent(tools, ephemeralCtx, true)
	if err != nil {
		terminalError(fmt.Errorf("build agent: %w", err))
		return
	}
	adkRunner, err := runner.New(runner.Config{AppName: applicationName, Agent: agent, SessionService: newTurnSessionService(boundedSessions(r.sessionService), origin)})
	if err != nil {
		terminalError(fmt.Errorf("create runner: %w", err))
		return
	}
	wrappedYield := func(event port.AgentStreamEvent) bool {
		if event.Kind == port.AgentStreamCompleted && event.Turn != nil && event.Turn.PendingConfirmation == nil {
			r.updateContinuity(ctx, sessionID, current.Content, event.Turn.Text, origin.Kind != port.AgentTurnOriginJobCompletion)
		}
		return yield(event)
	}
	runStreamingTurn(turnCtx, adkRunner, genai.NewContentFromText(current.Content, genai.RoleUser), sessionID, origin.Actor, req.ConversationKey, wrappedYield)
}

func (r *Runtime) updateContinuity(ctx context.Context, sessionID, currentText, finalText string, updateObjective bool) {
	if r.continuityStore == nil {
		return
	}
	ordinal, sourceRevision, ok := r.continuityHead(ctx, sessionID)
	if !ok {
		r.recordContinuityFallback()
		return
	}
	prior, err := r.continuityStore.Latest(ctx, sessionID)
	if err != nil {
		if errors.Is(err, port.ErrContinuityValidation) {
			if r.metrics != nil {
				r.metrics.AddCounter(domain.MetricContinuityCheckpointValidationFailure, 1, port.MetricLabels{"continuity_outcome": "validation_failure"})
			}
		} else {
			r.recordContinuityFallback()
		}
		return
	}
	revision := prior.Revision + 1
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", sourceRevision, currentText, finalText)))
	sourceDigest := fmt.Sprintf("%x", digest[:])
	candidate := prior
	candidate.Revision = revision
	candidate.CoveredThrough = ordinal
	candidate.SourceDigest = sourceDigest
	if updateObjective {
		objective, ok := continuityItem(domain.ContinuityKindObjective, "objective-", currentText, ordinal, sourceRevision, sourceDigest)
		if ok {
			if prior.Objective != nil && prior.Objective.Text != objective.Text {
				objective.SupersedesID = prior.Objective.ID
				superseded := *prior.Objective
				superseded.Status = domain.ContinuityStatusSuperseded
				candidate.Superseded = appendBoundedContinuity(candidate.Superseded, superseded)
			}
			candidate.Objective = &objective
		}
	}
	applyContinuityOutcome(&candidate, finalText, ordinal, sourceRevision, sourceDigest)
	commitErr := r.continuityStore.Commit(ctx, sessionID, candidate, prior.Revision)
	if commitErr == nil {
		if r.metrics != nil {
			r.metrics.AddCounter(domain.MetricContinuityCheckpointCommitTotal, 1, port.MetricLabels{"continuity_outcome": "success"})
		}
		return
	}
	switch {
	case errors.Is(commitErr, port.ErrContinuityCASConflict):
		if r.metrics != nil {
			r.metrics.AddCounter(domain.MetricContinuityCheckpointCASConflictTotal, 1, port.MetricLabels{"continuity_outcome": "cas_conflict"})
		}
	case errors.Is(commitErr, port.ErrContinuityValidation):
		if r.metrics != nil {
			r.metrics.AddCounter(domain.MetricContinuityCheckpointValidationFailure, 1, port.MetricLabels{"continuity_outcome": "validation_failure"})
		}
	default:
		r.recordContinuityFallback()
	}
}

// continuityHead resolves the event ordinal and session revision that
// updateContinuity needs, without loading the session when the backing
// service exposes LatestEventOrdinal. Under DEC-08-3, revision equals the
// event count equals max(ordinal)+1, so sourceRevision follows directly from
// the head with no extra query. ok is false for an empty session (head -1)
// or a read failure, matching the prior unbounded-Get fallback behavior.
func (r *Runtime) continuityHead(ctx context.Context, sessionID string) (ordinal, sourceRevision int64, ok bool) {
	if headReader, isHeadReader := r.sessionService.(epochEventHeadReader); isHeadReader {
		head, err := headReader.LatestEventOrdinal(ctx, applicationName, ephemeralUserID, sessionID)
		if err != nil || head < 0 {
			return 0, 0, false
		}
		return head, head + 1, true
	}
	loaded, err := r.sessionService.Get(ctx, &session.GetRequest{AppName: applicationName, UserID: ephemeralUserID, SessionID: sessionID})
	if err != nil || loaded == nil || loaded.Session == nil || loaded.Session.Events() == nil || loaded.Session.Events().Len() == 0 {
		return 0, 0, false
	}
	ordinal = int64(loaded.Session.Events().Len() - 1)
	sourceRevision = int64(loaded.Session.Events().Len())
	if revisioned, isRevisioned := loaded.Session.(interface{ Revision() int64 }); isRevisioned {
		sourceRevision = revisioned.Revision()
	}
	return ordinal, sourceRevision, true
}

func (r *Runtime) recordContinuityFallback() {
	if r != nil && r.metrics != nil {
		r.metrics.AddCounter(domain.MetricContinuityCheckpointFallbackTotal, 1, port.MetricLabels{"continuity_outcome": "fallback"})
	}
}

func continuityItem(kind domain.ContinuityItemKind, prefix, text string, ordinal, sourceRevision int64, digest string) (domain.ContinuityItem, bool) {
	return domain.SanitizeContinuityItem(domain.ContinuityItem{ID: prefix + digest[:16], Kind: kind,
		Text: boundedContinuityText(text), SourceEventOrdinal: ordinal, SourceSessionRevision: sourceRevision,
		SourceDigest: digest, Status: domain.ContinuityStatusCurrent})
}

func applyContinuityOutcome(capsule *domain.ContinuityCapsule, finalText string, ordinal, sourceRevision int64, digest string) {
	type section struct {
		prefix string
		kind   domain.ContinuityItemKind
		target *[]domain.ContinuityItem
	}
	sections := []section{
		{"constraint:", domain.ContinuityKindConstraint, &capsule.Constraints},
		{"decision:", domain.ContinuityKindDecision, &capsule.Decisions},
		{"pending:", domain.ContinuityKindPending, &capsule.Pending},
		{"open question:", domain.ContinuityKindOpenQuestion, &capsule.OpenQuestions},
		{"completed:", domain.ContinuityKindCompleted, &capsule.Completed},
	}
	classified := false
	for index, line := range strings.Split(finalText, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		for _, current := range sections {
			if !strings.HasPrefix(lower, current.prefix) {
				continue
			}
			text := strings.TrimSpace(trimmed[len(current.prefix):])
			item, ok := continuityItem(current.kind, string(current.kind)+fmt.Sprintf("-%d-", index), text, ordinal, sourceRevision, digest)
			if ok {
				*current.target = appendBoundedContinuity(*current.target, item)
				classified = true
			}
			break
		}
	}
	if !classified {
		if item, ok := continuityItem(domain.ContinuityKindCompleted, "completed-", finalText, ordinal, sourceRevision, digest); ok {
			capsule.Completed = appendBoundedContinuity(capsule.Completed, item)
		}
	}
}

func appendBoundedContinuity(items []domain.ContinuityItem, item domain.ContinuityItem) []domain.ContinuityItem {
	items = append(items, item)
	if len(items) > 8 {
		items = items[len(items)-8:]
	}
	return items
}

func boundedContinuityText(value string) string {
	const maxCodePoints = 1000
	runes := []rune(value)
	if len(runes) > maxCodePoints {
		runes = runes[:maxCodePoints]
	}
	return string(runes)
}

// Resume continues a pending confirmation by sending the user's decision.
func (r *Runtime) Resume(ctx context.Context, decision domain.ConfirmationDecision) (port.AgentTurn, error) {
	if strings.TrimSpace(string(decision.ConversationKey)) == "" {
		return port.AgentTurn{}, errors.New("confirmation conversation key is required")
	}
	if strings.TrimSpace(decision.WrapperCallID) == "" || strings.TrimSpace(decision.OriginalCallID) == "" {
		return port.AgentTurn{}, errors.New("confirmation call IDs are required")
	}
	if strings.TrimSpace(decision.Actor) == "" {
		return port.AgentTurn{}, errors.New("confirmation actor is required")
	}
	sessionID := adkSessionID(decision.ConversationKey)
	if _, err := r.ensureSession(ctx, sessionID); err != nil {
		return port.AgentTurn{}, fmt.Errorf("ensure ADK session for resume: %w", err)
	}

	tools := append([]tool.Tool(nil), r.staticTools...)
	if r.toolFactory != nil {
		rawTools, toolErr := r.toolFactory.ToolsForInvocation(decision.Actor, decision.ConversationKey)
		if toolErr != nil {
			return port.AgentTurn{}, fmt.Errorf("build invocation tools for resume: %w", toolErr)
		}
		for _, raw := range rawTools {
			if t, ok := raw.(tool.Tool); ok {
				tools = append(tools, t)
			}
		}
	}
	agent, err := r.buildAgent(tools, beforeModelData{actor: decision.Actor}, false)
	if err != nil {
		return port.AgentTurn{}, fmt.Errorf("build agent for resume: %w", err)
	}

	adkRunner, err := runner.New(runner.Config{
		AppName:        applicationName,
		Agent:          agent,
		SessionService: boundedSessions(r.sessionService),
	})
	if err != nil {
		return port.AgentTurn{}, fmt.Errorf("create runner for resume: %w", err)
	}

	payload := decision.Payload
	if payload == nil {
		payload = make(map[string]any)
	}

	resumeContent := &genai.Content{
		Role: string(genai.RoleUser),
		Parts: []*genai.Part{
			{
				FunctionResponse: &genai.FunctionResponse{
					ID:           decision.WrapperCallID,
					Name:         toolconfirmation.FunctionCallName,
					WillContinue: boolPointer(false),
					Response: map[string]any{
						"confirmed": decision.Approved,
						"payload":   payload,
					},
				},
			},
		},
	}

	turn, err := runTurn(ctx, adkRunner, resumeContent, sessionID, decision.Actor, decision.ConversationKey)
	if err != nil {
		return port.AgentTurn{}, err
	}
	if turn.PendingConfirmation == nil {
		r.updateContinuity(ctx, sessionID, "", turn.Text, false)
	}
	return turn, nil
}

// RecoverActivation inspects only durable ADK events tagged with the supplied
// activation. It intentionally does not construct a runner or call the model.
func (r *Runtime) RecoverActivation(ctx context.Context, conversationKey domain.ConversationKey, activationID string) (port.AgentTurn, bool, error) {
	if r == nil || r.sessionService == nil || strings.TrimSpace(string(conversationKey)) == "" || strings.TrimSpace(activationID) == "" {
		return port.AgentTurn{}, false, errors.New("activation recovery identity is required")
	}
	// The activation session is scoped to one activation, not the whole
	// conversation, so its row count is small; bounding the read still keeps
	// this off the unbounded-Get path that risk 2 and risk 4 share.
	loaded, err := boundedSessions(r.sessionService).Get(ctx, &session.GetRequest{
		AppName: applicationName, UserID: ephemeralUserID, SessionID: adkActivationSessionID(activationID),
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return port.AgentTurn{}, false, nil
		}
		return port.AgentTurn{}, false, fmt.Errorf("load durable activation session: %w", err)
	}
	if loaded == nil || loaded.Session == nil {
		return port.AgentTurn{}, false, nil
	}
	if err := r.checkProviderFamily(loaded.Session); err != nil {
		return port.AgentTurn{}, false, err
	}
	events := loaded.Session.Events()
	if events == nil {
		return port.AgentTurn{}, false, nil
	}
	var recovered string
	for index := 0; index < events.Len(); index++ {
		event := events.At(index)
		if event == nil || event.CustomMetadata == nil || event.Content == nil {
			continue
		}
		if event.CustomMetadata[port.AgentTurnOriginMetadataKey] != string(port.AgentTurnOriginJobCompletion) || event.CustomMetadata[port.AgentTurnActivationIDMetadataKey] != activationID {
			continue
		}
		if event.Content.Role != genai.RoleModel || event.Partial || !event.IsFinalResponse() {
			continue
		}
		text, textErr := eventText(event.Content)
		if textErr != nil || strings.TrimSpace(text) == "" {
			continue
		}
		if recovered != "" {
			// Multiple finals cannot prove which response belongs to the
			// activation, so fail closed and let the caller mark it unknown.
			return port.AgentTurn{}, false, nil
		}
		recovered = strings.TrimSpace(text)
	}
	if recovered == "" {
		return port.AgentTurn{}, false, nil
	}
	return port.AgentTurn{Text: recovered}, true, nil
}

func boolPointer(value bool) *bool { return &value }

// sessionExistenceChecker is a lightweight get-or-create primitive: it
// reports whether a session row exists, and returns its state, without
// loading any events. ensureSession only needs the state (provider family
// validation), never the event history, so callers that support this
// interface skip the unbounded Get that the create-then-get idiom used to
// require on every turn after the first.
type sessionExistenceChecker interface {
	SessionExists(ctx context.Context, appName, userID, sessionID string) (session.Session, bool, error)
}

func (r *Runtime) ensureSession(ctx context.Context, sessionID string) (session.Session, error) {
	if checker, ok := r.sessionService.(sessionExistenceChecker); ok {
		return r.ensureSessionIdempotent(ctx, checker, sessionID)
	}
	// Fallback for session services without the lightweight existence check
	// (for example ADK's in-memory service): keep the create-then-get idiom.
	created, err := r.sessionService.Create(ctx, &session.CreateRequest{
		AppName:   applicationName,
		UserID:    ephemeralUserID,
		SessionID: sessionID,
		State: map[string]any{
			domain.ProviderFamilyStateKey: r.providerFamily,
		},
	})
	if err != nil {
		// Session may already exist from a previous turn or crash recovery.
		resp, getErr := r.sessionService.Get(ctx, &session.GetRequest{
			AppName:   applicationName,
			UserID:    ephemeralUserID,
			SessionID: sessionID,
		})
		if getErr != nil {
			return nil, fmt.Errorf("create session: %w (get also failed: %v)", err, getErr)
		}
		if familyErr := r.checkProviderFamily(resp.Session); familyErr != nil {
			return nil, familyErr
		}
		return resp.Session, nil
	}
	return created.Session, nil
}

// ensureSessionIdempotent is the normal, non-failure path: check existence
// first, and create only when the session is genuinely new. A Create error
// here means a concurrent turn won the race, which is the true exceptional
// case, not the everyday one.
func (r *Runtime) ensureSessionIdempotent(ctx context.Context, checker sessionExistenceChecker, sessionID string) (session.Session, error) {
	existing, found, err := checker.SessionExists(ctx, applicationName, ephemeralUserID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("check session existence: %w", err)
	}
	if found {
		if familyErr := r.checkProviderFamily(existing); familyErr != nil {
			return nil, familyErr
		}
		return existing, nil
	}
	created, err := r.sessionService.Create(ctx, &session.CreateRequest{
		AppName:   applicationName,
		UserID:    ephemeralUserID,
		SessionID: sessionID,
		State: map[string]any{
			domain.ProviderFamilyStateKey: r.providerFamily,
		},
	})
	if err != nil {
		// Lost a create race against a concurrent turn for the same session.
		existing, found, existErr := checker.SessionExists(ctx, applicationName, ephemeralUserID, sessionID)
		if existErr != nil || !found {
			return nil, fmt.Errorf("create session: %w (existence check also failed: %v)", err, existErr)
		}
		if familyErr := r.checkProviderFamily(existing); familyErr != nil {
			return nil, familyErr
		}
		return existing, nil
	}
	return created.Session, nil
}

// ErrProviderFamilyMismatch indicates a durable session created by a different
// provider family. Structured history is never flattened across families.
var ErrProviderFamilyMismatch = errors.New("durable session provider family mismatch")

// checkProviderFamily defensively re-validates the stored provider family
// before a turn. Sessions without the marker are legacy openai_compatible.
func (r *Runtime) checkProviderFamily(sess session.Session) error {
	if sess == nil {
		return nil
	}
	stored := domain.ProviderFamilyOpenAICompatible
	if value, err := sess.State().Get(domain.ProviderFamilyStateKey); err == nil {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			stored = text
		}
	}
	if stored != r.providerFamily {
		return fmt.Errorf("%w: session %q was created by provider family %q but %q is configured. Run: local-agent init --reset-state",
			ErrProviderFamilyMismatch, sess.ID(), stored, r.providerFamily)
	}
	return nil
}

// --- turn execution ---

func runTurn(ctx context.Context, adkRunner *runner.Runner, input *genai.Content, sessionID, actor string, key domain.ConversationKey) (port.AgentTurn, error) {
	var (
		finalText           string
		pendingConfirmation *domain.PendingConfirmation
	)

	for event, runErr := range adkRunner.Run(ctx, ephemeralUserID, sessionID, input, agent.RunConfig{StreamingMode: agent.StreamingModeNone}) {
		if runErr != nil {
			return port.AgentTurn{}, fmt.Errorf("run ADK agent: %w", runErr)
		}
		if event == nil || event.Content == nil {
			continue
		}

		// Check for confirmation requests.
		for _, part := range event.Content.Parts {
			if part.FunctionCall != nil && part.FunctionCall.Name == toolconfirmation.FunctionCallName {
				pendingConfirmation = extractConfirmation(part.FunctionCall)
				if pendingConfirmation != nil {
					pendingConfirmation.Actor = actor
					pendingConfirmation.ConversationKey = key
				}
			}
		}

		if event.IsFinalResponse() && event.Content.Role == genai.RoleModel {
			text, _ := eventText(event.Content)
			if strings.TrimSpace(text) != "" {
				finalText = text
			}
		}
	}

	if strings.TrimSpace(finalText) == "" && pendingConfirmation == nil {
		return port.AgentTurn{}, ErrNoResponse
	}

	return port.AgentTurn{
		Text:                strings.TrimSpace(finalText),
		PendingConfirmation: pendingConfirmation,
	}, nil
}

func runStreamingTurn(ctx context.Context, adkRunner *runner.Runner, input *genai.Content, sessionID, actor string, key domain.ConversationKey, yield func(port.AgentStreamEvent) bool) {
	var (
		allText             strings.Builder
		partialText         strings.Builder
		pendingConfirmation *domain.PendingConfirmation
	)
	emitText := func(text string) bool {
		allText.WriteString(text)
		return yield(port.AgentStreamEvent{Kind: port.AgentStreamTextDelta, TextDelta: text})
	}
	for event, runErr := range adkRunner.Run(ctx, ephemeralUserID, sessionID, input, agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) {
		if runErr != nil {
			yield(port.AgentStreamEvent{Kind: port.AgentStreamError, Err: fmt.Errorf("run streaming ADK agent: %w", runErr)})
			return
		}
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part.FunctionCall != nil && part.FunctionCall.Name == toolconfirmation.FunctionCallName {
				pendingConfirmation = extractConfirmation(part.FunctionCall)
				if pendingConfirmation != nil {
					pendingConfirmation.Actor = actor
					pendingConfirmation.ConversationKey = key
				}
			}
		}
		text, _ := eventText(event.Content)
		if event.Partial && event.Content.Role == genai.RoleModel && text != "" {
			if partialText.Len() == 0 && allText.Len() > 0 {
				if !emitText("\n\n") {
					return
				}
			}
			partialText.WriteString(text)
			if !emitText(text) {
				return
			}
			continue
		}
		if !event.Partial && event.Content.Role == genai.RoleModel {
			if partialText.Len() > 0 {
				if text != "" && text != partialText.String() {
					yield(port.AgentStreamEvent{Kind: port.AgentStreamError, Err: errors.New("streamed ADK text differs from its final aggregate")})
					return
				}
				partialText.Reset()
			} else if text != "" {
				if allText.Len() > 0 {
					if !emitText("\n\n") {
						return
					}
				}
				if !emitText(text) {
					return
				}
			}
		}
	}
	text := allText.String()
	turn := &port.AgentTurn{Text: text, PendingConfirmation: pendingConfirmation}
	if pendingConfirmation != nil {
		yield(port.AgentStreamEvent{Kind: port.AgentStreamPendingConfirmation, Turn: turn})
		return
	}
	if strings.TrimSpace(text) == "" {
		yield(port.AgentStreamEvent{Kind: port.AgentStreamError, Err: ErrNoResponse})
		return
	}
	yield(port.AgentStreamEvent{Kind: port.AgentStreamCompleted, Turn: turn})
}

func extractConfirmation(fc *genai.FunctionCall) *domain.PendingConfirmation {
	if fc == nil {
		return nil
	}
	originalCall, err := toolconfirmation.OriginalCallFrom(fc)
	if err != nil || originalCall == nil {
		return nil
	}

	// Compute a stable parameter hash.
	var paramHash string
	if originalCall.Args != nil {
		hash := sha256.New()
		encoded, _ := json.Marshal(originalCall.Args)
		hash.Write(encoded)
		paramHash = fmt.Sprintf("%x", hash.Sum(nil))[:16]
	}

	summary := confirmationHint(fc)
	if strings.TrimSpace(summary) == "" {
		summary = fmt.Sprintf("Tool %q requires confirmation", originalCall.Name)
	}
	payload, ok := confirmationPayload(fc, originalCall)
	if !ok {
		return nil
	}
	return &domain.PendingConfirmation{
		WrapperCallID:  fc.ID,
		OriginalCallID: originalCall.ID,
		Summary:        summary,
		Payload:        payload,
		ParameterHash:  paramHash,
		Expiry:         time.Now().Add(15 * time.Minute),
	}
}

// confirmationPayload renders the host-issued confirmation payload for any
// tool call that requested confirmation with a custom, non-generic hint —
// the signal that the tool explicitly called RequestConfirmation with a
// real payload, rather than relying on the bare RequireConfirmation flag
// (whose ADK-synthesized hint always mentions "FunctionResponse" and is
// discarded by usableConfirmationHint). This covers workstream_ tools and
// the durable ACP delegation tool without hardcoding either tool's name; a
// missing payload on a call that did present a custom hint is a defect,
// not a normal path, and fails closed rather than silently proceeding with
// an unidentified request.
func confirmationPayload(wrapper, call *genai.FunctionCall) (string, bool) {
	if call == nil {
		return "", true
	}
	confirmation, ok := requestedToolConfirmation(wrapper)
	if !ok || usableConfirmationHint(confirmation.Hint) == "" {
		return "", true
	}
	if confirmation.Payload == nil {
		return "", false
	}
	encoded, err := json.Marshal(confirmation.Payload)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

func confirmationHint(fc *genai.FunctionCall) string {
	confirmation, ok := requestedToolConfirmation(fc)
	if !ok {
		return ""
	}
	return usableConfirmationHint(confirmation.Hint)
}

func requestedToolConfirmation(fc *genai.FunctionCall) (toolconfirmation.ToolConfirmation, bool) {
	if fc == nil || fc.Args == nil {
		return toolconfirmation.ToolConfirmation{}, false
	}
	raw, ok := fc.Args["toolConfirmation"]
	if !ok {
		return toolconfirmation.ToolConfirmation{}, false
	}
	switch confirmation := raw.(type) {
	case toolconfirmation.ToolConfirmation:
		return confirmation, true
	case *toolconfirmation.ToolConfirmation:
		if confirmation != nil {
			return *confirmation, true
		}
	case map[string]any:
		hint, _ := confirmation["hint"].(string)
		payload, exists := confirmation["payload"]
		return toolconfirmation.ToolConfirmation{Hint: hint, Payload: payload}, exists
	}
	return toolconfirmation.ToolConfirmation{}, false
}

func usableConfirmationHint(hint string) string {
	hint = strings.TrimSpace(hint)
	if strings.Contains(hint, "FunctionResponse") {
		return ""
	}
	return hint
}

// --- ephemeral context (before-model callback) ---

type beforeModelData struct {
	context            domain.AgentContext
	actor              string
	origin             port.AgentTurnOrigin
	beforeModel        func(context.Context) error
	knowledge          []domain.KnowledgeFrameCard
	workstreamRevision int64
	workstreamSnapshot *domain.WorkstreamSnapshot
}

func buildBeforeModelContext(req port.AgentRequest) beforeModelData {
	return beforeModelData{
		context:     req.Context,
		actor:       latestActor(req),
		beforeModel: req.BeforeModel,
		// Knowledge and the workstream snapshot live only in the before-model
		// path: they are cloned here and never appended to durable session
		// events.
		knowledge:          append([]domain.KnowledgeFrameCard(nil), req.Knowledge...),
		workstreamRevision: req.WorkstreamRevision,
		workstreamSnapshot: cloneWorkstreamSnapshot(req.WorkstreamSnapshot),
	}
}

// cloneWorkstreamSnapshot defensively copies the ephemeral snapshot so
// callers can never mutate it through the shared AgentRequest pointer.
func cloneWorkstreamSnapshot(snapshot *domain.WorkstreamSnapshot) *domain.WorkstreamSnapshot {
	if snapshot == nil {
		return nil
	}
	cloned := *snapshot
	return &cloned
}

func latestActor(req port.AgentRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].UserID != "" {
			return req.Messages[i].UserID
		}
	}
	return ""
}

func (d beforeModelData) reference() string {
	var parts []string

	contextRef := domain.RenderContextReference(d.context, d.context.MaxChars)
	if contextRef != "" {
		parts = append(parts, contextRef)
	}
	return strings.Join(parts, "\n\n")
}

func injectEphemeralReference(reference string) llmagent.BeforeModelCallback {
	return func(_ agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
		if request == nil {
			return nil, errors.New("ADK model request is nil")
		}
		if request.Config == nil {
			request.Config = &genai.GenerateContentConfig{}
		}
		if request.Config.SystemInstruction == nil {
			request.Config.SystemInstruction = genai.NewContentFromText(reference, genai.RoleUser)
			return nil, nil
		}
		request.Config.SystemInstruction.Parts = append(
			request.Config.SystemInstruction.Parts,
			genai.NewPartFromText("\n\n"+reference),
		)
		return nil, nil
	}
}
