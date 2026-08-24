package domain_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestWorkstreamLifecycleRejectsTerminalReopen(t *testing.T) {
	workstream := testWorkstream()
	for _, next := range []domain.WorkstreamStatus{
		domain.WorkstreamActive,
		domain.WorkstreamPaused,
		domain.WorkstreamActive,
		domain.WorkstreamBlocked,
		domain.WorkstreamActive,
		domain.WorkstreamCompleted,
	} {
		if err := workstream.Transition(next); err != nil {
			t.Fatalf("transition to %s: %v", next, err)
		}
	}
	if err := workstream.Transition(domain.WorkstreamActive); !errors.Is(err, domain.ErrWorkstreamTerminal) {
		t.Fatalf("completed workstream reopen error = %v, want %v", err, domain.ErrWorkstreamTerminal)
	}
}

func TestWorkstreamLimitsAndDependencyGraphAreValidated(t *testing.T) {
	workstream := testWorkstream()
	workstream.Tasks = make([]domain.WorkstreamTask, 0, 3)
	for i := 1; i <= 3; i++ {
		workstream.Tasks = append(workstream.Tasks, domain.WorkstreamTask{
			ID:          fmt.Sprintf("task-%d", i),
			Project:     workstream.Project,
			Description: fmt.Sprintf("task %d", i),
			Status:      domain.TaskProposed,
		})
	}
	workstream.Tasks[1].Dependencies = []string{"task-1"}
	workstream.Tasks[2].Dependencies = []string{"task-2"}
	workstream.Tasks[0].Dependencies = []string{"task-3"}
	if err := workstream.ValidateWithLimits(domain.WorkstreamLimits{
		MaxNonTerminalTasks:    3,
		MaxDependenciesPerTask: 1,
	}); !errors.Is(err, domain.ErrWorkstreamDependencyCycle) {
		t.Fatalf("dependency cycle error = %v, want %v", err, domain.ErrWorkstreamDependencyCycle)
	}

	workstream.Tasks[2].Dependencies = nil
	workstream.Tasks[0].Dependencies = []string{"task-2", "task-3"}
	if err := workstream.ValidateWithLimits(domain.WorkstreamLimits{
		MaxNonTerminalTasks:    3,
		MaxDependenciesPerTask: 1,
	}); !errors.Is(err, domain.ErrWorkstreamLimitExceeded) {
		t.Fatalf("dependency limit error = %v, want %v", err, domain.ErrWorkstreamLimitExceeded)
	}

	for i := range workstream.Tasks {
		workstream.Tasks[i].Status = domain.TaskCompleted
	}
	for i := 0; i < 4; i++ {
		workstream.Tasks = append(workstream.Tasks, domain.WorkstreamTask{
			ID:          fmt.Sprintf("open-%d", i),
			Project:     workstream.Project,
			Description: "open task",
			Status:      domain.TaskProposed,
		})
	}
	if err := workstream.ValidateWithLimits(domain.WorkstreamLimits{
		MaxNonTerminalTasks:    3,
		MaxDependenciesPerTask: 1,
	}); !errors.Is(err, domain.ErrWorkstreamLimitExceeded) {
		t.Fatalf("non-terminal task limit error = %v, want %v", err, domain.ErrWorkstreamLimitExceeded)
	}

	if err := (domain.WorkstreamLimits{
		MaxNonTerminalTasks:    domain.HardMaxWorkstreamTasks + 1,
		MaxDependenciesPerTask: 1,
	}).Validate(); !errors.Is(err, domain.ErrWorkstreamLimitExceeded) {
		t.Fatalf("hard task limit error = %v, want %v", err, domain.ErrWorkstreamLimitExceeded)
	}
}

func TestWorkstreamRejectsTaskProjectIsolationViolation(t *testing.T) {
	workstream := testWorkstream()
	workstream.Tasks = []domain.WorkstreamTask{{
		ID:          "task-1",
		Project:     "other-project",
		Description: "read another project",
		Status:      domain.TaskProposed,
	}}
	if err := workstream.Validate(); !errors.Is(err, domain.ErrWorkstreamProjectMismatch) {
		t.Fatalf("project isolation error = %v, want %v", err, domain.ErrWorkstreamProjectMismatch)
	}
}

func TestWorkstreamTransitionRequiresBoundConfirmationAtAuthorityBoundaries(t *testing.T) {
	proposal := testProposal(domain.WorkstreamSourceRoot, domain.WorkstreamActionCompleteWorkstream, 0)
	if err := proposal.ValidateAgainst(testWorkstream(), time.Now().UTC()); !errors.Is(err, domain.ErrWorkstreamConfirmationRequired) {
		t.Fatalf("unconfirmed root completion error = %v, want %v", err, domain.ErrWorkstreamConfirmationRequired)
	}

	proposal.Confirmation = &domain.WorkstreamConfirmation{
		ID:               "confirmation-1",
		WorkstreamID:     proposal.WorkstreamID,
		ExpectedRevision: proposal.ExpectedRevision,
		Actor:            proposal.Actor,
		ConversationKey:  proposal.ConversationKey,
		Project:          proposal.Project,
		Action:           proposal.Action,
		PayloadDigest:    proposal.PayloadDigestValue(),
		Approved:         true,
		ExpiresAt:        time.Now().UTC().Add(time.Hour),
	}
	if err := proposal.ValidateAgainst(testWorkstream(), time.Now().UTC()); err != nil {
		t.Fatalf("confirmed root completion rejected: %v", err)
	}

	planning := testProposal(domain.WorkstreamSourceRoot, domain.WorkstreamActionProposeTask, 0)
	planning.Task = &domain.WorkstreamTask{
		ID:          "task-1",
		Project:     planning.Project,
		Description: "bounded planning task",
		Status:      domain.TaskProposed,
	}
	if err := planning.ValidateAgainst(testWorkstream(), time.Now().UTC()); err != nil {
		t.Fatalf("reversible root planning proposal rejected: %v", err)
	}
}

func TestWorkstreamTransitionRejectsOwnerConversationAndProjectMismatch(t *testing.T) {
	checks := []struct {
		name string
		edit func(*domain.WorkstreamTransition)
		want error
	}{
		{name: "owner", edit: func(p *domain.WorkstreamTransition) { p.Actor = "UOTHER123" }, want: domain.ErrWorkstreamOwnerMismatch},
		{name: "conversation", edit: func(p *domain.WorkstreamTransition) { p.ConversationKey = "slack:T12345678:dm:D87654321" }, want: domain.ErrWorkstreamConversationMismatch},
		{name: "project", edit: func(p *domain.WorkstreamTransition) { p.Project = "other-project" }, want: domain.ErrWorkstreamProjectMismatch},
	}
	for _, tt := range checks {
		t.Run(tt.name, func(t *testing.T) {
			proposal := testProposal(domain.WorkstreamSourceHuman, domain.WorkstreamActionRecordConstraint, 0)
			proposal.Constraint = &domain.WorkstreamConstraint{ID: "constraint-1", Text: "keep scope bounded"}
			tt.edit(&proposal)
			if err := proposal.ValidateAgainst(testWorkstream(), time.Now().UTC()); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestStaleRootProposalLosesRevisionCASAfterHumanCorrection(t *testing.T) {
	workstream := testWorkstream()
	root := testProposal(domain.WorkstreamSourceRoot, domain.WorkstreamActionProposeTask, workstream.Revision)
	root.Task = &domain.WorkstreamTask{
		ID:          "root-task",
		Project:     workstream.Project,
		Description: "root proposal",
		Status:      domain.TaskProposed,
	}

	human := testProposal(domain.WorkstreamSourceHuman, domain.WorkstreamActionRecordConstraint, workstream.Revision)
	human.SourceID = "human-command-1"
	human.Constraint = &domain.WorkstreamConstraint{ID: "constraint-1", Text: "human correction"}
	record, err := workstream.ApplyTransition(human, time.Now().UTC())
	if err != nil {
		t.Fatalf("human correction rejected: %v", err)
	}
	if record.Source != domain.WorkstreamSourceHuman || record.SourceID != human.SourceID || record.FromRevision != 0 || record.ToRevision != 1 {
		t.Fatalf("human transition record = %+v", record)
	}
	staleRecord, err := workstream.ApplyTransition(root, time.Now().UTC())
	if !errors.Is(err, domain.ErrWorkstreamRevisionConflict) {
		t.Fatalf("stale root proposal error = %v, want %v", err, domain.ErrWorkstreamRevisionConflict)
	}
	if staleRecord.WorkstreamID != "" {
		t.Fatalf("stale proposal produced a journal record: %+v", staleRecord)
	}
	if workstream.Revision != 1 || len(workstream.Constraints) != 1 || workstream.Constraints[0].Text != "human correction" {
		t.Fatalf("stale proposal changed current state: revision=%d constraints=%+v", workstream.Revision, workstream.Constraints)
	}
}

func TestWorkstreamTransitionPreservesExistingTasks(t *testing.T) {
	workstream := testWorkstream()
	workstream.Tasks = []domain.WorkstreamTask{{
		ID: "task-1", Project: workstream.Project, Description: "existing task", Status: domain.TaskProposed,
	}}
	proposal := testProposal(domain.WorkstreamSourceHuman, domain.WorkstreamActionRecordConstraint, workstream.Revision)
	proposal.Constraint = &domain.WorkstreamConstraint{ID: "constraint-1", Text: "preserve task"}

	if _, err := workstream.ApplyTransition(proposal, time.Now().UTC()); err != nil {
		t.Fatalf("transition rejected: %v", err)
	}
	if len(workstream.Tasks) != 1 || workstream.Tasks[0].ID != "task-1" || workstream.Tasks[0].Description != "existing task" {
		t.Fatalf("existing task was not preserved: %+v", workstream.Tasks)
	}
}

func TestWorkstreamTaskCanBeRejectedWithoutACPExecution(t *testing.T) {
	workstream := testWorkstream()
	workstream.Tasks = []domain.WorkstreamTask{{
		ID: "task-1", Project: workstream.Project, Description: "declined task", Status: domain.TaskProposed,
	}}
	transition := testProposal(domain.WorkstreamSourceHuman, domain.WorkstreamActionRejectTask, workstream.Revision)
	transition.TaskID = "task-1"
	if _, err := workstream.ApplyTransition(transition, time.Now().UTC()); err != nil {
		t.Fatalf("reject task: %v", err)
	}
	if workstream.Tasks[0].Status != domain.TaskRejected || !workstream.Tasks[0].Status.Terminal() {
		t.Fatalf("rejected task = %+v", workstream.Tasks[0])
	}
}

func TestWorkstreamTransitionUsesConfiguredTaskLimit(t *testing.T) {
	workstream := testWorkstream()
	for i := 0; i < 33; i++ {
		workstream.Tasks = append(workstream.Tasks, domain.WorkstreamTask{
			ID: fmt.Sprintf("task-%d", i), Project: workstream.Project, Description: "configured-limit task", Status: domain.TaskProposed,
		})
	}
	proposal := testProposal(domain.WorkstreamSourceHuman, domain.WorkstreamActionRecordConstraint, workstream.Revision)
	proposal.Constraint = &domain.WorkstreamConstraint{ID: "constraint-1", Text: "configured limit"}
	if _, err := workstream.ApplyTransitionWithLimits(proposal, domain.WorkstreamLimits{MaxNonTerminalTasks: 33, MaxDependenciesPerTask: 8}, time.Now().UTC()); err != nil {
		t.Fatalf("transition rejected within configured limit: %v", err)
	}
}

func TestWorkstreamDecisionAndQuestionLifecyclesArePersistible(t *testing.T) {
	workstream := testWorkstream()
	apply := func(transition domain.WorkstreamTransition) {
		t.Helper()
		if _, err := workstream.ApplyTransition(transition, time.Unix(int64(workstream.Revision+1), 0).UTC()); err != nil {
			t.Fatalf("apply %s: %v", transition.Action, err)
		}
	}
	apply(domain.WorkstreamTransition{WorkstreamID: "ws-1", ExpectedRevision: 0, Source: domain.WorkstreamSourceHuman, SourceID: "propose-decision", Actor: workstream.OwnerActor, ConversationKey: workstream.ConversationKey, Project: workstream.Project, Action: domain.WorkstreamActionProposeDecision, Decision: &domain.WorkstreamDecision{ID: "decision-1", Proposal: "ship", Status: domain.DecisionProposed}})
	apply(domain.WorkstreamTransition{WorkstreamID: "ws-1", ExpectedRevision: 1, Source: domain.WorkstreamSourceHuman, SourceID: "approve-decision", Actor: workstream.OwnerActor, ConversationKey: workstream.ConversationKey, Project: workstream.Project, Action: domain.WorkstreamActionApproveDecision, DecisionID: "decision-1"})
	apply(domain.WorkstreamTransition{WorkstreamID: "ws-1", ExpectedRevision: 2, Source: domain.WorkstreamSourceHuman, SourceID: "ask-question", Actor: workstream.OwnerActor, ConversationKey: workstream.ConversationKey, Project: workstream.Project, Action: domain.WorkstreamActionRequestHumanDecision, Question: &domain.WorkstreamQuestion{ID: "question-1", Text: "ship now", Status: domain.QuestionOpen}})
	apply(domain.WorkstreamTransition{WorkstreamID: "ws-1", ExpectedRevision: 3, Source: domain.WorkstreamSourceHuman, SourceID: "resolve-question", Actor: workstream.OwnerActor, ConversationKey: workstream.ConversationKey, Project: workstream.Project, Action: domain.WorkstreamActionResolveQuestion, QuestionID: "question-1", QuestionResolution: "yes"})

	if workstream.Decisions[0].Status != domain.DecisionApproved || workstream.OpenQuestions[0].Status != domain.QuestionResolved || workstream.OpenQuestions[0].Resolution != "yes" {
		t.Fatalf("lifecycle state = %+v", workstream)
	}
}

func TestWorkstreamBoundsCoverAllSnapshotCollectionsAndText(t *testing.T) {
	workstream := testWorkstream()
	workstream.Constraints = []domain.WorkstreamConstraint{{ID: "c-1", Text: "one"}, {ID: "c-2", Text: "two"}}
	limits := domain.DefaultWorkstreamLimits()
	limits.MaxConstraints = 1
	if err := workstream.ValidateWithLimits(limits); !errors.Is(err, domain.ErrWorkstreamLimitExceeded) {
		t.Fatalf("constraint collection error = %v", err)
	}
	workstream.Constraints = nil
	workstream.Objective = "12345"
	limits.MaxTextRunes = 4
	if err := workstream.ValidateWithLimits(limits); !errors.Is(err, domain.ErrWorkstreamLimitExceeded) {
		t.Fatalf("objective text error = %v", err)
	}
	workstream.Objective = "bounded"
	workstream.Tasks = make([]domain.WorkstreamTask, 129)
	for index := range workstream.Tasks {
		workstream.Tasks[index] = domain.WorkstreamTask{ID: fmt.Sprintf("task-%d", index), Project: workstream.Project, Description: "bounded", Status: domain.TaskCompleted}
	}
	if err := workstream.Validate(); !errors.Is(err, domain.ErrWorkstreamLimitExceeded) {
		t.Fatalf("total task collection error = %v", err)
	}
}

func TestWorkstreamTaskCannotStartBeforeDependenciesComplete(t *testing.T) {
	workstream := testWorkstream()
	workstream.Tasks = []domain.WorkstreamTask{
		{ID: "input", Project: workstream.Project, Description: "input", Status: domain.TaskRunning, ExecutionIdentity: "exec-input"},
		{ID: "dependent", Project: workstream.Project, Description: "dependent", Status: domain.TaskQueued, Dependencies: []string{"input"}, RequiredInputs: []string{"result-1"}},
	}
	if err := workstream.ValidateTaskReady("dependent"); !errors.Is(err, domain.ErrWorkstreamTaskNotReady) {
		t.Fatalf("running dependency readiness error = %v, want %v", err, domain.ErrWorkstreamTaskNotReady)
	}
	workstream.Tasks[0].Status = domain.TaskCompleted
	if err := workstream.ValidateTaskReady("dependent"); !errors.Is(err, domain.ErrWorkstreamTaskNotReady) {
		t.Fatalf("missing required input error = %v, want %v", err, domain.ErrWorkstreamTaskNotReady)
	}
	workstream.ResultLinks = []domain.WorkstreamResultLink{{ID: "link-1", TaskID: "input", ResultIdentity: "result-1"}}
	if err := workstream.ValidateTaskReady("dependent"); err != nil {
		t.Fatalf("completed dependency rejected: %v", err)
	}
}

func TestWorkstreamStartTaskAdmission(t *testing.T) {
	workstream := testWorkstream()
	workstream.Tasks = []domain.WorkstreamTask{
		{ID: "task-1", Project: workstream.Project, Description: "admitted task", Status: domain.TaskProposed},
	}
	transition := testProposal(domain.WorkstreamSourceHuman, domain.WorkstreamActionStartTask, workstream.Revision)
	transition.TaskID = "task-1"
	transition.ExecutionIdentity = "exec-1"
	if _, err := workstream.ApplyTransition(transition, time.Now().UTC()); err != nil {
		t.Fatalf("start task: %v", err)
	}
	if workstream.Tasks[0].Status != domain.TaskRunning || workstream.Tasks[0].ExecutionIdentity != "exec-1" {
		t.Fatalf("started task = %+v", workstream.Tasks[0])
	}

	workstream = testWorkstream()
	workstream.Tasks = []domain.WorkstreamTask{
		{ID: "task-1", Project: workstream.Project, Description: "admitted task", Status: domain.TaskProposed},
	}
	rootTransition := testProposal(domain.WorkstreamSourceRoot, domain.WorkstreamActionStartTask, workstream.Revision)
	rootTransition.TaskID = "task-1"
	rootTransition.ExecutionIdentity = "exec-1"
	if _, err := workstream.ApplyTransition(rootTransition, time.Now().UTC()); err == nil {
		t.Fatal("root source was allowed to start a task without the trusted human path")
	}

	workstream = testWorkstream()
	workstream.Tasks = []domain.WorkstreamTask{
		{ID: "task-1", Project: workstream.Project, Description: "admitted task", Status: domain.TaskProposed},
	}
	missingIdentity := testProposal(domain.WorkstreamSourceHuman, domain.WorkstreamActionStartTask, workstream.Revision)
	missingIdentity.TaskID = "task-1"
	if _, err := workstream.ApplyTransition(missingIdentity, time.Now().UTC()); err == nil {
		t.Fatal("start task accepted an empty execution identity")
	}
}

func TestWorkstreamStartTaskRequiresReadyInputs(t *testing.T) {
	workstream := testWorkstream()
	workstream.Tasks = []domain.WorkstreamTask{
		{ID: "task-1", Project: workstream.Project, Description: "dependent task", Status: domain.TaskProposed, RequiredInputs: []string{"missing-result"}},
	}
	transition := testProposal(domain.WorkstreamSourceHuman, domain.WorkstreamActionStartTask, workstream.Revision)
	transition.TaskID = "task-1"
	transition.ExecutionIdentity = "exec-1"
	if _, err := workstream.ApplyTransition(transition, time.Now().UTC()); !errors.Is(err, domain.ErrWorkstreamTaskNotReady) {
		t.Fatalf("unavailable required input error = %v, want %v", err, domain.ErrWorkstreamTaskNotReady)
	}
}

func testWorkstream() domain.Workstream {
	return domain.Workstream{
		ID:              "ws-1",
		ConversationKey: "slack:T12345678:dm:D12345678",
		OwnerActor:      "U12345678",
		Project:         "workspace",
		Status:          domain.WorkstreamProposed,
		Revision:        0,
		Objective:       "deliver bounded change",
	}
}

func testProposal(source domain.WorkstreamTransitionSource, action domain.WorkstreamAction, revision int) domain.WorkstreamTransition {
	return domain.WorkstreamTransition{
		WorkstreamID:     "ws-1",
		ExpectedRevision: revision,
		Source:           source,
		SourceID:         "source-1",
		Actor:            "U12345678",
		ConversationKey:  "slack:T12345678:dm:D12345678",
		Project:          "workspace",
		Action:           action,
	}
}

// TestWorkstreamTransitionPayloadDigestCanonicalStability pins hallazgo 4:
// workstreamTransitionPayload's new Objective field must never change the
// canonical payload JSON, and therefore the digest, of any action that
// existed before the field was added. Each case is a representative
// transition for that action; the expected digests below were captured from
// this fix and must never move for these fixed inputs.
func TestWorkstreamTransitionPayloadDigestCanonicalStability(t *testing.T) {
	task := &domain.WorkstreamTask{ID: "task-1", Project: "workspace", Description: "do work", Status: domain.TaskProposed}
	constraint := &domain.WorkstreamConstraint{ID: "constraint-1", Text: "stay bounded", SourceID: "source-1"}
	decision := &domain.WorkstreamDecision{ID: "decision-1", Status: domain.DecisionProposed, Proposal: "adopt plan", Source: "source-1", Rationale: "why"}
	question := &domain.WorkstreamQuestion{ID: "question-1", Text: "which path?", Status: domain.QuestionOpen, SourceID: "source-1"}
	resultLink := &domain.WorkstreamResultLink{ID: "link-1", ResultIdentity: "result-1"}

	cases := []struct {
		name       string
		transition domain.WorkstreamTransition
		wantDigest string
	}{
		{"create_workstream", domain.WorkstreamTransition{Action: domain.WorkstreamActionCreateWorkstream, Objective: "ship the audit repairs"}, "edf629477e824bc09458526e94b2e3f94f5ee08d504d1aec5a2a7d9e97e13949"},
		{"activate_workstream", domain.WorkstreamTransition{Action: domain.WorkstreamActionActivateWorkstream}, "3f2c0da30a39067d21b42ae24bce5338766d8bcc00e0fbba837e50c459bf072e"},
		{"pause_workstream", domain.WorkstreamTransition{Action: domain.WorkstreamActionPauseWorkstream}, "29a2c179c19bc0b2a806a0c33d7eedbbf86a36f2228d3c0fc76d434ae4f5d997"},
		{"resume_workstream", domain.WorkstreamTransition{Action: domain.WorkstreamActionResumeWorkstream}, "d16e8ea4d361033d244615457761e7a99682fc2d74caf28d8930847a5ae7afdf"},
		{"cancel_workstream", domain.WorkstreamTransition{Action: domain.WorkstreamActionCancelWorkstream}, "03a3a0a73f99ab595dc25119d3af5a12b6d5a3e150b648e4faa71b1dca2dcabb"},
		{"complete_workstream", domain.WorkstreamTransition{Action: domain.WorkstreamActionCompleteWorkstream}, "7bc3904e176b33577a86d8de22dbcb8a1613a76c3f45e368ac038c7b22d8b1d3"},
		{"block_workstream", domain.WorkstreamTransition{Action: domain.WorkstreamActionBlockWorkstream}, "107fd7796d2b6a13dc5917a2db59e24fd06b99f434df1d68dfd9ea835f802bc4"},
		{"unblock_workstream", domain.WorkstreamTransition{Action: domain.WorkstreamActionUnblockWorkstream}, "5c08df25e994aef77a201a46e32e0f47dbc9951a63b1f602670357b58d41388b"},
		{"propose_task", domain.WorkstreamTransition{Action: domain.WorkstreamActionProposeTask, Task: task, TaskID: task.ID}, "a3d3b959490be4bdd711685fa2c1e999ad4481479ec5af1b94ca04c203e2a83c"},
		{"reject_task", domain.WorkstreamTransition{Action: domain.WorkstreamActionRejectTask, TaskID: "task-1"}, "463cb167bde829db04efdeec582fb48212066bb2efe64f44e59de4028233a3e9"},
		{"start_task", domain.WorkstreamTransition{Action: domain.WorkstreamActionStartTask, TaskID: "task-1", ExecutionIdentity: "exec-1"}, "024b2f76feea3423b340dd381ee4c54d1551f9ff6cc09366f0f77f0ddc02f05c"},
		{"revise_plan", domain.WorkstreamTransition{Action: domain.WorkstreamActionRevisePlan, CurrentPhase: "phase 2"}, "e75a1e0ef0c3c162ac0864c270eba48e905bc34f0db3d9d59e11e9006af396ac"},
		{"record_constraint", domain.WorkstreamTransition{Action: domain.WorkstreamActionRecordConstraint, Constraint: constraint}, "2d9c955fa4e673963e926b6eeb38f13715b92f6a0bb9a9d8fa9f92c1a71d6176"},
		{"propose_decision", domain.WorkstreamTransition{Action: domain.WorkstreamActionProposeDecision, Decision: decision}, "74cab6cac243b7993a98adee07bf71b1464521526a87eb75001a8bdcd15f12d2"},
		{"request_human_decision", domain.WorkstreamTransition{Action: domain.WorkstreamActionRequestHumanDecision, Question: question}, "00cab649ffd25a0237ff1285eec8e0b5ec4eb077b6e7ecb33ee1dd1c3ef33c2f"},
		{"approve_decision", domain.WorkstreamTransition{Action: domain.WorkstreamActionApproveDecision, DecisionID: "decision-1"}, "a8b4a5995974bfdb467620594bc751c9c1f875ad24806d091af9c583015e7dc7"},
		{"reject_decision", domain.WorkstreamTransition{Action: domain.WorkstreamActionRejectDecision, DecisionID: "decision-1"}, "d520b0414b5ceb2b5f47237d75da085abebb605dad07f661dad6e07ff0d4dae7"},
		{"resolve_question", domain.WorkstreamTransition{Action: domain.WorkstreamActionResolveQuestion, QuestionID: "question-1", QuestionResolution: "answered"}, "ec3e1d2679ca951bbc4a1ac3d3dda14c300c280a165fb3919705f4313dd20940"},
		{"link_completed_result", domain.WorkstreamTransition{Action: domain.WorkstreamActionLinkCompletedResult, ResultLink: resultLink}, "56a7b00d14e7271e211fc904cc70e9f6cf2b0da1226fce1d040084329890a3a1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.transition.PayloadDigestValue()
			if got != tc.wantDigest {
				t.Fatalf("digest = %q, want %q (payload JSON = %s)", got, tc.wantDigest, tc.transition.PayloadJSONValue())
			}
		})
	}
}

// TestWorkstreamTransitionPayloadObjectiveOmittedWhenEmpty pins the
// json:",omitempty" contract on the new Objective field: every action that
// existed before this field was added never sets it, so its canonical
// payload JSON must never contain the key, keeping the digest of every
// already-written workstream_transitions row unchanged.
func TestWorkstreamTransitionPayloadObjectiveOmittedWhenEmpty(t *testing.T) {
	transition := domain.WorkstreamTransition{Action: domain.WorkstreamActionRevisePlan, CurrentPhase: "phase 2"}
	if json := transition.PayloadJSONValue(); strings.Contains(json, "Objective") {
		t.Fatalf("payload JSON carries the Objective key with an empty value: %s", json)
	}
}
