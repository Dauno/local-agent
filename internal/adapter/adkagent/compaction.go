package adkagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type CompactionConfig struct {
	Enabled         bool
	MaxHistoryChars int
	RecentTurns     int
	SummaryEnabled  bool
	SummaryMaxChars int
}

type Projector struct {
	config       CompactionConfig
	summaryStore port.SummaryStore
	logger       port.Logger
}

func (p *Projector) SetSummaryStore(store port.SummaryStore) {
	if p != nil {
		p.summaryStore = store
	}
}

func (p *Projector) SetLogger(logger port.Logger) {
	if p != nil {
		p.logger = logger
	}
}

var _ port.ContextProjector = (*Projector)(nil)

func NewProjector(config CompactionConfig) (*Projector, error) {
	if config.MaxHistoryChars <= 0 || config.RecentTurns <= 0 || config.SummaryMaxChars <= 0 {
		return nil, errors.New("ADK compaction limits must be positive")
	}
	if config.SummaryEnabled && config.SummaryMaxChars >= config.MaxHistoryChars {
		return nil, errors.New("ADK compaction summary limit must be smaller than history limit")
	}
	return &Projector{config: config}, nil
}

func (p *Projector) Project(_ context.Context, request domain.CompactionRequest) (domain.CompactionResult, error) {
	if p == nil {
		return domain.CompactionResult{}, errors.New("ADK compaction projector is nil")
	}
	beforeChars, err := domain.ContentCost(request.Contents)
	if err != nil {
		return domain.CompactionResult{}, fmt.Errorf("measure ADK history: %w", err)
	}
	diagnostics := domain.CompactionDiagnostics{
		HistoryContentsBefore:  len(request.Contents),
		HistoryCharsBefore:     beforeChars,
		ConversationKey:        request.ConversationKey,
		SessionRevision:        request.SessionRevision,
		SystemInstructionChars: request.SystemInstructionChars,
		ToolChars:              request.ToolChars,
	}
	if !p.config.Enabled {
		diagnostics.HistoryContentsAfter = len(request.Contents)
		diagnostics.HistoryCharsAfter = beforeChars
		diagnostics.Reason = "disabled"
		diagnostics.ActiveSuffixContents = len(request.Contents)
		return domain.CompactionResult{Contents: cloneContents(request.Contents), Diagnostics: diagnostics}, nil
	}

	turns, activeStart, err := domain.ClassifyConversationTurns(request.Contents, domain.TurnClassificationOptions{OpenInvocationIDs: request.OpenInvocationIDs})
	if err != nil {
		return domain.CompactionResult{}, fmt.Errorf("classify ADK history: %w", err)
	}
	if len(turns) == 0 {
		diagnostics.HistoryContentsAfter = 0
		diagnostics.Reason = "empty"
		return domain.CompactionResult{Diagnostics: diagnostics}, nil
	}
	if request.ActiveSuffixStart > 0 && request.ActiveSuffixStart < activeStart {
		// A caller may conservatively widen the suffix; it can never narrow the
		// protocol-derived start.
		activeStart = request.ActiveSuffixStart
	}
	maxHistoryChars := p.config.MaxHistoryChars
	if request.MaxHistoryChars > 0 {
		maxHistoryChars = request.MaxHistoryChars
	}
	activeTurnIndex := turnIndexForContentStart(turns, activeStart, len(request.Contents)-turnContentCount(turns))
	if activeTurnIndex < 0 || activeTurnIndex >= len(turns) {
		return domain.CompactionResult{}, errors.New("active ADK suffix does not map to a turn")
	}
	active := cloneContents(request.Contents[activeStart:])
	diagnostics.ActiveSuffixContents = len(active)
	activeChars, err := domain.ContentCost(active)
	if err != nil {
		return domain.CompactionResult{}, fmt.Errorf("measure active ADK history: %w", err)
	}
	if activeChars > maxHistoryChars {
		return domain.CompactionResult{}, &domain.ActiveContextTooLargeError{Chars: activeChars, Budget: maxHistoryChars}
	}
	if err := validateSelectedTurns(turns[activeTurnIndex:], true); err != nil {
		return domain.CompactionResult{}, err
	}

	remaining := maxHistoryChars - activeChars
	recentTurns := p.config.RecentTurns
	if request.RecentTurns > 0 {
		recentTurns = request.RecentTurns
	}
	completed := turns[:activeTurnIndex]
	selected := make([]domain.ConversationTurn, 0, min(recentTurns, len(completed)))
	for index := len(completed) - 1; index >= 0 && len(selected) < recentTurns; index-- {
		turn := completed[index]
		if err := validateSelectedTurns([]domain.ConversationTurn{turn}, false); err != nil {
			return domain.CompactionResult{}, err
		}
		if turn.CharCount > remaining {
			break
		}
		selected = append(selected, turn.Clone())
		remaining -= turn.CharCount
	}
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}

	// Recent raw turns are selected first. A summary is optional context and
	// must never evict a recent turn that already fits the budget.
	var summary []domain.Content
	summaryChars := 0
	summaryFallback := false
	summaryLimit := p.config.SummaryMaxChars
	if request.SummaryMaxChars > 0 {
		summaryLimit = request.SummaryMaxChars
	}
	summaryOrdinalValid := request.ExistingSummaryCoveredOrdinal > 0 && len(completed) > 0 && request.ExistingSummaryCoveredOrdinal <= completed[len(completed)-1].Ordinal
	if p.config.SummaryEnabled && summaryOrdinalValid && strings.TrimSpace(request.ExistingSummary) != "" {
		cleanSummary, sanitizeErr := domain.SanitizeConversationSummary(request.ExistingSummary, summaryLimit)
		if sanitizeErr == nil {
			text := summaryReference(cleanSummary, summaryLimit)
			candidate := []domain.Content{{Role: domain.ContentRoleModel, Parts: []domain.ContentPart{{Text: text}}}}
			candidateChars, costErr := domain.ContentCost(candidate)
			if costErr != nil {
				return domain.CompactionResult{}, fmt.Errorf("measure ADK summary reference: %w", costErr)
			}
			if candidateChars <= remaining {
				summary, summaryChars = candidate, candidateChars
				remaining -= candidateChars
				diagnostics.SummaryPresent = true
				diagnostics.SummaryCoveredOrdinal = request.ExistingSummaryCoveredOrdinal
			} else {
				summaryFallback = true
			}
		} else {
			summaryFallback = true
		}
	} else if strings.TrimSpace(request.ExistingSummary) != "" {
		summaryFallback = true
	}
	selectedContents := domain.FlattenTurns(selected)
	resultContents := append(summary, selectedContents...)
	resultContents = append(resultContents, active...)
	if err := validateProjectedContents(resultContents); err != nil {
		return domain.CompactionResult{}, err
	}
	afterChars, err := domain.ContentCost(resultContents)
	if err != nil {
		return domain.CompactionResult{}, fmt.Errorf("measure projected ADK history: %w", err)
	}
	diagnostics.HistoryContentsAfter = len(resultContents)
	diagnostics.HistoryCharsAfter = afterChars
	diagnostics.RecentTurnsRetained = len(selected)
	diagnostics.CompactionApplied = len(resultContents) != len(request.Contents) || afterChars != beforeChars || summaryChars > 0
	diagnostics.Reason = "bounded"
	if summaryFallback {
		diagnostics.Reason = "bounded_raw_fallback"
	}
	return domain.CompactionResult{Contents: resultContents, Diagnostics: diagnostics}, nil
}

func turnIndexForContentStart(turns []domain.ConversationTurn, contentStart, prefixContents int) int {
	contentOffset := prefixContents
	for index, turn := range turns {
		if contentStart >= contentOffset && contentStart < contentOffset+len(turn.Contents) {
			return index
		}
		contentOffset += len(turn.Contents)
	}
	return -1
}

func turnContentCount(turns []domain.ConversationTurn) int {
	count := 0
	for _, turn := range turns {
		count += len(turn.Contents)
	}
	return count
}

func summaryReference(text string, limit int) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > limit {
		text = string([]rune(text)[:limit])
	}
	return "[UNTRUSTED CONVERSATION SUMMARY REFERENCE]\n" + text + "\n[/UNTRUSTED CONVERSATION SUMMARY REFERENCE]"
}

func cloneContents(contents []domain.Content) []domain.Content {
	clone := make([]domain.Content, len(contents))
	for i, content := range contents {
		clone[i] = content.Clone()
	}
	return clone
}

func validateSelectedTurns(turns []domain.ConversationTurn, active bool) error {
	return validateProtocol(domain.FlattenTurns(turns), active)
}

func validateProtocol(contents []domain.Content, active bool) error {
	return domain.ValidateContentProtocol(contents, domain.ProtocolValidationOptions{
		RequireComplete:            !active,
		AllowConfirmationLifecycle: true,
	})
}

func validateProjectedContents(contents []domain.Content) error {
	turns, activeStart, err := domain.ClassifyConversationTurns(contents, domain.TurnClassificationOptions{})
	if err != nil {
		return fmt.Errorf("validate projected ADK history: %w", err)
	}
	if len(turns) == 0 {
		return nil
	}
	activeTurnIndex := turnIndexForContentStart(turns, activeStart, 0)
	if activeTurnIndex > 0 {
		if err := validateSelectedTurns(turns[:activeTurnIndex], false); err != nil {
			return err
		}
	}
	return validateSelectedTurns(turns[activeTurnIndex:], true)
}

// BeforeModelCallback creates the callback used by the runtime. The callback
// converts to provider-neutral content before projection and converts back only
// after the bounded selection is complete.
func BeforeModelCallback(projector port.ContextProjector) llmagent.BeforeModelCallback {
	return func(ctx agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
		if request == nil {
			return nil, errors.New("ADK model request is nil")
		}
		if projector == nil {
			return nil, errors.New("ADK context projector is nil")
		}
		originalContents := request.Contents
		contents, err := toDomainContents(originalContents)
		if err != nil {
			return nil, err
		}
		compactionRequest := domain.CompactionRequest{Contents: contents, ConversationKey: conversationKeyFromSession(ctx)}
		if request.Config != nil {
			if request.Config.SystemInstruction != nil {
				for _, part := range request.Config.SystemInstruction.Parts {
					if part != nil {
						compactionRequest.SystemInstructionChars += utf8.RuneCountInString(part.Text)
					}
				}
			}
			if len(request.Config.Tools) > 0 {
				if encoded, marshalErr := json.Marshal(request.Config.Tools); marshalErr == nil {
					compactionRequest.ToolChars = utf8.RuneCount(encoded)
				}
			}
		}
		if projectorWithSummary, ok := projector.(*Projector); ok && projectorWithSummary.summaryStore != nil && ctx != nil {
			if summary, summaryErr := projectorWithSummary.summaryStore.LatestSummary(ctx, ctx.SessionID()); summaryErr == nil {
				compactionRequest.ExistingSummary = summary.SanitizedText
				compactionRequest.ExistingSummaryCoveredOrdinal = summary.CoveredThroughOrdinal
			}
		}
		result, err := projector.Project(ctx, compactionRequest)
		if err != nil {
			if concrete, ok := projector.(*Projector); ok && concrete.logger != nil {
				classification := "projection"
				if errors.Is(err, domain.ErrActiveContextTooLarge) {
					classification = "active_context_too_large"
				}
				concrete.logger.Warn("ADK context projection failed", "error_classification", classification)
			}
			return nil, err
		}
		if concrete, ok := projector.(*Projector); ok && concrete.logger != nil {
			concrete.logger.Debug("ADK context projection", "history_contents_before", result.Diagnostics.HistoryContentsBefore,
				"history_chars_before", result.Diagnostics.HistoryCharsBefore, "history_contents_after", result.Diagnostics.HistoryContentsAfter,
				"history_chars_after", result.Diagnostics.HistoryCharsAfter, "recent_turns_retained", result.Diagnostics.RecentTurnsRetained,
				"summary_present", result.Diagnostics.SummaryPresent, "summary_covered_through_ordinal", result.Diagnostics.SummaryCoveredOrdinal,
				"compaction_applied", result.Diagnostics.CompactionApplied, "conversation_key", result.Diagnostics.ConversationKey,
				"session_revision", result.Diagnostics.SessionRevision, "active_suffix_contents", result.Diagnostics.ActiveSuffixContents,
				"system_instruction_chars", result.Diagnostics.SystemInstructionChars, "tool_chars", result.Diagnostics.ToolChars)
		}
		projected, err := fromDomainContents(result.Contents)
		if err != nil {
			return nil, err
		}
		activeCount := result.Diagnostics.ActiveSuffixContents
		if activeCount > 0 && activeCount <= len(originalContents) && activeCount <= len(projected) {
			// The active suffix is protocol state. Reuse the original ADK objects
			// instead of round-tripping their parts through JSON.
			copy(projected[len(projected)-activeCount:], originalContents[len(originalContents)-activeCount:])
		}
		request.Contents = projected
		return nil, nil
	}
}

func conversationKeyFromSession(ctx agent.Context) string {
	if ctx == nil {
		return ""
	}
	return strings.TrimPrefix(ctx.SessionID(), "adk:")
}

func toDomainContents(contents []*genai.Content) ([]domain.Content, error) {
	result := make([]domain.Content, len(contents))
	for i, content := range contents {
		if content == nil {
			return nil, fmt.Errorf("ADK content %d is nil", i)
		}
		role := domain.ContentRole(content.Role)
		if role != domain.ContentRoleUser && role != domain.ContentRoleModel {
			return nil, fmt.Errorf("unsupported ADK content role %q", content.Role)
		}
		result[i] = domain.Content{Role: role, Parts: make([]domain.ContentPart, len(content.Parts))}
		for j, part := range content.Parts {
			if part == nil {
				return nil, fmt.Errorf("ADK content part %d.%d is nil", i, j)
			}
			result[i].Parts[j] = domain.ContentPart{Text: part.Text}
			if part.FunctionCall != nil && part.FunctionResponse == nil && part.ToolCall == nil && part.ToolResponse == nil {
				result[i].Parts[j].FunctionCall = &domain.FunctionCall{ID: part.FunctionCall.ID, Name: part.FunctionCall.Name, Args: cloneAnyMap(part.FunctionCall.Args)}
				continue
			}
			if part.FunctionResponse != nil && part.FunctionCall == nil && part.ToolCall == nil && part.ToolResponse == nil {
				result[i].Parts[j].FunctionResponse = &domain.FunctionResponse{ID: part.FunctionResponse.ID, Name: part.FunctionResponse.Name, Response: cloneAnyMap(part.FunctionResponse.Response), WillContinue: part.FunctionResponse.WillContinue}
				continue
			}
			if part.Text != "" && part.FunctionCall == nil && part.FunctionResponse == nil && part.ToolCall == nil && part.ToolResponse == nil && part.InlineData == nil && part.FileData == nil && part.ExecutableCode == nil && part.CodeExecutionResult == nil {
				continue
			}
			encoded, marshalErr := json.Marshal(part)
			if marshalErr != nil {
				return nil, fmt.Errorf("encode ADK structured content %d.%d: %w", i, j, marshalErr)
			}
			result[i].Parts[j].Text = ""
			result[i].Parts[j].StructuredJSON = encoded
		}
	}
	return result, nil
}

func fromDomainContents(contents []domain.Content) ([]*genai.Content, error) {
	result := make([]*genai.Content, len(contents))
	for i, content := range contents {
		result[i] = &genai.Content{Role: string(content.Role), Parts: make([]*genai.Part, len(content.Parts))}
		for j, part := range content.Parts {
			switch {
			case part.FunctionCall != nil:
				result[i].Parts[j] = &genai.Part{FunctionCall: &genai.FunctionCall{ID: part.FunctionCall.ID, Name: part.FunctionCall.Name, Args: cloneAnyMap(part.FunctionCall.Args)}}
			case part.FunctionResponse != nil:
				result[i].Parts[j] = &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: part.FunctionResponse.ID, Name: part.FunctionResponse.Name, Response: cloneAnyMap(part.FunctionResponse.Response), WillContinue: part.FunctionResponse.WillContinue}}
			case len(part.StructuredJSON) > 0:
				var output genai.Part
				if err := json.Unmarshal(part.StructuredJSON, &output); err != nil {
					return nil, fmt.Errorf("decode projected ADK structured content %d.%d: %w", i, j, err)
				}
				result[i].Parts[j] = &output
			default:
				result[i].Parts[j] = genai.NewPartFromText(part.Text)
			}
		}
	}
	return result, nil
}

func cloneAnyMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = cloneAnyValue(value)
	}
	return output
}

func cloneAnyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAnyMap(typed)
	case []any:
		output := make([]any, len(typed))
		for index, item := range typed {
			output[index] = cloneAnyValue(item)
		}
		return output
	default:
		return value
	}
}

// ConversationTurnsFromEvents converts durable ADK events to provider-neutral
// summary input. It deliberately excludes partial events and marks turns with
// an unresolved confirmation as ineligible for summary generation.
func ConversationTurnsFromEvents(events []*session.Event, redact func(string) string) ([]domain.ConversationTurn, error) {
	contents := make([]*genai.Content, 0, len(events))
	for _, event := range events {
		if event == nil || event.Partial || event.Content == nil {
			continue
		}
		content := cloneGenAIContent(event.Content)
		if redact != nil {
			for _, part := range content.Parts {
				if part == nil {
					continue
				}
				part.Text = redact(part.Text)
				if part.FunctionCall != nil {
					part.FunctionCall.Name = redact(part.FunctionCall.Name)
					part.FunctionCall.Args = redactMap(part.FunctionCall.Args, redact)
				}
				if part.FunctionResponse != nil {
					part.FunctionResponse.Name = redact(part.FunctionResponse.Name)
					part.FunctionResponse.Response = redactMap(part.FunctionResponse.Response, redact)
				}
			}
		}
		contents = append(contents, content)
	}
	converted, err := toDomainContents(contents)
	if err != nil {
		return nil, err
	}
	turns, _, err := domain.ClassifyConversationTurns(converted, domain.TurnClassificationOptions{})
	if err != nil {
		return nil, err
	}
	for index := range turns {
		turns[index].HasOpenConfirmation = hasOpenConfirmation(turns[index])
	}
	if len(turns) > 0 && !turns[len(turns)-1].HasOpenConfirmation && turnHasFinalModelContent(turns[len(turns)-1]) {
		turns[len(turns)-1].Closed = true
	}
	return turns, nil
}

func turnHasFinalModelContent(turn domain.ConversationTurn) bool {
	if len(turn.Contents) == 0 {
		return false
	}
	last := turn.Contents[len(turn.Contents)-1]
	if last.Role != domain.ContentRoleModel {
		return false
	}
	for _, part := range last.Parts {
		if part.FunctionCall != nil || part.FunctionResponse != nil || part.Text == "" {
			return false
		}
	}
	return len(last.Parts) > 0
}

func hasOpenConfirmation(turn domain.ConversationTurn) bool {
	calls := make(map[string]bool)
	responses := make(map[string]bool)
	for _, content := range turn.Contents {
		for _, part := range content.Parts {
			if part.FunctionCall != nil && part.FunctionCall.Name == domain.ConfirmationFunctionName {
				calls[part.FunctionCall.ID] = true
			}
			if part.FunctionResponse != nil && part.FunctionResponse.Name == domain.ConfirmationFunctionName {
				responses[part.FunctionResponse.ID] = true
			}
		}
	}
	for id := range calls {
		if !responses[id] {
			return true
		}
	}
	return false
}

func redactMap(input map[string]any, redact func(string) string) map[string]any {
	if input == nil {
		return nil
	}
	data, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	var output any
	if err := json.Unmarshal([]byte(redact(string(data))), &output); err != nil {
		return nil
	}
	result, _ := output.(map[string]any)
	return result
}

func cloneGenAIContent(content *genai.Content) *genai.Content {
	data, err := json.Marshal(content)
	if err != nil {
		return &genai.Content{Role: content.Role}
	}
	var clone genai.Content
	if json.Unmarshal(data, &clone) != nil {
		return &genai.Content{Role: content.Role}
	}
	return &clone
}

// DurableTurnSource reads the complete ADK session and converts events without
// filtering the durable ledger. It is used only by asynchronous summary work.
type DurableTurnSource struct {
	Service session.Service
	AppName string
	UserID  string
	Redact  func(string) string
}

func (s DurableTurnSource) ClosedTurns(ctx context.Context, sessionIdentity string, afterOrdinal, throughOrdinal int64) ([]domain.ConversationTurn, error) {
	if s.Service == nil || s.AppName == "" || s.UserID == "" || sessionIdentity == "" {
		return nil, errors.New("durable summary turn source is not configured")
	}
	response, err := s.Service.Get(ctx, &session.GetRequest{AppName: s.AppName, UserID: s.UserID, SessionID: sessionIdentity})
	if err != nil {
		return nil, err
	}
	var events []*session.Event
	if response != nil && response.Session != nil && response.Session.Events() != nil {
		for event := range response.Session.Events().All() {
			events = append(events, event)
		}
	}
	turns, err := ConversationTurnsFromEvents(events, s.Redact)
	if err != nil {
		return nil, err
	}
	result := make([]domain.ConversationTurn, 0)
	for _, turn := range turns {
		if turn.Closed && !turn.HasOpenConfirmation && turn.Ordinal > afterOrdinal && turn.Ordinal <= throughOrdinal {
			result = append(result, turn)
		}
	}
	return result, nil
}
