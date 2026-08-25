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

// ResultStore joins the v34 identity catalog to one typed physical backend.
// The backend has no catalog authority; every read is authorized and verified
// by the catalog before a handle or range is returned.
type ResultStore struct {
	catalog *Store
	payload port.ResultPayloadStore

	// Test-only crash seam between physical publication and metadata commit.
	afterPayloadPublished func() error
}

func NewResultStore(catalog *Store, payload port.ResultPayloadStore) (*ResultStore, error) {
	if catalog == nil || catalog.db == nil || payload == nil {
		return nil, errors.New("result store requires catalog and payload backend")
	}
	return &ResultStore{catalog: catalog, payload: payload}, nil
}

func (s *ResultStore) Materialize(ctx context.Context, request port.ResultMaterialization) (domain.ResultHandle, error) {
	if err := validateMaterialization(request); err != nil {
		return domain.ResultHandle{}, err
	}
	if s == nil || s.catalog == nil || s.catalog.db == nil || s.payload == nil {
		return domain.ResultHandle{}, domain.ErrResultUnavailable
	}
	for range 3 {
		reservation, err := s.reserve(ctx, request.Producer)
		if err != nil {
			return domain.ResultHandle{}, err
		}
		switch reservation.state {
		case "committed":
			_, handle, err := s.Resolve(ctx, reservation.resultID, request.Scope)
			return handle, err
		case "quarantined":
			return domain.ResultHandle{}, domain.ErrResultQuarantined
		case "reserved", "payload_published":
		default:
			return domain.ResultHandle{}, domain.ErrResultUnavailable
		}

		storage, err := s.payload.StorageFor(reservation.resultID)
		if err != nil || storage.Validate() != nil {
			return domain.ResultHandle{}, domain.ErrResultUnavailable
		}
		if reservation.state == "payload_published" && reservation.storage != storage {
			if err := s.quarantineReservation(ctx, reservation, reservation.storage); err != nil {
				return domain.ResultHandle{}, domain.ErrResultUnavailable
			}
			return domain.ResultHandle{}, domain.ErrResultQuarantined
		}
		if err := s.payload.Publish(ctx, storage, request.Payload); err != nil {
			if errors.Is(err, port.ErrResultPayloadConflict) {
				if quarantineErr := s.quarantineReservation(ctx, reservation, storage); quarantineErr != nil {
					return domain.ResultHandle{}, domain.ErrResultUnavailable
				}
				return domain.ResultHandle{}, domain.ErrResultQuarantined
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return domain.ResultHandle{}, err
			}
			return domain.ResultHandle{}, domain.ErrResultUnavailable
		}
		if reservation.state == "reserved" {
			published, err := s.markPayloadPublished(ctx, reservation, storage)
			if err != nil {
				return domain.ResultHandle{}, err
			}
			if !published {
				continue
			}
		}
		if s.afterPayloadPublished != nil {
			if err := s.afterPayloadPublished(); err != nil {
				return domain.ResultHandle{}, err
			}
		}
		if err := s.commit(ctx, reservation.resultID, storage, request); err != nil {
			return domain.ResultHandle{}, err
		}
		_, handle, err := s.Resolve(ctx, reservation.resultID, request.Scope)
		return handle, err
	}
	return domain.ResultHandle{}, domain.ErrResultUnavailable
}

func (s *ResultStore) Resolve(ctx context.Context, resultID string, required domain.ResultScope) (domain.ResultIdentity, domain.ResultHandle, error) {
	if err := required.Validate(); err != nil || !validResultOpaqueID(resultID) {
		return domain.ResultIdentity{}, domain.ResultHandle{}, domain.ErrResultInvalid
	}
	identity, err := s.identity(ctx, resultID)
	if err != nil {
		return domain.ResultIdentity{}, domain.ResultHandle{}, err
	}
	if err := identity.Scope.Matches(required); err != nil {
		return domain.ResultIdentity{}, domain.ResultHandle{}, err
	}
	if identity.State == domain.ResultQuarantined {
		return domain.ResultIdentity{}, domain.ResultHandle{}, domain.ErrResultQuarantined
	}
	if err := s.payload.Verify(ctx, identity.Storage, identity.SHA256, identity.Bytes); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return domain.ResultIdentity{}, domain.ResultHandle{}, err
		}
		if quarantineErr := s.quarantineResult(ctx, identity.ResultID); quarantineErr != nil {
			return domain.ResultIdentity{}, domain.ResultHandle{}, domain.ErrResultUnavailable
		}
		return domain.ResultIdentity{}, domain.ResultHandle{}, domain.ErrResultQuarantined
	}
	handle, err := identity.Handle(resultAvailability(identity.Storage.Kind), nil)
	if err != nil {
		return domain.ResultIdentity{}, domain.ResultHandle{}, err
	}
	return identity, handle, nil
}

func (s *ResultStore) ReadRange(ctx context.Context, resultID string, required domain.ResultScope, offsetBytes, maxBytes int64) (domain.ResultChunk, error) {
	if offsetBytes < 0 || maxBytes <= 0 {
		return domain.ResultChunk{}, domain.ErrResultInvalid
	}
	if err := required.Validate(); err != nil || !validResultOpaqueID(resultID) {
		return domain.ResultChunk{}, domain.ErrResultInvalid
	}
	identity, err := s.identity(ctx, resultID)
	if err != nil {
		return domain.ResultChunk{}, err
	}
	if err := identity.Scope.Matches(required); err != nil {
		return domain.ResultChunk{}, err
	}
	if identity.State == domain.ResultQuarantined {
		return domain.ResultChunk{}, domain.ErrResultQuarantined
	}
	chunk, err := s.payload.ReadRange(ctx, identity.Storage, identity.SHA256, identity.Bytes, offsetBytes, maxBytes)
	if err != nil {
		if errors.Is(err, domain.ErrResultInvalid) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return domain.ResultChunk{}, err
		}
		if quarantineErr := s.quarantineResult(ctx, identity.ResultID); quarantineErr != nil {
			return domain.ResultChunk{}, domain.ErrResultUnavailable
		}
		return domain.ResultChunk{}, domain.ErrResultQuarantined
	}
	return chunk, nil
}

// VerifyForWorkstream resolves the durable workstream before it reads the
// typed payload, so a result selector cannot grant authority outside the
// workstream's actor, conversation, or project binding.
func (s *ResultStore) VerifyForWorkstream(ctx context.Context, request port.WorkstreamResultVerification) (domain.ResultIdentity, error) {
	if s == nil || s.catalog == nil || s.catalog.db == nil || s.payload == nil ||
		!validResultOpaqueID(request.ResultID) || strings.TrimSpace(request.WorkstreamID) == "" ||
		strings.TrimSpace(request.TeamID) == "" {
		return domain.ResultIdentity{}, domain.ErrResultInvalid
	}
	workstream, err := loadWorkstream(ctx, s.catalog.db, `WHERE workstream_id = ? AND status IN ('proposed', 'active', 'paused', 'blocked')`, request.WorkstreamID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ResultIdentity{}, port.ErrWorkstreamNotFound
	}
	if err != nil {
		return domain.ResultIdentity{}, fmt.Errorf("resolve workstream for result verification: %w", err)
	}
	if err := workstream.ValidateBinding(request.Actor, domain.ConversationKey(request.Conversation), request.Project); err != nil {
		return domain.ResultIdentity{}, err
	}
	identity, _, err := s.Resolve(ctx, request.ResultID, domain.ResultScope{
		Actor: request.Actor, TeamID: request.TeamID, ConversationKey: request.Conversation, Project: request.Project,
	})
	if err != nil {
		return domain.ResultIdentity{}, err
	}
	if err := identity.VerifyWorkstreamEligible(); err != nil {
		return domain.ResultIdentity{}, err
	}
	if identity.Producer.Kind == domain.ResultProducerExternalAgentJob {
		var status, actor, teamID, conversationKey, project string
		var revision int
		err := s.catalog.db.QueryRowContext(ctx, `SELECT status, status_revision, actor, slack_team_id, conversation_key, primary_project
			FROM external_agent_jobs WHERE job_id = ?`, identity.Producer.ID).Scan(&status, &revision, &actor, &teamID, &conversationKey, &project)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ResultIdentity{}, domain.ErrResultUnavailable
		}
		if err != nil {
			return domain.ResultIdentity{}, fmt.Errorf("read ACP producer for result verification: %w", err)
		}
		if status != string(domain.JobCompleted) || revision != identity.Producer.Revision {
			return domain.ResultIdentity{}, domain.ErrResultUnavailable
		}
		if actor != identity.Scope.Actor || teamID != identity.Scope.TeamID || conversationKey != identity.Scope.ConversationKey || project != identity.Scope.Project {
			return domain.ResultIdentity{}, domain.ErrResultScopeMismatch
		}
		ownerID := fmt.Sprintf("%s:%d", identity.Producer.ID, identity.Producer.Revision)
		var boundResultID string
		err = s.catalog.db.QueryRowContext(ctx, `SELECT result_id FROM result_references
			WHERE owner_kind = ? AND owner_id = ? AND state = 'live'`, externalAgentResultOwnerKind, ownerID).Scan(&boundResultID)
		if errors.Is(err, sql.ErrNoRows) || boundResultID != identity.ResultID {
			return domain.ResultIdentity{}, domain.ErrResultUnavailable
		}
		if err != nil {
			return domain.ResultIdentity{}, fmt.Errorf("read ACP result binding for workstream verification: %w", err)
		}
	}
	return identity, nil
}

var _ port.ResultIdentityVerifier = (*ResultStore)(nil)

type resultReservation struct {
	resultID string
	state    string
	storage  domain.ResultStorage
}

func (s *ResultStore) reserve(ctx context.Context, producer domain.ResultProducer) (resultReservation, error) {
	tx, err := s.catalog.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return resultReservation{}, fmt.Errorf("begin result reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	reservation, err := readReservation(ctx, tx, producer)
	if errors.Is(err, sql.ErrNoRows) {
		resultID, err := newResultID()
		if err != nil {
			return resultReservation{}, err
		}
		now := time.Now().UTC().UnixNano()
		if _, err := tx.ExecContext(ctx, `INSERT INTO result_materializations (
			producer_kind, producer_id, producer_revision, result_id, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'reserved', ?, ?)`, producer.Kind, producer.ID, producer.Revision, resultID, now, now); err != nil {
			return resultReservation{}, fmt.Errorf("reserve result materialization: %w", err)
		}
		reservation = resultReservation{resultID: resultID, state: "reserved"}
	} else if err != nil {
		return resultReservation{}, fmt.Errorf("read result reservation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return resultReservation{}, fmt.Errorf("commit result reservation: %w", err)
	}
	return reservation, nil
}

func readReservation(ctx context.Context, tx *sql.Tx, producer domain.ResultProducer) (resultReservation, error) {
	var reservation resultReservation
	var kind, key string
	err := tx.QueryRowContext(ctx, `SELECT result_id, state, storage_kind, storage_key
		FROM result_materializations WHERE producer_kind = ? AND producer_id = ? AND producer_revision = ?`,
		producer.Kind, producer.ID, producer.Revision).Scan(&reservation.resultID, &reservation.state, &kind, &key)
	if err != nil {
		return resultReservation{}, err
	}
	if kind != "" {
		reservation.storage = domain.ResultStorage{Kind: domain.ResultStorageKind(kind), Key: key}
	}
	return reservation, nil
}

func (s *ResultStore) markPayloadPublished(ctx context.Context, reservation resultReservation, storage domain.ResultStorage) (bool, error) {
	result, err := s.catalog.db.ExecContext(ctx, `UPDATE result_materializations
		SET state = 'payload_published', storage_kind = ?, storage_key = ?, updated_at = ?
		WHERE result_id = ? AND state = 'reserved'`, storage.Kind, storage.Key, time.Now().UTC().UnixNano(), reservation.resultID)
	if err != nil {
		return false, fmt.Errorf("mark result payload published: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (s *ResultStore) commit(ctx context.Context, resultID string, storage domain.ResultStorage, request port.ResultMaterialization) error {
	digest := sha256.Sum256([]byte(request.Payload))
	digestHex := hex.EncodeToString(digest[:])
	bytes := int64(len(request.Payload))
	now := time.Now().UTC()
	tx, err := s.catalog.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin result commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var reservationState, storageKind, storageKey string
	if err := tx.QueryRowContext(ctx, `SELECT state, storage_kind, storage_key FROM result_materializations
		WHERE result_id = ?`, resultID).Scan(&reservationState, &storageKind, &storageKey); err != nil {
		return domain.ErrResultUnavailable
	}
	if reservationState == "committed" {
		return tx.Commit()
	}
	if reservationState != "payload_published" || storageKind != string(storage.Kind) || storageKey != storage.Key {
		return domain.ErrResultUnavailable
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO result_records (
		result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key,
		sha256, bytes, media_type, actor, team_id, conversation_key, project,
		retention_class, created_at, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'available')`,
		resultID, request.Producer.Kind, request.Producer.ID, request.Producer.Revision,
		storage.Kind, storage.Key, digestHex, bytes, request.MediaType, request.Scope.Actor,
		request.Scope.TeamID, request.Scope.ConversationKey, request.Scope.Project,
		request.Retention, now.UnixNano()); err != nil {
		return fmt.Errorf("insert result record: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE result_materializations SET state = 'committed', updated_at = ?
		WHERE result_id = ? AND state = 'payload_published' AND storage_kind = ? AND storage_key = ?`,
		now.UnixNano(), resultID, storage.Kind, storage.Key)
	if err != nil {
		return fmt.Errorf("commit result materialization: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return domain.ErrResultUnavailable
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit result metadata: %w", err)
	}
	return nil
}

func (s *ResultStore) identity(ctx context.Context, resultID string) (domain.ResultIdentity, error) {
	var identity domain.ResultIdentity
	var producerKind, storageKind, retention, state string
	var createdAt int64
	err := s.catalog.db.QueryRowContext(ctx, `SELECT result_id, producer_kind, producer_id, producer_revision,
		storage_kind, storage_key, sha256, bytes, media_type, actor, team_id, conversation_key,
		project, retention_class, created_at, state FROM result_records WHERE result_id = ?`, resultID).Scan(
		&identity.ResultID, &producerKind, &identity.Producer.ID, &identity.Producer.Revision,
		&storageKind, &identity.Storage.Key, &identity.SHA256, &identity.Bytes, &identity.MediaType,
		&identity.Scope.Actor, &identity.Scope.TeamID, &identity.Scope.ConversationKey, &identity.Scope.Project,
		&retention, &createdAt, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ResultIdentity{}, domain.ErrResultUnavailable
	}
	if err != nil {
		return domain.ResultIdentity{}, fmt.Errorf("read result identity: %w", err)
	}
	identity.Producer.Kind = domain.ResultProducerKind(producerKind)
	identity.Storage.Kind = domain.ResultStorageKind(storageKind)
	identity.Retention = domain.ResultRetentionClass(retention)
	identity.CreatedAt = time.Unix(0, createdAt).UTC()
	identity.State = domain.ResultState(state)
	if err := identity.Validate(); err != nil {
		return domain.ResultIdentity{}, domain.ErrResultUnavailable
	}
	return identity, nil
}

func (s *ResultStore) quarantineResult(ctx context.Context, resultID string) error {
	result, err := s.catalog.db.ExecContext(ctx, `UPDATE result_records SET state = 'quarantined'
		WHERE result_id = ? AND state = 'available'`, resultID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 1 {
		return nil
	}
	var state string
	if err := s.catalog.db.QueryRowContext(ctx, `SELECT state FROM result_records WHERE result_id = ?`, resultID).Scan(&state); err != nil || state != "quarantined" {
		return domain.ErrResultUnavailable
	}
	return nil
}

func (s *ResultStore) quarantineReservation(ctx context.Context, reservation resultReservation, storage domain.ResultStorage) error {
	if err := storage.Validate(); err != nil {
		return domain.ErrResultUnavailable
	}
	tx, err := s.catalog.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var state, storageKind, storageKey string
	if err := tx.QueryRowContext(ctx, `SELECT state, storage_kind, storage_key FROM result_materializations
		WHERE result_id = ?`, reservation.resultID).Scan(&state, &storageKind, &storageKey); err != nil {
		return domain.ErrResultUnavailable
	}
	if state == "quarantined" {
		return tx.Commit()
	}
	if state == "reserved" {
		result, err := tx.ExecContext(ctx, `UPDATE result_materializations
			SET state = 'payload_published', storage_kind = ?, storage_key = ?, updated_at = ?
			WHERE result_id = ? AND state = 'reserved'`, storage.Kind, storage.Key, time.Now().UTC().UnixNano(), reservation.resultID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != 1 {
			return domain.ErrResultUnavailable
		}
		state, storageKind, storageKey = "payload_published", string(storage.Kind), storage.Key
	}
	if state != "payload_published" || storageKind != string(storage.Kind) || storageKey != storage.Key {
		return domain.ErrResultUnavailable
	}
	result, err := tx.ExecContext(ctx, `UPDATE result_materializations SET state = 'quarantined', updated_at = ?
		WHERE result_id = ? AND state = 'payload_published' AND storage_kind = ? AND storage_key = ?`,
		time.Now().UTC().UnixNano(), reservation.resultID, storage.Kind, storage.Key)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return domain.ErrResultUnavailable
	}
	return tx.Commit()
}

func validateMaterialization(request port.ResultMaterialization) error {
	if err := request.Producer.Validate(); err != nil {
		return err
	}
	if err := request.Scope.Validate(); err != nil || !utf8.ValidString(request.Payload) || strings.TrimSpace(request.Payload) == "" {
		return domain.ErrResultInvalid
	}
	if sanitized, err := domain.SanitizeResultText(request.Payload); err != nil || sanitized != request.Payload {
		return domain.ErrResultInvalid
	}
	identity := domain.ResultIdentity{
		ResultID: strings.Repeat("0", 64), Producer: request.Producer,
		Storage: domain.ResultStorage{Kind: domain.ResultStorageRecoverable, Key: "placeholder"},
		SHA256:  resultSHA256(request.Payload), Bytes: int64(len(request.Payload)),
		MediaType: request.MediaType, Scope: request.Scope, Retention: request.Retention,
		CreatedAt: time.Unix(0, 1).UTC(), State: domain.ResultAvailable,
	}
	return identity.Validate()
}

func resultSHA256(payload string) string {
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}

func resultAvailability(kind domain.ResultStorageKind) []domain.ResultAvailability {
	availability := []domain.ResultAvailability{domain.ResultAvailabilityRangeRead}
	if kind == domain.ResultStorageArtifact {
		availability = append(availability, domain.ResultAvailabilityPrivateArtifact)
	}
	return availability
}

func newResultID() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func validResultOpaqueID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

var _ port.TrustedResultStore = (*ResultStore)(nil)
