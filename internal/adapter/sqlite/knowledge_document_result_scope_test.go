package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// insertKnowledgeTestResult writes a minimal result_records row and returns
// its result ID and content digest, for use by curated-document scope tests.
func insertKnowledgeTestResult(t *testing.T, store *Store, actor, teamID, conversationKey, project string, createdAt time.Time) (resultID, digestHex string) {
	t.Helper()
	content := "curated content for " + actor + ":" + teamID
	digest := sha256.Sum256([]byte(content))
	idSeed := sha256.Sum256([]byte(t.Name() + ":" + actor + ":" + teamID + ":" + conversationKey + ":" + project))
	resultID = hex.EncodeToString(idSeed[:])
	digestHex = hex.EncodeToString(digest[:])
	if _, err := store.db.ExecContext(context.Background(), `
		INSERT INTO result_records
			(result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key, sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state)
		VALUES (?, 'tool_operation', 'producer', 1, 'artifact', 'result-key', ?, ?, 'text/markdown', ?, ?, ?, ?, 'context', ?, 'available')`,
		resultID, digestHex, len(content), actor, teamID, conversationKey, project, createdAt.UnixNano()); err != nil {
		t.Fatal(err)
	}
	return resultID, digestHex
}

func TestCreateDocumentRejectsResultScopeMismatch(t *testing.T) {
	store, raw := newKnowledgeTestStore(t)
	resultID, digest := insertKnowledgeTestResult(t, raw, "U12345678", "T12345678", "slack:T12345678:dm:D12345678", "project-a", time.Now().UTC())

	cases := []struct {
		name      string
		scopeKind domain.KnowledgeScopeKind
		scopeID   string
	}{
		{"global scope has no rule", domain.KnowledgeScopeGlobal, ""},
		{"workstream scope has no rule", domain.KnowledgeScopeWorkstream, "ws-1"},
		{"team mismatch", domain.KnowledgeScopeTeam, "T-other"},
		{"user mismatch", domain.KnowledgeScopeUser, "U-other"},
		{"project mismatch", domain.KnowledgeScopeProject, "project-other"},
		{"conversation mismatch", domain.KnowledgeScopeConversation, "slack:T12345678:dm:D-other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document := domain.KnowledgeDocument{
				Subject: "architecture-" + tc.name, ScopeKind: tc.scopeKind, ScopeID: tc.scopeID,
				ContentDigest: digest, ContentHandle: "result:" + resultID,
				Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
			}
			if _, err := store.CreateDocument(t.Context(), document, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeValidation) {
				t.Fatalf("CreateDocument error = %v, want ErrKnowledgeValidation", err)
			}
			var count int
			if err := raw.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_references WHERE result_id = ?`, resultID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("result_references rows for rejected document = %d, want 0", count)
			}
		})
	}
}

func TestCreateDocumentRetainsResultReferenceOnMatchingScope(t *testing.T) {
	store, raw := newKnowledgeTestStore(t)
	resultID, digest := insertKnowledgeTestResult(t, raw, "U12345678", "T12345678", "slack:T12345678:dm:D12345678", "project-a", time.Now().UTC())

	document := domain.KnowledgeDocument{
		Subject: "architecture", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
		ContentDigest: digest, ContentHandle: "result:" + resultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}
	created, err := store.CreateDocument(t.Context(), document, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	var state string
	var ownerKind, ownerID string
	if err := raw.db.QueryRowContext(t.Context(), `
		SELECT state, owner_kind, owner_id FROM result_references WHERE result_id = ?`, resultID).Scan(&state, &ownerKind, &ownerID); err != nil {
		t.Fatalf("read result reference: %v", err)
	}
	if state != "live" || ownerKind != knowledgeDocumentResultOwnerKind || ownerID != string(created.ID) {
		t.Fatalf("reference = state=%q owner_kind=%q owner_id=%q, want live/%s/%s", state, ownerKind, ownerID, knowledgeDocumentResultOwnerKind, created.ID)
	}

	// Replay: same document content commits again without duplicating the
	// live reference row.
	replayed, err := store.CreateDocument(t.Context(), document, domain.DefaultKnowledgeLimits())
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("replay CreateDocument = %#v, err=%v", replayed, err)
	}
	var refCount int
	if err := raw.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_references WHERE result_id = ?`, resultID).Scan(&refCount); err != nil {
		t.Fatal(err)
	}
	if refCount != 1 {
		t.Fatalf("result_references rows after replay = %d, want 1", refCount)
	}
}

func TestCreateDocumentConflictLeavesNoReference(t *testing.T) {
	store, raw := newKnowledgeTestStore(t)
	resultID, digest := insertKnowledgeTestResult(t, raw, "U12345678", "T12345678", "slack:T12345678:dm:D12345678", "project-a", time.Now().UTC())

	document := domain.KnowledgeDocument{
		Subject: "architecture", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
		ContentDigest: digest, ContentHandle: "result:" + resultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}
	if _, err := store.CreateDocument(t.Context(), document, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	conflicting := document
	conflicting.ContentDigest = strings.Repeat("b", 64)
	if _, err := store.CreateDocument(t.Context(), conflicting, domain.DefaultKnowledgeLimits()); !errors.Is(err, port.ErrKnowledgeCASConflict) {
		t.Fatalf("conflicting CreateDocument error = %v, want ErrKnowledgeCASConflict", err)
	}
	var refCount int
	if err := raw.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_references WHERE result_id = ?`, resultID).Scan(&refCount); err != nil {
		t.Fatal(err)
	}
	if refCount != 1 {
		t.Fatalf("result_references rows after rejected conflict = %d, want 1", refCount)
	}
}

func TestArchiveDocumentReleasesResultReference(t *testing.T) {
	store, raw := newKnowledgeTestStore(t)
	resultID, digest := insertKnowledgeTestResult(t, raw, "U12345678", "T12345678", "slack:T12345678:dm:D12345678", "project-a", time.Now().UTC())

	document := domain.KnowledgeDocument{
		Subject: "architecture", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
		ContentDigest: digest, ContentHandle: "result:" + resultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}
	created, err := store.CreateDocument(t.Context(), document, domain.DefaultKnowledgeLimits())
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if _, err := store.ArchiveDocument(t.Context(), created.ID, created.Revision, "slack-human:evt-archive"); err != nil {
		t.Fatalf("ArchiveDocument: %v", err)
	}
	var state string
	var releasedAt int64
	if err := raw.db.QueryRowContext(t.Context(), `
		SELECT state, released_at FROM result_references WHERE result_id = ?`, resultID).Scan(&state, &releasedAt); err != nil {
		t.Fatalf("read result reference: %v", err)
	}
	if state != "released" || releasedAt <= 0 {
		t.Fatalf("reference after archive = state=%q released_at=%d, want released/>0", state, releasedAt)
	}
}

func TestForgetSubjectReleasesResultReference(t *testing.T) {
	store, raw := newKnowledgeTestStore(t)
	resultID, digest := insertKnowledgeTestResult(t, raw, "U12345678", "T12345678", "slack:T12345678:dm:D12345678", "project-a", time.Now().UTC())

	document := domain.KnowledgeDocument{
		Subject: "architecture", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
		ContentDigest: digest, ContentHandle: "result:" + resultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}
	if _, err := store.CreateDocument(t.Context(), document, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	forgotten, err := store.ForgetSubject(t.Context(), "architecture", domain.KnowledgeScopeTeam, "T12345678", "slack-human:evt-forget")
	if err != nil || !forgotten {
		t.Fatalf("ForgetSubject = %v, err = %v", forgotten, err)
	}
	var state string
	if err := raw.db.QueryRowContext(t.Context(), `
		SELECT state FROM result_references WHERE result_id = ?`, resultID).Scan(&state); err != nil {
		t.Fatalf("read result reference: %v", err)
	}
	if state != "released" {
		t.Fatalf("reference state after forget = %q, want released", state)
	}
}

func TestReferencedResultIsProtectedFromRetentionCandidacy(t *testing.T) {
	store, raw := newKnowledgeTestStore(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	resultID, digest := insertKnowledgeTestResult(t, raw, "U12345678", "T12345678", "slack:T12345678:dm:D12345678", "project-a", old)

	document := domain.KnowledgeDocument{
		Subject: "architecture", ScopeKind: domain.KnowledgeScopeTeam, ScopeID: "T12345678",
		ContentDigest: digest, ContentHandle: "result:" + resultID,
		Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
	}
	if _, err := store.CreateDocument(t.Context(), document, domain.DefaultKnowledgeLimits()); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	health, err := raw.CheckResultRetention(t.Context(), domain.ResultRetentionAges{Context: time.Hour}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CheckResultRetention: %v", err)
	}
	var found bool
	for _, class := range health.Classes {
		if class.Class != domain.ResultRetentionContext {
			continue
		}
		found = true
		if class.Candidates != 1 || class.ReferenceProtected != 1 {
			t.Fatalf("context class counts = %#v, want candidates=1 reference_protected=1", class)
		}
	}
	if !found {
		t.Fatal("context retention class was not reported")
	}
}

// documentResolverScopeStore lets Resolve-level scope tests reuse the
// existing documentResolverPayload fake from knowledge_document_resolver_test.go.
func documentResolverScopeStore(t *testing.T) *Store {
	t.Helper()
	store, err := Initialize(t.Context(), t.TempDir()+"/resolver-scope.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestKnowledgeDocumentResolverRejectsScopeMismatch(t *testing.T) {
	store := documentResolverScopeStore(t)
	resultID := strings.Repeat("a", 64)
	content := "curated document content"
	digest := sha256.Sum256([]byte(content))
	storage := domain.ResultStorage{Kind: domain.ResultStorageArtifact, Key: "result-key"}
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO result_records
			(result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key, sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state)
		VALUES (?, 'tool_operation', 'producer', 1, ?, ?, ?, ?, 'text/markdown', 'U12345678', 'T12345678', 'slack:T12345678:dm:D12345678', 'project-a', 'conversation', 1, 'available')`,
		resultID, storage.Kind, storage.Key, hex.EncodeToString(digest[:]), len(content)); err != nil {
		t.Fatal(err)
	}
	resolver := NewKnowledgeDocumentResolver(store, documentResolverPayload{storage: storage, content: content})

	cases := []struct {
		name      string
		scopeKind domain.KnowledgeScopeKind
		scopeID   string
	}{
		{"global", domain.KnowledgeScopeGlobal, ""},
		{"workstream", domain.KnowledgeScopeWorkstream, "ws-1"},
		{"team mismatch", domain.KnowledgeScopeTeam, "T-other"},
		{"user mismatch", domain.KnowledgeScopeUser, "U-other"},
		{"project mismatch", domain.KnowledgeScopeProject, "project-other"},
		{"conversation mismatch", domain.KnowledgeScopeConversation, "slack:T12345678:dm:D-other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document := domain.KnowledgeDocument{
				ID: "doc-1", Subject: "architecture", ScopeKind: tc.scopeKind, ScopeID: tc.scopeID,
				ContentDigest: hex.EncodeToString(digest[:]), ContentHandle: "result:" + resultID,
				Provenance: domain.KnowledgeProvenanceCurated, Status: domain.KnowledgeDocumentActive,
			}
			if _, err := resolver.Resolve(t.Context(), document, domain.DefaultKnowledgeRetrievalLimits()); !errors.Is(err, port.ErrKnowledgeUnavailable) {
				t.Fatalf("scope mismatch error = %v, want ErrKnowledgeUnavailable", err)
			}
		})
	}
}
