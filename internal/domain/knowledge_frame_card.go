package domain

import (
	"errors"
	"fmt"
	"sort"
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
	if err := ValidateKnowledgeText(c.Key); err != nil {
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
	if c.Provenance == KnowledgeProvenanceCurated {
		if c.SourceID != "" || c.SourceRev != 0 {
			return fmt.Errorf("%w: curated document cards must not carry source identity", ErrKnowledgeInvalidValue)
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
	if err := ValidateKnowledgeText(sourceRef); err != nil {
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
	if err := ValidateKnowledgeText(c.Subject); err != nil {
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
	if err := ValidateKnowledgeText(c.ScopeID); err != nil {
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

// localKnowledgeFrameCardCost estimates, without ever calling the
// authoritative cost function, the incremental rune cost of adding one more
// card to a selection that already has selectedSoFar cards in it. It follows
// the exact shape of RenderKnowledgeFrameCards: the preamble is paid once by
// the first card, every card after the first also pays one separator, and
// each card pays the rune cost of its own rendered text. This is the "local
// and deterministic" per-card measure that plans a selection without a
// frame-counter call per candidate.
func localKnowledgeFrameCardCost(selectedSoFar int, card KnowledgeFrameCard) int {
	n := utf8.RuneCountInString(card.Render())
	if selectedSoFar == 0 {
		// +1 for the trailing newline RenderKnowledgeFrameCards always adds.
		return utf8.RuneCountInString(knowledgeFrameCardsPreamble) + n + 1
	}
	return utf8.RuneCountInString("\n---\n") + n
}

// planKnowledgeFrameCardSelection greedily selects whole cards under budget
// using only the local cost model above. A card that does not fit is
// skipped, not selected, and the loop keeps testing the cards that follow:
// this is the same skip-and-continue contract FitKnowledgeFrameCards has
// always had, so a large card can be skipped and a smaller later card can
// still be admitted. It never calls the authoritative cost function.
func planKnowledgeFrameCardSelection(cards []KnowledgeFrameCard, budget int) []KnowledgeFrameCard {
	selected := make([]KnowledgeFrameCard, 0, len(cards))
	total := 0
	for _, card := range cards {
		inc := localKnowledgeFrameCardCost(len(selected), card)
		if total+inc > budget {
			continue
		}
		selected = append(selected, card)
		total += inc
	}
	return selected
}

// localKnowledgeFrameCardCardSelectionTotal sums the local model over an
// already-selected slice, in the order given: the same shape
// planKnowledgeFrameCardSelection accumulates while planning.
func localKnowledgeFrameCardSelectionTotal(selected []KnowledgeFrameCard) int {
	total := 0
	for i, card := range selected {
		total += localKnowledgeFrameCardCost(i, card)
	}
	return total
}

// knowledgeFrameCardMaxAuthoritativeCalls bounds the authoritative calls
// FitKnowledgeFrameCards itself may spend to at most 10; this is the
// constant Gate C (TestFitKnowledgeFrameCardsGateCBoundsAuthoritativeCalls)
// measures directly against the public FitKnowledgeFrameCards entry point.
// The benchmark-measured path (BenchmarkCompileKnowledgeSelection, the
// caller's one base count before this function runs plus this function's
// own calls plus the caller's one final count after) tops out at 12 for
// that specific path. There is no global ceiling of 12 for every shape a
// compilation can take: a general compilation can also count summary,
// workstream, and total-pressure reduction steps, none of which this
// constant bounds. DEC-08-5 amended the benchmark path's limit from 4 to 12
// on 2026-08-20 (checkpoint 4 repair 2, FIND-116): the reviewer's decision
// is that a constant that does not scale with card count is what Gate C
// protects, and 4 was an estimate written before any implementation
// existed, not a measurement. The 10 here is FitKnowledgeFrameCards's own
// internal limit, leaving two of that amended 12 for the benchmark's base
// and final counts. This headroom is spent on a per-card refinement pass
// that closes most of the remaining gap to the unbounded oracle under a
// non-proportional (word-like) counter.
const knowledgeFrameCardMaxAuthoritativeCalls = 10

// FitKnowledgeFrameCards selects whole frame cards under a cumulative budget
// measured over the canonical combined rendering (shared preamble, labels,
// and separators included). No card is cut in the middle; cards that do not
// fit are omitted entirely. An unavailable exact counter fails the selection
// closed. It mirrors the frozen claim-card fitting contract.
//
// Selection never calls the authoritative cost function once per candidate.
// It spends at most knowledgeFrameCardMaxAuthoritativeCalls (10) calls,
// constant regardless of how many candidate cards are offered, in three
// phases:
//
//  1. Plan and verify: assume the authoritative counter scales with rune
//     count (localKnowledgeFrameCardCost) and skip-and-continue select under
//     the raw budget, with zero authoritative calls; if nothing fits at all
//     under that raw assumption, substitute every candidate card instead of
//     a single one, so the very first authoritative call already samples a
//     representative mix rather than whichever card has the least preamble
//     overhead. One call verifies this plan.
//  2. Two correction rounds: each round learns a real-to-local scale from
//     the previous round's own (local, real) pair and replans from the full
//     candidate list at the budget that scale implies, then verifies once.
//     The second round is calibrated on the first round's result, which is
//     shaped much more like the eventual selection than a single card or
//     the whole pool, so it is the least biased of the three data points.
//  3. Per-card refinement: starting from the best selection verified safe
//     so far, try adding the cheapest still-missing card (by the local
//     model), verify, keep it if it fits, and repeat; stop after two
//     additions in a row fail to fit, or when the call budget runs out.
//     This is what closes the gap the two correction rounds alone cannot:
//     they learn one global scale, but a real counter is not a single
//     scale away from rune count, so the corrected plan is usually off by a
//     handful of cards rather than by order of magnitude, and a handful of
//     one-card, fully-verified steps is exactly what fixes that.
//
// Whichever selection is verified safe and best at the end is returned. If
// nothing is ever verified safe, the result is empty. Gate C's amendment
// does not touch this: selection never admits over budget under the
// authoritative count, regardless of how the local model under- or
// over-estimates the real counter.
func FitKnowledgeFrameCards(cards []KnowledgeFrameCard, budget int, cost KnowledgeFrameCardCostFunc) ([]KnowledgeFrameCard, error) {
	if budget <= 0 || len(cards) == 0 {
		return nil, nil
	}
	if cost == nil {
		cost = func(selected []KnowledgeFrameCard) (int, error) {
			return utf8.RuneCountInString(RenderKnowledgeFrameCards(selected)), nil
		}
	}
	callsUsed := 0
	countedCost := func(selected []KnowledgeFrameCard) (int, error) {
		callsUsed++
		return cost(selected)
	}

	plan1 := planKnowledgeFrameCardSelection(cards, budget)
	verifySet := plan1
	substituted := len(plan1) == 0
	if substituted {
		// A raw scale=1 assumption fit nothing at all: calibrate from every
		// candidate card, not the single cheapest one. A lone cheap card is
		// dominated by its own preamble overhead relative to its content,
		// which biases the learned scale exactly where it is used most: a
		// real multi-card selection has that overhead only once, not once
		// per card.
		verifySet = cards
	}
	total1, err := countedCost(verifySet)
	if err != nil {
		return nil, err
	}
	fits1 := total1 <= budget
	safe := verifySet
	safeTotal := total1
	safeFits := fits1
	if substituted && !fits1 {
		// Every candidate card together does not fit; there is nothing
		// verified-safe yet to fall back to besides empty.
		safe, safeTotal, safeFits = nil, 0, true
	}

	// Nothing was left out of the raw plan, so a correction round could not
	// gain anything by assuming a different scale, but refinement can still
	// close a remainder the raw plan's own greedy order missed.
	skipToRefine := (!substituted && fits1 && len(plan1) == len(cards)) || (substituted && fits1)
	if !skipToRefine {
		plan2, total2, err := knowledgeFrameCardCorrection(cards, verifySet, budget, total1, countedCost)
		if err != nil {
			return nil, err
		}
		if plan2 != nil {
			if total2 <= budget {
				safe, safeTotal, safeFits = plan2, total2, true
			}
			if total2 > budget || (len(plan2) != len(cards) && total2 != budget) {
				// One more round, calibrated on plan2 instead of the first
				// probe: plan2's shape (card count, mix of overhead-heavy
				// and content-heavy cards) is much closer to the eventual
				// selection than a single card or the whole pool was, so
				// this scale is the least biased of the three. The same
				// correction step handles both directions: it shrinks when
				// plan2 overshot and grows when plan2 still had headroom.
				plan3, total3, err := knowledgeFrameCardCorrection(cards, plan2, budget, total2, countedCost)
				if err != nil {
					return nil, err
				}
				if plan3 != nil && total3 <= budget {
					safe, safeTotal, safeFits = plan3, total3, true
				}
			}
		}
	}

	if !safeFits {
		return nil, nil
	}
	remaining := knowledgeFrameCardMaxAuthoritativeCalls - callsUsed
	if remaining <= 0 || safe == nil {
		return safe, nil
	}
	refined, _, err := refineKnowledgeFrameCardSelection(cards, safe, safeTotal, budget, remaining, countedCost)
	if err != nil {
		return nil, err
	}
	return refined, nil
}

// knowledgeFrameCardCorrection learns a real-to-local scale from the
// (verifySet, total1) pair already verified by the caller and replans from
// the full candidate list at the budget that scale implies. It calls cost
// exactly once, to verify the replanned selection; it returns a nil plan
// (with a zero total) when nothing can be safely inferred, and the caller
// falls back to whatever it already had verified.
func knowledgeFrameCardCorrection(cards, verifySet []KnowledgeFrameCard, budget, total1 int, cost KnowledgeFrameCardCostFunc) ([]KnowledgeFrameCard, int, error) {
	localTotal := localKnowledgeFrameCardSelectionTotal(verifySet)
	if localTotal <= 0 || total1 <= 0 {
		return nil, 0, nil
	}
	correctedBudget := budget * localTotal / total1
	if total1 > budget && correctedBudget >= localTotal {
		// An overshoot must shed at least one card: integer rounding must
		// not leave the corrected budget unchanged.
		correctedBudget = localTotal - 1
	}
	if correctedBudget <= 0 {
		return nil, 0, nil
	}
	plan2 := planKnowledgeFrameCardSelection(cards, correctedBudget)
	if len(plan2) == 0 {
		return nil, 0, nil
	}
	total2, err := cost(plan2)
	if err != nil {
		return nil, 0, err
	}
	return plan2, total2, nil
}

// knowledgeFrameCardKey identifies a card by its payload pointer, not by
// its content-free Identity(): refinement below needs to tell candidate
// cards apart even when Identity() is empty or repeated. FitKnowledgeFrameCards
// is a public function that does not validate its input itself, so it
// cannot assume every caller has already rejected cards with an empty or
// colliding Identity() before calling it.
func knowledgeFrameCardKey(c KnowledgeFrameCard) any {
	switch {
	case c.Claim != nil:
		return c.Claim
	case c.Preference != nil:
		return c.Preference
	case c.Document != nil:
		return c.Document
	default:
		return c
	}
}

// cardsInOriginalOrder rebuilds a candidate selection in the exact relative
// order of cards, keeping only the members named by included. Every attempt
// refinement verifies is built this way, so a selection this package returns
// never reorders the cards it received (domain.CompileRequest.Knowledge
// documents them as arriving already ordered): it may omit cards, but it
// never places one out of its original relative position.
func cardsInOriginalOrder(cards []KnowledgeFrameCard, included map[any]bool) []KnowledgeFrameCard {
	ordered := make([]KnowledgeFrameCard, 0, len(included))
	for _, c := range cards {
		if included[knowledgeFrameCardKey(c)] {
			ordered = append(ordered, c)
		}
	}
	return ordered
}

// refineKnowledgeFrameCardSelection improves an already verified-safe
// selection by trying to add the cheapest still-missing candidate cards,
// one at a time, by the local rune-based model. Each attempt is verified
// for real before being kept: this never returns a selection it has not
// itself confirmed fits. It stops after two additions in a row fail to fit
// (diminishing returns), or after maxCalls authoritative calls, whichever
// comes first. Every attempt and the final result are assembled in the
// original relative order of cards, never appended to the end of best: an
// accepted card takes the position it had in cards, not the position it was
// tried in.
func refineKnowledgeFrameCardSelection(cards, best []KnowledgeFrameCard, bestTotal, budget, maxCalls int, cost KnowledgeFrameCardCostFunc) ([]KnowledgeFrameCard, int, error) {
	included := make(map[any]bool, len(best))
	for _, c := range best {
		included[knowledgeFrameCardKey(c)] = true
	}
	missing := make([]KnowledgeFrameCard, 0, len(cards))
	for _, c := range cards {
		if !included[knowledgeFrameCardKey(c)] {
			missing = append(missing, c)
		}
	}
	sort.SliceStable(missing, func(i, j int) bool {
		return localKnowledgeFrameCardCost(0, missing[i]) < localKnowledgeFrameCardCost(0, missing[j])
	})

	callsUsed := 0
	consecutiveFailures := 0
	for _, candidate := range missing {
		if callsUsed >= maxCalls || consecutiveFailures >= 2 {
			break
		}
		candidateKey := knowledgeFrameCardKey(candidate)
		included[candidateKey] = true
		attempt := cardsInOriginalOrder(cards, included)
		total, err := cost(attempt)
		callsUsed++
		if err != nil {
			return best, bestTotal, err
		}
		if total <= budget {
			best, bestTotal = attempt, total
			consecutiveFailures = 0
			continue
		}
		included[candidateKey] = false
		consecutiveFailures++
	}
	return best, bestTotal, nil
}
