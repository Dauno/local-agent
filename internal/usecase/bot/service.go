package bot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const DefaultDedupeTTL = 7 * 24 * time.Hour

type Config struct {
	AccessPolicy        domain.AccessPolicy
	ContextLimits       domain.ContextLimits
	RetainMessages      int
	MaxConcurrentCalls  int
	ModelTimeout        time.Duration
	BusyMessage         string
	ModelErrorMessage   string
	UnauthorizedMessage string
	DedupeTTL           time.Duration
}

type Dependencies struct {
	Store      port.ConversationStore
	Agent      port.Agent
	Runtime    port.AgentRuntime
	History    port.HistoryReader
	Publisher  port.ResponsePublisher
	Clock      port.Clock
	Logger     port.Logger
	ModelCalls port.ModelCallLimiter

	SanitizeContent   func(string) string
	Memory            port.MemoryRetriever
	Exchange          port.AssistantExchangeWriter
	Enricher          port.ContextEnricher
	ConfirmationStore port.ConfirmationDeliveryStore
}

type Outcome string

const (
	OutcomeResponded       Outcome = "responded"
	OutcomeDenied          Outcome = "denied"
	OutcomeDuplicate       Outcome = "duplicate"
	OutcomeBusy            Outcome = "busy"
	OutcomeIgnoredFollowup Outcome = "ignored_followup"
	OutcomeModelFailed     Outcome = "model_failed"
	OutcomePublishFailed   Outcome = "publish_failed"
)

type Service struct {
	cfg               Config
	store             port.ConversationStore
	agent             port.Agent
	runtime           port.AgentRuntime
	history           port.HistoryReader
	publisher         port.ResponsePublisher
	clock             port.Clock
	logger            port.Logger
	limiter           *Limiter
	modelCalls        port.ModelCallLimiter
	sanitize          func(string) string
	recall            port.MemoryRetriever
	exchange          port.AssistantExchangeWriter
	enricher          port.ContextEnricher
	confirmationStore port.ConfirmationDeliveryStore
}

func New(cfg Config, deps Dependencies) (*Service, error) {
	if deps.Store == nil {
		return nil, errors.New("conversation store is required")
	}
	if deps.Agent == nil && deps.Runtime == nil {
		return nil, errors.New("agent or runtime is required")
	}
	if deps.Publisher == nil {
		return nil, errors.New("response publisher is required")
	}
	if cfg.ContextLimits.MaxMessages <= 0 || cfg.ContextLimits.MaxChars <= 0 {
		return nil, errors.New("context limits must be positive")
	}
	if cfg.RetainMessages <= 0 {
		return nil, errors.New("message retention must be positive")
	}
	if cfg.MaxConcurrentCalls <= 0 {
		return nil, errors.New("maximum concurrent model calls must be positive")
	}
	if cfg.ModelTimeout < 0 {
		return nil, errors.New("model timeout cannot be negative")
	}
	if strings.TrimSpace(cfg.BusyMessage) == "" || strings.TrimSpace(cfg.ModelErrorMessage) == "" || strings.TrimSpace(cfg.UnauthorizedMessage) == "" {
		return nil, errors.New("public runtime messages cannot be empty")
	}
	if cfg.DedupeTTL == 0 {
		cfg.DedupeTTL = DefaultDedupeTTL
	}
	if cfg.DedupeTTL < 0 {
		return nil, errors.New("dedupe TTL cannot be negative")
	}
	if deps.Clock == nil {
		deps.Clock = systemClock{}
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if deps.SanitizeContent == nil {
		deps.SanitizeContent = func(value string) string { return value }
	}
	if deps.ModelCalls == nil {
		deps.ModelCalls = unlimitedModelCalls{}
	}
	return &Service{
		cfg: cfg, store: deps.Store, agent: deps.Agent, runtime: deps.Runtime,
		history: deps.History, publisher: deps.Publisher, clock: deps.Clock, logger: deps.Logger,
		limiter: NewLimiter(cfg.MaxConcurrentCalls), modelCalls: deps.ModelCalls, sanitize: deps.SanitizeContent,
		recall: deps.Memory, exchange: deps.Exchange, enricher: deps.Enricher,
		confirmationStore: deps.ConfirmationStore,
	}, nil
}

func (s *Service) Handle(ctx context.Context, invocation domain.Invocation) (Outcome, error) {
	if err := invocation.Validate(); err != nil {
		return "", fmt.Errorf("invalid invocation: %w", err)
	}

	authorization := s.cfg.AccessPolicy.Authorize(invocation)
	now := s.clock.Now().UTC()
	claimed, err := s.store.ClaimDedupe(ctx, invocation.DedupeKeys(), now, now.Add(s.cfg.DedupeTTL))
	if err != nil {
		s.logger.Error("dedupe claim failed", "event_id", invocation.EventID, "error", err)
		return "", fmt.Errorf("claim Slack invocation: %w", err)
	}
	if !claimed {
		s.logger.Debug("duplicate Slack invocation ignored", "event_id", invocation.EventID)
		return OutcomeDuplicate, nil
	}

	if !authorization.Allowed {
		s.logger.Info("Slack invocation denied", "event_id", invocation.EventID, "user_id", invocation.UserID, "reason", authorization.Reason)
		if _, err := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.cfg.UnauthorizedMessage); err != nil {
			s.logger.Error("authorization response failed", "event_id", invocation.EventID, "error", err)
			return OutcomePublishFailed, nil
		}
		return OutcomeDenied, nil
	}

	// Before the normal agent flow, check if this is a confirmation reply.
	if s.runtime != nil && s.confirmationStore != nil {
		if outcome, ok := s.tryResumeConfirmation(ctx, invocation); ok {
			return outcome, nil
		}
	}

	key, err := invocation.ConversationKey()
	if err != nil {
		return "", err
	}

	var recovered port.History
	if invocation.Trigger == domain.TriggerThreadReply {
		participated, err := s.store.HasAssistantMessage(ctx, key)
		if err != nil {
			s.logger.Error("conversation participation lookup failed", "conversation_key", key, "error", err)
			return "", fmt.Errorf("look up conversation participation: %w", err)
		}
		if !participated {
			recovered, err = s.recoverHistory(ctx, invocation)
			if err != nil || !recovered.BotParticipated {
				if err != nil {
					s.logger.Warn("Slack history could not prove bot participation", "conversation_key", key, "error", err)
				}
				return OutcomeIgnoredFollowup, nil
			}
		}
	}

	release, acquired := s.limiter.TryAcquire(string(key))
	if !acquired {
		s.logger.Info("model call rejected by backpressure", "conversation_key", key)
		if _, err := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.cfg.BusyMessage); err != nil {
			s.logger.Error("busy response failed", "conversation_key", key, "error", err)
			return OutcomePublishFailed, nil
		}
		return OutcomeBusy, nil
	}
	defer release()
	prior, err := s.store.RecentMessages(ctx, key, s.cfg.ContextLimits.MaxMessages)
	if err != nil {
		s.logger.Error("conversation context lookup failed", "conversation_key", key, "error", err)
		return "", fmt.Errorf("load conversation context: %w", err)
	}
	if len(prior) == 0 {
		if len(recovered.Messages) == 0 {
			recovered, err = s.recoverHistory(ctx, invocation)
			if err != nil {
				s.logger.Warn("Slack history recovery failed", "conversation_key", key, "error", err)
			}
		}
		prior = withoutInvocation(recovered.Messages, invocation.EventTS)
	}

	metadata := domain.MetadataFor(invocation, key)
	userMessage := domain.Message{
		Role: domain.RoleUser, Content: invocation.Text, UserID: invocation.UserID,
		ExternalTS: invocation.EventTS, CreatedAt: now,
	}
	persistedUser := userMessage
	persistedUser.Content = s.sanitize(userMessage.Content)
	if err := s.store.AppendMessage(ctx, metadata, persistedUser, s.cfg.RetainMessages); err != nil {
		s.logger.Error("user message persistence failed", "conversation_key", key, "error", err)
		return "", fmt.Errorf("persist accepted user message: %w", err)
	}

	modelContext := domain.LimitMessages(append(prior, userMessage), s.cfg.ContextLimits)

	var memory []domain.MemorySnippet
	if s.recall != nil {
		snippets, err := s.recall.Recall(ctx, invocation.Text, domain.SlackOwnerKey(key, invocation.UserID))
		if err != nil {
			s.logger.Warn("memory recall failed", "event_id", invocation.EventID, "error", err)
		} else {
			memory = domain.FitMemorySnippets(snippets, s.cfg.ContextLimits.MaxChars-messageChars(modelContext))
		}
	}
	agentContext := s.enrich(ctx, invocation)

	modelCtx := ctx
	cancel := func() {}
	if s.cfg.ModelTimeout > 0 {
		modelCtx, cancel = context.WithTimeout(ctx, s.cfg.ModelTimeout)
	}
	modelRelease, modelAcquired := s.modelCalls.TryAcquire()
	if !modelAcquired {
		cancel()
		s.logger.Info("model call rejected by shared backpressure", "conversation_key", key)
		if _, err := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.cfg.BusyMessage); err != nil {
			s.logger.Error("busy response failed", "conversation_key", key, "error", err)
			return OutcomePublishFailed, nil
		}
		return OutcomeBusy, nil
	}
	s.logger.Info("model call started", "conversation_key", key, "event_id", invocation.EventID)

	if s.runtime != nil {
		return s.handleRuntimeTurn(ctx, modelCtx, cancel, invocation, key, modelContext, memory, agentContext, metadata, modelRelease)
	}

	response, modelErr := func() (string, error) {
		defer modelRelease() // Shared permit covers only Agent.Respond, not Slack or database latency.
		return s.agent.Respond(modelCtx, port.AgentRequest{
			Messages: modelContext,
			Memory:   memory,
			Context:  agentContext,
		})
	}()
	cancel()
	if modelErr != nil || strings.TrimSpace(response) == "" {
		if modelErr == nil {
			modelErr = errors.New("model returned an empty response")
		}
		s.logger.Error("model call failed", "conversation_key", key, "error", modelErr)
		if _, err := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.cfg.ModelErrorMessage); err != nil {
			s.logger.Error("model error response failed", "conversation_key", key, "error", err)
			return OutcomePublishFailed, nil
		}
		return OutcomeModelFailed, nil
	}
	s.logger.Info("model call completed", "conversation_key", key, "event_id", invocation.EventID)

	return s.finalizeTurn(ctx, invocation, key, response, metadata)
}

func (s *Service) handleRuntimeTurn(ctx context.Context, modelCtx context.Context, cancel func(), invocation domain.Invocation, key domain.ConversationKey, modelContext []domain.Message, memory []domain.MemorySnippet, agentContext domain.AgentContext, metadata domain.ConversationMetadata, modelRelease func()) (Outcome, error) {
	turn, modelErr := func() (port.AgentTurn, error) {
		defer modelRelease()
		return s.runtime.Run(modelCtx, port.AgentRequest{
			ConversationKey: key,
			Messages:        modelContext,
			Memory:          memory,
			Context:         agentContext,
		})
	}()
	cancel()
	if modelErr != nil {
		s.logger.Error("model call failed", "conversation_key", key, "error", modelErr)
		if _, err := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.cfg.ModelErrorMessage); err != nil {
			s.logger.Error("model error response failed", "conversation_key", key, "error", err)
			return OutcomePublishFailed, nil
		}
		return OutcomeModelFailed, nil
	}
	s.logger.Info("model call completed", "conversation_key", key, "event_id", invocation.EventID)

	if turn.PendingConfirmation != nil {
		return s.handlePendingConfirmation(ctx, invocation, key, turn)
	}

	return s.finalizeTurn(ctx, invocation, key, turn.Text, metadata)
}

func (s *Service) handlePendingConfirmation(ctx context.Context, invocation domain.Invocation, key domain.ConversationKey, turn port.AgentTurn) (Outcome, error) {
	pc := turn.PendingConfirmation
	pc.ConversationKey = key
	pc.Actor = invocation.UserID

	if s.confirmationStore != nil {
		delivery := port.ConfirmationDelivery{
			WrapperCallID:   pc.WrapperCallID,
			OriginalCallID:  pc.OriginalCallID,
			SessionID:       fmt.Sprintf("adk:%s", key),
			Actor:           pc.Actor,
			TeamID:          invocation.TeamID,
			ChannelID:       invocation.ChannelID,
			ThreadTS:        invocation.ReplyTarget().ThreadTS,
			ConversationKey: key,
			Summary:         pc.Summary,
			ParameterHash:   pc.ParameterHash,
			Status:          port.ConfirmationPending,
			Expiry:          pc.Expiry,
		}
		if err := s.confirmationStore.CreateDelivery(ctx, delivery); err != nil {
			s.logger.Error("confirmation delivery creation failed", "conversation_key", key, "error", err)
			if _, pubErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.cfg.ModelErrorMessage); pubErr != nil {
				s.logger.Error("confirmation delivery failure reply failed", "error", pubErr)
				return OutcomePublishFailed, nil
			}
			return OutcomeModelFailed, nil
		}
	}

	// Publish the confirmation prompt as a regular message.
	// Full interactive buttons are deferred (Phase 2 deferred work).
	confirmText := confirmationPrompt(pc.Summary, pc.OriginalCallID, pc.WrapperCallID, pc.Expiry)

	safeText := s.sanitize(confirmText)
	target := invocation.ReplyTarget()
	target.CorrelationID = confirmationCorrelationID(pc.WrapperCallID)
	if _, err := s.publisher.Publish(ctx, target, safeText); err != nil {
		s.logger.Error("confirmation prompt publish failed", "conversation_key", key, "error", err)
		return OutcomePublishFailed, nil
	}

	// Mark the delivery as published if the store is available.
	if s.confirmationStore != nil {
		if err := s.confirmationStore.MarkPublished(ctx, pc.WrapperCallID, target.CorrelationID); err != nil {
			s.logger.Error("confirmation delivery publication marking failed", "wrapper_call_id", pc.WrapperCallID, "error", err)
			return OutcomePublishFailed, nil
		}
	}

	return OutcomeResponded, nil
}

func (s *Service) finalizeTurn(ctx context.Context, invocation domain.Invocation, key domain.ConversationKey, response string, metadata domain.ConversationMetadata) (Outcome, error) {
	safeResponse := s.sanitize(response)
	if strings.TrimSpace(safeResponse) == "" {
		s.logger.Error("model response sanitizer removed all assistant content", "conversation_key", key)
		if _, err := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.cfg.ModelErrorMessage); err != nil {
			s.logger.Error("model error response failed", "conversation_key", key, "error", err)
			return OutcomePublishFailed, nil
		}
		return OutcomeModelFailed, nil
	}
	// Stage the complete exchange before Slack accepts the reply. If finalization
	// later fails, the durable intent is reconciled without losing curation input.
	prepared := port.PreparedAssistantExchange{}
	if s.exchange != nil {
		intentMessage := domain.Message{
			// Slack has not accepted this reply yet, so no timestamp is available.
			Role: domain.RoleAssistant, Content: safeResponse,
			CreatedAt: s.clock.Now().UTC(),
		}
		var prepareErr error
		prepared, prepareErr = s.exchange.PrepareAssistantExchange(ctx, metadata, intentMessage, s.cfg.RetainMessages)
		if prepareErr != nil {
			s.logger.Error("assistant exchange preparation failed", "conversation_key", key, "error", prepareErr)
			return "", fmt.Errorf("prepare assistant exchange: %w", prepareErr)
		}
	}
	target := invocation.ReplyTarget()
	target.CorrelationID = prepared.CorrelationID
	published, err := s.publisher.Publish(ctx, target, safeResponse)
	if err != nil {
		// A transport error can follow Slack accepting the reply; retain the
		// prepared intent so startup reconciliation can prove that outcome.
		s.logger.Error("assistant response publish failed", "conversation_key", key, "error", err)
		return OutcomePublishFailed, nil
	}
	assistantTS := published.LastMessageTS
	if assistantTS == "" {
		return "", errors.New("Slack published a response without a timestamp")
	}
	if s.exchange != nil {
		if err := s.exchange.MarkAssistantExchangePublished(ctx, prepared.ID, assistantTS); err != nil {
			s.logger.Error("assistant exchange publication marking failed", "conversation_key", key, "error", err)
			return "", fmt.Errorf("mark assistant exchange published: %w", err)
		}
		if err := s.exchange.FinalizeAssistantExchange(ctx, prepared.ID); err != nil {
			s.logger.Error("assistant exchange persistence failed", "conversation_key", key, "error", err)
			return "", fmt.Errorf("persist assistant exchange: %w", err)
		}
	} else {
		metadata.LastTS = assistantTS
		assistant := domain.Message{
			Role: domain.RoleAssistant, Content: safeResponse, ExternalTS: assistantTS,
			CreatedAt: s.clock.Now().UTC(),
		}
		if err := s.store.AppendMessage(ctx, metadata, assistant, s.cfg.RetainMessages); err != nil {
			s.logger.Error("assistant message persistence failed", "conversation_key", key, "error", err)
			return "", fmt.Errorf("persist assistant message: %w", err)
		}
	}

	s.logger.Info("Slack invocation completed", "conversation_key", key, "event_id", invocation.EventID)
	return OutcomeResponded, nil
}

func messageChars(messages []domain.Message) int {
	total := 0
	for _, message := range messages {
		total += utf8.RuneCountInString(message.Content)
	}
	return total
}

func (s *Service) AddMemory(recall port.MemoryRetriever, exchange port.AssistantExchangeWriter) {
	s.recall = recall
	s.exchange = exchange
}

func (s *Service) AddRuntime(runtime port.AgentRuntime, confirmations port.ConfirmationDeliveryStore) {
	s.runtime = runtime
	s.confirmationStore = confirmations
}

// ReconcileConfirmations recovers a persisted prompt after a process crash.
// A pending delivery is republished only when Slack history cannot prove the
// deterministic correlation ID was already accepted.
func (s *Service) ReconcileConfirmations(ctx context.Context, finder port.AssistantExchangeFinder) error {
	if s.confirmationStore == nil || finder == nil {
		return nil
	}
	deliveries, err := s.confirmationStore.ListPending(ctx)
	if err != nil {
		return fmt.Errorf("list pending confirmations: %w", err)
	}
	for _, delivery := range deliveries {
		if delivery.Status == port.ConfirmationPublished {
			continue
		}
		correlationID := confirmationCorrelationID(delivery.WrapperCallID)
		prompt := confirmationPrompt(delivery.Summary, delivery.OriginalCallID, delivery.WrapperCallID, delivery.Expiry)
		safePrompt := s.sanitize(prompt)
		channelKind := domain.ChannelDM
		if delivery.ThreadTS != "" {
			channelKind = domain.ChannelPublic
		}
		_, found, err := finder.FindPublishedAssistantExchange(ctx, port.AssistantExchangeIntent{
			ChannelID: delivery.ChannelID, ChannelKind: channelKind, RootTS: delivery.ThreadTS,
			Content: safePrompt, CorrelationID: correlationID,
		})
		if err != nil {
			return fmt.Errorf("find confirmation %s: %w", delivery.WrapperCallID, err)
		}
		if !found {
			if _, err := s.publisher.Publish(ctx, domain.ReplyTarget{
				ChannelID: delivery.ChannelID, ThreadTS: delivery.ThreadTS, CorrelationID: correlationID,
			}, safePrompt); err != nil {
				return fmt.Errorf("republish confirmation %s: %w", delivery.WrapperCallID, err)
			}
		}
		if err := s.confirmationStore.MarkPublished(ctx, delivery.WrapperCallID, correlationID); err != nil {
			return fmt.Errorf("mark confirmation %s published: %w", delivery.WrapperCallID, err)
		}
	}
	return nil
}

func confirmationCorrelationID(wrapperCallID string) string {
	return "confirmation:" + wrapperCallID
}

func confirmationPrompt(summary, originalCallID, wrapperCallID string, expiry time.Time) string {
	return fmt.Sprintf(":lock: %s\n\n**Call ID**: `%s`\n**Expires**: %s\n\nReply `approve %s` or `reject %s` to proceed.",
		summary, originalCallID, expiry.Format("15:04"), wrapperCallID, wrapperCallID)
}

func (s *Service) enrich(ctx context.Context, invocation domain.Invocation) domain.AgentContext {
	if s.enricher == nil {
		return domain.AgentContext{}
	}
	agentCtx, err := s.enricher.Enrich(ctx, invocation)
	if err != nil {
		s.logger.Warn("context enrichment failed", "event_id", invocation.EventID, "error", err)
		return domain.AgentContext{}
	}
	return agentCtx
}

func (s *Service) recoverHistory(ctx context.Context, invocation domain.Invocation) (port.History, error) {
	if s.history == nil {
		return port.History{}, nil
	}
	return s.history.RecentHistory(ctx, invocation, s.cfg.ContextLimits)
}

func withoutInvocation(messages []domain.Message, eventTS string) []domain.Message {
	result := make([]domain.Message, 0, len(messages))
	seen := make(map[string]struct{}, len(messages))
	for _, message := range messages {
		if message.ExternalTS == eventTS {
			continue
		}
		if message.ExternalTS != "" {
			if _, exists := seen[message.ExternalTS]; exists {
				continue
			}
			seen[message.ExternalTS] = struct{}{}
		}
		result = append(result, message)
	}
	return result
}

// tryResumeConfirmation checks whether the incoming message is a confirmation
// reply (approve/reject) and processes it atomically. Returns (Outcome, true)
// when consumed; returns ("", false) when the message is not a confirmation reply.
func (s *Service) tryResumeConfirmation(ctx context.Context, invocation domain.Invocation) (Outcome, bool) {
	text := strings.TrimSpace(invocation.Text)

	var approved bool
	var wrapperCallID string
	var isConfirmation bool

	if after, ok := strings.CutPrefix(text, "approve "); ok {
		approved = true
		wrapperCallID = strings.TrimSpace(after)
		isConfirmation = true
	} else if after, ok := strings.CutPrefix(text, "reject "); ok {
		approved = false
		wrapperCallID = strings.TrimSpace(after)
		isConfirmation = true
	}

	if !isConfirmation || wrapperCallID == "" {
		return "", false
	}

	return s.HandleConfirmation(ctx, invocation, wrapperCallID, approved), true
}

// HandleConfirmation verifies and executes a pending confirmation decision.
func (s *Service) HandleConfirmation(ctx context.Context, invocation domain.Invocation, wrapperCallID string, approved bool) Outcome {
	now := s.clock.Now().UTC()

	delivery, err := s.confirmationStore.GetByWrapperCallID(ctx, wrapperCallID)
	if err != nil {
		s.logger.Error("confirmation lookup failed", "wrapper_call_id", wrapperCallID, "error", err)
		return OutcomeModelFailed
	}
	if delivery == nil {
		s.logger.Warn("confirmation not found", "wrapper_call_id", wrapperCallID)
		if _, pubErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), "Confirmation not found or already processed."); pubErr != nil {
			s.logger.Error("confirmation-not-found reply failed", "error", pubErr)
		}
		return OutcomeIgnoredFollowup
	}

	// Validate actor match.
	if delivery.Actor != invocation.UserID {
		s.logger.Warn("confirmation actor mismatch",
			"expected", delivery.Actor, "got", invocation.UserID)
		if _, pubErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), "Only the original requester can approve this action."); pubErr != nil {
			s.logger.Error("actor-mismatch reply failed", "error", pubErr)
		}
		return OutcomeIgnoredFollowup
	}
	invocationKey, err := invocation.ConversationKey()
	if err != nil {
		s.logger.Error("confirmation conversation key failed", "error", err)
		return OutcomeModelFailed
	}
	if delivery.ConversationKey != invocationKey || delivery.SessionID != fmt.Sprintf("adk:%s", invocationKey) {
		s.logger.Warn("confirmation conversation mismatch", "wrapper_call_id", wrapperCallID)
		if _, pubErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), "This confirmation belongs to a different conversation."); pubErr != nil {
			s.logger.Error("conversation-mismatch reply failed", "error", pubErr)
		}
		return OutcomeIgnoredFollowup
	}

	// Validate not expired.
	if !delivery.Expiry.After(now) {
		s.confirmationStore.ExpireDeliveries(ctx, now)
		if _, pubErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), "This confirmation has expired."); pubErr != nil {
			s.logger.Error("expiry reply failed", "error", pubErr)
		}
		return OutcomeIgnoredFollowup
	}

	// Validate status is consumable (pending or published).
	if delivery.Status != port.ConfirmationPending && delivery.Status != port.ConfirmationPublished {
		s.logger.Warn("confirmation already consumed", "wrapper_call_id", wrapperCallID, "status", delivery.Status)
		if _, pubErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), "This confirmation has already been processed."); pubErr != nil {
			s.logger.Error("already-consumed reply failed", "error", pubErr)
		}
		return OutcomeIgnoredFollowup
	}

	// Execute the resume.
	modelCtx := ctx
	cancel := func() {}
	if s.cfg.ModelTimeout > 0 {
		modelCtx, cancel = context.WithTimeout(ctx, s.cfg.ModelTimeout)
	}
	modelRelease, modelAcquired := s.modelCalls.TryAcquire()
	if !modelAcquired {
		cancel()
		s.logger.Info("confirmation resume rejected by backpressure")
		if _, pubErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.cfg.BusyMessage); pubErr != nil {
			s.logger.Error("busy reply failed", "error", pubErr)
		}
		return OutcomeBusy
	}

	// Claim only after a model permit is available, so a busy response leaves
	// the confirmation pending and retryable. The conditional update prevents
	// a replay from reaching the runtime.
	if approved {
		if err := s.confirmationStore.MarkConsumed(ctx, wrapperCallID); err != nil {
			modelRelease()
			cancel()
			s.logger.Warn("confirmation already consumed (race)", "wrapper_call_id", wrapperCallID, "error", err)
			if _, pubErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), "This confirmation has already been processed."); pubErr != nil {
				s.logger.Error("race reply failed", "error", pubErr)
			}
			return OutcomeIgnoredFollowup
		}
	} else if err := s.confirmationStore.RejectDelivery(ctx, wrapperCallID); err != nil {
		modelRelease()
		cancel()
		s.logger.Warn("confirmation already rejected (race)", "wrapper_call_id", wrapperCallID, "error", err)
		if _, pubErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), "This confirmation has already been processed."); pubErr != nil {
			s.logger.Error("race reply failed", "error", pubErr)
		}
		return OutcomeIgnoredFollowup
	}

	turn, resumeErr := func() (port.AgentTurn, error) {
		defer modelRelease()
		return s.runtime.Resume(modelCtx, domain.ConfirmationDecision{
			WrapperCallID:   delivery.WrapperCallID,
			OriginalCallID:  delivery.OriginalCallID,
			ConversationKey: delivery.ConversationKey,
			Actor:           invocation.UserID,
			Approved:        approved,
		})
	}()
	cancel()
	if resumeErr != nil {
		s.logger.Error("confirmation resume failed", "wrapper_call_id", wrapperCallID, "error", resumeErr)
		if _, pubErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.cfg.ModelErrorMessage); pubErr != nil {
			s.logger.Error("resume-error reply failed", "error", pubErr)
		}
		return OutcomeModelFailed
	}

	safeText := s.sanitize(turn.Text)
	if strings.TrimSpace(safeText) == "" {
		safeText = s.sanitize(fmt.Sprintf("Confirmation %s.", map[bool]string{true: "approved", false: "rejected"}[approved]))
	}
	if _, pubErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), safeText); pubErr != nil {
		s.logger.Error("confirmation result publish failed", "error", pubErr)
		return OutcomePublishFailed
	}

	s.logger.Info("confirmation processed",
		"wrapper_call_id", wrapperCallID,
		"approved", approved,
		"actor", delivery.Actor)
	return OutcomeResponded
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// unlimitedModelCalls preserves standalone bot-service behavior. Runtime
// composition always injects the shared process-wide limiter.
type unlimitedModelCalls struct{}

func (unlimitedModelCalls) TryAcquire() (func(), bool) { return func() {}, true }
