package toolfactory_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/toolfactory"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

// projectionExternalJobReader is a stub external-agent reader that also
// implements the optional status projection contract.
type projectionExternalJobReader struct {
	stubExternalJobReader
	view *domain.ExternalAgentJobStatusView
}

func (r projectionExternalJobReader) StatusProjection(ctx context.Context, jobID, actor string, key domain.ConversationKey) (*domain.ExternalAgentJobStatusView, error) {
	if r.view == nil {
		return nil, errors.New("external-agent job was not found")
	}
	return r.view, nil
}

func TestJobStatusJSONContract(t *testing.T) {
	alive := true
	base := time.Date(2026, 8, 4, 2, 41, 2, 0, time.UTC)
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	reader := &projectionExternalJobReader{
		job: &domain.ExternalAgentJob{ID: "job_bb3ed6", Status: domain.JobRunning, StatusRevision: 1, ExternalAgentSessionID: "ses_full_identity_0123456789"},
		view: &domain.ExternalAgentJobStatusView{
			JobID: "job_bb3ed6", Status: domain.JobRunning, StatusRevision: 1,
			ExternalAgentSessionID: "ses_full_identity_0123456789",
			Phase:                  domain.ExternalAgentPhaseToolRunning, Health: domain.ExternalAgentHealthPossiblyStalled,
			LastEventKind:            domain.ExternalAgentEventToolCallUpdate,
			LastTransportActivityAt:  base,
			LastSessionUpdateAt:      base,
			LastMeaningfulProgressAt: base.Add(-4 * time.Second),
			ActiveToolCount:          1, PromptElapsedSeconds: 3780,
			ProcessAlive: &alive,
		},
	}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	tools, err := factory.ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	var statusTool runnableFunctionTool
	for _, candidate := range tools {
		if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == "job_status" {
			statusTool, _ = candidate.(runnableFunctionTool)
		}
	}
	if statusTool == nil {
		t.Fatal("job_status tool is unavailable")
	}
	value, err := statusTool.Run(&stubToolContext{}, map[string]any{"job_id": "job_bb3ed6"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	raw, _ := json.Marshal(value)
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["acp_session_id"] != "ses_full_identity_0123456789" {
		t.Fatalf("acp_session_id = %v", result["acp_session_id"])
	}
	if result["phase"] != "tool_running" || result["health"] != "possibly_stalled" {
		t.Fatalf("phase/health = %v/%v", result["phase"], result["health"])
	}
	if result["last_event_kind"] != "tool_call_update" {
		t.Fatalf("last_event_kind = %v", result["last_event_kind"])
	}
	if result["last_transport_activity_at"] != "2026-08-04T02:41:02Z" {
		t.Fatalf("last_transport_activity_at = %v", result["last_transport_activity_at"])
	}
	if result["active_tool_count"] != float64(1) || result["pending_permission"] != false {
		t.Fatalf("tool count/permission = %v/%v", result["active_tool_count"], result["pending_permission"])
	}
	if result["prompt_elapsed_seconds"] != float64(3780) {
		t.Fatalf("prompt_elapsed_seconds = %v", result["prompt_elapsed_seconds"])
	}
	if result["stop_reason"] != "" {
		t.Fatalf("stop_reason = %v", result["stop_reason"])
	}
	if result["process_alive"] != true {
		t.Fatalf("process_alive = %v", result["process_alive"])
	}
}

func TestJobStatusQueuedSessionIsEmpty(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	reader := &stubExternalJobReader{job: &domain.ExternalAgentJob{ID: "job_queued", Status: domain.JobQueued, StatusRevision: 0, ExternalAgentSessionID: ""}}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	tools, err := factory.ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	var statusTool runnableFunctionTool
	for _, candidate := range tools {
		if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == "job_status" {
			statusTool, _ = candidate.(runnableFunctionTool)
		}
	}
	value, err := statusTool.Run(&stubToolContext{}, map[string]any{"job_id": "job_queued"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	raw, _ := json.Marshal(value)
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["acp_session_id"] != "" {
		t.Fatalf("queued acp_session_id = %q, want empty", result["acp_session_id"])
	}
	if result["phase"] != "" || result["health"] != "" {
		t.Fatalf("queued projection = %v/%v", result["phase"], result["health"])
	}
}

// resultErrorExternalJobReader surfaces a bounded result-read failure from the
// read tools, simulating the closed errors the external-agent service returns.
type resultErrorExternalJobReader struct {
	stubExternalJobReader
	err error
}

func (r resultErrorExternalJobReader) ReadResult(context.Context, string, string, domain.ConversationKey) (domain.ExternalAgentJobResult, error) {
	return domain.ExternalAgentJobResult{}, r.err
}

func (r resultErrorExternalJobReader) ReadResultChunk(context.Context, string, string, domain.ConversationKey, int64, int64) (domain.ResultChunk, error) {
	return domain.ResultChunk{}, r.err
}

// TestJobResultToolsSurfaceOnlyBoundedResultErrorCodes verifies that the read
// tools propagate a service-produced ResultError as exactly its bounded code:
// no job ID, actor, conversation, digest, reference, path, or detail text is
// ever appended by the tool layer.
func TestJobResultToolsSurfaceOnlyBoundedResultErrorCodes(t *testing.T) {
	const (
		redactedContent = "REDACTED-TOOL-SECRET-4d91"
		redactedRef     = "job_redacted4d91-delivery.result"
		redactedPath    = "/var/run/local-agent-redacted-4d91/artifacts"
	)
	redactedDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(redactedContent)))
	forbidden := []string{redactedContent, redactedRef, redactedPath, redactedDigest, "synthetic detail"}
	for _, toolName := range []string{"read_job_result", "read_job_result_chunk"} {
		t.Run(toolName, func(t *testing.T) {
			for _, testCase := range []struct {
				name     string
				err      error
				wantCode string
			}{
				{
					name:     "bounded digest code with closed detail",
					err:      &domain.ResultError{Code: domain.ResultErrorArtifactDigestMismatch, Err: errors.New("synthetic detail")},
					wantCode: string(domain.ResultErrorArtifactDigestMismatch),
				},
				{
					name:     "bounded identity code",
					err:      &domain.ResultError{Code: domain.ResultErrorIdentityInvalid},
					wantCode: string(domain.ResultErrorIdentityInvalid),
				},
			} {
				t.Run(testCase.name, func(t *testing.T) {
					reader := resultErrorExternalJobReader{job: &domain.ExternalAgentJob{ID: "job_tool", Status: domain.JobCompleted, StatusRevision: 4}, err: testCase.err}
					factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
					tools, err := factory.ToolsForInvocation("U12345678", domain.ConversationKey("slack:T12345678:dm:D12345678"))
					if err != nil {
						t.Fatalf("tools: %v", err)
					}
					var tool runnableFunctionTool
					for _, candidate := range tools {
						if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == toolName {
							tool, _ = candidate.(runnableFunctionTool)
						}
					}
					if tool == nil {
						t.Fatalf("%s tool is unavailable", toolName)
					}
					_, err = tool.Run(&stubToolContext{}, map[string]any{"job_id": "job_tool"})
					if err == nil {
						t.Fatalf("%s accepted a failing result read", toolName)
					}
					if err.Error() != testCase.wantCode {
						t.Fatalf("%s error = %q, want exact bounded code %q", toolName, err.Error(), testCase.wantCode)
					}
					for _, value := range forbidden {
						if strings.Contains(err.Error(), value) {
							t.Fatalf("%s error %q leaked %q", toolName, err.Error(), value)
						}
					}
				})
			}
		})
	}
}

// runJobStatusTool invokes the bound job_status tool and returns the decoded
// JSON response for the given reader fixture.
func runJobStatusTool(t *testing.T, reader stubExternalJobReader, key domain.ConversationKey) map[string]any {
	t.Helper()
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	tools, err := factory.ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	var statusTool runnableFunctionTool
	for _, candidate := range tools {
		if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == "job_status" {
			statusTool, _ = candidate.(runnableFunctionTool)
		}
	}
	if statusTool == nil {
		t.Fatal("job_status tool is unavailable")
	}
	value, err := statusTool.Run(&stubToolContext{}, map[string]any{"job_id": reader.job.ID})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	raw, _ := json.Marshal(value)
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return result
}

func TestJobStatusUsesStrictResultAvailability(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	summary := "safe &lt;complete&gt;"
	summaryDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(summary)))
	wrongDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("other")))
	artifactDigest := strings.Repeat("a", 64)
	tests := []struct {
		name string
		job  domain.ExternalAgentJob
		want bool
	}{
		{
			name: "completed inline with complete identity",
			job:  domain.ExternalAgentJob{ID: "job_inline", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: summary, ResultSHA256: summaryDigest, ResultBytes: int64(len([]byte(summary)))},
			want: true,
		},
		{
			name: "completed file mode with complete identity",
			job:  domain.ExternalAgentJob{ID: "job_file", Status: domain.JobCompleted, StatusRevision: 4, ResultArtifact: "job_file-delivery.result", ResultSHA256: artifactDigest, ResultBytes: 1024},
			want: true,
		},
		{
			name: "completed with empty summary",
			job:  domain.ExternalAgentJob{ID: "job_empty", Status: domain.JobCompleted, StatusRevision: 4, ResultSHA256: summaryDigest, ResultBytes: int64(len([]byte(summary)))},
			want: false,
		},
		{
			name: "completed with empty SHA",
			job:  domain.ExternalAgentJob{ID: "job_nosha", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: summary, ResultBytes: int64(len([]byte(summary)))},
			want: false,
		},
		{
			name: "completed with wrong SHA value",
			job:  domain.ExternalAgentJob{ID: "job_wrong", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: summary, ResultSHA256: wrongDigest, ResultBytes: int64(len([]byte(summary)))},
			want: false,
		},
		{
			name: "completed with malformed SHA",
			job:  domain.ExternalAgentJob{ID: "job_badsha", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: summary, ResultSHA256: "digest", ResultBytes: int64(len([]byte(summary)))},
			want: false,
		},
		{
			name: "completed with zero result bytes",
			job:  domain.ExternalAgentJob{ID: "job_zero", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: summary, ResultSHA256: summaryDigest, ResultBytes: 0},
			want: false,
		},
		{
			name: "completed with mismatched byte count",
			job:  domain.ExternalAgentJob{ID: "job_mismatch", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: summary, ResultSHA256: summaryDigest, ResultBytes: int64(len([]byte(summary))) + 1},
			want: false,
		},
		{
			name: "completed artifact with path-like reference",
			job:  domain.ExternalAgentJob{ID: "job_badref", Status: domain.JobCompleted, StatusRevision: 4, ResultArtifact: "dir/job-delivery.result", ResultSHA256: artifactDigest, ResultBytes: 1024},
			want: false,
		},
		{
			name: "completed artifact with zero bytes",
			job:  domain.ExternalAgentJob{ID: "job_artifactzero", Status: domain.JobCompleted, StatusRevision: 4, ResultArtifact: "job_artifactzero-delivery.result", ResultSHA256: artifactDigest, ResultBytes: 0},
			want: false,
		},
		{
			name: "incoherent artifact does not fall back to inline",
			job:  domain.ExternalAgentJob{ID: "job_dual", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: summary, ResultSHA256: summaryDigest, ResultBytes: int64(len([]byte(summary))), ResultArtifact: "bad/ref.result"},
			want: false,
		},
		{
			name: "non-completed with complete inline identity",
			job:  domain.ExternalAgentJob{ID: "job_failed", Status: domain.JobFailed, StatusRevision: 4, ResultSummary: summary, ResultSHA256: summaryDigest, ResultBytes: int64(len([]byte(summary)))},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := stubExternalJobReader{job: &tt.job}
			result := runJobStatusTool(t, reader, key)
			if result["result_available"] != tt.want {
				t.Fatalf("result_available = %v, want %v", result["result_available"], tt.want)
			}
			if result["status"] != string(tt.job.Status) {
				t.Fatalf("status = %v", result["status"])
			}
		})
	}
}

func TestJobStatusTerminalRetainsSessionID(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	reader := &stubExternalJobReader{job: &domain.ExternalAgentJob{
		ID: "job_terminal", Status: domain.JobCompletionUnknown, StatusRevision: 3, ExternalAgentSessionID: "ses_terminal_identity_9876543210",
	}}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	tools, err := factory.ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	var statusTool runnableFunctionTool
	for _, candidate := range tools {
		if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == "job_status" {
			statusTool, _ = candidate.(runnableFunctionTool)
		}
	}
	value, err := statusTool.Run(&stubToolContext{}, map[string]any{"job_id": "job_terminal"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	raw, _ := json.Marshal(value)
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["acp_session_id"] != "ses_terminal_identity_9876543210" {
		t.Fatalf("terminal acp_session_id = %v", result["acp_session_id"])
	}
}

func TestActivationStatusKeepsRevisionBoundSnapshot(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(&stubExternalJobReader{})
	activation := domain.ExternalAgentJobActivation{
		ActivationID: "activation-1", JobID: "job_act", StatusRevision: 4, Kind: "terminal",
		TerminalStatus: domain.JobCompleted, Actor: "U12345678", ConversationKey: key,
	}
	tools, err := factory.ToolsForActivation(activation.Actor, key, activation)
	if err != nil {
		t.Fatalf("activation tools: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("activation tools = %d, want none", len(tools))
	}
}
