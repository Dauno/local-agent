package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.KnowledgeCandidateReader = (*KnowledgeCandidateReader)(nil)

// KnowledgeCandidateReader is the authoritative channel-shaped SQLite
// candidate surface. Every read receives the closed readable scopes and the
// trusted owner key derived from the binding; scope-or-owner, status, and
// validity predicates execute inside SQL before any row leaves the adapter,
// so an unreadable row is indistinguishable from a missing row and the
// adapter never scans the authorized catalog in Go.
type KnowledgeCandidateReader struct {
	db *sql.DB
}

func NewKnowledgeCandidateReader(store *Store) *KnowledgeCandidateReader {
	if store == nil || store.db == nil {
		return nil
	}
	return &KnowledgeCandidateReader{db: store.db}
}

const (
	knowledgeEligibleClaimStatuses = "('asserted', 'verified', 'disputed')"
)

// validateRetrievalBinding enforces the trusted read binding before any
// query: a plausible team and actor and a canonical conversation key whose
// team matches the binding. Retrieval never invokes write-policy methods.
func validateRetrievalBinding(binding domain.KnowledgeWriteBinding) error {
	if !domain.PlausibleTeamID(binding.Team) {
		return fmt.Errorf("%w: retrieval binding requires a plausible trusted team", port.ErrKnowledgeValidation)
	}
	if !domain.PlausibleUserID(binding.Actor) {
		return fmt.Errorf("%w: retrieval binding requires a plausible trusted actor", port.ErrKnowledgeValidation)
	}
	if !domain.ValidKnowledgeConversationKey(binding.Conversation) {
		return fmt.Errorf("%w: retrieval binding requires a canonical conversation key", port.ErrKnowledgeValidation)
	}
	parts := strings.Split(string(binding.Conversation), ":")
	if len(parts) < 4 || parts[1] != binding.Team {
		return fmt.Errorf("%w: retrieval conversation does not belong to the binding team", port.ErrKnowledgeValidation)
	}
	return nil
}

// exactMatchParams classifies the query and extracted technical tokens into
// byte-exact text values, canonical float values, and boolean values. Text
// values match string and reference columns byte-exactly; numbers match the
// value_number column through Go's canonical float representation; booleans
// match value_boolean.
type exactMatchParams struct {
	texts    []string
	numbers  []float64
	booleans []int
}

func classifyExactMatches(values []string) exactMatchParams {
	params := exactMatchParams{}
	seenText := make(map[string]bool)
	seenNumber := make(map[float64]bool)
	seenBoolean := make(map[int]bool)
	for _, value := range values {
		if value == "" {
			continue
		}
		if !seenText[value] {
			params.texts = append(params.texts, value)
			seenText[value] = true
		}
		if number, err := strconv.ParseFloat(value, 64); err == nil {
			if !seenNumber[number] {
				params.numbers = append(params.numbers, number)
				seenNumber[number] = true
			}
			continue
		}
		switch value {
		case "true":
			if !seenBoolean[1] {
				params.booleans = append(params.booleans, 1)
				seenBoolean[1] = true
			}
		case "false":
			if !seenBoolean[0] {
				params.booleans = append(params.booleans, 0)
				seenBoolean[0] = true
			}
		}
	}
	return params
}

// matchClaimColumns renders the byte-exact claim match predicate over
// subject, string value, reference value, canonical number value, and
// boolean value. Technical tokens keep byte-exact comparison; nothing is
// normalized inside SQL.
func matchClaimColumns(params exactMatchParams, args *[]any) string {
	var clauses []string
	if len(params.texts) > 0 {
		placeholders := placeholders(len(params.texts))
		clauses = append(clauses,
			"subject IN ("+placeholders+")",
			"(value_kind = 'string' AND value_text IN ("+placeholders+"))",
			"(value_kind = 'reference' AND value_reference IN ("+placeholders+"))")
		appendStrings(args, params.texts)
		appendStrings(args, params.texts)
		appendStrings(args, params.texts)
	}
	if len(params.numbers) > 0 {
		clauses = append(clauses, "(value_kind = 'number' AND value_number IN ("+placeholders(len(params.numbers))+"))")
		for _, number := range params.numbers {
			*args = append(*args, number)
		}
	}
	if len(params.booleans) > 0 {
		clauses = append(clauses, "(value_kind = 'boolean' AND value_boolean IN ("+placeholders(len(params.booleans))+"))")
		for _, boolean := range params.booleans {
			*args = append(*args, boolean)
		}
	}
	if len(clauses) == 0 {
		return "0 = 1"
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}

// matchPreferenceColumns renders the byte-exact preference match predicate
// over key and scalar value.
func matchPreferenceColumns(params exactMatchParams, args *[]any) string {
	var clauses []string
	if len(params.texts) > 0 {
		placeholders := placeholders(len(params.texts))
		clauses = append(clauses,
			"key IN ("+placeholders+")",
			"(value_kind = 'string' AND value_text IN ("+placeholders+"))")
		appendStrings(args, params.texts)
		appendStrings(args, params.texts)
	}
	if len(params.numbers) > 0 {
		clauses = append(clauses, "(value_kind = 'number' AND value_number IN ("+placeholders(len(params.numbers))+"))")
		for _, number := range params.numbers {
			*args = append(*args, number)
		}
	}
	if len(params.booleans) > 0 {
		clauses = append(clauses, "(value_kind = 'boolean' AND value_boolean IN ("+placeholders(len(params.booleans))+"))")
		for _, boolean := range params.booleans {
			*args = append(*args, boolean)
		}
	}
	if len(clauses) == 0 {
		return "0 = 1"
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func appendStrings(target *[]any, values []string) {
	for _, value := range values {
		*target = append(*target, value)
	}
}

// ReadExact matches the full normalized query and the extracted technical
// tokens against claim subject/value/reference, preference key/value, and
// document subject inside SQL with bound parameters, bounded by
// max_candidates_per_channel after authorization and stable kind-then-
// identity ordering.
func (r *KnowledgeCandidateReader) ReadExact(
	ctx context.Context,
	binding domain.KnowledgeWriteBinding,
	now time.Time,
	limits domain.KnowledgeRetrievalLimits,
	query string,
	tokens []string,
) ([]port.KnowledgeEligibleCandidate, error) {
	if err := r.validateRead(binding, now, limits); err != nil {
		return nil, err
	}
	if len(tokens) > domain.HardMaxKnowledgeRetrievalRank {
		return nil, fmt.Errorf("%w: exact match token count is not bounded", port.ErrKnowledgeValidation)
	}
	for _, token := range tokens {
		if token == "" || utf8.RuneCountInString(token) > 128 {
			return nil, fmt.Errorf("%w: exact match token is not bounded", port.ErrKnowledgeValidation)
		}
	}
	if strings.TrimSpace(query) == "" && len(tokens) == 0 {
		return nil, nil
	}
	scopes := domain.KnowledgeReadableScopes(binding)
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)
	scopeFilter, scopeArgs := knowledgeScopeFilter(scopes)
	params := classifyExactMatches(append([]string{query}, tokens...))

	var claimArgs, preferenceArgs []any
	claimClause := matchClaimColumns(params, &claimArgs)
	preferenceClause := matchPreferenceColumns(params, &preferenceArgs)
	documentClause := "0 = 1"
	if len(params.texts) > 0 {
		documentClause = "subject IN (" + placeholders(len(params.texts)) + ")"
	}

	nowUnix := now.Unix()
	sqlText := `
		SELECT kind, id, subject, scope_kind, scope_id, revision FROM (
			SELECT 'claim' AS kind, id, subject, scope_kind, scope_id, current_rev AS revision
			FROM knowledge_claims
			WHERE (` + scopeFilter + `)
				AND status IN ` + knowledgeEligibleClaimStatuses + `
				AND (valid_from = 0 OR valid_from <= ?) AND (valid_until = 0 OR valid_until > ?)
				AND ` + claimClause + `
			UNION ALL
			SELECT 'preference', 'preference:' || id, key, 'user', owner_key, current_rev
			FROM knowledge_preferences
			WHERE owner_key = ? AND status = 'active' AND ` + preferenceClause + `
			UNION ALL
			SELECT 'document', id, subject, scope_kind, scope_id, current_rev
			FROM knowledge_documents
			WHERE (` + scopeFilter + `) AND status = 'active' AND ` + documentClause + `
		)
		ORDER BY kind, id
		LIMIT ?`

	args := make([]any, 0, 2*len(scopeArgs)+2+len(claimArgs)+1+len(preferenceArgs)+len(params.texts)+1)
	args = append(args, scopeArgs...)
	args = append(args, nowUnix, nowUnix)
	args = append(args, claimArgs...)
	args = append(args, owner)
	args = append(args, preferenceArgs...)
	args = append(args, scopeArgs...)
	if len(params.texts) > 0 {
		appendStrings(&args, params.texts)
	}
	args = append(args, limits.MaxCandidatesPerChannel)

	return scanEligibleCandidates(r.db.QueryContext(ctx, sqlText, args...))
}

// ReadRelated expands one hop from the authorized exact claim seeds: only
// owns/relates_to edges are followed, endpoints are the seeds' (subject,
// value_reference) pairs, and result claims pass scope, status, and
// validity predicates again in SQL. There is no recursion, alias inference,
// cross-scope expansion, or graph table.
func (r *KnowledgeCandidateReader) ReadRelated(
	ctx context.Context,
	binding domain.KnowledgeWriteBinding,
	now time.Time,
	limits domain.KnowledgeRetrievalLimits,
	seeds []port.KnowledgeEligibleCandidate,
) ([]port.KnowledgeEligibleCandidate, error) {
	if err := r.validateRead(binding, now, limits); err != nil {
		return nil, err
	}
	if len(seeds) > limits.MaxCandidatesPerChannel {
		return nil, fmt.Errorf("%w: relation seed count exceeds the channel cap", port.ErrKnowledgeValidation)
	}
	claimIDs := make([]string, 0, len(seeds))
	seenSeed := make(map[string]bool)
	for _, seed := range seeds {
		if !domain.ValidKnowledgeRetrievalItemKind(seed.Kind) {
			return nil, fmt.Errorf("%w: relation seed kind is not closed", port.ErrKnowledgeValidation)
		}
		if seed.Kind != domain.KnowledgeRetrievalClaim {
			continue
		}
		if seed.ID == "" || utf8.RuneCountInString(seed.ID) > domain.HardMaxKnowledgeQueueItemIDRunes {
			return nil, fmt.Errorf("%w: relation seed identity is not bounded", port.ErrKnowledgeValidation)
		}
		if !seenSeed[seed.ID] {
			seenSeed[seed.ID] = true
			claimIDs = append(claimIDs, seed.ID)
		}
	}
	if len(claimIDs) == 0 {
		return nil, nil
	}

	// The seeds are re-verified in SQL: an edge endpoint is read only from
	// seed claims that still pass scope, status, and validity under the
	// current binding, so a stale or cross-scope seed can never expand a
	// relation hop on behalf of the caller.
	scopes := domain.KnowledgeReadableScopes(binding)
	scopeFilter, scopeArgs := knowledgeScopeFilter(scopes)
	nowUnix := now.Unix()
	endpointArgs := make([]any, 0, len(claimIDs)+len(scopeArgs)+2)
	appendStrings(&endpointArgs, claimIDs)
	endpointArgs = append(endpointArgs, scopeArgs...)
	endpointArgs = append(endpointArgs, nowUnix, nowUnix)
	rows, err := r.db.QueryContext(ctx, `
		SELECT subject, value_reference FROM knowledge_claims
		WHERE id IN (`+placeholders(len(claimIDs))+`)
			AND (`+scopeFilter+`)
			AND status IN `+knowledgeEligibleClaimStatuses+`
			AND (valid_from = 0 OR valid_from <= ?) AND (valid_until = 0 OR valid_until > ?)
			AND predicate IN ('owns', 'relates_to')`, endpointArgs...)
	if err != nil {
		return nil, fmt.Errorf("%w: relation endpoints: %v", port.ErrKnowledgeUnavailable, err)
	}
	endpoints := make(map[string]bool)
	if err := collectRows(rows, func(scan func(dest ...any) error) error {
		var subject, reference string
		if err := scan(&subject, &reference); err != nil {
			return err
		}
		endpoints[subject] = true
		if reference != "" {
			endpoints[reference] = true
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("%w: relation endpoints: %v", port.ErrKnowledgeUnavailable, err)
	}
	if len(endpoints) == 0 {
		return nil, nil
	}
	endpointList := make([]string, 0, len(endpoints))
	for endpoint := range endpoints {
		endpointList = append(endpointList, endpoint)
	}
	sortEndpointValues(endpointList)

	args := make([]any, 0, len(endpointList)*2+len(claimIDs)+len(scopeArgs)+3)
	args = append(args, scopeArgs...)
	args = append(args, nowUnix, nowUnix)
	appendStrings(&args, claimIDs)
	appendStrings(&args, endpointList)
	appendStrings(&args, endpointList)
	args = append(args, limits.MaxCandidatesPerChannel)

	sqlText := `
		SELECT 'claim' AS kind, id, subject, scope_kind, scope_id, current_rev AS revision
		FROM knowledge_claims
		WHERE (` + scopeFilter + `)
			AND status IN ` + knowledgeEligibleClaimStatuses + `
			AND (valid_from = 0 OR valid_from <= ?) AND (valid_until = 0 OR valid_until > ?)
			AND id NOT IN (` + placeholders(len(claimIDs)) + `)
			AND (subject IN (` + placeholders(len(endpointList)) + `)
				OR (value_kind = 'reference' AND value_reference IN (` + placeholders(len(endpointList)) + `)))
		ORDER BY id
		LIMIT ?`

	candidates, err := scanEligibleCandidates(r.db.QueryContext(ctx, sqlText, args...))
	if err != nil {
		return nil, fmt.Errorf("%w: relation candidates: %v", port.ErrKnowledgeUnavailable, err)
	}
	return candidates, nil
}

// ReadItem re-reads the complete authorized row for card construction after
// a channel or index hit. Scope-or-owner, status, and validity predicates
// apply again in SQL; an unreadable row is indistinguishable from a missing
// one.
func (r *KnowledgeCandidateReader) ReadItem(
	ctx context.Context,
	binding domain.KnowledgeWriteBinding,
	now time.Time,
	limits domain.KnowledgeRetrievalLimits,
	kind domain.KnowledgeRetrievalItemKind,
	id string,
) (port.KnowledgeAuthoritativeItem, error) {
	if err := r.validateRead(binding, now, limits); err != nil {
		return port.KnowledgeAuthoritativeItem{}, err
	}
	if !domain.ValidKnowledgeRetrievalItemKind(kind) {
		return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: unknown candidate kind", port.ErrKnowledgeValidation)
	}
	if id == "" || utf8.RuneCountInString(id) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: candidate identity is not bounded", port.ErrKnowledgeValidation)
	}
	scopes := domain.KnowledgeReadableScopes(binding)
	nowUnix := now.Unix()
	switch kind {
	case domain.KnowledgeRetrievalClaim:
		scopeFilter, scopeArgs := knowledgeScopeFilter(scopes)
		args := append([]any{id}, scopeArgs...)
		args = append(args, nowUnix, nowUnix)
		claim, err := scanKnowledgeClaim(r.db.QueryRowContext(ctx, `
			SELECT `+knowledgeClaimColumns+` FROM knowledge_claims
			WHERE id = ? AND (`+scopeFilter+`)
				AND status IN `+knowledgeEligibleClaimStatuses+`
				AND (valid_from = 0 OR valid_from <= ?) AND (valid_until = 0 OR valid_until > ?)`, args...))
		if errors.Is(err, sql.ErrNoRows) {
			return port.KnowledgeAuthoritativeItem{}, port.ErrKnowledgeNotFound
		}
		if err != nil {
			return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: authoritative claim read: %v", port.ErrKnowledgeUnavailable, err)
		}
		return port.KnowledgeAuthoritativeItem{Kind: kind, ID: id, Claim: &claim}, nil
	case domain.KnowledgeRetrievalPreference:
		rowID, ok := parseStrictPositiveDecimal(strings.TrimPrefix(id, "preference:"))
		if !ok || !strings.HasPrefix(id, "preference:") {
			return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: preference identity is not canonical", port.ErrKnowledgeValidation)
		}
		owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)
		preference, err := scanKnowledgePreference(r.db.QueryRowContext(ctx, `
			SELECT `+knowledgePreferenceColumns+` FROM knowledge_preferences
			WHERE id = ? AND owner_key = ? AND status = 'active'`, rowID, owner))
		if errors.Is(err, sql.ErrNoRows) {
			return port.KnowledgeAuthoritativeItem{}, port.ErrKnowledgeNotFound
		}
		if err != nil {
			return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: authoritative preference read: %v", port.ErrKnowledgeUnavailable, err)
		}
		return port.KnowledgeAuthoritativeItem{Kind: kind, ID: id, Preference: &preference}, nil
	case domain.KnowledgeRetrievalDocument:
		scopeFilter, scopeArgs := knowledgeScopeFilter(scopes)
		args := append([]any{id}, scopeArgs...)
		document, err := scanKnowledgeDocument(r.db.QueryRowContext(ctx, `
			SELECT `+knowledgeDocumentColumns+` FROM knowledge_documents
			WHERE id = ? AND (`+scopeFilter+`) AND status = 'active'`, args...))
		if errors.Is(err, sql.ErrNoRows) {
			return port.KnowledgeAuthoritativeItem{}, port.ErrKnowledgeNotFound
		}
		if err != nil {
			return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: authoritative document read: %v", port.ErrKnowledgeUnavailable, err)
		}
		return port.KnowledgeAuthoritativeItem{Kind: kind, ID: id, Document: &document}, nil
	default:
		return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: unknown candidate kind", port.ErrKnowledgeValidation)
	}
}

func (r *KnowledgeCandidateReader) validateRead(binding domain.KnowledgeWriteBinding, now time.Time, limits domain.KnowledgeRetrievalLimits) error {
	if err := validateRetrievalBinding(binding); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("%w: candidate read requires an injected clock", port.ErrKnowledgeValidation)
	}
	if err := limits.Validate(); err != nil {
		return fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	return nil
}

func scanEligibleCandidates(rows *sql.Rows, err error) ([]port.KnowledgeEligibleCandidate, error) {
	if err != nil {
		return nil, fmt.Errorf("%w: eligible candidate scan: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = rows.Close() }()
	var candidates []port.KnowledgeEligibleCandidate
	for rows.Next() {
		var candidate port.KnowledgeEligibleCandidate
		if err := rows.Scan(&candidate.Kind, &candidate.ID, &candidate.Subject, &candidate.ScopeKind, &candidate.ScopeID, &candidate.Revision); err != nil {
			return nil, fmt.Errorf("%w: eligible candidate scan: %v", port.ErrKnowledgeUnavailable, err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: eligible candidate scan: %v", port.ErrKnowledgeUnavailable, err)
	}
	return candidates, nil
}

func collectRows(rows *sql.Rows, consume func(scan func(dest ...any) error) error) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := consume(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

func sortEndpointValues(values []string) {
	sort.Strings(values)
}
