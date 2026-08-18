package sqlite

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func digestOf(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func legacyResolverDocument(subject, content string, topicID domain.TopicID, revisionRowID int64, sourceRev int) domain.KnowledgeDocument {
	now := time.Now().UTC()
	return domain.KnowledgeDocument{
		ID:            domain.KnowledgeDocumentID("kdoc-" + subject),
		Subject:       subject,
		ScopeKind:     domain.KnowledgeScopeGlobal,
		ContentDigest: digestOf([]byte(content)),
		ContentHandle: domain.LegacyTopicRevisionHandle(topicID, revisionRowID),
		SourceID:      string(topicID),
		SourceRev:     sourceRev,
		Provenance:    domain.KnowledgeProvenanceLegacyCurated,
		Status:        domain.KnowledgeDocumentActive,
		Revision:      1,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func TestKnowledgeDocumentResolverReturnsExactVerifiedBytes(t *testing.T) {
	store, _ := newTestStore(t)
	content := "immutable revision content"
	topic, err := store.CreateTopic(t.Context(), "legacy", "Legacy", "desc", nil, content, "init")
	if err != nil {
		t.Fatal(err)
	}
	var revisionRowID int64
	var revisionNumber int
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT id, revision_number FROM memory_topic_revisions WHERE topic_id = ? ORDER BY id DESC LIMIT 1`,
		string(topic.ID)).Scan(&revisionRowID, &revisionNumber); err != nil {
		t.Fatal(err)
	}
	resolver := NewKnowledgeDocumentResolver(store)

	// The mutable topic row carries different content; resolution must
	// return the immutable revision bytes, never memory_topics.content.
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE memory_topics SET content = 'mutable decoy content' WHERE id = ?`, string(topic.ID)); err != nil {
		t.Fatal(err)
	}

	got, err := resolver.Resolve(t.Context(), legacyResolverDocument("legacy", content, topic.ID, revisionRowID, revisionNumber), domain.KnowledgeRetrievalLimits{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if string(got) != content {
		t.Fatalf("resolved content = %q, want %q", string(got), content)
	}
	var mutable string
	if err := store.DB().QueryRowContext(t.Context(), `SELECT content FROM memory_topics WHERE id = ?`, string(topic.ID)).Scan(&mutable); err != nil || mutable != "mutable decoy content" {
		t.Fatalf("mutable topic content = %q, %v", mutable, err)
	}
}

func TestKnowledgeDocumentResolverRejectsMalformedHandles(t *testing.T) {
	store, _ := newTestStore(t)
	topic, revisionRowID := seedGuardedTopic(t, store, "legacy", "Legacy")
	resolver := NewKnowledgeDocumentResolver(store)
	content := "immutable revision content"

	malformed := []string{
		"",
		"memory_topics:",
		"memory_topics:mem_x:revision:",
		"memory_topics::revision:1",
		"memory_topics:mem_x:rev:1",
		"memory_topics:mem_x:revision:01",
		"memory_topics:mem_x:revision:007",
		"memory_topics:mem_x:revision:0",
		"memory_topics:mem_x:revision:-1",
		"memory_topics:mem_x:revision:+1",
		"memory_topics:mem_x:revision:1:extra",
		"memory_topics:mem_x:revision:1suffix",
		"memory_topics:mem_x:revision:1.5",
		"memory_topics:mem_x:revision:99999999999999999999",
		"memory_topics:mem_x:revision:9223372036854775808",
		"memory_topics:mem:a:revision:1",
		"memory_topics:mem_x:revision:1:revision:2",
		"memory:mem_x:revision:1",
		"memory_topics:mem_x",
		"memory_topics:mem_x:revision:1 ",
	}
	for _, handle := range malformed {
		document := legacyResolverDocument("legacy", content, topic.ID, revisionRowID, 1)
		document.ContentHandle = handle
		_, err := resolver.Resolve(t.Context(), document, domain.KnowledgeRetrievalLimits{})
		if !errors.Is(err, port.ErrKnowledgeValidation) {
			t.Errorf("handle %q: error = %v, want ErrKnowledgeValidation", handle, err)
		}
		if err != nil && handle != "" && (strings.Contains(err.Error(), handle) || strings.Contains(err.Error(), content)) {
			t.Errorf("handle %q: error leaks input: %v", handle, err)
		}
	}
}

func TestKnowledgeDocumentResolverRejectsUnavailableContent(t *testing.T) {
	store, _ := newTestStore(t)
	topic, revisionRowID := seedGuardedTopic(t, store, "legacy", "Legacy")
	resolver := NewKnowledgeDocumentResolver(store)
	content := "immutable revision content"

	unavailable := []struct {
		name    string
		mutate  func(*domain.KnowledgeDocument)
		limits  domain.KnowledgeRetrievalLimits
		secrets []string
	}{
		{"missing revision row", func(d *domain.KnowledgeDocument) {
			d.ContentHandle = domain.LegacyTopicRevisionHandle(topic.ID, revisionRowID+1000)
		}, domain.KnowledgeRetrievalLimits{}, []string{}},
		{"cross-topic source", func(d *domain.KnowledgeDocument) {
			d.SourceID = "mem_other_topic"
		}, domain.KnowledgeRetrievalLimits{}, []string{}},
		{"wrong revision number", func(d *domain.KnowledgeDocument) {
			d.SourceRev = 2
		}, domain.KnowledgeRetrievalLimits{}, []string{}},
		{"oversized content", nil, domain.KnowledgeRetrievalLimits{MaxDocumentBytes: 1}, []string{}},
		{"digest mismatch", func(d *domain.KnowledgeDocument) {
			d.ContentDigest = digestOf([]byte("other content"))
		}, domain.KnowledgeRetrievalLimits{}, []string{}},
		{"unsupported provenance", func(d *domain.KnowledgeDocument) {
			d.Provenance = domain.KnowledgeProvenanceCurated
		}, domain.KnowledgeRetrievalLimits{}, []string{}},
	}
	for _, candidate := range unavailable {
		t.Run(candidate.name, func(t *testing.T) {
			document := legacyResolverDocument("legacy", content, topic.ID, revisionRowID, 1)
			if candidate.mutate != nil {
				candidate.mutate(&document)
			}
			_, err := resolver.Resolve(t.Context(), document, candidate.limits)
			if !errors.Is(err, port.ErrKnowledgeUnavailable) {
				t.Fatalf("error = %v, want ErrKnowledgeUnavailable", err)
			}
			for _, secret := range append(candidate.secrets, content, document.ContentDigest, document.ContentHandle) {
				if secret != "" && err != nil && strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaks %q: %v", secret, err)
				}
			}
		})
	}
}

func TestKnowledgeDocumentResolverRejectsInvalidLimits(t *testing.T) {
	store, _ := newTestStore(t)
	topic, revisionRowID := seedGuardedTopic(t, store, "legacy", "Legacy")
	resolver := NewKnowledgeDocumentResolver(store)
	content := "immutable revision content"

	invalid := []struct {
		name   string
		limits domain.KnowledgeRetrievalLimits
	}{
		{"document bytes over hard cap", domain.KnowledgeRetrievalLimits{
			MaxDocumentBytes: domain.HardMaxKnowledgeRetrievalMaxDocumentBytes + 1,
		}},
		{"timeout over hard cap", domain.KnowledgeRetrievalLimits{
			TimeoutSeconds: domain.HardMaxKnowledgeRetrievalTimeoutSeconds + 1,
		}},
		{"max cards over hard cap", domain.KnowledgeRetrievalLimits{
			MaxCards: domain.HardMaxKnowledgeRetrievalMaxCards + 1,
		}},
	}
	for _, candidate := range invalid {
		t.Run(candidate.name, func(t *testing.T) {
			document := legacyResolverDocument("legacy", content, topic.ID, revisionRowID, 1)
			_, err := resolver.Resolve(t.Context(), document, candidate.limits)
			if !errors.Is(err, port.ErrKnowledgeValidation) {
				t.Fatalf("error = %v, want ErrKnowledgeValidation", err)
			}
			if err != nil && strings.Contains(err.Error(), content) {
				t.Fatalf("error leaks content: %v", err)
			}
		})
	}
}

func TestKnowledgeDocumentResolverRejectsInvalidUTF8(t *testing.T) {
	store, _ := newTestStore(t)
	resolver := NewKnowledgeDocumentResolver(store)

	topic, err := store.CreateTopic(t.Context(), "invalid-utf8", "Invalid UTF-8", "desc", nil, "placeholder", "init")
	if err != nil {
		t.Fatal(err)
	}
	invalid := []byte{0xff, 0xfe, 0xfd, 0x00}
	var revisionRowID int64
	var revisionNumber int
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT id, revision_number FROM memory_topic_revisions WHERE topic_id = ? ORDER BY id DESC LIMIT 1`,
		string(topic.ID)).Scan(&revisionRowID, &revisionNumber); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `
		UPDATE memory_topic_revisions SET content = ? WHERE id = ?`, string(invalid), revisionRowID); err != nil {
		t.Fatalf("seed invalid UTF-8 revision: %v", err)
	}
	document := legacyResolverDocument("invalid", string(invalid), topic.ID, revisionRowID, revisionNumber)
	if _, err := resolver.Resolve(t.Context(), document, domain.KnowledgeRetrievalLimits{}); !errors.Is(err, port.ErrKnowledgeUnavailable) {
		t.Fatalf("invalid UTF-8: error = %v, want ErrKnowledgeUnavailable", err)
	}
}
