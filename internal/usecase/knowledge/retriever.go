package knowledge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// RetrieverDependencies carries the authoritative reader, the lexical index,
// the document resolver, and the lexical queue for stale repair. Redact is
// the shared credential redactor applied to the query and to document
// content before card construction. Clock is the injected time source for
// validity evaluation. Metrics receives the closed retrieval metric samples
// with the frozen channel/outcome/reason labels only. Provider is the
// optional embedding provider: nil means the semantic channel stays
// disabled and the whole feature remains default-off.
type RetrieverDependencies struct {
	Reader   port.KnowledgeCandidateReader
	Index    port.KnowledgeIndex
	Resolver port.KnowledgeDocumentResolver
	Queue    port.KnowledgeQueueStore
	Provider port.EmbeddingProvider
	Clock    port.Clock
	Redact   func(string) string
	Metrics  port.MetricRecorder
}

// Retriever implements port.KnowledgeRetriever with the required channels:
// mandatory exact, optional one-hop relation, optional lexical FTS over
// redacted canonical index text, and optional semantic search when an
// embedding provider is wired. Every ranked identity is re-read
// authoritatively before card construction; stale index hits are excluded
// and enqueued for repair; documents resolve verified content and never
// fall back. An empty card list is a normal successful outcome.
type Retriever struct {
	reader   port.KnowledgeCandidateReader
	index    port.KnowledgeIndex
	resolver port.KnowledgeDocumentResolver
	queue    port.KnowledgeQueueStore
	provider port.EmbeddingProvider
	clock    port.Clock
	redact   func(string) string
	metrics  port.MetricRecorder
}

var _ port.KnowledgeRetriever = (*Retriever)(nil)

// NewRetriever validates the retrieval dependencies and returns the
// orchestration for one user-origin turn pipeline. A nil embedding provider
// is valid and keeps the semantic channel disabled.
func NewRetriever(deps RetrieverDependencies) (*Retriever, error) {
	if deps.Reader == nil || deps.Index == nil || deps.Resolver == nil || deps.Queue == nil {
		return nil, errors.New("knowledge retriever dependencies are incomplete")
	}
	if deps.Clock == nil {
		deps.Clock = port.SystemClock{}
	}
	return &Retriever{
		reader: deps.Reader, index: deps.Index, resolver: deps.Resolver,
		queue: deps.Queue, provider: deps.Provider, clock: deps.Clock,
		redact: deps.Redact, metrics: deps.Metrics,
	}, nil
}

// recordOutcome emits the closed retrieval total and duration metrics for
// one attempt with the frozen outcome label.
func (r *Retriever) recordOutcome(started time.Time, outcome domain.KnowledgeRetrievalOutcome) {
	if r == nil || r.metrics == nil {
		return
	}
	labels := port.MetricLabels{domain.MetricLabelOutcome: string(outcome)}
	r.metrics.AddCounter(domain.MetricKnowledgeRetrievalTotal, 1, labels)
	r.metrics.Observe(domain.MetricKnowledgeRetrievalDuration, r.clock.Now().Sub(started).Seconds(), labels)
}

// recordCandidates emits the per-channel candidate counts for channels that
// produced candidates.
func (r *Retriever) recordCandidates(channel domain.KnowledgeRetrievalChannel, count int) {
	if r == nil || r.metrics == nil || count <= 0 {
		return
	}
	r.metrics.AddCounter(domain.MetricKnowledgeRetrievalCandidates, int64(count), port.MetricLabels{domain.MetricLabelChannel: string(channel)})
}

// recordChannelFailure emits the closed channel-failure counter with the
// channel paired to its compatible failure reason.
func (r *Retriever) recordChannelFailure(channel domain.KnowledgeRetrievalChannel, failure domain.KnowledgeRetrievalFailure) {
	if r == nil || r.metrics == nil {
		return
	}
	r.metrics.AddCounter(domain.MetricKnowledgeRetrievalChannelFailure, 1, port.MetricLabels{
		domain.MetricLabelChannel: string(channel),
		domain.MetricLabelReason:  string(failure),
	})
}

// recordStaleHit emits the closed stale-index counter for one excluded hit.
func (r *Retriever) recordStaleHit(channel domain.KnowledgeRetrievalChannel) {
	if r == nil || r.metrics == nil {
		return
	}
	r.metrics.AddCounter(domain.MetricKnowledgeRetrievalStaleIndex, 1, port.MetricLabels{
		domain.MetricLabelChannel: string(channel),
		domain.MetricLabelReason:  string(domain.KnowledgeRetrievalReasonLabelStaleIndex),
	})
}

// Retrieve validates the trusted request, builds the redacted bounded query,
// runs the mandatory exact channel and the optional relation and lexical
// channels, fuses candidates with knowledge-rank-v1, and constructs complete
// frame cards from authoritative re-reads. Optional channel failures are
// typed diagnostics and never top-level errors; authoritative read failures
// and request rejection fail closed with no partial cards.
func (r *Retriever) Retrieve(ctx context.Context, request domain.KnowledgeRetrievalRequest) (domain.KnowledgeRetrievalResult, error) {
	started := r.clock.Now()
	limits := request.Limits.WithDefaults()
	if err := request.Validate(); err != nil {
		r.recordOutcome(started, domain.KnowledgeRetrievalOutcomeValidationRejected)
		return domain.KnowledgeRetrievalResult{}, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(limits.TimeoutSeconds)*time.Second)
	defer cancel()

	query := ExtractKnowledgeQuery(request.CurrentMessage, request.Workstream, r.redact, limits.MaxQueryRunes)
	if query == "" {
		// An empty normalized query returns no cards without touching the
		// reader, the index, or the queues.
		r.recordOutcome(started, domain.KnowledgeRetrievalOutcomeEmpty)
		if r.metrics != nil {
			r.metrics.AddCounter(domain.MetricKnowledgeRetrievalEmptyTotal, 1, port.MetricLabels{domain.MetricLabelOutcome: string(domain.KnowledgeRetrievalOutcomeEmpty)})
		}
		return domain.KnowledgeRetrievalResult{Diagnostics: domain.KnowledgeRetrievalDiagnostics{
			RankingPolicy:   domain.KnowledgeRankingPolicyID,
			EnabledChannels: r.enabledRetrievalChannels(),
			Elapsed:         r.clock.Now().Sub(started),
		}}, nil
	}
	tokens := ExtractKnowledgeTokens(query)

	exactCandidates, err := r.reader.ReadExact(runCtx, request.Binding, request.Now, limits, query, tokens)
	if err != nil {
		r.recordOutcome(started, domain.KnowledgeRetrievalOutcomeUnavailable)
		return domain.KnowledgeRetrievalResult{}, wrapRetrievalChannelFailure("exact candidates", err, runCtx)
	}
	r.recordCandidates(domain.KnowledgeRetrievalChannelExact, len(exactCandidates))

	var failures []domain.KnowledgeRetrievalFailure
	var relationCandidates []port.KnowledgeEligibleCandidate
	relationCandidates, err = r.reader.ReadRelated(runCtx, request.Binding, request.Now, limits, exactCandidates)
	if err != nil {
		failures = append(failures, domain.KnowledgeRetrievalRelationUnavailable)
		r.recordChannelFailure(domain.KnowledgeRetrievalChannelRelation, domain.KnowledgeRetrievalRelationUnavailable)
	} else {
		r.recordCandidates(domain.KnowledgeRetrievalChannelRelation, len(relationCandidates))
	}

	scopes := domain.KnowledgeReadableScopes(request.Binding)
	owner := domain.SlackOwnerKey(request.Binding.Conversation, request.Binding.Actor)
	var lexicalCandidates []port.KnowledgeIndexHit
	lexicalCandidates, err = r.index.SearchLexical(runCtx, scopes, owner, query, limits.MaxCandidatesPerChannel)
	if err != nil {
		failures = append(failures, domain.KnowledgeRetrievalLexicalUnavailable)
		r.recordChannelFailure(domain.KnowledgeRetrievalChannelLexical, domain.KnowledgeRetrievalLexicalUnavailable)
	} else {
		r.recordCandidates(domain.KnowledgeRetrievalChannelLexical, len(lexicalCandidates))
	}

	var semanticCandidates []port.KnowledgeIndexHit
	if r.provider != nil {
		semanticCandidates, err = r.searchSemantic(runCtx, scopes, owner, query, limits)
		if err != nil {
			failures = append(failures, domain.KnowledgeRetrievalSemanticUnavailable)
			r.recordChannelFailure(domain.KnowledgeRetrievalChannelSemantic, domain.KnowledgeRetrievalSemanticUnavailable)
		} else {
			r.recordCandidates(domain.KnowledgeRetrievalChannelSemantic, len(semanticCandidates))
		}
	}

	// Channel work that raced past the deadline must not flow into ranking
	// or card construction: fail closed before any late results are fused.
	if err := runCtx.Err(); err != nil {
		r.recordOutcome(started, domain.KnowledgeRetrievalOutcomeUnavailable)
		return domain.KnowledgeRetrievalResult{}, fmt.Errorf("%w: retrieval deadline expired: %w", port.ErrKnowledgeUnavailable, err)
	}

	// Rank contributions: exact tier 0, relation tier 1, verified lexical
	// and semantic hits tier 2 with their one-based channel ranks.
	// Deduplication happens per channel only: the ranker merges
	// cross-channel contributions by (kind, id) and fuses the complete
	// reason set, so an identity matched by exact and lexical keeps both
	// reasons with the exact tier.
	verifiedLexical := make([]port.KnowledgeIndexHit, 0, len(lexicalCandidates))
	seenLexical := make(map[string]bool)
	for _, hit := range lexicalCandidates {
		key := string(hit.Kind) + "\x00" + hit.ID
		if seenLexical[key] {
			continue
		}
		seenLexical[key] = true
		if err := r.verifyIndexHit(runCtx, request, limits, hit); err != nil {
			if errors.Is(err, errStaleIndexHit) {
				// Stale hits are excluded and enqueued for repair; the
				// repair failure is bounded best-effort and never leaks.
				r.recordStaleHit(domain.KnowledgeRetrievalChannelLexical)
				if _, enqueueErr := r.queue.Enqueue(runCtx, hit.Kind, hit.ID); enqueueErr != nil {
					_ = enqueueErr
				}
				continue
			}
			if errors.Is(err, port.ErrKnowledgeUnavailable) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return domain.KnowledgeRetrievalResult{}, wrapRetrievalChannelFailure("authoritative lexical re-read", err, runCtx)
			}
			// A hit that no longer exists is treated as stale: the worker
			// removes the orphan index rows.
			if _, enqueueErr := r.queue.Enqueue(runCtx, hit.Kind, hit.ID); enqueueErr != nil {
				_ = enqueueErr
			}
			continue
		}
		verifiedLexical = append(verifiedLexical, hit)
	}
	verifiedSemantic := make([]port.KnowledgeIndexHit, 0, len(semanticCandidates))
	seenSemantic := make(map[string]bool)
	for _, hit := range semanticCandidates {
		key := string(hit.Kind) + "\x00" + hit.ID
		if seenSemantic[key] {
			continue
		}
		seenSemantic[key] = true
		if err := r.verifyIndexHit(runCtx, request, limits, hit); err != nil {
			if errors.Is(err, errStaleIndexHit) {
				r.recordStaleHit(domain.KnowledgeRetrievalChannelSemantic)
				if _, enqueueErr := r.queue.Enqueue(runCtx, hit.Kind, hit.ID); enqueueErr != nil {
					_ = enqueueErr
				}
				continue
			}
			if errors.Is(err, port.ErrKnowledgeUnavailable) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return domain.KnowledgeRetrievalResult{}, wrapRetrievalChannelFailure("authoritative semantic re-read", err, runCtx)
			}
			if _, enqueueErr := r.queue.Enqueue(runCtx, hit.Kind, hit.ID); enqueueErr != nil {
				_ = enqueueErr
			}
			continue
		}
		verifiedSemantic = append(verifiedSemantic, hit)
	}
	contributions := buildRankContributions(exactCandidates, relationCandidates, verifiedLexical, verifiedSemantic, query)

	ranked, err := domain.RankKnowledgeCandidates(contributions)
	if err != nil {
		r.recordOutcome(started, domain.KnowledgeRetrievalOutcomeUnavailable)
		return domain.KnowledgeRetrievalResult{}, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	ranked = domain.CapRankedKnowledgeCandidates(ranked, limits.MaxCards)

	var omissions []domain.KnowledgeRetrievalOmission
	omittedTotal := 0
	cards := make([]domain.KnowledgeFrameCard, 0, len(ranked))
	for _, candidate := range ranked {
		item, err := r.reader.ReadItem(runCtx, request.Binding, request.Now, limits, candidate.Kind, candidate.ID)
		if err != nil {
			r.recordOutcome(started, domain.KnowledgeRetrievalOutcomeUnavailable)
			return domain.KnowledgeRetrievalResult{}, wrapRetrievalChannelFailure("authoritative card re-read", err, runCtx)
		}
		card, omit, err := r.buildFrameCard(runCtx, request, limits, item, candidate.Reasons)
		if err != nil {
			r.recordOutcome(started, domain.KnowledgeRetrievalOutcomeUnavailable)
			return domain.KnowledgeRetrievalResult{}, wrapRetrievalChannelFailure("card construction", err, runCtx)
		}
		if omit != "" {
			omissions = append(omissions, omit)
			omittedTotal++
			continue
		}
		if card == nil {
			continue
		}
		// Every card is validated before it is selected: a payload that
		// cannot be represented (for example a document whose verified
		// content redacts to empty) is omitted whole and its document is
		// enqueued for index cleanup. Invalid cards never reach the model.
		if err := card.Validate(); err != nil {
			if card.Kind == domain.KnowledgeRetrievalDocument {
				if _, enqueueErr := r.queue.Enqueue(runCtx, card.Kind, card.Document.ID); enqueueErr != nil {
					_ = enqueueErr
				}
			}
			omittedTotal++
			continue
		}
		cards = append(cards, *card)
	}

	sortedFailures := sortedFailureValues(failures)
	identities := make([]string, 0, len(cards))
	for _, card := range cards {
		identities = append(identities, card.Identity())
	}
	sort.Strings(identities)
	diagnostics := domain.KnowledgeRetrievalDiagnostics{
		RankingPolicy:      domain.KnowledgeRankingPolicyID,
		EnabledChannels:    r.enabledRetrievalChannels(),
		SelectedIdentities: identities,
		CandidateCount:     len(exactCandidates) + len(relationCandidates) + len(lexicalCandidates) + len(semanticCandidates),
		SelectedCount:      len(cards),
		OmittedCount:       omittedTotal,
		Failures:           sortedFailures,
		Omissions:          sortedOmissions(omissions),
		Elapsed:            r.clock.Now().Sub(started),
	}
	if err := domain.ValidateKnowledgeRetrievalDiagnostics(diagnostics); err != nil {
		r.recordOutcome(started, domain.KnowledgeRetrievalOutcomeUnavailable)
		return domain.KnowledgeRetrievalResult{}, fmt.Errorf("%w: %v", port.ErrKnowledgeUnavailable, err)
	}
	// Cards produced after the retrieval deadline or cancellation are never
	// returned: a success that raced the deadline fails closed instead of
	// admitting late results.
	if err := runCtx.Err(); err != nil {
		r.recordOutcome(started, domain.KnowledgeRetrievalOutcomeUnavailable)
		return domain.KnowledgeRetrievalResult{}, fmt.Errorf("%w: retrieval deadline expired: %w", port.ErrKnowledgeUnavailable, err)
	}
	if len(cards) == 0 {
		r.recordOutcome(started, domain.KnowledgeRetrievalOutcomeEmpty)
		if r.metrics != nil {
			r.metrics.AddCounter(domain.MetricKnowledgeRetrievalEmptyTotal, 1, port.MetricLabels{domain.MetricLabelOutcome: string(domain.KnowledgeRetrievalOutcomeEmpty)})
		}
	} else {
		r.recordOutcome(started, domain.KnowledgeRetrievalOutcomeSuccess)
	}
	return domain.KnowledgeRetrievalResult{Cards: cards, Diagnostics: diagnostics}, nil
}

// errStaleIndexHit marks an index hit whose authoritative item no longer
// matches its revision or source digest.
var errStaleIndexHit = errors.New("stale index hit")

// wrapRetrievalChannelFailure keeps the FIND-094 cancellation contract
// observable on the real SQL path: when a channel fails because the
// retrieval run context expired or was cancelled, the sentinel context
// error is attached with %w so callers can classify the failure with
// errors.Is exactly like the fake-backed unit contract. Failures without
// a done run context keep the original error text.
func wrapRetrievalChannelFailure(message string, err error, runCtx context.Context) error {
	if ctxErr := runCtx.Err(); ctxErr != nil {
		return fmt.Errorf("%w: %s: %w", port.ErrKnowledgeUnavailable, message, ctxErr)
	}
	return fmt.Errorf("%w: %s: %v", port.ErrKnowledgeUnavailable, message, err)
}

// recordEmbeddingRequest observes the frozen embedding request duration
// metric with the closed outcome label.
func (r *Retriever) recordEmbeddingRequest(started time.Time, outcome domain.KnowledgeRetrievalOutcome) {
	if r == nil || r.metrics == nil {
		return
	}
	r.metrics.Observe(domain.MetricKnowledgeEmbeddingRequestDuration, r.clock.Now().Sub(started).Seconds(), port.MetricLabels{domain.MetricLabelOutcome: string(outcome)})
}

// searchSemantic embeds the redacted bounded query, normalizes the single
// output vector, and searches the fingerprint-bound vector index. Every
// failure here degrades only the semantic channel and never blocks
// exact/relation/lexical retrieval.
func (r *Retriever) searchSemantic(ctx context.Context, scopes []domain.KnowledgeScopeRef, owner, query string, limits domain.KnowledgeRetrievalLimits) ([]port.KnowledgeIndexHit, error) {
	started := r.clock.Now()
	vectors, err := r.provider.Embed(ctx, []string{query})
	if err != nil {
		r.recordEmbeddingRequest(started, domain.KnowledgeRetrievalOutcomeUnavailable)
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vectors) != 1 {
		r.recordEmbeddingRequest(started, domain.KnowledgeRetrievalOutcomeUnavailable)
		return nil, fmt.Errorf("embed query: provider returned %d vectors for one input", len(vectors))
	}
	normalized, err := domain.NormalizeEmbeddingVector(vectors[0], limits.EmbeddingDimensions)
	if err != nil {
		r.recordEmbeddingRequest(started, domain.KnowledgeRetrievalOutcomeUnavailable)
		return nil, fmt.Errorf("normalize query vector: %w", err)
	}
	hits, err := r.index.SearchSemantic(ctx, scopes, owner, normalized, limits.MinSimilarityBasisPoints, limits.MaxCandidatesPerChannel)
	if err != nil {
		r.recordEmbeddingRequest(started, domain.KnowledgeRetrievalOutcomeUnavailable)
		return nil, fmt.Errorf("semantic search: %w", err)
	}
	r.recordEmbeddingRequest(started, domain.KnowledgeRetrievalOutcomeSuccess)
	return hits, nil
}

// buildRankContributions converts channel results into ranker inputs with
// per-channel deduplication only: exact candidates carry exact tier and
// subject/identifier attribution, relation candidates carry the relation
// tier, verified lexical hits carry the fused tier with their one-based
// lexical rank, and verified semantic hits carry the fused tier with their
// one-based semantic rank. The ranker owns cross-channel identity merging
// and the complete sorted reason set, so this function must never suppress
// a contribution because another channel already named the identity.
func buildRankContributions(exactCandidates, relationCandidates []port.KnowledgeEligibleCandidate, verifiedLexical, verifiedSemantic []port.KnowledgeIndexHit, query string) []domain.KnowledgeRankCandidate {
	contributions := make([]domain.KnowledgeRankCandidate, 0, len(exactCandidates)+len(relationCandidates)+len(verifiedLexical)+len(verifiedSemantic))
	seenExact := make(map[string]bool)
	for _, candidate := range exactCandidates {
		key := string(candidate.Kind) + "\x00" + candidate.ID
		if seenExact[key] {
			continue
		}
		seenExact[key] = true
		reason := domain.KnowledgeRetrievalReasonExactIdentifier
		if candidate.Subject == query {
			reason = domain.KnowledgeRetrievalReasonExactSubject
		}
		contributions = append(contributions, domain.KnowledgeRankCandidate{
			Kind: candidate.Kind, ID: candidate.ID,
			Tier: domain.KnowledgeRankTierExact, Reasons: []domain.KnowledgeRetrievalReason{reason},
		})
	}
	seenRelation := make(map[string]bool)
	for _, candidate := range relationCandidates {
		key := string(candidate.Kind) + "\x00" + candidate.ID
		if seenRelation[key] {
			continue
		}
		seenRelation[key] = true
		contributions = append(contributions, domain.KnowledgeRankCandidate{
			Kind: candidate.Kind, ID: candidate.ID,
			Tier: domain.KnowledgeRankTierRelation, Reasons: []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonRelation},
		})
	}
	for _, hit := range verifiedLexical {
		rank := hit.Rank
		if rank <= 0 {
			rank = 1
		}
		contributions = append(contributions, domain.KnowledgeRankCandidate{
			Kind: hit.Kind, ID: hit.ID,
			Tier:         domain.KnowledgeRankTierFused,
			Reasons:      []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonLexical},
			LexicalRank:  rank,
			SemanticRank: 0,
		})
	}
	seenSemantic := make(map[string]bool)
	for _, hit := range verifiedSemantic {
		key := string(hit.Kind) + "\x00" + hit.ID
		if seenSemantic[key] {
			continue
		}
		seenSemantic[key] = true
		rank := hit.Rank
		if rank <= 0 {
			rank = 1
		}
		contributions = append(contributions, domain.KnowledgeRankCandidate{
			Kind: hit.Kind, ID: hit.ID,
			Tier:         domain.KnowledgeRankTierFused,
			Reasons:      []domain.KnowledgeRetrievalReason{domain.KnowledgeRetrievalReasonSemantic},
			LexicalRank:  0,
			SemanticRank: rank,
		})
	}
	return contributions
}

// verifyIndexHit re-reads the authoritative item, validates the union,
// reconstructs the canonical index text and digest, and rejects hits whose
// revision or source digest no longer matches. It serves both the lexical
// and semantic channels identically.
func (r *Retriever) verifyIndexHit(ctx context.Context, request domain.KnowledgeRetrievalRequest, limits domain.KnowledgeRetrievalLimits, hit port.KnowledgeIndexHit) error {
	item, err := r.reader.ReadItem(ctx, request.Binding, request.Now, limits, hit.Kind, hit.ID)
	if err != nil {
		return err
	}
	if err := item.Validate(); err != nil {
		return fmt.Errorf("%w: %v", port.ErrKnowledgeUnavailable, err)
	}
	var content string
	if item.Document != nil {
		resolved, resolveErr := r.resolver.Resolve(ctx, *item.Document, limits)
		if resolveErr != nil {
			// An unverifiable document hit is stale by definition: the
			// index can no longer be reconstructed from verified content.
			return fmt.Errorf("%w: %v", errStaleIndexHit, resolveErr)
		}
		content = string(resolved)
	}
	text, err := BuildKnowledgeIndexText(item.Kind, item, content, r.redact)
	if err != nil {
		return fmt.Errorf("%w: %v", port.ErrKnowledgeUnavailable, err)
	}
	revision := 0
	switch item.Kind {
	case domain.KnowledgeRetrievalClaim:
		revision = item.Claim.Revision
	case domain.KnowledgeRetrievalPreference:
		revision = item.Preference.Revision
	case domain.KnowledgeRetrievalDocument:
		revision = item.Document.Revision
	}
	if revision != hit.Revision || text.SourceDigest != hit.SourceDigest {
		return fmt.Errorf("%w: index revision or digest mismatch", errStaleIndexHit)
	}
	return nil
}

// primaryCardReason selects the one deterministic card-attribution reason
// from the merged reason set by the frozen precedence.
func primaryCardReason(reasons []domain.KnowledgeRetrievalReason) domain.KnowledgeRetrievalReason {
	for _, candidate := range []domain.KnowledgeRetrievalReason{
		domain.KnowledgeRetrievalReasonExactSubject,
		domain.KnowledgeRetrievalReasonExactIdentifier,
		domain.KnowledgeRetrievalReasonRelation,
		domain.KnowledgeRetrievalReasonLexical,
		domain.KnowledgeRetrievalReasonSemantic,
	} {
		for _, reason := range reasons {
			if reason == candidate {
				return reason
			}
		}
	}
	return domain.KnowledgeRetrievalReasonLexical
}

// buildFrameCard constructs one complete frame card from an authoritative
// item. Document resolution failures exclude the card and enqueue cleanup
// without falling back; an omission category is returned for oversized
// documents.
func (r *Retriever) buildFrameCard(ctx context.Context, request domain.KnowledgeRetrievalRequest, limits domain.KnowledgeRetrievalLimits, item port.KnowledgeAuthoritativeItem, reasons []domain.KnowledgeRetrievalReason) (*domain.KnowledgeFrameCard, domain.KnowledgeRetrievalOmission, error) {
	reason := string(primaryCardReason(reasons))
	switch item.Kind {
	case domain.KnowledgeRetrievalClaim:
		card := domain.CardFromClaim(*item.Claim, reason, request.Now)
		return &domain.KnowledgeFrameCard{Kind: domain.KnowledgeRetrievalClaim, Claim: &card}, "", nil
	case domain.KnowledgeRetrievalPreference:
		preference := item.Preference
		card := domain.KnowledgePreferenceCard{
			ID:              "preference:" + fmt.Sprintf("%d", preference.ID),
			OwnerKey:        preference.OwnerKey,
			Key:             preference.Key,
			Value:           preference.Value,
			Status:          preference.Status,
			SourceRef:       preference.SourceRef,
			Revision:        preference.Revision,
			RetrievalReason: reason,
		}
		return &domain.KnowledgeFrameCard{Kind: domain.KnowledgeRetrievalPreference, Preference: &card}, "", nil
	case domain.KnowledgeRetrievalDocument:
		document := item.Document
		content, err := r.resolver.Resolve(ctx, *document, limits)
		if err != nil {
			// Missing, malformed, oversized, or digest-mismatched documents
			// are excluded and cleaned up; they never fall back to mutable
			// content.
			_, _ = r.queue.Enqueue(ctx, domain.KnowledgeRetrievalDocument, string(document.ID))
			return nil, "", nil //nolint:nilerr // documents are excluded on failure, not treated as request errors
		}
		redacted := content
		if r.redact != nil {
			redacted = []byte(r.redact(string(content)))
		}
		card := domain.KnowledgeDocumentCard{
			ID:              string(document.ID),
			Subject:         document.Subject,
			ScopeKind:       document.ScopeKind,
			ScopeID:         document.ScopeID,
			Provenance:      document.Provenance,
			Status:          document.Status,
			SourceID:        document.SourceID,
			SourceRev:       document.SourceRev,
			Content:         string(redacted),
			RetrievalReason: reason,
		}
		if card.ExceedsDocumentBytes(limits.MaxDocumentBytes) {
			return nil, domain.KnowledgeRetrievalOmissionDocumentOverLimit, nil
		}
		return &domain.KnowledgeFrameCard{Kind: domain.KnowledgeRetrievalDocument, Document: &card}, "", nil
	default:
		return nil, "", fmt.Errorf("unknown authoritative item kind %q", item.Kind)
	}
}

// enabledRetrievalChannels reports the closed sorted channel set. The
// semantic channel is reported only when an embedding provider is wired;
// without one the feature stays default-off.
func (r *Retriever) enabledRetrievalChannels() []domain.KnowledgeRetrievalChannel {
	channels := []domain.KnowledgeRetrievalChannel{
		domain.KnowledgeRetrievalChannelExact,
		domain.KnowledgeRetrievalChannelLexical,
		domain.KnowledgeRetrievalChannelRelation,
	}
	if r != nil && r.provider != nil {
		channels = append(channels, domain.KnowledgeRetrievalChannelSemantic)
	}
	return channels
}

func sortedFailureValues(failures []domain.KnowledgeRetrievalFailure) []domain.KnowledgeRetrievalFailure {
	values := make([]string, 0, len(failures))
	for _, failure := range failures {
		values = append(values, string(failure))
	}
	sort.Strings(values)
	result := make([]domain.KnowledgeRetrievalFailure, 0, len(values))
	for _, value := range values {
		result = append(result, domain.KnowledgeRetrievalFailure(value))
	}
	return result
}

func sortedOmissions(omissions []domain.KnowledgeRetrievalOmission) []domain.KnowledgeRetrievalOmission {
	values := make([]string, 0, len(omissions))
	for _, omission := range omissions {
		values = append(values, string(omission))
	}
	sort.Strings(values)
	result := make([]domain.KnowledgeRetrievalOmission, 0, len(values))
	for _, value := range values {
		result = append(result, domain.KnowledgeRetrievalOmission(value))
	}
	return result
}
