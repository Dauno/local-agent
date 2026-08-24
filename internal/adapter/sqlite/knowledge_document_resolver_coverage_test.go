package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// resolverCoverageFixture inserts one available result_records row and
// returns everything a test case needs to build a matching or mismatching
// KnowledgeDocument against it.
type resolverCoverageFixture struct {
	resultID string
	digest   string
	content  string
	storage  domain.ResultStorage
}

func newResolverCoverageFixture(t *testing.T, store *Store) resolverCoverageFixture {
	t.Helper()
	content := "curated document content for coverage"
	digest := sha256.Sum256([]byte(content))
	resultID := "b" + hex.EncodeToString(digest[:])[1:]
	storage := domain.ResultStorage{Kind: domain.ResultStorageArtifact, Key: "result-key"}
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO result_records
			(result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key, sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state)
		VALUES (?, 'tool_operation', 'producer', 1, ?, ?, ?, ?, 'text/markdown', 'U12345678', 'T12345678', 'slack:T12345678:dm:D12345678', 'project-a', 'conversation', 1, 'available')`,
		resultID, storage.Kind, storage.Key, hex.EncodeToString(digest[:]), len(content)); err != nil {
		t.Fatal(err)
	}
	return resolverCoverageFixture{resultID: resultID, digest: hex.EncodeToString(digest[:]), content: content, storage: storage}
}

func (f resolverCoverageFixture) document() domain.KnowledgeDocument {
	return domain.KnowledgeDocument{
		ID: "doc-1", Subject: "architecture", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
		ContentDigest: f.digest, ContentHandle: "result:" + f.resultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}
}

func TestKnowledgeDocumentResolverRejectsEveryIdentityFailure(t *testing.T) {
	otherStorage := domain.ResultStorage{Kind: domain.ResultStorageArtifact, Key: "different-key"}

	t.Run("nil payload store", func(t *testing.T) {
		store, err := Initialize(t.Context(), t.TempDir()+"/resolver-nil-payload.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		fixture := newResolverCoverageFixture(t, store)
		resolver := &KnowledgeDocumentResolver{db: store.DB(), payload: nil}
		if _, err := resolver.Resolve(t.Context(), fixture.document(), domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, port.ErrKnowledgeUnavailable) {
			t.Fatalf("nil payload error = %v, want ErrKnowledgeUnavailable", err)
		}
	})

	t.Run("archived document", func(t *testing.T) {
		store, err := Initialize(t.Context(), t.TempDir()+"/resolver-archived.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		fixture := newResolverCoverageFixture(t, store)
		resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{storage: fixture.storage, content: fixture.content})
		document := fixture.document()
		document.Status = domain.KnowledgeDocumentArchived
		if _, err := resolver.Resolve(t.Context(), document, domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, port.ErrKnowledgeUnavailable) {
			t.Fatalf("archived document error = %v, want ErrKnowledgeUnavailable", err)
		}
	})

	t.Run("non curated provenance", func(t *testing.T) {
		store, err := Initialize(t.Context(), t.TempDir()+"/resolver-provenance.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		fixture := newResolverCoverageFixture(t, store)
		resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{storage: fixture.storage, content: fixture.content})
		document := fixture.document()
		document.Provenance = domain.KnowledgeProvenance("authoritative")
		if _, err := resolver.Resolve(t.Context(), document, domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, port.ErrKnowledgeUnavailable) {
			t.Fatalf("non-curated provenance error = %v, want ErrKnowledgeUnavailable", err)
		}
	})

	t.Run("missing result", func(t *testing.T) {
		store, err := Initialize(t.Context(), t.TempDir()+"/resolver-missing.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		fixture := newResolverCoverageFixture(t, store)
		resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{storage: fixture.storage, content: fixture.content})
		document := fixture.document()
		document.ContentHandle = "result:" + (fixture.digest[:63] + "0")
		if _, err := resolver.Resolve(t.Context(), document, domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, port.ErrKnowledgeUnavailable) {
			t.Fatalf("missing result error = %v, want ErrKnowledgeUnavailable", err)
		}
	})

	t.Run("quarantined state", func(t *testing.T) {
		store, err := Initialize(t.Context(), t.TempDir()+"/resolver-quarantined.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		fixture := newResolverCoverageFixture(t, store)
		if _, err := store.DB().ExecContext(t.Context(), `UPDATE result_records SET state = 'quarantined' WHERE result_id = ?`, fixture.resultID); err != nil {
			t.Fatal(err)
		}
		resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{storage: fixture.storage, content: fixture.content})
		if _, err := resolver.Resolve(t.Context(), fixture.document(), domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, port.ErrKnowledgeUnavailable) {
			t.Fatalf("quarantined state error = %v, want ErrKnowledgeUnavailable", err)
		}
	})

	t.Run("digest mismatch", func(t *testing.T) {
		store, err := Initialize(t.Context(), t.TempDir()+"/resolver-digest.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		fixture := newResolverCoverageFixture(t, store)
		resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{storage: fixture.storage, content: fixture.content})
		document := fixture.document()
		document.ContentDigest = hex.EncodeToString(sha256Sum([]byte("different content")))
		if _, err := resolver.Resolve(t.Context(), document, domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, port.ErrKnowledgeUnavailable) {
			t.Fatalf("digest mismatch error = %v, want ErrKnowledgeUnavailable", err)
		}
	})

	t.Run("oversized result", func(t *testing.T) {
		store, err := Initialize(t.Context(), t.TempDir()+"/resolver-oversized.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		fixture := newResolverCoverageFixture(t, store)
		resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{storage: fixture.storage, content: fixture.content})
		limits := domain.DefaultKnowledgeRetrievalLimits()
		limits.MaxDocumentBytes = 1
		if _, err := resolver.Resolve(t.Context(), fixture.document(), limits); !errors.Is(err, port.ErrKnowledgeUnavailable) {
			t.Fatalf("oversized result error = %v, want ErrKnowledgeUnavailable", err)
		}
	})

	t.Run("storage mismatch", func(t *testing.T) {
		store, err := Initialize(t.Context(), t.TempDir()+"/resolver-storage.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		fixture := newResolverCoverageFixture(t, store)
		resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{
			storage: fixture.storage, content: fixture.content, storageForOverride: &otherStorage,
		})
		if _, err := resolver.Resolve(t.Context(), fixture.document(), domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, port.ErrKnowledgeUnavailable) {
			t.Fatalf("storage mismatch error = %v, want ErrKnowledgeUnavailable", err)
		}
	})

	t.Run("truncated read", func(t *testing.T) {
		store, err := Initialize(t.Context(), t.TempDir()+"/resolver-truncated.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		fixture := newResolverCoverageFixture(t, store)
		resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{storage: fixture.storage, content: fixture.content, truncate: true})
		if _, err := resolver.Resolve(t.Context(), fixture.document(), domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, port.ErrKnowledgeUnavailable) {
			t.Fatalf("truncated read error = %v, want ErrKnowledgeUnavailable", err)
		}
	})

	t.Run("altered bytes with matching length", func(t *testing.T) {
		store, err := Initialize(t.Context(), t.TempDir()+"/resolver-altered.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		fixture := newResolverCoverageFixture(t, store)
		altered := make([]byte, len(fixture.content))
		copy(altered, fixture.content)
		altered[0] ^= 0xff
		resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{storage: fixture.storage, content: string(altered)})
		if _, err := resolver.Resolve(t.Context(), fixture.document(), domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, port.ErrKnowledgeUnavailable) {
			t.Fatalf("altered bytes error = %v, want ErrKnowledgeUnavailable", err)
		}
	})

	t.Run("invalid UTF-8", func(t *testing.T) {
		store, err := Initialize(t.Context(), t.TempDir()+"/resolver-utf8.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		invalid := string([]byte{0xff, 0xfe})
		digest := sha256Sum([]byte(invalid))
		storage := domain.ResultStorage{Kind: domain.ResultStorageArtifact, Key: "result-key"}
		resultID := "c" + hex.EncodeToString(digest)[1:]
		if _, err := store.DB().ExecContext(t.Context(), `
			INSERT INTO result_records
				(result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key, sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state)
			VALUES (?, 'tool_operation', 'producer', 1, ?, ?, ?, ?, 'text/markdown', 'U12345678', 'T12345678', 'slack:T12345678:dm:D12345678', 'project-a', 'conversation', 1, 'available')`,
			resultID, storage.Kind, storage.Key, hex.EncodeToString(digest), len(invalid)); err != nil {
			t.Fatal(err)
		}
		document := domain.KnowledgeDocument{
			ID: "doc-1", Subject: "architecture", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
			ContentDigest: hex.EncodeToString(digest), ContentHandle: "result:" + resultID,
			Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
		}
		resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{storage: storage, content: invalid})
		if _, err := resolver.Resolve(t.Context(), document, domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, port.ErrKnowledgeUnavailable) {
			t.Fatalf("invalid UTF-8 error = %v, want ErrKnowledgeUnavailable", err)
		}
	})

	t.Run("read cancellation propagates", func(t *testing.T) {
		store, err := Initialize(t.Context(), t.TempDir()+"/resolver-cancel.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		fixture := newResolverCoverageFixture(t, store)
		resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{storage: fixture.storage, content: fixture.content, readErr: context.Canceled})
		if _, err := resolver.Resolve(t.Context(), fixture.document(), domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v, want context.Canceled", err)
		}
	})
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
