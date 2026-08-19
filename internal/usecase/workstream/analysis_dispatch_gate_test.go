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

// fakeAnalysisGate is an in-memory port.AnalysesByWorkstream. It records
// every call so a test can prove the gate was actually consulted, and prove
// a blocked transition never reached the underlying workstream store.
type fakeAnalysisGate struct {
	records []port.AnalysisRecord
	err     error
	calls   int
}

func (f *fakeAnalysisGate) ListByWorkstream(_ context.Context, workstreamID, actor string, conversationKey domain.ConversationKey, project string) ([]port.AnalysisRecord, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.records, nil
}

func staleTestBinding() workstream.Binding {
	return workstream.Binding{Actor: "U12345678", ConversationKey: testWorkstream().ConversationKey, Project: "workspace"}
}

// TestServiceBlocksConfirmationTransitionWhenAnalysisIsIncomplete is
// criterion 7 (ApplyConfirmed path) plus part of criterion 2's underlying
// mechanism: a preparing analysis bound to the workstream blocks a root
// confirmation transition before it ever reaches the durable store.
func TestServiceBlocksConfirmationTransitionWhenAnalysisIsIncomplete(t *testing.T) {
	store := &fakeStore{workstream: testWorkstream()}
	gate := &fakeAnalysisGate{records: []port.AnalysisRecord{{AnalysisID: "an-1", State: domain.AnalysisPreparing}}}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store, AnalysisGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	binding := staleTestBinding()
	proposal := domain.WorkstreamTransition{WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "root-activate", Action: domain.WorkstreamActionActivateWorkstream}
	confirmation := domain.WorkstreamConfirmation{ID: "host-confirmation", Approved: true, ExpiresAt: time.Now().Add(time.Hour)}
	if _, _, err := service.ApplyConfirmed(ctx(), binding, domain.WorkstreamSourceRoot, proposal, confirmation); !errors.Is(err, domain.ErrWorkstreamAnalysisBlocking) {
		t.Fatalf("confirmation with an incomplete bound analysis = %v, want %v", err, domain.ErrWorkstreamAnalysisBlocking)
	}
	if gate.calls == 0 {
		t.Fatal("expected the analysis gate to be consulted")
	}
	if store.lastTransition.Action != "" {
		t.Fatalf("blocked transition reached the store: %+v", store.lastTransition)
	}
}

// TestServiceBlocksStartTaskWhenAnalysisFailed is criterion 7 (ApplyHuman /
// start_task execution path): a failed analysis bound to the workstream
// blocks task execution, not only root confirmation.
func TestServiceBlocksStartTaskWhenAnalysisFailed(t *testing.T) {
	stored := testWorkstream()
	stored.Tasks = []domain.WorkstreamTask{{ID: "task-1", Project: "workspace", Description: "admit me", Status: domain.TaskProposed}}
	store := &fakeStore{workstream: stored}
	gate := &fakeAnalysisGate{records: []port.AnalysisRecord{{AnalysisID: "an-1", State: domain.AnalysisFailed, FailureCode: domain.AnalysisFailureLeafSchemaInvalid}}}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store, AnalysisGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	binding := staleTestBinding()
	_, _, err = service.ApplyHuman(ctx(), binding, domain.WorkstreamTransition{
		WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "slack-human:event-start",
		Action: domain.WorkstreamActionStartTask, TaskID: "task-1",
	})
	if !errors.Is(err, domain.ErrWorkstreamAnalysisBlocking) {
		t.Fatalf("start_task with a failed bound analysis = %v, want %v", err, domain.ErrWorkstreamAnalysisBlocking)
	}
	if store.lastTransition.Action != "" {
		t.Fatalf("blocked start_task reached the store: %+v", store.lastTransition)
	}
}

// TestServiceBlocksConfirmationWhenCompletedAnalysisSourceIsStale is
// criterion 7's staleness case: a completed analysis whose recorded source
// digest no longer matches what the trusted reader resolves today still
// blocks, because the gate always re-verifies rather than trusting the
// stored digest.
func TestServiceBlocksConfirmationWhenCompletedAnalysisSourceIsStale(t *testing.T) {
	store := &fakeStore{workstream: testWorkstream()}
	scope := domain.ResultScope{Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678", Project: "workspace"}
	freshIdentity := testVerifiedResultIdentity()
	staleIdentity := domain.AnalysisIdentity{SourceResultID: freshIdentity.ResultID, SourceSHA256: "stale-digest-does-not-match-current-source"}
	gate := &fakeAnalysisGate{records: []port.AnalysisRecord{{AnalysisID: "an-1", State: domain.AnalysisCompleted, Identity: staleIdentity, Scope: scope}}}
	reader := &fakeResultReader{identity: freshIdentity}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store, AnalysisGate: gate, ResultReader: reader})
	if err != nil {
		t.Fatal(err)
	}
	binding := staleTestBinding()
	proposal := domain.WorkstreamTransition{WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "root-activate", Action: domain.WorkstreamActionActivateWorkstream}
	confirmation := domain.WorkstreamConfirmation{ID: "host-confirmation", Approved: true, ExpiresAt: time.Now().Add(time.Hour)}
	if _, _, err := service.ApplyConfirmed(ctx(), binding, domain.WorkstreamSourceRoot, proposal, confirmation); !errors.Is(err, domain.ErrWorkstreamAnalysisBlocking) {
		t.Fatalf("confirmation with a stale completed analysis = %v, want %v", err, domain.ErrWorkstreamAnalysisBlocking)
	}
}

// TestServiceAllowsConfirmationWhenAnalysisCompletedAndFresh is the positive
// counterpart: a completed analysis whose recorded source digest still
// matches never blocks.
func TestServiceAllowsConfirmationWhenAnalysisCompletedAndFresh(t *testing.T) {
	store := &fakeStore{workstream: testWorkstream()}
	scope := domain.ResultScope{Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678", Project: "workspace"}
	sourceIdentity := testVerifiedResultIdentity()
	matchingIdentity := domain.AnalysisIdentity{SourceResultID: sourceIdentity.ResultID, SourceSHA256: sourceIdentity.SHA256}
	gate := &fakeAnalysisGate{records: []port.AnalysisRecord{{AnalysisID: "an-1", State: domain.AnalysisCompleted, Identity: matchingIdentity, Scope: scope}}}
	reader := &fakeResultReader{identity: sourceIdentity}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store, AnalysisGate: gate, ResultReader: reader})
	if err != nil {
		t.Fatal(err)
	}
	binding := staleTestBinding()
	proposal := domain.WorkstreamTransition{WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "root-activate", Action: domain.WorkstreamActionActivateWorkstream}
	confirmation := domain.WorkstreamConfirmation{ID: "host-confirmation", Approved: true, ExpiresAt: time.Now().Add(time.Hour)}
	if _, _, err := service.ApplyConfirmed(ctx(), binding, domain.WorkstreamSourceRoot, proposal, confirmation); err != nil {
		t.Fatalf("confirmation with a fresh completed analysis rejected: %v", err)
	}
	if store.lastTransition.Action != domain.WorkstreamActionActivateWorkstream {
		t.Fatalf("expected the transition to reach the store, got %+v", store.lastTransition)
	}
}

// TestServiceHandoffAloneDoesNotSatisfyRequiredAnalysis is criterion 8: a
// workstream that already has a verified result link (a generic handoff)
// bound to it is still blocked by an incomplete required analysis. The
// handoff establishes context; it never substitutes for the analysis.
func TestServiceHandoffAloneDoesNotSatisfyRequiredAnalysis(t *testing.T) {
	identity := testVerifiedResultIdentity()
	store := &fakeStore{workstream: testWorkstream()}
	verifier := &fakeResultVerifier{identity: identity}
	committer := &fakeResultLinkCommitter{workstream: testWorkstream()}
	gate := &fakeAnalysisGate{records: []port.AnalysisRecord{{AnalysisID: "an-1", State: domain.AnalysisPreparing}}}
	service, err := workstream.New(workstream.Config{Enabled: true, ResultHandlesEnabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{
		Store: store, ResultVerifier: verifier, LinkCommitter: committer, AnalysisGate: gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := staleTestBinding()

	// Establish the handoff: a verified result link, exactly like an ACP
	// producer's handoff would bind, completing successfully because
	// LinkCompletedResult does not go through the four apply() entry points
	// this gate guards.
	linkTransition := domain.WorkstreamTransition{WorkstreamID: "ws-1", ExpectedRevision: 0, Action: domain.WorkstreamActionLinkCompletedResult,
		ResultLink: &domain.WorkstreamResultLink{ID: "link-1", ResultIdentity: "model-forged-id"}}
	if _, _, err := service.LinkCompletedResult(ctx(), binding, "T12345678", identity.ResultID, linkTransition, "confirmation-1"); err != nil {
		t.Fatalf("establishing the handoff: %v", err)
	}

	// The required analysis is still incomplete. A root confirmation must
	// still be blocked: the handoff just established did not satisfy it.
	proposal := domain.WorkstreamTransition{WorkstreamID: "ws-1", ExpectedRevision: store.workstream.Revision, SourceID: "root-activate", Action: domain.WorkstreamActionActivateWorkstream}
	confirmation := domain.WorkstreamConfirmation{ID: "host-confirmation", Approved: true, ExpiresAt: time.Now().Add(time.Hour)}
	if _, _, err := service.ApplyConfirmed(ctx(), binding, domain.WorkstreamSourceRoot, proposal, confirmation); !errors.Is(err, domain.ErrWorkstreamAnalysisBlocking) {
		t.Fatalf("confirmation after a handoff-only link = %v, want %v", err, domain.ErrWorkstreamAnalysisBlocking)
	}
}

// TestServiceProposeTaskIsNotBlockedByAnalysisGate is a regression control:
// the gate scopes itself to confirmation and execution transitions only, per
// the TRD's "confirmacion y ejecucion" wording. A plain root proposal, which
// domain.WorkstreamTransition.RequiresConfirmation already excludes, must
// still succeed even while a blocking analysis exists.
func TestServiceProposeTaskIsNotBlockedByAnalysisGate(t *testing.T) {
	store := &fakeStore{workstream: testWorkstream()}
	gate := &fakeAnalysisGate{records: []port.AnalysisRecord{{AnalysisID: "an-1", State: domain.AnalysisPreparing}}}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: store, AnalysisGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	binding := staleTestBinding()
	proposal := domain.WorkstreamTransition{
		WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "root-proposal-1", Action: domain.WorkstreamActionProposeTask,
		Task: &domain.WorkstreamTask{ID: "task-1", Project: "workspace", Description: "bounded task", Status: domain.TaskProposed},
	}
	if _, _, err := service.Apply(ctx(), binding, domain.WorkstreamSourceRoot, proposal); err != nil {
		t.Fatalf("propose_task blocked by the analysis gate: %v", err)
	}
}
