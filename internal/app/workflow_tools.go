package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagents/loopagent"
	"google.golang.org/adk/v2/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/agenttool"
	"google.golang.org/adk/v2/tool/exitlooptool"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type preparedWorkflowTool struct {
	blueprint *agentdef.WorkflowBlueprint
	models    map[string]model.LLM
}

type runnableWorkflowTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	ProcessRequest(agent.Context, *model.LLMRequest) error
	Run(agent.Context, any) (map[string]any, error)
}

// nonEmptyWorkflowTool keeps ADK AgentTool execution intact while enforcing
// the workflow output contract that a successful run returns final text.
type nonEmptyWorkflowTool struct {
	delegate runnableWorkflowTool
}

func (t *nonEmptyWorkflowTool) Name() string        { return t.delegate.Name() }
func (t *nonEmptyWorkflowTool) Description() string { return t.delegate.Description() }
func (t *nonEmptyWorkflowTool) IsLongRunning() bool { return t.delegate.IsLongRunning() }
func (t *nonEmptyWorkflowTool) Declaration() *genai.FunctionDeclaration {
	return t.delegate.Declaration()
}

func (t *nonEmptyWorkflowTool) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if err := t.delegate.ProcessRequest(ctx, req); err != nil {
		return err
	}
	// AgentTool packs itself into the request. Replace only the runnable value;
	// its ADK-generated declaration remains unchanged.
	req.Tools[t.Name()] = t
	return nil
}

func (t *nonEmptyWorkflowTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	result, err := t.delegate.Run(ctx, args)
	if err != nil {
		return nil, err
	}
	text, ok := result["result"].(string)
	if !ok || strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("workflow tool %q produced no final text", t.Name())
	}
	return result, nil
}

type invocationScope struct {
	globalInstruction string
	toolIndex         map[string]tool.Tool
	validateOnly      bool
}

func prepareRootWorkflowTools(
	ctx context.Context,
	defs *agentdef.Definitions,
	root agentdef.AgentDef,
	values map[string]string,
	cfg config.Config,
	paths config.Paths,
	logger port.Logger,
	sanitize func(string) string,
	describedCLIProviders map[string]bool,
	stateDir string,
) ([]preparedWorkflowTool, error) {
	if defs == nil || len(root.WorkflowTools) == 0 {
		return nil, nil
	}

	blueprints := make([]*agentdef.WorkflowBlueprint, 0, len(root.WorkflowTools))
	for _, id := range root.WorkflowTools {
		blueprint, err := defs.LoadWorkflow(stateDir, id)
		if err != nil {
			return nil, fmt.Errorf("load workflow %q: %w", id, err)
		}
		blueprints = append(blueprints, blueprint)
	}
	if err := defs.ValidateWorkflowComposition(root, blueprints, cfg.Sandbox.Enabled); err != nil {
		return nil, err
	}

	models := make(map[string]model.LLM)
	for _, blueprint := range blueprints {
		for _, doc := range blueprint.OrderedDocuments() {
			if doc.AgentClass != agentdef.AgentClassLLM || doc.LLM == nil {
				continue
			}
			modelRef := doc.LLM.Model
			if _, exists := models[modelRef]; exists {
				continue
			}
			resolved, err := defs.ResolveModel(modelRef)
			if err != nil {
				return nil, fmt.Errorf("workflow %q agent %q: resolve model %q: %w", blueprint.ID, doc.Name, modelRef, err)
			}
			childModel, _, err := newModelForResolved(ctx, resolved, values, cfg, paths, logger, sanitize)
			if err != nil {
				return nil, fmt.Errorf("workflow %q agent %q: build model: %w", blueprint.ID, doc.Name, err)
			}
			if err := handshakeSelectedAgentCLI(ctx, resolved, childModel, describedCLIProviders); err != nil {
				return nil, fmt.Errorf("workflow %q agent %q: validate model: %w", blueprint.ID, doc.Name, err)
			}
			models[modelRef] = childModel
		}
	}

	prepared := make([]preparedWorkflowTool, 0, len(blueprints))
	for _, blueprint := range blueprints {
		// Dry-run construction at startup to catch errors before Slack opens.
		if _, err := buildWorkflowAgent(blueprint, models, invocationScope{
			globalInstruction: root.GlobalInstruction,
			validateOnly:      true,
		}); err != nil {
			return nil, fmt.Errorf("workflow %q: build agent tree: %w", blueprint.ID, err)
		}

		prepared = append(prepared, preparedWorkflowTool{
			blueprint: blueprint,
			models:    models,
		})
	}
	return prepared, nil
}

func (p *preparedWorkflowTool) buildAgentTool(scope invocationScope) (tool.Tool, error) {
	if p == nil || p.blueprint == nil {
		return nil, errors.New("workflow tool is not prepared")
	}
	workflowRoot, err := buildWorkflowAgent(p.blueprint, p.models, scope)
	if err != nil {
		return nil, err
	}
	adkTool := agenttool.New(workflowRoot, &agenttool.Config{})
	runnable, ok := adkTool.(runnableWorkflowTool)
	if !ok {
		return nil, fmt.Errorf("workflow tool %q: ADK AgentTool is not runnable", p.blueprint.ID)
	}
	return &nonEmptyWorkflowTool{delegate: runnable}, nil
}

func buildWorkflowAgent(bp *agentdef.WorkflowBlueprint, models map[string]model.LLM, scope invocationScope) (agent.Agent, error) {
	return buildWorkflowNode(bp.Root, bp, models, scope, false)
}

func buildWorkflowNode(doc agentdef.AgentDocument, bp *agentdef.WorkflowBlueprint, models map[string]model.LLM, scope invocationScope, loopAncestor bool) (agent.Agent, error) {
	switch doc.AgentClass {
	case agentdef.AgentClassLLM:
		return buildLLMNode(doc, models, scope, loopAncestor)
	case agentdef.AgentClassSequential:
		return buildSequentialNode(doc, bp, models, scope, loopAncestor)
	case agentdef.AgentClassLoop:
		if doc.Loop == nil {
			return nil, fmt.Errorf("agent %q: LoopAgent document is missing", doc.Name)
		}
		return buildLoopNode(doc, bp, models, scope)
	default:
		return nil, fmt.Errorf("agent %q: unsupported agent class %q", doc.Name, doc.AgentClass)
	}
}

func buildLLMNode(doc agentdef.AgentDocument, models map[string]model.LLM, scope invocationScope, loopAncestor bool) (agent.Agent, error) {
	if doc.LLM == nil {
		return nil, fmt.Errorf("agent %q: LLM document is missing", doc.Name)
	}

	modelRef := doc.LLM.Model
	llm, exists := models[modelRef]
	if !exists {
		return nil, fmt.Errorf("agent %q: model %q not prepared", doc.Name, modelRef)
	}

	includeContents := llmagent.IncludeContentsDefault
	if doc.LLM.IncludeContents == "none" {
		includeContents = llmagent.IncludeContentsNone
	}

	nodeTools, err := resolveWorkflowNodeTools(doc, scope, loopAncestor)
	if err != nil {
		return nil, fmt.Errorf("agent %q: resolve tools: %w", doc.Name, err)
	}

	cfg := llmagent.Config{
		Name:                     doc.Name,
		Description:              doc.Description,
		Model:                    &agentToolNonStreamingModel{delegate: llm},
		Instruction:              doc.LLM.Instruction,
		GlobalInstruction:        scope.globalInstruction,
		IncludeContents:          includeContents,
		OutputKey:                doc.LLM.OutputKey,
		Mode:                     llmagent.ModeChat,
		DisallowTransferToParent: doc.LLM.DisallowTransferToParent,
		DisallowTransferToPeers:  doc.LLM.DisallowTransferToPeers,
		Tools:                    nodeTools,
	}
	return llmagent.New(cfg)
}

func buildSequentialNode(doc agentdef.AgentDocument, bp *agentdef.WorkflowBlueprint, models map[string]model.LLM, scope invocationScope, loopAncestor bool) (agent.Agent, error) {
	children := make([]agent.Agent, 0, len(doc.SubAgents))
	for _, ref := range doc.SubAgents {
		child, err := resolveAndBuildChild(ref, bp, models, scope, loopAncestor)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return sequentialagent.New(sequentialagent.Config{
		AgentConfig: agent.Config{
			Name:        doc.Name,
			Description: doc.Description,
			SubAgents:   children,
		},
	})
}

func buildLoopNode(doc agentdef.AgentDocument, bp *agentdef.WorkflowBlueprint, models map[string]model.LLM, scope invocationScope) (agent.Agent, error) {
	children := make([]agent.Agent, 0, len(doc.SubAgents))
	for _, ref := range doc.SubAgents {
		child, err := resolveAndBuildChild(ref, bp, models, scope, true)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return loopagent.New(loopagent.Config{
		AgentConfig: agent.Config{
			Name:        doc.Name,
			Description: doc.Description,
			SubAgents:   children,
		},
		MaxIterations: uint(doc.Loop.MaxIterations),
	})
}

func resolveAndBuildChild(ref agentdef.AgentRef, bp *agentdef.WorkflowBlueprint, models map[string]model.LLM, scope invocationScope, loopAncestor bool) (agent.Agent, error) {
	childDoc, exists := bp.Documents[ref.Path]
	if !exists {
		return nil, fmt.Errorf("sub_agent %q not found in workflow documents", ref.ConfigPath)
	}
	return buildWorkflowNode(childDoc, bp, models, scope, loopAncestor)
}

func resolveWorkflowNodeTools(doc agentdef.AgentDocument, scope invocationScope, loopAncestor bool) ([]tool.Tool, error) {
	if len(doc.LLM.Tools) == 0 {
		return nil, nil
	}
	result := make([]tool.Tool, 0, len(doc.LLM.Tools))
	for _, toolRef := range doc.LLM.Tools {
		if toolRef.Name == "exit_loop" {
			if !loopAncestor {
				return nil, fmt.Errorf("tool %q is only allowed inside a LoopAgent", toolRef.Name)
			}
			exitTool, err := exitlooptool.New()
			if err != nil {
				return nil, fmt.Errorf("construct exit_loop tool: %w", err)
			}
			result = append(result, exitTool)
			continue
		}
		if scope.validateOnly {
			continue
		}
		t, exists := scope.toolIndex[toolRef.Name]
		if !exists {
			return nil, fmt.Errorf("tool %q is not registered or not available", toolRef.Name)
		}
		result = append(result, t)
	}
	return result, nil
}
