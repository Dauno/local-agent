package toolfactory

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestWorkstreamTransitionFromArgsBuildsTypedPayloads(t *testing.T) {
	args := workstreamTransitionArgs{
		WorkstreamID: "ws-1", Project: "workspace", ExpectedRevision: 4, CurrentPhase: "plan",
		TaskID: "task-1", TaskDescription: "bounded task", Dependencies: []string{"task-0"}, RequiredInputs: []string{"input"},
		ConstraintID: "constraint-1", ConstraintText: "constraint", QuestionID: "question-1", QuestionText: "question", QuestionResolution: "answer",
		DecisionID: "decision-1", DecisionProposal: "proposal", DecisionRationale: "rationale",
	}
	callID := "call-1"
	cases := []struct {
		action domain.WorkstreamAction
		check  func(t *testing.T, transition domain.WorkstreamTransition)
	}{
		{domain.WorkstreamActionProposeTask, func(t *testing.T, transition domain.WorkstreamTransition) {
			if transition.Task == nil ||
				transition.Task.ID != args.TaskID ||
				transition.Task.Project != args.Project ||
				transition.Task.Status != domain.TaskProposed ||
				!reflect.DeepEqual(transition.Task.Dependencies, args.Dependencies) ||
				!reflect.DeepEqual(transition.Task.RequiredInputs, args.RequiredInputs) {
				t.Fatalf("task payload = %+v", transition.Task)
			}
		}},
		{domain.WorkstreamActionRecordConstraint, func(t *testing.T, transition domain.WorkstreamTransition) {
			if transition.Constraint == nil || transition.Constraint.ID != args.ConstraintID || transition.Constraint.Text != args.ConstraintText || transition.Constraint.SourceID != callID {
				t.Fatalf("constraint payload = %+v", transition.Constraint)
			}
		}},
		{domain.WorkstreamActionProposeDecision, func(t *testing.T, transition domain.WorkstreamTransition) {
			if transition.Decision == nil ||
				transition.Decision.ID != args.DecisionID ||
				transition.Decision.Status != domain.DecisionProposed ||
				transition.Decision.Source != callID ||
				transition.Decision.Rationale != args.DecisionRationale {
				t.Fatalf("decision payload = %+v", transition.Decision)
			}
		}},
		{domain.WorkstreamActionRequestHumanDecision, func(t *testing.T, transition domain.WorkstreamTransition) {
			if transition.Question == nil ||
				transition.Question.ID != args.QuestionID ||
				transition.Question.Text != args.QuestionText ||
				transition.Question.Status != domain.QuestionOpen ||
				transition.Question.SourceID != callID {
				t.Fatalf("question payload = %+v", transition.Question)
			}
		}},
		{domain.WorkstreamActionApproveDecision, func(t *testing.T, transition domain.WorkstreamTransition) {
			if transition.DecisionID != args.DecisionID {
				t.Fatalf("decision ID = %q", transition.DecisionID)
			}
		}},
		{domain.WorkstreamActionRejectDecision, func(t *testing.T, transition domain.WorkstreamTransition) {
			if transition.DecisionID != args.DecisionID {
				t.Fatalf("decision ID = %q", transition.DecisionID)
			}
		}},
		{domain.WorkstreamActionResolveQuestion, func(t *testing.T, transition domain.WorkstreamTransition) {
			if transition.QuestionID != args.QuestionID || transition.QuestionResolution != args.QuestionResolution {
				t.Fatalf("question resolution = %q/%q", transition.QuestionID, transition.QuestionResolution)
			}
		}},
		{domain.WorkstreamActionPauseWorkstream, func(t *testing.T, transition domain.WorkstreamTransition) {
			if transition.Task != nil || transition.Constraint != nil || transition.Decision != nil || transition.Question != nil {
				t.Fatalf("unexpected payload for pause: %+v", transition)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(string(tc.action), func(t *testing.T) {
			transition := workstreamTransitionFromArgs(args, tc.action, callID)
			if transition.WorkstreamID != args.WorkstreamID || transition.ExpectedRevision != args.ExpectedRevision || transition.Project != args.Project || transition.SourceID != callID ||
				transition.Action != tc.action ||
				transition.CurrentPhase != args.CurrentPhase {
				t.Fatalf("common transition fields = %+v", transition)
			}
			tc.check(t, transition)
		})
	}
}

func TestWorkstreamCreateAndConfirmationHelpers(t *testing.T) {
	args := workstreamCreateArgs{WorkstreamID: "ws-1", Project: "workspace", Objective: "ship feature"}
	transition := workstreamCreateArgsTransition(args, domain.WorkstreamActionCreateWorkstream)
	confirmationTransition := workstreamConfirmationTransition(args, domain.WorkstreamActionCreateWorkstream)
	if !reflect.DeepEqual(transition, confirmationTransition) || transition.ExpectedRevision != 0 || transition.Objective != args.Objective {
		t.Fatalf("create transitions differ: %+v / %+v", transition, confirmationTransition)
	}

	payload := workstreamConfirmationPayload(transition)
	if payload["objective"] != args.Objective || payload["workstream_id"] != args.WorkstreamID || payload["action"] != string(transition.Action) ||
		payload["payload_digest"] != transition.PayloadDigestValue() {
		t.Fatalf("create confirmation payload = %#v", payload)
	}
	withoutObjective := workstreamConfirmationPayload(domain.WorkstreamTransition{WorkstreamID: "ws-1", Project: "workspace", Action: domain.WorkstreamActionPauseWorkstream})
	if _, ok := withoutObjective["objective"]; ok {
		t.Fatal("non-create confirmation included an objective")
	}
	text := workstreamConfirmationText(transition)
	if !strings.Contains(text, args.WorkstreamID) || !strings.Contains(text, transition.PayloadDigestValue()) {
		t.Fatalf("confirmation text = %q", text)
	}
	if hint := workstreamConfirmationHint(args.WorkstreamID, 2, args.Project, domain.WorkstreamActionActivateWorkstream); !strings.Contains(hint, "revision 2") {
		t.Fatalf("confirmation hint = %q", hint)
	}
}

func TestWorkstreamToolActionPolicyAndRendering(t *testing.T) {
	validActions := []domain.WorkstreamAction{
		domain.WorkstreamActionActivateWorkstream, domain.WorkstreamActionPauseWorkstream, domain.WorkstreamActionResumeWorkstream,
		domain.WorkstreamActionCancelWorkstream, domain.WorkstreamActionCompleteWorkstream, domain.WorkstreamActionProposeTask,
		domain.WorkstreamActionRejectTask, domain.WorkstreamActionRevisePlan, domain.WorkstreamActionRecordConstraint,
		domain.WorkstreamActionProposeDecision, domain.WorkstreamActionRequestHumanDecision, domain.WorkstreamActionApproveDecision,
		domain.WorkstreamActionRejectDecision, domain.WorkstreamActionResolveQuestion, domain.WorkstreamActionLinkCompletedResult,
	}
	for _, action := range validActions {
		if !validWorkstreamToolAction(action) {
			t.Errorf("valid action rejected: %q", action)
		}
	}
	if validWorkstreamToolAction("unknown") || workstreamActionRequiresConfirmation("unknown") {
		t.Fatal("unknown action accepted by tool policy")
	}
	for _, action := range []domain.WorkstreamAction{domain.WorkstreamActionProposeTask, domain.WorkstreamActionRevisePlan, domain.WorkstreamActionRequestHumanDecision} {
		if workstreamActionRequiresConfirmation(action) {
			t.Errorf("planning action requires confirmation: %q", action)
		}
	}
	if !workstreamActionRequiresConfirmation(domain.WorkstreamActionActivateWorkstream) {
		t.Fatal("activation action did not require confirmation")
	}

	snapshot := domain.WorkstreamSnapshot{
		ID:           "ws-1",
		Project:      "workspace",
		Status:       domain.WorkstreamActive,
		Revision:     5,
		Objective:    "objective",
		CurrentPhase: "execute",
		Constraints:  []domain.WorkstreamConstraint{{ID: "c-1", Text: "constraint"}},
		Decisions:    []domain.WorkstreamDecision{{ID: "d-1", Status: domain.DecisionApproved, Proposal: "decision"}},
		Tasks: []domain.WorkstreamTask{
			{
				ID:             "t-1",
				JobID:          "job-1",
				Project:        "workspace",
				Description:    "task",
				Status:         domain.TaskRunning,
				Dependencies:   []string{"t-0"},
				RequiredInputs: []string{"input"},
				ResultIdentity: "result",
				Integrated:     true,
			},
		},
		OpenQuestions: []domain.WorkstreamQuestion{{ID: "q-1", Text: "question", Status: domain.QuestionOpen, Resolution: ""}},
		ResultLinks:   []domain.WorkstreamResultLink{{ID: "l-1", TaskID: "t-1", ResultIdentity: "result", Description: "link"}},
	}
	rendered := renderWorkstreamState(snapshot)
	if rendered.WorkstreamID != snapshot.ID || rendered.Status != string(snapshot.Status) || rendered.Revision != snapshot.Revision || len(rendered.Constraints) != 1 || len(rendered.Decisions) != 1 ||
		len(rendered.Tasks) != 1 ||
		len(rendered.OpenQuestions) != 1 ||
		len(rendered.ResultLinks) != 1 {
		t.Fatalf("rendered state = %+v", rendered)
	}
	if rendered.Tasks[0].JobID != "job-1" || rendered.Tasks[0].ResultIdentity != "result" || !rendered.Tasks[0].Integrated {
		t.Fatalf("rendered task = %+v", rendered.Tasks[0])
	}
}

func TestWorkstreamConversationAndLineHelpers(t *testing.T) {
	if got := teamIDFromConversation("slack:T12345678:dm:D12345678"); got != "T12345678" {
		t.Fatalf("team ID = %q", got)
	}
	if got := teamIDFromConversation("T12345678:dm:D12345678"); got != "T12345678" {
		t.Fatalf("team ID without prefix = %q", got)
	}
	for _, key := range []domain.ConversationKey{"", "slack:", "slack:T12345678"} {
		if got := teamIDFromConversation(key); got != "" {
			t.Errorf("team ID for %q = %q", key, got)
		}
	}
	if got := splitLines("first\n\nlast"); !reflect.DeepEqual(got, []string{"first", "", "last"}) {
		t.Fatalf("split lines = %#v", got)
	}
	for input, want := range map[string][]string{"": nil, "(no worktrees)": nil, "one\n\ntwo\n": {"one", "two"}} {
		if got := splitNonEmpty(input); !reflect.DeepEqual(got, want) {
			t.Errorf("split non-empty %q = %#v, want %#v", input, got, want)
		}
	}
}
