package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.KnowledgeIndex = (*KnowledgeLexicalIndexStore)(nil)
var _ port.KnowledgeLexicalIndex = (*KnowledgeLexicalIndexStore)(nil)

// KnowledgeLexicalIndexStore implements the reconstructible lexical FTS5
// surface and the fingerprint-bound linear semantic surface: worker-owned
// row replacement/deletion/pagination, the scope-first BM25 search used by
// the retrieval pipeline, and the linear dot-product semantic search over
// knowledge_embeddings rows joined to currently authorized, eligible,
// matching-revision truth. Both searches return only kind, identity, rank,
// revision, and source-digest metadata; the authoritative row is always
// re-read before card construction. Index rows are never authoritative on
// their own. An empty bound fingerprint leaves semantic search disabled.
type KnowledgeLexicalIndexStore struct {
	db          *sql.DB
	fingerprint string
}

func NewKnowledgeLexicalIndexStore(store *Store, fingerprint string) *KnowledgeLexicalIndexStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &KnowledgeLexicalIndexStore{db: store.db, fingerprint: fingerprint}
}

// SearchLexical builds an FTS5 MATCH expression exclusively from quoted
// normalized terms joined with OR, authorizes every hit kind inside SQL
// against the trusted scopes and owner key, and orders by BM25 rank with
// kind and identity as stable tie-breaks. Queries with no terms return an
// empty hit set without touching the index.
func (s *KnowledgeLexicalIndexStore) SearchLexical(ctx context.Context, scopes []domain.KnowledgeScopeRef, ownerKey, query string, limit int) ([]port.KnowledgeIndexHit, error) {
	if err := validateLexicalScopes(scopes, ownerKey); err != nil {
		return nil, err
	}
	if limit < 1 || limit > domain.HardMaxKnowledgeRetrievalMaxCandidatesPerChannel {
		return nil, fmt.Errorf("%w: lexical search limit is not bounded", port.ErrKnowledgeValidation)
	}
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return nil, nil
	}
	match := buildLexicalMatch(terms)

	scopeFilter, scopeArgs := knowledgeScopeFilter(scopes)

	args := make([]any, 0, len(scopeArgs)*2+4)
	args = append(args, match)
	args = append(args, scopeArgs...)
	args = append(args, ownerKey)
	args = append(args, scopeArgs...)
	args = append(args, limit)

	// Claim validity is authorized inside SQL against the index clock; the
	// authoritative re-read then applies the injected retrieval clock
	// again, so a hit that has expired between index time and read time is
	// excluded as stale.
	sqlText := `
		SELECT item_kind, item_id, item_revision, source_digest
		FROM knowledge_retrieval_fts AS f
		WHERE knowledge_retrieval_fts MATCH ?
			AND (
				(f.item_kind = 'claim' AND EXISTS (
					SELECT 1 FROM knowledge_claims c
					WHERE c.id = f.item_id
						AND (` + scopeFilter + `)
						AND c.status IN ('asserted', 'verified', 'disputed')
						AND (c.valid_from = 0 OR c.valid_from <= strftime('%s', 'now'))
						AND (c.valid_until = 0 OR c.valid_until > strftime('%s', 'now'))
				))
				OR (f.item_kind = 'preference' AND EXISTS (
					SELECT 1 FROM knowledge_preferences p
					WHERE p.owner_key = ?
						AND p.status = 'active'
						AND CAST(p.id AS TEXT) = SUBSTR(f.item_id, 12)
				))
				OR (f.item_kind = 'document' AND EXISTS (
					SELECT 1 FROM knowledge_documents d
					WHERE d.id = f.item_id
						AND (` + scopeFilter + `)
						AND d.status = 'active'
				))
			)
		ORDER BY rank, f.item_kind, f.item_id
		LIMIT ?`

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: lexical search: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = rows.Close() }()
	hits := make([]port.KnowledgeIndexHit, 0, limit)
	for rows.Next() {
		var hit port.KnowledgeIndexHit
		if err := rows.Scan(&hit.Kind, &hit.ID, &hit.Revision, &hit.SourceDigest); err != nil {
			return nil, fmt.Errorf("%w: lexical hit scan: %v", port.ErrKnowledgeUnavailable, err)
		}
		hit.Rank = len(hits) + 1
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: lexical hit scan: %v", port.ErrKnowledgeUnavailable, err)
	}
	return hits, nil
}

// SearchSemantic performs the linear dot-product scan the TRD names: one
// query authorizes scope and owner inside SQL exactly like SearchLexical,
// restricts rows to the construction-bound model fingerprint, and requires
// the stored item_revision to equal the truth table's current current_rev.
// Surviving rows are decoded and compared against the already-normalized
// query vector in Go (both unit vectors, so the dot product is cosine
// similarity), converted to basis points, filtered by the configured
// threshold, sorted by similarity descending with kind and identity as
// stable tie-breaks, and capped at the bounded limit. Malformed stored rows
// are skipped defensively and never fail the whole search.
func (s *KnowledgeLexicalIndexStore) SearchSemantic(ctx context.Context, scopes []domain.KnowledgeScopeRef, ownerKey string, vector []float32, minSimilarityBasisPoints, limit int) ([]port.KnowledgeIndexHit, error) {
	if s.fingerprint == "" {
		return nil, fmt.Errorf("%w: semantic search is not configured", port.ErrKnowledgeUnavailable)
	}
	if err := validateLexicalScopes(scopes, ownerKey); err != nil {
		return nil, err
	}
	if limit < 1 || limit > domain.HardMaxKnowledgeRetrievalMaxCandidatesPerChannel {
		return nil, fmt.Errorf("%w: semantic search limit is not bounded", port.ErrKnowledgeValidation)
	}
	if minSimilarityBasisPoints < 0 || minSimilarityBasisPoints > domain.HardMaxKnowledgeMinSimilarityBasisPoints {
		return nil, fmt.Errorf("%w: semantic similarity threshold is not bounded", port.ErrKnowledgeValidation)
	}
	if len(vector) == 0 {
		return nil, fmt.Errorf("%w: semantic search requires a query vector", port.ErrKnowledgeValidation)
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("%w: semantic search query vector must be finite", port.ErrKnowledgeValidation)
		}
	}

	scopeFilter, scopeArgs := knowledgeScopeFilter(scopes)
	args := make([]any, 0, len(scopeArgs)*2+3)
	args = append(args, s.fingerprint)
	args = append(args, scopeArgs...)
	args = append(args, ownerKey)
	args = append(args, scopeArgs...)

	// Authorization mirrors SearchLexical. In addition every row must
	// match the bound fingerprint and its item_revision must equal the
	// truth table's current current_rev: a vector row whose truth advanced
	// is never searched until the embedding worker rebuilds it.
	sqlText := `
		SELECT e.item_kind, e.item_id, e.item_revision, e.source_digest, e.dimensions, e.vector
		FROM knowledge_embeddings AS e
		WHERE e.model_fingerprint = ?
			AND (
				(e.item_kind = 'claim' AND EXISTS (
					SELECT 1 FROM knowledge_claims c
					WHERE c.id = e.item_id
						AND (` + scopeFilter + `)
						AND c.status IN ('asserted', 'verified', 'disputed')
						AND (c.valid_from = 0 OR c.valid_from <= strftime('%s', 'now'))
						AND (c.valid_until = 0 OR c.valid_until > strftime('%s', 'now'))
						AND c.current_rev = e.item_revision
				))
				OR (e.item_kind = 'preference' AND EXISTS (
					SELECT 1 FROM knowledge_preferences p
					WHERE p.owner_key = ?
						AND p.status = 'active'
						AND CAST(p.id AS TEXT) = SUBSTR(e.item_id, 12)
						AND p.current_rev = e.item_revision
				))
				OR (e.item_kind = 'document' AND EXISTS (
					SELECT 1 FROM knowledge_documents d
					WHERE d.id = e.item_id
						AND (` + scopeFilter + `)
						AND d.status = 'active'
						AND d.current_rev = e.item_revision
				))
			)`

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: semantic search: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = rows.Close() }()

	type scoredHit struct {
		hit         port.KnowledgeIndexHit
		basisPoints int64
	}
	scored := make([]scoredHit, 0, limit)
	for rows.Next() {
		var (
			hit         port.KnowledgeIndexHit
			dimensions  int
			vectorBytes []byte
		)
		if err := rows.Scan(&hit.Kind, &hit.ID, &hit.Revision, &hit.SourceDigest, &dimensions, &vectorBytes); err != nil {
			return nil, fmt.Errorf("%w: semantic hit scan: %v", port.ErrKnowledgeUnavailable, err)
		}
		if dimensions < 1 || dimensions != len(vector) || len(vectorBytes) != dimensions*4 {
			// A corrupt row is skipped defensively; one bad row never
			// fails the whole search.
			continue
		}
		stored := make([]float32, dimensions)
		skip := false
		for index := range stored {
			value := math.Float32frombits(binary.LittleEndian.Uint32(vectorBytes[index*4:]))
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				skip = true
				break
			}
			stored[index] = value
		}
		if skip {
			continue
		}
		dot := 0.0
		for index, value := range stored {
			dot += float64(value) * float64(vector[index]) //nolint:gosec // dimensions equals the validated vector length
		}
		points := int64(math.Round(dot * 10000))
		if points < int64(minSimilarityBasisPoints) {
			continue
		}
		scored = append(scored, scoredHit{hit: hit, basisPoints: points})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: semantic hit scan: %v", port.ErrKnowledgeUnavailable, err)
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].basisPoints != scored[j].basisPoints {
			return scored[i].basisPoints > scored[j].basisPoints
		}
		if scored[i].hit.Kind != scored[j].hit.Kind {
			return scored[i].hit.Kind < scored[j].hit.Kind
		}
		return scored[i].hit.ID < scored[j].hit.ID
	})
	hits := make([]port.KnowledgeIndexHit, 0, min(len(scored), limit))
	for index, candidate := range scored {
		if index >= limit {
			break
		}
		candidate.hit.Rank = index + 1
		hits = append(hits, candidate.hit)
	}
	return hits, nil
}

// ReplaceLexical swaps every FTS row of one identity in a single
// transaction: the old rows are deleted and one new row carrying the
// redacted canonical subject and body plus revision and digest metadata is
// inserted.
func (s *KnowledgeLexicalIndexStore) ReplaceLexical(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, id string, revision int, sourceDigest, subject, body string) error {
	if err := validateLexicalMutation(kind, id, revision, sourceDigest); err != nil {
		return err
	}
	if subject == "" && body == "" {
		return fmt.Errorf("%w: lexical index text must not be empty", port.ErrKnowledgeValidation)
	}
	if !utf8.ValidString(subject) || !utf8.ValidString(body) {
		return fmt.Errorf("%w: lexical index text must be valid UTF-8", port.ErrKnowledgeValidation)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: begin lexical replacement: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM knowledge_retrieval_fts WHERE item_kind = ? AND item_id = ?`, kind, id); err != nil {
		return fmt.Errorf("%w: delete lexical rows: %v", port.ErrKnowledgeUnavailable, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_retrieval_fts (item_kind, item_id, item_revision, source_digest, subject, body)
		VALUES (?, ?, ?, ?, ?, ?)`, kind, id, revision, sourceDigest, subject, body); err != nil {
		return fmt.Errorf("%w: insert lexical row: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit lexical replacement: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}

// DeleteLexical removes every FTS row of one identity.
func (s *KnowledgeLexicalIndexStore) DeleteLexical(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, id string) error {
	if !domain.ValidKnowledgeRetrievalItemKind(kind) {
		return fmt.Errorf("%w: unknown lexical index kind", port.ErrKnowledgeValidation)
	}
	if id == "" || utf8.RuneCountInString(id) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return fmt.Errorf("%w: lexical index identity is not bounded", port.ErrKnowledgeValidation)
	}
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM knowledge_retrieval_fts WHERE item_kind = ? AND item_id = ?`, kind, id); err != nil {
		return fmt.Errorf("%w: delete lexical rows: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}

// ListLexical pages lexical index identities in stable identity order for
// reconciliation. Rows carry only metadata, never index text.
func (s *KnowledgeLexicalIndexStore) ListLexical(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, afterID string, limit int) ([]port.KnowledgeLexicalIndexRow, error) {
	if !domain.ValidKnowledgeRetrievalItemKind(kind) {
		return nil, fmt.Errorf("%w: unknown lexical index kind", port.ErrKnowledgeValidation)
	}
	if !domain.ValidKnowledgeQueueListLimit(limit) {
		return nil, fmt.Errorf("%w: lexical index list limit is not bounded", port.ErrKnowledgeValidation)
	}
	if afterID != "" && utf8.RuneCountInString(afterID) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return nil, fmt.Errorf("%w: lexical index page cursor is not bounded", port.ErrKnowledgeValidation)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT item_id, item_revision, source_digest FROM knowledge_retrieval_fts
		WHERE item_kind = ? AND item_id > ?
		ORDER BY item_id LIMIT ?`, kind, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: list lexical index rows: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]port.KnowledgeLexicalIndexRow, 0, limit)
	for rows.Next() {
		var row port.KnowledgeLexicalIndexRow
		row.Kind = kind
		if err := rows.Scan(&row.ID, &row.Revision, &row.SourceDigest); err != nil {
			return nil, fmt.Errorf("%w: lexical index row scan: %v", port.ErrKnowledgeUnavailable, err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: lexical index row scan: %v", port.ErrKnowledgeUnavailable, err)
	}
	return result, nil
}

// ClearLexical removes every reconstructible lexical index row for rebuild.
// It never touches authoritative knowledge tables.
func (s *KnowledgeLexicalIndexStore) ClearLexical(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM knowledge_retrieval_fts`); err != nil {
		return fmt.Errorf("%w: clear lexical index: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}

func validateLexicalScopes(scopes []domain.KnowledgeScopeRef, ownerKey string) error {
	if len(scopes) == 0 {
		return fmt.Errorf("%w: lexical search requires readable scopes", port.ErrKnowledgeValidation)
	}
	for _, scope := range scopes {
		if err := domain.ValidateKnowledgeScope(scope.Kind, scope.ID, domain.HardKnowledgeLimits()); err != nil {
			return fmt.Errorf("%w: lexical search scope is not closed: %v", port.ErrKnowledgeValidation, err)
		}
	}
	if strings.TrimSpace(ownerKey) == "" || utf8.RuneCountInString(ownerKey) > 256 {
		return fmt.Errorf("%w: lexical search requires a bounded owner key", port.ErrKnowledgeValidation)
	}
	return nil
}

func validateLexicalMutation(kind domain.KnowledgeRetrievalItemKind, id string, revision int, sourceDigest string) error {
	if !domain.ValidKnowledgeRetrievalItemKind(kind) {
		return fmt.Errorf("%w: unknown lexical index kind", port.ErrKnowledgeValidation)
	}
	if id == "" || utf8.RuneCountInString(id) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return fmt.Errorf("%w: lexical index identity is not bounded", port.ErrKnowledgeValidation)
	}
	if revision < 1 {
		return fmt.Errorf("%w: lexical index revision must be positive", port.ErrKnowledgeValidation)
	}
	if !domain.ValidKnowledgeIndexSourceDigest(sourceDigest) || sourceDigest == "" {
		return fmt.Errorf("%w: lexical index digest must be a lowercase 64-character hex string", port.ErrKnowledgeValidation)
	}
	return nil
}

// buildLexicalMatch quotes every normalized term as literal FTS5 data,
// escapes embedded quotes, and joins terms with OR. A term is never
// interpreted as an FTS operator or column selector.
func buildLexicalMatch(terms []string) string {
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " OR ")
}
