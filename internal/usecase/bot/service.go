package bot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

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
	Store           port.ConversationStore
	Agent           port.Agent
	History         port.HistoryReader
	Publisher       port.ResponsePublisher
	Clock           port.Clock
	Logger          port.Logger
	SanitizeContent func(string) string
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
	cfg       Config
	store     port.ConversationStore
	agent     port.Agent
	history   port.HistoryReader
	publisher port.ResponsePublisher
	clock     port.Clock
	logger    port.Logger
	limiter   *Limiter
	sanitize  func(string) string
}

func New(cfg Config, deps Dependencies) (*Service, error) {
	if deps.Store == nil {
		return nil, errors.New("conversation store is required")
	}
	if deps.Agent == nil {
		return nil, errors.New("agent is required")
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
	return &Service{
		cfg: cfg, store: deps.Store, agent: deps.Agent, history: deps.History,
		publisher: deps.Publisher, clock: deps.Clock, logger: deps.Logger,
		limiter: NewLimiter(cfg.MaxConcurrentCalls), sanitize: deps.SanitizeContent,
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
	modelCtx := ctx
	cancel := func() {}
	if s.cfg.ModelTimeout > 0 {
		modelCtx, cancel = context.WithTimeout(ctx, s.cfg.ModelTimeout)
	}
	s.logger.Info("model call started", "conversation_key", key, "event_id", invocation.EventID)
	response, modelErr := s.agent.Respond(modelCtx, modelContext)
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

	safeResponse := s.sanitize(response)
	if strings.TrimSpace(safeResponse) == "" {
		s.logger.Error("model response sanitizer removed all assistant content", "conversation_key", key)
		if _, err := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.cfg.ModelErrorMessage); err != nil {
			s.logger.Error("model error response failed", "conversation_key", key, "error", err)
			return OutcomePublishFailed, nil
		}
		return OutcomeModelFailed, nil
	}
	published, err := s.publisher.Publish(ctx, invocation.ReplyTarget(), safeResponse)
	if err != nil {
		s.logger.Error("assistant response publish failed", "conversation_key", key, "error", err)
		return OutcomePublishFailed, nil
	}
	assistantTS := published.LastMessageTS
	if assistantTS == "" {
		assistantTS = invocation.EventTS
	}
	metadata.LastTS = assistantTS
	assistant := domain.Message{
		Role: domain.RoleAssistant, Content: safeResponse, ExternalTS: assistantTS,
		CreatedAt: s.clock.Now().UTC(),
	}
	if err := s.store.AppendMessage(ctx, metadata, assistant, s.cfg.RetainMessages); err != nil {
		s.logger.Error("assistant message persistence failed", "conversation_key", key, "error", err)
		return "", fmt.Errorf("persist assistant message: %w", err)
	}
	s.logger.Info("Slack invocation completed", "conversation_key", key, "event_id", invocation.EventID)
	return OutcomeResponded, nil
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

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
