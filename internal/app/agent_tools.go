package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/agenttool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
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
// identity. ACP children carry an ExternalAgentRuntime reference.
type preparedAgentTool struct {
	definition       agentdef.AgentDef
	model            model.LLM
	cliTool          tool.Tool
	acpRuntime       port.ExternalAgentRuntime
	acpResolved      *agentdef.ResolvedModel
	projectRoots     map[string]string
	acpTimeout       time.Duration
	executionMode    string
	registryRevision string
}

type acpAgentArgs struct {
	Project            string   `json:"project" jsonschema:"registered project name to use as the primary workspace"`
	Task               string   `json:"task" jsonschema:"complete bounded task for the external agent"`
	AdditionalProjects []string `json:"additional_projects,omitempty" jsonschema:"optional additional registered project names"`
}

type acpAgentResult struct {
	Result string `json:"result"`
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
	acpRuntimeFactory func(resolved *agentdef.ResolvedModel) (port.ExternalAgentRuntime, error),
) ([]preparedAgentTool, error) {
	if defs == nil || len(root.AgentTools) == 0 {
		return nil, nil
	}

	prepared := make([]preparedAgentTool, 0, len(root.AgentTools))
	for _, name := range root.AgentTools {
		definition, exists := defs.Agents[name]
		if !exists {
			return nil, fmt.Errorf("agent tool %q is not defined", name)
		}

		if definition.AgentClass == "AcpAgent" {
			resolved, err := defs.ResolveModel(definition.Runtime)
			if err != nil {
				return nil, fmt.Errorf("resolve agent tool %q runtime: %w", name, err)
			}
			if acpRuntimeFactory == nil {
				return nil, fmt.Errorf("agent tool %q: ACP runtime factory is not configured", name)
			}
			runtime, err := acpRuntimeFactory(resolved)
			if err != nil {
				return nil, fmt.Errorf("agent tool %q: create ACP runtime: %w", name, err)
			}
			revision, err := agentExecutionFingerprint(definition, resolved, paths.SandboxProjectRoots, cfg)
			if err != nil {
				return nil, fmt.Errorf("fingerprint agent tool %q scope: %w", name, err)
			}
			prepared = append(prepared, preparedAgentTool{
				definition:       definition,
				acpRuntime:       runtime,
				acpResolved:      resolved,
				projectRoots:     paths.SandboxProjectRoots,
				acpTimeout:       acpAgentTimeout(definition, acpAgentFallback(definition, cfg)),
				executionMode:    definition.ExecutionMode,
				registryRevision: revision,
			})
			continue
		}

		resolved, err := defs.ResolveModel(definition.Model)
		if err != nil {
			return nil, fmt.Errorf("resolve agent tool %q model: %w", name, err)
		}
		childModel, _, err := newModelForResolved(ctx, resolved, values, cfg, paths, logger, sanitize)
		if err != nil {
			return nil, fmt.Errorf("build agent tool %q model: %w", name, err)
		}

		if resolved.IsAgentCLI() {
			if err := handshakeSelectedAgentCLI(ctx, resolved, childModel, describedCLIProviders); err != nil {
				return nil, fmt.Errorf("validate agent tool %q model: %w", name, err)
			}
			child, err := newAgentToolAgent(definition, root.EffectiveDelegatedGlobalInstruction(), childModel, nil)
			if err != nil {
				return nil, fmt.Errorf("build agent tool %q: %w", name, err)
			}
			prepared = append(prepared, preparedAgentTool{
				definition: definition,
				model:      childModel,
				cliTool:    agenttool.New(child, &agenttool.Config{}),
			})
			continue
		}

		// Prove the scoped OpenAI-compatible child is representable as an ADK
		// LlmAgent before Socket Mode; per-invocation construction attaches
		// the actor-scoped tools.
		if _, err := newAgentToolAgent(definition, root.EffectiveDelegatedGlobalInstruction(), childModel, nil); err != nil {
			return nil, fmt.Errorf("build agent tool %q: %w", name, err)
		}
		prepared = append(prepared, preparedAgentTool{definition: definition, model: childModel})
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
		// Child agents and workflow steps receive read-only invocation tools only.
		// create_canvas remains available to the root in baseRaw below.
		if adkTool.Name() != "create_canvas" {
			scoped = append(scoped, adkTool)
			toolIndex[adkTool.Name()] = adkTool
		}
	}

	combined := make([]any, 0, len(f.children)+len(f.workflowChildren)+len(baseRaw))
	for _, child := range f.children {
		if child.acpRuntime != nil {
			acpTool, err := newAcpAgentToolForInvocation(child.definition, f.delegatedGlobalInstruction, child.acpRuntime, child.acpResolved, child.projectRoots, child.acpTimeout, child.registryRevision, f.jobStarter, actor, key)
			if err != nil {
				return nil, fmt.Errorf("build ACP agent tool %q: %w", child.definition.Name, err)
			}
			combined = append(combined, acpTool)
			continue
		}
		if child.cliTool != nil {
			combined = append(combined, child.cliTool)
			continue
		}
		childAgent, err := newAgentToolAgent(child.definition, f.delegatedGlobalInstruction, child.model, scoped)
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

	for _, raw := range baseRaw {
		combined = append(combined, raw)
	}
	return combined, nil
}

func newAcpAgentTool(
	definition agentdef.AgentDef,
	globalInstruction string,
	runtime port.ExternalAgentRuntime,
	resolved *agentdef.ResolvedModel,
	projectRoots map[string]string,
	timeout time.Duration,
) (tool.Tool, error) {
	return newAcpAgentToolForInvocation(definition, globalInstruction, runtime, resolved, projectRoots, timeout, "", nil, "", "")
}

func newAcpAgentToolForInvocation(
	definition agentdef.AgentDef,
	globalInstruction string,
	runtime port.ExternalAgentRuntime,
	resolved *agentdef.ResolvedModel,
	projectRoots map[string]string,
	timeout time.Duration,
	registryRevision string,
	jobStarter port.ExternalAgentJobStarter,
	actor string,
	key domain.ConversationKey,
) (tool.Tool, error) {
	if runtime == nil || resolved == nil {
		return nil, errors.New("ACP runtime and profile are required")
	}
	if len(projectRoots) == 0 {
		return nil, errors.New("ACP agent tools require at least one registered sandbox project")
	}
	return functiontool.New(functiontool.Config{
		Name:                definition.Name,
		Description:         definition.Description + " Requires confirmation because OpenCode may modify files, run commands, access configured MCP servers, and use the network within its policy.",
		RequireConfirmation: true,
	}, func(ctx agent.Context, args acpAgentArgs) (acpAgentResult, error) {
		return invokeACPAgentForInvocation(ctx, definition, globalInstruction, runtime, resolved, projectRoots, timeout, registryRevision, jobStarter, actor, key, args)
	})
}

func invokeACPAgent(ctx context.Context, definition agentdef.AgentDef, globalInstruction string, runtime port.ExternalAgentRuntime, resolved *agentdef.ResolvedModel, projectRoots map[string]string, timeout time.Duration, args acpAgentArgs) (acpAgentResult, error) {
	return invokeACPAgentForInvocation(ctx, definition, globalInstruction, runtime, resolved, projectRoots, timeout, "", nil, "", "", args)
}

func invokeACPAgentForInvocation(ctx context.Context, definition agentdef.AgentDef, globalInstruction string, runtime port.ExternalAgentRuntime, resolved *agentdef.ResolvedModel, projectRoots map[string]string, timeout time.Duration, registryRevision string, jobStarter port.ExternalAgentJobStarter, actor string, key domain.ConversationKey, args acpAgentArgs) (acpAgentResult, error) {
	primaryPath, additionalPaths, err := resolveACPProjects(projectRoots, args.Project, args.AdditionalProjects)
	if err != nil {
		return acpAgentResult{}, err
	}
	if strings.TrimSpace(args.Task) == "" {
		return acpAgentResult{}, errors.New("ACP task must not be empty")
	}
	configOptions := make([]domain.ACPConfigOption, 0, len(resolved.ConfigOptions))
	for _, option := range resolved.ConfigOptions {
		configOptions = append(configOptions, domain.ACPConfigOption{ID: option.ID, Value: option.Value})
	}
	if definition.ExecutionMode == agentdef.ExecutionModeDurableJob {
		if jobStarter == nil || actor == "" || key == "" || registryRevision == "" {
			return acpAgentResult{}, errors.New("durable ACP execution is not configured for this invocation")
		}
		job, err := jobStarter.Start(ctx, domain.ExternalAgentJobRequest{
			Provider: resolved.Provider.Name, Profile: definition.Runtime, PrimaryProject: args.Project,
			AdditionalProjects: append([]string(nil), args.AdditionalProjects...), RegistryRevision: registryRevision,
			Task: args.Task, Mode: domain.JobDetached, PermissionOptionKind: resolved.PermissionOptionKind,
			Timeout: timeout, PrimaryPath: primaryPath, AdditionalPaths: additionalPaths,
			WrapperCallID: ctxFunctionCallID(ctx), OriginalCallID: ctxFunctionCallID(ctx), Actor: actor,
			TeamID: teamIDFromConversation(key), ConversationKey: key,
		})
		if err != nil {
			return acpAgentResult{}, err
		}
		encoded, _ := json.Marshal(map[string]any{"status": "accepted", "job_id": job.ID, "request_sha256": job.RequestSHA256})
		return acpAgentResult{Result: string(encoded)}, nil
	}
	result, err := runtime.Run(ctx, domain.AcpInvocationRequest{
		PrimaryProject:     args.Project,
		PrimaryPath:        primaryPath,
		AdditionalProjects: append([]string(nil), args.AdditionalProjects...),
		AdditionalPaths:    additionalPaths,
		ProfileName:        definition.Runtime, ProviderName: resolved.Provider.Name, RegistryRevision: registryRevision,
		ConfigOptions:        configOptions,
		PermissionOptionKind: resolved.PermissionOptionKind,
		GlobalInstruction:    globalInstruction,
		AgentInstruction:     definition.Instruction,
		Task:                 args.Task,
		Timeout:              timeout,
		Actor:                actor, TeamID: teamIDFromConversation(key), ConversationKey: key,
		OriginalCallID: ctxFunctionCallID(ctx),
	})
	if err != nil {
		return acpAgentResult{}, err
	}
	return acpAgentResult{Result: result.Text}, nil
}

func acpAgentTimeout(definition agentdef.AgentDef, fallback time.Duration) time.Duration {
	if definition.TimeoutSeconds > 0 {
		return time.Duration(definition.TimeoutSeconds) * time.Second
	}
	return fallback
}

func acpAgentFallback(definition agentdef.AgentDef, cfg config.Config) time.Duration {
	if definition.ExecutionMode == agentdef.ExecutionModeDurableJob {
		// Detached jobs have their own total budget; root model timeout is unrelated.
		return time.Duration(cfg.ACP.DefaultJobTimeoutSeconds) * time.Second
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

func resolveACPProjects(projectRoots map[string]string, primary string, additional []string) (string, []string, error) {
	if strings.TrimSpace(primary) == "" {
		return "", nil, errors.New("primary project must not be empty")
	}
	primaryPath, exists := projectRoots[primary]
	if !exists {
		return "", nil, fmt.Errorf("project %q is not registered", primary)
	}
	canonicalPrimary, err := canonicalProjectPath(primaryPath)
	if err != nil {
		return "", nil, fmt.Errorf("resolve project %q: %w", primary, err)
	}
	seen := map[string]struct{}{primary: {}}
	additionalPaths := make([]string, 0, len(additional))
	for _, name := range additional {
		if _, duplicate := seen[name]; duplicate {
			return "", nil, fmt.Errorf("project %q is selected more than once", name)
		}
		seen[name] = struct{}{}
		path, exists := projectRoots[name]
		if !exists {
			return "", nil, fmt.Errorf("project %q is not registered", name)
		}
		canonical, err := canonicalProjectPath(path)
		if err != nil {
			return "", nil, fmt.Errorf("resolve project %q: %w", name, err)
		}
		additionalPaths = append(additionalPaths, canonical)
	}
	return canonicalPrimary, additionalPaths, nil
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
	if len(tools) > 0 {
		cfg.Tools = tools
	}
	return llmagent.New(cfg)
}
