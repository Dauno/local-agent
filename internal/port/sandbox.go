package port

import (
	"context"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// AgentToolFactory creates a set of ADK tools scoped to an actor and
// conversation. It is called once per agent turn before the model call.
// Returning no tools is valid and produces a text-only agent. A non-nil
// error must fail the turn: a partial tool list would make runtime behavior
// differ from validated configuration.
//
// The factory returns []tool.Tool directly (ADK concrete types) to avoid
// the Go generics problem with functiontool construction. The adapter
// only imports the tool package, not the individual tool implementations.
type AgentToolFactory interface {
	ToolsForInvocation(actor string, key domain.ConversationKey) ([]any, error)
}

// ContextEnricher resolves a bounded, structured view of the invoking Slack
// user and conversation before a primary model call. Slack API failures and
// missing scopes must never prevent a normal response.
type ContextEnricher interface {
	Enrich(ctx context.Context, invocation domain.Invocation) (domain.AgentContext, error)
}
