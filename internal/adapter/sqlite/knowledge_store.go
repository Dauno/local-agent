package sqlite

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// knowledgeDocumentResultOwnerKind identifies result_references rows kept
// live by a curated knowledge document's "result:<id>" content handle. The
// reference keeps the referenced result out of retention candidacy for as
// long as the document stays active.
const knowledgeDocumentResultOwnerKind = "knowledge_document"

var _ port.KnowledgeStore = (*KnowledgeStore)(nil)

const knowledgeProjectionLeaseDuration = 5 * time.Minute

type KnowledgeStore struct {
	db *sql.DB
}

func NewKnowledgeStore(store *Store) *KnowledgeStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &KnowledgeStore{db: store.db}
}

func (s *KnowledgeStore) available() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: knowledge store is not configured", port.ErrKnowledgeUnavailable)
	}
	return nil
}

func generateKnowledgeID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return prefix + hex.EncodeToString(b)
}

const (
	knowledgeClaimColumns = `id, subject, predicate, value_kind, value_text, value_number,
		value_boolean, value_reference, scope_kind, scope_id, source_class, source_ref,
		author_id, status, valid_from, valid_until, supersedes_id, current_rev, created_at, updated_at`
	knowledgePreferenceColumns = `id, owner_key, key, value_kind, value_text, value_number,
		value_boolean, status, source_ref, current_rev, created_at, updated_at`
	knowledgeDocumentColumns = `id, subject, scope_kind, scope_id, content_digest, content_handle,
		source_id, source_rev, provenance, status, current_rev, created_at, updated_at`
)

func scanKnowledgeClaim(row interface{ Scan(dest ...any) error }) (domain.KnowledgeClaim, error) {
	var (
		c            domain.KnowledgeClaim
		valueKind    string
		valueNumber  float64
		valueBoolean int
		validFrom, validUntil,
		createdNanos, updatedNanos int64
	)
	err := row.Scan(&c.ID, &c.Subject, &c.Predicate, &valueKind, &c.Value.Text, &valueNumber,
		&valueBoolean, &c.Value.Reference, &c.ScopeKind, &c.ScopeID, &c.SourceClass, &c.SourceRef,
		&c.AuthorID, &c.Status, &validFrom, &validUntil, &c.SupersedesID, &c.Revision,
		&createdNanos, &updatedNanos)
	if err != nil {
		return domain.KnowledgeClaim{}, err
	}
	c.Value.Kind = domain.KnowledgeValueKind(valueKind)
	c.Value.Number = valueNumber
	c.Value.Boolean = valueBoolean == 1
	if validFrom > 0 {
		c.ValidFrom = time.Unix(0, validFrom).UTC()
	}
	if validUntil > 0 {
		c.ValidUntil = time.Unix(0, validUntil).UTC()
	}
	c.CreatedAt = time.Unix(0, createdNanos).UTC()
	c.UpdatedAt = time.Unix(0, updatedNanos).UTC()
	return c, nil
}

func scanKnowledgePreference(row interface{ Scan(dest ...any) error }) (domain.KnowledgePreference, error) {
	var (
		p            domain.KnowledgePreference
		valueKind    string
		valueNumber  float64
		valueBoolean int
		createdNanos int64
		updatedNanos int64
	)
	err := row.Scan(&p.ID, &p.OwnerKey, &p.Key, &valueKind, &p.Value.Text, &valueNumber,
		&valueBoolean, &p.Status, &p.SourceRef, &p.Revision, &createdNanos, &updatedNanos)
	if err != nil {
		return domain.KnowledgePreference{}, err
	}
	p.Value.Kind = domain.KnowledgeValueKind(valueKind)
	p.Value.Number = valueNumber
	p.Value.Boolean = valueBoolean == 1
	p.CreatedAt = time.Unix(0, createdNanos).UTC()
	p.UpdatedAt = time.Unix(0, updatedNanos).UTC()
	return p, nil
}

func scanKnowledgeDocument(row interface{ Scan(dest ...any) error }) (domain.KnowledgeDocument, error) {
	var (
		d                          domain.KnowledgeDocument
		createdNanos, updatedNanos int64
	)
	err := row.Scan(&d.ID, &d.Subject, &d.ScopeKind, &d.ScopeID, &d.ContentDigest, &d.ContentHandle,
		&d.SourceID, &d.SourceRev, &d.Provenance, &d.Status, &d.Revision, &createdNanos, &updatedNanos)
	if err != nil {
		return domain.KnowledgeDocument{}, err
	}
	d.CreatedAt = time.Unix(0, createdNanos).UTC()
	d.UpdatedAt = time.Unix(0, updatedNanos).UTC()
	return d, nil
}

func encodeKnowledgeValue(value domain.KnowledgeValue) (kind, text, reference string, number float64, boolean int) {
	switch value.Kind {
	case domain.KnowledgeValueNumber:
		return string(value.Kind), "", "", value.Number, 0
	case domain.KnowledgeValueBoolean:
		if value.Boolean {
			return string(value.Kind), "", "", 0, 1
		}
		return string(value.Kind), "", "", 0, 0
	case domain.KnowledgeValueReference:
		return string(value.Kind), "", value.Reference, 0, 0
	default:
		return string(domain.KnowledgeValueString), value.Text, "", 0, 0
	}
}

func nanosOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// sameKnowledgeClaimContent compares the immutable write identity of two
// claims, including the status the write committed. Revision, identity, and
// timestamps are excluded because they are assigned by the store.
func sameKnowledgeClaimContent(a, b domain.KnowledgeClaim) bool {
	return a.Subject == b.Subject &&
		a.Predicate == b.Predicate &&
		a.Value.Kind == b.Value.Kind &&
		a.Value.Text == b.Value.Text &&
		a.Value.Number == b.Value.Number &&
		a.Value.Boolean == b.Value.Boolean &&
		a.Value.Reference == b.Value.Reference &&
		a.ScopeKind == b.ScopeKind &&
		a.ScopeID == b.ScopeID &&
		a.SourceClass == b.SourceClass &&
		a.SourceRef == b.SourceRef &&
		a.AuthorID == b.AuthorID &&
		a.Status == b.Status &&
		a.SupersedesID == b.SupersedesID &&
		nanosOrZero(a.ValidFrom) == nanosOrZero(b.ValidFrom) &&
		nanosOrZero(a.ValidUntil) == nanosOrZero(b.ValidUntil)
}

// sameKnowledgePreferenceContent compares the immutable write identity of two
// preferences, including the status the write committed. Revision, identity,
// and timestamps are excluded.
func sameKnowledgePreferenceContent(a, b domain.KnowledgePreference) bool {
	return a.OwnerKey == b.OwnerKey &&
		a.Key == b.Key &&
		a.Value.Kind == b.Value.Kind &&
		a.Value.Text == b.Value.Text &&
		a.Value.Number == b.Value.Number &&
		a.Value.Boolean == b.Value.Boolean &&
		a.Status == b.Status &&
		a.SourceRef == b.SourceRef
}

// sameKnowledgeDocumentContent compares the immutable write identity of two
// documents. Status is included; timestamps are excluded.
func sameKnowledgeDocumentContent(a, b domain.KnowledgeDocument) bool {
	return a.Subject == b.Subject &&
		a.ScopeKind == b.ScopeKind &&
		a.ScopeID == b.ScopeID &&
		a.ContentDigest == b.ContentDigest &&
		a.ContentHandle == b.ContentHandle &&
		a.SourceID == b.SourceID &&
		a.SourceRev == b.SourceRev &&
		a.Provenance == b.Provenance &&
		a.Status == b.Status
}

// sameKnowledgeValue compares two typed values.
func sameKnowledgeValue(a, b domain.KnowledgeValue) bool {
	return a.Kind == b.Kind && a.Text == b.Text && a.Number == b.Number &&
		a.Boolean == b.Boolean && a.Reference == b.Reference
}

func insertKnowledgeProjection(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_projection_outbox (status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('pending', 0, ?, 0, '', ?, ?)`, now.UnixNano(), now.UnixNano(), now.UnixNano()); err != nil {
		return fmt.Errorf("enqueue knowledge projection: %w", err)
	}
	return nil
}

// CreateClaim persists a new claim with its revision-one record. The claim
// must carry empty SupersedesID; corrections use CorrectClaim. Replaying the
// same (subject, scope, source) returns the original committed claim.
func (s *KnowledgeStore) CreateClaim(ctx context.Context, claim domain.KnowledgeClaim, limits domain.KnowledgeLimits) (domain.KnowledgeClaim, error) {
	if err := s.available(); err != nil {
		return domain.KnowledgeClaim{}, err
	}
	if claim.SupersedesID != "" {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: new claims must not carry supersedes_id; use CorrectClaim", port.ErrKnowledgeValidation)
	}
	if err := claim.ValidateWithLimits(limits); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: begin create claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanKnowledgeClaim(tx.QueryRowContext(ctx, `
		SELECT `+knowledgeClaimColumns+` FROM knowledge_claims
		WHERE subject = ? AND scope_kind = ? AND scope_id = ? AND source_ref = ?`,
		claim.Subject, string(claim.ScopeKind), claim.ScopeID, claim.SourceRef))
	if err == nil {
		receiptValue, receiptStatus, receiptErr := scanKnowledgeClaimReceipt(tx.QueryRowContext(ctx, `
			SELECT status, value_kind, value_text, value_number, value_boolean, value_reference
			FROM knowledge_claim_revisions WHERE claim_id = ? AND revision_number = 1`, string(existing.ID)))
		if errors.Is(receiptErr, sql.ErrNoRows) {
			return domain.KnowledgeClaim{}, fmt.Errorf("%w: claim receipt missing for %q", port.ErrKnowledgeNotFound, existing.ID)
		} else if receiptErr != nil {
			return domain.KnowledgeClaim{}, fmt.Errorf("%w: claim receipt lookup: %v", port.ErrKnowledgeUnavailable, receiptErr)
		}
		receipt := existing
		receipt.Status = receiptStatus
		receipt.Value = receiptValue
		if !sameKnowledgeClaimContent(receipt, claim) {
			return domain.KnowledgeClaim{}, fmt.Errorf("%w: replay identity conflict for source %q", port.ErrKnowledgeCASConflict, claim.SourceRef)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return domain.KnowledgeClaim{}, fmt.Errorf("%w: commit create claim replay: %v", port.ErrKnowledgeUnavailable, commitErr)
		}
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: replay lookup: %v", port.ErrKnowledgeUnavailable, err)
	}

	digest := domain.KnowledgeSubjectDigest(claim.Subject, claim.ScopeKind, claim.ScopeID)
	var blocked int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM knowledge_tombstones WHERE subject_digest = ?`, digest).Scan(&blocked); err == nil {
		return domain.KnowledgeClaim{}, domain.ErrKnowledgeTombstoneBlocked
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: inspect tombstone: %v", port.ErrKnowledgeUnavailable, err)
	}

	var count int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM knowledge_claims WHERE subject = ? AND scope_kind = ? AND scope_id = ?`,
		claim.Subject, string(claim.ScopeKind), claim.ScopeID).Scan(&count); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: count subject claims: %v", port.ErrKnowledgeUnavailable, err)
	}
	if count >= limits.WithDefaults().MaxClaimsPerSubject {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: max claims per subject reached", domain.ErrKnowledgeLimitExceeded)
	}

	now := time.Now().UTC()
	claim.ID = domain.KnowledgeClaimID(generateKnowledgeID("kclaim_"))
	claim.Revision = 1
	kind, text, reference, number, boolean := encodeKnowledgeValue(claim.Value)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_claims (`+knowledgeClaimColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(claim.ID), claim.Subject, string(claim.Predicate), kind, text, number, boolean, reference,
		string(claim.ScopeKind), claim.ScopeID, string(claim.SourceClass), claim.SourceRef, claim.AuthorID,
		string(claim.Status), nanosOrZero(claim.ValidFrom), nanosOrZero(claim.ValidUntil), string(claim.SupersedesID),
		claim.Revision, now.UnixNano(), now.UnixNano()); err != nil {
		if isUniqueConstraint(err) {
			return domain.KnowledgeClaim{}, fmt.Errorf("%w: claim identity raced another writer", port.ErrKnowledgeCASConflict)
		}
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: insert claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := insertKnowledgeClaimRevision(ctx, tx, claim, claim.Status, claim.SourceClass, "create", claim.SourceRef, "created from "+claim.SourceRef, now); err != nil {
		return domain.KnowledgeClaim{}, err
	}
	if err := insertKnowledgeProjection(ctx, tx, now); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: commit create claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	claim.CreatedAt = now
	claim.UpdatedAt = now
	return claim, nil
}

func insertKnowledgeClaimRevision(ctx context.Context, tx *sql.Tx, claim domain.KnowledgeClaim, status domain.KnowledgeClaimStatus, sourceClass domain.KnowledgeSourceClass, operation, sourceRef, reason string, now time.Time) error {
	kind, text, reference, number, boolean := encodeKnowledgeValue(claim.Value)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_claim_revisions
			(claim_id, revision_number, subject, predicate, value_kind, value_text, value_number, value_boolean, value_reference, status, source_class, operation, change_reason, source_ref, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(claim.ID), claim.Revision, claim.Subject, string(claim.Predicate), kind, text, number, boolean,
		reference, string(status), string(sourceClass), operation, reason, sourceRef, now.UnixNano()); err != nil {
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: source %q already committed a revision of claim %q", port.ErrKnowledgeCASConflict, sourceRef, claim.ID)
		}
		return fmt.Errorf("%w: insert claim revision: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}

func insertKnowledgePreferenceRevision(ctx context.Context, tx *sql.Tx, preferenceID, revisionNumber int, value domain.KnowledgeValue, status domain.KnowledgePreferenceStatus, sourceRef string, now time.Time) error {
	kind, text, _, number, boolean := encodeKnowledgeValue(value)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_preference_revisions
			(preference_id, revision_number, value_kind, value_text, value_number, value_boolean, status, source_ref, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		preferenceID, revisionNumber, kind, text, number, boolean, string(status), sourceRef, now.UnixNano()); err != nil {
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: source %q already committed a revision of this preference", port.ErrKnowledgeCASConflict, sourceRef)
		}
		return fmt.Errorf("%w: insert preference revision: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}

// scanKnowledgeClaimReceipt loads the immutable revision-one receipt of a
// claim: the value and status the original write committed.
func scanKnowledgeClaimReceipt(row interface{ Scan(dest ...any) error }) (domain.KnowledgeValue, domain.KnowledgeClaimStatus, error) {
	var (
		status    string
		kind      string
		text      string
		reference string
		number    float64
		boolean   int
	)
	if err := row.Scan(&status, &kind, &text, &number, &boolean, &reference); err != nil {
		return domain.KnowledgeValue{}, "", err
	}
	return domain.KnowledgeValue{
		Kind: domain.KnowledgeValueKind(kind), Text: text, Number: number,
		Boolean: boolean == 1, Reference: reference,
	}, domain.KnowledgeClaimStatus(status), nil
}

type knowledgePreferenceReceipt struct {
	Status    domain.KnowledgePreferenceStatus
	Value     domain.KnowledgeValue
	SourceRef string
}

// scanKnowledgePreferenceReceipt loads the immutable revision-one receipt of
// a preference: the value, status, and source the original write committed.
func scanKnowledgePreferenceReceipt(row interface{ Scan(dest ...any) error }) (knowledgePreferenceReceipt, error) {
	var (
		status  string
		kind    string
		text    string
		number  float64
		boolean int
		source  string
	)
	if err := row.Scan(&status, &kind, &text, &number, &boolean, &source); err != nil {
		return knowledgePreferenceReceipt{}, err
	}
	return knowledgePreferenceReceipt{
		Status: domain.KnowledgePreferenceStatus(status),
		Value: domain.KnowledgeValue{
			Kind: domain.KnowledgeValueKind(kind), Text: text, Number: number, Boolean: boolean == 1,
		},
		SourceRef: source,
	}, nil
}

func (s *KnowledgeStore) GetClaim(ctx context.Context, id domain.KnowledgeClaimID, readable []domain.KnowledgeScopeRef) (domain.KnowledgeClaim, error) {
	if err := s.available(); err != nil {
		return domain.KnowledgeClaim{}, err
	}
	filter, args := knowledgeScopeFilter(readable)
	claim, err := scanKnowledgeClaim(s.db.QueryRowContext(ctx, `
		SELECT `+knowledgeClaimColumns+` FROM knowledge_claims WHERE id = ? AND (`+filter+`)`,
		append([]any{string(id)}, args...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgeClaim{}, port.ErrKnowledgeNotFound
	}
	if err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: read claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	return claim, nil
}

// knowledgeScopeFilter renders the closed set of readable scopes as an OR
// predicate. An empty set fails closed and matches no rows.
func knowledgeScopeFilter(scopes []domain.KnowledgeScopeRef) (string, []any) {
	if len(scopes) == 0 {
		return "0 = 1", nil
	}
	var conditions []string
	args := make([]any, 0, len(scopes)*2)
	for _, scope := range scopes {
		conditions = append(conditions, "(scope_kind = ? AND scope_id = ?)")
		args = append(args, string(scope.Kind), scope.ID)
	}
	return strings.Join(conditions, " OR "), args
}

// CorrectClaim atomically admits the replacement and marks the prior claim
// superseded. The prior claim's rows and provenance are preserved; only its
// status changes.
func (s *KnowledgeStore) CorrectClaim(ctx context.Context, replacement domain.KnowledgeClaim, source domain.KnowledgeSourceClass, limits domain.KnowledgeLimits) (domain.KnowledgeClaim, error) {
	if err := s.available(); err != nil {
		return domain.KnowledgeClaim{}, err
	}
	if source != replacement.SourceClass {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: correction source %q must match replacement provenance %q", port.ErrKnowledgeValidation, source, replacement.SourceClass)
	}
	if err := replacement.ValidateWithLimits(limits); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	if source.MaxKnowledgeClaimStatus() != domain.KnowledgeClaimVerified {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: %q cannot supersede", domain.ErrKnowledgeSourceCannotVerify, source)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: begin correction: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	prior, err := scanKnowledgeClaim(tx.QueryRowContext(ctx, `
		SELECT `+knowledgeClaimColumns+` FROM knowledge_claims WHERE id = ?`, string(replacement.SupersedesID)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: prior claim %q", port.ErrKnowledgeNotFound, replacement.SupersedesID)
	}
	if err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: read prior claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := domain.ValidateKnowledgeSupersession(replacement, prior); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	if prior.Status == domain.KnowledgeClaimSuperseded {
		committed, scanErr := scanKnowledgeClaim(tx.QueryRowContext(ctx, `
			SELECT `+knowledgeClaimColumns+` FROM knowledge_claims
			WHERE supersedes_id = ? AND source_ref = ?`,
			string(prior.ID), replacement.SourceRef))
		if scanErr != nil {
			return domain.KnowledgeClaim{}, fmt.Errorf("%w: correction replay identity mismatch", port.ErrKnowledgeCASConflict)
		}
		receiptValue, receiptStatus, receiptErr := scanKnowledgeClaimReceipt(tx.QueryRowContext(ctx, `
			SELECT status, value_kind, value_text, value_number, value_boolean, value_reference
			FROM knowledge_claim_revisions WHERE claim_id = ? AND revision_number = 1`, string(committed.ID)))
		if errors.Is(receiptErr, sql.ErrNoRows) {
			return domain.KnowledgeClaim{}, fmt.Errorf("%w: claim receipt missing for %q", port.ErrKnowledgeNotFound, committed.ID)
		} else if receiptErr != nil {
			return domain.KnowledgeClaim{}, fmt.Errorf("%w: claim receipt lookup: %v", port.ErrKnowledgeUnavailable, receiptErr)
		}
		receipt := committed
		receipt.Status = receiptStatus
		receipt.Value = receiptValue
		if !sameKnowledgeClaimContent(receipt, replacement) {
			return domain.KnowledgeClaim{}, fmt.Errorf("%w: correction replay identity mismatch for source %q", port.ErrKnowledgeCASConflict, replacement.SourceRef)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return domain.KnowledgeClaim{}, fmt.Errorf("%w: commit correction replay: %v", port.ErrKnowledgeUnavailable, commitErr)
		}
		return committed, nil
	}
	if prior.Status.Terminal() {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: prior claim status %q is terminal", port.ErrKnowledgeValidation, prior.Status)
	}
	var sourceUsed int
	if err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM knowledge_claim_revisions WHERE claim_id = ? AND source_ref = ?`,
		string(prior.ID), replacement.SourceRef).Scan(&sourceUsed); err == nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: source %q already committed a revision of the prior claim", port.ErrKnowledgeCASConflict, replacement.SourceRef)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: prior revision lookup: %v", port.ErrKnowledgeUnavailable, err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM knowledge_claims WHERE subject = ? AND scope_kind = ? AND scope_id = ?`,
		replacement.Subject, string(replacement.ScopeKind), replacement.ScopeID).Scan(&count); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: count subject claims: %v", port.ErrKnowledgeUnavailable, err)
	}
	if count >= limits.WithDefaults().MaxClaimsPerSubject {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: max claims per subject reached", domain.ErrKnowledgeLimitExceeded)
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		UPDATE knowledge_claims SET status = 'superseded', current_rev = current_rev + 1, updated_at = ?
		WHERE id = ? AND current_rev = ? AND status NOT IN ('superseded', 'archived')`,
		now.UnixNano(), string(prior.ID), prior.Revision)
	if err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: supersede prior claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return domain.KnowledgeClaim{}, port.ErrKnowledgeCASConflict
	}
	superseded := prior
	superseded.Status = domain.KnowledgeClaimSuperseded
	superseded.Revision = prior.Revision + 1
	if err := insertKnowledgeClaimRevision(ctx, tx, superseded, domain.KnowledgeClaimSuperseded, replacement.SourceClass, "supersede", replacement.SourceRef, "superseded by correction", now); err != nil {
		return domain.KnowledgeClaim{}, err
	}

	replacement.ID = domain.KnowledgeClaimID(generateKnowledgeID("kclaim_"))
	replacement.Revision = 1
	kind, text, reference, number, boolean := encodeKnowledgeValue(replacement.Value)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_claims (`+knowledgeClaimColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(replacement.ID), replacement.Subject, string(replacement.Predicate), kind, text, number, boolean, reference,
		string(replacement.ScopeKind), replacement.ScopeID, string(replacement.SourceClass), replacement.SourceRef,
		replacement.AuthorID, string(replacement.Status), nanosOrZero(replacement.ValidFrom), nanosOrZero(replacement.ValidUntil),
		string(replacement.SupersedesID), replacement.Revision, now.UnixNano(), now.UnixNano()); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: insert replacement claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := insertKnowledgeClaimRevision(ctx, tx, replacement, replacement.Status, replacement.SourceClass, "create", replacement.SourceRef, "correction supersedes "+string(prior.ID), now); err != nil {
		return domain.KnowledgeClaim{}, err
	}
	if err := insertKnowledgeProjection(ctx, tx, now); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: commit correction: %v", port.ErrKnowledgeUnavailable, err)
	}
	replacement.CreatedAt = now
	replacement.UpdatedAt = now
	return replacement, nil
}

// TransitionClaimStatus applies an explicit status change under revision CAS.
// Every transition advances the claim revision and appends an immutable
// revision row recording the transition operation, its new status, authority
// class, and command identity. The command identity is a receipt: an exact
// retry of an already committed transition — same operation, same authority
// class, same target status — returns the current claim without advancing it,
// while the same source reference committing any other command (creation,
// supersession, a different transition, or a different authority class) is
// rejected. Supersession is rejected here; it requires a validated
// replacement.
func (s *KnowledgeStore) TransitionClaimStatus(ctx context.Context, id domain.KnowledgeClaimID, expectedRev int, next domain.KnowledgeClaimStatus, source domain.KnowledgeSourceClass, sourceRef string) (domain.KnowledgeClaim, error) {
	if err := s.available(); err != nil {
		return domain.KnowledgeClaim{}, err
	}
	if err := domain.ValidateKnowledgeSourceRef(sourceRef); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: begin status transition: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	claim, err := scanKnowledgeClaim(tx.QueryRowContext(ctx, `
		SELECT `+knowledgeClaimColumns+` FROM knowledge_claims WHERE id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgeClaim{}, port.ErrKnowledgeNotFound
	}
	if err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: read claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	var committedOperation, committedClass, committedStatus string
	revisionErr := tx.QueryRowContext(ctx, `
		SELECT operation, source_class, status FROM knowledge_claim_revisions
		WHERE claim_id = ? AND source_ref = ?`, string(id), sourceRef).Scan(&committedOperation, &committedClass, &committedStatus)
	if revisionErr == nil {
		if committedOperation != "transition" ||
			domain.KnowledgeSourceClass(committedClass) != source ||
			domain.KnowledgeClaimStatus(committedStatus) != next {
			return domain.KnowledgeClaim{}, fmt.Errorf("%w: source %q already committed a different command for claim %q", port.ErrKnowledgeCASConflict, sourceRef, id)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return domain.KnowledgeClaim{}, fmt.Errorf("%w: commit status transition replay: %v", port.ErrKnowledgeUnavailable, commitErr)
		}
		return claim, nil
	} else if !errors.Is(revisionErr, sql.ErrNoRows) {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: transition receipt lookup: %v", port.ErrKnowledgeUnavailable, revisionErr)
	}
	if err := claim.TransitionStatus(next, source); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		UPDATE knowledge_claims SET status = ?, current_rev = current_rev + 1, updated_at = ?
		WHERE id = ? AND current_rev = ? AND status NOT IN ('superseded', 'archived')`,
		string(next), now.UnixNano(), string(id), expectedRev)
	if err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: transition status: %v", port.ErrKnowledgeUnavailable, err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return domain.KnowledgeClaim{}, port.ErrKnowledgeCASConflict
	}
	claim.Status = next
	claim.Revision = expectedRev + 1
	if err := insertKnowledgeClaimRevision(ctx, tx, claim, next, source, "transition", sourceRef, "status transition to "+string(next), now); err != nil {
		return domain.KnowledgeClaim{}, err
	}
	if err := insertKnowledgeProjection(ctx, tx, now); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.KnowledgeClaim{}, fmt.Errorf("%w: commit status transition: %v", port.ErrKnowledgeUnavailable, err)
	}
	claim.UpdatedAt = now
	return claim, nil
}

// ForgetSubject deletes all content-bearing rows for the subject and scope
// and inserts a content-free tombstone that blocks future writes and replays.
// Content deletion runs on every call, including replays, so a row created
// through any path cannot survive a repeated forget. Subject and scope bounds
// follow the hard maxima, not the current configured limits, so any value
// historically persistible can always be forgotten. It returns true when the
// tombstone was newly committed.
func (s *KnowledgeStore) ForgetSubject(ctx context.Context, subject string, scopeKind domain.KnowledgeScopeKind, scopeID, sourceRef string) (bool, error) {
	if err := s.available(); err != nil {
		return false, err
	}
	if err := domain.ValidateKnowledgeScope(scopeKind, scopeID, domain.KnowledgeLimits{MaxScopeIDRunes: domain.HardMaxKnowledgeScopeIDRunes}); err != nil {
		return false, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	if strings.TrimSpace(subject) == "" {
		return false, fmt.Errorf("%w: forget requires a subject", port.ErrKnowledgeValidation)
	}
	if utf8.RuneCountInString(subject) > domain.HardMaxKnowledgeSubjectRunes {
		return false, fmt.Errorf("%w: subject exceeds hard maximum of %d characters", port.ErrKnowledgeValidation, domain.HardMaxKnowledgeSubjectRunes)
	}
	if err := domain.ValidateKnowledgeText(subject); err != nil {
		return false, fmt.Errorf("%w: subject: %v", port.ErrKnowledgeValidation, err)
	}
	if err := domain.ValidateKnowledgeSourceRef(sourceRef); err != nil {
		return false, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, fmt.Errorf("%w: begin forget: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	digest := domain.KnowledgeSubjectDigest(subject, scopeKind, scopeID)
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_tombstones (subject_digest, scope_kind, scope_id, forgotten_at, source_ref)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (subject_digest) DO NOTHING`,
		digest, string(scopeKind), scopeID, now.UnixNano(), sourceRef)
	if err != nil {
		return false, fmt.Errorf("%w: insert tombstone: %v", port.ErrKnowledgeUnavailable, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%w: inspect tombstone insert: %v", port.ErrKnowledgeUnavailable, err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM knowledge_claims WHERE subject = ? AND scope_kind = ? AND scope_id = ?`,
		subject, string(scopeKind), scopeID); err != nil {
		return false, fmt.Errorf("%w: delete forgotten claims: %v", port.ErrKnowledgeUnavailable, err)
	}
	forgottenDocIDs, err := tx.QueryContext(ctx, `
		SELECT id FROM knowledge_documents WHERE subject = ? AND scope_kind = ? AND scope_id = ?`,
		subject, string(scopeKind), scopeID)
	if err != nil {
		return false, fmt.Errorf("%w: list forgotten documents: %v", port.ErrKnowledgeUnavailable, err)
	}
	var docIDs []string
	for forgottenDocIDs.Next() {
		var docID string
		if scanErr := forgottenDocIDs.Scan(&docID); scanErr != nil {
			forgottenDocIDs.Close()
			return false, fmt.Errorf("%w: scan forgotten document: %v", port.ErrKnowledgeUnavailable, scanErr)
		}
		docIDs = append(docIDs, docID)
	}
	if err := forgottenDocIDs.Err(); err != nil {
		forgottenDocIDs.Close()
		return false, fmt.Errorf("%w: list forgotten documents: %v", port.ErrKnowledgeUnavailable, err)
	}
	forgottenDocIDs.Close()
	for _, docID := range docIDs {
		if err := releaseCuratedDocumentResult(ctx, tx, docID, now); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM knowledge_documents WHERE subject = ? AND scope_kind = ? AND scope_id = ?`,
		subject, string(scopeKind), scopeID); err != nil {
		return false, fmt.Errorf("%w: delete forgotten documents: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := insertKnowledgeProjection(ctx, tx, now); err != nil {
		return false, fmt.Errorf("%w: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%w: commit forget: %v", port.ErrKnowledgeUnavailable, err)
	}
	return inserted == 1, nil
}

func (s *KnowledgeStore) AddEvidence(ctx context.Context, claimID domain.KnowledgeClaimID, revisionNumber int, evidence domain.KnowledgeEvidence) error {
	if err := s.available(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("%w: begin add evidence: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()
	var revisionID int
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM knowledge_claim_revisions WHERE claim_id = ? AND revision_number = ?`,
		string(claimID), revisionNumber).Scan(&revisionID); errors.Is(err, sql.ErrNoRows) {
		return port.ErrKnowledgeNotFound
	} else if err != nil {
		return fmt.Errorf("%w: inspect claim revision: %v", port.ErrKnowledgeUnavailable, err)
	}
	evidence.ClaimRevision = revisionID
	if err := evidence.Validate(); err != nil {
		return fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	var storedKind, storedAuthor string
	lookupErr := tx.QueryRowContext(ctx, `
		SELECT kind, author_id FROM knowledge_evidence
		WHERE claim_revision = ? AND conversation_key = ? AND exchange_ts = ?`,
		revisionID, string(evidence.ConversationKey), evidence.ExchangeTS).Scan(&storedKind, &storedAuthor)
	if lookupErr == nil {
		if storedKind != string(evidence.Kind) || storedAuthor != evidence.AuthorID {
			return fmt.Errorf("%w: evidence replay provenance mismatch for exchange %q", port.ErrKnowledgeValidation, evidence.ExchangeTS)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("%w: commit evidence replay: %v", port.ErrKnowledgeUnavailable, commitErr)
		}
		return nil
	} else if !errors.Is(lookupErr, sql.ErrNoRows) {
		return fmt.Errorf("%w: evidence replay lookup: %v", port.ErrKnowledgeUnavailable, lookupErr)
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_evidence (claim_revision, conversation_key, exchange_ts, author_id, kind)
		VALUES (?, ?, ?, ?, ?)`,
		revisionID, string(evidence.ConversationKey), evidence.ExchangeTS, evidence.AuthorID, string(evidence.Kind)); err != nil {
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: evidence identity raced another writer", port.ErrKnowledgeCASConflict)
		}
		return fmt.Errorf("%w: insert evidence: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := insertKnowledgeProjection(ctx, tx, now); err != nil {
		return fmt.Errorf("%w: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit evidence: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}

func (s *KnowledgeStore) CreatePreference(ctx context.Context, preference domain.KnowledgePreference, limits domain.KnowledgeLimits) (domain.KnowledgePreference, error) {
	if err := s.available(); err != nil {
		return domain.KnowledgePreference{}, err
	}
	if err := preference.ValidateWithLimits(limits); err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: begin create preference: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := scanKnowledgePreference(tx.QueryRowContext(ctx, `
		SELECT `+knowledgePreferenceColumns+` FROM knowledge_preferences
		WHERE owner_key = ? AND key = ?`, preference.OwnerKey, preference.Key))
	if err == nil {
		receipt, receiptErr := scanKnowledgePreferenceReceipt(tx.QueryRowContext(ctx, `
			SELECT status, value_kind, value_text, value_number, value_boolean, source_ref
			FROM knowledge_preference_revisions WHERE preference_id = ? AND revision_number = 1`, existing.ID))
		if errors.Is(receiptErr, sql.ErrNoRows) {
			return domain.KnowledgePreference{}, fmt.Errorf("%w: preference receipt missing for %d", port.ErrKnowledgeNotFound, existing.ID)
		} else if receiptErr != nil {
			return domain.KnowledgePreference{}, fmt.Errorf("%w: preference receipt lookup: %v", port.ErrKnowledgeUnavailable, receiptErr)
		}
		creationReceipt := existing
		creationReceipt.Status = receipt.Status
		creationReceipt.Value = receipt.Value
		creationReceipt.SourceRef = receipt.SourceRef
		if !sameKnowledgePreferenceContent(creationReceipt, preference) {
			return domain.KnowledgePreference{}, fmt.Errorf("%w: preference replay identity conflict for key %q", port.ErrKnowledgeCASConflict, preference.Key)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return domain.KnowledgePreference{}, fmt.Errorf("%w: commit create preference replay: %v", port.ErrKnowledgeUnavailable, commitErr)
		}
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: preference replay lookup: %v", port.ErrKnowledgeUnavailable, err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_preferences WHERE owner_key = ?`, preference.OwnerKey).Scan(&count); err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: count owner preferences: %v", port.ErrKnowledgeUnavailable, err)
	}
	if count >= limits.WithDefaults().MaxPreferences {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: max preferences per owner reached", domain.ErrKnowledgeLimitExceeded)
	}
	now := time.Now().UTC()
	preference.Revision = 1
	kind, text, _, number, boolean := encodeKnowledgeValue(preference.Value)
	result, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_preferences (`+knowledgePreferenceColumns+`)
		VALUES (NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		preference.OwnerKey, preference.Key, kind, text, number, boolean,
		string(preference.Status), preference.SourceRef, preference.Revision, now.UnixNano(), now.UnixNano())
	if err != nil {
		if isUniqueConstraint(err) {
			return domain.KnowledgePreference{}, fmt.Errorf("%w: preference identity raced another writer", port.ErrKnowledgeCASConflict)
		}
		return domain.KnowledgePreference{}, fmt.Errorf("%w: insert preference: %v", port.ErrKnowledgeUnavailable, err)
	}
	preferenceID, err := result.LastInsertId()
	if err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: inspect preference insert: %v", port.ErrKnowledgeUnavailable, err)
	}
	preference.ID = int(preferenceID)
	if err := insertKnowledgePreferenceRevision(ctx, tx, preference.ID, 1, preference.Value, preference.Status, preference.SourceRef, now); err != nil {
		return domain.KnowledgePreference{}, err
	}
	if err := insertKnowledgeProjection(ctx, tx, now); err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: commit create preference: %v", port.ErrKnowledgeUnavailable, err)
	}
	preference.CreatedAt = now
	preference.UpdatedAt = now
	return preference, nil
}

// UpdatePreference corrects an active preference under revision CAS. Every
// change advances the effective revision, appends an immutable revision row,
// and records the new source reference. A source reference may commit only
// one revision: replaying an earlier update returns the current row even if
// the preference was archived afterwards, and the same source reference with
// different content or a different command is rejected. Owner and key are
// immutable.
func (s *KnowledgeStore) UpdatePreference(ctx context.Context, preference domain.KnowledgePreference, expectedRev int, limits domain.KnowledgeLimits) (domain.KnowledgePreference, error) {
	if err := s.available(); err != nil {
		return domain.KnowledgePreference{}, err
	}
	if expectedRev <= 0 {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: expected revision must be positive", port.ErrKnowledgeValidation)
	}
	if preference.Status != domain.KnowledgePreferenceActive {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: preference correction must target an active preference", port.ErrKnowledgeValidation)
	}
	if err := preference.ValidateWithLimits(limits); err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: begin update preference: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := scanKnowledgePreference(tx.QueryRowContext(ctx, `
		SELECT `+knowledgePreferenceColumns+` FROM knowledge_preferences
		WHERE owner_key = ? AND key = ?`, preference.OwnerKey, preference.Key))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgePreference{}, port.ErrKnowledgeNotFound
	}
	if err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: read preference: %v", port.ErrKnowledgeUnavailable, err)
	}
	var committedRev int
	var committedKind, committedText, committedStatus, committedRef string
	var committedNumber float64
	var committedBoolean int
	revisionErr := tx.QueryRowContext(ctx, `
		SELECT revision_number, value_kind, value_text, value_number, value_boolean, status, source_ref
		FROM knowledge_preference_revisions
		WHERE preference_id = ? AND source_ref = ?`,
		existing.ID, preference.SourceRef).Scan(&committedRev, &committedKind, &committedText, &committedNumber, &committedBoolean, &committedStatus, &committedRef)
	if revisionErr == nil {
		if committedRev == 1 || committedStatus != string(domain.KnowledgePreferenceActive) {
			return domain.KnowledgePreference{}, fmt.Errorf("%w: source %q already committed a different command for this preference", port.ErrKnowledgeCASConflict, preference.SourceRef)
		}
		committed := domain.KnowledgeValue{
			Kind: domain.KnowledgeValueKind(committedKind), Text: committedText,
			Number: committedNumber, Boolean: committedBoolean == 1,
		}
		if !sameKnowledgeValue(committed, preference.Value) {
			return domain.KnowledgePreference{}, fmt.Errorf("%w: source %q already committed different preference content", port.ErrKnowledgeCASConflict, preference.SourceRef)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return domain.KnowledgePreference{}, fmt.Errorf("%w: commit update preference replay: %v", port.ErrKnowledgeUnavailable, commitErr)
		}
		return existing, nil
	} else if !errors.Is(revisionErr, sql.ErrNoRows) {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: preference revision lookup: %v", port.ErrKnowledgeUnavailable, revisionErr)
	}
	if existing.Status != domain.KnowledgePreferenceActive {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: preference %q is archived", port.ErrKnowledgeCASConflict, preference.Key)
	}
	now := time.Now().UTC()
	kind, text, _, number, boolean := encodeKnowledgeValue(preference.Value)
	result, err := tx.ExecContext(ctx, `
		UPDATE knowledge_preferences
		SET value_kind = ?, value_text = ?, value_number = ?, value_boolean = ?,
			source_ref = ?, current_rev = current_rev + 1, updated_at = ?
		WHERE owner_key = ? AND key = ? AND current_rev = ? AND status = 'active'`,
		kind, text, number, boolean, preference.SourceRef, now.UnixNano(),
		preference.OwnerKey, preference.Key, expectedRev)
	if err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: update preference: %v", port.ErrKnowledgeUnavailable, err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return domain.KnowledgePreference{}, port.ErrKnowledgeCASConflict
	}
	if err := insertKnowledgePreferenceRevision(ctx, tx, existing.ID, expectedRev+1, preference.Value, domain.KnowledgePreferenceActive, preference.SourceRef, now); err != nil {
		return domain.KnowledgePreference{}, err
	}
	if err := insertKnowledgeProjection(ctx, tx, now); err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: commit update preference: %v", port.ErrKnowledgeUnavailable, err)
	}
	return s.GetPreference(ctx, preference.OwnerKey, preference.Key)
}

func (s *KnowledgeStore) GetPreference(ctx context.Context, ownerKey, key string) (domain.KnowledgePreference, error) {
	if err := s.available(); err != nil {
		return domain.KnowledgePreference{}, err
	}
	preference, err := scanKnowledgePreference(s.db.QueryRowContext(ctx, `
		SELECT `+knowledgePreferenceColumns+` FROM knowledge_preferences WHERE owner_key = ? AND key = ?`, ownerKey, key))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgePreference{}, port.ErrKnowledgeNotFound
	}
	if err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: read preference: %v", port.ErrKnowledgeUnavailable, err)
	}
	return preference, nil
}

func (s *KnowledgeStore) ListPreferencesForOwner(ctx context.Context, ownerKey string, limits domain.KnowledgeLimits) ([]domain.KnowledgePreference, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if ownerKey == "" {
		return nil, fmt.Errorf("%w: preference owner must not be empty", port.ErrKnowledgeValidation)
	}
	maxRows := limits.WithDefaults().MaxPreferences + 1
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+knowledgePreferenceColumns+` FROM knowledge_preferences
		WHERE owner_key = ? AND status = 'active' ORDER BY updated_at DESC LIMIT ?`, ownerKey, maxRows)
	if err != nil {
		return nil, fmt.Errorf("%w: list preferences: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer rows.Close()
	var preferences []domain.KnowledgePreference
	for rows.Next() {
		preference, scanErr := scanKnowledgePreference(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: scan preference: %v", port.ErrKnowledgeUnavailable, scanErr)
		}
		preferences = append(preferences, preference)
	}
	return preferences, rows.Err()
}

// ArchivePreference marks an active preference archived under revision CAS.
// Archiving advances the effective revision and appends an immutable
// revision row recording the archived status and the archive command
// identity. The command identity is a receipt: replaying the archive returns
// the archived preference without inspecting expectedRev, while an archive
// attempted with any other source reference after archiving is rejected.
func (s *KnowledgeStore) ArchivePreference(ctx context.Context, ownerKey, key string, expectedRev int, sourceRef string) (domain.KnowledgePreference, error) {
	if err := s.available(); err != nil {
		return domain.KnowledgePreference{}, err
	}
	if err := domain.ValidateKnowledgeSourceRef(sourceRef); err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: begin archive preference: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := scanKnowledgePreference(tx.QueryRowContext(ctx, `
		SELECT `+knowledgePreferenceColumns+` FROM knowledge_preferences
		WHERE owner_key = ? AND key = ?`, ownerKey, key))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgePreference{}, port.ErrKnowledgeNotFound
	}
	if err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: read preference: %v", port.ErrKnowledgeUnavailable, err)
	}
	var committedRev int
	var committedStatus string
	revisionErr := tx.QueryRowContext(ctx, `
		SELECT revision_number, status FROM knowledge_preference_revisions
		WHERE preference_id = ? AND source_ref = ?`,
		existing.ID, sourceRef).Scan(&committedRev, &committedStatus)
	if revisionErr == nil {
		if committedRev == 1 || committedStatus != string(domain.KnowledgePreferenceArchived) {
			return domain.KnowledgePreference{}, fmt.Errorf("%w: source %q already committed a different command for this preference", port.ErrKnowledgeCASConflict, sourceRef)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return domain.KnowledgePreference{}, fmt.Errorf("%w: commit archive replay: %v", port.ErrKnowledgeUnavailable, commitErr)
		}
		return existing, nil
	} else if !errors.Is(revisionErr, sql.ErrNoRows) {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: archive receipt lookup: %v", port.ErrKnowledgeUnavailable, revisionErr)
	}
	if existing.Status == domain.KnowledgePreferenceArchived {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: preference %q was archived by a different source", port.ErrKnowledgeCASConflict, key)
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		UPDATE knowledge_preferences SET status = 'archived', current_rev = current_rev + 1, updated_at = ?
		WHERE owner_key = ? AND key = ? AND current_rev = ? AND status = 'active'`,
		now.UnixNano(), ownerKey, key, expectedRev)
	if err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: archive preference: %v", port.ErrKnowledgeUnavailable, err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return domain.KnowledgePreference{}, port.ErrKnowledgeCASConflict
	}
	if err := insertKnowledgePreferenceRevision(ctx, tx, existing.ID, expectedRev+1, existing.Value, domain.KnowledgePreferenceArchived, sourceRef, now); err != nil {
		return domain.KnowledgePreference{}, err
	}
	if err := insertKnowledgeProjection(ctx, tx, now); err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.KnowledgePreference{}, fmt.Errorf("%w: commit archive preference: %v", port.ErrKnowledgeUnavailable, err)
	}
	return s.GetPreference(ctx, ownerKey, key)
}

// verifyCuratedDocumentResult checks a "result:<id>" content handle against
// the referenced result_records row before the document is allowed to bind
// to it: the result must exist, be available, match the document's declared
// digest, carry structurally valid storage, and its scope must be one the
// document's KnowledgeScopeKind/ScopeID unambiguously authorizes. Curated is
// the only provenance knowledge documents carry, and the SQLite resolver is
// the only production resolver, so a document whose ContentHandle is not
// "result:<64 hex>" can never be resolved: CreateDocument rejects it rather
// than persist a document nothing can ever read back.
func verifyCuratedDocumentResult(ctx context.Context, tx *sql.Tx, document domain.KnowledgeDocument) (string, error) {
	resultID, err := parseCuratedDocumentHandle(document.ContentHandle)
	if err != nil {
		return "", fmt.Errorf("%w: invalid curated document handle", port.ErrKnowledgeValidation)
	}
	var storageKind, storageKey, digest, state, actor, teamID, conversationKey, project string
	var resultBytes int64
	err = tx.QueryRowContext(ctx, `
		SELECT storage_kind, storage_key, sha256, bytes, state, actor, team_id, conversation_key, project
		FROM result_records WHERE result_id = ?`, resultID).Scan(
		&storageKind, &storageKey, &digest, &resultBytes, &state, &actor, &teamID, &conversationKey, &project)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: referenced result does not exist", port.ErrKnowledgeValidation)
	}
	if err != nil {
		return "", fmt.Errorf("%w: read curated result identity: %v", port.ErrKnowledgeUnavailable, err)
	}
	if state != string(domain.ResultAvailable) || digest != document.ContentDigest || resultBytes <= 0 {
		return "", fmt.Errorf("%w: curated result identity does not match the document", port.ErrKnowledgeValidation)
	}
	storage := domain.ResultStorage{Kind: domain.ResultStorageKind(storageKind), Key: storageKey}
	if err := storage.Validate(); err != nil {
		return "", fmt.Errorf("%w: curated result storage is invalid", port.ErrKnowledgeValidation)
	}
	resultScope := domain.ResultScope{Actor: actor, TeamID: teamID, ConversationKey: conversationKey, Project: project}
	if !domain.KnowledgeDocumentAuthorizesResult(document.ScopeKind, document.ScopeID, resultScope) {
		return "", fmt.Errorf("%w: document scope does not authorize the referenced result", port.ErrKnowledgeValidation)
	}
	return resultID, nil
}

// retainCuratedDocumentResult inserts the live result_references row that
// keeps resultID out of retention candidacy for as long as documentID stays
// active. The reference identity is deterministic, so a replayed create is a
// harmless no-op via INSERT OR IGNORE.
func retainCuratedDocumentResult(ctx context.Context, tx *sql.Tx, documentID, resultID string, now time.Time) error {
	referenceDigest := sha256.Sum256([]byte(knowledgeDocumentResultOwnerKind + "\x00" + documentID + "\x00" + resultID))
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO result_references (
		reference_id, result_id, owner_kind, owner_id, state, created_at)
		VALUES (?, ?, ?, ?, 'live', ?)`, fmt.Sprintf("%x", referenceDigest), resultID,
		knowledgeDocumentResultOwnerKind, documentID, now.UnixNano())
	if err != nil {
		return fmt.Errorf("%w: retain curated result reference: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}

// releaseCuratedDocumentResult releases any live result_references row owned
// by documentID. It is a harmless no-op for a document that never bound to a
// result.
func releaseCuratedDocumentResult(ctx context.Context, tx *sql.Tx, documentID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE result_references SET state = 'released', released_at = ?
		WHERE owner_kind = ? AND owner_id = ? AND state = 'live'`,
		now.UnixNano(), knowledgeDocumentResultOwnerKind, documentID)
	if err != nil {
		return fmt.Errorf("%w: release curated result reference: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}

func (s *KnowledgeStore) CreateDocument(ctx context.Context, document domain.KnowledgeDocument, limits domain.KnowledgeLimits) (domain.KnowledgeDocument, error) {
	if err := s.available(); err != nil {
		return domain.KnowledgeDocument{}, err
	}
	if err := document.ValidateWithLimits(limits); err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: begin create document: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := scanKnowledgeDocument(tx.QueryRowContext(ctx, `
		SELECT `+knowledgeDocumentColumns+` FROM knowledge_documents
		WHERE subject = ? AND scope_kind = ? AND scope_id = ?`,
		document.Subject, string(document.ScopeKind), document.ScopeID))
	if err == nil {
		var receiptStatus string
		receiptErr := tx.QueryRowContext(ctx, `
			SELECT status FROM knowledge_document_receipts WHERE document_id = ?`,
			string(existing.ID)).Scan(&receiptStatus)
		if errors.Is(receiptErr, sql.ErrNoRows) {
			return domain.KnowledgeDocument{}, fmt.Errorf("%w: document receipt missing for %q", port.ErrKnowledgeNotFound, existing.ID)
		} else if receiptErr != nil {
			return domain.KnowledgeDocument{}, fmt.Errorf("%w: document receipt lookup: %v", port.ErrKnowledgeUnavailable, receiptErr)
		}
		receipt := existing
		receipt.Status = domain.KnowledgeDocumentStatus(receiptStatus)
		if !sameKnowledgeDocumentContent(receipt, document) {
			return domain.KnowledgeDocument{}, fmt.Errorf("%w: document replay identity conflict", port.ErrKnowledgeCASConflict)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return domain.KnowledgeDocument{}, fmt.Errorf("%w: commit create document replay: %v", port.ErrKnowledgeUnavailable, commitErr)
		}
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: document replay lookup: %v", port.ErrKnowledgeUnavailable, err)
	}
	digest := domain.KnowledgeSubjectDigest(document.Subject, document.ScopeKind, document.ScopeID)
	var blocked int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM knowledge_tombstones WHERE subject_digest = ?`, digest).Scan(&blocked); err == nil {
		return domain.KnowledgeDocument{}, domain.ErrKnowledgeTombstoneBlocked
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: inspect tombstone: %v", port.ErrKnowledgeUnavailable, err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_documents`).Scan(&count); err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: count documents: %v", port.ErrKnowledgeUnavailable, err)
	}
	if count >= limits.WithDefaults().MaxDocuments {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: max documents reached", domain.ErrKnowledgeLimitExceeded)
	}
	curatedResultID, err := verifyCuratedDocumentResult(ctx, tx, document)
	if err != nil {
		return domain.KnowledgeDocument{}, err
	}
	now := time.Now().UTC()
	document.ID = domain.KnowledgeDocumentID(generateKnowledgeID("kdoc_"))
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_documents (`+knowledgeDocumentColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(document.ID), document.Subject, string(document.ScopeKind), document.ScopeID,
		document.ContentDigest, document.ContentHandle, document.SourceID, document.SourceRev,
		string(document.Provenance), string(document.Status), 1, now.UnixNano(), now.UnixNano()); err != nil {
		if isUniqueConstraint(err) {
			return domain.KnowledgeDocument{}, port.ErrKnowledgeCASConflict
		}
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: insert document: %v", port.ErrKnowledgeUnavailable, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_document_receipts
			(document_id, subject, scope_kind, scope_id, content_digest, content_handle, source_id, source_rev, provenance, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(document.ID), document.Subject, string(document.ScopeKind), document.ScopeID,
		document.ContentDigest, document.ContentHandle, document.SourceID, document.SourceRev,
		string(document.Provenance), string(document.Status), now.UnixNano()); err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: insert document receipt: %v", port.ErrKnowledgeUnavailable, err)
	}
	if curatedResultID != "" {
		if err := retainCuratedDocumentResult(ctx, tx, string(document.ID), curatedResultID, now); err != nil {
			return domain.KnowledgeDocument{}, err
		}
	}
	if err := insertKnowledgeProjection(ctx, tx, now); err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: commit create document: %v", port.ErrKnowledgeUnavailable, err)
	}
	document.CreatedAt = now
	document.UpdatedAt = now
	document.Revision = 1
	return document, nil
}

func (s *KnowledgeStore) GetDocument(ctx context.Context, id domain.KnowledgeDocumentID, readable []domain.KnowledgeScopeRef) (domain.KnowledgeDocument, error) {
	if err := s.available(); err != nil {
		return domain.KnowledgeDocument{}, err
	}
	filter, args := knowledgeScopeFilter(readable)
	document, err := scanKnowledgeDocument(s.db.QueryRowContext(ctx, `
		SELECT `+knowledgeDocumentColumns+` FROM knowledge_documents WHERE id = ? AND (`+filter+`)`,
		append([]any{string(id)}, args...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgeDocument{}, port.ErrKnowledgeNotFound
	}
	if err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: read document: %v", port.ErrKnowledgeUnavailable, err)
	}
	return document, nil
}

func (s *KnowledgeStore) ListClaimsInScopes(ctx context.Context, scopes []domain.KnowledgeScopeRef, subject string, limits domain.KnowledgeLimits) ([]domain.KnowledgeClaim, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	filter, args := knowledgeScopeFilter(scopes)
	if strings.TrimSpace(subject) != "" {
		filter = "(" + filter + ") AND subject = ?"
		args = append(args, subject)
	}
	maxRows := limits.WithDefaults().MaxClaimsListing + 1
	if strings.TrimSpace(subject) != "" {
		// A subject-scoped listing is bounded per readable scope identity,
		// so every claim of the subject stays reachable through the subject
		// selector regardless of the global listing bound.
		maxRows = limits.WithDefaults().MaxClaimsPerSubject*len(scopes) + 1
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+knowledgeClaimColumns+` FROM knowledge_claims
		WHERE (`+filter+`) AND status NOT IN ('archived', 'superseded') ORDER BY id LIMIT ?`,
		append(args, maxRows)...)
	if err != nil {
		return nil, fmt.Errorf("%w: list claims: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer rows.Close()
	var claims []domain.KnowledgeClaim
	for rows.Next() {
		claim, scanErr := scanKnowledgeClaim(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: scan claim: %v", port.ErrKnowledgeUnavailable, scanErr)
		}
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

func (s *KnowledgeStore) ListDocumentsInScopes(ctx context.Context, scopes []domain.KnowledgeScopeRef, limits domain.KnowledgeLimits) ([]domain.KnowledgeDocument, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	filter, args := knowledgeScopeFilter(scopes)
	maxRows := limits.WithDefaults().MaxDocuments + 1
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+knowledgeDocumentColumns+` FROM knowledge_documents
		WHERE (`+filter+`) AND status = 'active' ORDER BY id LIMIT ?`,
		append(args, maxRows)...)
	if err != nil {
		return nil, fmt.Errorf("%w: list documents: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer rows.Close()
	var documents []domain.KnowledgeDocument
	for rows.Next() {
		document, scanErr := scanKnowledgeDocument(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: scan document: %v", port.ErrKnowledgeUnavailable, scanErr)
		}
		documents = append(documents, document)
	}
	return documents, rows.Err()
}

// ArchiveDocument marks a document archived under revision CAS and appends an
// immutable document revision row attributing the command source. An exact
// retry of an already committed archive — same source reference and target
// status — returns the current document without advancing it, while the same
// source reference committing a different command or a stale expected
// revision is rejected.
func (s *KnowledgeStore) ArchiveDocument(ctx context.Context, id domain.KnowledgeDocumentID, expectedRev int, sourceRef string) (domain.KnowledgeDocument, error) {
	if err := s.available(); err != nil {
		return domain.KnowledgeDocument{}, err
	}
	if expectedRev <= 0 {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: expected revision must be positive", port.ErrKnowledgeValidation)
	}
	if err := domain.ValidateKnowledgeSourceRef(sourceRef); err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: begin archive document: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	document, err := scanKnowledgeDocument(tx.QueryRowContext(ctx, `
		SELECT `+knowledgeDocumentColumns+` FROM knowledge_documents WHERE id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.KnowledgeDocument{}, port.ErrKnowledgeNotFound
	}
	if err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: read document: %v", port.ErrKnowledgeUnavailable, err)
	}
	var committedStatus string
	revisionErr := tx.QueryRowContext(ctx, `
		SELECT status FROM knowledge_document_revisions
		WHERE document_id = ? AND source_ref = ?`, string(id), sourceRef).Scan(&committedStatus)
	if revisionErr == nil {
		if committedStatus != string(domain.KnowledgeDocumentArchived) {
			return domain.KnowledgeDocument{}, fmt.Errorf("%w: source %q already committed a different command for document %q", port.ErrKnowledgeCASConflict, sourceRef, id)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return domain.KnowledgeDocument{}, fmt.Errorf("%w: commit archive document replay: %v", port.ErrKnowledgeUnavailable, commitErr)
		}
		return document, nil
	} else if !errors.Is(revisionErr, sql.ErrNoRows) {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: document revision lookup: %v", port.ErrKnowledgeUnavailable, revisionErr)
	}
	if document.Status != domain.KnowledgeDocumentActive {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: document %q is archived by another source", port.ErrKnowledgeCASConflict, id)
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		UPDATE knowledge_documents SET status = 'archived', current_rev = current_rev + 1, updated_at = ?
		WHERE id = ? AND current_rev = ? AND status = 'active'`, now.UnixNano(), string(id), expectedRev)
	if err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: archive document: %v", port.ErrKnowledgeUnavailable, err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return domain.KnowledgeDocument{}, port.ErrKnowledgeCASConflict
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_document_revisions (document_id, revision_number, status, source_ref, created_at)
		VALUES (?, ?, 'archived', ?, ?)`, string(id), expectedRev+1, sourceRef, now.UnixNano()); err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: insert document revision: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := releaseCuratedDocumentResult(ctx, tx, string(id), now); err != nil {
		return domain.KnowledgeDocument{}, err
	}
	if err := insertKnowledgeProjection(ctx, tx, now); err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.KnowledgeDocument{}, fmt.Errorf("%w: commit archive document: %v", port.ErrKnowledgeUnavailable, err)
	}
	document.Status = domain.KnowledgeDocumentArchived
	document.Revision = expectedRev + 1
	document.UpdatedAt = now
	return document, nil
}

// CommitCommandReceipt commits the global command identity before its target
// mutation. One source reference may carry exactly one canonical payload: the
// first commit inserts the receipt, an exact retry returns nil, and a
// different payload under the same identity conflicts.
func (s *KnowledgeStore) CommitCommandReceipt(ctx context.Context, receipt domain.KnowledgeCommandReceipt) error {
	if err := s.available(); err != nil {
		return err
	}
	if err := receipt.Validate(); err != nil {
		return fmt.Errorf("%w: %v", port.ErrKnowledgeValidation, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("%w: begin command receipt: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()
	var action, digest, target string
	lookupErr := tx.QueryRowContext(ctx, `
		SELECT action, payload_digest, target FROM knowledge_command_receipts
		WHERE source_ref = ?`, receipt.SourceRef).Scan(&action, &digest, &target)
	if lookupErr == nil {
		if action != string(receipt.Action) || digest != receipt.PayloadDigest || target != receipt.Target {
			return fmt.Errorf("%w: source %q already committed a different command", port.ErrKnowledgeCASConflict, receipt.SourceRef)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("%w: commit command receipt replay: %v", port.ErrKnowledgeUnavailable, commitErr)
		}
		return nil
	} else if !errors.Is(lookupErr, sql.ErrNoRows) {
		return fmt.Errorf("%w: command receipt lookup: %v", port.ErrKnowledgeUnavailable, lookupErr)
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO knowledge_command_receipts (source_ref, action, payload_digest, target, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		receipt.SourceRef, string(receipt.Action), receipt.PayloadDigest, receipt.Target, now.UnixNano()); err != nil {
		if isUniqueConstraint(err) {
			return fmt.Errorf("%w: command identity %q raced another writer", port.ErrKnowledgeCASConflict, receipt.SourceRef)
		}
		return fmt.Errorf("%w: insert command receipt: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit command receipt: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}

func (s *KnowledgeStore) EnqueueProjection(ctx context.Context) error {
	if err := s.available(); err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO knowledge_projection_outbox (status, attempts, next_attempt, lease_until, last_error, created_at, updated_at)
		VALUES ('pending', 0, ?, 0, '', ?, ?)`, now.UnixNano(), now.UnixNano(), now.UnixNano())
	if err != nil {
		return fmt.Errorf("%w: enqueue knowledge projection: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}

// readKnowledgeProjectionSnapshot loads all content-bearing knowledge rows
// inside the caller's read-only transaction: archived, disputed, and
// superseded records included, tombstones excluded. Evidence rows are joined
// to their claim revisions so projections carry only claim linkage, kind,
// and the safe temporal reference.
func (s *Store) readKnowledgeProjectionSnapshot(ctx context.Context, tx *sql.Tx) (port.KnowledgeSnapshot, error) {
	var snapshot port.KnowledgeSnapshot

	claimRows, err := tx.QueryContext(ctx, `
		SELECT `+knowledgeClaimColumns+` FROM knowledge_claims
		ORDER BY scope_kind, scope_id, subject, source_ref, id`)
	if err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("list knowledge claims for snapshot: %v", err)
	}
	for claimRows.Next() {
		claim, scanErr := scanKnowledgeClaim(claimRows)
		if scanErr != nil {
			_ = claimRows.Close()
			return port.KnowledgeSnapshot{}, fmt.Errorf("scan knowledge claim for snapshot: %v", scanErr)
		}
		snapshot.Claims = append(snapshot.Claims, claim)
	}
	if err := claimRows.Err(); err != nil {
		_ = claimRows.Close()
		return port.KnowledgeSnapshot{}, fmt.Errorf("iterate knowledge claims for snapshot: %v", err)
	}
	if err := claimRows.Close(); err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("close knowledge claim snapshot rows: %v", err)
	}

	preferenceRows, err := tx.QueryContext(ctx, `
		SELECT `+knowledgePreferenceColumns+` FROM knowledge_preferences
		ORDER BY key, id`)
	if err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("list knowledge preferences for snapshot: %v", err)
	}
	for preferenceRows.Next() {
		preference, scanErr := scanKnowledgePreference(preferenceRows)
		if scanErr != nil {
			_ = preferenceRows.Close()
			return port.KnowledgeSnapshot{}, fmt.Errorf("scan knowledge preference for snapshot: %v", scanErr)
		}
		snapshot.Preferences = append(snapshot.Preferences, preference)
	}
	if err := preferenceRows.Err(); err != nil {
		_ = preferenceRows.Close()
		return port.KnowledgeSnapshot{}, fmt.Errorf("iterate knowledge preferences for snapshot: %v", err)
	}
	if err := preferenceRows.Close(); err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("close knowledge preference snapshot rows: %v", err)
	}

	documentRows, err := tx.QueryContext(ctx, `
		SELECT `+knowledgeDocumentColumns+` FROM knowledge_documents
		ORDER BY subject, scope_kind, scope_id, id`)
	if err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("list knowledge documents for snapshot: %v", err)
	}
	for documentRows.Next() {
		document, scanErr := scanKnowledgeDocument(documentRows)
		if scanErr != nil {
			_ = documentRows.Close()
			return port.KnowledgeSnapshot{}, fmt.Errorf("scan knowledge document for snapshot: %v", scanErr)
		}
		snapshot.Documents = append(snapshot.Documents, document)
	}
	if err := documentRows.Err(); err != nil {
		_ = documentRows.Close()
		return port.KnowledgeSnapshot{}, fmt.Errorf("iterate knowledge documents for snapshot: %v", err)
	}
	if err := documentRows.Close(); err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("close knowledge document snapshot rows: %v", err)
	}

	evidenceRows, err := tx.QueryContext(ctx, `
		SELECT r.claim_id, r.revision_number, e.kind, e.exchange_ts
		FROM knowledge_evidence e
		JOIN knowledge_claim_revisions r ON r.id = e.claim_revision
		ORDER BY r.claim_id, r.revision_number, e.exchange_ts`)
	if err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("list knowledge evidence for snapshot: %v", err)
	}
	for evidenceRows.Next() {
		var ref port.KnowledgeEvidenceRef
		var claimID, kind string
		if scanErr := evidenceRows.Scan(&claimID, &ref.RevisionNumber, &kind, &ref.ExchangeTS); scanErr != nil {
			_ = evidenceRows.Close()
			return port.KnowledgeSnapshot{}, fmt.Errorf("scan knowledge evidence for snapshot: %v", scanErr)
		}
		ref.ClaimID = domain.KnowledgeClaimID(claimID)
		ref.Kind = domain.KnowledgeEvidenceKind(kind)
		snapshot.Evidence = append(snapshot.Evidence, ref)
	}
	if err := evidenceRows.Err(); err != nil {
		_ = evidenceRows.Close()
		return port.KnowledgeSnapshot{}, fmt.Errorf("iterate knowledge evidence for snapshot: %v", err)
	}
	if err := evidenceRows.Close(); err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("close knowledge evidence snapshot rows: %v", err)
	}

	return snapshot, nil
}

// knowledgeProjectionBatchLimit bounds one coalesced batch so a pathological
// backlog cannot claim unbounded rows. Remaining due rows are claimed by the
// next batch immediately after.
const knowledgeProjectionBatchLimit = 512

// ClaimProjectionBatch atomically claims every currently due projection
// trigger as one batch: pending rows whose next attempt is due and
// processing rows whose lease expired. All rows in the batch receive the
// same lease and an incremented attempt counter. A completion, retry, or
// failure applies to exactly the claimed batch.
func (s *KnowledgeStore) ClaimProjectionBatch(ctx context.Context) ([]domain.KnowledgeProjectionItem, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	for range 3 {
		items, err := s.claimProjectionBatchOnce(ctx)
		if errors.Is(err, port.ErrKnowledgeCASConflict) {
			continue
		}
		return items, err
	}
	return nil, nil
}

func (s *KnowledgeStore) claimProjectionBatchOnce(ctx context.Context) ([]domain.KnowledgeProjectionItem, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("%w: begin projection batch claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	nowNanos := now.UnixNano()
	leaseUntil := now.Add(knowledgeProjectionLeaseDuration)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, attempts, next_attempt, created_at, updated_at
		FROM knowledge_projection_outbox
		WHERE (status = 'pending' AND next_attempt <= ?) OR (status = 'processing' AND lease_until <= ?)
		ORDER BY id ASC
		LIMIT ?`, nowNanos, nowNanos, knowledgeProjectionBatchLimit)
	if err != nil {
		return nil, fmt.Errorf("%w: select projection batch: %v", port.ErrKnowledgeUnavailable, err)
	}
	type dueRow struct {
		id, attempts int
		nextAttempt  int64
		createdNanos int64
		updatedNanos int64
	}
	var due []dueRow
	for rows.Next() {
		var r dueRow
		if scanErr := rows.Scan(&r.id, &r.attempts, &r.nextAttempt, &r.createdNanos, &r.updatedNanos); scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("%w: scan projection batch: %v", port.ErrKnowledgeUnavailable, scanErr)
		}
		due = append(due, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("%w: iterate projection batch: %v", port.ErrKnowledgeUnavailable, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("%w: close projection batch rows: %v", port.ErrKnowledgeUnavailable, err)
	}
	if len(due) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(due))
	args := make([]any, 0, len(due)+4)
	args = append(args, leaseUntil.UnixNano(), nowNanos)
	for i, r := range due {
		placeholders[i] = "?"
		args = append(args, r.id)
	}
	args = append(args, nowNanos, nowNanos)
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE knowledge_projection_outbox
		SET status = 'processing', attempts = attempts + 1, lease_until = ?, updated_at = ?
		WHERE id IN (%s)
			AND ((status = 'pending' AND next_attempt <= ?) OR (status = 'processing' AND lease_until <= ?))`,
		strings.Join(placeholders, ", ")), args...)
	if err != nil {
		return nil, fmt.Errorf("%w: claim projection batch: %v", port.ErrKnowledgeUnavailable, err)
	}
	if changed, _ := result.RowsAffected(); changed != int64(len(due)) {
		if commitErr := tx.Commit(); commitErr != nil {
			return nil, fmt.Errorf("%w: commit projection batch conflict: %v", port.ErrKnowledgeUnavailable, commitErr)
		}
		return nil, port.ErrKnowledgeCASConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%w: commit projection batch claim: %v", port.ErrKnowledgeUnavailable, err)
	}
	items := make([]domain.KnowledgeProjectionItem, 0, len(due))
	for _, r := range due {
		items = append(items, domain.KnowledgeProjectionItem{
			ID: r.id, Status: domain.KnowledgeProjectionProcessing, Attempts: r.attempts + 1,
			NextAttempt: time.Unix(0, r.nextAttempt).UTC(), LeaseUntil: leaseUntil,
			CreatedAt: time.Unix(0, r.createdNanos).UTC(), UpdatedAt: now,
		})
	}
	return items, nil
}

// CompleteProjectionBatch marks exactly the claimed batch done after a
// successful promotion. Rows claimed under a different lease or by another
// worker are never touched.
func (s *KnowledgeStore) CompleteProjectionBatch(ctx context.Context, ids []int, leaseUntil time.Time) error {
	if err := s.available(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	nowNanos := time.Now().UTC().UnixNano()
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+2)
	args = append(args, nowNanos)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, leaseUntil.UnixNano())
	result, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE knowledge_projection_outbox SET status = 'done', lease_until = 0, updated_at = ?
		WHERE id IN (%s) AND status = 'processing' AND lease_until = ?`,
		strings.Join(placeholders, ", ")), args...)
	if err != nil {
		return fmt.Errorf("%w: complete projection batch: %v", port.ErrKnowledgeUnavailable, err)
	}
	if changed, _ := result.RowsAffected(); changed != int64(len(ids)) {
		return port.ErrKnowledgeCASConflict
	}
	return nil
}

// RetryProjectionBatch schedules exactly the claimed batch for a later
// attempt while preserving the attempt counter. Attempts are never
// decremented, so the worker's retry budget is monotonic and a row can never
// be retried forever.
func (s *KnowledgeStore) RetryProjectionBatch(ctx context.Context, ids []int, leaseUntil, nextAttempt time.Time) error {
	if err := s.available(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	nowNanos := time.Now().UTC().UnixNano()
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+3)
	args = append(args, nextAttempt.UnixNano(), nowNanos)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, leaseUntil.UnixNano())
	result, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE knowledge_projection_outbox SET status = 'pending', next_attempt = ?, lease_until = 0, updated_at = ?
		WHERE id IN (%s) AND status = 'processing' AND lease_until = ?`,
		strings.Join(placeholders, ", ")), args...)
	if err != nil {
		return fmt.Errorf("%w: retry projection batch: %v", port.ErrKnowledgeUnavailable, err)
	}
	if changed, _ := result.RowsAffected(); changed != int64(len(ids)) {
		return port.ErrKnowledgeCASConflict
	}
	return nil
}

// DeferProjectionCleanupBatch schedules the claimed batch for a
// cleanup-only retry without consuming the projection attempt budget. The
// claim incremented the attempt counter for this attempt; the deferral rolls
// it back (never below zero), so cleanup retries cycle the same claim value
// and a later real projection failure still has its full budget. The batch
// stays pending until the backup is actually removed.
func (s *KnowledgeStore) DeferProjectionCleanupBatch(ctx context.Context, ids []int, leaseUntil, nextAttempt time.Time) error {
	if err := s.available(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	nowNanos := time.Now().UTC().UnixNano()
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+3)
	args = append(args, nextAttempt.UnixNano(), nowNanos)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, leaseUntil.UnixNano())
	result, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE knowledge_projection_outbox SET status = 'pending', next_attempt = ?, attempts = MAX(attempts - 1, 0), lease_until = 0, updated_at = ?
		WHERE id IN (%s) AND status = 'processing' AND lease_until = ?`,
		strings.Join(placeholders, ", ")), args...)
	if err != nil {
		return fmt.Errorf("%w: defer projection cleanup batch: %v", port.ErrKnowledgeUnavailable, err)
	}
	if changed, _ := result.RowsAffected(); changed != int64(len(ids)) {
		return port.ErrKnowledgeCASConflict
	}
	return nil
}

// FailProjectionBatch marks exactly the claimed batch terminal with a stable
// bounded code. Detailed errors are logged sanitized and never persisted.
func (s *KnowledgeStore) FailProjectionBatch(ctx context.Context, ids []int, leaseUntil time.Time, code string) error {
	if err := s.available(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if len(code) > 64 || strings.ContainsAny(code, "\x00\r\n") {
		return fmt.Errorf("%w: projection failure code is not bounded", port.ErrKnowledgeValidation)
	}
	nowNanos := time.Now().UTC().UnixNano()
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+3)
	args = append(args, code, nowNanos)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, leaseUntil.UnixNano())
	result, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE knowledge_projection_outbox SET status = 'failed', last_error = ?, lease_until = 0, updated_at = ?
		WHERE id IN (%s) AND status = 'processing' AND lease_until = ?`,
		strings.Join(placeholders, ", ")), args...)
	if err != nil {
		return fmt.Errorf("%w: fail projection batch: %v", port.ErrKnowledgeUnavailable, err)
	}
	if changed, _ := result.RowsAffected(); changed != int64(len(ids)) {
		return port.ErrKnowledgeCASConflict
	}
	return nil
}

func (s *KnowledgeStore) CleanupProjection(ctx context.Context, before time.Time) error {
	if err := s.available(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM knowledge_projection_outbox WHERE status IN ('done', 'failed') AND updated_at < ?`, before.UnixNano())
	if err != nil {
		return fmt.Errorf("%w: cleanup projection: %v", port.ErrKnowledgeUnavailable, err)
	}
	return nil
}
