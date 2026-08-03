package adkagent

import (
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func CompilerBeforeModelCallback(compiler port.ContextCompiler, budget domain.RequestBudget, continuity port.ContinuityStore, summaries port.SummaryStore, actor string) llmagent.BeforeModelCallback {
	return compilerBeforeModelCallback(compiler, budget, continuity, summaries, nil, domain.ContextCompactionSettings{}, actor)
}

// CompilerBeforeModelCallbackWithSnapshot is the production callback. ADK's
// callback wrapper deliberately does not expose Session(); durable metadata is
// loaded through the supported session service instead.
func CompilerBeforeModelCallbackWithSnapshot(compiler port.ContextCompiler, budget domain.RequestBudget, continuity port.ContinuityStore, summaries port.SummaryStore, sessions session.Service, compaction domain.ContextCompactionSettings, actor string) llmagent.BeforeModelCallback {
	return compilerBeforeModelCallback(compiler, budget, continuity, summaries, sessions, compaction, actor)
}

func compilerBeforeModelCallback(compiler port.ContextCompiler, budget domain.RequestBudget, continuity port.ContinuityStore, summaries port.SummaryStore, sessions session.Service, compaction domain.ContextCompactionSettings, actor string) llmagent.BeforeModelCallback {
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
		openInvocationIDs := visibleOpenInvocationIDs(contents)
		var sessionRevision int64
		if ctx != nil && sessions != nil {
			loaded, snapshotErr := sessions.Get(ctx, &session.GetRequest{AppName: ctx.AppName(), UserID: ctx.UserID(), SessionID: ctx.SessionID()})
			if snapshotErr != nil || loaded == nil || loaded.Session == nil {
				if snapshotErr == nil {
					snapshotErr = errors.New("session snapshot is empty")
				}
				return nil, fmt.Errorf("ADK context snapshot unavailable: %w", snapshotErr)
			}
			events := loaded.Session.Events()
			if events != nil {
				sessionRevision = int64(events.Len())
			}
			if revisioned, ok := loaded.Session.(interface{ Revision() int64 }); ok {
				sessionRevision = revisioned.Revision()
			}
		}
		result, err := compiler.Compile(ctx, domain.CompileRequest{Contents: contents, Continuity: capsule,
			ExistingSummary: summary, Compaction: compaction, ModelBudget: budget, FixedRequestTokens: fixed, Actor: actor,
			ConversationKey: conversationKeyFromSession(ctx), SessionRevision: sessionRevision,
			OpenInvocationIDs: openInvocationIDs})
		if err != nil {
			return nil, err
		}
		request.Contents, err = fromDomainContents(result.Contents)
		return nil, err
	}
}

func visibleOpenInvocationIDs(contents []domain.Content) map[string]struct{} {
	open := make(map[string]struct{})
	for _, content := range contents {
		for _, part := range content.Parts {
			switch {
			case part.FunctionCall != nil && part.FunctionCall.ID != "":
				open[part.FunctionCall.ID] = struct{}{}
			case part.FunctionResponse != nil:
				delete(open, part.FunctionResponse.ID)
			}
		}
	}
	return open
}
