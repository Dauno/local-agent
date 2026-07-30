package adkagent

import (
	"encoding/json"
	"errors"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func CompilerBeforeModelCallback(compiler port.ContextCompiler, budget domain.RequestBudget, continuity port.ContinuityStore, summaries port.SummaryStore, actor string) llmagent.BeforeModelCallback {
	return func(ctx agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
		if request == nil || compiler == nil {
			return nil, errors.New("ADK context compiler and request are required")
		}
		contents, err := toDomainContents(request.Contents)
		if err != nil {
			return nil, err
		}
		// The production counter is byte-conservative. Reserve every request
		// component outside Contents plus bounded provider-envelope overhead.
		fixed := 1024
		if encoded, marshalErr := json.Marshal(struct {
			Model  string `json:"model"`
			Config any    `json:"config"`
			Tools  any    `json:"tools"`
		}{Model: request.Model, Config: request.Config, Tools: request.Tools}); marshalErr == nil {
			fixed += len(encoded)
		}
		var capsule domain.ContinuityCapsule
		if continuity != nil && ctx != nil {
			capsule, err = continuity.Latest(ctx, ctx.SessionID())
			if err != nil {
				capsule = domain.ContinuityCapsule{}
			}
		}
		var summary string
		if summaries != nil && ctx != nil {
			if record, summaryErr := summaries.LatestSummary(ctx, ctx.SessionID()); summaryErr == nil {
				summary = record.SanitizedText
			}
		}
		openInvocationIDs := make(map[string]struct{})
		var sessionRevision int64
		if ctx != nil && ctx.Session() != nil && ctx.Session().Events() != nil {
			events := ctx.Session().Events()
			sessionRevision = int64(events.Len())
			if revisioned, ok := ctx.Session().(interface{ Revision() int64 }); ok {
				sessionRevision = revisioned.Revision()
			}
			for event := range events.All() {
				for _, id := range event.LongRunningToolIDs {
					if id != "" {
						openInvocationIDs[id] = struct{}{}
					}
				}
				if event.Content != nil {
					for _, part := range event.Content.Parts {
						if part != nil && part.FunctionResponse != nil {
							delete(openInvocationIDs, part.FunctionResponse.ID)
						}
					}
				}
			}
		}
		result, err := compiler.Compile(ctx, domain.CompileRequest{Contents: contents, Continuity: capsule,
			ExistingSummary: summary, ModelBudget: budget, FixedRequestTokens: fixed, Actor: actor,
			ConversationKey: conversationKeyFromSession(ctx), SessionRevision: sessionRevision,
			OpenInvocationIDs: openInvocationIDs})
		if err != nil {
			return nil, err
		}
		request.Contents, err = fromDomainContents(result.Contents)
		return nil, err
	}
}
