package bot

import (
	"context"
	"errors"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// handleKnowledgeCommand dispatches one memory-human command through the
// consumer-owned knowledge executor. The caller must only invoke it after
// MatchesKnowledge confirmed the message is knowledge traffic, so binding
// resolution never blocks ordinary messages. The executor acquires the shared
// conversation coordinator itself, so this path never acquires it again for
// the same command.
//
// The executor's Enabled state is the single authoritative gate. Disabled
// commands never require binding resolution: the executor answers them with
// the deterministic disabled response. Enabled commands resolve the trusted
// binding first and fail closed on resolution failure with an unavailable
// response instead of executing with a partial binding, which could mutate
// the wrong scope. Binding resolution starts from the trusted invocation and
// only adds host-resolved state (registered project, active actor-bound
// workstream); payload selectors are validated against the trusted registry
// by the resolver and never copied unvalidated.
func (s *Service) handleKnowledgeCommand(ctx context.Context, invocation domain.Invocation, key domain.ConversationKey) (Outcome, bool, error) {
	binding := domain.KnowledgeWriteBinding{Team: invocation.TeamID, Actor: invocation.UserID, Conversation: key}
	if s.knowledge.Enabled() && s.knowledgeBindings != nil {
		resolved, err := s.knowledgeBindings.ResolveKnowledgeBinding(ctx, invocation.TeamID, invocation.UserID, key, invocation.Text)
		if err != nil {
			s.logger.Error("knowledge binding resolution failed", "conversation_key", key, "error", err)
			if _, publishErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), knowledgeUnavailableMessage); publishErr != nil {
				s.logger.Error("knowledge unavailable response failed", "conversation_key", key, "error", publishErr)
				return OutcomePublishFailed, true, nil
			}
			return OutcomeResponded, true, nil
		}
		binding = resolved
	}
	matched, message, err := s.knowledge.Execute(ctx, binding, invocation.EventID, invocation.Text)
	if !matched {
		return "", false, nil
	}
	if err == nil {
		if _, publishErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.sanitize(message)); publishErr != nil {
			s.logger.Error("knowledge response publish failed", "conversation_key", key, "error", publishErr)
			return OutcomePublishFailed, true, nil
		}
		return OutcomeResponded, true, nil
	}
	switch {
	case errors.Is(err, port.ErrKnowledgeBusy):
		s.logger.Info("knowledge command rejected by backpressure", "conversation_key", key)
		if _, publishErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.cfg.BusyMessage); publishErr != nil {
			s.logger.Error("busy response failed", "conversation_key", key, "error", publishErr)
			return OutcomePublishFailed, true, nil
		}
		return OutcomeBusy, true, nil
	case errors.Is(err, port.ErrKnowledgeDisabled):
		if _, publishErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), knowledgeDisabledMessage); publishErr != nil {
			s.logger.Error("knowledge disabled response failed", "conversation_key", key, "error", publishErr)
			return OutcomePublishFailed, true, nil
		}
		return OutcomeResponded, true, nil
	default:
		if _, publishErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.sanitize("Knowledge command rejected: "+err.Error())); publishErr != nil {
			s.logger.Error("knowledge rejection response failed", "conversation_key", key, "error", publishErr)
			return OutcomePublishFailed, true, nil
		}
		return OutcomeResponded, true, nil
	}
}
