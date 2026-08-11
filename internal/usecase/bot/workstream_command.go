package bot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

const humanWorkstreamCommandPrefix = "workstream-human "

type humanWorkstreamCommand struct {
	Project            string                      `json:"project"`
	WorkstreamID       string                      `json:"workstream_id"`
	ExpectedRevision   int                         `json:"expected_revision"`
	Action             string                      `json:"action"`
	Objective          string                      `json:"objective,omitempty"`
	CurrentPhase       string                      `json:"current_phase,omitempty"`
	TaskID             string                      `json:"task_id,omitempty"`
	TaskDescription    string                      `json:"task_description,omitempty"`
	Dependencies       []string                    `json:"dependencies,omitempty"`
	RequiredInputs     []string                    `json:"required_inputs,omitempty"`
	ConstraintID       string                      `json:"constraint_id,omitempty"`
	ConstraintText     string                      `json:"constraint_text,omitempty"`
	QuestionID         string                      `json:"question_id,omitempty"`
	QuestionText       string                      `json:"question_text,omitempty"`
	QuestionResolution string                      `json:"question_resolution,omitempty"`
	DecisionID         string                      `json:"decision_id,omitempty"`
	DecisionProposal   string                      `json:"decision_proposal,omitempty"`
	DecisionRationale  string                      `json:"decision_rationale,omitempty"`
	Transition         domain.WorkstreamTransition `json:"-"`
}

func parseHumanWorkstreamCommand(text string) (humanWorkstreamCommand, bool, error) {
	if !strings.HasPrefix(text, humanWorkstreamCommandPrefix) {
		return humanWorkstreamCommand{}, false, nil
	}
	var command humanWorkstreamCommand
	decoder := json.NewDecoder(bytes.NewReader([]byte(strings.TrimSpace(strings.TrimPrefix(text, humanWorkstreamCommandPrefix)))))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&command); err != nil {
		return humanWorkstreamCommand{}, true, fmt.Errorf("JSON payload: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return humanWorkstreamCommand{}, true, errors.New("JSON payload must contain one object")
		}
		return humanWorkstreamCommand{}, true, fmt.Errorf("JSON payload has trailing data: %w", err)
	}
	if strings.TrimSpace(command.Project) == "" || strings.TrimSpace(command.WorkstreamID) == "" || strings.TrimSpace(command.Action) == "" {
		return humanWorkstreamCommand{}, true, errors.New("project, workstream_id, and action are required")
	}
	transition := domain.WorkstreamTransition{
		WorkstreamID: command.WorkstreamID, ExpectedRevision: command.ExpectedRevision,
		Project: command.Project, Action: domain.WorkstreamAction(command.Action),
		CurrentPhase: command.CurrentPhase, TaskID: command.TaskID,
	}
	switch transition.Action {
	case domain.WorkstreamActionProposeTask:
		transition.Task = &domain.WorkstreamTask{ID: command.TaskID, Project: command.Project, Description: command.TaskDescription, Status: domain.TaskProposed, Dependencies: command.Dependencies, RequiredInputs: command.RequiredInputs}
	case domain.WorkstreamActionRecordConstraint:
		transition.Constraint = &domain.WorkstreamConstraint{ID: command.ConstraintID, Text: command.ConstraintText}
	case domain.WorkstreamActionProposeDecision:
		transition.Decision = &domain.WorkstreamDecision{ID: command.DecisionID, Proposal: command.DecisionProposal, Rationale: command.DecisionRationale, Status: domain.DecisionProposed}
	case domain.WorkstreamActionRequestHumanDecision:
		transition.Question = &domain.WorkstreamQuestion{ID: command.QuestionID, Text: command.QuestionText, Status: domain.QuestionOpen}
	case domain.WorkstreamActionApproveDecision, domain.WorkstreamActionRejectDecision:
		transition.DecisionID = command.DecisionID
	case domain.WorkstreamActionResolveQuestion:
		transition.QuestionID = command.QuestionID
		transition.QuestionResolution = command.QuestionResolution
	case domain.WorkstreamActionCreateWorkstream:
		if strings.TrimSpace(command.Objective) == "" {
			return humanWorkstreamCommand{}, true, errors.New("objective is required to create a workstream")
		}
	}
	return humanWorkstreamCommand{Project: command.Project, WorkstreamID: command.WorkstreamID, ExpectedRevision: command.ExpectedRevision, Action: command.Action, Objective: command.Objective, Transition: transition}, true, nil
}
