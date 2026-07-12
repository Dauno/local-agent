package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type Config struct {
	Recall      domain.MemoryRecallConfig
	Limits      domain.MemoryLimits
	MaxPatchOps int
}

type Dependencies struct {
	Store           port.MemoryStore
	Logger          port.Logger
	SanitizeContent func(string) string
}

type Outcome string

const (
	OutcomeRecallEmpty   Outcome = "recall_empty"
	OutcomeRecallHit     Outcome = "recall_hit"
	OutcomeRecallError   Outcome = "recall_error"
	OutcomeApplyCreated  Outcome = "apply_created"
	OutcomeApplyUpdated  Outcome = "apply_updated"
	OutcomeApplyNoop     Outcome = "apply_noop"
	OutcomeApplyRejected Outcome = "apply_rejected"
)

type Service struct {
	cfg      Config
	store    port.MemoryStore
	logger   port.Logger
	sanitize func(string) string
}

func New(cfg Config, deps Dependencies) (*Service, error) {
	if deps.Store == nil {
		return nil, errors.New("memory store is required")
	}
	if deps.Logger == nil {
		return nil, errors.New("logger is required")
	}
	if cfg.MaxPatchOps <= 0 || cfg.Limits.MaxTopics <= 0 || cfg.Limits.MaxTopicChars <= 0 || cfg.Limits.MaxLinks < 0 {
		return nil, errors.New("memory limits must be positive (max links may be zero)")
	}
	if deps.SanitizeContent == nil {
		deps.SanitizeContent = func(value string) string { return value }
	}
	return &Service{cfg: cfg, store: deps.Store, logger: deps.Logger, sanitize: deps.SanitizeContent}, nil
}

func (s *Service) Recall(ctx context.Context, query string) ([]domain.MemorySnippet, error) {
	snippets, _, err := s.recall(ctx, query)
	return snippets, err
}

// RelevantTopics supplies a bounded set of existing topic identities and
// revisions to the curator; the model never has to invent revision numbers.
func (s *Service) RelevantTopics(ctx context.Context, messages []domain.Message) ([]domain.TopicReference, error) {
	queries := domain.EntityMemorySearchQueries(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == domain.RoleUser && strings.TrimSpace(messages[i].Content) != "" {
			queries = append(queries, messages[i].Content)
			break
		}
	}
	seen := make(map[string]struct{})
	result := make([]domain.TopicReference, 0, s.cfg.Recall.MaxTopics)
	for _, query := range queries {
		if len(result) == s.cfg.Recall.MaxTopics {
			break
		}
		topics, err := s.store.SearchTopicReferences(ctx, query, s.cfg.Recall.MaxTopics-len(result))
		if err != nil {
			return nil, err
		}
		for _, topic := range topics {
			if _, exists := seen[topic.Slug]; exists {
				continue
			}
			seen[topic.Slug] = struct{}{}
			result = append(result, topic)
			if len(result) == s.cfg.Recall.MaxTopics {
				break
			}
		}
	}
	return result, nil
}

// TrustedEntityOperations resolves candidate entity slugs exactly so an
// existing entity is revised even when FTS recall is capped or misses it.
func (s *Service) TrustedEntityOperations(ctx context.Context, messages []domain.Message) ([]domain.MemoryOp, error) {
	candidates := domain.EntityMemoryCandidates(messages)
	topics := make([]domain.TopicReference, 0, len(candidates))
	for _, candidate := range candidates {
		topic, err := s.store.GetTopicReference(ctx, candidate.Slug)
		if err != nil {
			return nil, err
		}
		if topic != nil {
			topics = append(topics, *topic)
		}
	}
	return domain.TrustedEntityMemoryOperations(messages, topics), nil
}

func (s *Service) recall(ctx context.Context, query string) ([]domain.MemorySnippet, Outcome, error) {
	if !s.cfg.Recall.Enabled || strings.TrimSpace(query) == "" {
		return nil, OutcomeRecallEmpty, nil
	}
	if s.cfg.Recall.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.Recall.Timeout)
		defer cancel()
	}
	snippets, err := s.store.SearchTopics(ctx, query, s.cfg.Recall.MaxTopics, s.cfg.Recall.MaxChars)
	if err != nil {
		s.logger.Warn("memory recall failed", "error", err)
		return nil, OutcomeRecallError, err
	}
	if len(snippets) == 0 {
		return nil, OutcomeRecallEmpty, nil
	}
	s.logger.Debug("memory recall matched", "topics", len(snippets))
	return snippets, OutcomeRecallHit, nil
}

func (s *Service) ValidateAndApply(ctx context.Context, patch domain.MemoryPatch) (Outcome, error) {
	if len(patch.Operations) == 0 {
		return OutcomeApplyNoop, nil
	}
	if strings.TrimSpace(string(patch.ConversationKey)) == "" || strings.TrimSpace(patch.ExchangeTS) == "" {
		return OutcomeApplyRejected, errors.New("patch must reference a source conversation exchange")
	}
	if err := s.validatePatch(patch); err != nil {
		return OutcomeApplyRejected, err
	}
	patch = s.redactPatch(patch) // Apply redaction only after raw credentials and control text were rejected.
	applied, err := s.store.ApplyMemoryPatch(ctx, patch, s.cfg.Limits)
	if err != nil {
		return OutcomeApplyRejected, err
	}
	if !applied {
		return OutcomeApplyNoop, nil
	}
	for _, op := range patch.Operations {
		if op.Type == domain.MemoryOpCreateTopic {
			return OutcomeApplyCreated, nil
		}
	}
	return OutcomeApplyUpdated, nil
}

// Validate checks a proposed patch without writing it, allowing optional
// curator output to be discarded safely when it makes a merged patch unsafe.
func (s *Service) Validate(patch domain.MemoryPatch) error {
	return s.validatePatch(patch)
}

func (s *Service) validatePatch(patch domain.MemoryPatch) error {
	reasons := make([]string, 0)
	add := func(format string, args ...any) { reasons = append(reasons, fmt.Sprintf(format, args...)) }
	if len(patch.Operations) > s.cfg.MaxPatchOps {
		add("patch has %d operations; maximum is %d", len(patch.Operations), s.cfg.MaxPatchOps)
	}
	for _, field := range []struct{ name, value string }{
		{"conversation key", string(patch.ConversationKey)}, {"exchange timestamp", patch.ExchangeTS}, {"source author", patch.SourceAuthorID},
	} {
		if err := domain.ValidateMemoryReferenceText(field.value); err != nil {
			add("patch: %s: %v", field.name, err)
		}
	}
	for i, op := range patch.Operations {
		prefix := fmt.Sprintf("operation %d (%s)", i, op.Type)
		if err := domain.ValidateMemoryReferenceText(op.Type); err != nil {
			add("%s: operation type: %v", prefix, err)
		}
		if !domain.ValidMemoryOps[op.Type] {
			add("%s: unknown operation type %q", prefix, op.Type)
			continue
		}
		if err := domain.ValidateSlug(op.TopicSlug); err != nil {
			add("%s: %v", prefix, err)
		}
		for _, field := range []struct{ name, value string }{
			{"topic slug", op.TopicSlug}, {"target topic slug", op.TargetTopicSlug},
			{"topic title", op.TopicTitle}, {"topic description", op.TopicDesc}, {"content", op.Content},
			{"change reason", op.ChangeReason}, {"decision", op.Decision}, {"question", op.Question},
			{"link relation", op.LinkRelation},
		} {
			if err := domain.ValidateMemoryReferenceText(field.value); err != nil {
				add("%s: %s: %v", prefix, field.name, err)
			}
		}
		for _, tag := range op.Tags {
			if err := domain.ValidateMemoryReferenceText(tag); err != nil {
				add("%s: tag: %v", prefix, err)
			}
		}
		switch op.Type {
		case domain.MemoryOpCreateTopic:
			if err := domain.ValidateTopicTitle(op.TopicTitle); err != nil {
				add("%s: %v", prefix, err)
			}
			if err := domain.ValidateTopicContent(op.Content, s.cfg.Limits.MaxTopicChars); err != nil {
				add("%s: %v", prefix, err)
			}
		case domain.MemoryOpRevise, domain.MemoryOpCorrect:
			if op.ExpectedRev <= 0 {
				add("%s: expected_rev must be positive", prefix)
			}
			if err := domain.ValidateTopicContent(op.Content, s.cfg.Limits.MaxTopicChars); err != nil {
				add("%s: %v", prefix, err)
			}
		case domain.MemoryOpDecide:
			if op.ExpectedRev <= 0 {
				add("%s: expected_rev must be positive", prefix)
			}
			if strings.TrimSpace(op.Decision) == "" {
				add("%s: decision text must not be empty", prefix)
			}
		case domain.MemoryOpQuestionAdd, domain.MemoryOpQuestionResolve:
			if op.ExpectedRev <= 0 {
				add("%s: expected_rev must be positive", prefix)
			}
			if strings.TrimSpace(op.Question) == "" {
				add("%s: question text must not be empty", prefix)
			}
		case domain.MemoryOpLinkAdd, domain.MemoryOpLinkRemove:
			if op.ExpectedRev <= 0 {
				add("%s: expected_rev must be positive", prefix)
			}
			if err := domain.ValidateSlug(op.TargetTopicSlug); err != nil {
				add("%s: target topic: %v", prefix, err)
			}
			if op.Type == domain.MemoryOpLinkAdd && strings.TrimSpace(op.LinkRelation) == "" {
				add("%s: link relation must not be empty", prefix)
			}
		}
	}
	if len(reasons) > 0 {
		return &domain.MemoryValidationError{Reasons: reasons}
	}
	return nil
}

func (s *Service) redactPatch(patch domain.MemoryPatch) domain.MemoryPatch {
	patch.Operations = append([]domain.MemoryOp(nil), patch.Operations...)
	patch.SourceAuthorID = s.sanitize(patch.SourceAuthorID)
	for i := range patch.Operations {
		op := &patch.Operations[i]
		op.Tags = append([]string(nil), op.Tags...)
		op.Type = s.sanitize(op.Type)
		op.TopicSlug = s.sanitize(op.TopicSlug)
		op.TargetTopicSlug = s.sanitize(op.TargetTopicSlug)
		op.TopicTitle = s.sanitize(op.TopicTitle)
		op.TopicDesc = s.sanitize(op.TopicDesc)
		op.Content = s.sanitize(op.Content)
		op.ChangeReason = s.sanitize(op.ChangeReason)
		op.Decision = s.sanitize(op.Decision)
		op.Question = s.sanitize(op.Question)
		op.LinkRelation = s.sanitize(op.LinkRelation)
		for j := range op.Tags {
			op.Tags[j] = s.sanitize(op.Tags[j])
		}
	}
	return patch
}
