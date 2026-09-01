package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestWorkstreamStoreFreshSchemaAndSQLConstraints(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "workstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	var version int
	if err := store.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, SchemaVersion)
	}
	for _, table := range []string{
		"workstreams", "workstream_constraints", "workstream_decisions",
		"workstream_tasks", "workstream_task_inputs", "workstream_task_dependencies",
		"workstream_questions", "workstream_result_links", "workstream_transitions",
	} {
		var name string
		if err := store.DB().QueryRowContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil || name != table {
			t.Fatalf("schema table %q: %v", table, err)
		}
	}

	if _, err := store.DB().ExecContext(ctx, `INSERT INTO workstreams (
		workstream_id, conversation_key, owner_actor, project, status, revision, objective, created_at, updated_at)
		VALUES ('invalid', 'slack:T12345678:dm:D12345678', 'U12345678', 'workspace', 'unknown', 0, 'objective', 1, 1)`); err == nil {
		t.Fatal("invalid workstream status was accepted")
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO workstreams (
		workstream_id, conversation_key, owner_actor, project, status, revision, objective, created_at, updated_at)
		VALUES ('negative', 'slack:T12345678:dm:D12345678', 'U12345678', 'workspace', 'proposed', -1, 'objective', 1, 1)`); err == nil {
		t.Fatal("negative workstream revision was accepted")
	}

	workstreams := NewWorkstreamStore(store)
	if err := workstreams.Create(ctx, testSQLiteWorkstream("ws-1", "slack:T12345678:dm:D12345678"), domain.WorkstreamSourceHuman, "create-1"); err != nil {
		t.Fatalf("create first workstream: %v", err)
	}
	if err := workstreams.Create(ctx, testSQLiteWorkstream("ws-2", "slack:T12345678:dm:D12345678"), domain.WorkstreamSourceHuman, "create-2"); !errors.Is(err, ErrWorkstreamConversationConflict) {
		t.Fatalf("second active conversation error = %v, want %v", err, ErrWorkstreamConversationConflict)
	}
}

func TestWorkstreamStoreStartTaskPersistsRunningBindingAndJournal(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "workstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	workstreams := NewWorkstreamStore(store)
	workstream := testSQLiteWorkstream("ws-1", "slack:T12345678:dm:D12345678")
	if err := workstreams.Create(ctx, workstream, domain.WorkstreamSourceHuman, "create-1"); err != nil {
		t.Fatal(err)
	}
	activate := testSQLiteTransition(workstream, domain.WorkstreamSourceHuman, domain.WorkstreamActionActivateWorkstream, 0)
	activate.SourceID = "activate-1"
	if _, err := workstreams.Apply(ctx, activate, domain.DefaultWorkstreamLimits(), time.Unix(10, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	propose := testSQLiteTransition(workstream, domain.WorkstreamSourceHuman, domain.WorkstreamActionProposeTask, 1)
	propose.SourceID = "propose-1"
	propose.Task = &domain.WorkstreamTask{ID: "task-1", Project: workstream.Project, Description: "admitted task", Status: domain.TaskProposed}
	if _, err := workstreams.Apply(ctx, propose, domain.DefaultWorkstreamLimits(), time.Unix(11, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	jobs := NewExternalAgentJobStore(store)
	job := testExternalAgentJob(time.Unix(12, 0).UTC())
	job.ID, job.OriginalCallID, job.WrapperCallID = "job-1", "original-job-1", "wrapper-job-1"
	if _, _, err := jobs.CreateIfAbsent(ctx, job); err != nil {
		t.Fatalf("create start-task job: %v", err)
	}
	start := testSQLiteTransition(workstream, domain.WorkstreamSourceSystem, domain.WorkstreamActionStartTask, 2)
	start.SourceID = "start-1"
	start.TaskID = "task-1"
	start.JobID = job.ID
	start.ExecutionIdentity = job.ID
	if _, err := workstreams.Apply(ctx, start, domain.DefaultWorkstreamLimits(), time.Unix(12, 0).UTC()); err != nil {
		t.Fatalf("start task: %v", err)
	}
	got, err := workstreams.Get(ctx, workstream.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tasks) != 1 || got.Tasks[0].Status != domain.TaskRunning || got.Tasks[0].JobID != job.ID || got.Tasks[0].ExecutionIdentity != job.ID {
		t.Fatalf("started workstream = %+v", got)
	}
	var journalAction string
	if err := store.DB().QueryRowContext(ctx, `SELECT action FROM workstream_transitions WHERE workstream_id = ? AND to_revision = 3`, workstream.ID).Scan(&journalAction); err != nil {
		t.Fatal(err)
	}
	if journalAction != string(domain.WorkstreamActionStartTask) {
		t.Fatalf("start task journal action = %q", journalAction)
	}
}

func TestWorkstreamStoreCASJournalAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workstream.db")
	store, err := Initialize(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	workstreams := NewWorkstreamStore(store)
	workstream := testSQLiteWorkstream("ws-1", "slack:T12345678:dm:D12345678")
	if err := workstreams.Create(ctx, workstream, domain.WorkstreamSourceHuman, "create-1"); err != nil {
		t.Fatalf("create workstream: %v", err)
	}

	human := testSQLiteTransition(workstream, domain.WorkstreamSourceHuman, domain.WorkstreamActionRecordConstraint, 0)
	human.SourceID = "human-correction"
	human.Constraint = &domain.WorkstreamConstraint{ID: "constraint-1", Text: "human correction"}
	record, err := workstreams.Apply(ctx, human, domain.DefaultWorkstreamLimits(), time.Unix(10, 0).UTC())
	if err != nil {
		t.Fatalf("apply human correction: %v", err)
	}
	if record.FromRevision != 0 || record.ToRevision != 1 || record.Source != domain.WorkstreamSourceHuman {
		t.Fatalf("transition record = %+v", record)
	}

	staleRoot := testSQLiteTransition(workstream, domain.WorkstreamSourceRoot, domain.WorkstreamActionProposeTask, 0)
	staleRoot.Task = &domain.WorkstreamTask{ID: "root-task", Project: workstream.Project, Description: "stale", Status: domain.TaskProposed}
	if _, err := workstreams.Apply(ctx, staleRoot, domain.DefaultWorkstreamLimits(), time.Unix(11, 0).UTC()); !errors.Is(err, ErrWorkstreamCASConflict) {
		t.Fatalf("stale root error = %v, want %v", err, ErrWorkstreamCASConflict)
	}

	got, err := workstreams.Get(ctx, workstream.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 1 || len(got.Constraints) != 1 || got.Constraints[0].Text != "human correction" {
		t.Fatalf("current workstream = %+v", got)
	}
	var journalCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM workstream_transitions WHERE workstream_id = ?`, workstream.ID).Scan(&journalCount); err != nil {
		t.Fatal(err)
	}
	if journalCount != 2 {
		t.Fatalf("journal count = %d, want 2 including creation", journalCount)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE workstream_transitions SET actor = 'UOTHER123' WHERE workstream_id = ? AND to_revision = 1`, workstream.ID); err == nil {
		t.Fatal("transition journal update was accepted")
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM workstream_transitions WHERE workstream_id = ? AND to_revision = 1`, workstream.ID); err == nil {
		t.Fatal("transition journal delete was accepted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenExisting(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	got, err = NewWorkstreamStore(store).Get(ctx, workstream.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 1 || len(got.Constraints) != 1 || got.Constraints[0].SourceID != "" {
		t.Fatalf("reopened workstream = %+v", got)
	}
}

func TestWorkstreamStoreRejectsInvalidTaskConstraint(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "workstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	workstreams := NewWorkstreamStore(store)
	workstream := testSQLiteWorkstream("ws-1", "slack:T12345678:dm:D12345678")
	if err := workstreams.Create(ctx, workstream, domain.WorkstreamSourceHuman, "create-1"); err != nil {
		t.Fatal(err)
	}

	proposal := testSQLiteTransition(workstream, domain.WorkstreamSourceHuman, domain.WorkstreamActionProposeTask, 0)
	proposal.Task = &domain.WorkstreamTask{
		ID: "bad-task", Project: "other-project", Description: "cross-project", Status: domain.TaskProposed,
	}
	if _, err := workstreams.Apply(ctx, proposal, domain.DefaultWorkstreamLimits(), time.Unix(10, 0).UTC()); !errors.Is(err, domain.ErrWorkstreamProjectMismatch) {
		t.Fatalf("cross-project task error = %v, want %v", err, domain.ErrWorkstreamProjectMismatch)
	}

	var taskCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM workstream_tasks WHERE workstream_id = ?`, workstream.ID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 0 {
		t.Fatalf("invalid task was persisted: %d rows", taskCount)
	}
}

func TestWorkstreamResultLinkCommitIsAtomicAndBindsVerifiedIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "workstream-result-link.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	materialization := testResultMaterialization("tool-workstream-link", 1, "verified link payload")
	materialization.Producer.Kind = domain.ResultProducerToolOperation
	handle, err := results.Materialize(ctx, materialization)
	if err != nil {
		t.Fatal(err)
	}
	identity, _, err := results.Resolve(ctx, handle.ResultID, materialization.Scope)
	if err != nil {
		t.Fatal(err)
	}
	workstreams := NewWorkstreamStore(store)
	workstream := testSQLiteWorkstream("ws-result-link", domain.ConversationKey(materialization.Scope.ConversationKey))
	workstream.OwnerActor = materialization.Scope.Actor
	workstream.Project = materialization.Scope.Project
	if err := workstreams.Create(ctx, workstream, domain.WorkstreamSourceHuman, "create-result-link"); err != nil {
		t.Fatal(err)
	}
	verification := port.WorkstreamResultVerification{
		ResultID: handle.ResultID, WorkstreamID: workstream.ID,
		Actor: materialization.Scope.Actor, TeamID: materialization.Scope.TeamID, Conversation: materialization.Scope.ConversationKey, Project: materialization.Scope.Project,
	}
	transition := testSQLiteTransition(workstream, domain.WorkstreamSourceHuman, domain.WorkstreamActionLinkCompletedResult, 0)
	transition.SourceID = "link-result-1"
	transition.ResultLink = &domain.WorkstreamResultLink{ID: "result-link-1", ResultIdentity: handle.ResultID, Description: "verified output"}
	if _, err := workstreams.Apply(ctx, transition, domain.DefaultWorkstreamLimits(), time.Unix(9, 0).UTC()); !errors.Is(err, domain.ErrResultInvalid) {
		t.Fatalf("generic result-link error = %v, want %v", err, domain.ErrResultInvalid)
	}

	forged := identity
	forged.SHA256 = strings.Repeat("a", 64)
	if _, err := workstreams.CommitVerifiedResultLink(
		ctx,
		port.WorkstreamResultLinkCommit{Verification: verification, VerifiedIdentity: forged, Transition: transition, Limits: domain.DefaultWorkstreamLimits(), Now: time.Unix(10, 0).UTC()},
	); !errors.Is(
		err,
		domain.ErrResultUnavailable,
	) {
		t.Fatalf("forged commit error = %v", err)
	}
	var revision, links, bindings, references int
	if err := store.DB().QueryRowContext(ctx, `SELECT revision FROM workstreams WHERE workstream_id = ?`, workstream.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM workstream_result_links WHERE workstream_id = ?`, workstream.ID).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM workstream_result_link_results WHERE workstream_id = ?`, workstream.ID).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM result_references WHERE result_id = ? AND owner_kind = 'workstream_result_link'`, handle.ResultID).Scan(&references); err != nil {
		t.Fatal(err)
	}
	if revision != 0 || links != 0 || bindings != 0 || references != 0 {
		t.Fatalf("forged commit persisted revision/links/bindings/references = %d/%d/%d/%d", revision, links, bindings, references)
	}

	record, err := workstreams.CommitVerifiedResultLink(
		ctx,
		port.WorkstreamResultLinkCommit{Verification: verification, VerifiedIdentity: identity, Transition: transition, Limits: domain.DefaultWorkstreamLimits(), Now: time.Unix(11, 0).UTC()},
	)
	if err != nil {
		t.Fatalf("verified commit: %v", err)
	}
	if record.ToRevision != 1 {
		t.Fatalf("verified record = %+v", record)
	}
	if err := store.DB().QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM workstream_result_link_results WHERE workstream_id = ? AND result_id = ?`,
		workstream.ID,
		handle.ResultID,
	).Scan(
		&bindings,
	); err != nil ||
		bindings != 1 {
		t.Fatalf("verified result binding = %d, %v", bindings, err)
	}
	if err := store.DB().QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM result_references WHERE result_id = ? AND owner_kind = 'workstream_result_link' AND state = 'live'`,
		handle.ResultID,
	).Scan(
		&references,
	); err != nil ||
		references != 1 {
		t.Fatalf("verified result reference = %d, %v", references, err)
	}
}

func TestWorkstreamResultLinkCommitRejectsCrossScopeRequest(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "workstream-cross-scope-link.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	materialization := testResultMaterialization("tool-cross-scope-link", 1, "scope A payload")
	materialization.Producer.Kind = domain.ResultProducerToolOperation
	handle, err := results.Materialize(ctx, materialization)
	if err != nil {
		t.Fatal(err)
	}
	identity, _, err := results.Resolve(ctx, handle.ResultID, materialization.Scope)
	if err != nil {
		t.Fatal(err)
	}

	workstreams := NewWorkstreamStore(store)
	workstream := testSQLiteWorkstream("ws-scope-b", "slack:T2:dm:D2")
	workstream.OwnerActor = "U2"
	workstream.Project = "project-b"
	if err := workstreams.Create(ctx, workstream, domain.WorkstreamSourceHuman, "create-scope-b"); err != nil {
		t.Fatal(err)
	}
	transition := testSQLiteTransition(workstream, domain.WorkstreamSourceHuman, domain.WorkstreamActionLinkCompletedResult, 0)
	transition.SourceID = "cross-scope-link"
	transition.ResultLink = &domain.WorkstreamResultLink{ID: "link-scope-b", ResultIdentity: handle.ResultID}
	verification := port.WorkstreamResultVerification{
		ResultID: handle.ResultID, WorkstreamID: workstream.ID, Actor: materialization.Scope.Actor,
		TeamID: materialization.Scope.TeamID, Conversation: materialization.Scope.ConversationKey, Project: materialization.Scope.Project,
	}
	_, err = workstreams.CommitVerifiedResultLink(ctx, port.WorkstreamResultLinkCommit{
		Verification: verification, VerifiedIdentity: identity, Transition: transition,
		Limits: domain.DefaultWorkstreamLimits(), Now: time.Unix(10, 0).UTC(),
	})
	if !errors.Is(err, domain.ErrResultInvalid) {
		t.Fatalf("cross-scope commit error = %v, want %v", err, domain.ErrResultInvalid)
	}
	var revision, links int
	if err := store.DB().QueryRowContext(ctx, `SELECT revision FROM workstreams WHERE workstream_id = ?`, workstream.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM workstream_result_links WHERE workstream_id = ?`, workstream.ID).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if revision != 0 || links != 0 {
		t.Fatalf("cross-scope commit persisted revision/links = %d/%d", revision, links)
	}
}

func TestWorkstreamStoreJournalIsReconstructibleAndTransitionsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "workstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	workstreams := NewWorkstreamStore(store)
	workstream := testSQLiteWorkstream("ws-1", "slack:T12345678:dm:D12345678")
	if err := workstreams.Create(ctx, workstream, domain.WorkstreamSourceHuman, "create-1"); err != nil {
		t.Fatal(err)
	}
	transition := testSQLiteTransition(workstream, domain.WorkstreamSourceHuman, domain.WorkstreamActionRecordConstraint, 0)
	transition.SourceID = "human-replay-1"
	transition.Constraint = &domain.WorkstreamConstraint{ID: "constraint-1", Text: "bounded"}
	first, err := workstreams.Apply(ctx, transition, domain.DefaultWorkstreamLimits(), time.Unix(10, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := workstreams.Apply(ctx, transition, domain.DefaultWorkstreamLimits(), time.Unix(99, 0).UTC())
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.ToRevision != first.ToRevision || replayed.StateDigest != first.StateDigest || replayed.StateJSON != first.StateJSON {
		t.Fatalf("replayed record = %+v, first = %+v", replayed, first)
	}
	conflicting := transition
	conflicting.Constraint = &domain.WorkstreamConstraint{ID: "constraint-2", Text: "different"}
	if _, err := workstreams.Apply(ctx, conflicting, domain.DefaultWorkstreamLimits(), time.Unix(100, 0).UTC()); !errors.Is(err, domain.ErrWorkstreamSourceConflict) {
		t.Fatalf("source conflict = %v, want %v", err, domain.ErrWorkstreamSourceConflict)
	}
	records, err := workstreams.Transitions(ctx, workstream.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Action != domain.WorkstreamActionCreateWorkstream || records[0].PayloadJSON == "" || records[1].PayloadDigest == "" || records[1].PayloadJSON == "" ||
		records[1].StateDigest == "" ||
		records[1].StateJSON == "" {
		t.Fatalf("journal records = %+v", records)
	}
	if err := workstreams.Create(ctx, workstream, domain.WorkstreamSourceHuman, "create-1"); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	workstream.Objective = "different objective"
	if err := workstreams.Create(ctx, workstream, domain.WorkstreamSourceHuman, "create-1"); !errors.Is(err, port.ErrWorkstreamValidation) {
		t.Fatalf("conflicting create = %v", err)
	}
}

func TestWorkstreamStoreCreationReplayIgnoresRetryTime(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "workstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	workstreams := NewWorkstreamStore(store)
	workstream := testSQLiteWorkstream("ws-1", "slack:T12345678:dm:D12345678")
	workstream.CreatedAt = time.Time{}
	workstream.UpdatedAt = time.Time{}
	if err := workstreams.Create(ctx, workstream, domain.WorkstreamSourceHuman, "create-1"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := workstreams.Create(ctx, workstream, domain.WorkstreamSourceHuman, "create-1"); err != nil {
		t.Fatalf("replay with a new clock time: %v", err)
	}
}

func TestWorkstreamStorePersistsDecisionQuestionAndTaskTerminalActions(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "workstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	workstreams := NewWorkstreamStore(store)
	workstream := testSQLiteWorkstream("ws-1", "slack:T12345678:dm:D12345678")
	if err := workstreams.Create(ctx, workstream, domain.WorkstreamSourceHuman, "create-1"); err != nil {
		t.Fatal(err)
	}
	apply := func(action domain.WorkstreamAction, sourceID string, mutate func(*domain.WorkstreamTransition)) {
		t.Helper()
		transition := testSQLiteTransition(workstream, domain.WorkstreamSourceHuman, action, workstream.Revision)
		transition.SourceID = sourceID
		mutate(&transition)
		if _, err := workstreams.Apply(ctx, transition, domain.DefaultWorkstreamLimits(), time.Unix(int64(workstream.Revision+10), 0).UTC()); err != nil {
			t.Fatalf("apply %s: %v", action, err)
		}
		workstream, err = workstreams.Get(ctx, workstream.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	apply(domain.WorkstreamActionProposeDecision, "decision-propose-1", func(transition *domain.WorkstreamTransition) {
		transition.Decision = &domain.WorkstreamDecision{ID: "decision-1", Proposal: "ship"}
	})
	apply(domain.WorkstreamActionApproveDecision, "decision-approve-1", func(transition *domain.WorkstreamTransition) { transition.DecisionID = "decision-1" })
	apply(domain.WorkstreamActionProposeDecision, "decision-propose-2", func(transition *domain.WorkstreamTransition) {
		transition.Decision = &domain.WorkstreamDecision{ID: "decision-2", Proposal: "expand scope"}
	})
	apply(domain.WorkstreamActionRejectDecision, "decision-reject-2", func(transition *domain.WorkstreamTransition) { transition.DecisionID = "decision-2" })
	apply(domain.WorkstreamActionRequestHumanDecision, "question-open-1", func(transition *domain.WorkstreamTransition) {
		transition.Question = &domain.WorkstreamQuestion{ID: "question-1", Text: "ship now?"}
	})
	apply(domain.WorkstreamActionResolveQuestion, "question-resolve-1", func(transition *domain.WorkstreamTransition) {
		transition.QuestionID, transition.QuestionResolution = "question-1", "yes"
	})
	apply(domain.WorkstreamActionProposeTask, "task-propose-1", func(transition *domain.WorkstreamTransition) {
		transition.Task = &domain.WorkstreamTask{ID: "task-1", Project: workstream.Project, Description: "optional task", Status: domain.TaskProposed}
	})
	apply(domain.WorkstreamActionRejectTask, "task-reject-1", func(transition *domain.WorkstreamTransition) { transition.TaskID = "task-1" })

	if workstream.Decisions[0].Status != domain.DecisionApproved || workstream.Decisions[1].Status != domain.DecisionRejected {
		t.Fatalf("decisions = %+v", workstream.Decisions)
	}
	if workstream.OpenQuestions[0].Status != domain.QuestionResolved || workstream.OpenQuestions[0].Resolution != "yes" {
		t.Fatalf("questions = %+v", workstream.OpenQuestions)
	}
	if workstream.Tasks[0].Status != domain.TaskRejected {
		t.Fatalf("tasks = %+v", workstream.Tasks)
	}
}

func TestWorkstreamStoreConcurrentCASHasOneWinner(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "workstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	workstreams := NewWorkstreamStore(store)
	workstream := testSQLiteWorkstream("ws-1", "slack:T12345678:dm:D12345678")
	if err := workstreams.Create(ctx, workstream, domain.WorkstreamSourceHuman, "create-1"); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var wg sync.WaitGroup
	for index := range 2 {
		wg.Go(func() {
			<-start
			transition := testSQLiteTransition(workstream, domain.WorkstreamSourceHuman, domain.WorkstreamActionRecordConstraint, 0)
			transition.SourceID = fmt.Sprintf("concurrent-%d", index)
			transition.Constraint = &domain.WorkstreamConstraint{ID: fmt.Sprintf("constraint-%d", index), Text: "concurrent correction"}
			_, err := workstreams.Apply(ctx, transition, domain.DefaultWorkstreamLimits(), time.Unix(10, 0).UTC())
			errorsCh <- err
		})
	}
	close(start)
	wg.Wait()
	close(errorsCh)
	winners, conflicts := 0, 0
	for err := range errorsCh {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, port.ErrWorkstreamCASConflict):
			conflicts++
		default:
			t.Fatalf("concurrent apply error = %v", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("concurrent results: winners=%d conflicts=%d", winners, conflicts)
	}
}

func testSQLiteWorkstream(id string, conversation domain.ConversationKey) domain.Workstream {
	return domain.Workstream{
		ID: id, ConversationKey: conversation, OwnerActor: "U12345678", Project: "workspace",
		Status: domain.WorkstreamProposed, Objective: "deliver bounded change",
		Revision: 0, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}
}

func testSQLiteTransition(workstream domain.Workstream, source domain.WorkstreamTransitionSource, action domain.WorkstreamAction, revision int) domain.WorkstreamTransition {
	return domain.WorkstreamTransition{
		WorkstreamID: workstream.ID, ExpectedRevision: revision, Source: source, SourceID: "source-1",
		Actor: workstream.OwnerActor, ConversationKey: workstream.ConversationKey, Project: workstream.Project,
		Action: action,
	}
}
