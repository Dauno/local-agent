package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultMaxKnowledgeClaimsPerSubject   = 32
	HardMaxKnowledgeClaimsPerSubject      = 256
	DefaultMaxKnowledgeSubjectRunes       = 200
	HardMaxKnowledgeSubjectRunes          = 512
	DefaultMaxKnowledgeValueRunes         = 2000
	HardMaxKnowledgeValueRunes            = 8000
	DefaultMaxKnowledgeReferenceRunes     = 128
	HardMaxKnowledgeReferenceRunes        = 256
	DefaultMaxKnowledgeScopeIDRunes       = 256
	HardMaxKnowledgeScopeIDRunes          = 512
	DefaultMaxKnowledgeSourceRefRunes     = 256
	HardMaxKnowledgeSourceRefRunes        = 1024
	DefaultMaxKnowledgePreferences        = 64
	HardMaxKnowledgePreferences           = 256
	DefaultMaxKnowledgePreferenceKeyRunes = 128
	HardMaxKnowledgePreferenceKeyRunes    = 256
	DefaultMaxKnowledgeDocuments          = 256
	HardMaxKnowledgeDocuments             = 1024
	DefaultMaxKnowledgeCardBudget         = 1024
	HardMaxKnowledgeCardBudget            = 8192
	DefaultMaxKnowledgeClaimsListing      = 256
	HardMaxKnowledgeClaimsListing         = 2048
)

var (
	ErrKnowledgeInvalidScope           = errors.New("knowledge scope is invalid")
	ErrKnowledgeScopeIdentityRequired  = errors.New("knowledge scope identity is required")
	ErrKnowledgeScopeNotWritable       = errors.New("knowledge scope is not writable")
	ErrKnowledgeScopeBindingMismatch   = errors.New("knowledge scope does not match the trusted binding")
	ErrKnowledgeReadNotAllowed         = errors.New("knowledge scope is not readable by the trusted binding")
	ErrKnowledgeInvalidPredicate       = errors.New("knowledge predicate is invalid")
	ErrKnowledgeInvalidValue           = errors.New("knowledge value is invalid")
	ErrKnowledgeInvalidSource          = errors.New("knowledge source is invalid")
	ErrKnowledgeStatusTransition       = errors.New("knowledge status transition is invalid")
	ErrKnowledgeSourceCannotVerify     = errors.New("knowledge source cannot verify")
	ErrKnowledgeSourceCannotDispute    = errors.New("knowledge source cannot dispute")
	ErrKnowledgeSourceUnauthorized     = errors.New("knowledge source is not authorized for this transition")
	ErrKnowledgeSubjectMismatch        = errors.New("knowledge correction subject mismatch")
	ErrKnowledgeScopeMismatch          = errors.New("knowledge correction scope mismatch")
	ErrKnowledgeSupersedesMissing      = errors.New("knowledge correction must reference the prior claim")
	ErrKnowledgeTombstoneBlocked       = errors.New("knowledge write is blocked by tombstone")
	ErrKnowledgeLimitExceeded          = errors.New("knowledge limit exceeded")
	ErrKnowledgeInvalidDocumentDigest  = errors.New("knowledge document digest is invalid")
	ErrKnowledgeInvalidTombstoneDigest = errors.New("knowledge tombstone digest is invalid")
	ErrKnowledgeInvalidCommandDigest   = errors.New("knowledge command digest is invalid")
	ErrKnowledgeInvalidEvidence        = errors.New("knowledge evidence reference is invalid")
)

type KnowledgeScopeKind string

const (
	KnowledgeScopeGlobal       KnowledgeScopeKind = "global"
	KnowledgeScopeTeam         KnowledgeScopeKind = "team"
	KnowledgeScopeUser         KnowledgeScopeKind = "user"
	KnowledgeScopeProject      KnowledgeScopeKind = "project"
	KnowledgeScopeConversation KnowledgeScopeKind = "conversation"
	KnowledgeScopeWorkstream   KnowledgeScopeKind = "workstream"
)

func validKnowledgeScopeKind(kind KnowledgeScopeKind) bool {
	switch kind {
	case KnowledgeScopeGlobal, KnowledgeScopeTeam, KnowledgeScopeUser,
		KnowledgeScopeProject, KnowledgeScopeConversation, KnowledgeScopeWorkstream:
		return true
	default:
		return false
	}
}

func (s KnowledgeScopeKind) RequiresIdentity() bool {
	return s != KnowledgeScopeGlobal
}

type KnowledgeSourceClass string

const (
	KnowledgeSourceHuman       KnowledgeSourceClass = "human"
	KnowledgeSourceDecision    KnowledgeSourceClass = "decision"
	KnowledgeSourceObservation KnowledgeSourceClass = "observation"
	KnowledgeSourceWorker      KnowledgeSourceClass = "worker"
	KnowledgeSourceRoot        KnowledgeSourceClass = "root"
)

func validKnowledgeSourceClass(source KnowledgeSourceClass) bool {
	switch source {
	case KnowledgeSourceHuman, KnowledgeSourceDecision, KnowledgeSourceObservation,
		KnowledgeSourceWorker, KnowledgeSourceRoot:
		return true
	default:
		return false
	}
}

// MaxKnowledgeClaimStatus returns the strongest status a source class may
// assign. Model-derived classes can never produce verified fact. Verified
// host observations are an empty set in V1, so observation is capped at
// asserted until an owning TRD defines a producer.
func (s KnowledgeSourceClass) MaxKnowledgeClaimStatus() KnowledgeClaimStatus {
	switch s {
	case KnowledgeSourceHuman, KnowledgeSourceDecision:
		return KnowledgeClaimVerified
	default:
		return KnowledgeClaimAsserted
	}
}

type KnowledgeClaimStatus string

const (
	KnowledgeClaimAsserted   KnowledgeClaimStatus = "asserted"
	KnowledgeClaimVerified   KnowledgeClaimStatus = "verified"
	KnowledgeClaimDisputed   KnowledgeClaimStatus = "disputed"
	KnowledgeClaimSuperseded KnowledgeClaimStatus = "superseded"
	KnowledgeClaimExpired    KnowledgeClaimStatus = "expired"
	KnowledgeClaimArchived   KnowledgeClaimStatus = "archived"
)

func validKnowledgeClaimStatus(status KnowledgeClaimStatus) bool {
	switch status {
	case KnowledgeClaimAsserted, KnowledgeClaimVerified, KnowledgeClaimDisputed,
		KnowledgeClaimSuperseded, KnowledgeClaimExpired, KnowledgeClaimArchived:
		return true
	default:
		return false
	}
}

// Terminal reports whether a status is a durable endpoint that no transition
// may leave. Expiry is computed from validity and is therefore not terminal
// on its own; an expired claim may be corrected or archived.
func (s KnowledgeClaimStatus) Terminal() bool {
	return s == KnowledgeClaimSuperseded || s == KnowledgeClaimArchived
}

type KnowledgePredicate string

const (
	KnowledgePredicateIs        KnowledgePredicate = "is"
	KnowledgePredicateUses      KnowledgePredicate = "uses"
	KnowledgePredicateRunsOn    KnowledgePredicate = "runs_on"
	KnowledgePredicateLocatedIn KnowledgePredicate = "located_in"
	KnowledgePredicateOwns      KnowledgePredicate = "owns"
	KnowledgePredicateRelatesTo KnowledgePredicate = "relates_to"
)

func validKnowledgePredicate(predicate KnowledgePredicate) bool {
	switch predicate {
	case KnowledgePredicateIs, KnowledgePredicateUses, KnowledgePredicateRunsOn,
		KnowledgePredicateLocatedIn, KnowledgePredicateOwns, KnowledgePredicateRelatesTo:
		return true
	default:
		return false
	}
}

// RequiresReference reports whether the predicate binds an object identity
// instead of a scalar literal.
func (p KnowledgePredicate) RequiresReference() bool {
	return p == KnowledgePredicateOwns || p == KnowledgePredicateRelatesTo
}

type KnowledgeValueKind string

const (
	KnowledgeValueString    KnowledgeValueKind = "string"
	KnowledgeValueNumber    KnowledgeValueKind = "number"
	KnowledgeValueBoolean   KnowledgeValueKind = "boolean"
	KnowledgeValueReference KnowledgeValueKind = "reference"
)

func validKnowledgeValueKind(kind KnowledgeValueKind) bool {
	switch kind {
	case KnowledgeValueString, KnowledgeValueNumber, KnowledgeValueBoolean, KnowledgeValueReference:
		return true
	default:
		return false
	}
}

// KnowledgeValue is a tagged union: exactly the field matching Kind carries
// the value, and populated fields of any other kind are rejected.
type KnowledgeValue struct {
	Kind      KnowledgeValueKind
	Text      string
	Number    float64
	Boolean   bool
	Reference string
}

type KnowledgeClaimID string

type KnowledgeClaim struct {
	ID           KnowledgeClaimID
	Subject      string
	Predicate    KnowledgePredicate
	Value        KnowledgeValue
	ScopeKind    KnowledgeScopeKind
	ScopeID      string
	SourceClass  KnowledgeSourceClass
	SourceRef    string
	AuthorID     string
	Status       KnowledgeClaimStatus
	ValidFrom    time.Time
	ValidUntil   time.Time
	SupersedesID KnowledgeClaimID
	Revision     int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// KnowledgeWriteBinding carries the trusted identity resolved by the host for
// one write. Model-supplied scope selectors never fill these fields.
type KnowledgeWriteBinding struct {
	Team         string
	Actor        string
	Conversation ConversationKey
	Project      string
	WorkstreamID string
}

// EffectiveStatus resolves computed expiry over the durable status.
func (c KnowledgeClaim) EffectiveStatus(now time.Time) KnowledgeClaimStatus {
	if !c.ValidUntil.IsZero() && now.After(c.ValidUntil) {
		switch c.Status {
		case KnowledgeClaimAsserted, KnowledgeClaimVerified, KnowledgeClaimDisputed:
			return KnowledgeClaimExpired
		}
	}
	return c.Status
}

// Validate checks intrinsic validity without configured limits. It validates
// persisted state: structural invariants, legal scope kinds, and enum values.
// Status authority ceilings belong to admission and transitions, not to
// persisted rows, because a claim's status reflects its full history.
func (c KnowledgeClaim) Validate() error {
	return c.ValidateWithLimits(DefaultKnowledgeLimits())
}

func (c KnowledgeClaim) ValidateWithLimits(limits KnowledgeLimits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	limits = limits.withDefaults()
	if err := ValidateKnowledgeScope(c.ScopeKind, c.ScopeID, limits); err != nil {
		return err
	}
	if err := ValidateKnowledgeScopeWritable(c.SourceClass, c.ScopeKind); err != nil {
		return err
	}
	if !validKnowledgeSourceClass(c.SourceClass) {
		return fmt.Errorf("%w: %q", ErrKnowledgeInvalidSource, c.SourceClass)
	}
	if strings.TrimSpace(c.Subject) == "" {
		return fmt.Errorf("%w: subject must not be empty", ErrKnowledgeInvalidValue)
	}
	if utf8.RuneCountInString(c.Subject) > limits.MaxSubjectRunes {
		return fmt.Errorf("%w: subject exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxSubjectRunes)
	}
	if err := ValidateMemoryReferenceText(c.Subject); err != nil {
		return fmt.Errorf("%w: subject: %v", ErrKnowledgeInvalidValue, err)
	}
	if !validKnowledgePredicate(c.Predicate) {
		return fmt.Errorf("%w: %q", ErrKnowledgeInvalidPredicate, c.Predicate)
	}
	if err := c.Value.validate(c.Predicate, limits); err != nil {
		return err
	}
	if err := ValidateMemoryReferenceText(c.ScopeID); err != nil {
		return fmt.Errorf("%w: scope identity: %v", ErrKnowledgeInvalidScope, err)
	}
	if strings.TrimSpace(c.SourceRef) == "" {
		return fmt.Errorf("%w: source reference must not be empty", ErrKnowledgeInvalidSource)
	}
	if err := ValidateMemoryReferenceText(c.SourceRef); err != nil {
		return fmt.Errorf("%w: source reference: %v", ErrKnowledgeInvalidSource, err)
	}
	if utf8.RuneCountInString(c.SourceRef) > limits.MaxSourceRefRunes {
		return fmt.Errorf("%w: source reference exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxSourceRefRunes)
	}
	if !validKnowledgeClaimStatus(c.Status) {
		return fmt.Errorf("%w: %q", ErrKnowledgeStatusTransition, c.Status)
	}
	if c.Status == KnowledgeClaimExpired {
		return fmt.Errorf("%w: expiry is computed from validity and is never written", ErrKnowledgeStatusTransition)
	}
	if !c.ValidFrom.IsZero() && !c.ValidUntil.IsZero() && c.ValidUntil.Before(c.ValidFrom) {
		return fmt.Errorf("%w: valid_until precedes valid_from", ErrKnowledgeInvalidValue)
	}
	return nil
}

// ValidateCandidate admits a new claim before persistence. It enforces the
// trusted scope binding, binds human and decision authors to the trusted
// actor, and restricts creation to the asserted or, for authoritative
// sources only, verified status.
func (c KnowledgeClaim) ValidateCandidate(limits KnowledgeLimits, binding KnowledgeWriteBinding) error {
	if err := c.ValidateWithLimits(limits); err != nil {
		return err
	}
	if err := ValidateKnowledgeWriteBinding(c.SourceClass, c.ScopeKind, c.ScopeID, binding); err != nil {
		return err
	}
	switch c.SourceClass {
	case KnowledgeSourceHuman, KnowledgeSourceDecision:
		if c.AuthorID == "" || c.AuthorID != binding.Actor || !PlausibleUserID(c.AuthorID) {
			return fmt.Errorf("%w: claim author %q must be the trusted actor %q", ErrKnowledgeScopeBindingMismatch, c.AuthorID, binding.Actor)
		}
	case KnowledgeSourceRoot, KnowledgeSourceWorker:
		if c.AuthorID != "" {
			return fmt.Errorf("%w: model-derived source %q must not carry an author identity", ErrKnowledgeInvalidSource, c.SourceClass)
		}
	}
	switch c.Status {
	case KnowledgeClaimAsserted:
	case KnowledgeClaimVerified:
		if c.SourceClass.MaxKnowledgeClaimStatus() != KnowledgeClaimVerified {
			return fmt.Errorf("%w: %q cannot create a verified claim", ErrKnowledgeSourceCannotVerify, c.SourceClass)
		}
	default:
		return fmt.Errorf("%w: status %q is not a creation status", ErrKnowledgeStatusTransition, c.Status)
	}
	return nil
}

// TransitionStatus applies an explicit status change and enforces the
// monotonic authority rules. Supersession is excluded: it requires a
// validated replacement and must go through Correct. Archived is terminal;
// expired is computed and therefore rejected here.
func (c *KnowledgeClaim) TransitionStatus(next KnowledgeClaimStatus, source KnowledgeSourceClass) error {
	if !validKnowledgeSourceClass(source) {
		return fmt.Errorf("%w: %q", ErrKnowledgeInvalidSource, source)
	}
	if !validKnowledgeClaimStatus(next) {
		return fmt.Errorf("%w: %q", ErrKnowledgeStatusTransition, next)
	}
	if next == KnowledgeClaimExpired {
		return fmt.Errorf("%w: expiry is computed from validity and is never written", ErrKnowledgeStatusTransition)
	}
	if next == KnowledgeClaimSuperseded {
		return fmt.Errorf("%w: supersession requires a validated replacement; use Correct", ErrKnowledgeStatusTransition)
	}
	if c.Status.Terminal() {
		return fmt.Errorf("%w: status %q is terminal", ErrKnowledgeStatusTransition, c.Status)
	}
	if next == c.Status {
		return nil
	}
	if !validKnowledgeStatusTransition(c.Status, next) {
		return fmt.Errorf("%w: %q to %q", ErrKnowledgeStatusTransition, c.Status, next)
	}
	if next == KnowledgeClaimVerified && source.MaxKnowledgeClaimStatus() != KnowledgeClaimVerified {
		return fmt.Errorf("%w: %q cannot verify", ErrKnowledgeSourceCannotVerify, source)
	}
	if next == KnowledgeClaimDisputed && source != KnowledgeSourceHuman {
		return fmt.Errorf("%w: only an explicit human dispute is allowed", ErrKnowledgeSourceCannotDispute)
	}
	if next == KnowledgeClaimArchived && source != KnowledgeSourceHuman {
		return fmt.Errorf("%w: only an explicit human archive is allowed", ErrKnowledgeSourceUnauthorized)
	}
	c.Status = next
	return nil
}

func validKnowledgeStatusTransition(from, to KnowledgeClaimStatus) bool {
	switch from {
	case KnowledgeClaimAsserted, KnowledgeClaimVerified:
		return to == KnowledgeClaimVerified || to == KnowledgeClaimDisputed || to == KnowledgeClaimArchived
	case KnowledgeClaimDisputed:
		return to == KnowledgeClaimArchived
	default:
		return false
	}
}

// Correct validates the replacement as a new candidate against the trusted
// binding before marking the prior claim superseded. The replacement source
// must match the claiming source, the replacement is admitted completely or
// the prior claim is not mutated, and supersession requires an authority
// able to verify. Provenance rows of the prior claim are preserved by the
// store.
func (c *KnowledgeClaim) Correct(newClaim KnowledgeClaim, source KnowledgeSourceClass, limits KnowledgeLimits, binding KnowledgeWriteBinding) error {
	if source != newClaim.SourceClass {
		return fmt.Errorf("%w: correction source %q must match replacement provenance %q", ErrKnowledgeInvalidSource, source, newClaim.SourceClass)
	}
	if err := newClaim.ValidateCandidate(limits, binding); err != nil {
		return err
	}
	if err := ValidateKnowledgeSupersession(newClaim, *c); err != nil {
		return err
	}
	if source.MaxKnowledgeClaimStatus() != KnowledgeClaimVerified {
		return fmt.Errorf("%w: %q cannot supersede", ErrKnowledgeSourceCannotVerify, source)
	}
	if c.Status == KnowledgeClaimSuperseded {
		return nil
	}
	if c.Status.Terminal() {
		return fmt.Errorf("%w: status %q is terminal", ErrKnowledgeStatusTransition, c.Status)
	}
	c.Status = KnowledgeClaimSuperseded
	return nil
}

func ValidateKnowledgeSupersession(newClaim, prior KnowledgeClaim) error {
	if newClaim.Subject != prior.Subject {
		return fmt.Errorf("%w: %q versus %q", ErrKnowledgeSubjectMismatch, newClaim.Subject, prior.Subject)
	}
	if newClaim.ScopeKind != prior.ScopeKind || newClaim.ScopeID != prior.ScopeID {
		return fmt.Errorf("%w: correction must target the same scope", ErrKnowledgeScopeMismatch)
	}
	if newClaim.SupersedesID == "" || newClaim.SupersedesID != prior.ID {
		return fmt.Errorf("%w: supersedes_id must reference the prior claim", ErrKnowledgeSupersedesMissing)
	}
	if prior.Status == KnowledgeClaimArchived {
		return fmt.Errorf("%w: archived claim %q cannot be corrected", ErrKnowledgeStatusTransition, prior.ID)
	}
	return nil
}

func (v KnowledgeValue) validate(predicate KnowledgePredicate, limits KnowledgeLimits) error {
	if !validKnowledgeValueKind(v.Kind) {
		return fmt.Errorf("%w: unknown value kind %q", ErrKnowledgeInvalidValue, v.Kind)
	}
	populated := func(kind KnowledgeValueKind) error {
		if v.Kind != kind {
			return fmt.Errorf("%w: value kind %q must not carry %s data", ErrKnowledgeInvalidValue, v.Kind, kind)
		}
		return nil
	}
	if v.Text != "" {
		if err := populated(KnowledgeValueString); err != nil {
			return err
		}
	}
	if v.Number != 0 {
		if err := populated(KnowledgeValueNumber); err != nil {
			return err
		}
	}
	if v.Boolean {
		if err := populated(KnowledgeValueBoolean); err != nil {
			return err
		}
	}
	if v.Reference != "" {
		if err := populated(KnowledgeValueReference); err != nil {
			return err
		}
	}
	if predicate.RequiresReference() {
		if v.Kind != KnowledgeValueReference {
			return fmt.Errorf("%w: predicate %q requires a reference value", ErrKnowledgeInvalidValue, predicate)
		}
		return validateKnowledgeReference(v.Reference, limits)
	}
	if v.Kind == KnowledgeValueReference {
		return fmt.Errorf("%w: predicate %q does not accept a reference value", ErrKnowledgeInvalidValue, predicate)
	}
	switch v.Kind {
	case KnowledgeValueString:
		if strings.TrimSpace(v.Text) == "" {
			return fmt.Errorf("%w: string value must not be empty", ErrKnowledgeInvalidValue)
		}
		if utf8.RuneCountInString(v.Text) > limits.MaxValueRunes {
			return fmt.Errorf("%w: value exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxValueRunes)
		}
		if err := ValidateMemoryReferenceText(v.Text); err != nil {
			return fmt.Errorf("%w: value: %v", ErrKnowledgeInvalidValue, err)
		}
	case KnowledgeValueNumber:
		if math.IsNaN(v.Number) || math.IsInf(v.Number, 0) {
			return fmt.Errorf("%w: number value must be finite", ErrKnowledgeInvalidValue)
		}
	case KnowledgeValueBoolean:
	}
	return nil
}

func validateKnowledgeReference(reference string, limits KnowledgeLimits) error {
	if strings.TrimSpace(reference) == "" {
		return fmt.Errorf("%w: reference value must not be empty", ErrKnowledgeInvalidValue)
	}
	if utf8.RuneCountInString(reference) > limits.MaxReferenceRunes {
		return fmt.Errorf("%w: reference exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxReferenceRunes)
	}
	if err := ValidateMemoryReferenceText(reference); err != nil {
		return fmt.Errorf("%w: reference: %v", ErrKnowledgeInvalidValue, err)
	}
	return nil
}

func ValidateKnowledgeScope(kind KnowledgeScopeKind, id string, limits KnowledgeLimits) error {
	limits = limits.withDefaults()
	if !validKnowledgeScopeKind(kind) {
		return fmt.Errorf("%w: %q", ErrKnowledgeInvalidScope, kind)
	}
	if kind.RequiresIdentity() {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("%w: scope %q requires an identity", ErrKnowledgeScopeIdentityRequired, kind)
		}
		if utf8.RuneCountInString(id) > limits.MaxScopeIDRunes {
			return fmt.Errorf("%w: scope identity exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxScopeIDRunes)
		}
	} else if strings.TrimSpace(id) != "" {
		return fmt.Errorf("%w: scope %q must not carry an identity", ErrKnowledgeInvalidScope, kind)
	}
	return nil
}

// ValidateKnowledgeSourceRef validates a mutation source reference attached
// directly to a command (status transition, preference archive, subject
// forget) rather than carried inside a persisted payload. It applies the
// same identity rules as payload source references.
func ValidateKnowledgeSourceRef(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("%w: source reference must not be empty", ErrKnowledgeInvalidSource)
	}
	if err := ValidateMemoryReferenceText(ref); err != nil {
		return fmt.Errorf("%w: source reference: %v", ErrKnowledgeInvalidSource, err)
	}
	if utf8.RuneCountInString(ref) > DefaultMaxKnowledgeSourceRefRunes {
		return fmt.Errorf("%w: source reference exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, DefaultMaxKnowledgeSourceRefRunes)
	}
	return nil
}

// ValidateKnowledgeScopeWritable enforces the V1 write policy: user and
// project scopes accept explicit human sources only, workstream scope
// accepts approved decisions only, and global, team, and conversation
// scopes accept no writes. There is no automatic curator source class in
// V1.
func ValidateKnowledgeScopeWritable(source KnowledgeSourceClass, kind KnowledgeScopeKind) error {
	if !validKnowledgeSourceClass(source) {
		return fmt.Errorf("%w: %q", ErrKnowledgeInvalidSource, source)
	}
	switch kind {
	case KnowledgeScopeUser, KnowledgeScopeProject:
		switch source {
		case KnowledgeSourceHuman:
			return nil
		default:
			return fmt.Errorf("%w: source %q cannot write scope %q", ErrKnowledgeScopeNotWritable, source, kind)
		}
	case KnowledgeScopeWorkstream:
		if source != KnowledgeSourceDecision {
			return fmt.Errorf("%w: scope %q requires an approved decision source", ErrKnowledgeScopeNotWritable, kind)
		}
		return nil
	default:
		return fmt.Errorf("%w: scope %q accepts no writes", ErrKnowledgeScopeNotWritable, kind)
	}
}

// ValidateKnowledgeWriteBinding enforces that the claim's scope identity
// matches the trusted host-resolved binding. Scope selectors are never taken
// from model output; they must equal the authoritative identity.
func ValidateKnowledgeWriteBinding(source KnowledgeSourceClass, kind KnowledgeScopeKind, scopeID string, binding KnowledgeWriteBinding) error {
	if err := ValidateKnowledgeScopeWritable(source, kind); err != nil {
		return err
	}
	switch kind {
	case KnowledgeScopeUser:
		owner := SlackOwnerKey(binding.Conversation, binding.Actor)
		if owner == "" || scopeID != owner {
			return fmt.Errorf("%w: user scope identity %q must be the trusted actor owner key", ErrKnowledgeScopeBindingMismatch, scopeID)
		}
	case KnowledgeScopeProject:
		if binding.Project == "" || scopeID != binding.Project {
			return fmt.Errorf("%w: project scope identity %q must be the trusted project selector", ErrKnowledgeScopeBindingMismatch, scopeID)
		}
	case KnowledgeScopeWorkstream:
		if binding.WorkstreamID == "" || scopeID != binding.WorkstreamID {
			return fmt.Errorf("%w: workstream scope identity %q must be the bound workstream", ErrKnowledgeScopeBindingMismatch, scopeID)
		}
	}
	return nil
}

// ValidateKnowledgeReadBinding enforces the frozen V1 read policy: global
// items are readable, team/user/project/conversation/workstream items are
// readable only when their identity matches the trusted binding. Retrieval
// resolves these rules before any relevance search; a model-supplied scope
// selector grants no access.
func ValidateKnowledgeReadBinding(kind KnowledgeScopeKind, scopeID string, binding KnowledgeWriteBinding) error {
	if !validKnowledgeScopeKind(kind) {
		return fmt.Errorf("%w: %q", ErrKnowledgeInvalidScope, kind)
	}
	switch kind {
	case KnowledgeScopeGlobal:
		if scopeID != "" {
			return fmt.Errorf("%w: scope %q must not carry an identity", ErrKnowledgeInvalidScope, kind)
		}
		return nil
	case KnowledgeScopeTeam:
		if binding.Team == "" || scopeID != binding.Team {
			return fmt.Errorf("%w: team scope %q", ErrKnowledgeReadNotAllowed, scopeID)
		}
	case KnowledgeScopeUser:
		owner := SlackOwnerKey(binding.Conversation, binding.Actor)
		if owner == "" || scopeID != owner {
			return fmt.Errorf("%w: user scope %q", ErrKnowledgeReadNotAllowed, scopeID)
		}
	case KnowledgeScopeProject:
		if binding.Project == "" || scopeID != binding.Project {
			return fmt.Errorf("%w: project scope %q", ErrKnowledgeReadNotAllowed, scopeID)
		}
	case KnowledgeScopeConversation:
		if binding.Conversation == "" || scopeID != string(binding.Conversation) {
			return fmt.Errorf("%w: conversation scope %q", ErrKnowledgeReadNotAllowed, scopeID)
		}
	case KnowledgeScopeWorkstream:
		if binding.WorkstreamID == "" || scopeID != binding.WorkstreamID {
			return fmt.Errorf("%w: workstream scope %q", ErrKnowledgeReadNotAllowed, scopeID)
		}
	}
	return nil
}

// KnowledgeScopeRef identifies one readable scope under the frozen V1 read
// policy. Storage reads filter by the closed set of readable scopes so an
// unreadable item is indistinguishable from a missing one and scope
// identities never leak through read errors.
type KnowledgeScopeRef struct {
	Kind KnowledgeScopeKind
	ID   string
}

// KnowledgeReadableScopes derives the closed set of scopes readable by the
// trusted binding. Global is always readable; team, user, project,
// conversation, and workstream scopes join the set only when their identity
// is present in the binding. A missing identity never grants a scope.
func KnowledgeReadableScopes(binding KnowledgeWriteBinding) []KnowledgeScopeRef {
	scopes := []KnowledgeScopeRef{{Kind: KnowledgeScopeGlobal}}
	if strings.TrimSpace(binding.Team) != "" {
		scopes = append(scopes, KnowledgeScopeRef{Kind: KnowledgeScopeTeam, ID: binding.Team})
	}
	if owner := SlackOwnerKey(binding.Conversation, binding.Actor); owner != "" {
		scopes = append(scopes, KnowledgeScopeRef{Kind: KnowledgeScopeUser, ID: owner})
	}
	if binding.Project != "" {
		scopes = append(scopes, KnowledgeScopeRef{Kind: KnowledgeScopeProject, ID: binding.Project})
	}
	if binding.Conversation != "" {
		scopes = append(scopes, KnowledgeScopeRef{Kind: KnowledgeScopeConversation, ID: string(binding.Conversation)})
	}
	if binding.WorkstreamID != "" {
		scopes = append(scopes, KnowledgeScopeRef{Kind: KnowledgeScopeWorkstream, ID: binding.WorkstreamID})
	}
	return scopes
}

type KnowledgeAction string

const (
	KnowledgeActionRemember KnowledgeAction = "remember"
	KnowledgeActionCorrect  KnowledgeAction = "correct"
	KnowledgeActionForget   KnowledgeAction = "forget"
	KnowledgeActionArchive  KnowledgeAction = "archive"
	KnowledgeActionDispute  KnowledgeAction = "dispute"
	KnowledgeActionInspect  KnowledgeAction = "inspect"
)

func validKnowledgeAction(action KnowledgeAction) bool {
	switch action {
	case KnowledgeActionRemember, KnowledgeActionCorrect, KnowledgeActionForget,
		KnowledgeActionArchive, KnowledgeActionDispute, KnowledgeActionInspect:
		return true
	default:
		return false
	}
}

// ValidateKnowledgeAction rejects any mutation that does not belong to the
// knowledge surface. Workstream, task, and question actions are separate
// authority classes and can never be expressed here.
func ValidateKnowledgeAction(action KnowledgeAction) error {
	if !validKnowledgeAction(action) {
		return fmt.Errorf("unknown knowledge action %q", action)
	}
	return nil
}

// KnowledgeCommandReceipt is the global command identity for one mutating
// knowledge command. It is committed before the target mutation: one source
// reference may execute exactly one canonical command payload, a retry with
// the identical payload reuses the receipt, and a different payload under
// the same identity is rejected.
type KnowledgeCommandReceipt struct {
	SourceRef     string
	Action        KnowledgeAction
	PayloadDigest string
	Target        string
}

func (r KnowledgeCommandReceipt) Validate() error {
	if strings.TrimSpace(r.SourceRef) == "" {
		return fmt.Errorf("%w: command receipt requires a source reference", ErrKnowledgeInvalidSource)
	}
	if err := ValidateKnowledgeSourceRef(r.SourceRef); err != nil {
		return err
	}
	switch r.Action {
	case KnowledgeActionRemember, KnowledgeActionCorrect, KnowledgeActionForget,
		KnowledgeActionArchive, KnowledgeActionDispute:
	default:
		return fmt.Errorf("%w: command receipt action %q is not a mutating action", ErrKnowledgeInvalidSource, r.Action)
	}
	if len(r.PayloadDigest) != 64 {
		return fmt.Errorf("%w: command receipt payload digest must be a 64-character sha256 hex string", ErrKnowledgeInvalidCommandDigest)
	}
	for _, c := range r.PayloadDigest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: command receipt payload digest must be lowercase hex", ErrKnowledgeInvalidCommandDigest)
		}
	}
	if strings.TrimSpace(r.Target) == "" {
		return fmt.Errorf("%w: command receipt requires a target identity", ErrKnowledgeInvalidSource)
	}
	if utf8.RuneCountInString(r.Target) > DefaultMaxKnowledgeSourceRefRunes {
		return fmt.Errorf("%w: command receipt target exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, DefaultMaxKnowledgeSourceRefRunes)
	}
	return nil
}

func KnowledgeSubjectDigest(subject string, scopeKind KnowledgeScopeKind, scopeID string) string {
	sum := sha256.Sum256([]byte(string(scopeKind) + "\x00" + scopeID + "\x00" + subject))
	return hex.EncodeToString(sum[:])
}

type KnowledgeTombstone struct {
	ID            int
	SubjectDigest string
	ScopeKind     KnowledgeScopeKind
	ScopeID       string
	ForgottenAt   time.Time
	SourceRef     string
}

func (t KnowledgeTombstone) Validate() error {
	return t.ValidateWithLimits(DefaultKnowledgeLimits())
}

func (t KnowledgeTombstone) ValidateWithLimits(limits KnowledgeLimits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if len(t.SubjectDigest) != 64 {
		return fmt.Errorf("%w: subject digest must be a 64-character sha256 hex string", ErrKnowledgeInvalidTombstoneDigest)
	}
	for _, r := range t.SubjectDigest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("%w: subject digest must be lowercase hex", ErrKnowledgeInvalidTombstoneDigest)
		}
	}
	if err := ValidateKnowledgeScope(t.ScopeKind, t.ScopeID, limits); err != nil {
		return err
	}
	if err := ValidateMemoryReferenceText(t.ScopeID); err != nil {
		return fmt.Errorf("%w: scope identity: %v", ErrKnowledgeInvalidScope, err)
	}
	if t.ForgottenAt.IsZero() {
		return errors.New("knowledge tombstone requires a forgotten timestamp")
	}
	if strings.TrimSpace(t.SourceRef) == "" {
		return fmt.Errorf("%w: tombstone requires a source reference", ErrKnowledgeInvalidSource)
	}
	if err := ValidateMemoryReferenceText(t.SourceRef); err != nil {
		return fmt.Errorf("%w: source reference: %v", ErrKnowledgeInvalidSource, err)
	}
	if utf8.RuneCountInString(t.SourceRef) > limits.MaxSourceRefRunes {
		return fmt.Errorf("%w: source reference exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxSourceRefRunes)
	}
	return nil
}

// Blocks reports whether a write targeting the given subject and scope is
// covered by this tombstone and must not be written again. The digest covers
// subject, scope kind, and scope identity, so a matching digest proves the
// candidate belongs to the same scope.
func (t KnowledgeTombstone) Blocks(subject string, scopeKind KnowledgeScopeKind, scopeID string) bool {
	return t.SubjectDigest == KnowledgeSubjectDigest(subject, scopeKind, scopeID)
}

type KnowledgePreferenceStatus string

const (
	KnowledgePreferenceActive   KnowledgePreferenceStatus = "active"
	KnowledgePreferenceArchived KnowledgePreferenceStatus = "archived"
)

func validKnowledgePreferenceStatus(status KnowledgePreferenceStatus) bool {
	return status == KnowledgePreferenceActive || status == KnowledgePreferenceArchived
}

type KnowledgePreference struct {
	ID        int
	OwnerKey  string
	Key       string
	Value     KnowledgeValue
	Status    KnowledgePreferenceStatus
	SourceRef string
	Revision  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (p KnowledgePreference) Validate() error {
	return p.ValidateWithLimits(DefaultKnowledgeLimits())
}

func (p KnowledgePreference) ValidateWithLimits(limits KnowledgeLimits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	limits = limits.withDefaults()
	if strings.TrimSpace(p.OwnerKey) == "" {
		return fmt.Errorf("%w: preference owner must not be empty", ErrKnowledgeInvalidValue)
	}
	if err := ValidateMemoryReferenceText(p.OwnerKey); err != nil {
		return fmt.Errorf("%w: owner: %v", ErrKnowledgeInvalidValue, err)
	}
	if strings.TrimSpace(p.Key) == "" {
		return fmt.Errorf("%w: preference key must not be empty", ErrKnowledgeInvalidValue)
	}
	if utf8.RuneCountInString(p.Key) > limits.MaxPreferenceKeyRunes {
		return fmt.Errorf("%w: preference key exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxPreferenceKeyRunes)
	}
	if err := ValidateMemoryReferenceText(p.Key); err != nil {
		return fmt.Errorf("%w: key: %v", ErrKnowledgeInvalidValue, err)
	}
	if p.Value.Kind == KnowledgeValueReference {
		return fmt.Errorf("%w: preferences accept scalar values only", ErrKnowledgeInvalidValue)
	}
	if err := p.Value.validate(KnowledgePredicateIs, limits); err != nil {
		return err
	}
	if !validKnowledgePreferenceStatus(p.Status) {
		return fmt.Errorf("%w: %q", ErrKnowledgeInvalidValue, p.Status)
	}
	if strings.TrimSpace(p.SourceRef) == "" {
		return fmt.Errorf("%w: preference requires a source reference", ErrKnowledgeInvalidSource)
	}
	if err := ValidateMemoryReferenceText(p.SourceRef); err != nil {
		return fmt.Errorf("%w: source reference: %v", ErrKnowledgeInvalidSource, err)
	}
	if utf8.RuneCountInString(p.SourceRef) > limits.MaxSourceRefRunes {
		return fmt.Errorf("%w: source reference exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxSourceRefRunes)
	}
	return nil
}

// ValidateCandidate admits a new preference against the trusted binding:
// preferences are explicit human actions, so the owner must be the trusted
// actor's owner key.
func (p KnowledgePreference) ValidateCandidate(limits KnowledgeLimits, binding KnowledgeWriteBinding) error {
	if err := p.ValidateWithLimits(limits); err != nil {
		return err
	}
	owner := SlackOwnerKey(binding.Conversation, binding.Actor)
	if owner == "" || p.OwnerKey != owner {
		return fmt.Errorf("%w: preference owner %q must be the trusted actor owner key", ErrKnowledgeScopeBindingMismatch, p.OwnerKey)
	}
	return nil
}

type KnowledgeDocumentStatus string

const (
	KnowledgeDocumentActive   KnowledgeDocumentStatus = "active"
	KnowledgeDocumentArchived KnowledgeDocumentStatus = "archived"
)

func validKnowledgeDocumentStatus(status KnowledgeDocumentStatus) bool {
	return status == KnowledgeDocumentActive || status == KnowledgeDocumentArchived
}

type KnowledgeProvenance string

const (
	KnowledgeProvenanceLegacyCurated KnowledgeProvenance = "legacy_curated_document"
	KnowledgeProvenanceCurated       KnowledgeProvenance = "curated"
)

func validKnowledgeProvenance(provenance KnowledgeProvenance) bool {
	return provenance == KnowledgeProvenanceLegacyCurated || provenance == KnowledgeProvenanceCurated
}

type KnowledgeDocumentID string

type KnowledgeDocument struct {
	ID            KnowledgeDocumentID
	Subject       string
	ScopeKind     KnowledgeScopeKind
	ScopeID       string
	ContentDigest string
	ContentHandle string
	SourceID      string
	SourceRev     int
	Provenance    KnowledgeProvenance
	Status        KnowledgeDocumentStatus
	Revision      int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (d KnowledgeDocument) Validate() error {
	return d.ValidateWithLimits(DefaultKnowledgeLimits())
}

func (d KnowledgeDocument) ValidateWithLimits(limits KnowledgeLimits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	limits = limits.withDefaults()
	if err := ValidateKnowledgeScope(d.ScopeKind, d.ScopeID, limits); err != nil {
		return err
	}
	if strings.TrimSpace(d.Subject) == "" {
		return fmt.Errorf("%w: document subject must not be empty", ErrKnowledgeInvalidValue)
	}
	if utf8.RuneCountInString(d.Subject) > limits.MaxSubjectRunes {
		return fmt.Errorf("%w: document subject exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxSubjectRunes)
	}
	if len(d.ContentDigest) != 64 {
		return fmt.Errorf("%w: content digest must be a 64-character sha256 hex string", ErrKnowledgeInvalidDocumentDigest)
	}
	for _, r := range d.ContentDigest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("%w: content digest must be lowercase hex", ErrKnowledgeInvalidDocumentDigest)
		}
	}
	if strings.TrimSpace(d.ContentHandle) == "" {
		return fmt.Errorf("%w: document requires a content handle", ErrKnowledgeInvalidValue)
	}
	if utf8.RuneCountInString(d.ContentHandle) > limits.MaxSourceRefRunes {
		return fmt.Errorf("%w: content handle exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxSourceRefRunes)
	}
	if err := ValidateMemoryReferenceText(d.ContentHandle); err != nil {
		return fmt.Errorf("%w: content handle: %v", ErrKnowledgeInvalidValue, err)
	}
	if !validKnowledgeProvenance(d.Provenance) {
		return fmt.Errorf("%w: unknown provenance %q", ErrKnowledgeInvalidValue, d.Provenance)
	}
	switch d.Provenance {
	case KnowledgeProvenanceLegacyCurated:
		if strings.TrimSpace(d.SourceID) == "" {
			return fmt.Errorf("%w: legacy documents require their original source identity", ErrKnowledgeInvalidValue)
		}
		if utf8.RuneCountInString(d.SourceID) > limits.MaxSourceRefRunes {
			return fmt.Errorf("%w: source identity exceeds maximum of %d characters", ErrKnowledgeLimitExceeded, limits.MaxSourceRefRunes)
		}
		if err := ValidateMemoryReferenceText(d.SourceID); err != nil {
			return fmt.Errorf("%w: source identity: %v", ErrKnowledgeInvalidValue, err)
		}
		if d.SourceRev < 1 {
			return fmt.Errorf("%w: legacy documents require their original revision", ErrKnowledgeInvalidValue)
		}
	case KnowledgeProvenanceCurated:
		if d.SourceID != "" || d.SourceRev != 0 {
			return fmt.Errorf("%w: curated documents must not carry legacy source identity", ErrKnowledgeInvalidValue)
		}
	}
	if !validKnowledgeDocumentStatus(d.Status) {
		return fmt.Errorf("%w: %q", ErrKnowledgeInvalidValue, d.Status)
	}
	return nil
}

type KnowledgeEvidenceKind string

const (
	KnowledgeEvidenceSource   KnowledgeEvidenceKind = "source"
	KnowledgeEvidenceDecision KnowledgeEvidenceKind = "decision"
)

func validKnowledgeEvidenceKind(kind KnowledgeEvidenceKind) bool {
	return kind == KnowledgeEvidenceSource || kind == KnowledgeEvidenceDecision
}

// KnowledgeEvidence is an episodic reference to the conversation ledger. It
// never copies exchange content as truth.
type KnowledgeEvidence struct {
	ID              int
	ClaimRevision   int
	ConversationKey ConversationKey
	ExchangeTS      string
	AuthorID        string
	Kind            KnowledgeEvidenceKind
}

// ValidKnowledgeConversationKey reports whether a key matches the canonical
// Slack forms produced by Invocation.ConversationKey.
func ValidKnowledgeConversationKey(key ConversationKey) bool {
	parts := strings.Split(string(key), ":")
	if len(parts) < 4 || parts[0] != "slack" || !PlausibleTeamID(parts[1]) {
		return false
	}
	switch parts[2] {
	case "dm":
		if len(parts) == 4 {
			return PlausibleChannelID(parts[3])
		}
		return len(parts) == 6 && parts[4] == "thread" && PlausibleChannelID(parts[3]) && slackTimestampPattern.MatchString(parts[5])
	case "channel":
		return len(parts) == 6 && parts[3] != "" && PlausibleChannelID(parts[3]) && parts[4] == "thread" && slackTimestampPattern.MatchString(parts[5])
	default:
		return false
	}
}

// Validate enforces the episodic-reference ledger contract: a canonical
// conversation key, a Slack timestamp, a plausible Slack user identity, and
// a bounded known evidence kind. Content is never copied.
func (e KnowledgeEvidence) Validate() error {
	if !validKnowledgeEvidenceKind(e.Kind) {
		return fmt.Errorf("%w: unknown evidence kind %q", ErrKnowledgeInvalidEvidence, e.Kind)
	}
	if e.ClaimRevision <= 0 {
		return fmt.Errorf("%w: claim revision must be positive", ErrKnowledgeInvalidEvidence)
	}
	if !ValidKnowledgeConversationKey(e.ConversationKey) {
		return fmt.Errorf("%w: conversation key %q is not canonical", ErrKnowledgeInvalidEvidence, e.ConversationKey)
	}
	if utf8.RuneCountInString(string(e.ConversationKey)) > HardMaxKnowledgeSourceRefRunes {
		return fmt.Errorf("%w: conversation key is too long", ErrKnowledgeInvalidEvidence)
	}
	if !slackTimestampPattern.MatchString(e.ExchangeTS) {
		return fmt.Errorf("%w: exchange timestamp %q is not a Slack timestamp", ErrKnowledgeInvalidEvidence, e.ExchangeTS)
	}
	if !PlausibleUserID(e.AuthorID) {
		return fmt.Errorf("%w: author %q is not a plausible Slack user ID", ErrKnowledgeInvalidEvidence, e.AuthorID)
	}
	return nil
}

// KnowledgeCard is the complete atomic unit of knowledge recall. It is
// selected whole or not at all and never truncated inside a frame.
type KnowledgeCard struct {
	ClaimID         KnowledgeClaimID
	Subject         string
	Predicate       KnowledgePredicate
	Value           KnowledgeValue
	ScopeKind       KnowledgeScopeKind
	ScopeID         string
	SourceClass     KnowledgeSourceClass
	SourceRef       string
	Status          KnowledgeClaimStatus
	ValidFrom       time.Time
	ValidUntil      time.Time
	SupersedesID    KnowledgeClaimID
	RetrievalReason string
}

// CardFromClaim renders the recall contract for one claim evaluated at the
// given time, so expiry is reflected in the card status. Retrieval reasons
// are assigned by the retrieval pipeline (TRD 06), never by claim authors.
func CardFromClaim(claim KnowledgeClaim, reason string, now time.Time) KnowledgeCard {
	return KnowledgeCard{
		ClaimID: claim.ID, Subject: claim.Subject, Predicate: claim.Predicate,
		Value: claim.Value, ScopeKind: claim.ScopeKind, ScopeID: claim.ScopeID,
		SourceClass: claim.SourceClass, SourceRef: claim.SourceRef,
		Status: claim.EffectiveStatus(now), ValidFrom: claim.ValidFrom, ValidUntil: claim.ValidUntil,
		SupersedesID: claim.SupersedesID, RetrievalReason: reason,
	}
}

// Render produces the canonical complete card text. Selection cost is
// computed over this exact rendering so validity and framing overhead are
// never omitted.
func (c KnowledgeCard) Render() string {
	var b strings.Builder
	b.WriteString(c.Subject)
	b.WriteString(" ")
	b.WriteString(string(c.Predicate))
	b.WriteString(" ")
	b.WriteString(c.Value.render())
	if !c.ValidFrom.IsZero() || !c.ValidUntil.IsZero() {
		b.WriteString(" (")
		if !c.ValidFrom.IsZero() {
			b.WriteString("from ")
			b.WriteString(c.ValidFrom.Format(time.RFC3339))
			if !c.ValidUntil.IsZero() {
				b.WriteString(" ")
			}
		}
		if !c.ValidUntil.IsZero() {
			b.WriteString("until ")
			b.WriteString(c.ValidUntil.Format(time.RFC3339))
		}
		b.WriteString(")")
	}
	b.WriteString(" [")
	b.WriteString(string(c.ScopeKind))
	if c.ScopeID != "" {
		b.WriteString(":")
		b.WriteString(c.ScopeID)
	}
	b.WriteString("; ")
	b.WriteString(string(c.SourceClass))
	b.WriteString("; ")
	b.WriteString(c.SourceRef)
	b.WriteString("; ")
	b.WriteString(string(c.Status))
	if c.SupersedesID != "" {
		b.WriteString("; supersedes ")
		b.WriteString(string(c.SupersedesID))
	}
	b.WriteString("; reason: ")
	b.WriteString(c.RetrievalReason)
	b.WriteString("]")
	return b.String()
}

func (v KnowledgeValue) render() string {
	switch v.Kind {
	case KnowledgeValueNumber:
		return strconv.FormatFloat(v.Number, 'g', -1, 64)
	case KnowledgeValueBoolean:
		if v.Boolean {
			return "true"
		}
		return "false"
	case KnowledgeValueReference:
		return v.Reference
	default:
		return v.Text
	}
}

// Runes counts the canonical rendered card in Unicode code points. It is the
// deterministic default cost for atomic selection; frame compilation may
// supply provider-shaped token costs instead.
func (c KnowledgeCard) Runes() int {
	return utf8.RuneCountInString(c.Render())
}

const knowledgeCardsPreamble = "[KNOWLEDGE]\n"

// RenderKnowledgeCards produces the canonical combined card block including
// the shared preamble and separators. The default cumulative cost is
// computed over this exact rendering.
func RenderKnowledgeCards(cards []KnowledgeCard) string {
	if len(cards) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(knowledgeCardsPreamble)
	for i, card := range cards {
		if i > 0 {
			b.WriteString("\n---\n")
		}
		b.WriteString(card.Render())
	}
	b.WriteString("\n")
	return b.String()
}

// KnowledgeCardCostFunc computes the cumulative cost of a candidate card
// selection. It may return an error when exact costing is unavailable;
// selection then fails closed rather than admitting an unbudgeted frame.
type KnowledgeCardCostFunc func(selected []KnowledgeCard) (int, error)

// FitKnowledgeCards selects whole cards under the budget. No card is cut in
// the middle; cards that do not fit are dropped entirely. Costing is
// cumulative over the combined rendering (shared preamble, separators, and
// framing included). The context compiler passes a provider-shaped cost
// function so the selection honors the exact model budget; an unavailable
// exact counter fails the selection closed.
func FitKnowledgeCards(cards []KnowledgeCard, budget int, cost KnowledgeCardCostFunc) ([]KnowledgeCard, error) {
	if budget <= 0 {
		return nil, nil
	}
	if cost == nil {
		cost = func(selected []KnowledgeCard) (int, error) {
			return utf8.RuneCountInString(RenderKnowledgeCards(selected)), nil
		}
	}
	selected := make([]KnowledgeCard, 0, len(cards))
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

type KnowledgeProjectionStatus string

const (
	KnowledgeProjectionPending    KnowledgeProjectionStatus = "pending"
	KnowledgeProjectionProcessing KnowledgeProjectionStatus = "processing"
	KnowledgeProjectionDone       KnowledgeProjectionStatus = "done"
	KnowledgeProjectionFailed     KnowledgeProjectionStatus = "failed"
)

// KnowledgeProjectionItem is one coalesced projection trigger. The OKF
// projection worker renders a whole snapshot per item; multiple knowledge
// commits before the worker runs collapse into the snapshot the item
// describes.
type KnowledgeProjectionItem struct {
	ID          int
	Status      KnowledgeProjectionStatus
	Attempts    int
	NextAttempt time.Time
	LeaseUntil  time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type KnowledgeLimits struct {
	MaxClaimsPerSubject   int
	MaxSubjectRunes       int
	MaxValueRunes         int
	MaxReferenceRunes     int
	MaxScopeIDRunes       int
	MaxSourceRefRunes     int
	MaxPreferences        int
	MaxPreferenceKeyRunes int
	MaxDocuments          int
	MaxCardBudget         int
	MaxClaimsListing      int
}

func DefaultKnowledgeLimits() KnowledgeLimits {
	return KnowledgeLimits{
		MaxClaimsPerSubject:   DefaultMaxKnowledgeClaimsPerSubject,
		MaxSubjectRunes:       DefaultMaxKnowledgeSubjectRunes,
		MaxValueRunes:         DefaultMaxKnowledgeValueRunes,
		MaxReferenceRunes:     DefaultMaxKnowledgeReferenceRunes,
		MaxScopeIDRunes:       DefaultMaxKnowledgeScopeIDRunes,
		MaxSourceRefRunes:     DefaultMaxKnowledgeSourceRefRunes,
		MaxPreferences:        DefaultMaxKnowledgePreferences,
		MaxPreferenceKeyRunes: DefaultMaxKnowledgePreferenceKeyRunes,
		MaxDocuments:          DefaultMaxKnowledgeDocuments,
		MaxCardBudget:         DefaultMaxKnowledgeCardBudget,
		MaxClaimsListing:      DefaultMaxKnowledgeClaimsListing,
	}
}

// HardKnowledgeLimits returns the hard storage maxima. Projection validation
// must use hard bounds rather than configured defaults: a row persisted under
// a larger configured limit must never be rejected by a smaller default
// during rendering.
func HardKnowledgeLimits() KnowledgeLimits {
	return KnowledgeLimits{
		MaxClaimsPerSubject:   HardMaxKnowledgeClaimsPerSubject,
		MaxSubjectRunes:       HardMaxKnowledgeSubjectRunes,
		MaxValueRunes:         HardMaxKnowledgeValueRunes,
		MaxReferenceRunes:     HardMaxKnowledgeReferenceRunes,
		MaxScopeIDRunes:       HardMaxKnowledgeScopeIDRunes,
		MaxSourceRefRunes:     HardMaxKnowledgeSourceRefRunes,
		MaxPreferences:        HardMaxKnowledgePreferences,
		MaxPreferenceKeyRunes: HardMaxKnowledgePreferenceKeyRunes,
		MaxDocuments:          HardMaxKnowledgeDocuments,
		MaxCardBudget:         HardMaxKnowledgeCardBudget,
		MaxClaimsListing:      HardMaxKnowledgeClaimsListing,
	}
}

// ValidKnowledgeOpaqueID reports whether id matches the host-generated
// opaque identifier shape for the given prefix: the prefix followed by 24
// lowercase hexadecimal characters. Projection validation uses it so
// malformed persisted identifiers fail closed instead of rendering
// structure or invalid UTF-8.
func ValidKnowledgeOpaqueID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+24 {
		return false
	}
	for _, r := range id[len(prefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// ValidKnowledgeClaimID reports whether id matches the host-generated claim
// identifier shape.
func ValidKnowledgeClaimID(id KnowledgeClaimID) bool {
	return ValidKnowledgeOpaqueID(string(id), "kclaim_")
}

// ValidKnowledgeDocumentID reports whether id matches the host-generated
// document identifier shape.
func ValidKnowledgeDocumentID(id KnowledgeDocumentID) bool {
	return ValidKnowledgeOpaqueID(string(id), "kdoc_")
}

// KnowledgeLegacyImportResult reports one deterministic legacy import run.
// Imported counts newly created documents; Archived counts existing active
// documents mirrored to archived because their legacy topic was archived;
// Skipped counts legacy topics whose identity already exists with matching
// state (previous import, archived document) or whose subject was forgotten
// (tombstone). Failed imports return an error and create nothing.
type KnowledgeLegacyImportResult struct {
	Imported int
	Archived int
	Skipped  int
}

// LegacyTopicDocumentID derives the deterministic knowledge document
// identifier for an imported legacy topic. The identity is a pure function
// of the opaque topic ID, so replays and concurrent runs converge on the
// same document row.
func LegacyTopicDocumentID(topicID TopicID) KnowledgeDocumentID {
	sum := sha256.Sum256([]byte("legacy_topic\x00" + string(topicID)))
	return KnowledgeDocumentID("kdoc_" + hex.EncodeToString(sum[:12]))
}

// LegacyTopicDocumentSubjectSuffix derives the opaque disambiguation suffix
// for a legacy topic subject. It is a pure function of the opaque topic ID
// and never leaks owner or content identity.
func LegacyTopicDocumentSubjectSuffix(topicID TopicID) string {
	sum := sha256.Sum256([]byte("legacy_subject\x00" + string(topicID)))
	return "#" + hex.EncodeToString(sum[:4])
}

// LegacyTopicRevisionHandle builds the immutable revision reference for an
// imported legacy topic document. The handle points at the append-only
// memory_topic_revisions row the content digest was computed from, never at
// the mutable topic row, so the referenced bytes always match the digest.
func LegacyTopicRevisionHandle(topicID TopicID, revisionID int64) string {
	return fmt.Sprintf("memory_topics:%s:revision:%d", topicID, revisionID)
}

// KnowledgeDocumentFromLegacyTopic maps one legacy topic to its imported
// knowledge document. People topics bind the user scope to their canonical
// owner key and fail closed without a valid owner; every other topic
// imports at global scope, preserving existing visibility without
// amplifying it. Archived legacy topics import as archived documents, so
// legacy visibility is never widened. The subject is a pure function of the
// topic (readable title plus an opaque topic-derived suffix), so subject
// assignment never depends on import history and duplicate valid titles can
// always be imported. The content handle is resolved by the store against
// the immutable revision row. The mapping is deterministic: replaying the
// same topic produces the same document. The caller validates the result
// against its limits; scope construction failures return
// ErrKnowledgeInvalidScope.
func KnowledgeDocumentFromLegacyTopic(topic Topic) (KnowledgeDocument, error) {
	scopeKind := KnowledgeScopeGlobal
	scopeID := ""
	if topic.BundlePath == "people" {
		if !ValidSlackOwnerKey(topic.OwnerKey) {
			return KnowledgeDocument{}, fmt.Errorf("%w: person topic %q has no valid owner", ErrKnowledgeInvalidScope, topic.ID)
		}
		scopeKind = KnowledgeScopeUser
		scopeID = topic.OwnerKey
	}
	status := KnowledgeDocumentActive
	if topic.Status == TopicStatusArchived {
		status = KnowledgeDocumentArchived
	} else if topic.Status != TopicStatusActive && topic.Status != "" {
		return KnowledgeDocument{}, fmt.Errorf("%w: legacy topic %q has unknown status %q", ErrKnowledgeInvalidValue, topic.ID, topic.Status)
	}
	sum := sha256.Sum256([]byte(topic.Content))
	return KnowledgeDocument{
		ID:            LegacyTopicDocumentID(topic.ID),
		Subject:       topic.Title + " " + LegacyTopicDocumentSubjectSuffix(topic.ID),
		ScopeKind:     scopeKind,
		ScopeID:       scopeID,
		ContentDigest: hex.EncodeToString(sum[:]),
		SourceID:      string(topic.ID),
		SourceRev:     topic.CurrentRev,
		Provenance:    KnowledgeProvenanceLegacyCurated,
		Status:        status,
	}, nil
}

func (l KnowledgeLimits) withDefaults() KnowledgeLimits {
	defaults := DefaultKnowledgeLimits()
	if l.MaxClaimsPerSubject == 0 {
		l.MaxClaimsPerSubject = defaults.MaxClaimsPerSubject
	}
	if l.MaxSubjectRunes == 0 {
		l.MaxSubjectRunes = defaults.MaxSubjectRunes
	}
	if l.MaxValueRunes == 0 {
		l.MaxValueRunes = defaults.MaxValueRunes
	}
	if l.MaxReferenceRunes == 0 {
		l.MaxReferenceRunes = defaults.MaxReferenceRunes
	}
	if l.MaxScopeIDRunes == 0 {
		l.MaxScopeIDRunes = defaults.MaxScopeIDRunes
	}
	if l.MaxSourceRefRunes == 0 {
		l.MaxSourceRefRunes = defaults.MaxSourceRefRunes
	}
	if l.MaxPreferences == 0 {
		l.MaxPreferences = defaults.MaxPreferences
	}
	if l.MaxPreferenceKeyRunes == 0 {
		l.MaxPreferenceKeyRunes = defaults.MaxPreferenceKeyRunes
	}
	if l.MaxDocuments == 0 {
		l.MaxDocuments = defaults.MaxDocuments
	}
	if l.MaxCardBudget == 0 {
		l.MaxCardBudget = defaults.MaxCardBudget
	}
	if l.MaxClaimsListing == 0 {
		l.MaxClaimsListing = defaults.MaxClaimsListing
	}
	return l
}

// WithDefaults fills omitted optional bounds while preserving configured
// values.
func (l KnowledgeLimits) WithDefaults() KnowledgeLimits { return l.withDefaults() }

func (l KnowledgeLimits) Validate() error {
	l = l.withDefaults()
	if l.MaxClaimsPerSubject <= 0 || l.MaxClaimsPerSubject > HardMaxKnowledgeClaimsPerSubject {
		return fmt.Errorf("%w: max claims per subject must be between 1 and %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgeClaimsPerSubject)
	}
	if l.MaxSubjectRunes <= 0 || l.MaxSubjectRunes > HardMaxKnowledgeSubjectRunes {
		return fmt.Errorf("%w: max subject length must be between 1 and %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgeSubjectRunes)
	}
	if l.MaxValueRunes <= 0 || l.MaxValueRunes > HardMaxKnowledgeValueRunes {
		return fmt.Errorf("%w: max value length must be between 1 and %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgeValueRunes)
	}
	if l.MaxReferenceRunes <= 0 || l.MaxReferenceRunes > HardMaxKnowledgeReferenceRunes {
		return fmt.Errorf("%w: max reference length must be between 1 and %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgeReferenceRunes)
	}
	if l.MaxScopeIDRunes <= 0 || l.MaxScopeIDRunes > HardMaxKnowledgeScopeIDRunes {
		return fmt.Errorf("%w: max scope identity length must be between 1 and %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgeScopeIDRunes)
	}
	if l.MaxSourceRefRunes <= 0 || l.MaxSourceRefRunes > HardMaxKnowledgeSourceRefRunes {
		return fmt.Errorf("%w: max source reference length must be between 1 and %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgeSourceRefRunes)
	}
	if l.MaxPreferences <= 0 || l.MaxPreferences > HardMaxKnowledgePreferences {
		return fmt.Errorf("%w: max preferences must be between 1 and %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgePreferences)
	}
	if l.MaxPreferenceKeyRunes <= 0 || l.MaxPreferenceKeyRunes > HardMaxKnowledgePreferenceKeyRunes {
		return fmt.Errorf("%w: max preference key length must be between 1 and %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgePreferenceKeyRunes)
	}
	if l.MaxDocuments <= 0 || l.MaxDocuments > HardMaxKnowledgeDocuments {
		return fmt.Errorf("%w: max documents must be between 1 and %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgeDocuments)
	}
	if l.MaxCardBudget <= 0 || l.MaxCardBudget > HardMaxKnowledgeCardBudget {
		return fmt.Errorf("%w: max card budget must be between 1 and %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgeCardBudget)
	}
	if l.MaxClaimsListing <= 0 || l.MaxClaimsListing > HardMaxKnowledgeClaimsListing {
		return fmt.Errorf("%w: max claims listing must be between 1 and %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgeClaimsListing)
	}
	return nil
}

// KnowledgeIndexRebuildResult reports what an operator-triggered
// reconstructible knowledge index rebuild did (TRD 06 finding 12). It
// carries no identity, content, or digest: only which reconstructible
// indexes were scheduled.
type KnowledgeIndexRebuildResult struct {
	LexicalRebuilt bool
	// EmbeddingSkippedReason is non-empty when the vector index was not
	// rebuilt: either embeddings are disabled, or embedding index rebuild is
	// not yet wired to this operator entry point.
	EmbeddingSkippedReason string
}
