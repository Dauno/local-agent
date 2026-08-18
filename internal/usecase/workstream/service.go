// Package workstream owns actor-bound orchestration transitions. It keeps
// trusted Slack identity separate from selectors and payloads supplied by a
// model or a human command.
package workstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var ErrDisabled = errors.New("workstream orchestration is disabled")
var ErrResultHandlesDisabled = errors.New("native result handles are disabled")

type Config struct {
	Enabled              bool
	ResultHandlesEnabled bool
	Limits               domain.WorkstreamLimits
	AllowedProjects      map[string]struct{}
}

type Dependencies struct {
	Store          port.WorkstreamStore
	Clock          port.Clock
	ResultVerifier port.ResultIdentityVerifier
	LinkCommitter  port.WorkstreamResultLinkCommitter
	ResultReader   port.TrustedResultStore
}

type Binding = port.WorkstreamBinding

type Service struct {
	cfg           Config
	store         port.WorkstreamStore
	clock         port.Clock
	verifier      port.ResultIdentityVerifier
	linkCommitter port.WorkstreamResultLinkCommitter
	resultReader  port.TrustedResultStore
}

var _ port.WorkstreamMutator = (*Service)(nil)
var _ port.WorkstreamSnapshotReader = (*Service)(nil)
var _ port.WorkstreamService = (*Service)(nil)

func New(cfg Config, deps Dependencies) (*Service, error) {
	if deps.Store == nil {
		return nil, errors.New("workstream store is required")
	}
	if cfg.Limits.MaxNonTerminalTasks == 0 && cfg.Limits.MaxDependenciesPerTask == 0 {
		cfg.Limits = domain.DefaultWorkstreamLimits()
	}
	cfg.Limits = cfg.Limits.WithDefaults()
	if err := cfg.Limits.Validate(); err != nil {
		return nil, err
	}
	if cfg.Enabled && len(cfg.AllowedProjects) == 0 {
		return nil, errors.New("enabled workstream service requires registered projects")
	}
	if deps.Clock == nil {
		deps.Clock = port.SystemClock{}
	}
	allowedProjects := make(map[string]struct{}, len(cfg.AllowedProjects))
	for project := range cfg.AllowedProjects {
		allowedProjects[project] = struct{}{}
	}
	cfg.AllowedProjects = allowedProjects
	return &Service{cfg: cfg, store: deps.Store, clock: deps.Clock, verifier: deps.ResultVerifier, linkCommitter: deps.LinkCommitter, resultReader: deps.ResultReader}, nil
}

func (s *Service) Create(ctx context.Context, binding Binding, id, objective string) (domain.WorkstreamSnapshot, error) {
	return s.create(ctx, binding, id, objective, domain.WorkstreamSourceHuman, id)
}

func (s *Service) CreateHuman(ctx context.Context, binding Binding, id, objective, sourceID string) (domain.WorkstreamSnapshot, error) {
	return s.create(ctx, binding, id, objective, domain.WorkstreamSourceHuman, sourceID)
}

// ValidateCreate checks a root proposal before asking the owner to approve it.
func (s *Service) ValidateCreate(ctx context.Context, binding Binding, id, objective string) error {
	if _, err := s.newWorkstream(binding, id, objective); err != nil {
		return err
	}
	_, err := s.store.ActiveForConversation(ctx, binding.ConversationKey)
	if err == nil {
		return port.ErrWorkstreamConversationConflict
	}
	if errors.Is(err, port.ErrWorkstreamNotFound) {
		return nil
	}
	return fmt.Errorf("inspect active workstream: %w", err)
}

func (s *Service) CreateFromRoot(ctx context.Context, binding Binding, id, objective, sourceID string) (domain.WorkstreamSnapshot, error) {
	return s.create(ctx, binding, id, objective, domain.WorkstreamSourceRoot, sourceID)
}

func (s *Service) create(ctx context.Context, binding Binding, id, objective string, source domain.WorkstreamTransitionSource, sourceID string) (domain.WorkstreamSnapshot, error) {
	workstream, err := s.newWorkstream(binding, id, objective)
	if err != nil {
		return domain.WorkstreamSnapshot{}, err
	}
	if err := s.store.Create(ctx, workstream, source, sourceID); err != nil {
		return domain.WorkstreamSnapshot{}, fmt.Errorf("create workstream: %w", err)
	}
	return s.Get(ctx, binding, id)
}

func (s *Service) newWorkstream(binding Binding, id, objective string) (domain.Workstream, error) {
	if err := s.requireEnabled(); err != nil {
		return domain.Workstream{}, err
	}
	if err := s.validateBinding(binding); err != nil {
		return domain.Workstream{}, err
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(objective) == "" {
		return domain.Workstream{}, errors.New("workstream ID and objective are required")
	}
	now := s.clock.Now().UTC()
	workstream := domain.Workstream{
		ID: id, ConversationKey: binding.ConversationKey, OwnerActor: binding.Actor,
		Project: binding.Project, Status: domain.WorkstreamProposed, Objective: objective,
		Revision: 0, CreatedAt: now, UpdatedAt: now,
	}
	if err := workstream.ValidateWithLimits(s.cfg.Limits); err != nil {
		return domain.Workstream{}, err
	}
	return workstream, nil
}

func (s *Service) Get(ctx context.Context, binding Binding, id string) (domain.WorkstreamSnapshot, error) {
	if err := s.requireEnabled(); err != nil {
		return domain.WorkstreamSnapshot{}, err
	}
	if err := s.validateBinding(binding); err != nil {
		return domain.WorkstreamSnapshot{}, err
	}
	if strings.TrimSpace(id) == "" {
		return domain.WorkstreamSnapshot{}, errors.New("workstream ID is required")
	}
	workstream, err := s.store.Get(ctx, id)
	if err != nil {
		return domain.WorkstreamSnapshot{}, fmt.Errorf("get workstream: %w", err)
	}
	if err := workstream.ValidateBinding(binding.Actor, binding.ConversationKey, binding.Project); err != nil {
		return domain.WorkstreamSnapshot{}, err
	}
	if err := workstream.ValidateWithLimits(s.cfg.Limits); err != nil {
		return domain.WorkstreamSnapshot{}, err
	}
	return workstream.Snapshot(), nil
}

// SnapshotForActivation reads the current trusted workstream state without
// accepting a project selector from the activation payload. Publication has
// already admitted the binding; this second read keeps the frame fail-closed
// if the workstream is no longer active or has become invalid.
func (s *Service) SnapshotForActivation(ctx context.Context, workstreamID, actor string, conversationKey domain.ConversationKey) (domain.WorkstreamSnapshot, error) {
	if err := s.requireEnabled(); err != nil {
		return domain.WorkstreamSnapshot{}, err
	}
	if strings.TrimSpace(workstreamID) == "" || strings.TrimSpace(actor) == "" || strings.TrimSpace(string(conversationKey)) == "" {
		return domain.WorkstreamSnapshot{}, errors.New("activation workstream binding is required")
	}
	workstream, err := s.store.Get(ctx, workstreamID)
	if err != nil {
		return domain.WorkstreamSnapshot{}, fmt.Errorf("get activation workstream: %w", err)
	}
	if err := workstream.ValidateBinding(actor, conversationKey, workstream.Project); err != nil {
		return domain.WorkstreamSnapshot{}, err
	}
	if err := s.validateBinding(Binding{Actor: actor, ConversationKey: conversationKey, Project: workstream.Project}); err != nil {
		return domain.WorkstreamSnapshot{}, err
	}
	if workstream.Status != domain.WorkstreamActive {
		return domain.WorkstreamSnapshot{}, fmt.Errorf("%w: status is %q", domain.ErrWorkstreamNotActive, workstream.Status)
	}
	if err := workstream.ValidateWithLimits(s.cfg.Limits); err != nil {
		return domain.WorkstreamSnapshot{}, err
	}
	return workstream.Snapshot(), nil
}

// CompletionBindingForTask returns a binding only for an exact task already
// running in the caller's active workstream. No model value can choose the
// workstream, task ID, execution identity, or revision.
func (s *Service) CompletionBindingForTask(ctx context.Context, actor string, conversationKey domain.ConversationKey, project, task string) (domain.ExternalAgentJobCompletionBinding, bool, error) {
	if err := s.requireEnabled(); err != nil {
		return domain.ExternalAgentJobCompletionBinding{}, false, err
	}
	binding := Binding{Actor: actor, ConversationKey: conversationKey, Project: project}
	if err := s.validateBinding(binding); err != nil {
		return domain.ExternalAgentJobCompletionBinding{}, false, err
	}
	workstream, err := s.store.ActiveForConversation(ctx, conversationKey)
	if errors.Is(err, port.ErrWorkstreamNotFound) {
		return domain.ExternalAgentJobCompletionBinding{}, false, nil
	}
	if err != nil {
		return domain.ExternalAgentJobCompletionBinding{}, false, fmt.Errorf("get active workstream for external-agent job: %w", err)
	}
	if err := workstream.ValidateBinding(actor, conversationKey, project); err != nil {
		return domain.ExternalAgentJobCompletionBinding{}, false, err
	}
	if workstream.Status != domain.WorkstreamActive {
		return domain.ExternalAgentJobCompletionBinding{}, false, nil
	}
	for _, candidate := range workstream.Tasks {
		if candidate.Project == project && candidate.Description == task && candidate.Status == domain.TaskRunning && candidate.ExecutionIdentity != "" {
			return domain.ExternalAgentJobCompletionBinding{
				WorkstreamID: workstream.ID, TaskID: candidate.ID, ExecutionIdentity: candidate.ExecutionIdentity, AdmissionRevision: workstream.Revision,
				RequiredInputs: append([]string(nil), candidate.RequiredInputs...),
			}, true, nil
		}
	}
	return domain.ExternalAgentJobCompletionBinding{}, false, nil
}

func (s *Service) Active(ctx context.Context, binding Binding) (domain.WorkstreamSnapshot, error) {
	if err := s.requireEnabled(); err != nil {
		return domain.WorkstreamSnapshot{}, err
	}
	if err := s.validateBinding(binding); err != nil {
		return domain.WorkstreamSnapshot{}, err
	}
	workstream, err := s.store.ActiveForConversation(ctx, binding.ConversationKey)
	if err != nil {
		return domain.WorkstreamSnapshot{}, fmt.Errorf("get active workstream: %w", err)
	}
	if err := workstream.ValidateBinding(binding.Actor, binding.ConversationKey, binding.Project); err != nil {
		return domain.WorkstreamSnapshot{}, err
	}
	if err := workstream.ValidateWithLimits(s.cfg.Limits); err != nil {
		return domain.WorkstreamSnapshot{}, err
	}
	return workstream.Snapshot(), nil
}

// Apply binds all authority fields to the trusted invocation. Callers provide
// only the action payload, expected revision, and provenance identity; model
// selectors cannot replace actor, conversation, project, or source kind.
func (s *Service) Apply(ctx context.Context, binding Binding, source domain.WorkstreamTransitionSource, transition domain.WorkstreamTransition) (domain.WorkstreamTransitionRecord, domain.WorkstreamSnapshot, error) {
	// Confirmation proofs are accepted only through ApplyConfirmed, after a
	// host-owned confirmation store has consumed the matching decision.
	transition.Confirmation = nil
	return s.apply(ctx, binding, source, transition)
}

// ApplyConfirmed applies a root transition after the host has verified and
// consumed a bound confirmation. The proof is still rebound to the trusted
// invocation before the domain policy checks it.
func (s *Service) ApplyConfirmed(ctx context.Context, binding Binding, source domain.WorkstreamTransitionSource, transition domain.WorkstreamTransition, confirmation domain.WorkstreamConfirmation) (domain.WorkstreamTransitionRecord, domain.WorkstreamSnapshot, error) {
	if confirmation.Action == "" {
		confirmation.Action = transition.Action
	}
	if confirmation.PayloadDigest == "" {
		confirmation.PayloadDigest = transition.PayloadDigestValue()
	}
	transition.Confirmation = &confirmation
	return s.apply(ctx, binding, source, transition)
}

// ApplyHostConfirmed adapts an already-consumed host confirmation (such as the
// ADK confirmation resumed by the bot use case) to the domain proof contract.
func (s *Service) ApplyHostConfirmed(ctx context.Context, binding Binding, source domain.WorkstreamTransitionSource, transition domain.WorkstreamTransition, confirmationID string) (domain.WorkstreamTransitionRecord, domain.WorkstreamSnapshot, error) {
	if strings.TrimSpace(confirmationID) == "" {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, domain.ErrWorkstreamConfirmationRequired
	}
	confirmation := domain.WorkstreamConfirmation{ID: confirmationID, Action: transition.Action, PayloadDigest: transition.PayloadDigestValue(), Approved: true, ExpiresAt: s.clock.Now().UTC().Add(time.Minute)}
	return s.ApplyConfirmed(ctx, binding, source, transition, confirmation)
}

// ApplyHuman records a direct, actor-bound human command. The caller supplies
// only the project selector and action payload; source and identity fields are
// replaced with the trusted human invocation by apply.
func (s *Service) ApplyHuman(ctx context.Context, binding Binding, transition domain.WorkstreamTransition) (domain.WorkstreamTransitionRecord, domain.WorkstreamSnapshot, error) {
	return s.Apply(ctx, binding, domain.WorkstreamSourceHuman, transition)
}

// LinkCompletedResult admits only a fully verified result into the durable
// workstream journal. Result scope is rebuilt from the trusted invocation; the
// model-provided link can never select its own identity or scope.
func (s *Service) LinkCompletedResult(ctx context.Context, binding Binding, teamID, resultID string, transition domain.WorkstreamTransition, confirmationID string) (domain.WorkstreamTransitionRecord, domain.WorkstreamSnapshot, error) {
	if err := s.requireEnabled(); err != nil {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, err
	}
	if !s.cfg.ResultHandlesEnabled {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, ErrResultHandlesDisabled
	}
	if err := s.validateBinding(binding); err != nil {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, err
	}
	if s.verifier == nil || s.linkCommitter == nil || strings.TrimSpace(teamID) == "" || strings.TrimSpace(confirmationID) == "" {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, domain.ErrResultUnavailable
	}
	if transition.Action != domain.WorkstreamActionLinkCompletedResult || transition.ResultLink == nil || strings.TrimSpace(transition.WorkstreamID) == "" {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, domain.ErrResultInvalid
	}
	verification, identity, err := s.verifyResultForWorkstream(ctx, binding, teamID, resultID, transition.WorkstreamID)
	if err != nil {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, err
	}
	transition.Source = domain.WorkstreamSourceRoot
	transition.SourceID = confirmationID
	transition.Actor = binding.Actor
	transition.ConversationKey = binding.ConversationKey
	transition.Project = binding.Project
	transition.ResultLink.ResultIdentity = identity.ResultID
	transition.Confirmation = &domain.WorkstreamConfirmation{
		ID: confirmationID, WorkstreamID: transition.WorkstreamID, ExpectedRevision: transition.ExpectedRevision,
		Actor: binding.Actor, ConversationKey: binding.ConversationKey, Project: binding.Project,
		Action: transition.Action, PayloadDigest: transition.PayloadDigestValue(), Approved: true,
		ExpiresAt: s.clock.Now().UTC().Add(time.Minute),
	}
	record, err := s.linkCommitter.CommitVerifiedResultLink(ctx, port.WorkstreamResultLinkCommit{
		Verification: verification, VerifiedIdentity: identity, Transition: transition, Limits: s.cfg.Limits, Now: s.clock.Now().UTC(),
	})
	if err != nil {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, err
	}
	var applied domain.Workstream
	if err := json.Unmarshal([]byte(record.StateJSON), &applied); err != nil || !domain.VerifyWorkstreamStateJSON(record.StateJSON, record.StateDigest) {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, domain.ErrResultUnavailable
	}
	if err := applied.ValidateBinding(binding.Actor, binding.ConversationKey, binding.Project); err != nil {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, err
	}
	return record, applied.Snapshot(), nil
}

// ResultHandleForWorkstream returns only the bounded handle after binding the
// requested result to the active trusted workstream.
func (s *Service) ResultHandleForWorkstream(ctx context.Context, binding Binding, teamID, resultID, workstreamID string) (domain.ResultHandle, error) {
	_, identity, err := s.verifyResultForWorkstream(ctx, binding, teamID, resultID, workstreamID)
	if err != nil {
		return domain.ResultHandle{}, err
	}
	if s.resultReader == nil {
		return domain.ResultHandle{}, domain.ErrResultUnavailable
	}
	_, handle, err := s.resultReader.Resolve(ctx, identity.ResultID, identity.Scope)
	return handle, err
}

// ReadResultChunkForWorkstream returns one fully verified bounded range. The
// result scope is taken from the verified identity, never from tool arguments.
func (s *Service) ReadResultChunkForWorkstream(ctx context.Context, binding Binding, teamID, resultID, workstreamID string, offsetBytes, maxBytes int64) (domain.ResultChunk, error) {
	_, identity, err := s.verifyResultForWorkstream(ctx, binding, teamID, resultID, workstreamID)
	if err != nil {
		return domain.ResultChunk{}, err
	}
	if s.resultReader == nil {
		return domain.ResultChunk{}, domain.ErrResultUnavailable
	}
	return s.resultReader.ReadRange(ctx, identity.ResultID, identity.Scope, offsetBytes, maxBytes)
}

func (s *Service) verifyResultForWorkstream(ctx context.Context, binding Binding, teamID, resultID, workstreamID string) (port.WorkstreamResultVerification, domain.ResultIdentity, error) {
	if err := s.requireEnabled(); err != nil {
		return port.WorkstreamResultVerification{}, domain.ResultIdentity{}, err
	}
	if err := s.validateBinding(binding); err != nil {
		return port.WorkstreamResultVerification{}, domain.ResultIdentity{}, err
	}
	if s.verifier == nil || strings.TrimSpace(teamID) == "" || strings.TrimSpace(resultID) == "" || strings.TrimSpace(workstreamID) == "" {
		return port.WorkstreamResultVerification{}, domain.ResultIdentity{}, domain.ErrResultUnavailable
	}
	verification := port.WorkstreamResultVerification{ResultID: resultID, WorkstreamID: workstreamID, Actor: binding.Actor,
		TeamID: teamID, Conversation: string(binding.ConversationKey), Project: binding.Project}
	identity, err := s.verifier.VerifyForWorkstream(ctx, verification)
	return verification, identity, err
}

// ValidateProposal checks the exact transition and resulting bounded state
// without writing it. Confirmation-required root actions use it before asking
// the owner to approve a proposal that could not be committed.
func (s *Service) ValidateProposal(ctx context.Context, binding Binding, source domain.WorkstreamTransitionSource, transition domain.WorkstreamTransition) error {
	if err := s.requireEnabled(); err != nil {
		return err
	}
	if err := s.validateBinding(binding); err != nil {
		return err
	}
	if source != domain.WorkstreamSourceHuman && source != domain.WorkstreamSourceRoot && source != domain.WorkstreamSourceSystem {
		return errors.New("invalid workstream transition source")
	}
	transition.Source = source
	transition.Actor = binding.Actor
	transition.ConversationKey = binding.ConversationKey
	transition.Project = binding.Project
	if transition.TransitionPayloadDigest == "" {
		transition.TransitionPayloadDigest = transition.PayloadDigestValue()
	}
	current, err := s.store.Get(ctx, transition.WorkstreamID)
	if err != nil {
		return fmt.Errorf("get workstream for proposal: %w", err)
	}
	if err := current.ValidateBinding(binding.Actor, binding.ConversationKey, binding.Project); err != nil {
		return err
	}
	if transition.RequiresConfirmation() {
		transition.Confirmation = &domain.WorkstreamConfirmation{
			ID: "proposal-validation", WorkstreamID: transition.WorkstreamID,
			ExpectedRevision: transition.ExpectedRevision, Actor: binding.Actor,
			ConversationKey: binding.ConversationKey, Project: binding.Project,
			Action: transition.Action, PayloadDigest: transition.PayloadDigestValue(),
			Approved: true, ExpiresAt: s.clock.Now().UTC().Add(time.Second),
		}
	}
	_, err = (&current).ApplyTransitionWithLimits(transition, s.cfg.Limits, s.clock.Now().UTC())
	return err
}

func (s *Service) apply(ctx context.Context, binding Binding, source domain.WorkstreamTransitionSource, transition domain.WorkstreamTransition) (domain.WorkstreamTransitionRecord, domain.WorkstreamSnapshot, error) {
	if err := s.requireEnabled(); err != nil {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, err
	}
	if err := s.validateBinding(binding); err != nil {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, err
	}
	if source == domain.WorkstreamSourceWorker {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, domain.ErrWorkstreamWorkerMutation
	}
	if source != domain.WorkstreamSourceHuman && source != domain.WorkstreamSourceRoot && source != domain.WorkstreamSourceSystem {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, errors.New("invalid workstream transition source")
	}
	if strings.TrimSpace(transition.WorkstreamID) == "" {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, errors.New("workstream ID is required")
	}
	if transition.Action == domain.WorkstreamActionLinkCompletedResult {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, domain.ErrResultInvalid
	}
	transition.Source = source
	transition.Actor = binding.Actor
	transition.ConversationKey = binding.ConversationKey
	transition.Project = binding.Project
	if transition.Action == domain.WorkstreamActionStartTask {
		// The execution identity is always host-owned and derived from the
		// trusted binding plus the caller's provenance identity. A caller-
		// supplied value is never persisted, and a replay of the same
		// source identity resolves the same execution identity, so the
		// journaled payload digest stays stable.
		transition.ExecutionIdentity = executionIdentityFor(transition.WorkstreamID, transition.TaskID, transition.SourceID)
	}
	if transition.TransitionPayloadDigest == "" {
		transition.TransitionPayloadDigest = transition.PayloadDigestValue()
	}
	if transition.Confirmation != nil {
		transition.Confirmation.WorkstreamID = transition.WorkstreamID
		transition.Confirmation.ExpectedRevision = transition.ExpectedRevision
		transition.Confirmation.Actor = binding.Actor
		transition.Confirmation.ConversationKey = binding.ConversationKey
		transition.Confirmation.Project = binding.Project
	}
	record, err := s.store.Apply(ctx, transition, s.cfg.Limits, s.clock.Now().UTC())
	if err != nil {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, fmt.Errorf("apply workstream transition: %w", err)
	}
	if record.StateJSON != "" {
		var applied domain.Workstream
		if err := json.Unmarshal([]byte(record.StateJSON), &applied); err != nil {
			return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, fmt.Errorf("decode applied workstream state: %w", err)
		}
		if !domain.VerifyWorkstreamStateJSON(record.StateJSON, record.StateDigest) {
			return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, errors.New("applied workstream state digest mismatch")
		}
		if err := applied.ValidateBinding(binding.Actor, binding.ConversationKey, binding.Project); err != nil {
			return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, err
		}
		if err := applied.ValidateWithLimits(s.cfg.Limits); err != nil {
			return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, err
		}
		return record, applied.Snapshot(), nil
	}
	snapshot, err := s.Get(ctx, binding, transition.WorkstreamID)
	if err != nil {
		return domain.WorkstreamTransitionRecord{}, domain.WorkstreamSnapshot{}, fmt.Errorf("read applied workstream: %w", err)
	}
	return record, snapshot, nil
}

func (s *Service) requireEnabled() error {
	if s == nil || !s.cfg.Enabled {
		return ErrDisabled
	}
	return nil
}

// executionIdentityFor derives the host-owned execution identity from the
// trusted workstream, task, and provenance identity. It never accepts a
// caller-supplied value and is stable across replays of the same source
// identity, keeping the journaled transition payload digest deterministic.
func executionIdentityFor(workstreamID, taskID, sourceID string) string {
	digest := sha256.Sum256([]byte(workstreamID + "\x00" + taskID + "\x00" + sourceID))
	return "exec_" + hex.EncodeToString(digest[:])
}

func (s *Service) validateBinding(binding Binding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	if _, ok := s.cfg.AllowedProjects[binding.Project]; !ok {
		return fmt.Errorf("%w: project is not registered", domain.ErrWorkstreamProjectMismatch)
	}
	return nil
}
