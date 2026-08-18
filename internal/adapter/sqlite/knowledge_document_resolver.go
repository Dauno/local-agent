package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// KnowledgeDocumentResolver resolves complete verified legacy curated
// document content against the immutable memory_topic_revisions row named by
// the document handle. It never reads mutable memory_topics.content, never
// follows a different revision, and never returns partial bytes. Invalid
// inputs wrap port.ErrKnowledgeValidation; missing, mismatched, oversized,
// or unverifiable content wraps port.ErrKnowledgeUnavailable. Error text
// never carries handles, digests, content, or credentials.
type KnowledgeDocumentResolver struct {
	db *sql.DB
}

// NewKnowledgeDocumentResolver wires the strict legacy resolver to the
// shared store connection.
func NewKnowledgeDocumentResolver(store *Store) *KnowledgeDocumentResolver {
	return &KnowledgeDocumentResolver{db: store.DB()}
}

func (r *KnowledgeDocumentResolver) Resolve(ctx context.Context, document domain.KnowledgeDocument, limits domain.KnowledgeRetrievalLimits) ([]byte, error) {
	if document.Provenance != domain.KnowledgeProvenanceLegacyCurated {
		return nil, fmt.Errorf("%w: unsupported knowledge document provenance", port.ErrKnowledgeUnavailable)
	}
	bounded := limits.WithDefaults()
	if err := bounded.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	topicID, revisionRowID, err := parseLegacyDocumentHandle(document.ContentHandle)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	if topicID != string(document.SourceID) {
		return nil, fmt.Errorf("%w: knowledge document source identity does not match its handle", port.ErrKnowledgeUnavailable)
	}
	var content []byte
	var revisionNumber int64
	err = r.db.QueryRowContext(ctx, `
		SELECT r.content, r.revision_number
		FROM memory_topic_revisions r
		JOIN memory_topics t ON t.id = r.topic_id
		WHERE r.id = ? AND r.topic_id = ? AND length(CAST(r.content AS BLOB)) <= ?`,
		revisionRowID, topicID, bounded.MaxDocumentBytes).Scan(&content, &revisionNumber)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: knowledge document content is unavailable", port.ErrKnowledgeUnavailable)
		}
		return nil, fmt.Errorf("%w: knowledge document resolution failed", port.ErrKnowledgeUnavailable)
	}
	if int(revisionNumber) != document.SourceRev {
		return nil, fmt.Errorf("%w: knowledge document content is unavailable", port.ErrKnowledgeUnavailable)
	}
	if !utf8.Valid(content) {
		return nil, fmt.Errorf("%w: knowledge document content is unavailable", port.ErrKnowledgeUnavailable)
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != document.ContentDigest {
		return nil, fmt.Errorf("%w: knowledge document content is unavailable", port.ErrKnowledgeUnavailable)
	}
	return content, nil
}

// parseLegacyDocumentHandle accepts exactly
// "memory_topics:<topic-id>:revision:<revision-row-id>" and rejects
// variants, suffixes, empty or colon-bearing topic IDs, leading zeros,
// non-positive row IDs, and int64 overflow.
func parseLegacyDocumentHandle(handle string) (string, int64, error) {
	const prefix = "memory_topics:"
	const marker = ":revision:"
	if !strings.HasPrefix(handle, prefix) {
		return "", 0, errors.New("malformed knowledge document handle")
	}
	rest := strings.TrimPrefix(handle, prefix)
	markerIdx := strings.Index(rest, marker)
	if markerIdx <= 0 {
		return "", 0, errors.New("malformed knowledge document handle")
	}
	topicID := rest[:markerIdx]
	if strings.Contains(topicID, ":") {
		return "", 0, errors.New("malformed knowledge document handle")
	}
	rowPart := rest[markerIdx+len(marker):]
	rowID, ok := parseStrictPositiveDecimal(rowPart)
	if !ok {
		return "", 0, errors.New("malformed knowledge document handle")
	}
	return topicID, rowID, nil
}

// parseStrictPositiveDecimal accepts a canonical positive decimal integer:
// digits only, no leading zeros, no sign, and within int64 range.
func parseStrictPositiveDecimal(value string) (int64, bool) {
	if value == "" || len(value) > 19 {
		return 0, false
	}
	if value[0] == '0' && len(value) > 1 {
		return 0, false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 {
		return 0, false
	}
	return parsed, true
}
