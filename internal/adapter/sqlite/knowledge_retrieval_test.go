package sqlite

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// seedRetrievalClaim inserts one complete knowledge claim directly so
// retrieval tests control every identity, scope, and validity column
// without the write service.
func seedRetrievalClaim(t *testing.T, store *Store, id, subject, predicate, valueKind, valueText, valueReference, scopeKind, scopeID, status string, validFrom, validUntil int64, revision int) {
	t.Helper()
	if revision < 1 {
		revision = 1
	}
	sourceClass := "root"
	switch scopeKind {
	case "user", "project":
		sourceClass = "human"
	case "workstream":
		sourceClass = "decision"
	}
	var number float64
	var boolean int
	switch valueKind {
	case "number":
		number, _ = strconv.ParseFloat(valueText, 64)
		valueText = ""
	case "boolean":
		if valueText == "true" {
			boolean = 1
		}
		valueText = ""
	case "reference":
		valueText = ""
	default:
		valueReference = ""
	}
	now := time.Now().UTC().UnixNano()
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, value_number,
			value_boolean, value_reference, scope_kind, scope_id, source_class, source_ref, author_id,
			status, valid_from, valid_until, supersedes_id, current_rev, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, '', ?, ?, ?)`,
		id, subject, predicate, valueKind, valueText, number, boolean, valueReference,
		scopeKind, scopeID, sourceClass, "src:"+id, status, validFrom, validUntil, revision, now, now); err != nil {
		t.Fatalf("seed claim %s: %v", id, err)
	}
}

// seedRetrievalPreference inserts one complete preference directly.
func seedRetrievalPreference(t *testing.T, store *Store, ownerKey, key, valueKind, valueText string, status string, revision int) int {
	t.Helper()
	if revision < 1 {
		revision = 1
	}
	var number float64
	var boolean int
	now := time.Now().UTC().UnixNano()
	result, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_preferences (owner_key, key, value_kind, value_text, value_number,
			value_boolean, status, source_ref, current_rev, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ownerKey, key, valueKind, valueText, number, boolean, status, "src:pref:"+key, revision, now, now)
	if err != nil {
		t.Fatalf("seed preference %s: %v", key, err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("preference row id: %v", err)
	}
	return int(rowID)
}

// seedRetrievalDocument inserts one complete document row directly.
func seedRetrievalDocument(t *testing.T, store *Store, id, subject, scopeKind, scopeID, status, provenance string, sourceID string, sourceRev int) {
	t.Helper()
	digest := sha256.Sum256([]byte("content-" + id))
	now := time.Now().UTC().UnixNano()
	if provenance == "" {
		provenance = "curated"
	}
	handle := "memory_topics:" + sourceID + ":revision:" + "1"
	if provenance == "curated" {
		handle = "curated:" + id
	}
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO knowledge_documents (id, subject, scope_kind, scope_id, content_digest, content_handle,
			source_id, source_rev, provenance, status, current_rev, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		id, subject, scopeKind, scopeID, hex.EncodeToString(digest[:]), handle,
		sourceID, sourceRev, provenance, status, now, now); err != nil {
		t.Fatalf("seed document %s: %v", id, err)
	}
}

func retrievalTestBinding(team, actor string, project, workstream string) domain.KnowledgeWriteBinding {
	return domain.KnowledgeWriteBinding{
		Team: team, Actor: actor,
		Conversation: domain.ConversationKey("slack:" + team + ":dm:C00000001"),
		Project:      project, WorkstreamID: workstream,
	}
}

func retrievalTestLimits() domain.KnowledgeRetrievalLimits {
	limits := domain.DefaultKnowledgeRetrievalLimits()
	limits.MaxCandidatesPerChannel = 8
	limits.MaxCards = 8
	return limits
}

func retrievalTestNow() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}
