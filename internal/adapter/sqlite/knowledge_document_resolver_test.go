package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type documentResolverPayload struct {
	storage            domain.ResultStorage
	content            string
	storageForOverride *domain.ResultStorage
	readErr            error
	truncate           bool
}

func (p documentResolverPayload) StorageFor(string) (domain.ResultStorage, error) {
	if p.storageForOverride != nil {
		return *p.storageForOverride, nil
	}
	return p.storage, nil
}

func (p documentResolverPayload) Publish(context.Context, domain.ResultStorage, string) error {
	return nil
}

func (p documentResolverPayload) Verify(context.Context, domain.ResultStorage, string, int64) error {
	return nil
}

func (p documentResolverPayload) ReadRange(_ context.Context, _ domain.ResultStorage, _ string, _ int64, offset, max int64) (domain.ResultChunk, error) {
	if p.readErr != nil {
		return domain.ResultChunk{}, p.readErr
	}
	if offset != 0 || max < int64(len(p.content)) {
		return domain.ResultChunk{}, errors.New("invalid range")
	}
	if p.truncate && len(p.content) > 0 {
		truncated := p.content[:len(p.content)-1]
		return domain.ResultChunk{Content: truncated, OffsetBytes: 0, NextOffsetBytes: int64(len(truncated)), EOF: true}, nil
	}
	return domain.ResultChunk{Content: p.content, OffsetBytes: 0, NextOffsetBytes: int64(len(p.content)), EOF: true}, nil
}

func TestKnowledgeDocumentResolverReadsVerifiedCuratedResult(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/resolver.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	resultID := strings.Repeat("a", 64)
	content := "curated document content"
	digest := sha256.Sum256([]byte(content))
	storage := domain.ResultStorage{Kind: domain.ResultStorageArtifact, Key: "result-key"}
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO result_records
			(result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key, sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state)
		VALUES (?, 'tool_operation', 'producer', 1, ?, ?, ?, ?, 'text/markdown', 'U12345678', 'T12345678', 'slack:T12345678:dm:D12345678', 'project', 'conversation', 1, 'available')`,
		resultID, storage.Kind, storage.Key, hex.EncodeToString(digest[:]), len(content)); err != nil {
		t.Fatal(err)
	}
	resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{storage: storage, content: content})
	document := domain.KnowledgeDocument{
		ID: "doc-1", Subject: "architecture", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
		ContentDigest: hex.EncodeToString(digest[:]), ContentHandle: "result:" + resultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}
	got, err := resolver.Resolve(t.Context(), document, domain.DefaultKnowledgeRetrievalLimits())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("resolved content = %q, want %q", got, content)
	}
}

func TestKnowledgeDocumentResolverRejectsInvalidCuratedContent(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/resolver-invalid.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{})
	document := domain.KnowledgeDocument{
		ContentDigest: strings.Repeat("a", 64), ContentHandle: "memory_topics:old:revision:1",
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}
	if _, err := resolver.Resolve(t.Context(), document, domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, port.ErrKnowledgeValidation) {
		t.Fatalf("invalid handle error = %v, want ErrKnowledgeValidation", err)
	}
}
