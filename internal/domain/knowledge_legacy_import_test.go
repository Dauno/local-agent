package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestKnowledgeDocumentFromLegacyTopicScopes(t *testing.T) {
	person := Topic{
		ID:         TopicID("mem_person1"),
		Title:      "Dauno",
		Content:    "dauno is the creator",
		CurrentRev: 3,
		BundlePath: "people",
		OwnerKey:   "slack:T12345678:user:U12345678",
	}
	doc, err := KnowledgeDocumentFromLegacyTopic(person)
	if err != nil {
		t.Fatal(err)
	}
	if doc.ScopeKind != KnowledgeScopeUser || doc.ScopeID != person.OwnerKey {
		t.Fatalf("person topic scope = %s:%q, want user bound to its owner", doc.ScopeKind, doc.ScopeID)
	}

	plain := Topic{
		ID:         TopicID("mem_plain1"),
		Title:      "Durable fact",
		Content:    "the fact is durable",
		CurrentRev: 1,
		BundlePath: "topics",
	}
	doc, err = KnowledgeDocumentFromLegacyTopic(plain)
	if err != nil {
		t.Fatal(err)
	}
	if doc.ScopeKind != KnowledgeScopeGlobal || doc.ScopeID != "" {
		t.Fatalf("non-person topic scope = %s:%q, want global with empty identity", doc.ScopeKind, doc.ScopeID)
	}

	noOwner := Topic{ID: TopicID("mem_orphan"), Title: "Orphan", Content: "x", CurrentRev: 1, BundlePath: "people"}
	if _, err := KnowledgeDocumentFromLegacyTopic(noOwner); err == nil {
		t.Fatal("person topic without an owner must fail closed")
	}

	badOwner := Topic{ID: TopicID("mem_badowner"), Title: "Bad", Content: "x", CurrentRev: 1, BundlePath: "people", OwnerKey: "not-a-valid-owner"}
	if _, err := KnowledgeDocumentFromLegacyTopic(badOwner); err == nil {
		t.Fatal("person topic with a malformed owner must fail closed")
	}
}

func TestKnowledgeDocumentFromLegacyTopicIdentityIsDeterministic(t *testing.T) {
	topic := Topic{ID: TopicID("mem_deterministic"), Title: "Stable", Content: "stable content", CurrentRev: 2, BundlePath: "topics"}
	first, err := KnowledgeDocumentFromLegacyTopic(topic)
	if err != nil {
		t.Fatal(err)
	}
	second, err := KnowledgeDocumentFromLegacyTopic(topic)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || !ValidKnowledgeDocumentID(first.ID) {
		t.Fatalf("document id %q is not deterministic and valid (%q)", first.ID, second.ID)
	}
	if first.Provenance != KnowledgeProvenanceLegacyCurated || first.Status != KnowledgeDocumentActive {
		t.Fatalf("provenance/status = %q/%q, want legacy_curated_document/active", first.Provenance, first.Status)
	}
	if first.SourceID != string(topic.ID) || first.SourceRev != topic.CurrentRev {
		t.Fatalf("source identity = %q rev %d, want the original topic identity and revision", first.SourceID, first.SourceRev)
	}
	want := sha256.Sum256([]byte(topic.Content))
	if first.ContentDigest != hex.EncodeToString(want[:]) {
		t.Fatalf("content digest = %q, want sha256 of the current content", first.ContentDigest)
	}
	if first.Subject != topic.Title+" "+LegacyTopicDocumentSubjectSuffix(topic.ID) {
		t.Fatalf("subject %q is not the pure per-topic value", first.Subject)
	}
	if strings.Contains(first.Subject, topic.Content) {
		t.Fatalf("subject %q leaked content", first.Subject)
	}
	if first.ContentHandle != "" {
		t.Fatalf("mapping must not fabricate a content handle: %q; the store resolves the immutable revision", first.ContentHandle)
	}
	if handle := LegacyTopicRevisionHandle(topic.ID, 7); handle != "memory_topics:mem_deterministic:revision:7" {
		t.Fatalf("revision handle %q does not reference the immutable revision", handle)
	}

	other := Topic{ID: TopicID("mem_other"), Title: "Stable", Content: "stable content", CurrentRev: 2, BundlePath: "topics"}
	third, err := KnowledgeDocumentFromLegacyTopic(other)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == third.ID {
		t.Fatal("different topics must produce different document ids")
	}
}

func TestKnowledgeDocumentFromLegacyTopicMapsArchivedStatus(t *testing.T) {
	active := Topic{ID: TopicID("mem_active"), Title: "Active", Content: "x", CurrentRev: 1, BundlePath: "topics", Status: TopicStatusActive}
	doc, err := KnowledgeDocumentFromLegacyTopic(active)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != KnowledgeDocumentActive {
		t.Fatalf("active topic imported as %q", doc.Status)
	}

	archived := Topic{ID: TopicID("mem_archived"), Title: "Archived", Content: "x", CurrentRev: 1, BundlePath: "topics", Status: TopicStatusArchived}
	doc, err = KnowledgeDocumentFromLegacyTopic(archived)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != KnowledgeDocumentArchived {
		t.Fatalf("archived topic imported as %q, want archived visibility preserved", doc.Status)
	}

	unknown := Topic{ID: TopicID("mem_unknown"), Title: "Unknown", Content: "x", CurrentRev: 1, BundlePath: "topics", Status: TopicStatus("gone")}
	if _, err := KnowledgeDocumentFromLegacyTopic(unknown); err == nil {
		t.Fatal("unknown legacy status must fail closed")
	}
}

func TestLegacyTopicDocumentSubjectSuffixIsDeterministicAndOpaque(t *testing.T) {
	topic := Topic{ID: TopicID("mem_dup"), Title: "Shared", Content: "secret content", CurrentRev: 1, BundlePath: "topics", OwnerKey: "slack:T12345678:user:U12345678"}
	first := LegacyTopicDocumentSubjectSuffix(topic.ID)
	second := LegacyTopicDocumentSubjectSuffix(topic.ID)
	if first != second {
		t.Fatalf("suffix %q is not deterministic (%q)", first, second)
	}
	joined := "Shared " + first
	if strings.Contains(joined, "T12345678") || strings.Contains(joined, "U12345678") || strings.Contains(joined, "secret content") {
		t.Fatalf("suffix %q leaked owner or content identity", first)
	}
	if LegacyTopicDocumentSubjectSuffix(TopicID("mem_other")) == first {
		t.Fatal("different topics must produce different suffixes")
	}
}

func TestValidSlackOwnerKey(t *testing.T) {
	for _, test := range []struct {
		key  string
		want bool
	}{
		{key: "slack:T12345678:user:U12345678", want: true},
		{key: "slack:T:user:U", want: true},
		{key: "slack::user:U12345678", want: false},
		{key: "slack:T12345678:team:U12345678", want: false},
		{key: "other:T12345678:user:U12345678", want: false},
		{key: "", want: false},
		{key: "slack:T12345678:user:", want: false},
	} {
		if got := ValidSlackOwnerKey(test.key); got != test.want {
			t.Fatalf("ValidSlackOwnerKey(%q) = %v, want %v", test.key, got, test.want)
		}
	}
}
