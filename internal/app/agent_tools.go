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
	"github.com/Dauno/slack-local-agent/internal/port"
)

// agentToolNonStreamingModel adapts ADK AgentTool's internal SSE runner to a
// text-only model. AgentTool consumes only the completed child result, so no
// streaming semantics are exposed or lost.
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

func buildRootAgentTools(
	ctx context.Context,
	defs *agentdef.Definitions,
	root agentdef.AgentDef,
	values map[string]string,
	cfg config.Config,
	paths config.Paths,
	logger port.Logger,
	sanitize func(string) string,
	describedCLIProviders map[string]bool,
) ([]tool.Tool, error) {
	if defs == nil || len(root.AgentTools) == 0 {
		return nil, nil
	}

	tools := make([]tool.Tool, 0, len(root.AgentTools))
	for _, name := range root.AgentTools {
		definition, exists := defs.Agents[name]
		if !exists {
			return nil, fmt.Errorf("agent tool %q is not defined", name)
		}
		resolved, err := defs.ResolveModel(definition.Model)
		if err != nil {
			return nil, fmt.Errorf("resolve agent tool %q model: %w", name, err)
		}
		if !resolved.IsAgentCLI() {
			return nil, fmt.Errorf("agent tool %q must use an agent_cli provider", name)
		}
		childModel, _, err := newModelForResolved(ctx, resolved, values, cfg, paths, logger, sanitize)
		if err != nil {
			return nil, fmt.Errorf("build agent tool %q model: %w", name, err)
		}
		if err := handshakeSelectedAgentCLI(ctx, resolved, childModel, describedCLIProviders); err != nil {
			return nil, fmt.Errorf("validate agent tool %q model: %w", name, err)
		}

		child, err := newAgentToolAgent(definition, root.GlobalInstruction, childModel)
		if err != nil {
			return nil, fmt.Errorf("build agent tool %q: %w", name, err)
		}
		tools = append(tools, agenttool.New(child, &agenttool.Config{}))
	}
	return tools, nil
}

func newAgentToolAgent(definition agentdef.AgentDef, globalInstruction string, childModel model.LLM) (agent.Agent, error) {
	instruction := definition.Instruction
	includeContents := llmagent.IncludeContentsDefault
	if definition.IncludeContents == "none" {
		includeContents = llmagent.IncludeContentsNone
	}
	return llmagent.New(llmagent.Config{
		Name:                     definition.Name,
		Description:              definition.Description,
		Model:                    &agentToolNonStreamingModel{delegate: childModel},
		InstructionProvider:      func(agent.ReadonlyContext) (string, error) { return instruction, nil },
		GlobalInstruction:        globalInstruction,
		IncludeContents:          includeContents,
		Mode:                     llmagent.ModeChat,
		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
	})
}
