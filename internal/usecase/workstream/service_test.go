package workstream_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	workstream "github.com/Dauno/slack-local-agent/internal/usecase/workstream"
)

func TestServiceBindsTrustedInvocationAndPreservesJournalRecord(t *testing.T) {
	store := &fakeStore{workstream: testWorkstream()}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store, Clock: fixedClock{at: time.Unix(10, 0).UTC()}})
	if err != nil {
		t.Fatal(err)
	}

	proposal := domain.WorkstreamTransition{
		WorkstreamID: "ws-1", ExpectedRevision: 0, Source: domain.WorkstreamSourceWorker,
		SourceID: "root-proposal-1", Actor: "UATTACKER", ConversationKey: "slack:T99999999:dm:D99999999", Project: "other-project",
		Action: domain.WorkstreamActionProposeTask,
		Task:   &domain.WorkstreamTask{ID: "task-1", Project: "workspace", Description: "bounded task", Status: domain.TaskProposed},
	}
	record, snapshot, err := service.Apply(ctx(), workstream.Binding{
		Actor: "U12345678", ConversationKey: "slack:T12345678:dm:D12345678", Project: "workspace",
	}, domain.WorkstreamSourceRoot, proposal)
	if err != nil {
		t.Fatalf("trusted root proposal rejected: %v", err)
	}
	if record.Source != domain.WorkstreamSourceRoot || record.SourceID != "root-proposal-1" {
		t.Fatalf("record = %+v", record)
	}
	if snapshot.Revision != 1 || len(snapshot.Tasks) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if store.lastTransition.Actor != "U12345678" || store.lastTransition.Project != "workspace" || store.lastTransition.ConversationKey != "slack:T12345678:dm:D12345678" {
		t.Fatalf("store received untrusted binding: %+v", store.lastTransition)
	}
	if store.lastTransition.Source != domain.WorkstreamSourceRoot {
		t.Fatalf("store received untrusted source: %q", store.lastTransition.Source)
	}
}

func TestServiceRejectsForeignActorForReadAndMutation(t *testing.T) {
	store := &fakeStore{workstream: testWorkstream()}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}

	foreign := workstream.Binding{Actor: "UOTHER123", ConversationKey: testWorkstream().ConversationKey, Project: "workspace"}
	if _, err := service.Get(ctx(), foreign, "ws-1"); !errors.Is(err, domain.ErrWorkstreamOwnerMismatch) {
		t.Fatalf("foreign read error = %v, want %v", err, domain.ErrWorkstreamOwnerMismatch)
	}
	proposal := domain.WorkstreamTransition{
		WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "human-1", Action: domain.WorkstreamActionRecordConstraint,
		Constraint: &domain.WorkstreamConstraint{ID: "constraint-1", Text: "foreign mutation"},
	}
	if _, _, err := service.Apply(ctx(), foreign, domain.WorkstreamSourceHuman, proposal); !errors.Is(err, domain.ErrWorkstreamOwnerMismatch) {
		t.Fatalf("foreign mutation error = %v, want %v", err, domain.ErrWorkstreamOwnerMismatch)
	}
}

func TestServiceSnapshotForActivationRechecksTrustedActiveBinding(t *testing.T) {
	stored := testWorkstream()
	stored.Status = domain.WorkstreamActive
	stored.Tasks = []domain.WorkstreamTask{{ID: "task-1", Project: "workspace", Description: "run task", Status: domain.TaskRunning, ExecutionIdentity: "exec-1"}}
	store := &fakeStore{workstream: stored}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := service.SnapshotForActivation(ctx(), "ws-1", "U12345678", stored.ConversationKey)
	if err != nil {
		t.Fatalf("snapshot for activation: %v", err)
	}
	if snapshot.ID != "ws-1" || snapshot.Status != domain.WorkstreamActive || len(snapshot.Tasks) != 1 {
		t.Fatalf("activation snapshot = %+v", snapshot)
	}
	if _, err := service.SnapshotForActivation(ctx(), "ws-1", "UOTHER123", stored.ConversationKey); !errors.Is(err, domain.ErrWorkstreamOwnerMismatch) {
		t.Fatalf("foreign activation actor error = %v", err)
	}
	store.workstream.Status = domain.WorkstreamPaused
	if _, err := service.SnapshotForActivation(ctx(), "ws-1", "U12345678", stored.ConversationKey); !errors.Is(err, domain.ErrWorkstreamNotActive) {
		t.Fatalf("paused activation workstream error = %v", err)
	}
}

func TestCompletionBindingForTaskRequiresExactRunningTask(t *testing.T) {
	stored := testWorkstream()
	stored.Status = domain.WorkstreamActive
	stored.Revision = 7
	stored.Tasks = []domain.WorkstreamTask{{ID: "task-1", Project: "workspace", Description: "run task", Status: domain.TaskRunning, ExecutionIdentity: "exec-1"}}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: &fakeStore{workstream: stored}})
	if err != nil {
		t.Fatal(err)
	}
	binding, found, err := service.CompletionBindingForTask(ctx(), "U12345678", stored.ConversationKey, "workspace", "run task")
	if err != nil || !found || binding.WorkstreamID != stored.ID || binding.TaskID != "task-1" || binding.ExecutionIdentity != "exec-1" || binding.AdmissionRevision != 7 {
		t.Fatalf("binding = %+v, found=%t, err=%v", binding, found, err)
	}
	if _, found, err := service.CompletionBindingForTask(ctx(), "U12345678", stored.ConversationKey, "workspace", "other task"); err != nil || found {
		t.Fatalf("nonmatching task found=%t, err=%v", found, err)
	}
}

func TestServiceCreationAndFeatureGate(t *testing.T) {
	store := &fakeStore{}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	binding := workstream.Binding{Actor: "U12345678", ConversationKey: "slack:T12345678:dm:D12345678", Project: "workspace"}
	snapshot, err := service.Create(ctx(), binding, "ws-1", "explicit objective")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if snapshot.Status != domain.WorkstreamProposed || snapshot.Revision != 0 || snapshot.OwnerActor != binding.Actor {
		t.Fatalf("created snapshot = %+v", snapshot)
	}

	disabled, err := workstream.New(workstream.Config{}, workstream.Dependencies{Store: &fakeStore{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disabled.Get(ctx(), binding, "ws-1"); !errors.Is(err, workstream.ErrDisabled) {
		t.Fatalf("disabled service error = %v, want %v", err, workstream.ErrDisabled)
	}
}

func TestServiceRejectsOverLimitCreationBeforePersistence(t *testing.T) {
	store := &fakeStore{}
	service, err := workstream.New(workstream.Config{
		Enabled: true, Limits: domain.WorkstreamLimits{MaxNonTerminalTasks: 1, MaxDependenciesPerTask: 1, MaxTextRunes: 4},
		AllowedProjects: map[string]struct{}{"workspace": {}},
	}, workstream.Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	binding := workstream.Binding{Actor: "U12345678", ConversationKey: "slack:T12345678:dm:D12345678", Project: "workspace"}
	if _, err := service.Create(ctx(), binding, "ws-1", "too long"); !errors.Is(err, domain.ErrWorkstreamLimitExceeded) {
		t.Fatalf("create error = %v, want configured limit error", err)
	}
	if store.workstream.ID != "" {
		t.Fatalf("over-limit workstream was persisted: %+v", store.workstream)
	}
}

func TestServiceEnabledRequiresRegisteredProjects(t *testing.T) {
	if _, err := workstream.New(workstream.Config{Enabled: true}, workstream.Dependencies{Store: &fakeStore{}}); err == nil {
		t.Fatal("enabled service accepted an empty project registry")
	}
}

func TestServiceStartTaskGeneratesHostExecutionIdentity(t *testing.T) {
	stored := testWorkstream()
	stored.Tasks = []domain.WorkstreamTask{{ID: "task-1", Project: "workspace", Description: "admit me", Status: domain.TaskProposed}}
	store := &fakeStore{workstream: stored}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	binding := workstream.Binding{Actor: "U12345678", ConversationKey: testWorkstream().ConversationKey, Project: "workspace"}
	// A caller-supplied identity must never be persisted.
	_, _, err = service.ApplyHuman(ctx(), binding, domain.WorkstreamTransition{
		WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "slack-human:event-start",
		Action: domain.WorkstreamActionStartTask, TaskID: "task-1", ExecutionIdentity: "forged-identity",
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.lastTransition.Action != domain.WorkstreamActionStartTask || strings.TrimSpace(store.lastTransition.ExecutionIdentity) == "" {
		t.Fatalf("start task transition = %+v", store.lastTransition)
	}
	if store.lastTransition.ExecutionIdentity == "forged-identity" || !strings.HasPrefix(store.lastTransition.ExecutionIdentity, "exec_") {
		t.Fatalf("generated execution identity = %q", store.lastTransition.ExecutionIdentity)
	}
	firstIdentity := store.lastTransition.ExecutionIdentity

	// The derivation is deterministic per trusted binding and provenance: the
	// same command on a fresh store resolves the same execution identity, so a
	// replay keeps the journaled payload digest stable.
	fresh := &fakeStore{workstream: testWorkstream()}
	fresh.workstream.Tasks = []domain.WorkstreamTask{{ID: "task-1", Project: "workspace", Description: "admit me", Status: domain.TaskProposed}}
	replay, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: fresh})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := replay.ApplyHuman(ctx(), binding, domain.WorkstreamTransition{
		WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "slack-human:event-start",
		Action: domain.WorkstreamActionStartTask, TaskID: "task-1",
	}); err != nil {
		t.Fatal(err)
	}
	if fresh.lastTransition.ExecutionIdentity != firstIdentity {
		t.Fatalf("replayed identity = %q, want %q", fresh.lastTransition.ExecutionIdentity, firstIdentity)
	}
}

func TestServiceHumanMutationRecordsHumanProvenance(t *testing.T) {
	store := &fakeStore{workstream: testWorkstream()}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = service.ApplyHuman(ctx(), workstream.Binding{Actor: "U12345678", ConversationKey: testWorkstream().ConversationKey, Project: "workspace"}, domain.WorkstreamTransition{
		WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "slack-human:event-1", Action: domain.WorkstreamActionRecordConstraint,
		Constraint: &domain.WorkstreamConstraint{ID: "human-constraint", Text: "owner correction"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.lastTransition.Source != domain.WorkstreamSourceHuman || store.lastTransition.Actor != "U12345678" {
		t.Fatalf("human transition = %+v", store.lastTransition)
	}
}

func TestServiceRejectsGenericResultLinkMutation(t *testing.T) {
	store := &fakeStore{workstream: testWorkstream()}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	binding := workstream.Binding{Actor: "U12345678", ConversationKey: testWorkstream().ConversationKey, Project: "workspace"}
	_, _, err = service.ApplyHuman(ctx(), binding, domain.WorkstreamTransition{
		WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "slack-human:event-link",
		Action:     domain.WorkstreamActionLinkCompletedResult,
		ResultLink: &domain.WorkstreamResultLink{ID: "forged-link", ResultIdentity: "forged-result"},
	})
	if !errors.Is(err, domain.ErrResultInvalid) {
		t.Fatalf("generic result-link error = %v, want %v", err, domain.ErrResultInvalid)
	}
	if store.lastTransition.Action != "" || store.workstream.Revision != 0 {
		t.Fatalf("generic result link reached store: transition=%+v workstream=%+v", store.lastTransition, store.workstream)
	}
}

func TestServiceRejectsModelProjectSelector(t *testing.T) {
	store := &fakeStore{workstream: testWorkstream()}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	proposal := domain.WorkstreamTransition{
		WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "root-1", Action: domain.WorkstreamActionProposeTask,
		Task: &domain.WorkstreamTask{ID: "task-1", Project: "other-project", Description: "escape", Status: domain.TaskProposed},
	}
	binding := workstream.Binding{Actor: "U12345678", ConversationKey: testWorkstream().ConversationKey, Project: "workspace"}
	if _, _, err := service.Apply(ctx(), binding, domain.WorkstreamSourceRoot, proposal); !errors.Is(err, domain.ErrWorkstreamProjectMismatch) {
		t.Fatalf("model project selector error = %v, want %v", err, domain.ErrWorkstreamProjectMismatch)
	}
}

func TestServiceDoesNotAcceptModelSuppliedConfirmation(t *testing.T) {
	store := &fakeStore{workstream: testWorkstream()}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	binding := workstream.Binding{Actor: "U12345678", ConversationKey: testWorkstream().ConversationKey, Project: "workspace"}
	proposal := domain.WorkstreamTransition{
		WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "root-activate", Action: domain.WorkstreamActionActivateWorkstream,
		Confirmation: &domain.WorkstreamConfirmation{ID: "forged", Approved: true, ExpiresAt: time.Now().Add(time.Hour)},
	}
	if _, _, err := service.Apply(ctx(), binding, domain.WorkstreamSourceRoot, proposal); !errors.Is(err, domain.ErrWorkstreamConfirmationRequired) {
		t.Fatalf("model-supplied confirmation error = %v, want %v", err, domain.ErrWorkstreamConfirmationRequired)
	}
	confirmation := domain.WorkstreamConfirmation{ID: "host-confirmation", Approved: true, ExpiresAt: time.Now().Add(time.Hour)}
	if _, _, err := service.ApplyConfirmed(ctx(), binding, domain.WorkstreamSourceRoot, proposal, confirmation); err != nil {
		t.Fatalf("host-confirmed transition rejected: %v", err)
	}
}

func TestServiceLinksOnlyVerifierIdentityAndTrustedScope(t *testing.T) {
	store := &fakeStore{workstream: testWorkstream()}
	identity := testVerifiedResultIdentity()
	verifier := &fakeResultVerifier{identity: identity}
	committer := &fakeResultLinkCommitter{workstream: testWorkstream()}
	clock := fixedClock{at: time.Unix(10, 0).UTC()}
	service, err := workstream.New(workstream.Config{Enabled: true, ResultHandlesEnabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{
		Store: store, Clock: clock, ResultVerifier: verifier, LinkCommitter: committer,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := workstream.Binding{Actor: "U12345678", ConversationKey: testWorkstream().ConversationKey, Project: "workspace"}
	transition := domain.WorkstreamTransition{WorkstreamID: "ws-1", ExpectedRevision: 0, Action: domain.WorkstreamActionLinkCompletedResult,
		ResultLink: &domain.WorkstreamResultLink{ID: "link-1", ResultIdentity: "model-forged-id"}}
	record, snapshot, err := service.LinkCompletedResult(ctx(), binding, "T12345678", identity.ResultID, transition, "confirmation-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.ToRevision != 1 || snapshot.Revision != 1 || len(snapshot.ResultLinks) != 1 {
		t.Fatalf("link result = %+v / %+v", record, snapshot)
	}
	if verifier.request.Actor != binding.Actor || verifier.request.TeamID != "T12345678" || verifier.request.Conversation != string(binding.ConversationKey) || verifier.request.Project != binding.Project {
		t.Fatalf("untrusted verifier scope = %+v", verifier.request)
	}
	if committer.request.Transition.ResultLink.ResultIdentity != identity.ResultID || committer.request.VerifiedIdentity != identity {
		t.Fatalf("committer request = %+v", committer.request)
	}
}

func TestServiceResultHandleGateBlocksNewLinksButPreservesReads(t *testing.T) {
	identity := testVerifiedResultIdentity()
	store := &fakeStore{workstream: testWorkstream()}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{
		Store: store, ResultVerifier: &fakeResultVerifier{identity: identity}, LinkCommitter: &fakeResultLinkCommitter{workstream: testWorkstream()}, ResultReader: &fakeResultReader{identity: identity},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := workstream.Binding{Actor: "U12345678", ConversationKey: testWorkstream().ConversationKey, Project: "workspace"}
	transition := domain.WorkstreamTransition{WorkstreamID: "ws-1", ExpectedRevision: 0, Action: domain.WorkstreamActionLinkCompletedResult,
		ResultLink: &domain.WorkstreamResultLink{ID: "link-1", ResultIdentity: identity.ResultID}}
	if _, _, err := service.LinkCompletedResult(ctx(), binding, "T12345678", identity.ResultID, transition, "confirmation-1"); !errors.Is(err, workstream.ErrResultHandlesDisabled) {
		t.Fatalf("disabled result link error = %v", err)
	}
	if store.lastTransition.Action != "" {
		t.Fatalf("disabled result link reached store: %+v", store.lastTransition)
	}
	if handle, err := service.ResultHandleForWorkstream(ctx(), binding, "T12345678", identity.ResultID, "ws-1"); err != nil || handle.ResultID != identity.ResultID {
		t.Fatalf("existing result remains readable: %+v, %v", handle, err)
	}
}

func TestServiceReadsBoundedResultOnlyThroughVerifiedWorkstreamScope(t *testing.T) {
	identity := testVerifiedResultIdentity()
	verifier := &fakeResultVerifier{identity: identity}
	reader := &fakeResultReader{identity: identity}
	service, err := workstream.New(workstream.Config{Enabled: true, ResultHandlesEnabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{
		Store: &fakeStore{workstream: testWorkstream()}, ResultVerifier: verifier, ResultReader: reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := workstream.Binding{Actor: "U12345678", ConversationKey: testWorkstream().ConversationKey, Project: "workspace"}
	handle, err := service.ResultHandleForWorkstream(ctx(), binding, "T12345678", identity.ResultID, "ws-1")
	if err != nil || handle.ResultID != identity.ResultID {
		t.Fatalf("verified handle = %+v, %v", handle, err)
	}
	chunk, err := service.ReadResultChunkForWorkstream(ctx(), binding, "T12345678", identity.ResultID, "ws-1", 0, 8)
	if err != nil || chunk.Content != "chunk" {
		t.Fatalf("verified chunk = %+v, %v", chunk, err)
	}
	if reader.scope != identity.Scope || reader.reads != 1 || verifier.request.Project != binding.Project {
		t.Fatalf("bound reader scope/calls = %+v/%d; verifier = %+v", reader.scope, reader.reads, verifier.request)
	}
}

type fakeStore struct {
	workstream     domain.Workstream
	lastTransition domain.WorkstreamTransition
}

func (f *fakeStore) Create(_ context.Context, workstream domain.Workstream, _ domain.WorkstreamTransitionSource, _ string) error {
	if f.workstream.ID != "" {
		return port.ErrWorkstreamConversationConflict
	}
	f.workstream = workstream
	return nil
}

func (f *fakeStore) Get(_ context.Context, id string) (domain.Workstream, error) {
	if f.workstream.ID == "" || f.workstream.ID != id {
		return domain.Workstream{}, port.ErrWorkstreamNotFound
	}
	return f.workstream, nil
}

func (f *fakeStore) ActiveForConversation(_ context.Context, key domain.ConversationKey) (domain.Workstream, error) {
	if f.workstream.ID == "" || f.workstream.ConversationKey != key || f.workstream.Status.Terminal() {
		return domain.Workstream{}, port.ErrWorkstreamNotFound
	}
	return f.workstream, nil
}

func (f *fakeStore) Apply(_ context.Context, transition domain.WorkstreamTransition, limits domain.WorkstreamLimits, now time.Time) (domain.WorkstreamTransitionRecord, error) {
	f.lastTransition = transition
	if transition.ExpectedRevision != f.workstream.Revision {
		return domain.WorkstreamTransitionRecord{}, port.ErrWorkstreamCASConflict
	}
	record, err := (&f.workstream).ApplyTransitionWithLimits(transition, limits, now)
	return record, err
}

func (f *fakeStore) Transitions(_ context.Context, _ string) ([]domain.WorkstreamTransitionRecord, error) {
	return nil, nil
}

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

func testWorkstream() domain.Workstream {
	return domain.Workstream{
		ID: "ws-1", ConversationKey: "slack:T12345678:dm:D12345678", OwnerActor: "U12345678",
		Project: "workspace", Status: domain.WorkstreamProposed, Revision: 0, Objective: "objective",
	}
}

func ctx() context.Context { return context.Background() }

type fakeResultVerifier struct {
	request  port.WorkstreamResultVerification
	identity domain.ResultIdentity
	err      error
}

func (f *fakeResultVerifier) VerifyForWorkstream(_ context.Context, request port.WorkstreamResultVerification) (domain.ResultIdentity, error) {
	f.request = request
	return f.identity, f.err
}

type fakeResultLinkCommitter struct {
	workstream domain.Workstream
	request    port.WorkstreamResultLinkCommit
}

type fakeResultReader struct {
	identity domain.ResultIdentity
	scope    domain.ResultScope
	reads    int
}

func (f *fakeResultReader) Materialize(context.Context, port.ResultMaterialization) (domain.ResultHandle, error) {
	return domain.ResultHandle{}, domain.ErrResultUnavailable
}

func (f *fakeResultReader) Resolve(_ context.Context, resultID string, scope domain.ResultScope) (domain.ResultIdentity, domain.ResultHandle, error) {
	f.scope = scope
	if resultID != f.identity.ResultID || scope != f.identity.Scope {
		return domain.ResultIdentity{}, domain.ResultHandle{}, domain.ErrResultScopeMismatch
	}
	handle, err := f.identity.Handle([]domain.ResultAvailability{domain.ResultAvailabilityRangeRead}, nil)
	return f.identity, handle, err
}

func (f *fakeResultReader) ReadRange(_ context.Context, resultID string, scope domain.ResultScope, offset, max int64) (domain.ResultChunk, error) {
	f.scope = scope
	f.reads++
	if resultID != f.identity.ResultID || scope != f.identity.Scope || offset != 0 || max != 8 {
		return domain.ResultChunk{}, domain.ErrResultScopeMismatch
	}
	return domain.ResultChunk{Content: "chunk", OffsetBytes: 0, NextOffsetBytes: 5, EOF: true, SHA256: f.identity.SHA256}, nil
}

func (f *fakeResultLinkCommitter) CommitVerifiedResultLink(_ context.Context, request port.WorkstreamResultLinkCommit) (domain.WorkstreamTransitionRecord, error) {
	f.request = request
	return (&f.workstream).ApplyTransitionWithLimits(request.Transition, request.Limits, request.Now)
}

func testVerifiedResultIdentity() domain.ResultIdentity {
	return domain.ResultIdentity{
		ResultID: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Producer: domain.ResultProducer{Kind: domain.ResultProducerToolOperation, ID: "tool-1", Revision: 1},
		Storage:  domain.ResultStorage{Kind: domain.ResultStorageRecoverable, Key: "payload-1"},
		SHA256:   "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		Bytes:    12, MediaType: "text/plain", Scope: domain.ResultScope{Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678", Project: "workspace"},
		Retention: domain.ResultRetentionWorkstream, CreatedAt: time.Unix(1, 0).UTC(), State: domain.ResultAvailable,
	}
}
