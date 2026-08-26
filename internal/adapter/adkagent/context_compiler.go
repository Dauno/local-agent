package adkagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type llmRequestCounter interface {
	CountLLMRequest(context.Context, *model.LLMRequest, bool) (port.TokenCount, error)
}

type providerFrameCounter struct {
	request *model.LLMRequest
	model   llmRequestCounter
	stream  bool
}

func (c providerFrameCounter) CountContextFrame(ctx context.Context, contents []domain.Content) (port.TokenCount, error) {
	projected, err := fromDomainContents(contents)
	if err != nil {
		return port.TokenCount{}, err
	}
	request := *c.request
	request.Contents = projected
	return c.model.CountLLMRequest(ctx, &request, c.stream)
}

// CompilerCallbackConfig carries the per-turn compiler callback inputs.
// Knowledge frame cards and the workstream revision are ephemeral turn data
// owned by the runtime: they flow only into the before-model compile request
// and are never appended to durable events. EpochSink, when non-nil,
// receives the final CompileResult facts of the same model step.
type CompilerCallbackConfig struct {
	Compiler               port.ContextCompiler
	RequestModel           model.LLM
	Stream                 bool
	Budget                 domain.RequestBudget
	Continuity             port.ContinuityStore
	Summaries              port.SummaryStore
	Compaction             domain.ContextCompactionSettings
	Actor                  string
	Knowledge              []domain.KnowledgeFrameCard
	KnowledgeBudgetTokens  int
	Workstream             *domain.WorkstreamSnapshot
	WorkstreamBudgetTokens int
	WorkstreamRevision     int64
	EpochSink              *epochRecorder
}

func CompilerBeforeModelCallback(cfg CompilerCallbackConfig) llmagent.BeforeModelCallback {
	compiler := cfg.Compiler
	requestModel := cfg.RequestModel
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
		if cfg.Continuity != nil && ctx != nil {
			capsule, err = cfg.Continuity.Latest(ctx, ctx.SessionID())
			if err != nil {
				capsule = domain.ContinuityCapsule{}
			}
		}
		var summary string
		var summaryRecord port.SummaryRecord
		if cfg.Summaries != nil && ctx != nil {
			if record, summaryErr := cfg.Summaries.LatestSummary(ctx, ctx.SessionID()); summaryErr == nil {
				summary = record.SanitizedText
				summaryRecord = record
			}
		}
		openInvocationIDs := visibleOpenInvocationIDs(contents)
		compileRequest := domain.CompileRequest{
			Contents: contents, Continuity: capsule,
			ExistingSummary: summary, Compaction: cfg.Compaction, ModelBudget: cfg.Budget, FixedRequestTokens: fixed, Actor: cfg.Actor,
			ConversationKey:        conversationKeyFromSession(ctx),
			OpenInvocationIDs:      openInvocationIDs,
			Knowledge:              cloneKnowledgeCards(cfg.Knowledge),
			KnowledgeBudgetTokens:  cfg.KnowledgeBudgetTokens,
			Workstream:             cfg.Workstream,
			WorkstreamBudgetTokens: cfg.WorkstreamBudgetTokens,
		}
		var result domain.CompileResult
		if exactCompiler, ok := compiler.(port.ContextFrameCompiler); ok {
			if counter, supported := requestModel.(llmRequestCounter); supported {
				result, err = exactCompiler.CompileFrame(ctx, compileRequest, providerFrameCounter{request: request, model: counter, stream: cfg.Stream})
			} else {
				result, err = compiler.Compile(ctx, compileRequest)
			}
		} else {
			result, err = compiler.Compile(ctx, compileRequest)
		}
		if err != nil {
			return nil, err
		}
		// The final facts of this model step reach the epoch recorder from
		// the same callback that produced them; capture later snapshots the
		// final request frame.
		if cfg.EpochSink != nil {
			summaryIdentity := ""
			// The summary identity is the durable source digest and covered
			// ordinal of the summary record, never its text, and only when the
			// summary actually survived into the final admitted frame: a
			// source that was not selected leaves the field empty by outcome,
			// not by omission.
			if result.SummaryIncluded && summaryRecord.SourceDigest != "" {
				summaryIdentity = fmt.Sprintf("%s@%d", summaryRecord.SourceDigest, summaryRecord.CoveredThroughOrdinal)
			}
			cfg.EpochSink.setCompileFacts(result, cfg.WorkstreamRevision, summaryIdentity, extractResultIdentities(result.Contents))
		}
		request.Contents, err = fromDomainContents(result.Contents)
		return nil, err
	}
}

// cloneKnowledgeCards defensively copies the ephemeral retrieval cards so
// callers can never mutate them through the shared AgentRequest slice.
func cloneKnowledgeCards(cards []domain.KnowledgeFrameCard) []domain.KnowledgeFrameCard {
	if len(cards) == 0 {
		return nil
	}
	cloned := make([]domain.KnowledgeFrameCard, len(cards))
	copy(cloned, cards)
	return cloned
}

// extractResultIdentities collects the sorted, deduplicated, bounded set of
// V2 result identities admitted into the final frame: every well-formed
// "result_id" field of a tool FunctionResponse in the final admitted
// contents. It never reads content, and it deliberately does not resolve
// identities embedded in free-form text blocks (such as the TRD 04
// activation frame's JSON-in-text envelope), only the typed structured
// response field TRD 02 result-reading tools already return.
func extractResultIdentities(contents []domain.Content) []string {
	seen := make(map[string]struct{})
	for _, content := range contents {
		for _, part := range content.Parts {
			if part.FunctionResponse == nil {
				continue
			}
			value, ok := part.FunctionResponse.Response["result_id"]
			if !ok {
				continue
			}
			id, ok := value.(string)
			if !ok || !domain.ValidResultID(id) {
				continue
			}
			seen[id] = struct{}{}
		}
	}
	identities := make([]string, 0, len(seen))
	for id := range seen {
		identities = append(identities, id)
	}
	sort.Strings(identities)
	if len(identities) > domain.MaxContextEpochIdentities {
		identities = identities[:domain.MaxContextEpochIdentities]
	}
	return identities
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
