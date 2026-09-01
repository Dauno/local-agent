package domain_test

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func digest(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func TestActivationFrameRenderIncludesBoundedWorkstreamSnapshot(t *testing.T) {
	task := domain.WorkstreamTask{
		ID: "task-1", JobID: "job-1", Project: "workspace", Description: "inspect the repository",
		Status: domain.TaskRunning, ExecutionIdentity: "exec-1", RequiredInputs: []string{"scope"},
	}
	frame := domain.ActivationFrame{
		ActivationID: "activation-1", JobID: "job-1", ActivationScope: domain.ExternalAgentActivationWorkstream, Actor: "U12345678", TeamID: "T12345678",
		ConversationKey: "slack:T12345678:dm:D12345678", TerminalStatus: domain.JobCompleted, PrimaryProject: "workspace",
		DelegatedTaskExcerpt: "inspect the repository", DelegatedTaskSHA256: digest("inspect the repository"),
		WorkstreamID: "ws-1", Workstream: domain.WorkstreamSnapshot{
			ID: "ws-1", ConversationKey: "slack:T12345678:dm:D12345678", OwnerActor: "U12345678",
			Project: "workspace", Status: domain.WorkstreamActive, Revision: 3,
			Objective: "finish the repository inspection", CurrentPhase: "investigation",
			Tasks: []domain.WorkstreamTask{task},
		},
		TaskID: "task-1", Task: task, ExecutionIdentity: "exec-1", AdmissionRevision: 2,
		Representation: domain.ActivationResultArtifactOnly,
		ResultSHA256:   strings.Repeat("a", 64), ResultBytes: 123,
	}

	rendered, err := frame.Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\"workstream\"", "finish the repository inspection", "\"task_id\":\"task-1\"", "\"result_representation\":\"artifact_only\"", "\"proposal_policy\":\"At most one text-only proposal"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered frame does not contain %q: %s", want, rendered)
		}
	}
	if strings.Contains(rendered, "result_text") {
		t.Fatalf("non-inline frame rendered result text: %s", rendered)
	}
}

func TestActivationFrameRejectsOversizedSnapshot(t *testing.T) {
	task := domain.WorkstreamTask{ID: "task-1", Project: "workspace", Description: "task", Status: domain.TaskProposed}
	frame := domain.ActivationFrame{
		ActivationID:         "activation-1",
		JobID:                "job-1",
		ActivationScope:      domain.ExternalAgentActivationWorkstream,
		PrimaryProject:       "workspace",
		DelegatedTaskExcerpt: "task",
		DelegatedTaskSHA256:  digest("task"),
		WorkstreamID:         "ws-1",
		Workstream: domain.WorkstreamSnapshot{
			ID: "ws-1", Project: "workspace", Status: domain.WorkstreamActive, Revision: 1,
			Objective: strings.Repeat("x", domain.HardMaxWorkstreamSnapshotRunes+1), Tasks: []domain.WorkstreamTask{task},
		},
		TaskID:            "task-1",
		Task:              task,
		AdmissionRevision: 1,
		Representation:    domain.ActivationResultUnavailable,
	}
	if err := frame.Validate(); err == nil {
		t.Fatal("oversized workstream snapshot was accepted")
	}
}

func TestActivationFrameNativeHandleRequiresBoundedMetadata(t *testing.T) {
	frame := domain.ActivationFrame{
		ActivationID:         "activation-1",
		JobID:                "job-1",
		ActivationScope:      domain.ExternalAgentActivationConversation,
		PrimaryProject:       "workspace",
		DelegatedTaskExcerpt: "task",
		DelegatedTaskSHA256:  digest("task"),
		Representation:       domain.ActivationResultNativeHandle,
		ResultSHA256:         strings.Repeat("a", 64),
		ResultBytes:          10,
	}
	if err := frame.Validate(); err == nil {
		t.Fatal("native handle without metadata was accepted")
	}
}
