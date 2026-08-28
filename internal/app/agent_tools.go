package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/agenttool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/adkagent"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/tooldef"
	contextcompilerusecase "github.com/Dauno/slack-local-agent/internal/usecase/contextcompiler"
)

// agentToolNonStreamingModel adapts ADK AgentTool's internal SSE runner to a
// non-streaming model. AgentTool consumes only the completed child result, so
// no streaming semantics are exposed or lost.
type agentToolNonStreamingModel struct {
	delegate model.LLM
}

func (m *agentToolNonStreamingModel) Name() string {
	if m == nil || m.delegate == nil {
		return ""
	}
	return m.delegate.Name()
}

func (m *agentToolNonStreamingModel) GenerateContent(ctx context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	if m == nil || m.delegate == nil {
		return func(yield func(*model.LLMResponse, error) bool) {
			yield(nil, errors.New("agent tool model is not configured"))
		}
	}
	return m.delegate.GenerateContent(ctx, request, false)
}

// preparedAgentTool is one startup-validated agent-tool child. CLI children
// carry a reusable tool-less AgentTool wrapper; OpenAI-compatible children are
// rebuilt per invocation because their tool instances capture invocation
// identity.
type preparedAgentTool struct {
	definition           agentdef.AgentDef
	model                model.LLM
	contextCompiler      port.ContextCompiler
	contextCounter       port.RequestTokenCounter
	contextBudget        domain.RequestBudget
	cliTool              tool.Tool
	cliResolved          *agentdef.ResolvedModel
	projectRoots         map[string]string
	externalAgentTimeout time.Duration
	executionMode        string
	registryRevision     string
}

type externalAgentArgs struct {
	Project string `json:"project" jsonschema:"registered project name to use as the workspace"`
	Task    string `json:"task" jsonschema:"complete bounded task for the external agent"`
}

type externalAgentResult struct {
	Result       string               `json:"result"`
	ResultHandle *boundedResultHandle `json:"result_handle,omitempty"`
}

type boundedResultHandle struct {
	JobID        string                      `json:"job_id"`
	ResultID     string                      `json:"result_id"`
	SHA256       string                      `json:"sha256"`
	Bytes        int64                       `json:"bytes"`
	MediaType    string                      `json:"media_type"`
	Availability []domain.ResultAvailability `json:"availability"`
}

type agentToolContextConfig struct {
	compiler port.ContextCompiler
	budget   domain.RequestBudget
	actor    string
}

func isReadOnlyChildTool(name string) bool {
	switch name {
	case "list_messages", "list_repos", "list_directory", "read_file", "list_worktrees", "read_file_range":
		return true
	default:
		return false
	}
}

// prepareRootAgentTools resolves, constructs, and validates every configured
// agent-tool child model at process start, before Slack Socket Mode opens.
func prepareRootAgentTools(
	ctx context.Context,
	defs *agentdef.Definitions,
	root agentdef.AgentDef,
	values map[string]string,
	cfg config.Config,
	paths config.Paths,
	logger port.Logger,
	sanitize func(string) string,
	describedCLIProviders map[string]bool,
) ([]preparedAgentTool, error) {
	if defs == nil {
		return nil, nil
	}

	names := root.AgentTools
	if len(names) == 0 {
		names = agentdef.EligibleAgentNames(defs)
	}
	prepared := make([]preparedAgentTool, 0, len(names))
	for _, name := range names {
		definition, exists := defs.Agents[name]
		if !exists {
			return nil, fmt.Errorf("agent tool %q is not defined", name)
		}

		resolved, err := defs.ResolveModel(definition.Model)
		if err != nil {
			return nil, fmt.Errorf("resolve agent tool %q model: %w", name, err)
		}
		requestPercent := definition.EffectiveContextBudgetPercent(cfg.Context.ModelBudget.MaxRequestPercent)
		contextCounter, contextBudget, contextErr := composeModelContextAdmission(resolved, cfg, requestPercent)
		if contextErr != nil {
			return nil, fmt.Errorf("compose agent tool %q context admission: %w", name, contextErr)
		}
		childModel, _, err := newModelForResolved(ctx, resolved, values, cfg, paths, logger, sanitize, requestPercent)
		if err != nil {
			return nil, fmt.Errorf("build agent tool %q model: %w", name, err)
		}

		if resolved.IsAgentCLI() {
			if err := handshakeSelectedAgentCLI(ctx, resolved, childModel, describedCLIProviders); err != nil {
				return nil, fmt.Errorf("validate agent tool %q model: %w", name, err)
			}
			child, err := newAgentCLIToolAgent(definition, root.EffectiveDelegatedGlobalInstruction(), childModel)
			if err != nil {
				return nil, fmt.Errorf("build agent tool %q: %w", name, err)
			}
			revision, err := agentExecutionFingerprint(definition, resolved, paths.SandboxProjectRoots, cfg)
			if err != nil {
				return nil, fmt.Errorf("fingerprint agent tool %q scope: %w", name, err)
			}
			prepared = append(prepared, preparedAgentTool{
				definition:           definition,
				model:                childModel,
				cliResolved:          resolved,
				projectRoots:         paths.SandboxProjectRoots,
				cliTool:              agenttool.New(child, &agenttool.Config{}),
				executionMode:        definition.ExecutionMode,
				externalAgentTimeout: externalAgentTimeout(definition, externalAgentFallback(definition, cfg)),
				registryRevision:     revision,
			})
			continue
		}

		// Prove the scoped OpenAI-compatible child is representable as an ADK
		// LlmAgent before Socket Mode; per-invocation construction attaches
		// the actor-scoped tools.
		if _, err := newAgentToolAgent(definition, root.EffectiveDelegatedGlobalInstruction(), childModel, nil); err != nil {
			return nil, fmt.Errorf("build agent tool %q: %w", name, err)
		}
		prepared = append(prepared, preparedAgentTool{definition: definition, model: childModel, contextCounter: contextCounter, contextBudget: contextBudget})
	}
	return prepared, nil
}

// compositeAgentToolFactory composes the base invocation tool factory with the
// prepared agent-tool children and workflow-tool children. Agent tools precede
// workflow tools, and both precede direct root tools in one deterministic list;
// any construction failure fails the whole turn.
type compositeAgentToolFactory struct {
	base                       port.AgentToolFactory
	children                   []preparedAgentTool
	workflowChildren           []preparedWorkflowTool
	delegatedGlobalInstruction string
	jobStarter                 port.ExternalAgentJobStarter
	completionBindings         port.ExternalAgentJobCompletionBindingResolver
	declarativeTools           map[string]tooldef.ToolDef
}

// declarativeToolProvider is implemented by the base factory so children and
// workflow steps can build individual declared tools without receiving the
// whole registry.
type declarativeToolProvider interface {
	DeclarativeToolByName(name string) (tool.Tool, error)
}

var _ port.AgentToolFactory = (*compositeAgentToolFactory)(nil)

func newCompositeAgentToolFactory(base port.AgentToolFactory, children []preparedAgentTool, workflowChildren []preparedWorkflowTool, delegatedGlobalInstruction string) *compositeAgentToolFactory {
	return &compositeAgentToolFactory{
		base:                       base,
		children:                   children,
		workflowChildren:           workflowChildren,
		delegatedGlobalInstruction: delegatedGlobalInstruction,
	}
}

func (f *compositeAgentToolFactory) setJobStarter(starter port.ExternalAgentJobStarter) {
	if f != nil {
		f.jobStarter = starter
	}
}

func (f *compositeAgentToolFactory) setCompletionBindingResolver(resolver port.ExternalAgentJobCompletionBindingResolver) {
	if f != nil {
		f.completionBindings = resolver
	}
}

// setDeclarativeTools records the declarative tools active for this
// deployment. Children may reference them through their own tool_scope;
// workflow steps may reference them through their tools list.
func (f *compositeAgentToolFactory) setDeclarativeTools(tools map[string]tooldef.ToolDef) {
	if f != nil {
		f.declarativeTools = tools
	}
}

func (f *compositeAgentToolFactory) setChildContextResultStore(store port.RecoverableResultStore) {
	if f == nil || store == nil {
		return
	}
	for index := range f.children {
		child := &f.children[index]
		if child.contextCompiler == nil && child.contextCounter != nil && child.contextBudget.HardTokens > 0 {
			child.contextCompiler = contextcompilerusecase.New(store, child.contextCounter)
		}
	}
}

// ToolsForInvocation implements port.AgentToolFactory.
func (f *compositeAgentToolFactory) ToolsForInvocation(actor string, key domain.ConversationKey) ([]any, error) {
	if f == nil {
		return nil, nil
	}
	var baseRaw []any
	if f.base != nil {
		var err error
		baseRaw, err = f.base.ToolsForInvocation(actor, key)
		if err != nil {
			return nil, err
		}
	}
	scoped := make([]tool.Tool, 0, len(baseRaw))
	toolIndex := make(map[string]tool.Tool, len(baseRaw))
	for index, raw := range baseRaw {
		adkTool, ok := raw.(tool.Tool)
		if !ok {
			return nil, fmt.Errorf("invocation tool %d is not an ADK tool: %T", index, raw)
		}
		// Child agents and workflow steps receive only the fixed read-only allowlist.
		if isReadOnlyChildTool(adkTool.Name()) {
			scoped = append(scoped, adkTool)
			toolIndex[adkTool.Name()] = adkTool
		}
	}

	// Declarative tools (sandbox_read_only) are available to child agents that
	// declare them in tool_scope and to workflow steps by name.
	provider, hasProvider := f.base.(declarativeToolProvider)
	for _, name := range sortedDeclarativeNames(f.declarativeTools) {
		if !hasProvider {
			return nil, fmt.Errorf("declarative tool %q is configured without a tool provider", name)
		}
		declared, err := provider.DeclarativeToolByName(name)
		if err != nil {
			return nil, fmt.Errorf("build declarative tool %q: %w", name, err)
		}
		toolIndex[name] = declared
	}

	combined := make([]any, 0, len(f.children)+len(f.workflowChildren)+len(baseRaw))
	for _, child := range f.children {
		if child.cliTool != nil {
			// A durable agent CLI leaf needs invocation identity for the
			// confirmation gate and the job record, so it is rebuilt per call.
			// A foreground leaf reuses the startup-validated wrapper.
			if child.executionMode == agentdef.ExecutionModeDurableJob {
				durable, err := newAgentCLIDurableTool(
					child.definition,
					child.cliResolved,
					child.projectRoots,
					child.externalAgentTimeout,
					child.registryRevision,
					f.jobStarter,
					f.completionBindings,
					actor,
					key,
				)
				if err != nil {
					return nil, fmt.Errorf("build durable agent CLI tool %q: %w", child.definition.Name, err)
				}
				combined = append(combined, durable)
				continue
			}
			combined = append(combined, child.cliTool)
			continue
		}
		childTools, err := f.childDeclarativeTools(provider, child.definition, scoped)
		if err != nil {
			return nil, fmt.Errorf("build agent tool %q: %w", child.definition.Name, err)
		}
		childAgent, err := newAgentToolAgentWithContext(child.definition, f.delegatedGlobalInstruction, child.model, childTools, childContextConfig(child, actor))
		if err != nil {
			return nil, fmt.Errorf("build agent tool %q: %w", child.definition.Name, err)
		}
		combined = append(combined, agenttool.New(childAgent, &agenttool.Config{}))
	}

	for idx := range f.workflowChildren {
		workflowTool, err := f.workflowChildren[idx].buildAgentTool(invocationScope{
			globalInstruction: f.delegatedGlobalInstruction,
			toolIndex:         toolIndex,
		})
		if err != nil {
			return nil, fmt.Errorf("build workflow tool %q: %w", f.workflowChildren[idx].blueprint.ID, err)
		}
		combined = append(combined, workflowTool)
	}

	combined = append(combined, baseRaw...)
	return combined, nil
}

// ToolsForActivation delegates only to the base host factory. Children and
// workflows are intentionally absent from this path even if their names
// collide with a host tool.
func (f *compositeAgentToolFactory) ToolsForActivation(actor string, key domain.ConversationKey, activation domain.ExternalAgentJobActivation) ([]any, error) {
	if f == nil || f.base == nil {
		return nil, errors.New("activation host-only tool factory is unavailable")
	}
	factory, ok := f.base.(port.ActivationAgentToolFactory)
	if !ok {
		return nil, errors.New("activation host-only tool factory is unavailable")
	}
	return factory.ToolsForActivation(actor, key, activation)
}

func externalAgentDelegationConfirmation(
	ctx context.Context,
	completionBindings port.ExternalAgentJobCompletionBindingResolver,
	actor string,
	key domain.ConversationKey,
	args externalAgentArgs,
) (string, map[string]any) {
	task := boundedConfirmationText(args.Task, maxConfirmationTaskRunes)
	payload := map[string]any{
		"project": args.Project,
		"task":    task,
	}
	hint := fmt.Sprintf("Approve delegating task %q in project %q to an external agent.", task, args.Project)
	if completionBindings == nil || actor == "" || key == "" {
		return hint, payload
	}
	binding, found, err := completionBindings.CompletionBindingForTask(ctx, actor, key, args.Project, args.Task)
	if err != nil || !found {
		return hint, payload
	}
	payload["workstream_id"] = binding.WorkstreamID
	payload["task_id"] = binding.TaskID
	payload["expected_revision"] = binding.AdmissionRevision
	if len(binding.RequiredInputs) > 0 {
		payload["source_result_identities"] = append([]string(nil), binding.RequiredInputs...)
	}
	hint = fmt.Sprintf("Approve delegating workstream %q task %q (project %q) to an external agent at revision %d.",
		binding.WorkstreamID, task, args.Project, binding.AdmissionRevision)
	return hint, payload
}

const maxConfirmationTaskRunes = 2000

// boundedConfirmationText truncates on a rune boundary and marks truncation,
// so an oversized model-authored task never grows the confirmation payload
// unbounded.
func boundedConfirmationText(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "…"
}

func externalAgentTimeout(definition agentdef.AgentDef, fallback time.Duration) time.Duration {
	if definition.TimeoutSeconds > 0 {
		return time.Duration(definition.TimeoutSeconds) * time.Second
	}
	return fallback
}

func externalAgentFallback(definition agentdef.AgentDef, cfg config.Config) time.Duration {
	if definition.ExecutionMode == agentdef.ExecutionModeDurableJob {
		// Detached jobs have their own total budget; root model timeout is unrelated.
		return time.Duration(cfg.ExternalAgent.DefaultJobTimeoutSeconds) * time.Second
	}
	return time.Duration(cfg.Runtime.ModelTimeoutSeconds) * time.Second
}

func agentExecutionFingerprint(definition agentdef.AgentDef, resolved *agentdef.ResolvedModel, projectRoots map[string]string, cfg config.Config) (string, error) {
	if resolved == nil {
		return "", errors.New("resolved model is required")
	}
	payload := struct {
		Definition   agentdef.AgentDef
		Resolved     agentdef.ResolvedModel
		ProjectRoots map[string]string
		Config       config.Config
	}{Definition: definition, Resolved: *resolved, ProjectRoots: projectRoots, Config: cfg}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", digest), nil
}

func ctxFunctionCallID(ctx context.Context) string {
	if value, ok := ctx.(interface{ FunctionCallID() string }); ok {
		return value.FunctionCallID()
	}
	return ""
}

func teamIDFromConversation(key domain.ConversationKey) string {
	value := string(key)
	if len(value) > len("slack:") {
		value = value[len("slack:"):]
		if index := strings.IndexByte(value, ':'); index > 0 {
			return value[:index]
		}
	}
	return ""
}

func resolveExternalAgentProject(projectRoots map[string]string, project string) (string, error) {
	if strings.TrimSpace(project) == "" {
		return "", errors.New("project must not be empty")
	}
	projectPath, exists := projectRoots[project]
	if !exists {
		return "", fmt.Errorf("project %q is not registered", project)
	}
	canonical, err := canonicalProjectPath(projectPath)
	if err != nil {
		return "", fmt.Errorf("resolve project %q: %w", project, err)
	}
	return canonical, nil
}

func canonicalProjectPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("registered project path must be absolute")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(canonical), nil
}

func newAgentToolAgent(definition agentdef.AgentDef, globalInstruction string, childModel model.LLM, tools []tool.Tool) (agent.Agent, error) {
	return newAgentToolAgentWithContext(definition, globalInstruction, childModel, tools, nil)
}

// agentCLIInputSchema is the argument object an agent CLI leaf exposes to its
// caller. It keeps the structured durable delegation shape used by the root model.
//
// Declaring it replaces ADK's default single "request" string. That default is
// what forced every CLI to run in the application root, because a free-text
// argument cannot name a workspace the host is willing to trust.
func agentCLIInputSchema() *genai.Schema {
	return &genai.Schema{
		Type: "OBJECT",
		Properties: map[string]*genai.Schema{
			"project": {Type: "STRING", Description: "registered project name to use as the workspace"},
			"task":    {Type: "STRING", Description: "complete bounded task for the agent CLI"},
		},
		Required: []string{"project", "task"},
	}
}

func newAgentCLIToolAgent(definition agentdef.AgentDef, globalInstruction string, childModel model.LLM) (agent.Agent, error) {
	return newAgentToolAgentWithSchema(definition, globalInstruction, childModel, agentCLIInputSchema())
}

func newAgentToolAgentWithSchema(definition agentdef.AgentDef, globalInstruction string, childModel model.LLM, inputSchema *genai.Schema) (agent.Agent, error) {
	return newAgentToolAgentFull(definition, globalInstruction, childModel, nil, nil, inputSchema)
}

func newAgentToolAgentWithContext(definition agentdef.AgentDef, globalInstruction string, childModel model.LLM, tools []tool.Tool, contextConfig *agentToolContextConfig) (agent.Agent, error) {
	return newAgentToolAgentFull(definition, globalInstruction, childModel, tools, contextConfig, nil)
}

func newAgentToolAgentFull(
	definition agentdef.AgentDef,
	globalInstruction string,
	childModel model.LLM,
	tools []tool.Tool,
	contextConfig *agentToolContextConfig,
	inputSchema *genai.Schema,
) (agent.Agent, error) {
	instruction := definition.Instruction
	includeContents := llmagent.IncludeContentsDefault
	if definition.IncludeContents == "none" {
		includeContents = llmagent.IncludeContentsNone
	}
	cfg := llmagent.Config{
		Name:                     definition.Name,
		Description:              definition.Description,
		Model:                    &agentToolNonStreamingModel{delegate: childModel},
		InstructionProvider:      func(agent.ReadonlyContext) (string, error) { return instruction, nil },
		GlobalInstruction:        globalInstruction,
		IncludeContents:          includeContents,
		Mode:                     llmagent.ModeChat,
		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
	}
	if inputSchema != nil {
		cfg.InputSchema = inputSchema
	}
	if len(tools) > 0 {
		cfg.Tools = tools
	}
	if contextConfig != nil && contextConfig.compiler != nil {
		// Scoped child agents never receive root retrieval knowledge or a
		// workstream revision: retrieval is root-only for V1.
		cfg.BeforeModelCallbacks = append(cfg.BeforeModelCallbacks,
			adkagent.CompilerBeforeModelCallback(adkagent.CompilerCallbackConfig{
				Compiler: contextConfig.compiler, RequestModel: childModel, Stream: false,
				Budget: contextConfig.budget, Actor: contextConfig.actor,
			}))
	}
	return llmagent.New(cfg)
}

func childContextConfig(child preparedAgentTool, actor string) *agentToolContextConfig {
	if child.contextCompiler == nil {
		return nil
	}
	return &agentToolContextConfig{compiler: child.contextCompiler, budget: child.contextBudget, actor: actor}
}

func sortedDeclarativeNames(tools map[string]tooldef.ToolDef) []string {
	if len(tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// childDeclarativeTools returns the fixed read-only allowlist plus the
// declarative tools the child declares in its own tool_scope. A child may only
// reference tools that are active for this deployment.
func (f *compositeAgentToolFactory) childDeclarativeTools(provider declarativeToolProvider, definition agentdef.AgentDef, fixed []tool.Tool) ([]tool.Tool, error) {
	result := append([]tool.Tool(nil), fixed...)
	names := make([]string, 0, len(definition.ToolScope))
	for _, scope := range definition.ToolScope {
		if scope == "invocation_scoped" || isReadOnlyChildTool(scope) {
			continue
		}
		if _, active := f.declarativeTools[scope]; !active {
			return nil, fmt.Errorf("tool_scope references undeclared tool %q", scope)
		}
		names = append(names, scope)
	}
	sort.Strings(names)
	for _, name := range names {
		if provider == nil {
			return nil, fmt.Errorf("declarative tool %q is configured without a tool provider", name)
		}
		declared, err := provider.DeclarativeToolByName(name)
		if err != nil {
			return nil, fmt.Errorf("build declarative tool %q: %w", name, err)
		}
		result = append(result, declared)
	}
	return result, nil
}

// generateAgentCLIText runs one agent CLI turn outside ADK. A durable job has
// no ADK session: the worker owns the turn, so the delegation is built here in
// the same {project, task} shape the in-session leaf receives.
func generateAgentCLIText(ctx context.Context, childModel model.LLM, globalInstruction, instruction, project, task string) (string, error) {
	if childModel == nil {
		return "", errors.New("agent CLI model is unavailable")
	}
	arguments, err := json.Marshal(map[string]string{"project": project, "task": task})
	if err != nil {
		return "", fmt.Errorf("encode agent CLI delegation: %w", err)
	}
	request := &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText(string(arguments), genai.RoleUser)},
	}
	if combined := combineInstructions(globalInstruction, instruction); combined != "" {
		request.Config = &genai.GenerateContentConfig{SystemInstruction: genai.NewContentFromText(combined, genai.RoleUser)}
	}
	for response, err := range childModel.GenerateContent(ctx, request, false) {
		if err != nil {
			return "", err
		}
		if response == nil || response.Content == nil {
			continue
		}
		var text strings.Builder
		for _, part := range response.Content.Parts {
			text.WriteString(part.Text)
		}
		return strings.TrimSpace(text.String()), nil
	}
	return "", errors.New("agent CLI produced no response")
}

func combineInstructions(globalInstruction, instruction string) string {
	parts := make([]string, 0, 2)
	if trimmed := strings.TrimSpace(globalInstruction); trimmed != "" {
		parts = append(parts, trimmed)
	}
	if trimmed := strings.TrimSpace(instruction); trimmed != "" {
		parts = append(parts, trimmed)
	}
	return strings.Join(parts, "\n\n")
}

// newAgentCLIDurableTool exposes an agent CLI leaf that runs as a durable job.
//
// It uses structured {project, task} arguments, an optional confirmation gate,
// and an acceptance receipt. The root turn ends once
// the job is accepted; the durable worker produces the result and Slack
// receives it later.
func newAgentCLIDurableTool(
	definition agentdef.AgentDef,
	resolved *agentdef.ResolvedModel,
	projectRoots map[string]string,
	timeout time.Duration,
	registryRevision string,
	jobStarter port.ExternalAgentJobStarter,
	completionBindings port.ExternalAgentJobCompletionBindingResolver,
	actor string,
	key domain.ConversationKey,
) (tool.Tool, error) {
	if resolved == nil {
		return nil, errors.New("agent CLI profile is required")
	}
	if len(projectRoots) == 0 {
		return nil, errors.New("agent CLI durable tools require at least one registered sandbox project")
	}
	requiresConfirmation := definition.Confirmation == "required"
	description := definition.Description
	if requiresConfirmation {
		description += " Requires confirmation because the agent CLI may modify files and run commands within its approval policy."
	}
	return functiontool.New(functiontool.Config{
		Name:        definition.Name,
		Description: description,
	}, func(ctx agent.Context, args externalAgentArgs) (externalAgentResult, error) {
		primaryPath, err := resolveExternalAgentProject(projectRoots, args.Project)
		if err != nil {
			return externalAgentResult{}, err
		}
		if strings.TrimSpace(args.Task) == "" {
			return externalAgentResult{}, errors.New("agent CLI task must not be empty")
		}
		if requiresConfirmation {
			confirmation := ctx.ToolConfirmation()
			if confirmation == nil {
				hint, payload := externalAgentDelegationConfirmation(ctx, completionBindings, actor, key, args)
				if err := ctx.RequestConfirmation(hint, payload); err != nil {
					return externalAgentResult{}, err
				}
				return externalAgentResult{}, nil
			}
			if !confirmation.Confirmed {
				return externalAgentResult{}, errors.New("agent CLI delegation confirmation was rejected")
			}
		}
		if jobStarter == nil || actor == "" || key == "" || registryRevision == "" {
			return externalAgentResult{}, errors.New("durable agent CLI execution is not configured for this invocation")
		}
		request := domain.ExternalAgentJobRequest{
			Provider: resolved.Provider.Name, Profile: definition.Model, PrimaryProject: args.Project,
			RegistryRevision: registryRevision, Task: args.Task, Mode: domain.JobDetached,
			Timeout: timeout, PrimaryPath: primaryPath,
			WrapperCallID: ctxFunctionCallID(ctx), OriginalCallID: ctxFunctionCallID(ctx), Actor: actor,
			TeamID: teamIDFromConversation(key), ConversationKey: key,
		}
		if completionBindings != nil {
			binding, found, bindingErr := completionBindings.CompletionBindingForTask(ctx, actor, key, args.Project, args.Task)
			if bindingErr != nil {
				return externalAgentResult{}, fmt.Errorf("resolve external-agent completion binding: %w", bindingErr)
			}
			if found {
				request.WorkstreamID = binding.WorkstreamID
				request.TaskID = binding.TaskID
				request.ExecutionIdentity = binding.ExecutionIdentity
				request.AdmissionRevision = binding.AdmissionRevision
			}
		}
		job, err := jobStarter.Start(ctx, request)
		if err != nil {
			return externalAgentResult{}, err
		}
		encoded, _ := json.Marshal(map[string]any{"status": "accepted", "job_id": job.ID, "request_sha256": job.RequestSHA256})
		return externalAgentResult{Result: string(encoded)}, nil
	})
}
