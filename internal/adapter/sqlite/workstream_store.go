package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.WorkstreamStore = (*WorkstreamStore)(nil)

var (
	ErrWorkstreamCASConflict          = port.ErrWorkstreamCASConflict
	ErrWorkstreamConversationConflict = port.ErrWorkstreamConversationConflict
)

type WorkstreamStore struct {
	db *sql.DB
}

func NewWorkstreamStore(store *Store) *WorkstreamStore {
	if store == nil || store.db == nil {
		return nil
	}
	return &WorkstreamStore{db: store.db}
}

func (s *WorkstreamStore) Create(ctx context.Context, workstream domain.Workstream, source domain.WorkstreamTransitionSource, sourceID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: workstream store is not configured", port.ErrWorkstreamUnavailable)
	}
	if workstream.Status != domain.WorkstreamProposed || workstream.Revision != 0 {
		return fmt.Errorf("%w: new workstreams must be proposed at revision zero", port.ErrWorkstreamValidation)
	}
	if source == domain.WorkstreamSourceWorker || (source != domain.WorkstreamSourceHuman && source != domain.WorkstreamSourceRoot && source != domain.WorkstreamSourceSystem) || strings.TrimSpace(sourceID) == "" {
		return fmt.Errorf("%w: creation provenance is invalid", port.ErrWorkstreamValidation)
	}
	if err := workstream.ValidateWithLimits(storageWorkstreamLimits()); err != nil {
		return fmt.Errorf("%w: %v", port.ErrWorkstreamValidation, err)
	}
	now := time.Now().UTC()
	if workstream.CreatedAt.IsZero() {
		workstream.CreatedAt = now
	}
	if workstream.UpdatedAt.IsZero() {
		workstream.UpdatedAt = workstream.CreatedAt
	}
	stateJSON, stateDigest, err := workstream.StateJSON()
	if err != nil {
		return fmt.Errorf("%w: encode initial workstream state: %v", port.ErrWorkstreamUnavailable, err)
	}
	creationPayloadJSON, creationPayloadDigest, err := workstreamCreationPayload(workstream)
	if err != nil {
		return fmt.Errorf("%w: encode workstream creation payload: %v", port.ErrWorkstreamUnavailable, err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("%w: begin workstream creation: %v", port.ErrWorkstreamUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()
	var existingCreationPayload string
	err = tx.QueryRowContext(ctx, `SELECT payload_json FROM workstream_transitions WHERE workstream_id = ? AND source_id = ?`, workstream.ID, sourceID).Scan(&existingCreationPayload)
	if err == nil {
		if existingCreationPayload == creationPayloadJSON {
			return nil
		}
		return fmt.Errorf("%w: creation source %q was already committed with different state", port.ErrWorkstreamValidation, sourceID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: inspect workstream creation identity: %v", port.ErrWorkstreamUnavailable, err)
	}

	var existingOwner, existingConversation, existingProject, existingStatus, existingObjective string
	var existingRevision int
	err = tx.QueryRowContext(ctx, `SELECT owner_actor, conversation_key, project, status, revision, objective
		FROM workstreams WHERE workstream_id = ?`, workstream.ID).Scan(&existingOwner, &existingConversation, &existingProject, &existingStatus, &existingRevision, &existingObjective)
	if err == nil {
		if existingOwner == workstream.OwnerActor && existingConversation == string(workstream.ConversationKey) && existingProject == workstream.Project && existingStatus == string(workstream.Status) && existingRevision == workstream.Revision && existingObjective == workstream.Objective {
			return nil
		}
		return fmt.Errorf("%w: workstream ID %q is already bound to different state (owner=%q conversation=%q project=%q status=%q revision=%d objective=%q)", port.ErrWorkstreamValidation, workstream.ID, existingOwner, existingConversation, existingProject, existingStatus, existingRevision, existingObjective)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: inspect workstream identity: %v", port.ErrWorkstreamUnavailable, err)
	}

	var existingID string
	err = tx.QueryRowContext(ctx, `SELECT workstream_id FROM workstreams WHERE conversation_key = ? AND status IN ('proposed', 'active', 'paused', 'blocked') LIMIT 1`, string(workstream.ConversationKey)).Scan(&existingID)
	if err == nil {
		return fmt.Errorf("%w: conversation is already bound to %q", port.ErrWorkstreamConversationConflict, existingID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: inspect workstream conversation binding: %v", port.ErrWorkstreamUnavailable, err)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO workstreams (
		workstream_id, conversation_key, owner_actor, project, status, revision,
		objective, current_phase, continuation_of, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		workstream.ID, string(workstream.ConversationKey), workstream.OwnerActor, workstream.Project,
		string(workstream.Status), workstream.Revision, workstream.Objective, workstream.CurrentPhase,
		workstream.ContinuationOf, workstream.CreatedAt.UTC().UnixNano(), workstream.UpdatedAt.UTC().UnixNano()); err != nil {
		if isWorkstreamConversationConstraint(err) {
			return fmt.Errorf("%w: %v", port.ErrWorkstreamConversationConflict, err)
		}
		return fmt.Errorf("%w: insert workstream: %v", port.ErrWorkstreamUnavailable, err)
	}
	if err := insertWorkstreamChildren(ctx, tx, workstream); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workstream_transitions (
		workstream_id, from_revision, to_revision, source, source_id, actor, action,
		payload_digest, payload_json, state_digest, state_json, committed_at)
		VALUES (?, 0, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		workstream.ID, string(source), sourceID, workstream.OwnerActor, string(domain.WorkstreamActionCreateWorkstream),
		creationPayloadDigest, creationPayloadJSON, stateDigest, stateJSON, workstream.CreatedAt.UTC().UnixNano()); err != nil {
		return fmt.Errorf("%w: append workstream creation: %v", port.ErrWorkstreamUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit workstream creation: %v", port.ErrWorkstreamUnavailable, err)
	}
	return nil
}

func workstreamCreationPayload(workstream domain.Workstream) (string, string, error) {
	workstream.CreatedAt = time.Time{}
	workstream.UpdatedAt = time.Time{}
	return workstream.StateJSON()
}

func (s *WorkstreamStore) Get(ctx context.Context, workstreamID string) (domain.Workstream, error) {
	if s == nil || s.db == nil {
		return domain.Workstream{}, fmt.Errorf("%w: workstream store is not configured", port.ErrWorkstreamUnavailable)
	}
	if strings.TrimSpace(workstreamID) == "" {
		return domain.Workstream{}, fmt.Errorf("%w: workstream ID is required", port.ErrWorkstreamValidation)
	}
	workstream, err := loadWorkstream(ctx, s.db, `WHERE workstream_id = ?`, workstreamID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Workstream{}, port.ErrWorkstreamNotFound
	}
	if err != nil {
		return domain.Workstream{}, fmt.Errorf("%w: load workstream: %v", port.ErrWorkstreamUnavailable, err)
	}
	if err := workstream.ValidateWithLimits(storageWorkstreamLimits()); err != nil {
		return domain.Workstream{}, fmt.Errorf("%w: %v", port.ErrWorkstreamValidation, err)
	}
	return workstream, nil
}

func (s *WorkstreamStore) ActiveForConversation(ctx context.Context, conversationKey domain.ConversationKey) (domain.Workstream, error) {
	if s == nil || s.db == nil {
		return domain.Workstream{}, fmt.Errorf("%w: workstream store is not configured", port.ErrWorkstreamUnavailable)
	}
	if strings.TrimSpace(string(conversationKey)) == "" {
		return domain.Workstream{}, fmt.Errorf("%w: conversation key is required", port.ErrWorkstreamValidation)
	}
	workstream, err := loadWorkstream(ctx, s.db, `WHERE conversation_key = ? AND status IN ('proposed', 'active', 'paused', 'blocked') ORDER BY updated_at DESC LIMIT 1`, string(conversationKey))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Workstream{}, port.ErrWorkstreamNotFound
	}
	if err != nil {
		return domain.Workstream{}, fmt.Errorf("%w: load active workstream: %v", port.ErrWorkstreamUnavailable, err)
	}
	if err := workstream.ValidateWithLimits(storageWorkstreamLimits()); err != nil {
		return domain.Workstream{}, fmt.Errorf("%w: %v", port.ErrWorkstreamValidation, err)
	}
	return workstream, nil
}

func (s *WorkstreamStore) Apply(ctx context.Context, transition domain.WorkstreamTransition, limits domain.WorkstreamLimits, now time.Time) (domain.WorkstreamTransitionRecord, error) {
	if transition.Action == domain.WorkstreamActionLinkCompletedResult {
		return domain.WorkstreamTransitionRecord{}, domain.ErrResultInvalid
	}
	return s.apply(ctx, transition, limits, now, nil)
}

func (s *WorkstreamStore) apply(ctx context.Context, transition domain.WorkstreamTransition, limits domain.WorkstreamLimits, now time.Time, after func(context.Context, *sql.Tx, domain.WorkstreamTransitionRecord, domain.Workstream) error) (domain.WorkstreamTransitionRecord, error) {
	if s == nil || s.db == nil {
		return domain.WorkstreamTransitionRecord{}, fmt.Errorf("%w: workstream store is not configured", port.ErrWorkstreamUnavailable)
	}
	if err := limits.Validate(); err != nil {
		return domain.WorkstreamTransitionRecord{}, fmt.Errorf("%w: %v", port.ErrWorkstreamValidation, err)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.WorkstreamTransitionRecord{}, fmt.Errorf("%w: begin workstream transition: %v", port.ErrWorkstreamUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := loadWorkstream(ctx, tx, `WHERE workstream_id = ?`, transition.WorkstreamID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkstreamTransitionRecord{}, port.ErrWorkstreamNotFound
	}
	if err != nil {
		return domain.WorkstreamTransitionRecord{}, fmt.Errorf("%w: load current workstream: %v", port.ErrWorkstreamUnavailable, err)
	}
	if err := current.ValidateBinding(transition.Actor, transition.ConversationKey, transition.Project); err != nil {
		return domain.WorkstreamTransitionRecord{}, err
	}
	var existingRecord domain.WorkstreamTransitionRecord
	var existingCommittedAt int64
	err = tx.QueryRowContext(ctx, `SELECT workstream_id, from_revision, to_revision, source, source_id, actor, action,
		payload_digest, payload_json, state_digest, state_json, committed_at
		FROM workstream_transitions WHERE workstream_id = ? AND source_id = ?`, transition.WorkstreamID, transition.SourceID).Scan(
		&existingRecord.WorkstreamID, &existingRecord.FromRevision, &existingRecord.ToRevision,
		&existingRecord.Source, &existingRecord.SourceID, &existingRecord.Actor, &existingRecord.Action,
		&existingRecord.PayloadDigest, &existingRecord.PayloadJSON, &existingRecord.StateDigest, &existingRecord.StateJSON, &existingCommittedAt)
	if err == nil {
		existingRecord.CommittedAt = time.Unix(0, existingCommittedAt).UTC()
		if existingRecord.Actor != transition.Actor || existingRecord.Source != transition.Source || existingRecord.PayloadDigest != transition.PayloadDigestValue() {
			return domain.WorkstreamTransitionRecord{}, domain.ErrWorkstreamSourceConflict
		}
		return existingRecord, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.WorkstreamTransitionRecord{}, fmt.Errorf("%w: inspect workstream source identity: %v", port.ErrWorkstreamUnavailable, err)
	}
	next := current
	record, err := (&next).ApplyTransitionWithLimits(transition, limits, now)
	if errors.Is(err, domain.ErrWorkstreamRevisionConflict) {
		return domain.WorkstreamTransitionRecord{}, port.ErrWorkstreamCASConflict
	}
	if err != nil {
		return domain.WorkstreamTransitionRecord{}, err
	}
	if next.ConversationKey != current.ConversationKey {
		return domain.WorkstreamTransitionRecord{}, fmt.Errorf("%w: conversation binding cannot change", port.ErrWorkstreamValidation)
	}
	if err := ensureNoOtherActiveConversation(ctx, tx, next); err != nil {
		return domain.WorkstreamTransitionRecord{}, err
	}

	updated, err := tx.ExecContext(ctx, `UPDATE workstreams SET
		status = ?, revision = ?, objective = ?, current_phase = ?, continuation_of = ?, updated_at = ?
		WHERE workstream_id = ? AND revision = ?`,
		string(next.Status), next.Revision, next.Objective, next.CurrentPhase, next.ContinuationOf,
		next.UpdatedAt.UTC().UnixNano(), next.ID, current.Revision)
	if err != nil {
		if isWorkstreamConversationConstraint(err) {
			return domain.WorkstreamTransitionRecord{}, port.ErrWorkstreamConversationConflict
		}
		return domain.WorkstreamTransitionRecord{}, fmt.Errorf("%w: update workstream current state: %v", port.ErrWorkstreamUnavailable, err)
	}
	affected, err := updated.RowsAffected()
	if err != nil {
		return domain.WorkstreamTransitionRecord{}, fmt.Errorf("%w: inspect workstream CAS update: %v", port.ErrWorkstreamUnavailable, err)
	}
	if affected != 1 {
		return domain.WorkstreamTransitionRecord{}, port.ErrWorkstreamCASConflict
	}
	if err := persistWorkstreamChildDelta(ctx, tx, transition, next); err != nil {
		return domain.WorkstreamTransitionRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workstream_transitions (
		workstream_id, from_revision, to_revision, source, source_id, actor, action,
		payload_digest, payload_json, state_digest, state_json, committed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.WorkstreamID, record.FromRevision, record.ToRevision, string(record.Source), record.SourceID,
		record.Actor, string(record.Action), record.PayloadDigest, record.PayloadJSON, record.StateDigest,
		record.StateJSON, record.CommittedAt.UTC().UnixNano()); err != nil {
		if isWorkstreamSourceConstraint(err) {
			return domain.WorkstreamTransitionRecord{}, domain.ErrWorkstreamSourceConflict
		}
		return domain.WorkstreamTransitionRecord{}, fmt.Errorf("%w: append workstream transition: %v", port.ErrWorkstreamUnavailable, err)
	}
	if after != nil {
		if err := after(ctx, tx, record, next); err != nil {
			return domain.WorkstreamTransitionRecord{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.WorkstreamTransitionRecord{}, fmt.Errorf("%w: commit workstream transition: %v", port.ErrWorkstreamUnavailable, err)
	}
	return record, nil
}

// CommitVerifiedResultLink joins the verified identity to the same SQLite
// transaction that persists the workstream transition and its child link.
func (s *WorkstreamStore) CommitVerifiedResultLink(ctx context.Context, request port.WorkstreamResultLinkCommit) (domain.WorkstreamTransitionRecord, error) {
	if s == nil || s.db == nil {
		return domain.WorkstreamTransitionRecord{}, fmt.Errorf("%w: workstream store is not configured", port.ErrWorkstreamUnavailable)
	}
	if err := request.VerifiedIdentity.VerifyWorkstreamEligible(); err != nil {
		return domain.WorkstreamTransitionRecord{}, err
	}
	if request.Transition.Action != domain.WorkstreamActionLinkCompletedResult || request.Transition.ResultLink == nil ||
		request.Transition.ResultLink.ResultIdentity != request.VerifiedIdentity.ResultID ||
		request.Verification.ResultID != request.VerifiedIdentity.ResultID ||
		request.Transition.WorkstreamID != request.Verification.WorkstreamID ||
		request.Transition.Actor != request.Verification.Actor ||
		string(request.Transition.ConversationKey) != request.Verification.Conversation ||
		request.Transition.Project != request.Verification.Project {
		return domain.WorkstreamTransitionRecord{}, domain.ErrResultInvalid
	}
	if request.Now.IsZero() {
		request.Now = time.Now().UTC()
	}
	return s.apply(ctx, request.Transition, request.Limits, request.Now, func(ctx context.Context, tx *sql.Tx, record domain.WorkstreamTransitionRecord, _ domain.Workstream) error {
		var producerKind, producerID, storageKind, storageKey, sha256Hex, mediaType, actor, teamID, conversationKey, project, retention, state string
		var producerRevision int
		var bytes, createdAt int64
		err := tx.QueryRowContext(ctx, `SELECT producer_kind, producer_id, producer_revision, storage_kind, storage_key,
			sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state
			FROM result_records WHERE result_id = ?`, request.VerifiedIdentity.ResultID).Scan(
			&producerKind, &producerID, &producerRevision, &storageKind, &storageKey, &sha256Hex, &bytes, &mediaType,
			&actor, &teamID, &conversationKey, &project, &retention, &createdAt, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrResultUnavailable
		}
		if err != nil {
			return fmt.Errorf("read verified result for workstream link: %w", err)
		}
		canonical := domain.ResultIdentity{
			ResultID: request.VerifiedIdentity.ResultID,
			Producer: domain.ResultProducer{Kind: domain.ResultProducerKind(producerKind), ID: producerID, Revision: producerRevision},
			Storage:  domain.ResultStorage{Kind: domain.ResultStorageKind(storageKind), Key: storageKey},
			SHA256:   sha256Hex, Bytes: bytes, MediaType: mediaType,
			Scope:     domain.ResultScope{Actor: actor, TeamID: teamID, ConversationKey: conversationKey, Project: project},
			Retention: domain.ResultRetentionClass(retention), CreatedAt: time.Unix(0, createdAt).UTC(), State: domain.ResultState(state),
		}
		if err := canonical.VerifyWorkstreamEligible(); err != nil || canonical != request.VerifiedIdentity {
			return domain.ErrResultUnavailable
		}
		if canonical.Scope.Actor != request.Verification.Actor || canonical.Scope.TeamID != request.Verification.TeamID ||
			canonical.Scope.ConversationKey != request.Verification.Conversation || canonical.Scope.Project != request.Verification.Project {
			return domain.ErrResultScopeMismatch
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workstream_result_link_results
			(workstream_id, result_link_id, result_id, verified_at) VALUES (?, ?, ?, ?)`,
			record.WorkstreamID, request.Transition.ResultLink.ID, canonical.ResultID, request.Now.UTC().UnixNano()); err != nil {
			return fmt.Errorf("bind verified workstream result: %w", err)
		}
		ownerID := record.WorkstreamID + ":" + request.Transition.ResultLink.ID
		digest := sha256.Sum256([]byte("workstream_result_link\x00" + ownerID + "\x00" + canonical.ResultID))
		if _, err := tx.ExecContext(ctx, `INSERT INTO result_references
			(reference_id, result_id, owner_kind, owner_id, state, created_at) VALUES (?, ?, 'workstream_result_link', ?, 'live', ?)`,
			fmt.Sprintf("%x", digest), canonical.ResultID, ownerID, request.Now.UTC().UnixNano()); err != nil {
			return fmt.Errorf("insert workstream result reference: %w", err)
		}
		return nil
	})
}

var _ port.WorkstreamResultLinkCommitter = (*WorkstreamStore)(nil)

func (s *WorkstreamStore) Transitions(ctx context.Context, workstreamID string) ([]domain.WorkstreamTransitionRecord, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("%w: workstream store is not configured", port.ErrWorkstreamUnavailable)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT workstream_id, from_revision, to_revision, source, source_id, actor, action,
		payload_digest, payload_json, state_digest, state_json, committed_at
		FROM workstream_transitions WHERE workstream_id = ? ORDER BY to_revision`, workstreamID)
	if err != nil {
		return nil, fmt.Errorf("%w: read workstream journal: %v", port.ErrWorkstreamUnavailable, err)
	}
	defer func() { _ = rows.Close() }()
	var records []domain.WorkstreamTransitionRecord
	for rows.Next() {
		var record domain.WorkstreamTransitionRecord
		var committedAt int64
		if err := rows.Scan(&record.WorkstreamID, &record.FromRevision, &record.ToRevision, &record.Source, &record.SourceID, &record.Actor, &record.Action, &record.PayloadDigest, &record.PayloadJSON, &record.StateDigest, &record.StateJSON, &committedAt); err != nil {
			return nil, fmt.Errorf("%w: scan workstream journal: %v", port.ErrWorkstreamUnavailable, err)
		}
		record.CommittedAt = time.Unix(0, committedAt).UTC()
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: read workstream journal rows: %v", port.ErrWorkstreamUnavailable, err)
	}
	return records, nil
}

func storageWorkstreamLimits() domain.WorkstreamLimits {
	return domain.WorkstreamLimits{
		MaxNonTerminalTasks:    domain.HardMaxWorkstreamTasks,
		MaxDependenciesPerTask: domain.HardMaxWorkstreamDependencies,
		MaxTasks:               domain.HardMaxWorkstreamCollections,
		MaxConstraints:         domain.HardMaxWorkstreamCollections,
		MaxDecisions:           domain.HardMaxWorkstreamCollections,
		MaxQuestions:           domain.HardMaxWorkstreamCollections,
		MaxResultLinks:         domain.HardMaxWorkstreamCollections,
		MaxTextRunes:           domain.HardMaxWorkstreamTextRunes,
		MaxIDRunes:             domain.HardMaxWorkstreamIDRunes,
	}
}

func isWorkstreamSourceConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "workstream_transitions_by_source")
}

func ensureNoOtherActiveConversation(ctx context.Context, tx *sql.Tx, workstream domain.Workstream) error {
	var existingID string
	err := tx.QueryRowContext(ctx, `SELECT workstream_id FROM workstreams
		WHERE conversation_key = ? AND workstream_id != ?
			AND status IN ('proposed', 'active', 'paused', 'blocked') LIMIT 1`,
		string(workstream.ConversationKey), workstream.ID).Scan(&existingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: inspect active conversation binding: %v", port.ErrWorkstreamUnavailable, err)
	}
	return fmt.Errorf("%w: conversation is already bound to %q", port.ErrWorkstreamConversationConflict, existingID)
}

func isWorkstreamConversationConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "workstreams.conversation_key")
}

type workstreamQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadWorkstream(ctx context.Context, queryer workstreamQueryer, where string, args ...any) (domain.Workstream, error) {
	var workstream domain.Workstream
	var conversationKey, status string
	var createdAt, updatedAt int64
	err := queryer.QueryRowContext(ctx, `SELECT workstream_id, conversation_key, owner_actor, project,
		status, revision, objective, current_phase, continuation_of, created_at, updated_at
		FROM workstreams `+where, args...).Scan(
		&workstream.ID, &conversationKey, &workstream.OwnerActor, &workstream.Project, &status,
		&workstream.Revision, &workstream.Objective, &workstream.CurrentPhase, &workstream.ContinuationOf,
		&createdAt, &updatedAt)
	if err != nil {
		return domain.Workstream{}, err
	}
	workstream.ConversationKey = domain.ConversationKey(conversationKey)
	workstream.Status = domain.WorkstreamStatus(status)
	workstream.CreatedAt = time.Unix(0, createdAt).UTC()
	workstream.UpdatedAt = time.Unix(0, updatedAt).UTC()

	rows, err := queryer.QueryContext(ctx, `SELECT constraint_id, text, source_id
		FROM workstream_constraints WHERE workstream_id = ? ORDER BY constraint_id`, workstream.ID)
	if err != nil {
		return domain.Workstream{}, err
	}
	for rows.Next() {
		var constraint domain.WorkstreamConstraint
		if err := rows.Scan(&constraint.ID, &constraint.Text, &constraint.SourceID); err != nil {
			_ = rows.Close()
			return domain.Workstream{}, err
		}
		workstream.Constraints = append(workstream.Constraints, constraint)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return domain.Workstream{}, err
	}
	if err := rows.Close(); err != nil {
		return domain.Workstream{}, err
	}

	rows, err = queryer.QueryContext(ctx, `SELECT decision_id, status, proposal, source, rationale, effective_revision
		FROM workstream_decisions WHERE workstream_id = ? ORDER BY decision_id`, workstream.ID)
	if err != nil {
		return domain.Workstream{}, err
	}
	for rows.Next() {
		var decision domain.WorkstreamDecision
		var status string
		if err := rows.Scan(&decision.ID, &status, &decision.Proposal, &decision.Source, &decision.Rationale, &decision.EffectiveRevision); err != nil {
			_ = rows.Close()
			return domain.Workstream{}, err
		}
		decision.Status = domain.DecisionStatus(status)
		workstream.Decisions = append(workstream.Decisions, decision)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return domain.Workstream{}, err
	}
	if err := rows.Close(); err != nil {
		return domain.Workstream{}, err
	}

	rows, err = queryer.QueryContext(ctx, `SELECT task_id, project, description, status, result_identity,
		confirmation_identity, confirmation_status, execution_identity, integrated
		FROM workstream_tasks WHERE workstream_id = ? ORDER BY task_id`, workstream.ID)
	if err != nil {
		return domain.Workstream{}, err
	}
	taskIndexes := make(map[string]int)
	for rows.Next() {
		var task domain.WorkstreamTask
		var status, confirmationStatus string
		var integrated int
		if err := rows.Scan(&task.ID, &task.Project, &task.Description, &status, &task.ResultIdentity,
			&task.ConfirmationIdentity, &confirmationStatus, &task.ExecutionIdentity, &integrated); err != nil {
			_ = rows.Close()
			return domain.Workstream{}, err
		}
		task.Status = domain.TaskStatus(status)
		task.ConfirmationStatus = domain.TaskConfirmationStatus(confirmationStatus)
		task.Integrated = integrated != 0
		taskIndexes[task.ID] = len(workstream.Tasks)
		workstream.Tasks = append(workstream.Tasks, task)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return domain.Workstream{}, err
	}
	if err := rows.Close(); err != nil {
		return domain.Workstream{}, err
	}

	rows, err = queryer.QueryContext(ctx, `SELECT task_id, input_identity
		FROM workstream_task_inputs WHERE workstream_id = ? ORDER BY task_id, input_identity`, workstream.ID)
	if err != nil {
		return domain.Workstream{}, err
	}
	for rows.Next() {
		var taskID, input string
		if err := rows.Scan(&taskID, &input); err != nil {
			_ = rows.Close()
			return domain.Workstream{}, err
		}
		index, ok := taskIndexes[taskID]
		if !ok {
			_ = rows.Close()
			return domain.Workstream{}, fmt.Errorf("task input references unknown task %q", taskID)
		}
		workstream.Tasks[index].RequiredInputs = append(workstream.Tasks[index].RequiredInputs, input)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return domain.Workstream{}, err
	}
	if err := rows.Close(); err != nil {
		return domain.Workstream{}, err
	}

	rows, err = queryer.QueryContext(ctx, `SELECT task_id, dependency_id
		FROM workstream_task_dependencies WHERE workstream_id = ? ORDER BY task_id, dependency_id`, workstream.ID)
	if err != nil {
		return domain.Workstream{}, err
	}
	for rows.Next() {
		var taskID, dependency string
		if err := rows.Scan(&taskID, &dependency); err != nil {
			_ = rows.Close()
			return domain.Workstream{}, err
		}
		index, ok := taskIndexes[taskID]
		if !ok {
			_ = rows.Close()
			return domain.Workstream{}, fmt.Errorf("task dependency references unknown task %q", taskID)
		}
		workstream.Tasks[index].Dependencies = append(workstream.Tasks[index].Dependencies, dependency)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return domain.Workstream{}, err
	}
	if err := rows.Close(); err != nil {
		return domain.Workstream{}, err
	}

	rows, err = queryer.QueryContext(ctx, `SELECT question_id, text, status, resolution, source_id
		FROM workstream_questions WHERE workstream_id = ? ORDER BY question_id`, workstream.ID)
	if err != nil {
		return domain.Workstream{}, err
	}
	for rows.Next() {
		var question domain.WorkstreamQuestion
		var status string
		if err := rows.Scan(&question.ID, &question.Text, &status, &question.Resolution, &question.SourceID); err != nil {
			_ = rows.Close()
			return domain.Workstream{}, err
		}
		question.Status = domain.QuestionStatus(status)
		workstream.OpenQuestions = append(workstream.OpenQuestions, question)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return domain.Workstream{}, err
	}
	if err := rows.Close(); err != nil {
		return domain.Workstream{}, err
	}

	rows, err = queryer.QueryContext(ctx, `SELECT result_link_id, task_id, result_identity, description
		FROM workstream_result_links WHERE workstream_id = ? ORDER BY result_link_id`, workstream.ID)
	if err != nil {
		return domain.Workstream{}, err
	}
	for rows.Next() {
		var result domain.WorkstreamResultLink
		if err := rows.Scan(&result.ID, &result.TaskID, &result.ResultIdentity, &result.Description); err != nil {
			_ = rows.Close()
			return domain.Workstream{}, err
		}
		workstream.ResultLinks = append(workstream.ResultLinks, result)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return domain.Workstream{}, err
	}
	if err := rows.Close(); err != nil {
		return domain.Workstream{}, err
	}
	return workstream, nil
}

func insertWorkstreamChildren(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, workstream domain.Workstream) error {
	for _, constraint := range workstream.Constraints {
		if _, err := exec.ExecContext(ctx, `INSERT INTO workstream_constraints (workstream_id, constraint_id, text, source_id) VALUES (?, ?, ?, ?)`, workstream.ID, constraint.ID, constraint.Text, constraint.SourceID); err != nil {
			return fmt.Errorf("%w: insert workstream constraint: %v", port.ErrWorkstreamUnavailable, err)
		}
	}
	for _, decision := range workstream.Decisions {
		if _, err := exec.ExecContext(ctx, `INSERT INTO workstream_decisions (workstream_id, decision_id, status, proposal, source, rationale, effective_revision) VALUES (?, ?, ?, ?, ?, ?, ?)`, workstream.ID, decision.ID, string(decision.Status), decision.Proposal, decision.Source, decision.Rationale, decision.EffectiveRevision); err != nil {
			return fmt.Errorf("%w: insert workstream decision: %v", port.ErrWorkstreamUnavailable, err)
		}
	}
	for _, task := range workstream.Tasks {
		confirmationStatus := task.ConfirmationStatus
		if confirmationStatus == "" {
			confirmationStatus = domain.TaskConfirmationNotRequired
		}
		if _, err := exec.ExecContext(ctx, `INSERT INTO workstream_tasks (
			workstream_id, task_id, project, description, status, result_identity,
			confirmation_identity, confirmation_status, execution_identity, integrated)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, workstream.ID, task.ID, task.Project, task.Description,
			string(task.Status), task.ResultIdentity, task.ConfirmationIdentity, string(confirmationStatus), task.ExecutionIdentity, boolInt(task.Integrated)); err != nil {
			return fmt.Errorf("%w: insert workstream task: %v", port.ErrWorkstreamUnavailable, err)
		}
	}
	for _, task := range workstream.Tasks {
		for _, input := range task.RequiredInputs {
			if _, err := exec.ExecContext(ctx, `INSERT INTO workstream_task_inputs (workstream_id, task_id, input_identity) VALUES (?, ?, ?)`, workstream.ID, task.ID, input); err != nil {
				return fmt.Errorf("%w: insert workstream task input: %v", port.ErrWorkstreamUnavailable, err)
			}
		}
		for _, dependency := range task.Dependencies {
			if _, err := exec.ExecContext(ctx, `INSERT INTO workstream_task_dependencies (workstream_id, task_id, dependency_id) VALUES (?, ?, ?)`, workstream.ID, task.ID, dependency); err != nil {
				return fmt.Errorf("%w: insert workstream task dependency: %v", port.ErrWorkstreamUnavailable, err)
			}
		}
	}
	for _, question := range workstream.OpenQuestions {
		status := question.Status
		if status == "" {
			status = domain.QuestionOpen
		}
		if _, err := exec.ExecContext(ctx, `INSERT INTO workstream_questions (workstream_id, question_id, text, status, resolution, source_id) VALUES (?, ?, ?, ?, ?, ?)`, workstream.ID, question.ID, question.Text, string(status), question.Resolution, question.SourceID); err != nil {
			return fmt.Errorf("%w: insert workstream question: %v", port.ErrWorkstreamUnavailable, err)
		}
	}
	for _, result := range workstream.ResultLinks {
		if _, err := exec.ExecContext(ctx, `INSERT INTO workstream_result_links (workstream_id, result_link_id, task_id, result_identity, description) VALUES (?, ?, ?, ?, ?)`, workstream.ID, result.ID, result.TaskID, result.ResultIdentity, result.Description); err != nil {
			return fmt.Errorf("%w: insert workstream result link: %v", port.ErrWorkstreamUnavailable, err)
		}
	}
	return nil
}

func persistWorkstreamChildDelta(ctx context.Context, tx *sql.Tx, transition domain.WorkstreamTransition, next domain.Workstream) error {
	insertTask := func(task domain.WorkstreamTask) error {
		confirmationStatus := task.ConfirmationStatus
		if confirmationStatus == "" {
			confirmationStatus = domain.TaskConfirmationNotRequired
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workstream_tasks (
			workstream_id, task_id, project, description, status, result_identity,
			confirmation_identity, confirmation_status, execution_identity, integrated)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, next.ID, task.ID, task.Project, task.Description,
			string(task.Status), task.ResultIdentity, task.ConfirmationIdentity, string(confirmationStatus), task.ExecutionIdentity, boolInt(task.Integrated)); err != nil {
			return fmt.Errorf("%w: insert workstream task delta: %v", port.ErrWorkstreamUnavailable, err)
		}
		for _, input := range task.RequiredInputs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO workstream_task_inputs (workstream_id, task_id, input_identity) VALUES (?, ?, ?)`, next.ID, task.ID, input); err != nil {
				return fmt.Errorf("%w: insert workstream task input delta: %v", port.ErrWorkstreamUnavailable, err)
			}
		}
		for _, dependency := range task.Dependencies {
			if _, err := tx.ExecContext(ctx, `INSERT INTO workstream_task_dependencies (workstream_id, task_id, dependency_id) VALUES (?, ?, ?)`, next.ID, task.ID, dependency); err != nil {
				return fmt.Errorf("%w: insert workstream task dependency delta: %v", port.ErrWorkstreamUnavailable, err)
			}
		}
		return nil
	}

	findTask := func(id string) (domain.WorkstreamTask, bool) {
		for _, task := range next.Tasks {
			if task.ID == id {
				return task, true
			}
		}
		return domain.WorkstreamTask{}, false
	}
	findDecision := func(id string) (domain.WorkstreamDecision, bool) {
		for _, decision := range next.Decisions {
			if decision.ID == id {
				return decision, true
			}
		}
		return domain.WorkstreamDecision{}, false
	}
	findQuestion := func(id string) (domain.WorkstreamQuestion, bool) {
		for _, question := range next.OpenQuestions {
			if question.ID == id {
				return question, true
			}
		}
		return domain.WorkstreamQuestion{}, false
	}
	switch transition.Action {
	case domain.WorkstreamActionProposeTask:
		task, ok := findTask(transition.TaskID)
		if !ok && transition.Task != nil {
			task, ok = findTask(transition.Task.ID)
		}
		if !ok {
			return fmt.Errorf("%w: task delta is missing", port.ErrWorkstreamValidation)
		}
		return insertTask(task)
	case domain.WorkstreamActionRecordConstraint:
		if _, err := tx.ExecContext(ctx, `INSERT INTO workstream_constraints (workstream_id, constraint_id, text, source_id) VALUES (?, ?, ?, ?)`, next.ID, transition.Constraint.ID, transition.Constraint.Text, transition.Constraint.SourceID); err != nil {
			return fmt.Errorf("%w: insert workstream constraint delta: %v", port.ErrWorkstreamUnavailable, err)
		}
	case domain.WorkstreamActionProposeDecision:
		decision, ok := findDecision(transition.Decision.ID)
		if !ok {
			return fmt.Errorf("%w: decision delta is missing", port.ErrWorkstreamValidation)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workstream_decisions (workstream_id, decision_id, status, proposal, source, rationale, effective_revision) VALUES (?, ?, ?, ?, ?, ?, ?)`, next.ID, decision.ID, string(decision.Status), decision.Proposal, decision.Source, decision.Rationale, decision.EffectiveRevision); err != nil {
			return fmt.Errorf("%w: insert workstream decision delta: %v", port.ErrWorkstreamUnavailable, err)
		}
	case domain.WorkstreamActionRequestHumanDecision:
		question, ok := findQuestion(transition.Question.ID)
		if !ok {
			return fmt.Errorf("%w: question delta is missing", port.ErrWorkstreamValidation)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workstream_questions (workstream_id, question_id, text, status, resolution, source_id) VALUES (?, ?, ?, ?, ?, ?)`, next.ID, question.ID, question.Text, string(question.Status), question.Resolution, question.SourceID); err != nil {
			return fmt.Errorf("%w: insert workstream question delta: %v", port.ErrWorkstreamUnavailable, err)
		}
	case domain.WorkstreamActionApproveDecision, domain.WorkstreamActionRejectDecision:
		status := domain.DecisionApproved
		if transition.Action == domain.WorkstreamActionRejectDecision {
			status = domain.DecisionRejected
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workstream_decisions SET status = ?, effective_revision = ? WHERE workstream_id = ? AND decision_id = ?`, string(status), next.Revision, next.ID, transition.DecisionID); err != nil {
			return fmt.Errorf("%w: update workstream decision delta: %v", port.ErrWorkstreamUnavailable, err)
		}
	case domain.WorkstreamActionResolveQuestion:
		if _, err := tx.ExecContext(ctx, `UPDATE workstream_questions SET status = ?, resolution = ? WHERE workstream_id = ? AND question_id = ?`, string(domain.QuestionResolved), transition.QuestionResolution, next.ID, transition.QuestionID); err != nil {
			return fmt.Errorf("%w: update workstream question delta: %v", port.ErrWorkstreamUnavailable, err)
		}
	case domain.WorkstreamActionRejectTask:
		task, ok := findTask(transition.TaskID)
		if !ok {
			return fmt.Errorf("%w: task delta is missing", port.ErrWorkstreamValidation)
		}
		confirmationStatus := task.ConfirmationStatus
		if confirmationStatus == "" {
			confirmationStatus = domain.TaskConfirmationNotRequired
		}
		updated, err := tx.ExecContext(ctx, `UPDATE workstream_tasks SET status = ?, result_identity = ?, confirmation_identity = ?, confirmation_status = ?, execution_identity = ?, integrated = ? WHERE workstream_id = ? AND task_id = ?`, string(task.Status), task.ResultIdentity, task.ConfirmationIdentity, string(confirmationStatus), task.ExecutionIdentity, boolInt(task.Integrated), next.ID, task.ID)
		if err != nil {
			return fmt.Errorf("%w: update workstream task delta: %v", port.ErrWorkstreamUnavailable, err)
		}
		if affected, err := updated.RowsAffected(); err != nil || affected != 1 {
			return fmt.Errorf("%w: update workstream task delta affected %d rows", port.ErrWorkstreamUnavailable, affected)
		}
	case domain.WorkstreamActionStartTask:
		task, ok := findTask(transition.TaskID)
		if !ok {
			return fmt.Errorf("%w: task delta is missing", port.ErrWorkstreamValidation)
		}
		confirmationStatus := task.ConfirmationStatus
		if confirmationStatus == "" {
			confirmationStatus = domain.TaskConfirmationNotRequired
		}
		updated, err := tx.ExecContext(ctx, `UPDATE workstream_tasks SET status = ?, execution_identity = ?, confirmation_status = ?, integrated = ? WHERE workstream_id = ? AND task_id = ?`, string(task.Status), task.ExecutionIdentity, string(confirmationStatus), boolInt(task.Integrated), next.ID, task.ID)
		if err != nil {
			return fmt.Errorf("%w: update workstream task start delta: %v", port.ErrWorkstreamUnavailable, err)
		}
		if affected, err := updated.RowsAffected(); err != nil || affected != 1 {
			return fmt.Errorf("%w: update workstream task start delta affected %d rows", port.ErrWorkstreamUnavailable, affected)
		}
	case domain.WorkstreamActionLinkCompletedResult:
		if transition.ResultLink == nil {
			return fmt.Errorf("%w: result link delta is missing", port.ErrWorkstreamValidation)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workstream_result_links
			(workstream_id, result_link_id, task_id, result_identity, description)
			VALUES (?, ?, ?, ?, ?)`, next.ID, transition.ResultLink.ID, transition.ResultLink.TaskID,
			transition.ResultLink.ResultIdentity, transition.ResultLink.Description); err != nil {
			return fmt.Errorf("%w: insert workstream result link delta: %v", port.ErrWorkstreamUnavailable, err)
		}
	}
	return nil
}
