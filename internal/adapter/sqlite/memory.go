package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.MemoryStore = (*Store)(nil)

const outboxLeaseDuration = 5 * time.Minute

// ApplyMemoryPatch commits all curator operations, their source evidence, and
// the exchange receipt in one transaction. The receipt makes replay after a
// worker crash safe: a committed patch is never applied twice.
func (s *Store) ApplyMemoryPatch(ctx context.Context, patch domain.MemoryPatch, limits domain.MemoryLimits) (bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, fmt.Errorf("begin apply memory patch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO memory_patch_receipts (conversation_key, exchange_ts, applied_at)
		VALUES (?, ?, ?) ON CONFLICT (conversation_key, exchange_ts) DO NOTHING`,
		string(patch.ConversationKey), patch.ExchangeTS, now.UnixNano())
	if err != nil {
		return false, fmt.Errorf("record memory patch receipt: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect memory patch receipt: %w", err)
	}
	if inserted == 0 {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit memory patch replay: %w", err)
		}
		return false, nil
	}
	for _, op := range patch.Operations {
		if err := applyMemoryOp(ctx, tx, patch, op, limits, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit memory patch: %w", err)
	}
	return true, nil
}

func applyMemoryOp(ctx context.Context, tx *sql.Tx, patch domain.MemoryPatch, op domain.MemoryOp, limits domain.MemoryLimits, now time.Time) error {
	switch op.Type {
	case domain.MemoryOpCreateTopic:
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_topics`).Scan(&count); err != nil {
			return fmt.Errorf("count memory topics: %w", err)
		}
		if count >= limits.MaxTopics {
			return fmt.Errorf("memory topic limit of %d reached", limits.MaxTopics)
		}
		id, tags := generateTopicID(), marshalTags(op.Tags)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO memory_topics (id, slug, title, description, status, tags, content, current_rev, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'active', ?, ?, 1, ?, ?)`, id, op.TopicSlug, op.TopicTitle, op.TopicDesc, tags, op.Content, now.UnixNano(), now.UnixNano()); err != nil {
			return fmt.Errorf("create topic %q: %w", op.TopicSlug, err)
		}
		if err := syncFTSInsert(ctx, tx, id, op.TopicTitle, op.TopicDesc, tags, op.Content); err != nil {
			return fmt.Errorf("sync topic search index: %w", err)
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO memory_topic_revisions (topic_id, revision_number, content, change_reason, created_at) VALUES (?, 1, ?, ?, ?)`, id, op.Content, op.ChangeReason, now.UnixNano())
		if err != nil {
			return fmt.Errorf("create topic revision: %w", err)
		}
		revID, _ := result.LastInsertId()
		return addPatchEvidence(ctx, tx, int(revID), patch, domain.EvidenceSource)
	case domain.MemoryOpRevise, domain.MemoryOpCorrect:
		topic, revID, err := topicForPatch(ctx, tx, op.TopicSlug, op.ExpectedRev)
		if err != nil {
			return err
		}
		_ = revID
		newRevID, err := addRevisionTx(ctx, tx, topic, op.Content, op.ChangeReason, now, limits.MaxTopicChars)
		if err != nil {
			return err
		}
		return addPatchEvidence(ctx, tx, newRevID, patch, domain.EvidenceSource)
	case domain.MemoryOpDecide, domain.MemoryOpQuestionAdd, domain.MemoryOpQuestionResolve:
		topic, _, err := topicForPatch(ctx, tx, op.TopicSlug, op.ExpectedRev)
		if err != nil {
			return err
		}
		var content, reason string
		switch op.Type {
		case domain.MemoryOpDecide:
			content = topic.Content + fmt.Sprintf("\n\n## Decision (%s)\n\n%s\n", now.Format(time.RFC3339), op.Decision)
			reason = "record decision: " + truncateMemoryText(op.Decision, 80)
		case domain.MemoryOpQuestionAdd:
			content = topic.Content + fmt.Sprintf("\n\n## Open Question\n\nQ: %s\n\n_Asked: %s_\n", op.Question, now.Format(time.RFC3339))
			reason = "add question: " + truncateMemoryText(op.Question, 80)
		default:
			content = topic.Content + fmt.Sprintf("\n\n## Resolved Question\n\nQ: %s\n\n_Resolved: %s_\n", op.Question, now.Format(time.RFC3339))
			reason = "resolve question: " + truncateMemoryText(op.Question, 80)
		}
		newRevID, err := addRevisionTx(ctx, tx, topic, content, reason, now, limits.MaxTopicChars)
		if err != nil {
			return err
		}
		evidenceType := domain.EvidenceSource
		if op.Type == domain.MemoryOpDecide {
			evidenceType = domain.EvidenceDecision
		}
		return addPatchEvidence(ctx, tx, newRevID, patch, evidenceType)
	case domain.MemoryOpLinkAdd:
		source, revisionID, err := topicForPatch(ctx, tx, op.TopicSlug, op.ExpectedRev)
		if err != nil {
			return err
		}
		target, _, err := topicForPatch(ctx, tx, op.TargetTopicSlug, 0)
		if err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM memory_topic_links WHERE source_topic_id = ? AND target_topic_id = ?)`, string(source.ID), string(target.ID)).Scan(&exists); err != nil {
			return fmt.Errorf("check topic link: %w", err)
		}
		if !exists {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_topic_links`).Scan(&count); err != nil {
				return fmt.Errorf("count topic links: %w", err)
			}
			if count >= limits.MaxLinks {
				return fmt.Errorf("memory link limit of %d reached", limits.MaxLinks)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_topic_links (source_topic_id, target_topic_id, relation, revision_id) VALUES (?, ?, ?, ?) ON CONFLICT(source_topic_id, target_topic_id) DO UPDATE SET relation = excluded.relation, revision_id = excluded.revision_id`, string(source.ID), string(target.ID), op.LinkRelation, revisionID); err != nil {
			return fmt.Errorf("add topic link: %w", err)
		}
		return nil
	case domain.MemoryOpLinkRemove:
		source, _, err := topicForPatch(ctx, tx, op.TopicSlug, op.ExpectedRev)
		if err != nil {
			return err
		}
		target, _, err := topicForPatch(ctx, tx, op.TargetTopicSlug, 0)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_topic_links WHERE source_topic_id = ? AND target_topic_id = ?`, string(source.ID), string(target.ID)); err != nil {
			return fmt.Errorf("remove topic link: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported memory operation %q", op.Type)
	}
}

func topicForPatch(ctx context.Context, tx *sql.Tx, slug string, expectedRev int) (domain.Topic, int, error) {
	var topic domain.Topic
	var id, status, tags string
	var created, updated int64
	err := tx.QueryRowContext(ctx, `SELECT id, title, description, status, tags, content, current_rev, created_at, updated_at FROM memory_topics WHERE slug = ?`, slug).Scan(&id, &topic.Title, &topic.Description, &status, &tags, &topic.Content, &topic.CurrentRev, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Topic{}, 0, fmt.Errorf("topic %q not found", slug)
		}
		return domain.Topic{}, 0, fmt.Errorf("read topic %q: %w", slug, err)
	}
	topic.ID, topic.Slug, topic.Status, topic.Tags = domain.TopicID(id), slug, domain.TopicStatus(status), unmarshalTags(tags)
	if expectedRev > 0 && topic.CurrentRev != expectedRev {
		return domain.Topic{}, 0, fmt.Errorf("stale revision for topic %q: expected rev %d, current rev %d", slug, expectedRev, topic.CurrentRev)
	}
	var revisionID int
	if err := tx.QueryRowContext(ctx, `SELECT id FROM memory_topic_revisions WHERE topic_id = ? AND revision_number = ?`, id, topic.CurrentRev).Scan(&revisionID); err != nil {
		return domain.Topic{}, 0, fmt.Errorf("read current revision for topic %q: %w", slug, err)
	}
	return topic, revisionID, nil
}

func addRevisionTx(ctx context.Context, tx *sql.Tx, topic domain.Topic, content, reason string, now time.Time, maxChars int) (int, error) {
	if err := domain.ValidateTopicContent(content, maxChars); err != nil {
		return 0, err
	}
	newRev := topic.CurrentRev + 1
	result, err := tx.ExecContext(ctx, `INSERT INTO memory_topic_revisions (topic_id, revision_number, content, change_reason, created_at) VALUES (?, ?, ?, ?, ?)`, string(topic.ID), newRev, content, reason, now.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("insert topic revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_topics SET current_rev = ?, content = ?, updated_at = ? WHERE id = ? AND current_rev = ?`, newRev, content, now.UnixNano(), string(topic.ID), topic.CurrentRev); err != nil {
		return 0, fmt.Errorf("update topic revision: %w", err)
	}
	if err := syncFTSUpdate(ctx, tx, topic.ID, content); err != nil {
		return 0, fmt.Errorf("sync topic search index: %w", err)
	}
	id, _ := result.LastInsertId()
	return int(id), nil
}

func addPatchEvidence(ctx context.Context, tx *sql.Tx, revisionID int, patch domain.MemoryPatch, evidenceType domain.EvidenceType) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_evidence (topic_revision, source_key, source_ts, author_id, type) VALUES (?, ?, ?, ?, ?)`, revisionID, string(patch.ConversationKey), patch.ExchangeTS, patch.SourceAuthorID, string(evidenceType)); err != nil {
		return fmt.Errorf("add memory evidence: %w", err)
	}
	return nil
}

func generateTopicID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return "mem_" + hex.EncodeToString(b)
}

func (s *Store) CreateTopic(
	ctx context.Context,
	slug, title, description string,
	tags []string,
	content, changeReason string,
) (domain.Topic, error) {
	if err := domain.ValidateSlug(slug); err != nil {
		return domain.Topic{}, err
	}
	if err := domain.ValidateTopicTitle(title); err != nil {
		return domain.Topic{}, err
	}
	if strings.TrimSpace(content) == "" {
		return domain.Topic{}, errors.New("topic content must not be empty")
	}

	id := generateTopicID()
	now := time.Now().UTC()
	nowNanos := now.UnixNano()
	tagsJSON := marshalTags(tags)

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.Topic{}, fmt.Errorf("begin create topic: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO memory_topics (id, slug, title, description, status, tags, content, current_rev, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'active', ?, ?, 1, ?, ?)`,
		id, slug, title, description, tagsJSON, content, nowNanos, nowNanos,
	)
	if err != nil {
		if isUniqueConstraint(err) {
			return domain.Topic{}, fmt.Errorf("topic slug %q already exists", slug)
		}
		return domain.Topic{}, fmt.Errorf("insert topic: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return domain.Topic{}, errors.New("create topic: no rows inserted")
	}

	if err := syncFTSInsert(ctx, tx, id, title, description, tagsJSON, content); err != nil {
		return domain.Topic{}, fmt.Errorf("sync FTS insert: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_topic_revisions (topic_id, revision_number, content, change_reason, created_at)
		VALUES (?, 1, ?, ?, ?)`,
		id, content, changeReason, nowNanos,
	); err != nil {
		return domain.Topic{}, fmt.Errorf("insert initial revision: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return domain.Topic{}, fmt.Errorf("commit create topic: %w", err)
	}

	return domain.Topic{
		ID: domain.TopicID(id), Slug: slug, Title: title,
		Description: description, Status: domain.TopicStatusActive,
		Tags: tags, CurrentRev: 1, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (s *Store) GetTopic(ctx context.Context, slug string) (domain.Topic, error) {
	return scanTopic(s.db.QueryRowContext(ctx, `
		SELECT id, slug, title, description, status, tags, content, current_rev, created_at, updated_at
		FROM memory_topics WHERE slug = ?`, slug))
}

func (s *Store) GetTopicByID(ctx context.Context, id domain.TopicID) (domain.Topic, *domain.TopicRevision, error) {
	topic, err := scanTopic(s.db.QueryRowContext(ctx, `
		SELECT id, slug, title, description, status, tags, content, current_rev, created_at, updated_at
		FROM memory_topics WHERE id = ?`, string(id)))
	if err != nil {
		return domain.Topic{}, nil, err
	}

	rev, err := scanRevision(s.db.QueryRowContext(ctx, `
		SELECT id, topic_id, revision_number, content, change_reason, created_at
		FROM memory_topic_revisions
		WHERE topic_id = ? AND revision_number = ?
		ORDER BY revision_number DESC LIMIT 1`, string(id), topic.CurrentRev))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.Topic{}, nil, err
	}
	return topic, rev, nil
}

func (s *Store) ListTopics(ctx context.Context) ([]domain.Topic, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, slug, title, description, status, tags, content, current_rev, created_at, updated_at
		FROM memory_topics ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list topics: %w", err)
	}
	defer rows.Close()

	var topics []domain.Topic
	for rows.Next() {
		t, err := scanTopicFromRows(rows)
		if err != nil {
			return nil, err
		}
		topics = append(topics, t)
	}
	return topics, rows.Err()
}

func (s *Store) DeleteTopic(ctx context.Context, id domain.TopicID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete topic: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := deleteFromFTS(ctx, tx, id); err != nil {
		return fmt.Errorf("sync FTS delete: %w", err)
	}

	result, err := tx.ExecContext(ctx, `DELETE FROM memory_topics WHERE id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("delete topic: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("topic %q not found", string(id))
	}

	return tx.Commit()
}

func (s *Store) AddRevision(
	ctx context.Context,
	topicID domain.TopicID, expectedRev int,
	content, changeReason string,
) (domain.TopicRevision, error) {
	if strings.TrimSpace(content) == "" {
		return domain.TopicRevision{}, errors.New("revision content must not be empty")
	}
	now := time.Now().UTC()
	nowNanos := now.UnixNano()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.TopicRevision{}, fmt.Errorf("begin add revision: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var currentRev int
	if err := tx.QueryRowContext(ctx,
		`SELECT current_rev FROM memory_topics WHERE id = ?`, string(topicID),
	).Scan(&currentRev); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.TopicRevision{}, fmt.Errorf("topic %q not found", string(topicID))
		}
		return domain.TopicRevision{}, fmt.Errorf("read topic revision: %w", err)
	}
	if currentRev != expectedRev {
		return domain.TopicRevision{}, fmt.Errorf("stale revision: expected rev %d, current rev %d", expectedRev, currentRev)
	}

	newRev := currentRev + 1
	result, err := tx.ExecContext(ctx, `
		INSERT INTO memory_topic_revisions (topic_id, revision_number, content, change_reason, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		string(topicID), newRev, content, changeReason, nowNanos,
	)
	if err != nil {
		return domain.TopicRevision{}, fmt.Errorf("insert revision: %w", err)
	}
	revID, _ := result.LastInsertId()

	if _, err := tx.ExecContext(ctx, `
		UPDATE memory_topics SET current_rev = ?, content = ?, updated_at = ? WHERE id = ?`,
		newRev, content, nowNanos, string(topicID),
	); err != nil {
		return domain.TopicRevision{}, fmt.Errorf("update topic revision: %w", err)
	}

	if err := syncFTSUpdate(ctx, tx, topicID, content); err != nil {
		return domain.TopicRevision{}, fmt.Errorf("sync FTS update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return domain.TopicRevision{}, fmt.Errorf("commit add revision: %w", err)
	}

	return domain.TopicRevision{
		ID: int(revID), TopicID: topicID, RevisionNumber: newRev,
		Content: content, ChangeReason: changeReason, CreatedAt: now,
	}, nil
}

func (s *Store) AddEvidence(ctx context.Context, revisionID int, sourceKey domain.ConversationKey, sourceTS, authorID string, evidenceType domain.EvidenceType) (int, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_evidence (topic_revision, source_key, source_ts, author_id, type)
		VALUES (?, ?, ?, ?, ?)`,
		revisionID, string(sourceKey), sourceTS, authorID, string(evidenceType),
	)
	if err != nil {
		return 0, fmt.Errorf("insert evidence: %w", err)
	}
	id, _ := result.LastInsertId()
	return int(id), nil
}

func (s *Store) AddEvidenceBatch(ctx context.Context, evidence []domain.Evidence) error {
	if len(evidence) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin evidence batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO memory_evidence (topic_revision, source_key, source_ts, author_id, type)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare evidence insert: %w", err)
	}
	defer stmt.Close()

	for _, e := range evidence {
		if _, err := stmt.ExecContext(ctx, e.TopicRevision, string(e.SourceKey), e.SourceTS, e.AuthorID, string(e.Type)); err != nil {
			return fmt.Errorf("insert evidence: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) GetEvidence(ctx context.Context, topicID domain.TopicID) ([]domain.Evidence, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.topic_revision, e.source_key, e.source_ts, e.author_id, e.type
		FROM memory_evidence e
		JOIN memory_topic_revisions r ON e.topic_revision = r.id
		WHERE r.topic_id = ?
		ORDER BY e.id DESC`, string(topicID))
	if err != nil {
		return nil, fmt.Errorf("query evidence: %w", err)
	}
	defer rows.Close()

	var evidence []domain.Evidence
	for rows.Next() {
		var e domain.Evidence
		var key, evType string
		if err := rows.Scan(&e.ID, &e.TopicRevision, &key, &e.SourceTS, &e.AuthorID, &evType); err != nil {
			return nil, fmt.Errorf("scan evidence: %w", err)
		}
		e.SourceKey = domain.ConversationKey(key)
		e.Type = domain.EvidenceType(evType)
		evidence = append(evidence, e)
	}
	return evidence, rows.Err()
}

func (s *Store) ListRevisions(ctx context.Context, topicID domain.TopicID) ([]domain.TopicRevision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, topic_id, revision_number, content, change_reason, created_at
		FROM memory_topic_revisions
		WHERE topic_id = ?
		ORDER BY revision_number DESC`, string(topicID))
	if err != nil {
		return nil, fmt.Errorf("list revisions: %w", err)
	}
	defer rows.Close()

	var revisions []domain.TopicRevision
	for rows.Next() {
		r, err := scanRevisionFromRows(rows)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, r)
	}
	return revisions, rows.Err()
}

func (s *Store) SearchTopics(ctx context.Context, query string, maxTopics, maxChars int) ([]domain.MemorySnippet, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if maxTopics <= 0 {
		maxTopics = 3
	}
	if maxChars <= 0 {
		maxChars = 2000
	}

	ftsQuery := buildFTSQuery(query)
	if ftsQuery == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.title, t.slug, t.content, t.current_rev, t.updated_at
		FROM memory_topics_fts fts
		JOIN memory_topics t ON t.rowid = fts.rowid
		WHERE memory_topics_fts MATCH ? AND t.status = 'active'
		ORDER BY rank
		LIMIT ?`, ftsQuery, maxTopics+20)
	if err != nil {
		return nil, fmt.Errorf("search topics: %w", err)
	}
	defer rows.Close()

	var snippets []domain.MemorySnippet
	totalChars := 0
	var partial *domain.MemorySnippet
	for rows.Next() {
		var (
			id           string
			title        string
			slug         string
			content      string
			currentRev   int
			updatedNanos int64
		)
		if err := rows.Scan(&id, &title, &slug, &content, &currentRev, &updatedNanos); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		contentLen := utf8.RuneCountInString(content)
		remaining := maxChars - totalChars
		if contentLen > remaining {
			if partial == nil && remaining > 0 {
				value := domain.MemorySnippet{
					TopicID: domain.TopicID(id), Title: title, Slug: slug,
					Content: string([]rune(content)[:remaining]), RevisionNumber: currentRev,
					RevisedAt: time.Unix(0, updatedNanos).UTC(), Source: "curated_memory",
				}
				partial = &value
			}
			continue
		}
		snippets = append(snippets, domain.MemorySnippet{
			TopicID:        domain.TopicID(id),
			Title:          title,
			Slug:           slug,
			Content:        content,
			RevisionNumber: currentRev,
			RevisedAt:      time.Unix(0, updatedNanos).UTC(),
			Source:         "curated_memory",
		})
		totalChars += contentLen
		if len(snippets) == maxTopics {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(snippets) == 0 && partial != nil {
		return []domain.MemorySnippet{*partial}, nil
	}
	return snippets, nil
}

func (s *Store) SearchTopicReferences(ctx context.Context, query string, maxTopics int) ([]domain.TopicReference, error) {
	if strings.TrimSpace(query) == "" || maxTopics <= 0 {
		return nil, nil
	}
	ftsQuery := buildFTSQuery(query)
	if ftsQuery == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.slug, t.title, t.description, t.tags, t.current_rev
		FROM memory_topics_fts fts
		JOIN memory_topics t ON t.rowid = fts.rowid
		WHERE memory_topics_fts MATCH ? AND t.status = 'active'
		ORDER BY rank LIMIT ?`, ftsQuery, maxTopics)
	if err != nil {
		return nil, fmt.Errorf("search topic references: %w", err)
	}
	defer rows.Close()
	var references []domain.TopicReference
	for rows.Next() {
		var reference domain.TopicReference
		var tags string
		if err := rows.Scan(&reference.Slug, &reference.Title, &reference.Description, &tags, &reference.Revision); err != nil {
			return nil, fmt.Errorf("scan topic reference: %w", err)
		}
		reference.Tags = unmarshalTags(tags)
		references = append(references, reference)
	}
	return references, rows.Err()
}

// GetTopicReference selects an entity target by its canonical slug rather than
// relying on a capped full-text search result.
func (s *Store) GetTopicReference(ctx context.Context, slug string) (*domain.TopicReference, error) {
	var reference domain.TopicReference
	var tags string
	err := s.db.QueryRowContext(ctx, `
		SELECT slug, title, description, tags, current_rev
		FROM memory_topics WHERE slug = ?`, slug,
	).Scan(&reference.Slug, &reference.Title, &reference.Description, &tags, &reference.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get topic reference: %w", err)
	}
	reference.Tags = unmarshalTags(tags)
	return &reference, nil
}

func buildFTSQuery(query string) string {
	// FTS5 only receives quoted tokenizer-compatible terms, never raw Slack syntax.
	terms := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	for index, term := range terms {
		terms[index] = `"` + term + `"`
	}
	// Natural-language questions include interrogatives that need not occur in a
	// factual topic. Matching any sanitized term lets entity names recall facts.
	return strings.Join(terms, " OR ")
}

func (s *Store) FindSimilarTopic(ctx context.Context, title string) (*domain.Topic, error) {
	t, err := scanTopic(s.db.QueryRowContext(ctx, `
		SELECT id, slug, title, description, status, tags, content, current_rev, created_at, updated_at
		FROM memory_topics WHERE title = ?`, title))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find similar topic: %w", err)
	}
	return &t, nil
}

func (s *Store) TopicExistsBySlug(ctx context.Context, slug string) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM memory_topics WHERE slug = ?)`, slug,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check topic slug: %w", err)
	}
	return exists, nil
}

func (s *Store) AddTopicLink(ctx context.Context, sourceID, targetID domain.TopicID, relation string, revisionID int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO memory_topic_links (source_topic_id, target_topic_id, relation, revision_id)
		VALUES (?, ?, ?, ?)`,
		string(sourceID), string(targetID), relation, revisionID,
	)
	if err != nil {
		return fmt.Errorf("add topic link: %w", err)
	}
	return nil
}

func (s *Store) RemoveTopicLink(ctx context.Context, sourceID, targetID domain.TopicID) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM memory_topic_links WHERE source_topic_id = ? AND target_topic_id = ?`,
		string(sourceID), string(targetID))
	if err != nil {
		return fmt.Errorf("remove topic link: %w", err)
	}
	return nil
}

func (s *Store) GetTopicLinks(ctx context.Context, topicID domain.TopicID) ([]domain.TopicLink, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_topic_id, target_topic_id, relation, revision_id
		FROM memory_topic_links
		WHERE source_topic_id = ? OR target_topic_id = ?
		ORDER BY revision_id DESC`, string(topicID), string(topicID))
	if err != nil {
		return nil, fmt.Errorf("get topic links: %w", err)
	}
	defer rows.Close()

	var links []domain.TopicLink
	for rows.Next() {
		var l domain.TopicLink
		var src, tgt string
		if err := rows.Scan(&src, &tgt, &l.Relation, &l.RevisionID); err != nil {
			return nil, fmt.Errorf("scan topic link: %w", err)
		}
		l.SourceTopicID = domain.TopicID(src)
		l.TargetTopicID = domain.TopicID(tgt)
		links = append(links, l)
	}
	return links, rows.Err()
}

func (s *Store) EnqueueOutboxItem(ctx context.Context, conversationKey domain.ConversationKey, exchangeTS string) error {
	if strings.TrimSpace(string(conversationKey)) == "" {
		return nil
	}
	now := time.Now().UTC()
	nowNanos := now.UnixNano()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_outbox (conversation_key, exchange_ts, status, attempts, next_attempt, created_at, updated_at)
		VALUES (?, ?, 'pending', 0, ?, ?, ?)`,
		string(conversationKey), exchangeTS, nowNanos, nowNanos, nowNanos,
	)
	if err != nil {
		return fmt.Errorf("enqueue outbox item: %w", err)
	}
	return nil
}

func (s *Store) ClaimNextOutboxItem(ctx context.Context) (*domain.OutboxItem, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("begin claim outbox: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	nowNanos := now.UnixNano()
	leaseUntil := now.Add(outboxLeaseDuration)
	var (
		id              int
		conversationKey string
		exchangeTS      string
		attempts        int
		nextAttempt     int64
		createdNanos    int64
		updatedNanos    int64
		oldLeaseUntil   int64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, conversation_key, exchange_ts, attempts, next_attempt, created_at, updated_at, lease_until
		FROM memory_outbox
		WHERE (status = 'pending' AND next_attempt <= ?) OR (status = 'processing' AND lease_until <= ?)
		ORDER BY CASE WHEN status = 'processing' THEN lease_until ELSE next_attempt END ASC
		LIMIT 1`, nowNanos, nowNanos,
	).Scan(&id, &conversationKey, &exchangeTS, &attempts, &nextAttempt, &createdNanos, &updatedNanos, &oldLeaseUntil)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("select next outbox item: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE memory_outbox SET status = 'processing', attempts = attempts + 1, lease_until = ?, updated_at = ?
		WHERE id = ? AND ((status = 'pending' AND next_attempt <= ?) OR (status = 'processing' AND lease_until = ?))`,
		leaseUntil.UnixNano(), nowNanos, id, nowNanos, oldLeaseUntil,
	); err != nil {
		return nil, fmt.Errorf("claim outbox item: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim outbox: %w", err)
	}

	return &domain.OutboxItem{
		ID: id, ConversationKey: domain.ConversationKey(conversationKey), ExchangeTS: exchangeTS,
		Status:      domain.OutboxStatusProcessing,
		Attempts:    attempts + 1,
		NextAttempt: time.Unix(0, nextAttempt).UTC(),
		LeaseUntil:  leaseUntil,
		CreatedAt:   time.Unix(0, createdNanos).UTC(),
		UpdatedAt:   time.Unix(0, nowNanos).UTC(),
	}, nil
}

func (s *Store) LoadOutboxMessages(ctx context.Context, item *domain.OutboxItem) ([]domain.Message, error) {
	if item == nil || item.ConversationKey == "" {
		return nil, nil
	}
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT source_messages FROM memory_outbox WHERE id = ?`, item.ID).Scan(&encoded)
	if err != nil {
		return nil, fmt.Errorf("load outbox source snapshot: %w", err)
	}
	var snapshot []domain.Message
	if err := json.Unmarshal([]byte(encoded), &snapshot); err != nil {
		return nil, fmt.Errorf("decode outbox source snapshot: %w", err)
	}
	if len(snapshot) > 0 {
		return snapshot, nil
	}
	// Pre-v4 outbox rows have no source snapshot, so retain legacy recovery.
	rows, err := s.db.QueryContext(ctx, `
		WITH exchange AS (
			SELECT id, role, content, user_id, external_ts, created_at
			FROM messages
			WHERE conversation_key = ? AND external_ts = ? AND role = 'assistant'
			ORDER BY id DESC LIMIT 1
		), prior_user AS (
			SELECT m.id, m.role, m.content, m.user_id, m.external_ts, m.created_at
			FROM messages m JOIN exchange e
			ON m.conversation_key = ? AND m.role = 'user'
			AND (m.created_at < e.created_at OR (m.created_at = e.created_at AND m.id < e.id))
			ORDER BY m.created_at DESC, m.id DESC LIMIT 1
		), source AS (
			SELECT id, role, content, user_id, external_ts, created_at FROM exchange
			UNION ALL
			SELECT id, role, content, user_id, external_ts, created_at FROM prior_user
		)
		SELECT role, content, user_id, external_ts, created_at FROM source ORDER BY created_at ASC, id ASC`,
		string(item.ConversationKey), item.ExchangeTS, string(item.ConversationKey))
	if err != nil {
		return nil, fmt.Errorf("load outbox messages: %w", err)
	}
	defer rows.Close()

	var messages []domain.Message
	for rows.Next() {
		var (
			msg          domain.Message
			role         string
			createdNanos int64
		)
		if err := rows.Scan(&role, &msg.Content, &msg.UserID, &msg.ExternalTS, &createdNanos); err != nil {
			return nil, fmt.Errorf("scan outbox message: %w", err)
		}
		msg.Role = domain.Role(role)
		msg.CreatedAt = time.Unix(0, createdNanos).UTC()
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

func (s *Store) CompleteOutboxItem(ctx context.Context, id int, leaseUntil time.Time) error {
	nowNanos := time.Now().UTC().UnixNano()
	result, err := s.db.ExecContext(ctx,
		`UPDATE memory_outbox SET status = 'done', lease_until = 0, updated_at = ? WHERE id = ? AND status = 'processing' AND lease_until = ?`,
		nowNanos, id, leaseUntil.UnixNano())
	if err != nil {
		return fmt.Errorf("complete outbox item: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return errors.New("complete outbox item: lease lost")
	}
	return nil
}

func (s *Store) FailOutboxItem(ctx context.Context, id int, leaseUntil time.Time, reason string) error {
	nowNanos := time.Now().UTC().UnixNano()
	result, err := s.db.ExecContext(ctx,
		`UPDATE memory_outbox SET status = 'failed', last_error = ?, next_attempt = ?, lease_until = 0, updated_at = ? WHERE id = ? AND status = 'processing' AND lease_until = ?`,
		reason, nowNanos, nowNanos, id, leaseUntil.UnixNano())
	if err != nil {
		return fmt.Errorf("fail outbox item: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return errors.New("fail outbox item: lease lost")
	}
	return nil
}

func (s *Store) RetryOutboxItem(ctx context.Context, id int, leaseUntil, nextAttempt time.Time) error {
	nowNanos := time.Now().UTC().UnixNano()
	result, err := s.db.ExecContext(ctx,
		`UPDATE memory_outbox SET status = 'pending', next_attempt = ?, lease_until = 0, updated_at = ? WHERE id = ? AND status = 'processing' AND lease_until = ?`,
		nextAttempt.UnixNano(), nowNanos, id, leaseUntil.UnixNano())
	if err != nil {
		return fmt.Errorf("retry outbox item: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return errors.New("retry outbox item: lease lost")
	}
	return nil
}

// RescheduleOutboxItem returns a leased item to pending without charging a
// retry attempt when no model permit was available to process it.
func (s *Store) RescheduleOutboxItem(ctx context.Context, id int, leaseUntil, nextAttempt time.Time) error {
	nowNanos := time.Now().UTC().UnixNano()
	result, err := s.db.ExecContext(ctx, `
		UPDATE memory_outbox
		SET status = 'pending', attempts = CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END,
			next_attempt = ?, lease_until = 0, updated_at = ?
		WHERE id = ? AND status = 'processing' AND lease_until = ?`,
		nextAttempt.UnixNano(), nowNanos, id, leaseUntil.UnixNano())
	if err != nil {
		return fmt.Errorf("reschedule outbox item: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return errors.New("reschedule outbox item: lease lost")
	}
	return nil
}

func (s *Store) CleanupOutbox(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM memory_outbox WHERE status IN ('done', 'failed') AND updated_at < ?`, before.UnixNano())
	if err != nil {
		return fmt.Errorf("cleanup outbox: %w", err)
	}
	return nil
}

func truncateMemoryText(value string, maxLen int) string {
	if utf8.RuneCountInString(value) <= maxLen {
		return value
	}
	return string([]rune(value)[:maxLen]) + "..."
}

func marshalTags(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(tags)
	return string(b)
}

func unmarshalTags(raw string) []string {
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil
	}
	return tags
}

func scanTopic(row interface{ Scan(dest ...any) error }) (domain.Topic, error) {
	var (
		t            domain.Topic
		id           string
		status       string
		tagsJSON     string
		createdNanos int64
		updatedNanos int64
	)
	if err := row.Scan(&id, &t.Slug, &t.Title, &t.Description, &status, &tagsJSON, &t.Content, &t.CurrentRev, &createdNanos, &updatedNanos); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Topic{}, fmt.Errorf("topic not found: %w", err)
		}
		return domain.Topic{}, fmt.Errorf("scan topic: %w", err)
	}
	t.ID = domain.TopicID(id)
	t.Status = domain.TopicStatus(status)
	t.Tags = unmarshalTags(tagsJSON)
	t.CreatedAt = time.Unix(0, createdNanos).UTC()
	t.UpdatedAt = time.Unix(0, updatedNanos).UTC()
	return t, nil
}

func scanTopicFromRows(rows *sql.Rows) (domain.Topic, error) {
	var (
		t            domain.Topic
		id           string
		status       string
		tagsJSON     string
		createdNanos int64
		updatedNanos int64
	)
	if err := rows.Scan(&id, &t.Slug, &t.Title, &t.Description, &status, &tagsJSON, &t.Content, &t.CurrentRev, &createdNanos, &updatedNanos); err != nil {
		return domain.Topic{}, fmt.Errorf("scan topic: %w", err)
	}
	t.ID = domain.TopicID(id)
	t.Status = domain.TopicStatus(status)
	t.Tags = unmarshalTags(tagsJSON)
	t.CreatedAt = time.Unix(0, createdNanos).UTC()
	t.UpdatedAt = time.Unix(0, updatedNanos).UTC()
	return t, nil
}

func scanRevision(row interface{ Scan(dest ...any) error }) (*domain.TopicRevision, error) {
	var (
		r            domain.TopicRevision
		topicID      string
		createdNanos int64
	)
	if err := row.Scan(&r.ID, &topicID, &r.RevisionNumber, &r.Content, &r.ChangeReason, &createdNanos); err != nil {
		return nil, err
	}
	r.TopicID = domain.TopicID(topicID)
	r.CreatedAt = time.Unix(0, createdNanos).UTC()
	return &r, nil
}

func scanRevisionFromRows(rows *sql.Rows) (domain.TopicRevision, error) {
	var (
		r            domain.TopicRevision
		topicID      string
		createdNanos int64
	)
	if err := rows.Scan(&r.ID, &topicID, &r.RevisionNumber, &r.Content, &r.ChangeReason, &createdNanos); err != nil {
		return domain.TopicRevision{}, fmt.Errorf("scan revision: %w", err)
	}
	r.TopicID = domain.TopicID(topicID)
	r.CreatedAt = time.Unix(0, createdNanos).UTC()
	return r, nil
}

func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func syncFTSInsert(ctx context.Context, tx *sql.Tx, id, title, description, tagsJSON, content string) error {
	var rowid int64
	if err := tx.QueryRowContext(ctx, `SELECT rowid FROM memory_topics WHERE id = ?`, id).Scan(&rowid); err != nil {
		return fmt.Errorf("read rowid: %w", err)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO memory_topics_fts(rowid, title, description, tags, content)
		VALUES (?, ?, ?, ?, ?)`,
		rowid, title, description, tagsJSON, content,
	)
	if err != nil {
		return fmt.Errorf("insert into FTS: %w", err)
	}
	return nil
}

func syncFTSUpdate(ctx context.Context, tx *sql.Tx, id domain.TopicID, content string) error {
	var rowid int64
	var title, description, tagsJSON string
	if err := tx.QueryRowContext(ctx,
		`SELECT rowid, title, description, tags FROM memory_topics WHERE id = ?`, string(id),
	).Scan(&rowid, &title, &description, &tagsJSON); err != nil {
		return fmt.Errorf("read topic for FTS update: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_topics_fts WHERE rowid = ?`, rowid); err != nil {
		return fmt.Errorf("delete from FTS: %w", err)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO memory_topics_fts(rowid, title, description, tags, content)
		VALUES (?, ?, ?, ?, ?)`,
		rowid, title, description, tagsJSON, content,
	)
	if err != nil {
		return fmt.Errorf("insert into FTS: %w", err)
	}
	return nil
}

func deleteFromFTS(ctx context.Context, tx *sql.Tx, id domain.TopicID) error {
	var rowid int64
	if err := tx.QueryRowContext(ctx, `SELECT rowid FROM memory_topics WHERE id = ?`, string(id)).Scan(&rowid); err != nil {
		return fmt.Errorf("read rowid for FTS delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_topics_fts WHERE rowid = ?`, rowid); err != nil {
		return fmt.Errorf("delete from FTS: %w", err)
	}
	return nil
}
