package toolfactory

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/Dauno/slack-local-agent/internal/domain"
	workstreamusecase "github.com/Dauno/slack-local-agent/internal/usecase/workstream"
)

type workstreamBindingArgs struct {
	Project string `json:"project" jsonschema:"registered project bound to this workstream"`
}

type workstreamIDArgs struct {
	WorkstreamID string `json:"workstream_id" jsonschema:"durable workstream selector"`
	Project      string `json:"project" jsonschema:"registered project bound to this workstream"`
}

type workstreamCreateArgs struct {
	WorkstreamID string `json:"workstream_id" jsonschema:"new durable workstream identity"`
	Project      string `json:"project" jsonschema:"registered project for the workstream"`
	Objective    string `json:"objective" jsonschema:"human objective; this creates a proposed workstream"`
}

type workstreamTransitionArgs struct {
	WorkstreamID       string   `json:"workstream_id" jsonschema:"durable workstream selector"`
	Project            string   `json:"project" jsonschema:"registered project bound to the workstream"`
	ExpectedRevision   int      `json:"expected_revision" jsonschema:"current workstream revision required for CAS"`
	Action             string   `json:"action" jsonschema:"typed action such as activate_workstream, pause_workstream, resume_workstream, cancel_workstream, complete_workstream, propose_task, reject_task, revise_plan, record_constraint, propose_decision, request_human_decision, approve_decision, reject_decision, or resolve_question"`
	CurrentPhase       string   `json:"current_phase,omitempty"`
	TaskID             string   `json:"task_id,omitempty"`
	TaskDescription    string   `json:"task_description,omitempty"`
	Dependencies       []string `json:"dependencies,omitempty"`
	RequiredInputs     []string `json:"required_inputs,omitempty"`
	ConstraintID       string   `json:"constraint_id,omitempty"`
	ConstraintText     string   `json:"constraint_text,omitempty"`
	QuestionID         string   `json:"question_id,omitempty"`
	QuestionText       string   `json:"question_text,omitempty"`
	QuestionResolution string   `json:"question_resolution,omitempty"`
	DecisionID         string   `json:"decision_id,omitempty"`
	DecisionProposal   string   `json:"decision_proposal,omitempty"`
	DecisionRationale  string   `json:"decision_rationale,omitempty"`
}

type workstreamLinkResultArgs struct {
	WorkstreamID     string `json:"workstream_id" jsonschema:"durable workstream selector"`
	Project          string `json:"project" jsonschema:"registered project bound to the workstream"`
	ExpectedRevision int    `json:"expected_revision" jsonschema:"current workstream revision required for CAS"`
	ResultID         string `json:"result_id" jsonschema:"opaque verified result handle identity"`
	ResultLinkID     string `json:"result_link_id" jsonschema:"new durable workstream result-link identity"`
	TaskID           string `json:"task_id,omitempty"`
	Description      string `json:"description,omitempty"`
}

type workstreamResultArgs struct {
	WorkstreamID string `json:"workstream_id" jsonschema:"durable workstream selector"`
	Project      string `json:"project" jsonschema:"registered project bound to the workstream"`
	ResultID     string `json:"result_id" jsonschema:"opaque verified result handle identity"`
}

type workstreamResultChunkArgs struct {
	WorkstreamID string `json:"workstream_id" jsonschema:"durable workstream selector"`
	Project      string `json:"project" jsonschema:"registered project bound to the workstream"`
	ResultID     string `json:"result_id" jsonschema:"opaque verified result handle identity"`
	OffsetBytes  int64  `json:"offset_bytes,omitzero" jsonschema:"server-provided UTF-8 continuation offset"`
	MaxBytes     int64  `json:"max_bytes,omitzero" jsonschema:"maximum bytes requested for this bounded chunk"`
}

type workstreamConstraintResult struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type workstreamDecisionResult struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Proposal string `json:"proposal"`
}

type workstreamQuestionResult struct {
	ID         string `json:"id"`
	Text       string `json:"text"`
	Status     string `json:"status"`
	Resolution string `json:"resolution,omitempty"`
}

type workstreamTaskResult struct {
	ID             string   `json:"id"`
	Project        string   `json:"project"`
	Description    string   `json:"description"`
	Status         string   `json:"status"`
	Dependencies   []string `json:"dependencies,omitempty"`
	RequiredInputs []string `json:"required_inputs,omitempty"`
	ResultIdentity string   `json:"result_identity,omitempty"`
	Integrated     bool     `json:"integrated"`
}

type workstreamResultLinkResult struct {
	ID             string `json:"id"`
	TaskID         string `json:"task_id,omitempty"`
	ResultIdentity string `json:"result_identity"`
	Description    string `json:"description,omitempty"`
}

type workstreamStateResult struct {
	WorkstreamID  string                       `json:"workstream_id"`
	Project       string                       `json:"project"`
	Status        string                       `json:"status"`
	Revision      int                          `json:"revision"`
	Objective     string                       `json:"objective"`
	CurrentPhase  string                       `json:"current_phase,omitempty"`
	Constraints   []workstreamConstraintResult `json:"constraints,omitempty"`
	Decisions     []workstreamDecisionResult   `json:"decisions,omitempty"`
	Tasks         []workstreamTaskResult       `json:"tasks,omitempty"`
	OpenQuestions []workstreamQuestionResult   `json:"open_questions,omitempty"`
	ResultLinks   []workstreamResultLinkResult `json:"result_links,omitempty"`
}

type workstreamTransitionResult struct {
	Action       string                `json:"action"`
	FromRevision int                   `json:"from_revision"`
	ToRevision   int                   `json:"to_revision"`
	State        workstreamStateResult `json:"state"`
}

type workstreamResultHandleResult struct {
	ResultID     string                      `json:"result_id"`
	SHA256       string                      `json:"sha256"`
	Bytes        int64                       `json:"bytes"`
	MediaType    string                      `json:"media_type"`
	Availability []domain.ResultAvailability `json:"availability"`
}

type workstreamResultChunkResult struct {
	Content         string `json:"content"`
	OffsetBytes     int64  `json:"offset_bytes"`
	NextOffsetBytes int64  `json:"next_offset_bytes"`
	EOF             bool   `json:"eof"`
	SHA256          string `json:"sha256"`
}

func (f *Factory) workstreamGetTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "workstream_get",
		Description: "Reads the durable workstream snapshot bound to the trusted Slack actor, conversation, and registered project. Read-only.",
	}, func(ctx agent.Context, args workstreamIDArgs) (workstreamStateResult, error) {
		if strings.TrimSpace(args.WorkstreamID) == "" || strings.TrimSpace(args.Project) == "" {
			return workstreamStateResult{}, errors.New("workstream_id and project are required")
		}
		snapshot, err := f.workstreams.Get(ctx, workstreamusecase.Binding{Actor: actor, ConversationKey: key, Project: args.Project}, args.WorkstreamID)
		if err != nil {
			return workstreamStateResult{}, err
		}
		return renderWorkstreamState(snapshot), nil
	})
}

func (f *Factory) workstreamActiveTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "workstream_active",
		Description: "Reads the one non-terminal workstream bound to this conversation and registered project. Read-only.",
	}, func(ctx agent.Context, args workstreamBindingArgs) (workstreamStateResult, error) {
		if strings.TrimSpace(args.Project) == "" {
			return workstreamStateResult{}, errors.New("project is required")
		}
		snapshot, err := f.workstreams.Active(ctx, workstreamusecase.Binding{Actor: actor, ConversationKey: key, Project: args.Project})
		if err != nil {
			return workstreamStateResult{}, err
		}
		return renderWorkstreamState(snapshot), nil
	})
}

func (f *Factory) workstreamCreateTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "workstream_create",
		Description: "Creates a proposed durable workstream for one registered project. Requires explicit human confirmation.",
	}, func(ctx agent.Context, args workstreamCreateArgs) (workstreamStateResult, error) {
		if strings.TrimSpace(args.WorkstreamID) == "" || strings.TrimSpace(args.Project) == "" || strings.TrimSpace(args.Objective) == "" {
			return workstreamStateResult{}, errors.New("workstream_id, project, and objective are required")
		}
		callID := strings.TrimSpace(ctx.FunctionCallID())
		if callID == "" {
			return workstreamStateResult{}, errors.New("workstream creation function call ID is required")
		}
		confirmation := ctx.ToolConfirmation()
		if confirmation == nil {
			if err := f.workstreams.ValidateCreate(ctx, workstreamusecase.Binding{Actor: actor, ConversationKey: key, Project: args.Project}, args.WorkstreamID, args.Objective); err != nil {
				return workstreamStateResult{}, err
			}
			transition := workstreamConfirmationTransition(args, domain.WorkstreamActionCreateWorkstream)
			if err := ctx.RequestConfirmation(workstreamConfirmationText(transition), workstreamConfirmationPayload(transition)); err != nil {
				return workstreamStateResult{}, err
			}
			return workstreamStateResult{}, nil
		}
		if !confirmation.Confirmed {
			return workstreamStateResult{}, errors.New("workstream creation confirmation was rejected")
		}
		snapshot, err := f.workstreams.CreateFromRoot(ctx, workstreamusecase.Binding{Actor: actor, ConversationKey: key, Project: args.Project}, args.WorkstreamID, args.Objective, callID)
		if err != nil {
			return workstreamStateResult{}, err
		}
		return renderWorkstreamState(snapshot), nil
	})
}

func (f *Factory) workstreamTransitionTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "workstream_transition",
		Description: "Applies one typed root proposal to durable workstream state. Reversible planning actions do not ask again; authority-boundary actions require explicit human confirmation. The host enforces actor, conversation, project, revision, and dependency policy.",
	}, func(ctx agent.Context, args workstreamTransitionArgs) (workstreamTransitionResult, error) {
		if strings.TrimSpace(args.WorkstreamID) == "" || strings.TrimSpace(args.Project) == "" || strings.TrimSpace(args.Action) == "" {
			return workstreamTransitionResult{}, errors.New("workstream_id, project, and action are required")
		}
		action := domain.WorkstreamAction(args.Action)
		if !validWorkstreamToolAction(action) {
			return workstreamTransitionResult{}, fmt.Errorf("unknown workstream action %q", action)
		}
		callID := strings.TrimSpace(ctx.FunctionCallID())
		if callID == "" {
			return workstreamTransitionResult{}, errors.New("workstream transition function call ID is required")
		}
		transition := workstreamTransitionFromArgs(args, action, callID)
		if workstreamActionRequiresConfirmation(action) {
			confirmation := ctx.ToolConfirmation()
			if confirmation == nil {
				if err := f.workstreams.ValidateProposal(
					ctx,
					workstreamusecase.Binding{Actor: actor, ConversationKey: key, Project: args.Project},
					domain.WorkstreamSourceRoot,
					transition,
				); err != nil {
					return workstreamTransitionResult{}, err
				}
				if err := ctx.RequestConfirmation(workstreamConfirmationText(transition), workstreamConfirmationPayload(transition)); err != nil {
					return workstreamTransitionResult{}, err
				}
				return workstreamTransitionResult{}, nil
			}
			if !confirmation.Confirmed {
				return workstreamTransitionResult{}, errors.New("workstream transition confirmation was rejected")
			}
			record, snapshot, err := f.workstreams.ApplyHostConfirmed(
				ctx,
				workstreamusecase.Binding{Actor: actor, ConversationKey: key, Project: args.Project},
				domain.WorkstreamSourceRoot,
				transition,
				callID,
			)
			if err != nil {
				return workstreamTransitionResult{}, err
			}
			return workstreamTransitionResult{Action: string(record.Action), FromRevision: record.FromRevision, ToRevision: record.ToRevision, State: renderWorkstreamState(snapshot)}, nil
		}
		record, snapshot, err := f.workstreams.Apply(ctx, workstreamusecase.Binding{Actor: actor, ConversationKey: key, Project: args.Project}, domain.WorkstreamSourceRoot, transition)
		if err != nil {
			return workstreamTransitionResult{}, err
		}
		return workstreamTransitionResult{Action: string(record.Action), FromRevision: record.FromRevision, ToRevision: record.ToRevision, State: renderWorkstreamState(snapshot)}, nil
	})
}

func (f *Factory) workstreamLinkCompletedResultTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "workstream_link_completed_result",
		Description: "Links one completed, verified V2 result to the durable workstream. Requires explicit human confirmation; actor, team, conversation, project, and result identity are verified by the host.",
	}, func(ctx agent.Context, args workstreamLinkResultArgs) (workstreamTransitionResult, error) {
		if strings.TrimSpace(args.WorkstreamID) == "" || strings.TrimSpace(args.Project) == "" || strings.TrimSpace(args.ResultID) == "" || strings.TrimSpace(args.ResultLinkID) == "" {
			return workstreamTransitionResult{}, errors.New("workstream_id, project, result_id, and result_link_id are required")
		}
		callID := strings.TrimSpace(ctx.FunctionCallID())
		if callID == "" {
			return workstreamTransitionResult{}, errors.New("workstream result-link function call ID is required")
		}
		transition := domain.WorkstreamTransition{
			WorkstreamID: args.WorkstreamID, ExpectedRevision: args.ExpectedRevision, Project: args.Project,
			SourceID: callID, Action: domain.WorkstreamActionLinkCompletedResult,
			ResultLink: &domain.WorkstreamResultLink{ID: args.ResultLinkID, TaskID: args.TaskID, ResultIdentity: args.ResultID, Description: args.Description},
		}
		confirmation := ctx.ToolConfirmation()
		if confirmation == nil {
			if err := f.workstreams.ValidateProposal(ctx, workstreamusecase.Binding{Actor: actor, ConversationKey: key, Project: args.Project}, domain.WorkstreamSourceRoot, transition); err != nil {
				return workstreamTransitionResult{}, err
			}
			if err := ctx.RequestConfirmation(workstreamConfirmationText(transition), workstreamConfirmationPayload(transition)); err != nil {
				return workstreamTransitionResult{}, err
			}
			return workstreamTransitionResult{}, nil
		}
		if !confirmation.Confirmed {
			return workstreamTransitionResult{}, errors.New("workstream result-link confirmation was rejected")
		}
		record, snapshot, err := f.workstreams.LinkCompletedResult(ctx,
			workstreamusecase.Binding{Actor: actor, ConversationKey: key, Project: args.Project}, teamIDFromConversation(key), args.ResultID, transition, callID)
		if err != nil {
			return workstreamTransitionResult{}, err
		}
		return workstreamTransitionResult{Action: string(record.Action), FromRevision: record.FromRevision, ToRevision: record.ToRevision, State: renderWorkstreamState(snapshot)}, nil
	})
}

func (f *Factory) workstreamResultHandleTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "workstream_result_handle",
		Description: "Reads one bounded verified V2 result handle through a trusted workstream binding. Read-only; no payload bytes are returned.",
	}, func(ctx agent.Context, args workstreamResultArgs) (workstreamResultHandleResult, error) {
		if strings.TrimSpace(args.WorkstreamID) == "" || strings.TrimSpace(args.Project) == "" || strings.TrimSpace(args.ResultID) == "" {
			return workstreamResultHandleResult{}, errors.New("workstream_id, project, and result_id are required")
		}
		handle, err := f.workstreams.ResultHandleForWorkstream(ctx,
			workstreamusecase.Binding{Actor: actor, ConversationKey: key, Project: args.Project}, teamIDFromConversation(key), args.ResultID, args.WorkstreamID)
		if err != nil {
			return workstreamResultHandleResult{}, err
		}
		return workstreamResultHandleResult{
			ResultID:     handle.ResultID,
			SHA256:       handle.SHA256,
			Bytes:        handle.Bytes,
			MediaType:    handle.MediaType,
			Availability: append([]domain.ResultAvailability(nil), handle.Availability...),
		}, nil
	})
}

func (f *Factory) workstreamReadResultChunkTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "workstream_read_result_chunk",
		Description: "Reads one bounded UTF-8 chunk from a fully verified V2 result through a trusted workstream binding. Read-only; complete payloads are never returned.",
	}, func(ctx agent.Context, args workstreamResultChunkArgs) (workstreamResultChunkResult, error) {
		if strings.TrimSpace(args.WorkstreamID) == "" || strings.TrimSpace(args.Project) == "" || strings.TrimSpace(args.ResultID) == "" {
			return workstreamResultChunkResult{}, errors.New("workstream_id, project, and result_id are required")
		}
		chunk, err := f.workstreams.ReadResultChunkForWorkstream(ctx,
			workstreamusecase.Binding{Actor: actor, ConversationKey: key, Project: args.Project}, teamIDFromConversation(key), args.ResultID, args.WorkstreamID, args.OffsetBytes, args.MaxBytes)
		if err != nil {
			return workstreamResultChunkResult{}, err
		}
		return workstreamResultChunkResult{Content: chunk.Content, OffsetBytes: chunk.OffsetBytes, NextOffsetBytes: chunk.NextOffsetBytes, EOF: chunk.EOF, SHA256: chunk.SHA256}, nil
	})
}

func workstreamConfirmationHint(workstreamID string, expectedRevision int, project string, action domain.WorkstreamAction) string {
	return fmt.Sprintf(
		"Approve workstream %q action %q at revision %d for registered project %q. This confirmation is bound to the current actor and conversation.",
		workstreamID,
		action,
		expectedRevision,
		project,
	)
}

func workstreamConfirmationPayload(transition domain.WorkstreamTransition) map[string]any {
	payload := map[string]any{
		"workstream_id":       transition.WorkstreamID,
		"expected_revision":   transition.ExpectedRevision,
		"project":             transition.Project,
		"action":              string(transition.Action),
		"payload_digest":      transition.PayloadDigestValue(),
		"task":                transition.Task,
		"task_id":             transition.TaskID,
		"result_link":         transition.ResultLink,
		"constraint":          transition.Constraint,
		"decision":            transition.Decision,
		"decision_id":         transition.DecisionID,
		"question":            transition.Question,
		"question_id":         transition.QuestionID,
		"question_resolution": transition.QuestionResolution,
		"current_phase":       transition.CurrentPhase,
	}
	if transition.Action == domain.WorkstreamActionCreateWorkstream {
		payload["objective"] = transition.Objective
	}
	return payload
}

func workstreamConfirmationText(transition domain.WorkstreamTransition) string {
	return workstreamConfirmationHint(transition.WorkstreamID, transition.ExpectedRevision, transition.Project, transition.Action) +
		"\nScope: " + transition.Project +
		"\nPayload digest: " + transition.PayloadDigestValue()
}

func workstreamCreateArgsTransition(args workstreamCreateArgs, action domain.WorkstreamAction) domain.WorkstreamTransition {
	return domain.WorkstreamTransition{WorkstreamID: args.WorkstreamID, ExpectedRevision: 0, Project: args.Project, Action: action, Objective: args.Objective}
}

func workstreamConfirmationTransition(args workstreamCreateArgs, action domain.WorkstreamAction) domain.WorkstreamTransition {
	return workstreamCreateArgsTransition(args, action)
}

func workstreamTransitionFromArgs(args workstreamTransitionArgs, action domain.WorkstreamAction, sourceID string) domain.WorkstreamTransition {
	transition := domain.WorkstreamTransition{
		WorkstreamID: args.WorkstreamID, ExpectedRevision: args.ExpectedRevision,
		Project:  args.Project,
		SourceID: sourceID, Action: action, CurrentPhase: args.CurrentPhase, TaskID: args.TaskID,
	}
	switch action {
	case domain.WorkstreamActionProposeTask:
		transition.Task = &domain.WorkstreamTask{
			ID:             args.TaskID,
			Project:        args.Project,
			Description:    args.TaskDescription,
			Status:         domain.TaskProposed,
			Dependencies:   args.Dependencies,
			RequiredInputs: args.RequiredInputs,
		}
	case domain.WorkstreamActionRecordConstraint:
		transition.Constraint = &domain.WorkstreamConstraint{ID: args.ConstraintID, Text: args.ConstraintText, SourceID: sourceID}
	case domain.WorkstreamActionProposeDecision:
		transition.Decision = &domain.WorkstreamDecision{ID: args.DecisionID, Status: domain.DecisionProposed, Proposal: args.DecisionProposal, Source: sourceID, Rationale: args.DecisionRationale}
	case domain.WorkstreamActionRequestHumanDecision:
		transition.Question = &domain.WorkstreamQuestion{ID: args.QuestionID, Text: args.QuestionText, Status: domain.QuestionOpen, SourceID: sourceID}
	case domain.WorkstreamActionApproveDecision, domain.WorkstreamActionRejectDecision:
		transition.DecisionID = args.DecisionID
	case domain.WorkstreamActionResolveQuestion:
		transition.QuestionID = args.QuestionID
		transition.QuestionResolution = args.QuestionResolution
	}
	return transition
}

func validWorkstreamToolAction(action domain.WorkstreamAction) bool {
	switch action {
	case domain.WorkstreamActionActivateWorkstream, domain.WorkstreamActionPauseWorkstream,
		domain.WorkstreamActionResumeWorkstream, domain.WorkstreamActionCancelWorkstream,
		domain.WorkstreamActionCompleteWorkstream, domain.WorkstreamActionProposeTask,
		domain.WorkstreamActionRejectTask, domain.WorkstreamActionRevisePlan, domain.WorkstreamActionRecordConstraint,
		domain.WorkstreamActionProposeDecision, domain.WorkstreamActionRequestHumanDecision,
		domain.WorkstreamActionApproveDecision,
		domain.WorkstreamActionRejectDecision, domain.WorkstreamActionResolveQuestion,
		domain.WorkstreamActionLinkCompletedResult:
		return true
	default:
		return false
	}
}

func workstreamActionRequiresConfirmation(action domain.WorkstreamAction) bool {
	if !validWorkstreamToolAction(action) {
		return false
	}
	return (domain.WorkstreamTransition{Source: domain.WorkstreamSourceRoot, Action: action}).RequiresConfirmation()
}

func renderWorkstreamState(snapshot domain.WorkstreamSnapshot) workstreamStateResult {
	result := workstreamStateResult{
		WorkstreamID: snapshot.ID,
		Project:      snapshot.Project,
		Status:       string(snapshot.Status),
		Revision:     snapshot.Revision,
		Objective:    snapshot.Objective,
		CurrentPhase: snapshot.CurrentPhase,
	}
	for _, constraint := range snapshot.Constraints {
		result.Constraints = append(result.Constraints, workstreamConstraintResult{ID: constraint.ID, Text: constraint.Text})
	}
	for _, decision := range snapshot.Decisions {
		result.Decisions = append(result.Decisions, workstreamDecisionResult{ID: decision.ID, Status: string(decision.Status), Proposal: decision.Proposal})
	}
	for _, task := range snapshot.Tasks {
		result.Tasks = append(
			result.Tasks,
			workstreamTaskResult{
				ID:             task.ID,
				Project:        task.Project,
				Description:    task.Description,
				Status:         string(task.Status),
				Dependencies:   append([]string(nil), task.Dependencies...),
				RequiredInputs: append([]string(nil), task.RequiredInputs...),
				ResultIdentity: task.ResultIdentity,
				Integrated:     task.Integrated,
			},
		)
	}
	for _, question := range snapshot.OpenQuestions {
		result.OpenQuestions = append(result.OpenQuestions, workstreamQuestionResult{ID: question.ID, Text: question.Text, Status: string(question.Status), Resolution: question.Resolution})
	}
	for _, link := range snapshot.ResultLinks {
		result.ResultLinks = append(result.ResultLinks, workstreamResultLinkResult{ID: link.ID, TaskID: link.TaskID, ResultIdentity: link.ResultIdentity, Description: link.Description})
	}
	return result
}

func teamIDFromConversation(key domain.ConversationKey) string {
	value := strings.TrimPrefix(string(key), "slack:")
	if index := strings.IndexByte(value, ':'); index > 0 {
		return value[:index]
	}
	return ""
}
