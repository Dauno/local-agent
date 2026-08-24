package sqlite

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

const semanticTestFingerprint = "semantic-test-fingerprint"

// semanticTestVector encodes one 4-dimension vector as little-endian
// float32 bytes for a knowledge_embeddings row.
func semanticTestVector(values ...float32) []byte {
	encoded := make([]byte, len(values)*4)
	for index, value := range values {
		binary.LittleEndian.PutUint32(encoded[index*4:], math.Float32bits(value))
	}
	return encoded
}

func semanticTestSeedVector(t *testing.T, store *Store, kind domain.KnowledgeRetrievalItemKind, id string, revision int, digest, fingerprint string, dimensions int, vector []byte) {
	t.Helper()
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_embeddings (item_kind, item_id, item_revision, source_digest, model_fingerprint, dimensions, vector, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 100)`,
		kind, id, revision, digest, fingerprint, dimensions, vector); err != nil {
		t.Fatalf("seed vector row %s/%s: %v", kind, id, err)
	}
}

func TestKnowledgeSemanticSearchAuthorizesAndFiltersInSQL(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	nowUnix := now.Unix()
	seedRetrievalClaim(t, store, "kclaim_sem_project", "semantic project subject", "is", "string", "value", "", "project", "my-project", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_sem_foreign", "semantic foreign subject", "is", "string", "value", "", "team", "T00000002", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_sem_archived", "semantic archived subject", "is", "string", "value", "", "project", "my-project", "archived", nowUnix, 0, 1)
	ownOwner := "slack:T00000001:user:U00000001"
	otherOwner := "slack:T00000001:user:U99999999"
	seedRetrievalPreference(t, store, ownOwner, "semantic pref", "string", "value", "active", 1)
	seedRetrievalPreference(t, store, otherOwner, "semantic other pref", "string", "value", "active", 1)
	seedRetrievalDocument(t, store, "kdoc_sem", "semantic doc subject", "global", "", "active", "curated", "", 0)
	seedRetrievalDocument(t, store, "kdoc_sem_archived", "semantic archived doc", "global", "", "archived", "curated", "", 0)

	seed := func(kind domain.KnowledgeRetrievalItemKind, id string, revision int) {
		semanticTestSeedVector(t, store, kind, id, revision, validVectorDigest, semanticTestFingerprint, 4, semanticTestVector(1, 0, 0, 0))
	}
	seed(domain.KnowledgeRetrievalClaim, "kclaim_sem_project", 1)
	seed(domain.KnowledgeRetrievalClaim, "kclaim_sem_foreign", 1)
	seed(domain.KnowledgeRetrievalClaim, "kclaim_sem_archived", 1)
	seed(domain.KnowledgeRetrievalPreference, "preference:1", 1)
	seed(domain.KnowledgeRetrievalPreference, "preference:2", 1)
	seed(domain.KnowledgeRetrievalDocument, "kdoc_sem", 1)
	seed(domain.KnowledgeRetrievalDocument, "kdoc_sem_archived", 1)

	index := NewKnowledgeLexicalIndexStore(store, semanticTestFingerprint)
	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "")
	scopes := domain.KnowledgeReadableScopes(binding)
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)

	// The query vector is identical to every stored vector, so similarity
	// cannot distinguish rows: authorization must.
	hits, err := index.SearchSemantic(t.Context(), scopes, owner, []float32{1, 0, 0, 0}, 0, 16)
	if err != nil {
		t.Fatalf("SearchSemantic() error = %v", err)
	}
	got := indexHitIDs(hits)
	want := []string{
		"claim:kclaim_sem_project",
		"document:kdoc_sem",
		"preference:preference:1",
	}
	if !equalStringSets(got, want) {
		t.Fatalf("SearchSemantic() hits = %v, want exactly %v (foreign, archived, and other-owner rows invisible)", got, want)
	}
}

func TestKnowledgeSemanticSearchIgnoresOtherFingerprintsAndStaleRevisions(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	seedRetrievalClaim(t, store, "kclaim_sem_ok", "semantic ok subject", "is", "string", "value", "", "project", "my-project", "asserted", now.Unix(), 0, 2)
	seedRetrievalClaim(t, store, "kclaim_sem_stale", "semantic stale subject", "is", "string", "value", "", "project", "my-project", "asserted", now.Unix(), 0, 2)

	// Bound fingerprint, matching revision.
	semanticTestSeedVector(t, store, domain.KnowledgeRetrievalClaim, "kclaim_sem_ok", 2, validVectorDigest, semanticTestFingerprint, 4, semanticTestVector(1, 0, 0, 0))
	// A different provider configuration: same identity, other fingerprint.
	semanticTestSeedVector(t, store, domain.KnowledgeRetrievalClaim, "kclaim_sem_ok", 2, validVectorDigest, "other-fingerprint", 4, semanticTestVector(1, 0, 0, 0))
	// Bound fingerprint but the truth advanced: stale revision 1 vs 2.
	semanticTestSeedVector(t, store, domain.KnowledgeRetrievalClaim, "kclaim_sem_stale", 1, validVectorDigest, semanticTestFingerprint, 4, semanticTestVector(1, 0, 0, 0))

	index := NewKnowledgeLexicalIndexStore(store, semanticTestFingerprint)
	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "")
	scopes := domain.KnowledgeReadableScopes(binding)
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)

	hits, err := index.SearchSemantic(t.Context(), scopes, owner, []float32{1, 0, 0, 0}, 0, 16)
	if err != nil {
		t.Fatalf("SearchSemantic() error = %v", err)
	}
	got := indexHitIDs(hits)
	if len(got) != 1 || got[0] != "claim:kclaim_sem_ok" {
		t.Fatalf("SearchSemantic() hits = %v, want only the bound-fingerprint current-revision row", got)
	}
}

func TestKnowledgeSemanticSearchThresholdOrderAndLimit(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	for _, id := range []string{"kclaim_sim_high", "kclaim_sim_mid", "kclaim_sim_low"} {
		seedRetrievalClaim(t, store, id, "semantic "+id, "is", "string", "value", "", "project", "my-project", "asserted", now.Unix(), 0, 1)
	}
	// Query vector [1 0 0 0]. High similarity ~1.0, mid ~0.6, low ~0.2.
	semanticTestSeedVector(t, store, domain.KnowledgeRetrievalClaim, "kclaim_sim_high", 1, validVectorDigest, semanticTestFingerprint, 4, semanticTestVector(1, 0, 0, 0))
	semanticTestSeedVector(t, store, domain.KnowledgeRetrievalClaim, "kclaim_sim_mid", 1, validVectorDigest, semanticTestFingerprint, 4, semanticTestVector(0.6, 0.8, 0, 0))
	semanticTestSeedVector(t, store, domain.KnowledgeRetrievalClaim, "kclaim_sim_low", 1, validVectorDigest, semanticTestFingerprint, 4, semanticTestVector(0.2, 0.98, 0, 0))

	index := NewKnowledgeLexicalIndexStore(store, semanticTestFingerprint)
	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "")
	scopes := domain.KnowledgeReadableScopes(binding)
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)

	// Threshold 5000 basis points excludes the ~0.2 row only.
	hits, err := index.SearchSemantic(t.Context(), scopes, owner, []float32{1, 0, 0, 0}, 5000, 16)
	if err != nil {
		t.Fatalf("SearchSemantic() error = %v", err)
	}
	got := indexHitIDs(hits)
	if len(got) != 2 || got[0] != "claim:kclaim_sim_high" || got[1] != "claim:kclaim_sim_mid" {
		t.Fatalf("SearchSemantic(threshold) hits = %v, want high then mid in descending similarity", got)
	}
	// Ranks are one-based positions in similarity order.
	for i, hit := range hits {
		if hit.Rank != i+1 {
			t.Fatalf("hit %d rank = %d, want %d", i, hit.Rank, i+1)
		}
	}
	// Limit caps the result count.
	capped, err := index.SearchSemantic(t.Context(), scopes, owner, []float32{1, 0, 0, 0}, 0, 1)
	if err != nil {
		t.Fatalf("SearchSemantic(capped) error = %v", err)
	}
	if len(capped) != 1 || capped[0].ID != "kclaim_sim_high" {
		t.Fatalf("SearchSemantic(capped) hits = %v, want only the highest similarity row", indexHitIDs(capped))
	}
}

func TestKnowledgeSemanticSearchSkipsMalformedRowsDefensively(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	seedRetrievalClaim(t, store, "kclaim_sem_good", "semantic good subject", "is", "string", "value", "", "project", "my-project", "asserted", now.Unix(), 0, 1)
	seedRetrievalClaim(t, store, "kclaim_sem_badlen", "semantic badlen subject", "is", "string", "value", "", "project", "my-project", "asserted", now.Unix(), 0, 1)
	seedRetrievalClaim(t, store, "kclaim_sem_baddims", "semantic baddims subject", "is", "string", "value", "", "project", "my-project", "asserted", now.Unix(), 0, 1)

	semanticTestSeedVector(t, store, domain.KnowledgeRetrievalClaim, "kclaim_sem_good", 1, validVectorDigest, semanticTestFingerprint, 4, semanticTestVector(1, 0, 0, 0))
	// Structural CHECK constraints are bypassed to place corrupt rows: a
	// vector whose byte length does not match its dimensions, and a
	// dimensions column disagreeing with the query vector. The scan must
	// skip both without failing the search.
	if _, err := store.DB().ExecContext(t.Context(), `PRAGMA ignore_check_constraints = ON`); err != nil {
		t.Fatal(err)
	}
	semanticTestSeedVector(t, store, domain.KnowledgeRetrievalClaim, "kclaim_sem_badlen", 1, validVectorDigest, semanticTestFingerprint, 4, make([]byte, 6))
	semanticTestSeedVector(t, store, domain.KnowledgeRetrievalClaim, "kclaim_sem_baddims", 1, validVectorDigest, semanticTestFingerprint, 2, semanticTestVector(1, 0))

	index := NewKnowledgeLexicalIndexStore(store, semanticTestFingerprint)
	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "")
	scopes := domain.KnowledgeReadableScopes(binding)
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)

	hits, err := index.SearchSemantic(t.Context(), scopes, owner, []float32{1, 0, 0, 0}, 0, 16)
	if err != nil {
		t.Fatalf("SearchSemantic() error = %v", err)
	}
	got := indexHitIDs(hits)
	if len(got) != 1 || got[0] != "claim:kclaim_sem_good" {
		t.Fatalf("SearchSemantic() hits = %v, want only the well-formed row (corrupt rows skipped)", got)
	}
}

func TestKnowledgeSemanticSearchValidationNegatives(t *testing.T) {
	store, _ := newTestStore(t)
	index := NewKnowledgeLexicalIndexStore(store, semanticTestFingerprint)
	binding := retrievalTestBinding("T00000001", "U00000001", "", "")
	scopes := domain.KnowledgeReadableScopes(binding)
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)

	if _, err := index.SearchSemantic(t.Context(), nil, owner, []float32{1}, 0, 8); err == nil {
		t.Fatal("SearchSemantic(no scopes) succeeded")
	}
	if _, err := index.SearchSemantic(t.Context(), scopes, "", []float32{1}, 0, 8); err == nil {
		t.Fatal("SearchSemantic(no owner) succeeded")
	}
	if _, err := index.SearchSemantic(t.Context(), scopes, owner, nil, 0, 8); err == nil {
		t.Fatal("SearchSemantic(empty vector) succeeded")
	}
	if _, err := index.SearchSemantic(t.Context(), scopes, owner, []float32{float32(math.NaN())}, 0, 8); err == nil {
		t.Fatal("SearchSemantic(non-finite vector) succeeded")
	}
	if _, err := index.SearchSemantic(t.Context(), scopes, owner, []float32{1}, -1, 8); err == nil {
		t.Fatal("SearchSemantic(negative threshold) succeeded")
	}
	if _, err := index.SearchSemantic(t.Context(), scopes, owner, []float32{1}, 0, 0); err == nil {
		t.Fatal("SearchSemantic(zero limit) succeeded")
	}
	if _, err := index.SearchSemantic(t.Context(), scopes, owner, []float32{1}, 0, domain.HardMaxKnowledgeRetrievalMaxCandidatesPerChannel+1); err == nil {
		t.Fatal("SearchSemantic(over-limit) succeeded")
	}
	disabled := NewKnowledgeLexicalIndexStore(store, "")
	if _, err := disabled.SearchSemantic(t.Context(), scopes, owner, []float32{1}, 0, 8); err == nil {
		t.Fatal("SearchSemantic(no bound fingerprint) succeeded")
	}
}
