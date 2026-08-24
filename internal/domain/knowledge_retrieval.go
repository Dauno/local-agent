package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultKnowledgeRetrievalTimeoutSeconds          = 2
	HardMaxKnowledgeRetrievalTimeoutSeconds          = 30
	DefaultKnowledgeRetrievalMaxQueryRunes           = 2048
	HardMaxKnowledgeRetrievalMaxQueryRunes           = 8000
	DefaultKnowledgeRetrievalMaxCandidatesPerChannel = 32
	HardMaxKnowledgeRetrievalMaxCandidatesPerChannel = 128
	DefaultKnowledgeRetrievalMaxCards                = 8
	HardMaxKnowledgeRetrievalMaxCards                = 32
	DefaultKnowledgeRetrievalMaxDocumentBytes        = 64 * 1024
	HardMaxKnowledgeRetrievalMaxDocumentBytes        = 1 << 20
	DefaultKnowledgeRetrievalWorkerIntervalSeconds   = 10
	HardMaxKnowledgeRetrievalWorkerIntervalSeconds   = 3600
	DefaultKnowledgeRetrievalWorkerMaxRetries        = 3
	HardMaxKnowledgeRetrievalWorkerMaxRetries        = 10
	DefaultKnowledgeRetrievalWorkerBatchSize         = 32
	HardMaxKnowledgeRetrievalWorkerBatchSize         = 128
	DefaultKnowledgeEmbeddingTimeoutSeconds          = 5
	HardMaxKnowledgeEmbeddingTimeoutSeconds          = 120
	HardMaxKnowledgeEmbeddingDimensions              = 4096
	HardMaxKnowledgeMinSimilarityBasisPoints         = 10000
	// HardMaxKnowledgeQueueItemIDRunes bounds queue item identity runes.
	HardMaxKnowledgeQueueItemIDRunes = 256
	// HardMaxKnowledgeQueueLeaseSeconds bounds the queue claim lease.
	HardMaxKnowledgeQueueLeaseSeconds = 3600
	// HardMaxKnowledgeQueueListLimit bounds reconciliation page sizes.
	HardMaxKnowledgeQueueListLimit = HardMaxKnowledgeRetrievalWorkerBatchSize
	// HardMaxKnowledgeRetrievalRank bounds per-channel one-based ranks: a
	// channel can never rank more candidates than its hard cap.
	HardMaxKnowledgeRetrievalRank = HardMaxKnowledgeRetrievalMaxCandidatesPerChannel
	// HardMaxKnowledgeRetrievalCandidates bounds the diagnostic candidate
	// count across the four channels.
	HardMaxKnowledgeRetrievalCandidates = HardMaxKnowledgeRetrievalMaxCandidatesPerChannel * MaxKnowledgeRetrievalChannels
)

// ValidKnowledgeQueueLease reports whether a claim lease is positive and
// within the hard bound, so workers can never hold unbounded claims.
func ValidKnowledgeQueueLease(lease time.Duration) bool {
	return lease > 0 && lease <= time.Duration(HardMaxKnowledgeQueueLeaseSeconds)*time.Second
}

// ValidKnowledgeQueueListLimit reports whether a reconciliation page size is
// within the hard bound.
func ValidKnowledgeQueueListLimit(limit int) bool {
	return limit >= 1 && limit <= HardMaxKnowledgeQueueListLimit
}

// ValidKnowledgeIndexSourceDigest reports whether a digest is absent or a
// lowercase 64-character SHA-256 hex string, the shape carried by index
// hits for stale detection.
func ValidKnowledgeIndexSourceDigest(digest string) bool {
	if digest == "" {
		return true
	}
	return isLowerHexDigest(digest)
}

// KnowledgeRetrievalLimits carries the effective retrieval bounds for one
// turn or one worker lifetime. Zero fields fall back to the frozen defaults;
// validation enforces the hard maxima. Embedding dimensions and the minimum
// similarity threshold are zero when embeddings are disabled.
type KnowledgeRetrievalLimits struct {
	TimeoutSeconds           int
	MaxQueryRunes            int
	MaxCandidatesPerChannel  int
	MaxCards                 int
	MaxCardTokens            int
	MaxDocumentBytes         int
	WorkerIntervalSeconds    int
	WorkerMaxRetries         int
	WorkerBatchSize          int
	EmbeddingTimeoutSeconds  int
	EmbeddingDimensions      int
	MinSimilarityBasisPoints int
}

func DefaultKnowledgeRetrievalLimits() KnowledgeRetrievalLimits {
	return KnowledgeRetrievalLimits{
		TimeoutSeconds:           DefaultKnowledgeRetrievalTimeoutSeconds,
		MaxQueryRunes:            DefaultKnowledgeRetrievalMaxQueryRunes,
		MaxCandidatesPerChannel:  DefaultKnowledgeRetrievalMaxCandidatesPerChannel,
		MaxCards:                 DefaultKnowledgeRetrievalMaxCards,
		MaxCardTokens:            DefaultMaxKnowledgeCardBudget,
		MaxDocumentBytes:         DefaultKnowledgeRetrievalMaxDocumentBytes,
		WorkerIntervalSeconds:    DefaultKnowledgeRetrievalWorkerIntervalSeconds,
		WorkerMaxRetries:         DefaultKnowledgeRetrievalWorkerMaxRetries,
		WorkerBatchSize:          DefaultKnowledgeRetrievalWorkerBatchSize,
		EmbeddingTimeoutSeconds:  DefaultKnowledgeEmbeddingTimeoutSeconds,
		EmbeddingDimensions:      0,
		MinSimilarityBasisPoints: 0,
	}
}

func (l KnowledgeRetrievalLimits) withDefaults() KnowledgeRetrievalLimits {
	defaults := DefaultKnowledgeRetrievalLimits()
	if l.TimeoutSeconds == 0 {
		l.TimeoutSeconds = defaults.TimeoutSeconds
	}
	if l.MaxQueryRunes == 0 {
		l.MaxQueryRunes = defaults.MaxQueryRunes
	}
	if l.MaxCandidatesPerChannel == 0 {
		l.MaxCandidatesPerChannel = defaults.MaxCandidatesPerChannel
	}
	if l.MaxCards == 0 {
		l.MaxCards = defaults.MaxCards
	}
	if l.MaxCardTokens == 0 {
		l.MaxCardTokens = defaults.MaxCardTokens
	}
	if l.MaxDocumentBytes == 0 {
		l.MaxDocumentBytes = defaults.MaxDocumentBytes
	}
	if l.WorkerIntervalSeconds == 0 {
		l.WorkerIntervalSeconds = defaults.WorkerIntervalSeconds
	}
	if l.WorkerMaxRetries == 0 {
		l.WorkerMaxRetries = defaults.WorkerMaxRetries
	}
	if l.WorkerBatchSize == 0 {
		l.WorkerBatchSize = defaults.WorkerBatchSize
	}
	if l.EmbeddingTimeoutSeconds == 0 {
		l.EmbeddingTimeoutSeconds = defaults.EmbeddingTimeoutSeconds
	}
	return l
}

// WithDefaults fills omitted optional bounds while preserving configured
// values.
func (l KnowledgeRetrievalLimits) WithDefaults() KnowledgeRetrievalLimits { return l.withDefaults() }

func (l KnowledgeRetrievalLimits) Validate() error {
	l = l.withDefaults()
	rangeChecks := []struct {
		name  string
		value int
		min   int
		max   int
	}{
		{"timeout", l.TimeoutSeconds, 1, HardMaxKnowledgeRetrievalTimeoutSeconds},
		{"max query runes", l.MaxQueryRunes, 1, HardMaxKnowledgeRetrievalMaxQueryRunes},
		{"max candidates per channel", l.MaxCandidatesPerChannel, 1, HardMaxKnowledgeRetrievalMaxCandidatesPerChannel},
		{"max cards", l.MaxCards, 1, HardMaxKnowledgeRetrievalMaxCards},
		{"max card tokens", l.MaxCardTokens, 1, HardMaxKnowledgeCardBudget},
		{"max document bytes", l.MaxDocumentBytes, 1, HardMaxKnowledgeRetrievalMaxDocumentBytes},
		{"worker interval", l.WorkerIntervalSeconds, 1, HardMaxKnowledgeRetrievalWorkerIntervalSeconds},
		{"worker retries", l.WorkerMaxRetries, 1, HardMaxKnowledgeRetrievalWorkerMaxRetries},
		{"worker batch size", l.WorkerBatchSize, 1, HardMaxKnowledgeRetrievalWorkerBatchSize},
		{"embedding timeout", l.EmbeddingTimeoutSeconds, 1, HardMaxKnowledgeEmbeddingTimeoutSeconds},
	}
	for _, check := range rangeChecks {
		if check.value < check.min || check.value > check.max {
			return fmt.Errorf("%w: knowledge retrieval %s must be between %d and %d", ErrKnowledgeLimitExceeded, check.name, check.min, check.max)
		}
	}
	if l.EmbeddingDimensions < 0 || l.EmbeddingDimensions > HardMaxKnowledgeEmbeddingDimensions {
		return fmt.Errorf("%w: knowledge embedding dimensions must be 0 or between 1 and %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgeEmbeddingDimensions)
	}
	if l.MinSimilarityBasisPoints < 0 || l.MinSimilarityBasisPoints > HardMaxKnowledgeMinSimilarityBasisPoints {
		return fmt.Errorf("%w: knowledge embedding minimum similarity must be 0 or between 1 and %d", ErrKnowledgeLimitExceeded, HardMaxKnowledgeMinSimilarityBasisPoints)
	}
	return nil
}

// KnowledgeRetrievalItemKind is the closed set of retrievable knowledge item
// classes. Index rows, queues, and ranking identities are bound to it.
type KnowledgeRetrievalItemKind string

const (
	KnowledgeRetrievalClaim      KnowledgeRetrievalItemKind = "claim"
	KnowledgeRetrievalPreference KnowledgeRetrievalItemKind = "preference"
	KnowledgeRetrievalDocument   KnowledgeRetrievalItemKind = "document"
)

func validKnowledgeRetrievalItemKind(kind KnowledgeRetrievalItemKind) bool {
	switch kind {
	case KnowledgeRetrievalClaim, KnowledgeRetrievalPreference, KnowledgeRetrievalDocument:
		return true
	default:
		return false
	}
}

// ValidKnowledgeRetrievalItemKind is the exported closed-kind predicate for
// port-layer union and queue validation.
func ValidKnowledgeRetrievalItemKind(kind KnowledgeRetrievalItemKind) bool {
	return validKnowledgeRetrievalItemKind(kind)
}

// KnowledgeRetrievalItemKindOrder assigns the stable tie-break order used by
// knowledge-rank-v1 after tier and fused score. Unknown kinds sort last.
func KnowledgeRetrievalItemKindOrder(kind KnowledgeRetrievalItemKind) int {
	switch kind {
	case KnowledgeRetrievalClaim:
		return 0
	case KnowledgeRetrievalPreference:
		return 1
	case KnowledgeRetrievalDocument:
		return 2
	default:
		return 3
	}
}

// KnowledgeRetrievalReason is the closed set of channel reasons attached to a
// candidate. Reasons merge into a sorted set during deduplication.
type KnowledgeRetrievalReason string

const (
	KnowledgeRetrievalReasonExactSubject    KnowledgeRetrievalReason = "exact_subject"
	KnowledgeRetrievalReasonExactIdentifier KnowledgeRetrievalReason = "exact_identifier"
	KnowledgeRetrievalReasonRelation        KnowledgeRetrievalReason = "relation"
	KnowledgeRetrievalReasonLexical         KnowledgeRetrievalReason = "lexical"
	KnowledgeRetrievalReasonSemantic        KnowledgeRetrievalReason = "semantic"
)

func validKnowledgeRetrievalReason(reason KnowledgeRetrievalReason) bool {
	switch reason {
	case KnowledgeRetrievalReasonExactSubject, KnowledgeRetrievalReasonExactIdentifier,
		KnowledgeRetrievalReasonRelation, KnowledgeRetrievalReasonLexical,
		KnowledgeRetrievalReasonSemantic:
		return true
	default:
		return false
	}
}

// KnowledgeRetrievalChannelForReason maps each retrieval reason to its owning
// retrieval channel for the selected-card metric. Unknown reasons fail closed.
func KnowledgeRetrievalChannelForReason(reason KnowledgeRetrievalReason) (KnowledgeRetrievalChannel, bool) {
	switch reason {
	case KnowledgeRetrievalReasonExactSubject, KnowledgeRetrievalReasonExactIdentifier:
		return KnowledgeRetrievalChannelExact, true
	case KnowledgeRetrievalReasonRelation:
		return KnowledgeRetrievalChannelRelation, true
	case KnowledgeRetrievalReasonLexical:
		return KnowledgeRetrievalChannelLexical, true
	case KnowledgeRetrievalReasonSemantic:
		return KnowledgeRetrievalChannelSemantic, true
	default:
		return "", false
	}
}

// KnowledgeRankTier is the fixed tier ladder of knowledge-rank-v1: exact
// matches outrank relation expansion, which outranks the fused lexical and
// semantic channels.
type KnowledgeRankTier int

const (
	KnowledgeRankTierExact    KnowledgeRankTier = 0
	KnowledgeRankTierRelation KnowledgeRankTier = 1
	KnowledgeRankTierFused    KnowledgeRankTier = 2
)

func validKnowledgeRankTier(tier KnowledgeRankTier) bool {
	return tier >= KnowledgeRankTierExact && tier <= KnowledgeRankTierFused
}

const (
	// KnowledgeRankingPolicyID versions the deterministic ranking contract.
	KnowledgeRankingPolicyID = "knowledge-rank-v1"
	// knowledgeRRFScale and knowledgeRRFConstant are the integer
	// reciprocal-rank fusion parameters: each tier-2 channel contributes
	// scale/(constant+rank) with one-based ranks, so no floating score
	// controls final ordering.
	knowledgeRRFScale    = 1_000_000
	knowledgeRRFConstant = 60
)

// KnowledgeRankCandidate is one channel contribution before fusion. The tier
// is assigned by the retrieval orchestration; the pure ranking policy only
// merges and orders. LexicalRank and SemanticRank are one-based per-channel
// ranks (zero when the channel did not produce the candidate).
type KnowledgeRankCandidate struct {
	Kind         KnowledgeRetrievalItemKind
	ID           string
	Tier         KnowledgeRankTier
	Reasons      []KnowledgeRetrievalReason
	LexicalRank  int
	SemanticRank int
}

// KnowledgeRankedCandidate is one fused identity after deduplication and
// deterministic ordering.
type KnowledgeRankedCandidate struct {
	Kind       KnowledgeRetrievalItemKind
	ID         string
	Tier       KnowledgeRankTier
	FusedScore int
	Reasons    []KnowledgeRetrievalReason
}

// RankKnowledgeCandidates applies knowledge-rank-v1: candidates deduplicate
// only by (item_kind, item_id), channel reasons merge into a sorted closed
// set, tier-2 channels fuse through integer reciprocal rank using the best
// (lowest) per-channel rank for each identity, and ordering is tier, then
// fused score, then item kind, then stable identity. Status and recency
// never contribute. Malformed contributions — unknown kinds, unbounded or
// empty identities, unknown tiers, invalid reasons, or negative channel
// ranks — fail closed instead of entering the arithmetic. The output is a
// pure function of the input set: permutations produce identical ordered
// results.
func RankKnowledgeCandidates(candidates []KnowledgeRankCandidate) ([]KnowledgeRankedCandidate, error) {
	merged := make(map[identityKey]KnowledgeRankedCandidate)
	seen := make(map[identityKey]bool)
	seenReasons := make(map[identityKey]map[KnowledgeRetrievalReason]struct{})
	bestLexical := make(map[identityKey]int)
	bestSemantic := make(map[identityKey]int)
	for _, candidate := range candidates {
		if err := candidate.validate(); err != nil {
			return nil, err
		}
		key := identityKey{kind: string(candidate.Kind), id: candidate.ID}
		fused := merged[key]
		fused.Kind = candidate.Kind
		fused.ID = candidate.ID
		if !seen[key] || candidate.Tier < fused.Tier {
			fused.Tier = candidate.Tier
		}
		seen[key] = true
		if reasonSet := seenReasons[key]; reasonSet == nil {
			reasonSet = make(map[KnowledgeRetrievalReason]struct{})
			seenReasons[key] = reasonSet
		}
		for _, reason := range candidate.Reasons {
			if _, exists := seenReasons[key][reason]; !exists {
				seenReasons[key][reason] = struct{}{}
				fused.Reasons = append(fused.Reasons, reason)
			}
		}
		if candidate.LexicalRank > 0 && (bestLexical[key] == 0 || candidate.LexicalRank < bestLexical[key]) {
			bestLexical[key] = candidate.LexicalRank
		}
		if candidate.SemanticRank > 0 && (bestSemantic[key] == 0 || candidate.SemanticRank < bestSemantic[key]) {
			bestSemantic[key] = candidate.SemanticRank
		}
		merged[key] = fused
	}
	ranked := make([]KnowledgeRankedCandidate, 0, len(merged))
	for key, candidate := range merged {
		if candidate.Tier == KnowledgeRankTierFused {
			if rank := bestLexical[key]; rank > 0 {
				candidate.FusedScore += knowledgeRRFScale / (knowledgeRRFConstant + rank)
			}
			if rank := bestSemantic[key]; rank > 0 {
				candidate.FusedScore += knowledgeRRFScale / (knowledgeRRFConstant + rank)
			}
		}
		sort.Slice(candidate.Reasons, func(i, j int) bool { return candidate.Reasons[i] < candidate.Reasons[j] })
		ranked = append(ranked, candidate)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Tier != ranked[j].Tier {
			return ranked[i].Tier < ranked[j].Tier
		}
		if ranked[i].FusedScore != ranked[j].FusedScore {
			return ranked[i].FusedScore > ranked[j].FusedScore
		}
		if ranked[i].Kind != ranked[j].Kind {
			return KnowledgeRetrievalItemKindOrder(ranked[i].Kind) < KnowledgeRetrievalItemKindOrder(ranked[j].Kind)
		}
		return ranked[i].ID < ranked[j].ID
	})
	return ranked, nil
}

func (c KnowledgeRankCandidate) validate() error {
	if !validKnowledgeRetrievalItemKind(c.Kind) {
		return fmt.Errorf("%w: unknown ranking item kind %q", ErrKnowledgeInvalidValue, c.Kind)
	}
	if strings.TrimSpace(c.ID) == "" || utf8.RuneCountInString(c.ID) > HardMaxKnowledgeQueueItemIDRunes {
		return fmt.Errorf("%w: ranking identity %q is not bounded", ErrKnowledgeInvalidValue, c.ID)
	}
	if !validKnowledgeRankTier(c.Tier) {
		return fmt.Errorf("%w: unknown ranking tier %d", ErrKnowledgeInvalidValue, c.Tier)
	}
	for _, reason := range c.Reasons {
		if !validKnowledgeRetrievalReason(reason) {
			return fmt.Errorf("%w: unknown ranking reason %q", ErrKnowledgeInvalidValue, reason)
		}
	}
	if c.LexicalRank < 0 || c.SemanticRank < 0 ||
		c.LexicalRank > HardMaxKnowledgeRetrievalRank || c.SemanticRank > HardMaxKnowledgeRetrievalRank {
		return fmt.Errorf("%w: ranking channel ranks must be between 0 and %d", ErrKnowledgeInvalidValue, HardMaxKnowledgeRetrievalRank)
	}
	if len(c.Reasons) == 0 {
		return fmt.Errorf("%w: ranking candidates require at least one reason", ErrKnowledgeInvalidValue)
	}
	// Tier/reason/rank coherence: exact and relation candidates carry no
	// channel ranks and only their tier's reason set; fused candidates need
	// at least one channel rank, only lexical/semantic reasons, and each
	// channel rank must match the cited reason set.
	switch c.Tier {
	case KnowledgeRankTierExact:
		if c.LexicalRank != 0 || c.SemanticRank != 0 {
			return fmt.Errorf("%w: exact candidates must not carry channel ranks", ErrKnowledgeInvalidValue)
		}
		for _, reason := range c.Reasons {
			if reason != KnowledgeRetrievalReasonExactSubject && reason != KnowledgeRetrievalReasonExactIdentifier {
				return fmt.Errorf("%w: exact candidate carries reason %q", ErrKnowledgeInvalidValue, reason)
			}
		}
	case KnowledgeRankTierRelation:
		if c.LexicalRank != 0 || c.SemanticRank != 0 {
			return fmt.Errorf("%w: relation candidates must not carry channel ranks", ErrKnowledgeInvalidValue)
		}
		for _, reason := range c.Reasons {
			if reason != KnowledgeRetrievalReasonRelation {
				return fmt.Errorf("%w: relation candidate carries reason %q", ErrKnowledgeInvalidValue, reason)
			}
		}
	case KnowledgeRankTierFused:
		if c.LexicalRank == 0 && c.SemanticRank == 0 {
			return fmt.Errorf("%w: fused candidates require at least one channel rank", ErrKnowledgeInvalidValue)
		}
		for _, reason := range c.Reasons {
			if reason != KnowledgeRetrievalReasonLexical && reason != KnowledgeRetrievalReasonSemantic {
				return fmt.Errorf("%w: fused candidate carries reason %q", ErrKnowledgeInvalidValue, reason)
			}
		}
		if (c.LexicalRank > 0) != hasKnowledgeRetrievalReason(c.Reasons, KnowledgeRetrievalReasonLexical) {
			return fmt.Errorf("%w: fused lexical rank and reason set do not match", ErrKnowledgeInvalidValue)
		}
		if (c.SemanticRank > 0) != hasKnowledgeRetrievalReason(c.Reasons, KnowledgeRetrievalReasonSemantic) {
			return fmt.Errorf("%w: fused semantic rank and reason set do not match", ErrKnowledgeInvalidValue)
		}
	}
	return nil
}

func hasKnowledgeRetrievalReason(reasons []KnowledgeRetrievalReason, target KnowledgeRetrievalReason) bool {
	for _, reason := range reasons {
		if reason == target {
			return true
		}
	}
	return false
}

type identityKey struct {
	kind string
	id   string
}

// CapRankedKnowledgeCandidates bounds a ranked list deterministically. A
// non-positive cap selects nothing, so a misconfigured limit can never
// admit the full ranked set.
func CapRankedKnowledgeCandidates(candidates []KnowledgeRankedCandidate, limit int) []KnowledgeRankedCandidate {
	if limit <= 0 {
		return nil
	}
	if len(candidates) <= limit {
		return candidates
	}
	return candidates[:limit]
}

// KnowledgeRetrievalFailure is the closed set of optional-channel failure
// categories carried in per-turn diagnostics. They never become top-level
// errors.
type KnowledgeRetrievalFailure string

const (
	KnowledgeRetrievalRelationUnavailable KnowledgeRetrievalFailure = "relation_unavailable"
	KnowledgeRetrievalLexicalUnavailable  KnowledgeRetrievalFailure = "lexical_unavailable"
	KnowledgeRetrievalSemanticUnavailable KnowledgeRetrievalFailure = "semantic_unavailable"
	KnowledgeRetrievalCounterUnavailable  KnowledgeRetrievalFailure = "counter_unavailable"
	// MaxKnowledgeRetrievalFailures bounds the diagnostic failure set.
	MaxKnowledgeRetrievalFailures = 4
)

func validKnowledgeRetrievalFailure(failure KnowledgeRetrievalFailure) bool {
	switch failure {
	case KnowledgeRetrievalRelationUnavailable, KnowledgeRetrievalLexicalUnavailable,
		KnowledgeRetrievalSemanticUnavailable, KnowledgeRetrievalCounterUnavailable:
		return true
	default:
		return false
	}
}

// KnowledgeRetrievalChannel is the closed enabled-channel set reported in
// diagnostics and metric labels.
type KnowledgeRetrievalChannel string

const (
	KnowledgeRetrievalChannelExact    KnowledgeRetrievalChannel = "exact"
	KnowledgeRetrievalChannelRelation KnowledgeRetrievalChannel = "relation"
	KnowledgeRetrievalChannelLexical  KnowledgeRetrievalChannel = "lexical"
	KnowledgeRetrievalChannelSemantic KnowledgeRetrievalChannel = "semantic"
	// MaxKnowledgeRetrievalChannels bounds the enabled-channel set.
	MaxKnowledgeRetrievalChannels = 4
)

func ValidKnowledgeRetrievalChannel(channel KnowledgeRetrievalChannel) bool {
	switch channel {
	case KnowledgeRetrievalChannelExact, KnowledgeRetrievalChannelRelation,
		KnowledgeRetrievalChannelLexical, KnowledgeRetrievalChannelSemantic:
		return true
	default:
		return false
	}
}

// KnowledgeRetrievalOmission is the closed set of card omission categories
// reported in diagnostics: an oversized document, a card that cannot fit by
// itself, or an unavailable provider-shaped counter.
type KnowledgeRetrievalOmission string

const (
	KnowledgeRetrievalOmissionDocumentOverLimit  KnowledgeRetrievalOmission = "document_over_limit"
	KnowledgeRetrievalOmissionCardOverBudget     KnowledgeRetrievalOmission = "card_over_budget"
	KnowledgeRetrievalOmissionCounterUnavailable KnowledgeRetrievalOmission = "counter_unavailable"
	// MaxKnowledgeRetrievalOmissions bounds the omission category set.
	MaxKnowledgeRetrievalOmissions = 3
)

func validKnowledgeRetrievalOmission(omission KnowledgeRetrievalOmission) bool {
	switch omission {
	case KnowledgeRetrievalOmissionDocumentOverLimit, KnowledgeRetrievalOmissionCardOverBudget,
		KnowledgeRetrievalOmissionCounterUnavailable:
		return true
	default:
		return false
	}
}

// KnowledgeVectorEncodingVersion freezes the embedding encoding contract
// reported in diagnostics when embeddings are enabled.
const KnowledgeVectorEncodingVersion = "l2-f32le-v1"

// KnowledgeRetrievalRequest is the trusted retrieval surface for one
// user-origin turn. The binding is host-resolved and ExchangeTS binds the
// request to the originating user-message turn; the current message is
// untrusted data. Retrieval never invokes write-policy methods on the
// binding. Now is injected for deterministic eligibility and card validity.
type KnowledgeRetrievalRequest struct {
	Binding        KnowledgeWriteBinding
	Workstream     *WorkstreamSnapshot
	ExchangeTS     string
	CurrentMessage string
	Now            time.Time
	Limits         KnowledgeRetrievalLimits
}

// Validate enforces the retrieval preconditions: a plausible team and actor,
// a canonical conversation key whose team matches the binding, a canonical
// Slack timestamp binding the request to its originating turn, a coherent
// workstream snapshot (nil snapshots carry no project or workstream scope;
// a present snapshot must match the binding's actor, conversation, project,
// and workstream identity), an injected clock, and validated limits.
// Without a valid binding no retrieval request exists and no cards may be
// produced.
func (r KnowledgeRetrievalRequest) Validate() error {
	if !PlausibleTeamID(r.Binding.Team) {
		return fmt.Errorf("%w: retrieval requires a plausible trusted team", ErrKnowledgeInvalidValue)
	}
	if !PlausibleUserID(r.Binding.Actor) {
		return fmt.Errorf("%w: retrieval requires a plausible trusted actor", ErrKnowledgeInvalidValue)
	}
	if !ValidKnowledgeConversationKey(r.Binding.Conversation) {
		return fmt.Errorf("%w: retrieval requires a canonical conversation key", ErrKnowledgeInvalidValue)
	}
	parts := strings.Split(string(r.Binding.Conversation), ":")
	if len(parts) < 4 || parts[1] != r.Binding.Team {
		return fmt.Errorf("%w: retrieval conversation %q does not belong to team %q", ErrKnowledgeInvalidValue, r.Binding.Conversation, r.Binding.Team)
	}
	if !slackTimestampPattern.MatchString(r.ExchangeTS) {
		return fmt.Errorf("%w: retrieval requires the originating Slack exchange timestamp", ErrKnowledgeInvalidValue)
	}
	if r.Workstream == nil {
		if r.Binding.Project != "" || r.Binding.WorkstreamID != "" {
			return fmt.Errorf("%w: a nil workstream snapshot must not carry project or workstream scope", ErrKnowledgeInvalidValue)
		}
	} else {
		if strings.TrimSpace(r.Workstream.ID) == "" || strings.TrimSpace(r.Workstream.Project) == "" {
			return fmt.Errorf("%w: a bound workstream snapshot requires identity and a registered project", ErrKnowledgeInvalidValue)
		}
		if r.Workstream.Status != WorkstreamActive {
			return fmt.Errorf("%w: workstream status %q is not active and grants no project or workstream scope", ErrKnowledgeInvalidValue, r.Workstream.Status)
		}
		if r.Workstream.ID != r.Binding.WorkstreamID {
			return fmt.Errorf("%w: workstream snapshot %q does not match the binding workstream %q", ErrKnowledgeInvalidValue, r.Workstream.ID, r.Binding.WorkstreamID)
		}
		if r.Workstream.Project != r.Binding.Project {
			return fmt.Errorf("%w: workstream project %q does not match the binding project %q", ErrKnowledgeInvalidValue, r.Workstream.Project, r.Binding.Project)
		}
		if r.Workstream.OwnerActor != r.Binding.Actor {
			return fmt.Errorf("%w: workstream owner %q does not match the trusted actor", ErrKnowledgeInvalidValue, r.Workstream.OwnerActor)
		}
		if r.Workstream.ConversationKey != r.Binding.Conversation {
			return fmt.Errorf("%w: workstream conversation %q does not match the trusted conversation", ErrKnowledgeInvalidValue, r.Workstream.ConversationKey)
		}
	}
	if r.Now.IsZero() {
		return fmt.Errorf("%w: retrieval requires an injected clock", ErrKnowledgeInvalidValue)
	}
	if err := r.Limits.Validate(); err != nil {
		return err
	}
	return nil
}

// KnowledgeRetrievalDiagnostics carries bounded content-free facts about one
// retrieval run for metrics and the epoch recorder. It never contains
// queries, card content, vectors, handles, digests, source references,
// actor, conversation, or credential values.
type KnowledgeRetrievalDiagnostics struct {
	RankingPolicy           string
	IndexFingerprintVersion string
	EnabledChannels         []KnowledgeRetrievalChannel
	SelectedIdentities      []string
	CandidateCount          int
	SelectedCount           int
	OmittedCount            int
	Failures                []KnowledgeRetrievalFailure
	Omissions               []KnowledgeRetrievalOmission
	Elapsed                 time.Duration
}

// KnowledgeRetrievalResult is the ordered, complete, untrusted frame-card
// candidate set for one user turn. An empty card list is a normal successful
// outcome.
type KnowledgeRetrievalResult struct {
	Cards       []KnowledgeFrameCard
	Diagnostics KnowledgeRetrievalDiagnostics
}

// ValidateKnowledgeRetrievalDiagnostics enforces the content-free diagnostic
// contract: the closed ranking policy, a closed fingerprint version, closed
// sorted deduplicated channel/omission/failure sets, bounded sorted
// deduplicated identities within the card cap, consistent non-negative
// counts, and non-negative elapsed duration.
func ValidateKnowledgeRetrievalDiagnostics(diagnostics KnowledgeRetrievalDiagnostics) error {
	if diagnostics.RankingPolicy != KnowledgeRankingPolicyID {
		return fmt.Errorf("%w: retrieval diagnostics policy %q is not the closed %q", ErrKnowledgeInvalidValue, diagnostics.RankingPolicy, KnowledgeRankingPolicyID)
	}
	if diagnostics.IndexFingerprintVersion != "" && diagnostics.IndexFingerprintVersion != KnowledgeVectorEncodingVersion {
		return fmt.Errorf("%w: retrieval diagnostics fingerprint version %q is not the closed %q", ErrKnowledgeInvalidValue, diagnostics.IndexFingerprintVersion, KnowledgeVectorEncodingVersion)
	}
	if diagnostics.CandidateCount < 0 || diagnostics.SelectedCount < 0 || diagnostics.OmittedCount < 0 {
		return fmt.Errorf("%w: retrieval diagnostics counts must be non-negative", ErrKnowledgeInvalidValue)
	}
	if diagnostics.CandidateCount > HardMaxKnowledgeRetrievalCandidates {
		return fmt.Errorf("%w: retrieval diagnostics candidate count exceeds %d", ErrKnowledgeInvalidValue, HardMaxKnowledgeRetrievalCandidates)
	}
	if diagnostics.SelectedCount > HardMaxKnowledgeRetrievalMaxCards {
		return fmt.Errorf("%w: retrieval diagnostics selected count exceeds %d", ErrKnowledgeInvalidValue, HardMaxKnowledgeRetrievalMaxCards)
	}
	if diagnostics.SelectedCount != len(diagnostics.SelectedIdentities) {
		return fmt.Errorf("%w: retrieval diagnostics selected count %d must equal the %d recorded identities", ErrKnowledgeInvalidValue, diagnostics.SelectedCount, len(diagnostics.SelectedIdentities))
	}
	if diagnostics.SelectedCount+diagnostics.OmittedCount > diagnostics.CandidateCount {
		return fmt.Errorf("%w: retrieval diagnostics selection counts exceed the candidate count", ErrKnowledgeInvalidValue)
	}
	if diagnostics.Elapsed < 0 {
		return fmt.Errorf("%w: retrieval diagnostics elapsed must be non-negative", ErrKnowledgeInvalidValue)
	}
	if err := validateClosedSortedSet(len(diagnostics.EnabledChannels), MaxKnowledgeRetrievalChannels); err != nil {
		return fmt.Errorf("%w: retrieval diagnostics channels: %v", ErrKnowledgeInvalidValue, err)
	}
	for i, channel := range diagnostics.EnabledChannels {
		if !ValidKnowledgeRetrievalChannel(channel) {
			return fmt.Errorf("%w: unknown retrieval channel %q", ErrKnowledgeInvalidValue, channel)
		}
		if i > 0 && diagnostics.EnabledChannels[i] <= diagnostics.EnabledChannels[i-1] {
			return fmt.Errorf("%w: retrieval diagnostics channels must be sorted and unique", ErrKnowledgeInvalidValue)
		}
	}
	if len(diagnostics.Failures) > MaxKnowledgeRetrievalFailures {
		return fmt.Errorf("%w: retrieval diagnostics failures exceed %d", ErrKnowledgeInvalidValue, MaxKnowledgeRetrievalFailures)
	}
	for i, failure := range diagnostics.Failures {
		if !validKnowledgeRetrievalFailure(failure) {
			return fmt.Errorf("%w: unknown retrieval failure category %q", ErrKnowledgeInvalidValue, failure)
		}
		if i > 0 && diagnostics.Failures[i] <= diagnostics.Failures[i-1] {
			return fmt.Errorf("%w: retrieval diagnostics failures must be sorted and unique", ErrKnowledgeInvalidValue)
		}
	}
	if len(diagnostics.Omissions) > MaxKnowledgeRetrievalOmissions {
		return fmt.Errorf("%w: retrieval diagnostics omissions exceed %d", ErrKnowledgeInvalidValue, MaxKnowledgeRetrievalOmissions)
	}
	for i, omission := range diagnostics.Omissions {
		if !validKnowledgeRetrievalOmission(omission) {
			return fmt.Errorf("%w: unknown retrieval omission category %q", ErrKnowledgeInvalidValue, omission)
		}
		if i > 0 && diagnostics.Omissions[i] <= diagnostics.Omissions[i-1] {
			return fmt.Errorf("%w: retrieval diagnostics omissions must be sorted and unique", ErrKnowledgeInvalidValue)
		}
	}
	if len(diagnostics.SelectedIdentities) > HardMaxKnowledgeRetrievalMaxCards {
		return fmt.Errorf("%w: retrieval diagnostics identities exceed %d", ErrKnowledgeInvalidValue, HardMaxKnowledgeRetrievalMaxCards)
	}
	for i, identity := range diagnostics.SelectedIdentities {
		if strings.TrimSpace(identity) == "" || utf8.RuneCountInString(identity) > MaxContextEpochIdentityRunes {
			return fmt.Errorf("%w: retrieval diagnostics identity is not bounded", ErrKnowledgeInvalidValue)
		}
		if i > 0 && diagnostics.SelectedIdentities[i] <= diagnostics.SelectedIdentities[i-1] {
			return fmt.Errorf("%w: retrieval diagnostics identities must be sorted and unique", ErrKnowledgeInvalidValue)
		}
	}
	return nil
}

func validateClosedSortedSet(length, max int) error {
	if length < 0 || length > max {
		return fmt.Errorf("set length %d exceeds %d", length, max)
	}
	return nil
}

// KnowledgeQueueStatus is the closed queue row lifecycle.
type KnowledgeQueueStatus string

const (
	KnowledgeQueuePending    KnowledgeQueueStatus = "pending"
	KnowledgeQueueProcessing KnowledgeQueueStatus = "processing"
	KnowledgeQueueDone       KnowledgeQueueStatus = "done"
	KnowledgeQueueFailed     KnowledgeQueueStatus = "failed"
)

func validKnowledgeQueueStatus(status KnowledgeQueueStatus) bool {
	switch status {
	case KnowledgeQueuePending, KnowledgeQueueProcessing, KnowledgeQueueDone, KnowledgeQueueFailed:
		return true
	default:
		return false
	}
}

// KnowledgeQueueFailureCode is the closed persisted queue failure code set.
// Transient failures retry without a persisted code and become
// attempts_exhausted only after the finite budget.
type KnowledgeQueueFailureCode string

const (
	KnowledgeQueueFailureSourceInvalid     KnowledgeQueueFailureCode = "source_invalid"
	KnowledgeQueueFailureProviderInvalid   KnowledgeQueueFailureCode = "provider_invalid"
	KnowledgeQueueFailureAttemptsExhausted KnowledgeQueueFailureCode = "attempts_exhausted"
)

func ValidKnowledgeQueueFailureCode(code KnowledgeQueueFailureCode) bool {
	switch code {
	case KnowledgeQueueFailureSourceInvalid, KnowledgeQueueFailureProviderInvalid, KnowledgeQueueFailureAttemptsExhausted:
		return true
	default:
		return false
	}
}

// KnowledgeQueueItem is one durable generation-CAS queue row. The claim
// token (generation plus lease identity) travels through every terminal and
// retry transition so a stale worker can never complete a reclaimed claim.
type KnowledgeQueueItem struct {
	Kind        KnowledgeRetrievalItemKind
	ID          string
	Status      KnowledgeQueueStatus
	Generation  int64
	Attempts    int
	NextAttempt time.Time
	LeaseUntil  time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Validate enforces the durable queue row contract: closed kind and status,
// bounded identity, non-negative generation and attempts, and timestamps.
func (i KnowledgeQueueItem) Validate() error {
	if !validKnowledgeRetrievalItemKind(i.Kind) {
		return fmt.Errorf("%w: unknown queue item kind %q", ErrKnowledgeInvalidValue, i.Kind)
	}
	if strings.TrimSpace(i.ID) == "" || utf8.RuneCountInString(i.ID) > HardMaxKnowledgeQueueItemIDRunes {
		return fmt.Errorf("%w: queue item identity is not bounded", ErrKnowledgeInvalidValue)
	}
	if !validKnowledgeQueueStatus(i.Status) {
		return fmt.Errorf("%w: unknown queue status %q", ErrKnowledgeInvalidValue, i.Status)
	}
	if i.Generation < 0 || i.Attempts < 0 {
		return fmt.Errorf("%w: queue generation and attempts must be non-negative", ErrKnowledgeInvalidValue)
	}
	if i.CreatedAt.IsZero() || i.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: queue item requires created and updated timestamps", ErrKnowledgeInvalidValue)
	}
	return nil
}

// KnowledgeQueueClaim is the structural claim token returned by a queue
// claim. Terminal and retry CAS operations must present this exact token, so
// a worker whose lease expired and was reclaimed can never mutate the new
// owner's claim.
type KnowledgeQueueClaim struct {
	Kind       KnowledgeRetrievalItemKind
	ID         string
	Generation int64
	LeaseUntil time.Time
}

// ValidateKnowledgeRetrievalOutcome enforces the closed retrieval outcome
// label set.
func ValidKnowledgeRetrievalOutcome(outcome string) bool {
	switch KnowledgeRetrievalOutcome(outcome) {
	case KnowledgeRetrievalOutcomeSuccess, KnowledgeRetrievalOutcomeEmpty,
		KnowledgeRetrievalOutcomeValidationRejected, KnowledgeRetrievalOutcomeUnavailable:
		return true
	default:
		return false
	}
}

// KnowledgeRetrievalReasonLabel is the closed reason label set for retrieval
// metrics: the four failure categories plus stale-index and oversized
// omissions.
func ValidKnowledgeRetrievalReasonLabel(reason string) bool {
	switch KnowledgeRetrievalFailure(reason) {
	case KnowledgeRetrievalRelationUnavailable, KnowledgeRetrievalLexicalUnavailable,
		KnowledgeRetrievalSemanticUnavailable, KnowledgeRetrievalCounterUnavailable:
		return true
	}
	switch KnowledgeRetrievalReasonLabel(reason) {
	case KnowledgeRetrievalReasonLabelStaleIndex, KnowledgeRetrievalReasonLabelOversized:
		return true
	default:
		return false
	}
}

// ValidateKnowledgeMetricLabels enforces the closed per-metric label
// contract: known retrieval metrics accept only the channel/outcome/reason
// keys with closed values, mandatory keys must be present, and semantic
// combinations are frozen per metric (channel failures pair each
// channel-specific reason with its one compatible channel, stale-index
// metrics require the stale_index reason on a lexical or semantic channel,
// and the empty total only permits the empty outcome).
func ValidateKnowledgeMetricLabels(metric string, labels map[string]string) error {
	if !validKnowledgeRetrievalMetricName(metric) {
		return fmt.Errorf("%w: unknown knowledge retrieval metric %q", ErrKnowledgeInvalidValue, metric)
	}
	allowed := knowledgeRetrievalMetricLabelKeys[metric]
	required := knowledgeRetrievalMetricRequiredKeys[metric]
	for key, value := range labels {
		if !allowed[key] {
			return fmt.Errorf("%w: metric %q does not accept label %q", ErrKnowledgeInvalidValue, metric, key)
		}
		switch key {
		case MetricLabelChannel:
			if !ValidKnowledgeRetrievalChannel(KnowledgeRetrievalChannel(value)) {
				return fmt.Errorf("%w: unknown channel label value %q", ErrKnowledgeInvalidValue, value)
			}
		case MetricLabelOutcome:
			if !ValidKnowledgeRetrievalOutcome(value) {
				return fmt.Errorf("%w: unknown outcome label value %q", ErrKnowledgeInvalidValue, value)
			}
		case MetricLabelReason:
			if !ValidKnowledgeRetrievalReasonLabel(value) {
				return fmt.Errorf("%w: unknown reason label value %q", ErrKnowledgeInvalidValue, value)
			}
		}
	}
	for key := range required {
		if _, present := labels[key]; !present {
			return fmt.Errorf("%w: metric %q requires label %q", ErrKnowledgeInvalidValue, metric, key)
		}
	}
	switch metric {
	case MetricKnowledgeRetrievalChannelFailure:
		failure := KnowledgeRetrievalFailure(labels[MetricLabelReason])
		channel, ok := knowledgeRetrievalFailureChannel(failure)
		if !ok {
			return fmt.Errorf("%w: channel failure metric requires a channel-specific failure reason, not %q", ErrKnowledgeInvalidValue, labels[MetricLabelReason])
		}
		if KnowledgeRetrievalChannel(labels[MetricLabelChannel]) != channel {
			return fmt.Errorf("%w: channel %q is incompatible with failure reason %q", ErrKnowledgeInvalidValue, labels[MetricLabelChannel], labels[MetricLabelReason])
		}
	case MetricKnowledgeRetrievalEmptyTotal:
		if KnowledgeRetrievalOutcome(labels[MetricLabelOutcome]) != KnowledgeRetrievalOutcomeEmpty {
			return fmt.Errorf("%w: empty total metric only permits the empty outcome", ErrKnowledgeInvalidValue)
		}
	case MetricKnowledgeRetrievalStaleIndex:
		if KnowledgeRetrievalReasonLabel(labels[MetricLabelReason]) != KnowledgeRetrievalReasonLabelStaleIndex {
			return fmt.Errorf("%w: stale index metric requires the stale_index reason", ErrKnowledgeInvalidValue)
		}
		switch KnowledgeRetrievalChannel(labels[MetricLabelChannel]) {
		case KnowledgeRetrievalChannelLexical, KnowledgeRetrievalChannelSemantic:
		default:
			return fmt.Errorf("%w: stale index metric requires a lexical or semantic channel", ErrKnowledgeInvalidValue)
		}
	}
	return nil
}

// knowledgeRetrievalFailureChannel maps each channel-specific failure reason
// to its one compatible retrieval channel. The counter unavailability is a
// channel-independent diagnostic and is not admissible on the channel failure
// metric.
func knowledgeRetrievalFailureChannel(failure KnowledgeRetrievalFailure) (KnowledgeRetrievalChannel, bool) {
	switch failure {
	case KnowledgeRetrievalRelationUnavailable:
		return KnowledgeRetrievalChannelRelation, true
	case KnowledgeRetrievalLexicalUnavailable:
		return KnowledgeRetrievalChannelLexical, true
	case KnowledgeRetrievalSemanticUnavailable:
		return KnowledgeRetrievalChannelSemantic, true
	default:
		return "", false
	}
}
