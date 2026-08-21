// Package knowledge owns admission for human knowledge commands. It keeps
// trusted Slack identity (actor, team, conversation, registered project,
// bound workstream) separate from command payloads supplied by a human, and
// serializes every memory-human command through the conversation coordinator
// before any mutation, mirroring workstream-human.
package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type Config struct {
	Enabled bool
	Limits  domain.KnowledgeLimits
}

type Dependencies struct {
	Store       port.KnowledgeStore
	Clock       port.Clock
	Coordinator port.ConversationCoordinator
	Wakes       []func()
}

type Service struct {
	cfg         Config
	store       port.KnowledgeStore
	clock       port.Clock
	coordinator port.ConversationCoordinator
	wakes       []func()
}

var _ port.KnowledgeCommands = (*Service)(nil)

func New(cfg Config, deps Dependencies) (*Service, error) {
	if deps.Store == nil {
		return nil, errors.New("knowledge store is required")
	}
	if deps.Coordinator == nil {
		return nil, errors.New("conversation coordinator is required")
	}
	if deps.Clock == nil {
		deps.Clock = port.SystemClock{}
	}
	cfg.Limits = cfg.Limits.WithDefaults()
	if err := cfg.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("knowledge limits: %w", err)
	}
	return &Service{cfg: cfg, store: deps.Store, clock: deps.Clock, coordinator: deps.Coordinator, wakes: deps.Wakes}, nil
}

// MatchesKnowledge reports whether text is a memory-human command, including
// payloads that will later fail parsing. The bot uses it to recognize
// knowledge traffic before resolving a trusted binding, so ordinary messages
// never touch binding resolution.
func (s *Service) MatchesKnowledge(text string) bool {
	_, matched, _ := ParseHumanCommand(text)
	return matched
}

// Enabled reports the authoritative gate state behind Execute. The bot
// consults it before binding resolution: enabled commands fail closed when
// resolution fails, disabled commands never require resolution.
func (s *Service) Enabled() bool {
	return s.cfg.Enabled
}

// Execute handles one memory-human command. binding and eventID are trusted
// host inputs resolved from verified Slack event metadata; they are never
// taken from model output. The conversation coordinator is acquired before
// any validation or mutation. The returned message is bounded and carries
// only validated identities. matched=false when the text is not a knowledge
// command so callers can fall through.
func (s *Service) Execute(ctx context.Context, binding domain.KnowledgeWriteBinding, eventID, text string) (matched bool, message string, err error) {
	command, matched, err := ParseHumanCommand(text)
	if !matched || err != nil {
		return matched, "", err
	}
	if !s.cfg.Enabled {
		return true, "", port.ErrKnowledgeDisabled
	}
	if err := validateBinding(binding); err != nil {
		return true, "", err
	}
	if strings.TrimSpace(eventID) == "" || eventID != strings.TrimSpace(eventID) {
		return true, "", fmt.Errorf("%w: knowledge commands require a non-empty event identity", port.ErrKnowledgeValidation)
	}
	sourceRef := "slack-human:" + eventID
	if err := domain.ValidateKnowledgeSourceRef(sourceRef); err != nil {
		return true, "", fmt.Errorf("%w: %w", port.ErrKnowledgeValidation, err)
	}
	release, acquired := s.coordinator.TryAcquire(string(binding.Conversation))
	if !acquired {
		return true, "", fmt.Errorf("%w: conversation %q", port.ErrKnowledgeBusy, binding.Conversation)
	}
	defer release()
	switch command.Action {
	case domain.KnowledgeActionRemember:
		message, err = s.remember(ctx, binding, command, sourceRef)
	case domain.KnowledgeActionCorrect:
		message, err = s.correct(ctx, binding, command, sourceRef)
	case domain.KnowledgeActionForget:
		message, err = s.forget(ctx, binding, command, sourceRef)
	case domain.KnowledgeActionArchive:
		message, err = s.archive(ctx, binding, command, sourceRef)
	case domain.KnowledgeActionDispute:
		message, err = s.dispute(ctx, binding, command, sourceRef)
	case domain.KnowledgeActionInspect:
		message, err = s.inspect(ctx, binding, command)
	default:
		return true, "", fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, command.Action)
	}
	if err == nil && command.Action != domain.KnowledgeActionInspect {
		for _, wake := range s.wakes {
			if wake != nil {
				wake()
			}
		}
	}
	return true, message, err
}

// canonicalCommandDigest is the sha256 of the canonical JSON rendering of the
// parsed command. Struct field order is deterministic, so two payloads that
// parse identically always share the digest.
func canonicalCommandDigest(command HumanCommand) string {
	payload, _ := json.Marshal(command)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// commitCommandReceipt commits the global command identity before the target
// mutation: one source reference may carry exactly one canonical payload.
func (s *Service) commitCommandReceipt(ctx context.Context, command HumanCommand, sourceRef, target string) error {
	return s.store.CommitCommandReceipt(ctx, domain.KnowledgeCommandReceipt{
		SourceRef: sourceRef, Action: command.Action,
		PayloadDigest: canonicalCommandDigest(command), Target: target,
	})
}

func validateBinding(binding domain.KnowledgeWriteBinding) error {
	if binding.Conversation == "" {
		return fmt.Errorf("%w: knowledge commands require a canonical conversation key", domain.ErrKnowledgeScopeIdentityRequired)
	}
	if !domain.ValidKnowledgeConversationKey(binding.Conversation) {
		return fmt.Errorf("%w: conversation key %q is not canonical", domain.ErrKnowledgeScopeBindingMismatch, binding.Conversation)
	}
	if strings.TrimSpace(binding.Actor) == "" || !domain.PlausibleUserID(binding.Actor) {
		return fmt.Errorf("%w: knowledge commands require a trusted human actor", domain.ErrKnowledgeScopeBindingMismatch)
	}
	return nil
}

// resolveWriteScope applies the deterministic V1 default: the bound project
// scope when the binding carries a registered project, otherwise the trusted
// actor's user scope. An explicit payload scope is re-validated against the
// trusted binding and grants no access on its own.
func resolveWriteScope(binding domain.KnowledgeWriteBinding, command HumanCommand) (domain.KnowledgeScopeKind, string, error) {
	if command.ScopeKind != "" {
		kind := domain.KnowledgeScopeKind(command.ScopeKind)
		if err := domain.ValidateKnowledgeWriteBinding(domain.KnowledgeSourceHuman, kind, command.ScopeID, binding); err != nil {
			return "", "", err
		}
		return kind, command.ScopeID, nil
	}
	if binding.Project != "" {
		return domain.KnowledgeScopeProject, binding.Project, nil
	}
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)
	if owner == "" {
		return "", "", fmt.Errorf("%w: user scope requires the trusted actor owner key", domain.ErrKnowledgeScopeBindingMismatch)
	}
	return domain.KnowledgeScopeUser, owner, nil
}

func (s *Service) remember(ctx context.Context, binding domain.KnowledgeWriteBinding, command HumanCommand, sourceRef string) (string, error) {
	if command.PreferenceKey != "" {
		return s.rememberPreference(ctx, binding, command, sourceRef)
	}
	value, err := command.value()
	if err != nil {
		return "", fmt.Errorf("%w: %w", port.ErrKnowledgeValidation, err)
	}
	validFrom, validUntil, err := command.validity()
	if err != nil {
		return "", fmt.Errorf("%w: %w", port.ErrKnowledgeValidation, err)
	}
	scopeKind, scopeID, err := resolveWriteScope(binding, command)
	if err != nil {
		return "", err
	}
	claim := domain.KnowledgeClaim{
		Subject: command.Subject, Predicate: domain.KnowledgePredicate(command.Predicate), Value: value,
		ScopeKind: scopeKind, ScopeID: scopeID,
		SourceClass: domain.KnowledgeSourceHuman, SourceRef: sourceRef, AuthorID: binding.Actor,
		Status: domain.KnowledgeClaimVerified, ValidFrom: validFrom, ValidUntil: validUntil,
	}
	if err := claim.ValidateCandidate(s.cfg.Limits, binding); err != nil {
		return "", fmt.Errorf("%w: %w", port.ErrKnowledgeValidation, err)
	}
	digest := domain.KnowledgeSubjectDigest(command.Subject, scopeKind, scopeID)
	if err := s.commitCommandReceipt(ctx, command, sourceRef, "claim:"+digest); err != nil {
		return "", err
	}
	created, err := s.store.CreateClaim(ctx, claim, s.cfg.Limits)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Claim `%s` remembered in scope `%s:%s` at revision `%d`.", created.ID, created.ScopeKind, created.ScopeID, created.Revision), nil
}

func (s *Service) rememberPreference(ctx context.Context, binding domain.KnowledgeWriteBinding, command HumanCommand, sourceRef string) (string, error) {
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)
	if owner == "" {
		return "", fmt.Errorf("%w: preferences require the trusted actor owner key", domain.ErrKnowledgeScopeBindingMismatch)
	}
	value, err := command.value()
	if err != nil {
		return "", fmt.Errorf("%w: %w", port.ErrKnowledgeValidation, err)
	}
	preference := domain.KnowledgePreference{
		OwnerKey: owner, Key: command.PreferenceKey, Value: value,
		Status: domain.KnowledgePreferenceActive, SourceRef: sourceRef,
	}
	if err := preference.ValidateCandidate(s.cfg.Limits, binding); err != nil {
		return "", fmt.Errorf("%w: %w", port.ErrKnowledgeValidation, err)
	}
	if command.ExpectedRevision > 0 {
		if err := s.commitCommandReceipt(ctx, command, sourceRef, "preference:"+owner+":"+command.PreferenceKey); err != nil {
			return "", err
		}
		updated, err := s.store.UpdatePreference(ctx, preference, command.ExpectedRevision, s.cfg.Limits)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Preference `%s` remembered at revision `%d`.", updated.Key, updated.Revision), nil
	}
	if err := s.commitCommandReceipt(ctx, command, sourceRef, "preference:"+owner+":"+command.PreferenceKey); err != nil {
		return "", err
	}
	created, err := s.store.CreatePreference(ctx, preference, s.cfg.Limits)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Preference `%s` remembered at revision `%d`.", created.Key, created.Revision), nil
}

func (s *Service) correct(ctx context.Context, binding domain.KnowledgeWriteBinding, command HumanCommand, sourceRef string) (string, error) {
	prior, err := s.store.GetClaim(ctx, domain.KnowledgeClaimID(command.ClaimID), domain.KnowledgeReadableScopes(binding))
	if err != nil {
		return "", err
	}
	value, err := command.value()
	if err != nil {
		return "", fmt.Errorf("%w: %w", port.ErrKnowledgeValidation, err)
	}
	validFrom, validUntil, err := command.validity()
	if err != nil {
		return "", fmt.Errorf("%w: %w", port.ErrKnowledgeValidation, err)
	}
	predicate := prior.Predicate
	if command.Predicate != "" {
		predicate = domain.KnowledgePredicate(command.Predicate)
	}
	replacement := domain.KnowledgeClaim{
		Subject: prior.Subject, Predicate: predicate, Value: value,
		ScopeKind: prior.ScopeKind, ScopeID: prior.ScopeID,
		SourceClass: domain.KnowledgeSourceHuman, SourceRef: sourceRef, AuthorID: binding.Actor,
		Status: domain.KnowledgeClaimVerified, ValidFrom: validFrom, ValidUntil: validUntil,
		SupersedesID: prior.ID,
	}
	if err := replacement.ValidateCandidate(s.cfg.Limits, binding); err != nil {
		return "", fmt.Errorf("%w: %w", port.ErrKnowledgeValidation, err)
	}
	if err := s.commitCommandReceipt(ctx, command, sourceRef, "claim:"+string(prior.ID)); err != nil {
		return "", err
	}
	created, err := s.store.CorrectClaim(ctx, replacement, domain.KnowledgeSourceHuman, s.cfg.Limits)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Claim `%s` corrected by replacement claim `%s`.", prior.ID, created.ID), nil
}

func (s *Service) forget(ctx context.Context, binding domain.KnowledgeWriteBinding, command HumanCommand, sourceRef string) (string, error) {
	scopeKind, scopeID, err := resolveWriteScope(binding, command)
	if err != nil {
		return "", err
	}
	if err := validateForgetSubject(command.Subject); err != nil {
		return "", fmt.Errorf("%w: %w", port.ErrKnowledgeValidation, err)
	}
	if err := domain.ValidateKnowledgeScope(scopeKind, scopeID, domain.KnowledgeLimits{MaxScopeIDRunes: domain.HardMaxKnowledgeScopeIDRunes}); err != nil {
		return "", fmt.Errorf("%w: %w", port.ErrKnowledgeValidation, err)
	}
	digest := domain.KnowledgeSubjectDigest(command.Subject, scopeKind, scopeID)
	if err := s.commitCommandReceipt(ctx, command, sourceRef, "subject:"+digest); err != nil {
		return "", err
	}
	forgotten, err := s.store.ForgetSubject(ctx, command.Subject, scopeKind, scopeID, sourceRef)
	if err != nil {
		return "", err
	}
	if !forgotten {
		return fmt.Sprintf("Forget for subject `%s` in scope `%s:%s` replayed; tombstone already recorded.", command.Subject, scopeKind, scopeID), nil
	}
	return fmt.Sprintf("Subject `%s` forgotten in scope `%s:%s`; tombstone recorded.", command.Subject, scopeKind, scopeID), nil
}

// validateForgetSubject rejects invalid subjects before the global command
// receipt is committed so a rejected forget never consumes the command
// identity nor persists content the store would later refuse. Forget accepts
// every subject historically persistible up to the hard bound, independent of
// the current configured limit, so a claim admitted under an amplified
// configuration can always be forgotten after a restart with defaults.
func validateForgetSubject(subject string) error {
	if strings.TrimSpace(subject) == "" {
		return fmt.Errorf("%w: forget requires a subject", domain.ErrKnowledgeInvalidValue)
	}
	if utf8.RuneCountInString(subject) > domain.HardMaxKnowledgeSubjectRunes {
		return fmt.Errorf("%w: subject exceeds hard maximum of %d characters", domain.ErrKnowledgeLimitExceeded, domain.HardMaxKnowledgeSubjectRunes)
	}
	if err := domain.ValidateMemoryReferenceText(subject); err != nil {
		return fmt.Errorf("%w: subject: %v", domain.ErrKnowledgeInvalidValue, err)
	}
	return nil
}

func (s *Service) archive(ctx context.Context, binding domain.KnowledgeWriteBinding, command HumanCommand, sourceRef string) (string, error) {
	readable := domain.KnowledgeReadableScopes(binding)
	switch {
	case command.ClaimID != "":
		claim, err := s.store.GetClaim(ctx, domain.KnowledgeClaimID(command.ClaimID), readable)
		if err != nil {
			return "", err
		}
		if err := s.commitCommandReceipt(ctx, command, sourceRef, "claim:"+string(claim.ID)); err != nil {
			return "", err
		}
		updated, err := s.store.TransitionClaimStatus(ctx, claim.ID, command.ExpectedRevision, domain.KnowledgeClaimArchived, domain.KnowledgeSourceHuman, sourceRef)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Claim `%s` archived at revision `%d`.", updated.ID, updated.Revision), nil
	case command.DocumentID != "":
		document, err := s.store.GetDocument(ctx, domain.KnowledgeDocumentID(command.DocumentID), readable)
		if err != nil {
			return "", err
		}
		if err := s.commitCommandReceipt(ctx, command, sourceRef, "document:"+string(document.ID)); err != nil {
			return "", err
		}
		updated, err := s.store.ArchiveDocument(ctx, document.ID, command.ExpectedRevision, sourceRef)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Document `%s` archived at revision `%d`.", updated.ID, updated.Revision), nil
	default:
		owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)
		if owner == "" {
			return "", fmt.Errorf("%w: preferences require the trusted actor owner key", domain.ErrKnowledgeScopeBindingMismatch)
		}
		if err := s.commitCommandReceipt(ctx, command, sourceRef, "preference:"+owner+":"+command.PreferenceKey); err != nil {
			return "", err
		}
		updated, err := s.store.ArchivePreference(ctx, owner, command.PreferenceKey, command.ExpectedRevision, sourceRef)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Preference `%s` archived at revision `%d`.", updated.Key, updated.Revision), nil
	}
}

func (s *Service) dispute(ctx context.Context, binding domain.KnowledgeWriteBinding, command HumanCommand, sourceRef string) (string, error) {
	claim, err := s.store.GetClaim(ctx, domain.KnowledgeClaimID(command.ClaimID), domain.KnowledgeReadableScopes(binding))
	if err != nil {
		return "", err
	}
	if err := s.commitCommandReceipt(ctx, command, sourceRef, "claim:"+string(claim.ID)); err != nil {
		return "", err
	}
	updated, err := s.store.TransitionClaimStatus(ctx, claim.ID, command.ExpectedRevision, domain.KnowledgeClaimDisputed, domain.KnowledgeSourceHuman, sourceRef)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Claim `%s` disputed at revision `%d`.", updated.ID, updated.Revision), nil
}

func (s *Service) inspect(ctx context.Context, binding domain.KnowledgeWriteBinding, command HumanCommand) (string, error) {
	readable := domain.KnowledgeReadableScopes(binding)
	switch {
	case command.ClaimID != "":
		claim, err := s.store.GetClaim(ctx, domain.KnowledgeClaimID(command.ClaimID), readable)
		if err != nil {
			return "", err
		}
		return domain.CardFromClaim(claim, "human inspect", s.clock.Now().UTC()).Render(), nil
	case command.DocumentID != "":
		document, err := s.store.GetDocument(ctx, domain.KnowledgeDocumentID(command.DocumentID), readable)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("document %s [%s:%s; %s; revision %d]", document.Subject, document.ScopeKind, document.ScopeID, document.Status, document.Revision), nil
	case command.PreferenceKey != "":
		owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)
		if owner == "" {
			return "", fmt.Errorf("%w: preferences require the trusted actor owner key", domain.ErrKnowledgeScopeBindingMismatch)
		}
		preference, err := s.store.GetPreference(ctx, owner, command.PreferenceKey)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s = %s [%s; revision %d]", preference.Key, renderPreferenceValue(preference.Value), preference.Status, preference.Revision), nil
	default:
		return s.inspectListing(ctx, binding, readable, command.Subject)
	}
}

func (s *Service) inspectListing(ctx context.Context, binding domain.KnowledgeWriteBinding, readable []domain.KnowledgeScopeRef, subject string) (string, error) {
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)
	if owner == "" {
		return "", fmt.Errorf("%w: inspect requires the trusted actor owner key", domain.ErrKnowledgeScopeBindingMismatch)
	}
	var lines []string
	claims, err := s.store.ListClaimsInScopes(ctx, readable, subject, s.cfg.Limits)
	if err != nil {
		return "", err
	}
	limits := s.cfg.Limits.WithDefaults()
	listLimit := limits.MaxClaimsListing
	if strings.TrimSpace(subject) != "" {
		listLimit = limits.MaxClaimsPerSubject * len(readable)
	}
	claimsTruncated := false
	if len(claims) > listLimit {
		claims = claims[:listLimit]
		claimsTruncated = true
	}
	for _, claim := range claims {
		card := domain.CardFromClaim(claim, "human inspect", s.clock.Now().UTC())
		lines = append(lines, fmt.Sprintf("claim %s: %s", claim.ID, card.Render()))
	}
	if claimsTruncated {
		lines = append(lines, fmt.Sprintf("claim listing truncated at %d items; inspect individual claim_id for the remaining items.", listLimit))
	}
	preferences, err := s.store.ListPreferencesForOwner(ctx, owner, s.cfg.Limits)
	if err != nil {
		return "", err
	}
	for _, preference := range preferences {
		lines = append(lines, fmt.Sprintf("preference %s = %s [%s; revision %d]", preference.Key, renderPreferenceValue(preference.Value), preference.Status, preference.Revision))
	}
	documents, err := s.store.ListDocumentsInScopes(ctx, readable, s.cfg.Limits)
	if err != nil {
		return "", err
	}
	for _, document := range documents {
		lines = append(lines, fmt.Sprintf("document %s [%s:%s; %s; revision %d]", document.Subject, document.ScopeKind, document.ScopeID, document.Status, document.Revision))
	}
	if len(lines) == 0 {
		return "No knowledge visible in this binding.", nil
	}
	return strings.Join(lines, "\n"), nil
}

// renderPreferenceValue formats scalar preference values. Reference values are
// rejected by preference validation and never reach this path.
func renderPreferenceValue(value domain.KnowledgeValue) string {
	switch value.Kind {
	case domain.KnowledgeValueNumber:
		return strconv.FormatFloat(value.Number, 'g', -1, 64)
	case domain.KnowledgeValueBoolean:
		if value.Boolean {
			return "true"
		}
		return "false"
	default:
		return value.Text
	}
}
