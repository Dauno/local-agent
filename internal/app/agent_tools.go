package app

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/agenttool"

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
// identity.
type preparedAgentTool struct {
	definition agentdef.AgentDef
	model      model.LLM
	cliTool    tool.Tool
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
	if defs == nil || len(root.AgentTools) == 0 {
		return nil, nil
	}

	prepared := make([]preparedAgentTool, 0, len(root.AgentTools))
	for _, name := range root.AgentTools {
		definition, exists := defs.Agents[name]
		if !exists {
			return nil, fmt.Errorf("agent tool %q is not defined", name)
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
			child, err := newAgentToolAgent(definition, root.GlobalInstruction, childModel, nil)
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
		if _, err := newAgentToolAgent(definition, root.GlobalInstruction, childModel, nil); err != nil {
			return nil, fmt.Errorf("build agent tool %q: %w", name, err)
		}
		prepared = append(prepared, preparedAgentTool{definition: definition, model: childModel})
	}
	return prepared, nil
}

// compositeAgentToolFactory composes the base invocation tool factory with the
// prepared agent-tool children. Agent tools precede the direct root tools in
// one deterministic list; any construction failure fails the whole turn.
type compositeAgentToolFactory struct {
	base              port.AgentToolFactory
	children          []preparedAgentTool
	globalInstruction string
}

var _ port.AgentToolFactory = (*compositeAgentToolFactory)(nil)

func newCompositeAgentToolFactory(base port.AgentToolFactory, children []preparedAgentTool, globalInstruction string) *compositeAgentToolFactory {
	return &compositeAgentToolFactory{
		base:              base,
		children:          children,
		globalInstruction: globalInstruction,
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
	for index, raw := range baseRaw {
		adkTool, ok := raw.(tool.Tool)
		if !ok {
			return nil, fmt.Errorf("invocation tool %d is not an ADK tool: %T", index, raw)
		}
		scoped = append(scoped, adkTool)
	}

	combined := make([]any, 0, len(f.children)+len(baseRaw))
	for _, child := range f.children {
		if child.cliTool != nil {
			combined = append(combined, child.cliTool)
			continue
		}
		childAgent, err := newAgentToolAgent(child.definition, f.globalInstruction, child.model, scoped)
		if err != nil {
			return nil, fmt.Errorf("build agent tool %q: %w", child.definition.Name, err)
		}
		combined = append(combined, agenttool.New(childAgent, &agenttool.Config{}))
	}
	for _, raw := range baseRaw {
		combined = append(combined, raw)
	}
	return combined, nil
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
