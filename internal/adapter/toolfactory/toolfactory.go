// Package toolfactory creates ADK function tools scoped to an actor and
// conversation. The first tools are read-only; mutable tools are enabled
// only after durable confirmation recovery passes acceptance tests.
package toolfactory

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.AgentToolFactory = (*Factory)(nil)

// Factory implements port.AgentToolFactory by producing typed ADK function
// tools for the invoking actor and conversation.
type Factory struct {
	store  port.ConversationStore
	clock  port.Clock
}

// New creates a tool factory. When the conversation store is nil, no tools
// are produced (text-only mode).
func New(store port.ConversationStore, clock port.Clock) *Factory {
	if store == nil {
		return nil
	}
	if clock == nil {
		clock = systemClock{}
	}
	return &Factory{store: store, clock: clock}
}

// ToolsForInvocation implements port.AgentToolFactory.
func (f *Factory) ToolsForInvocation(actor string, key domain.ConversationKey) []any {
	if f == nil || f.store == nil {
		return nil
	}

	tools := make([]any, 0, 1)

	ro, err := f.listMessagesTool(key)
	if err == nil && ro != nil {
		tools = append(tools, ro)
	}

	return tools
}

// --- read-only tools ---

// listMessagesArgs defines the schema for the list_messages tool.
type listMessagesArgs struct {
	Limit int `json:"limit,omitzero" jsonschema:"maximum number of messages to retrieve (default 5, max 20)"`
}

// listMessagesResult is the structured output of the list_messages tool.
type listMessagesResult struct {
	Messages []messageItem `json:"messages"`
	Count    int            `json:"count"`
}

type messageItem struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

func (f *Factory) listMessagesTool(key domain.ConversationKey) (tool.Tool, error) {
	store := f.store
	conversationKey := key
	return functiontool.New(
		functiontool.Config{
			Name:        "list_messages",
			Description: "Lists recent messages from the current conversation. Read-only — no mutations.",
		},
		func(ctx agent.Context, args listMessagesArgs) (listMessagesResult, error) {
			limit := args.Limit
			if limit <= 0 {
				limit = 5
			}
			if limit > 20 {
				limit = 20
			}

			msgs, err := store.RecentMessages(context.Background(), conversationKey, limit)
			if err != nil {
				return listMessagesResult{}, fmt.Errorf("read messages: %w", err)
			}

			result := listMessagesResult{
				Messages: make([]messageItem, 0, len(msgs)),
				Count:    len(msgs),
			}
			for _, m := range msgs {
				result.Messages = append(result.Messages, messageItem{
					Role:      string(m.Role),
					Content:   m.Content,
					Timestamp: m.CreatedAt.Format(time.RFC3339),
				})
			}
			return result, nil
		},
	)
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
