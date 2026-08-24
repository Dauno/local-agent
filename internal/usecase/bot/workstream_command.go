package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const humanWorkstreamCommandPrefix = "workstream-human "

type humanWorkstreamCommand struct {
	Project              string                      `json:"project"`
	WorkstreamID         string                      `json:"workstream_id"`
	ExpectedRevision     int                         `json:"expected_revision"`
	Action               string                      `json:"action"`
	Objective            string                      `json:"objective,omitempty"`
	CurrentPhase         string                      `json:"current_phase,omitempty"`
	TaskID               string                      `json:"task_id,omitempty"`
	TaskDescription      string                      `json:"task_description,omitempty"`
	Dependencies         []string                    `json:"dependencies,omitempty"`
	RequiredInputs       []string                    `json:"required_inputs,omitempty"`
	SourceResultIdentity string                      `json:"source_result_identity,omitempty"`
	ConstraintID         string                      `json:"constraint_id,omitempty"`
	ConstraintText       string                      `json:"constraint_text,omitempty"`
	QuestionID           string                      `json:"question_id,omitempty"`
	QuestionText         string                      `json:"question_text,omitempty"`
	QuestionResolution   string                      `json:"question_resolution,omitempty"`
	DecisionID           string                      `json:"decision_id,omitempty"`
	DecisionProposal     string                      `json:"decision_proposal,omitempty"`
	DecisionRationale    string                      `json:"decision_rationale,omitempty"`
	Transition           domain.WorkstreamTransition `json:"-"`
}

func validSourceResultIdentity(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// applyHumanWorkstreamCommand executes one validated human workstream command.
// The caller must hold the conversation coordinator for the command's
// conversation key.
func (s *Service) applyHumanWorkstreamCommand(ctx context.Context, invocation domain.Invocation, key domain.ConversationKey, command humanWorkstreamCommand) (Outcome, error) {
	binding := port.WorkstreamBinding{Actor: invocation.UserID, ConversationKey: key, Project: command.Project}
	if command.Transition.Action == domain.WorkstreamActionCreateWorkstream {
		if _, createErr := s.workstreams.CreateHuman(ctx, binding, command.WorkstreamID, command.Objective, "slack-human:"+invocation.EventID); createErr != nil {
			if _, publishErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.sanitize("Workstream creation rejected: "+createErr.Error())); publishErr != nil {
				return OutcomePublishFailed, nil //nolint:nilerr // publish failure is reported via the Outcome sentinel, not err
			}
			return OutcomeResponded, nil
		}
		if _, publishErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), "Workstream `"+command.WorkstreamID+"` created at revision `0`."); publishErr != nil {
			return OutcomePublishFailed, nil //nolint:nilerr // publish failure is reported via the Outcome sentinel, not err
		}
		return OutcomeResponded, nil
	}
	command.Transition.SourceID = "slack-human:" + invocation.EventID
	record, _, applyErr := s.workstreams.ApplyHuman(ctx, binding, command.Transition)
	if applyErr != nil {
		if _, publishErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), s.sanitize("Workstream transition rejected: "+applyErr.Error())); publishErr != nil {
			return OutcomePublishFailed, nil //nolint:nilerr // publish failure is reported via the Outcome sentinel, not err
		}
		return OutcomeResponded, nil
	}
	message := fmt.Sprintf("Workstream `%s` applied human action `%s` at revision `%d`.", record.WorkstreamID, record.Action, record.ToRevision)
	if _, publishErr := s.publisher.Publish(ctx, invocation.ReplyTarget(), message); publishErr != nil {
		return OutcomePublishFailed, nil //nolint:nilerr // publish failure is reported via the Outcome sentinel, not err
	}
	return OutcomeResponded, nil
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
		requiredInputs := append([]string(nil), command.RequiredInputs...)
		if command.SourceResultIdentity != "" {
			if !validSourceResultIdentity(command.SourceResultIdentity) {
				return humanWorkstreamCommand{}, true, errors.New("source_result_identity must be a 64-character lowercase hex result ID")
			}
			duplicate := slices.Contains(requiredInputs, command.SourceResultIdentity)
			if !duplicate {
				requiredInputs = append(requiredInputs, command.SourceResultIdentity)
			}
		}
		transition.Task = &domain.WorkstreamTask{ID: command.TaskID, Project: command.Project, Description: command.TaskDescription, Status: domain.TaskProposed, Dependencies: command.Dependencies, RequiredInputs: requiredInputs}
	case domain.WorkstreamActionStartTask:
		if strings.TrimSpace(command.TaskID) == "" {
			return humanWorkstreamCommand{}, true, errors.New("task_id is required to start a task")
		}
		transition.TaskID = command.TaskID
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
