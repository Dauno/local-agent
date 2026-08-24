package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// KnowledgeDocumentResolver reads curated document bytes from the verified V2
// result payload store. Knowledge authorization happens before this resolver
// runs; this layer verifies the immutable content identity and payload bytes.
type KnowledgeDocumentResolver struct {
	db      *sql.DB
	payload port.ResultPayloadStore
}

func NewKnowledgeDocumentResolver(store *Store, payload port.ResultPayloadStore) *KnowledgeDocumentResolver {
	if store == nil || store.db == nil {
		return nil
	}
	return &KnowledgeDocumentResolver{db: store.db, payload: payload}
}

func (r *KnowledgeDocumentResolver) Resolve(ctx context.Context, document domain.KnowledgeDocument, limits domain.KnowledgeRetrievalLimits) ([]byte, error) {
	if r == nil || r.db == nil || r.payload == nil || document.Provenance != domain.KnowledgeProvenanceCurated || document.Status != domain.KnowledgeDocumentActive {
		return nil, port.ErrKnowledgeUnavailable
	}
	bounded := limits.WithDefaults()
	if err := bounded.Validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid retrieval limits", port.ErrKnowledgeValidation)
	}
	resultID, err := parseCuratedDocumentHandle(document.ContentHandle)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid curated document handle", port.ErrKnowledgeValidation)
	}

	var storageKind, storageKey, digest, state, actor, teamID, conversationKey, project string
	var resultBytes int64
	err = r.db.QueryRowContext(ctx, `
		SELECT storage_kind, storage_key, sha256, bytes, state, actor, team_id, conversation_key, project
		FROM result_records WHERE result_id = ?`, resultID).Scan(
		&storageKind, &storageKey, &digest, &resultBytes, &state, &actor, &teamID, &conversationKey, &project)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, port.ErrKnowledgeUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read curated document content identity", port.ErrKnowledgeUnavailable)
	}
	if state != string(domain.ResultAvailable) || digest != document.ContentDigest || resultBytes <= 0 || resultBytes > int64(bounded.MaxDocumentBytes) {
		return nil, port.ErrKnowledgeUnavailable
	}
	resultScope := domain.ResultScope{Actor: actor, TeamID: teamID, ConversationKey: conversationKey, Project: project}
	if !domain.KnowledgeDocumentAuthorizesResult(document.ScopeKind, document.ScopeID, resultScope) {
		return nil, port.ErrKnowledgeUnavailable
	}
	storage := domain.ResultStorage{Kind: domain.ResultStorageKind(storageKind), Key: storageKey}
	if err := storage.Validate(); err != nil {
		return nil, port.ErrKnowledgeUnavailable
	}
	expectedStorage, err := r.payload.StorageFor(resultID)
	if err != nil || expectedStorage != storage {
		return nil, port.ErrKnowledgeUnavailable
	}
	chunk, err := r.payload.ReadRange(ctx, storage, digest, resultBytes, 0, resultBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, port.ErrKnowledgeUnavailable
	}
	content := []byte(chunk.Content)
	if !chunk.EOF || chunk.OffsetBytes != 0 || chunk.NextOffsetBytes != resultBytes || int64(len(content)) != resultBytes || !utf8.Valid(content) {
		return nil, port.ErrKnowledgeUnavailable
	}
	contentDigest := sha256.Sum256(content)
	if hex.EncodeToString(contentDigest[:]) != document.ContentDigest {
		return nil, port.ErrKnowledgeUnavailable
	}
	return append([]byte(nil), content...), nil
}

func parseCuratedDocumentHandle(handle string) (string, error) {
	const prefix = "result:"
	if !strings.HasPrefix(handle, prefix) {
		return "", errors.New("unsupported curated document handle")
	}
	resultID := strings.TrimPrefix(handle, prefix)
	if !domain.ValidResultID(resultID) {
		return "", errors.New("malformed result identity")
	}
	return resultID, nil
}

var _ port.KnowledgeDocumentResolver = (*KnowledgeDocumentResolver)(nil)
