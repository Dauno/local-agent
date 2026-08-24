package sqlite

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestKnowledgeLexicalIndexSearchAuthorizesEachKindInSQL(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	nowUnix := now.Unix()
	seedRetrievalClaim(t, store, "kclaim_project", "lexical project subject", "is", "string", "lexical-value", "", "project", "my-project", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_foreign", "lexical foreign subject", "is", "string", "lexical-value", "", "team", "T00000002", "asserted", nowUnix, 0, 1)
	seedRetrievalClaim(t, store, "kclaim_archived", "lexical archived subject", "is", "string", "lexical-value", "", "project", "my-project", "archived", nowUnix, 0, 1)
	ownOwner := "slack:T00000001:user:U00000001"
	otherOwner := "slack:T00000001:user:U99999999"
	seedRetrievalPreference(t, store, ownOwner, "lexical preference", "string", "pref-value", "active", 1)
	seedRetrievalPreference(t, store, otherOwner, "lexical other preference", "string", "pref-value", "active", 1)
	seedRetrievalDocument(t, store, "kdoc_lex", "lexical document subject", "global", "", "active", "curated", "", 0)
	seedRetrievalDocument(t, store, "kdoc_archived", "lexical old document", "global", "", "archived", "curated", "", 0)

	index := NewKnowledgeLexicalIndexStore(store, "")
	replace := func(kind domain.KnowledgeRetrievalItemKind, id, subject, body string, revision int) {
		t.Helper()
		if err := index.ReplaceLexical(t.Context(), kind, id, revision, knowledgeIndexTestDigest(kind, id, revision, subject, body), subject, body); err != nil {
			t.Fatalf("ReplaceLexical(%s/%s) error = %v", kind, id, err)
		}
	}
	replace(domain.KnowledgeRetrievalClaim, "kclaim_project", "lexical project subject", "lexical-value", 1)
	replace(domain.KnowledgeRetrievalClaim, "kclaim_foreign", "lexical foreign subject", "lexical-value", 1)
	replace(domain.KnowledgeRetrievalClaim, "kclaim_archived", "lexical archived subject", "lexical-value", 1)
	replace(domain.KnowledgeRetrievalPreference, "preference:1", "lexical preference", "pref-value", 1)
	replace(domain.KnowledgeRetrievalPreference, "preference:2", "lexical other preference", "pref-value", 1)
	replace(domain.KnowledgeRetrievalDocument, "kdoc_lex", "lexical document subject", "document-body", 1)
	replace(domain.KnowledgeRetrievalDocument, "kdoc_archived", "lexical old document", "document-body", 1)

	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "")
	scopes := domain.KnowledgeReadableScopes(binding)
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)

	hits, err := index.SearchLexical(t.Context(), scopes, owner, "lexical", 16)
	if err != nil {
		t.Fatalf("SearchLexical() error = %v", err)
	}
	got := indexHitIDs(hits)
	want := []string{
		"claim:kclaim_project",
		"document:kdoc_lex",
		"preference:preference:1",
	}
	if !equalStringSets(got, want) {
		t.Fatalf("SearchLexical() hits = %v, want exactly %v (foreign, archived, and other-owner rows invisible)", got, want)
	}
	for i, hit := range hits {
		if hit.Rank != i+1 {
			t.Fatalf("hit rank = %d at position %d, want %d", hit.Rank, i, i+1)
		}
	}
	for _, hit := range hits {
		if hit.Revision != 1 || hit.SourceDigest == "" {
			t.Fatalf("hit %s/%s carries revision %d digest %q", hit.Kind, hit.ID, hit.Revision, hit.SourceDigest)
		}
	}
}

func TestKnowledgeLexicalIndexSearchQuotesTermsAsData(t *testing.T) {
	store, _ := newTestStore(t)
	index := NewKnowledgeLexicalIndexStore(store, "")
	// Metacharacter-shaped terms must match as literal data, never as FTS
	// operators, and the MATCH value is always a bound parameter.
	subjects := []struct {
		kind               domain.KnowledgeRetrievalItemKind
		id, subject_, body string
	}{
		{domain.KnowledgeRetrievalClaim, "kclaim_meta_1", "service name", "a-b.c:d_e/f@g#h"},
		{domain.KnowledgeRetrievalClaim, "kclaim_meta_2", "quoted subject", `he said "hello" to us`},
		{domain.KnowledgeRetrievalClaim, "kclaim_meta_3", "star subject", "asterisk*star"},
	}
	for _, subject := range subjects {
		if err := index.ReplaceLexical(t.Context(), subject.kind, subject.id, 1, knowledgeIndexTestDigest(subject.kind, subject.id, 1, subject.subject_, subject.body), subject.subject_, subject.body); err != nil {
			t.Fatalf("ReplaceLexical() error = %v", err)
		}
	}
	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "")
	scopes := domain.KnowledgeReadableScopes(binding)
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)

	// Claims in the project scope exist so the authorization EXISTS passes.
	now := retrievalTestNow()
	for _, id := range []string{"kclaim_meta_1", "kclaim_meta_2", "kclaim_meta_3"} {
		seedRetrievalClaim(t, store, id, "authoritative subject", "is", "string", "authoritative-value", "", "project", "my-project", "asserted", now.Unix(), 0, 1)
	}

	for _, term := range []string{`a-b.c:d_e/f@g#h`, `"hello"`, `asterisk*star`, `service`, `star`} {
		hits, err := index.SearchLexical(t.Context(), scopes, owner, term, 16)
		if err != nil {
			t.Fatalf("SearchLexical(%q) error = %v", term, err)
		}
		if len(hits) == 0 {
			t.Fatalf("SearchLexical(%q) = no hits, want the literal-data match", term)
		}
	}
	// A term that matches nothing returns an empty set, not an error.
	hits, err := index.SearchLexical(t.Context(), scopes, owner, `zzz-no-match`, 16)
	if err != nil || len(hits) != 0 {
		t.Fatalf("SearchLexical(no-match) = %v, %v, want empty", indexHitIDs(hits), err)
	}
	// Empty term sets never touch the index.
	hits, err = index.SearchLexical(t.Context(), scopes, owner, "   ", 16)
	if err != nil || len(hits) != 0 {
		t.Fatalf("SearchLexical(blank) = %v, %v, want empty", indexHitIDs(hits), err)
	}
}

func TestKnowledgeLexicalIndexBM25OrderAndKindTies(t *testing.T) {
	store, _ := newTestStore(t)
	now := retrievalTestNow()
	index := NewKnowledgeLexicalIndexStore(store, "")
	// Two documents share one distinctive term; BM25 ranks the one with
	// more occurrences first, and ties resolve by kind then identity.
	seedRetrievalDocument(t, store, "kdoc_common_a", "subject common a", "global", "", "active", "curated", "", 0)
	seedRetrievalDocument(t, store, "kdoc_common_b", "subject common b", "global", "", "active", "curated", "", 0)
	seedRetrievalClaim(t, store, "kclaim_common", "subject common claim", "is", "string", "common", "", "project", "my-project", "asserted", now.Unix(), 0, 1)

	replace := func(kind domain.KnowledgeRetrievalItemKind, id, subject, body string) {
		t.Helper()
		if err := index.ReplaceLexical(t.Context(), kind, id, 1, knowledgeIndexTestDigest(kind, id, 1, subject, body), subject, body); err != nil {
			t.Fatalf("ReplaceLexical() error = %v", err)
		}
	}
	replace(domain.KnowledgeRetrievalDocument, "kdoc_common_a", "subject common a", "common common common")
	replace(domain.KnowledgeRetrievalDocument, "kdoc_common_b", "subject common b", "common")
	replace(domain.KnowledgeRetrievalClaim, "kclaim_common", "subject common claim", "common")

	binding := retrievalTestBinding("T00000001", "U00000001", "my-project", "")
	scopes := domain.KnowledgeReadableScopes(binding)
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)
	hits, err := index.SearchLexical(t.Context(), scopes, owner, "common", 16)
	if err != nil {
		t.Fatalf("SearchLexical() error = %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("SearchLexical() = %v, want 3 hits", indexHitIDs(hits))
	}
	if hits[0].ID != "kdoc_common_a" {
		t.Fatalf("first hit = %s/%s, want the higher-BM25 document kdoc_common_a", hits[0].Kind, hits[0].ID)
	}
	// Ranks are strictly increasing one-based positions in BM25 order.
	for i, hit := range hits {
		if hit.Rank != i+1 {
			t.Fatalf("hit %d rank = %d, want %d", i, hit.Rank, i+1)
		}
	}
}

func TestKnowledgeLexicalIndexWorkerSurface(t *testing.T) {
	store, _ := newTestStore(t)
	index := NewKnowledgeLexicalIndexStore(store, "")

	if err := index.ReplaceLexical(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_row", 2, knowledgeIndexTestDigest(domain.KnowledgeRetrievalClaim, "kclaim_row", 2, "s", "b"), "s", "b"); err != nil {
		t.Fatalf("ReplaceLexical() error = %v", err)
	}
	rows, err := index.ListLexical(t.Context(), domain.KnowledgeRetrievalClaim, "", 16)
	if err != nil || len(rows) != 1 || rows[0].Revision != 2 {
		t.Fatalf("ListLexical() = %+v, %v", rows, err)
	}
	// Replacement is atomic: old rows are gone.
	if err := index.ReplaceLexical(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_row", 3, knowledgeIndexTestDigest(domain.KnowledgeRetrievalClaim, "kclaim_row", 3, "s2", "b2"), "s2", "b2"); err != nil {
		t.Fatalf("ReplaceLexical(second) error = %v", err)
	}
	rows, err = index.ListLexical(t.Context(), domain.KnowledgeRetrievalClaim, "", 16)
	if err != nil || len(rows) != 1 || rows[0].Revision != 3 {
		t.Fatalf("ListLexical(after replace) = %+v, %v", rows, err)
	}
	if err := index.DeleteLexical(t.Context(), domain.KnowledgeRetrievalClaim, "kclaim_row"); err != nil {
		t.Fatalf("DeleteLexical() error = %v", err)
	}
	rows, err = index.ListLexical(t.Context(), domain.KnowledgeRetrievalClaim, "", 16)
	if err != nil || len(rows) != 0 {
		t.Fatalf("ListLexical(after delete) = %+v, %v", rows, err)
	}
	// Clear removes everything for rebuild.
	if err := index.ReplaceLexical(t.Context(), domain.KnowledgeRetrievalDocument, "kdoc_x", 1, knowledgeIndexTestDigest(domain.KnowledgeRetrievalDocument, "kdoc_x", 1, "s", "b"), "s", "b"); err != nil {
		t.Fatalf("ReplaceLexical(doc) error = %v", err)
	}
	if err := index.ClearLexical(t.Context()); err != nil {
		t.Fatalf("ClearLexical() error = %v", err)
	}
	rows, err = index.ListLexical(t.Context(), domain.KnowledgeRetrievalDocument, "", 16)
	if err != nil || len(rows) != 0 {
		t.Fatalf("ListLexical(after clear) = %+v, %v", rows, err)
	}
}

func TestKnowledgeLexicalIndexValidationAndSemanticDisabled(t *testing.T) {
	store, _ := newTestStore(t)
	index := NewKnowledgeLexicalIndexStore(store, "")
	binding := retrievalTestBinding("T00000001", "U00000001", "", "")
	scopes := domain.KnowledgeReadableScopes(binding)
	owner := domain.SlackOwnerKey(binding.Conversation, binding.Actor)

	if _, err := index.SearchSemantic(t.Context(), scopes, owner, nil, 0, 8); !errors.Is(err, port.ErrKnowledgeUnavailable) {
		t.Fatalf("SearchSemantic() error = %v, want ErrKnowledgeUnavailable", err)
	}
	if _, err := index.SearchLexical(t.Context(), nil, owner, "query", 8); err == nil {
		t.Fatal("SearchLexical(no scopes) succeeded")
	}
	if _, err := index.SearchLexical(t.Context(), scopes, "", "query", 8); err == nil {
		t.Fatal("SearchLexical(no owner) succeeded")
	}
	if _, err := index.SearchLexical(t.Context(), scopes, owner, "query", 0); err == nil {
		t.Fatal("SearchLexical(zero limit) succeeded")
	}
	if _, err := index.SearchLexical(t.Context(), scopes, owner, "query", domain.HardMaxKnowledgeRetrievalMaxCandidatesPerChannel+1); err == nil {
		t.Fatal("SearchLexical(over-limit) succeeded")
	}
	if err := index.ReplaceLexical(t.Context(), domain.KnowledgeRetrievalClaim, "x", 1, "not-hex", "s", "b"); err == nil {
		t.Fatal("ReplaceLexical(bad digest) succeeded")
	}
	if err := index.ReplaceLexical(t.Context(), domain.KnowledgeRetrievalClaim, "x", 0, knowledgeIndexTestDigest(domain.KnowledgeRetrievalClaim, "x", 0, "s", "b"), "s", "b"); err == nil {
		t.Fatal("ReplaceLexical(zero revision) succeeded")
	}
	if err := index.ReplaceLexical(t.Context(), domain.KnowledgeRetrievalClaim, "x", 1, knowledgeIndexTestDigest(domain.KnowledgeRetrievalClaim, "x", 1, "s", "b"), "", ""); err == nil {
		t.Fatal("ReplaceLexical(empty text) succeeded")
	}
	if _, err := index.ListLexical(t.Context(), "unknown", "", 8); err == nil {
		t.Fatal("ListLexical(unknown kind) succeeded")
	}
	if _, err := index.ListLexical(t.Context(), domain.KnowledgeRetrievalClaim, "", 0); err == nil {
		t.Fatal("ListLexical(zero limit) succeeded")
	}
}

func knowledgeIndexTestDigest(kind domain.KnowledgeRetrievalItemKind, id string, revision int, subject, body string) string {
	return knowledgeLexicalDigestForTest(string(kind), id, revision, subject, body)
}

func indexHitIDs(hits []port.KnowledgeIndexHit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, string(hit.Kind)+":"+hit.ID)
	}
	return ids
}

// knowledgeLexicalDigestForTest mirrors the canonical digest construction
// without importing the use case package.
func knowledgeLexicalDigestForTest(kind, id string, revision int, subject, body string) string {
	payload := struct {
		Kind     string `json:"kind"`
		ID       string `json:"id"`
		Revision int    `json:"revision"`
		Subject  string `json:"subject"`
		Body     string `json:"body"`
	}{Kind: kind, ID: id, Revision: revision, Subject: subject, Body: body}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int)
	for _, value := range a {
		seen[value]++
	}
	for _, value := range b {
		seen[value]--
	}
	for _, count := range seen {
		if count != 0 {
			return false
		}
	}
	return true
}
