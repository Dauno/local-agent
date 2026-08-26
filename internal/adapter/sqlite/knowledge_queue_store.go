package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var (
	_ port.KnowledgeQueueStore = (*KnowledgeLexicalQueueStore)(nil)
	_ port.KnowledgeQueueStore = (*KnowledgeEmbeddingQueueStore)(nil)
)

// KnowledgeLexicalQueueStore is the generation-CAS queue surface fixed over
// the lexical queue. Claims receive the bounded injected clock and lease;
// every terminal and retry transition presents the exact structural claim
// token (kind, identity, generation, lease identity), so an expired
// claimant can never mutate a reclaimed claim. Errors never carry item
// identities, content, handles, digests, or credentials.
type KnowledgeLexicalQueueStore struct {
	knowledgeQueueStore
}

func NewKnowledgeLexicalQueueStore(store *Store) *KnowledgeLexicalQueueStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &KnowledgeLexicalQueueStore{knowledgeQueueStore{db: store.db, table: "knowledge_lexical_queue"}}
}

// KnowledgeEmbeddingQueueStore is the generation-CAS queue surface fixed
// over the embedding queue. It shares the exact claim, transition, list, and
// enqueue semantics of the lexical queue store; only the backing table
// differs.
type KnowledgeEmbeddingQueueStore struct {
	knowledgeQueueStore
}

func NewKnowledgeEmbeddingQueueStore(store *Store) *KnowledgeEmbeddingQueueStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &KnowledgeEmbeddingQueueStore{knowledgeQueueStore{db: store.db, table: "knowledge_embedding_queue"}}
}

// knowledgeQueueStore is the table-parameterized generation-CAS queue
// implementation shared by the lexical and embedding queue stores.
type knowledgeQueueStore struct {
	db    *sql.DB
	table string
}

const knowledgeQueueColumns = `item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at`

func scanKnowledgeQueueItem(row interface{ Scan(dest ...any) error }) (domain.KnowledgeQueueItem, error) {
	var (
		item                                      domain.KnowledgeQueueItem
		lastError                                 string
		nextAttempt, leaseUntil, created, updated int64
	)
	if err := row.Scan(&item.Kind, &item.ID, &item.Generation, &item.Status, &item.Attempts,
		&nextAttempt, &leaseUntil, &lastError, &created, &updated); err != nil {
		return domain.KnowledgeQueueItem{}, err
	}
	_ = lastError
	if nextAttempt > 0 {
		item.NextAttempt = time.Unix(nextAttempt, 0).UTC()
	}
	if leaseUntil > 0 {
		item.LeaseUntil = time.Unix(leaseUntil, 0).UTC()
	}
	item.CreatedAt = time.Unix(created, 0).UTC()
	item.UpdatedAt = time.Unix(updated, 0).UTC()
	return item, nil
}

// ClaimNext claims one ready pending row or one expired processing row in
// stable identity order, advancing attempts and setting a fresh bounded
// lease atomically. It returns the durable row and the structural claim
// token.
func (s *knowledgeQueueStore) ClaimNext(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, now time.Time, lease time.Duration) (domain.KnowledgeQueueItem, bool, error) {
	if !domain.ValidKnowledgeRetrievalItemKind(kind) {
		return domain.KnowledgeQueueItem{}, false, fmt.Errorf("%w: unknown queue kind", port.ErrKnowledgeValidation)
	}
	if now.IsZero() {
		return domain.KnowledgeQueueItem{}, false, fmt.Errorf("%w: queue claim requires an injected clock", port.ErrKnowledgeValidation)
	}
	if !domain.ValidKnowledgeQueueLease(lease) {
		return domain.KnowledgeQueueItem{}, false, fmt.Errorf("%w: queue claim lease is not bounded", port.ErrKnowledgeValidation)
	}
	nowUnix := now.Unix()
	leaseUnix := now.Add(lease).Unix()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.KnowledgeQueueItem{}, false, fmt.Errorf("%w: begin queue claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	var rowID int64
	err = tx.QueryRowContext(ctx, `
		SELECT rowid FROM `+s.table+`
		WHERE item_kind = ? AND (
			(status = 'pending' AND (next_attempt = 0 OR next_attempt <= ?))
			OR (status = 'processing' AND lease_until > 0 AND lease_until <= ?)
		)
		ORDER BY item_id LIMIT 1`, kind, nowUnix, nowUnix).Scan(&rowID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgeQueueItem{}, false, nil
	}
	if err != nil {
		return domain.KnowledgeQueueItem{}, false, fmt.Errorf("%w: select queue claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE `+s.table+`
		SET status = 'processing', attempts = attempts + 1, lease_until = ?, updated_at = ?
		WHERE rowid = ?`, leaseUnix, nowUnix, rowID)
	if err != nil {
		return domain.KnowledgeQueueItem{}, false, fmt.Errorf("%w: update queue claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return domain.KnowledgeQueueItem{}, false, fmt.Errorf("%w: queue claim row changed", port.ErrKnowledgeUnavailable)
	}
	item, err := scanKnowledgeQueueItem(tx.QueryRowContext(ctx, `
		SELECT `+knowledgeQueueColumns+` FROM `+s.table+` WHERE rowid = ?`, rowID))
	if err != nil {
		return domain.KnowledgeQueueItem{}, false, fmt.Errorf("%w: read queue claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.KnowledgeQueueItem{}, false, fmt.Errorf("%w: commit queue claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	return item, true, nil
}

// validateClaimToken enforces the structural claim token before any
// terminal or retry transition.
func validateClaimToken(claim domain.KnowledgeQueueClaim) error {
	if !domain.ValidKnowledgeRetrievalItemKind(claim.Kind) {
		return fmt.Errorf("%w: unknown claim kind", port.ErrKnowledgeValidation)
	}
	if claim.ID == "" || utf8.RuneCountInString(claim.ID) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return fmt.Errorf("%w: claim identity is not bounded", port.ErrKnowledgeValidation)
	}
	if claim.Generation < 0 {
		return fmt.Errorf("%w: claim generation must be non-negative", port.ErrKnowledgeValidation)
	}
	if claim.LeaseUntil.IsZero() {
		return fmt.Errorf("%w: claim requires its lease identity", port.ErrKnowledgeValidation)
	}
	return nil
}

func (s *knowledgeQueueStore) transition(ctx context.Context, claim domain.KnowledgeQueueClaim, set string, args []any) error {
	if err := validateClaimToken(claim); err != nil {
		return err
	}
	updateArgs := append([]any{}, args...)
	updateArgs = append(updateArgs, claim.Kind, claim.ID, claim.Generation, claim.LeaseUntil.Unix())
	result, err := s.db.ExecContext(ctx, `
		UPDATE `+s.table+`
		SET `+set+`
		WHERE item_kind = ? AND item_id = ? AND generation = ? AND status = 'processing' AND lease_until = ?`,
		updateArgs...)
	if err != nil {
		return fmt.Errorf("%w: queue transition: %v", port.ErrKnowledgeUnavailable, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%w: inspect queue transition: %v", port.ErrKnowledgeUnavailable, err)
	}
	if affected != 1 {
		return port.ErrKnowledgeCASConflict
	}
	return nil
}

// Complete marks the claimed generation done through exact CAS on kind,
// identity, generation, processing status, and lease identity.
func (s *knowledgeQueueStore) Complete(ctx context.Context, claim domain.KnowledgeQueueClaim) error {
	return s.transition(ctx, claim, `
		status = 'done', next_attempt = 0, lease_until = 0, last_error = '', updated_at = strftime('%s', 'now')`, nil)
}

// Retry reschedules the claimed generation as pending at the bounded next
// attempt time through the same CAS.
func (s *knowledgeQueueStore) Retry(ctx context.Context, claim domain.KnowledgeQueueClaim, nextAttempt time.Time) error {
	if nextAttempt.IsZero() {
		return fmt.Errorf("%w: queue retry requires a next attempt time", port.ErrKnowledgeValidation)
	}
	return s.transition(ctx, claim, `
		status = 'pending', next_attempt = ?, lease_until = 0, last_error = '', updated_at = strftime('%s', 'now')`,
		[]any{nextAttempt.Unix()})
}

// Fail marks the claimed generation failed with one of the closed persisted
// failure codes through the same CAS.
func (s *knowledgeQueueStore) Fail(ctx context.Context, claim domain.KnowledgeQueueClaim, code domain.KnowledgeQueueFailureCode) error {
	if !domain.ValidKnowledgeQueueFailureCode(code) {
		return fmt.Errorf("%w: queue failure code is not closed", port.ErrKnowledgeValidation)
	}
	return s.transition(ctx, claim, `
		status = 'failed', next_attempt = 0, lease_until = 0, last_error = ?, updated_at = strftime('%s', 'now')`,
		[]any{string(code)})
}

// List pages queue rows in stable identity order for reconciliation.
func (s *knowledgeQueueStore) List(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, afterID string, limit int) ([]domain.KnowledgeQueueItem, error) {
	if !domain.ValidKnowledgeRetrievalItemKind(kind) {
		return nil, fmt.Errorf("%w: unknown queue kind", port.ErrKnowledgeValidation)
	}
	if !domain.ValidKnowledgeQueueListLimit(limit) {
		return nil, fmt.Errorf("%w: queue list limit is not bounded", port.ErrKnowledgeValidation)
	}
	if afterID != "" && utf8.RuneCountInString(afterID) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return nil, fmt.Errorf("%w: queue page cursor is not bounded", port.ErrKnowledgeValidation)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+knowledgeQueueColumns+` FROM `+s.table+`
		WHERE item_kind = ? AND item_id > ?
		ORDER BY item_id LIMIT ?`, kind, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: list queue rows: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = rows.Close() }()
	var items []domain.KnowledgeQueueItem
	for rows.Next() {
		var (
			item                                      domain.KnowledgeQueueItem
			lastError                                 string
			nextAttempt, leaseUntil, created, updated int64
		)
		if err := rows.Scan(&item.Kind, &item.ID, &item.Generation, &item.Status, &item.Attempts,
			&nextAttempt, &leaseUntil, &lastError, &created, &updated); err != nil {
			return nil, fmt.Errorf("%w: queue row scan: %v", port.ErrKnowledgeUnavailable, err)
		}
		if nextAttempt > 0 {
			item.NextAttempt = time.Unix(nextAttempt, 0).UTC()
		}
		if leaseUntil > 0 {
			item.LeaseUntil = time.Unix(leaseUntil, 0).UTC()
		}
		item.CreatedAt = time.Unix(created, 0).UTC()
		item.UpdatedAt = time.Unix(updated, 0).UTC()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: queue row scan: %v", port.ErrKnowledgeUnavailable, err)
	}
	return items, nil
}

// Enqueue upserts one identity with a fresh generation and zeroed pending
// state. It is used for truth mutations, reconciliation repair, and stale
// hit repair.
func (s *knowledgeQueueStore) Enqueue(ctx context.Context, kind domain.KnowledgeRetrievalItemKind, itemID string) (domain.KnowledgeQueueItem, error) {
	if !domain.ValidKnowledgeRetrievalItemKind(kind) {
		return domain.KnowledgeQueueItem{}, fmt.Errorf("%w: unknown queue kind", port.ErrKnowledgeValidation)
	}
	if itemID == "" || utf8.RuneCountInString(itemID) > domain.HardMaxKnowledgeQueueItemIDRunes {
		return domain.KnowledgeQueueItem{}, fmt.Errorf("%w: queue identity is not bounded", port.ErrKnowledgeValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.KnowledgeQueueItem{}, fmt.Errorf("%w: begin queue enqueue: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO `+s.table+` (item_kind, item_id, generation, status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES (?, ?, 1, 'pending', 0, 0, 0, '', strftime('%s', 'now'), strftime('%s', 'now'))
		ON CONFLICT(item_kind, item_id) DO UPDATE SET
			generation = `+s.table+`.generation + 1,
			status = 'pending', attempts = 0, next_attempt = 0, lease_until = 0,
			last_error = '', updated_at = strftime('%s', 'now')`,
		kind, itemID); err != nil {
		return domain.KnowledgeQueueItem{}, fmt.Errorf("%w: queue enqueue: %v", port.ErrKnowledgeUnavailable, err)
	}
	item, err := s.scanQueueRow(ctx, tx, kind, itemID)
	if err != nil {
		return domain.KnowledgeQueueItem{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.KnowledgeQueueItem{}, fmt.Errorf("%w: commit queue enqueue: %v", port.ErrKnowledgeUnavailable, err)
	}
	return item, nil
}

func (s *knowledgeQueueStore) scanQueueRow(ctx context.Context, tx *sql.Tx, kind domain.KnowledgeRetrievalItemKind, itemID string) (domain.KnowledgeQueueItem, error) {
	var (
		item                                      domain.KnowledgeQueueItem
		lastError                                 string
		nextAttempt, leaseUntil, created, updated int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT `+knowledgeQueueColumns+` FROM `+s.table+`
		WHERE item_kind = ? AND item_id = ?`, kind, itemID).Scan(
		&item.Kind, &item.ID, &item.Generation, &item.Status, &item.Attempts,
		&nextAttempt, &leaseUntil, &lastError, &created, &updated)
	if err != nil {
		return domain.KnowledgeQueueItem{}, fmt.Errorf("%w: read enqueued row: %v", port.ErrKnowledgeUnavailable, err)
	}
	_ = lastError
	if nextAttempt > 0 {
		item.NextAttempt = time.Unix(nextAttempt, 0).UTC()
	}
	if leaseUntil > 0 {
		item.LeaseUntil = time.Unix(leaseUntil, 0).UTC()
	}
	item.CreatedAt = time.Unix(created, 0).UTC()
	item.UpdatedAt = time.Unix(updated, 0).UTC()
	return item, nil
}

// knowledgeQueueDepth counts non-terminal queue rows: pending plus
// processing only, excluding done and failed. It reads metadata columns
// only and never content, identities, or digests.
func knowledgeQueueDepth(ctx context.Context, db *sql.DB, table string) (int, error) {
	if db == nil {
		return 0, errors.New("knowledge queue store is unavailable")
	}
	var depth int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE status IN ('pending','processing')").Scan(&depth); err != nil {
		return 0, err
	}
	return depth, nil
}

// LexicalDepth implements the consumer-owned queue depth surface: pending
// plus processing lexical rows.
func (s *KnowledgeLexicalQueueStore) LexicalDepth(ctx context.Context) (int, error) {
	if s == nil {
		return 0, errors.New("knowledge lexical queue store is unavailable")
	}
	return knowledgeQueueDepth(ctx, s.db, "knowledge_lexical_queue")
}

// EmbeddingDepth implements the consumer-owned queue depth surface: pending
// plus processing embedding rows.
func (s *KnowledgeLexicalQueueStore) EmbeddingDepth(ctx context.Context) (int, error) {
	if s == nil {
		return 0, errors.New("knowledge embedding queue store is unavailable")
	}
	return knowledgeQueueDepth(ctx, s.db, "knowledge_embedding_queue")
}

// LexicalDepth implements the consumer-owned queue depth surface for the
// embedding queue store with the same fixed table semantics.
func (s *KnowledgeEmbeddingQueueStore) LexicalDepth(ctx context.Context) (int, error) {
	if s == nil {
		return 0, errors.New("knowledge lexical queue store is unavailable")
	}
	return knowledgeQueueDepth(ctx, s.db, "knowledge_lexical_queue")
}

// EmbeddingDepth implements the consumer-owned queue depth surface for the
// embedding queue store with the same fixed table semantics.
func (s *KnowledgeEmbeddingQueueStore) EmbeddingDepth(ctx context.Context) (int, error) {
	if s == nil {
		return 0, errors.New("knowledge embedding queue store is unavailable")
	}
	return knowledgeQueueDepth(ctx, s.db, "knowledge_embedding_queue")
}
