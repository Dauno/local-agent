package workstream_test

import (
	"context"
	"errors"
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
