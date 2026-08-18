package domain

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// knowledgeFrameCardsPreamble is the canonical block preamble for retrieved
// knowledge. It is distinct from the claim-only [KNOWLEDGE] preamble so the
// two renderers never alias.
const knowledgeFrameCardsPreamble = "[KNOWLEDGE DATA]\n"

// KnowledgePreferenceCard is the complete model-facing preference payload. It
// carries only active owner-bound preferences with their stable identity,
// provenance, and retrieval reason.
type KnowledgePreferenceCard struct {
	ID              string
	OwnerKey        string
	Key             string
	Value           KnowledgeValue
	Status          KnowledgePreferenceStatus
	SourceRef       string
	Revision        int
	RetrievalReason string
}

// ValidKnowledgePreferenceCardID reports whether id matches the stable
// preference:<row-id> identity shape with a positive bounded row identity:
// decimal digits without leading zeros, no zero value, and within int64.
func ValidKnowledgePreferenceCardID(id string) bool {
	const prefix = "preference:"
	if !strings.HasPrefix(id, prefix) || len(id) == len(prefix) {
		return false
	}
	digits := id[len(prefix):]
	if len(digits) > 19 || digits[0] == '0' {
		return false
	}
	rowID, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || rowID < 1 {
		return false
	}
	return true
}

// Validate enforces the complete preference-card contract: stable bounded
// identity, a canonical owner key, non-empty bounded key, a scalar value,
// active status, a valid bounded source reference, a positive revision, and
// a closed retrieval reason.
func (c KnowledgePreferenceCard) Validate() error {
	limits := HardKnowledgeLimits()
	if !ValidKnowledgePreferenceCardID(c.ID) {
		return fmt.Errorf("%w: preference card identity %q is not a positive bounded preference:<row-id>", ErrKnowledgeInvalidValue, c.ID)
	}
	if !ValidSlackOwnerKey(c.OwnerKey) {
		return fmt.Errorf("%w: preference card owner %q is not a canonical owner key", ErrKnowledgeInvalidValue, c.OwnerKey)
	}
	parts := strings.Split(c.OwnerKey, ":")
	if !PlausibleTeamID(parts[1]) || !PlausibleUserID(parts[3]) {
		return fmt.Errorf("%w: preference card owner %q carries implausible identities", ErrKnowledgeInvalidValue, c.OwnerKey)
	}
	if strings.TrimSpace(c.Key) == "" {
		return fmt.Errorf("%w: preference card key must not be empty", ErrKnowledgeInvalidValue)
	}
	if utf8.RuneCountInString(c.Key) > limits.MaxPreferenceKeyRunes {
		return fmt.Errorf("%w: preference card key exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxPreferenceKeyRunes)
	}
	if err := ValidateMemoryReferenceText(c.Key); err != nil {
		return fmt.Errorf("%w: preference card key: %v", ErrKnowledgeInvalidValue, err)
	}
	if c.Value.Kind == KnowledgeValueReference {
		return fmt.Errorf("%w: preference cards accept scalar values only", ErrKnowledgeInvalidValue)
	}
	if err := c.Value.validate(KnowledgePredicateIs, limits); err != nil {
		return fmt.Errorf("%w: preference card value: %v", ErrKnowledgeInvalidValue, err)
	}
	if c.Status != KnowledgePreferenceActive {
		return fmt.Errorf("%w: preference card status must be active", ErrKnowledgeInvalidValue)
	}
	if err := validateBoundedSourceRef(c.SourceRef, limits); err != nil {
		return fmt.Errorf("%w: preference card source: %v", ErrKnowledgeInvalidSource, err)
	}
	if c.Revision < 1 {
		return fmt.Errorf("%w: preference card revision must be positive", ErrKnowledgeInvalidValue)
	}
	if err := ValidateKnowledgeRetrievalReason(c.RetrievalReason); err != nil {
		return err
	}
	return nil
}

// Render produces the canonical complete preference-card text.
func (c KnowledgePreferenceCard) Render() string {
	return fmt.Sprintf("preference:%s %s %s = %s [status=active; revision=%d; source=%s; reason=%s]",
		strings.TrimPrefix(c.ID, "preference:"), c.OwnerKey, c.Key, c.Value.render(), c.Revision, c.SourceRef, c.RetrievalReason)
}

// KnowledgeDocumentCard is the complete model-facing document payload. It
// carries the verified, redacted, complete content bytes and never a
// truncated window into them.
type KnowledgeDocumentCard struct {
	ID              string
	Subject         string
	ScopeKind       KnowledgeScopeKind
	ScopeID         string
	Provenance      KnowledgeProvenance
	Status          KnowledgeDocumentStatus
	SourceID        string
	SourceRev       int
	Content         string
	RetrievalReason string
}

// Validate enforces the complete document-card contract: stable identity,
// subject and scope, provenance-consistent bounded source identity, active
// status, non-empty valid complete UTF-8 content within the hard byte
// bound, and a closed retrieval reason.
func (c KnowledgeDocumentCard) Validate() error {
	limits := HardKnowledgeLimits()
	if !ValidKnowledgeDocumentID(KnowledgeDocumentID(c.ID)) {
		return fmt.Errorf("%w: document card identity %q is not a valid document id", ErrKnowledgeInvalidValue, c.ID)
	}
	if strings.TrimSpace(c.Subject) == "" {
		return fmt.Errorf("%w: document card subject must not be empty", ErrKnowledgeInvalidValue)
	}
	if utf8.RuneCountInString(c.Subject) > limits.MaxSubjectRunes {
		return fmt.Errorf("%w: document card subject exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxSubjectRunes)
	}
	if err := ValidateKnowledgeScope(c.ScopeKind, c.ScopeID, limits); err != nil {
		return err
	}
	if !validKnowledgeProvenance(c.Provenance) {
		return fmt.Errorf("%w: document card has unknown provenance %q", ErrKnowledgeInvalidValue, c.Provenance)
	}
	switch c.Provenance {
	case KnowledgeProvenanceLegacyCurated:
		if strings.TrimSpace(c.SourceID) == "" || c.SourceRev < 1 {
			return fmt.Errorf("%w: legacy document cards require source identity and revision", ErrKnowledgeInvalidValue)
		}
		if utf8.RuneCountInString(c.SourceID) > limits.MaxSourceRefRunes {
			return fmt.Errorf("%w: legacy document card source identity exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxSourceRefRunes)
		}
		if err := ValidateMemoryReferenceText(c.SourceID); err != nil {
			return fmt.Errorf("%w: legacy document card source identity: %v", ErrKnowledgeInvalidValue, err)
		}
	case KnowledgeProvenanceCurated:
		if c.SourceID != "" || c.SourceRev != 0 {
			return fmt.Errorf("%w: curated document cards must not carry legacy source identity", ErrKnowledgeInvalidValue)
		}
	}
	if c.Status != KnowledgeDocumentActive {
		return fmt.Errorf("%w: document card status must be active", ErrKnowledgeInvalidValue)
	}
	if strings.TrimSpace(c.Content) == "" {
		return fmt.Errorf("%w: document card content must not be empty", ErrKnowledgeInvalidValue)
	}
	if !utf8.ValidString(c.Content) {
		return fmt.Errorf("%w: document card content must be valid UTF-8", ErrKnowledgeInvalidValue)
	}
	if len(c.Content) > HardMaxKnowledgeRetrievalMaxDocumentBytes {
		return fmt.Errorf("%w: document card content exceeds the hard byte bound of %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgeRetrievalMaxDocumentBytes)
	}
	if err := ValidateKnowledgeRetrievalReason(c.RetrievalReason); err != nil {
		return err
	}
	return nil
}

// ExceedsDocumentBytes reports whether the complete content exceeds the
// retrieval byte limit. Oversized documents are omitted whole, never
// truncated into a card.
func (c KnowledgeDocumentCard) ExceedsDocumentBytes(limitBytes int) bool {
	return limitBytes > 0 && len(c.Content) > limitBytes
}

// validateBoundedSourceRef enforces the authoritative source-reference
// bounds: non-empty, memory-reference safe, and within the hard rune limit.
func validateBoundedSourceRef(sourceRef string, limits KnowledgeLimits) error {
	if strings.TrimSpace(sourceRef) == "" {
		return errors.New("source reference must not be empty")
	}
	if utf8.RuneCountInString(sourceRef) > limits.MaxSourceRefRunes {
		return fmt.Errorf("source reference exceeds maximum of %d characters", limits.MaxSourceRefRunes)
	}
	if err := ValidateMemoryReferenceText(sourceRef); err != nil {
		return fmt.Errorf("source reference: %v", err)
	}
	return nil
}

// Render produces the canonical complete document-card text, including the
// full content. It never truncates.
func (c KnowledgeDocumentCard) Render() string {
	var b strings.Builder
	b.WriteString("document:")
	b.WriteString(c.ID)
	b.WriteString(" ")
	b.WriteString(c.Subject)
	b.WriteString(" [")
	b.WriteString(string(c.ScopeKind))
	if c.ScopeID != "" {
		b.WriteString(":")
		b.WriteString(c.ScopeID)
	}
	b.WriteString("; provenance=")
	b.WriteString(string(c.Provenance))
	b.WriteString("; status=active")
	if c.Provenance == KnowledgeProvenanceLegacyCurated {
		b.WriteString("; source=")
		b.WriteString(c.SourceID)
		b.WriteString("; revision=")
		b.WriteString(strconv.Itoa(c.SourceRev))
	}
	b.WriteString("; reason=")
	b.WriteString(c.RetrievalReason)
	b.WriteString("]\n")
	b.WriteString(c.Content)
	return b.String()
}

// KnowledgeFrameCard is the tagged union of one complete retrieved knowledge
// payload. Exactly one of Claim, Preference, or Document must be non-nil and
// must match Kind; zero or multiple payloads are rejected. The existing
// KnowledgeCard remains the frozen claim shape and is not expanded.
type KnowledgeFrameCard struct {
	Kind       KnowledgeRetrievalItemKind
	Claim      *KnowledgeCard
	Preference *KnowledgePreferenceCard
	Document   *KnowledgeDocumentCard
}

// Identity derives the stable content-free epoch identity for the card:
// kind plus the payload identity.
func (c KnowledgeFrameCard) Identity() string {
	switch c.Kind {
	case KnowledgeRetrievalClaim:
		if c.Claim != nil {
			return "claim:" + string(c.Claim.ClaimID)
		}
	case KnowledgeRetrievalPreference:
		if c.Preference != nil {
			return "preference:" + strings.TrimPrefix(c.Preference.ID, "preference:")
		}
	case KnowledgeRetrievalDocument:
		if c.Document != nil {
			return "document:" + c.Document.ID
		}
	}
	return ""
}

// Validate enforces the tagged-union contract and the full payload
// validation for the single present payload. The claim payload keeps the
// frozen KnowledgeCard shape and is validated card-specifically: effective
// statuses such as expired are legal rendered states even though expired is
// never a durable claim status.
func (c KnowledgeFrameCard) Validate() error {
	if !validKnowledgeRetrievalItemKind(c.Kind) {
		return fmt.Errorf("%w: unknown frame card kind %q", ErrKnowledgeInvalidValue, c.Kind)
	}
	payloads := 0
	if c.Claim != nil {
		payloads++
	}
	if c.Preference != nil {
		payloads++
	}
	if c.Document != nil {
		payloads++
	}
	if payloads != 1 {
		return fmt.Errorf("%w: frame card must carry exactly one payload, got %d", ErrKnowledgeInvalidValue, payloads)
	}
	switch c.Kind {
	case KnowledgeRetrievalClaim:
		if c.Claim == nil {
			return fmt.Errorf("%w: claim frame card carries no claim payload", ErrKnowledgeInvalidValue)
		}
		return c.Claim.validateFramePayload()
	case KnowledgeRetrievalPreference:
		if c.Preference == nil {
			return fmt.Errorf("%w: preference frame card carries no preference payload", ErrKnowledgeInvalidValue)
		}
		return c.Preference.Validate()
	case KnowledgeRetrievalDocument:
		if c.Document == nil {
			return fmt.Errorf("%w: document frame card carries no document payload", ErrKnowledgeInvalidValue)
		}
		return c.Document.Validate()
	}
	return nil
}

// validateFramePayload validates a frozen KnowledgeCard as rendered frame
// data without mutating the frozen claim contract. It enforces the
// retrieval eligibility subset: superseded and archived claims are terminal
// and never reach the model; asserted, verified, and disputed are eligible,
// and expired is a legal computed effective state.
func (c KnowledgeCard) validateFramePayload() error {
	limits := HardKnowledgeLimits()
	if !ValidKnowledgeClaimID(c.ClaimID) {
		return fmt.Errorf("%w: claim card identity %q is not a valid claim id", ErrKnowledgeInvalidValue, c.ClaimID)
	}
	if strings.TrimSpace(c.Subject) == "" {
		return fmt.Errorf("%w: claim card subject must not be empty", ErrKnowledgeInvalidValue)
	}
	if utf8.RuneCountInString(c.Subject) > limits.MaxSubjectRunes {
		return fmt.Errorf("%w: claim card subject exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxSubjectRunes)
	}
	if err := ValidateMemoryReferenceText(c.Subject); err != nil {
		return fmt.Errorf("%w: claim card subject: %v", ErrKnowledgeInvalidValue, err)
	}
	if !validKnowledgePredicate(c.Predicate) {
		return fmt.Errorf("%w: claim card predicate %q is invalid", ErrKnowledgeInvalidPredicate, c.Predicate)
	}
	if err := c.Value.validate(c.Predicate, limits); err != nil {
		return fmt.Errorf("%w: claim card value: %v", ErrKnowledgeInvalidValue, err)
	}
	if err := ValidateKnowledgeScope(c.ScopeKind, c.ScopeID, limits); err != nil {
		return err
	}
	if err := ValidateMemoryReferenceText(c.ScopeID); err != nil {
		return fmt.Errorf("%w: claim card scope identity: %v", ErrKnowledgeInvalidScope, err)
	}
	if !c.ValidFrom.IsZero() && !c.ValidUntil.IsZero() && c.ValidUntil.Before(c.ValidFrom) {
		return fmt.Errorf("%w: claim card valid_until precedes valid_from", ErrKnowledgeInvalidValue)
	}
	if !validKnowledgeSourceClass(c.SourceClass) {
		return fmt.Errorf("%w: claim card source %q is invalid", ErrKnowledgeInvalidSource, c.SourceClass)
	}
	if err := validateBoundedSourceRef(c.SourceRef, limits); err != nil {
		return fmt.Errorf("%w: claim card source: %v", ErrKnowledgeInvalidSource, err)
	}
	if !validKnowledgeClaimStatus(c.Status) {
		return fmt.Errorf("%w: claim card status %q is invalid", ErrKnowledgeStatusTransition, c.Status)
	}
	if c.Status.Terminal() {
		return fmt.Errorf("%w: terminal claim status %q is not eligible for retrieval", ErrKnowledgeInvalidValue, c.Status)
	}
	if c.SupersedesID != "" && !ValidKnowledgeClaimID(c.SupersedesID) {
		return fmt.Errorf("%w: claim card supersedes identity %q is invalid", ErrKnowledgeInvalidValue, c.SupersedesID)
	}
	return ValidateKnowledgeRetrievalReason(c.RetrievalReason)
}

// ValidateKnowledgeRetrievalReason enforces the closed retrieval-reason set.
func ValidateKnowledgeRetrievalReason(reason string) error {
	if !validKnowledgeRetrievalReason(KnowledgeRetrievalReason(reason)) {
		return fmt.Errorf("%w: unknown retrieval reason %q", ErrKnowledgeInvalidValue, reason)
	}
	return nil
}

// Render produces the canonical single frame-card text with its type label.
func (c KnowledgeFrameCard) Render() string {
	var b strings.Builder
	b.WriteString(string(c.Kind))
	b.WriteString("\n")
	switch c.Kind {
	case KnowledgeRetrievalClaim:
		if c.Claim != nil {
			b.WriteString(c.Claim.Render())
		}
	case KnowledgeRetrievalPreference:
		if c.Preference != nil {
			b.WriteString(c.Preference.Render())
		}
	case KnowledgeRetrievalDocument:
		if c.Document != nil {
			b.WriteString(c.Document.Render())
		}
	}
	return b.String()
}

// RenderKnowledgeFrameCards produces the canonical combined frame-card block
// with the explicit [KNOWLEDGE DATA] preamble, type-labeled cards, and
// deterministic separators. The default cumulative cost is computed over
// this exact rendering.
func RenderKnowledgeFrameCards(cards []KnowledgeFrameCard) string {
	if len(cards) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(knowledgeFrameCardsPreamble)
	for i, card := range cards {
		if i > 0 {
			b.WriteString("\n---\n")
		}
		b.WriteString(card.Render())
	}
	b.WriteString("\n")
	return b.String()
}

// KnowledgeFrameCardCostFunc computes the cumulative cost of a candidate
// frame-card selection. It may return an error when exact costing is
// unavailable; selection then fails closed rather than admitting an
// unbudgeted frame.
type KnowledgeFrameCardCostFunc func(selected []KnowledgeFrameCard) (int, error)

// FitKnowledgeFrameCards selects whole frame cards under a cumulative budget
// measured over the canonical combined rendering (shared preamble, labels,
// and separators included). No card is cut in the middle; cards that do not
// fit are omitted entirely. An unavailable exact counter fails the selection
// closed. It mirrors the frozen claim-card fitting contract.
func FitKnowledgeFrameCards(cards []KnowledgeFrameCard, budget int, cost KnowledgeFrameCardCostFunc) ([]KnowledgeFrameCard, error) {
	if budget <= 0 {
		return nil, nil
	}
	if cost == nil {
		cost = func(selected []KnowledgeFrameCard) (int, error) {
			return utf8.RuneCountInString(RenderKnowledgeFrameCards(selected)), nil
		}
	}
	selected := make([]KnowledgeFrameCard, 0, len(cards))
	for _, card := range cards {
		candidate := append(selected, card)
		total, err := cost(candidate)
		if err != nil {
			return nil, err
		}
		if total > budget {
			continue
		}
		selected = candidate
	}
	return selected, nil
}
