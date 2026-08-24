package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.KnowledgeIndexSource = (*KnowledgeIndexSourceStore)(nil)
var _ port.KnowledgeIdentityLister = (*KnowledgeIndexSourceStore)(nil)

// KnowledgeIndexSourceStore re-reads one complete authoritative item by
// stable identity for index construction. It applies no scope, status, or
// validity predicates: the worker owns the private identity and must see
// archived rows so it can remove their index rows. It never reads document
// content: document resolution happens through the resolver.
type KnowledgeIndexSourceStore struct {
	db *sql.DB
}

func NewKnowledgeIndexSourceStore(store *Store) *KnowledgeIndexSourceStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &KnowledgeIndexSourceStore{db: store.db}
}

// ReadIndexSource returns the authoritative item for one identity. Missing
// items report ErrKnowledgeNotFound.
func (s *KnowledgeIndexSourceStore) ReadIndexSource(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, id string) (port.KnowledgeAuthoritativeItem, error) {
	if !domain.ValidKnowledgeRetrievalItemKind(kind) {
		return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: unknown index source kind", port.ErrKnowledgeValidation)
	}
	if id == "" || utf8.RuneCountInString(id) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: index source identity is not bounded", port.ErrKnowledgeValidation)
	}
	switch kind {
	case domain.KnowledgeRetrievalClaim:
		claim, err := scanKnowledgeClaim(s.db.QueryRowContext(ctx, `
			SELECT `+knowledgeClaimColumns+` FROM knowledge_claims WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return port.KnowledgeAuthoritativeItem{}, port.ErrKnowledgeNotFound
		}
		if err != nil {
			return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: index source claim read: %v", port.ErrKnowledgeUnavailable, err)
		}
		return port.KnowledgeAuthoritativeItem{Kind: kind, ID: id, Claim: &claim}, nil
	case domain.KnowledgeRetrievalPreference:
		rowID, ok := parseStrictPositiveDecimal(strings.TrimPrefix(id, "preference:"))
		if !ok || !strings.HasPrefix(id, "preference:") {
			return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: preference identity is not canonical", port.ErrKnowledgeValidation)
		}
		preference, err := scanKnowledgePreference(s.db.QueryRowContext(ctx, `
			SELECT `+knowledgePreferenceColumns+` FROM knowledge_preferences WHERE id = ?`, rowID))
		if errors.Is(err, sql.ErrNoRows) {
			return port.KnowledgeAuthoritativeItem{}, port.ErrKnowledgeNotFound
		}
		if err != nil {
			return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: index source preference read: %v", port.ErrKnowledgeUnavailable, err)
		}
		return port.KnowledgeAuthoritativeItem{Kind: kind, ID: id, Preference: &preference}, nil
	case domain.KnowledgeRetrievalDocument:
		document, err := scanKnowledgeDocument(s.db.QueryRowContext(ctx, `
			SELECT `+knowledgeDocumentColumns+` FROM knowledge_documents WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return port.KnowledgeAuthoritativeItem{}, port.ErrKnowledgeNotFound
		}
		if err != nil {
			return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: index source document read: %v", port.ErrKnowledgeUnavailable, err)
		}
		return port.KnowledgeAuthoritativeItem{Kind: kind, ID: id, Document: &document}, nil
	default:
		return port.KnowledgeAuthoritativeItem{}, fmt.Errorf("%w: unknown index source kind", port.ErrKnowledgeValidation)
	}
}

// ListTruthIdentities pages authoritative truth identities with their
// current revisions in stable identity order. It never reads content.
func (s *KnowledgeIndexSourceStore) ListTruthIdentities(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, afterID string, limit int) ([]port.KnowledgeTruthIdentity, error) {
	if !domain.ValidKnowledgeRetrievalItemKind(kind) {
		return nil, fmt.Errorf("%w: unknown truth identity kind", port.ErrKnowledgeValidation)
	}
	if !domain.ValidKnowledgeQueueListLimit(limit) {
		return nil, fmt.Errorf("%w: truth identity list limit is not bounded", port.ErrKnowledgeValidation)
	}
	if afterID != "" && utf8.RuneCountInString(afterID) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return nil, fmt.Errorf("%w: truth identity page cursor is not bounded", port.ErrKnowledgeValidation)
	}
	var query string
	switch kind {
	case domain.KnowledgeRetrievalClaim:
		query = `SELECT id, current_rev FROM knowledge_claims WHERE id > ? ORDER BY id LIMIT ?`
	case domain.KnowledgeRetrievalPreference:
		query = `SELECT 'preference:' || id, current_rev FROM knowledge_preferences
			WHERE ('preference:' || id) > ? ORDER BY ('preference:' || id) LIMIT ?`
	case domain.KnowledgeRetrievalDocument:
		query = `SELECT id, current_rev FROM knowledge_documents WHERE id > ? ORDER BY id LIMIT ?`
	default:
		return nil, fmt.Errorf("%w: unknown truth identity kind", port.ErrKnowledgeValidation)
	}
	rows, err := s.db.QueryContext(ctx, query, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: list truth identities: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = rows.Close() }()
	identities := make([]port.KnowledgeTruthIdentity, 0, limit)
	for rows.Next() {
		var identity port.KnowledgeTruthIdentity
		if err := rows.Scan(&identity.ID, &identity.Revision); err != nil {
			return nil, fmt.Errorf("%w: truth identity scan: %v", port.ErrKnowledgeUnavailable, err)
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: truth identity scan: %v", port.ErrKnowledgeUnavailable, err)
	}
	return identities, nil
}
