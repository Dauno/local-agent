package domain

import (
	"strconv"
	"testing"
	"time"
)

func TestProgressApplyLifecyclePhases(t *testing.T) {
	now := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	apply := func(event ACPProgressEvent) {
		now = now.Add(time.Second)
		progress.Apply(event, now)
	}
	apply(ACPProgressEvent{Kind: ACPEventProcessStarted, PID: 42})
	if progress.Phase != ACPPhaseStarting {
		t.Fatalf("phase after process start = %s, want starting", progress.Phase)
	}
	apply(ACPProgressEvent{Kind: ACPEventInitializeResponse})
	apply(ACPProgressEvent{Kind: ACPEventSessionNew})
	if progress.Phase != ACPPhaseSessionReady {
		t.Fatalf("phase after session/new = %s, want session_ready", progress.Phase)
	}
	apply(ACPProgressEvent{Kind: ACPEventPromptSent})
	if progress.Phase != ACPPhaseAgentProcessing || progress.PromptStartedAt != now {
		t.Fatalf("phase after prompt sent = %s, prompt_started=%v", progress.Phase, progress.PromptStartedAt)
	}
	apply(ACPProgressEvent{Kind: ACPEventPlan})
	if progress.Phase != ACPPhasePlanning {
		t.Fatalf("phase after plan = %s, want planning", progress.Phase)
	}
	apply(ACPProgressEvent{Kind: ACPEventToolCall, Tool: &ACPToolProgress{CallID: "tool-1", Kind: ACPToolKindExecute, Status: ACPToolStatusPending}})
	if progress.Phase != ACPPhaseToolPending || progress.ActiveToolCount != 1 {
		t.Fatalf("phase after tool_call = %s count=%d", progress.Phase, progress.ActiveToolCount)
	}
	apply(ACPProgressEvent{Kind: ACPEventPermissionRequested, PermissionPending: true})
	if progress.Phase != ACPPhaseWaitingPermission || !progress.PendingPermission {
		t.Fatalf("phase after permission request = %s pending=%t", progress.Phase, progress.PendingPermission)
	}
	apply(ACPProgressEvent{Kind: ACPEventPermissionResponded, PermissionPending: false})
	if progress.Phase != ACPPhaseAgentProcessing || progress.PendingPermission {
		t.Fatalf("phase after permission response = %s pending=%t", progress.Phase, progress.PendingPermission)
	}
	apply(ACPProgressEvent{Kind: ACPEventToolCallUpdate, Tool: &ACPToolProgress{CallID: "tool-1", Status: ACPToolStatusRunning}})
	if progress.Phase != ACPPhaseToolRunning {
		t.Fatalf("phase after tool running = %s", progress.Phase)
	}
	if progress.LastToolKind != ACPToolKindExecute {
		t.Fatalf("partial tool update replaced kind = %s", progress.LastToolKind)
	}
	apply(ACPProgressEvent{Kind: ACPEventMessageChunk})
	if progress.Phase != ACPPhaseResponding {
		t.Fatalf("phase after message chunk = %s, want responding", progress.Phase)
	}
	apply(ACPProgressEvent{Kind: ACPEventToolCallUpdate, Tool: &ACPToolProgress{CallID: "tool-1", Status: ACPToolStatusTerminal}})
	if progress.Phase != ACPPhaseAgentProcessing || progress.ActiveToolCount != 0 {
		t.Fatalf("phase after tool terminal = %s count=%d, want agent_processing/0", progress.Phase, progress.ActiveToolCount)
	}
	apply(ACPProgressEvent{Kind: ACPEventPromptResponse, StopReason: ACPStopReasonEndTurn})
	if progress.Phase != ACPPhaseCompleted || progress.StopReason != ACPStopReasonEndTurn {
		t.Fatalf("phase after end_turn = %s stop=%q", progress.Phase, progress.StopReason)
	}
	if err := progress.Validate(); err != nil {
		t.Fatalf("final projection must validate: %v", err)
	}
}

func TestProgressApplyClocks(t *testing.T) {
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	// Outbound events do not refresh transport activity.
	progress.Apply(ACPProgressEvent{Kind: ACPEventPromptSent}, base.Add(time.Second))
	if !progress.LastTransportActivityAt.IsZero() {
		t.Fatalf("prompt_sent must not refresh transport activity")
	}
	// Session updates refresh transport and session clocks.
	progress.Apply(ACPProgressEvent{Kind: ACPEventMessageChunk}, base.Add(2*time.Second))
	if progress.LastTransportActivityAt != base.Add(2*time.Second) || progress.LastSessionUpdateAt != base.Add(2*time.Second) {
		t.Fatalf("message chunk did not refresh transport/session clocks")
	}
	// Unknown notifications have already been attributed to the expected
	// session by the adapter, so they refresh both clocks.
	progress.Apply(ACPProgressEvent{Kind: ACPEventUnknownNotification}, base.Add(3*time.Second))
	if progress.LastTransportActivityAt != base.Add(3*time.Second) || progress.LastSessionUpdateAt != base.Add(3*time.Second) {
		t.Fatalf("attributable unknown notification must refresh transport/session")
	}
	// Monotonic clocks never move backwards.
	progress.Apply(ACPProgressEvent{Kind: ACPEventThoughtChunk}, base.Add(time.Second))
	if progress.LastTransportActivityAt != base.Add(3*time.Second) {
		t.Fatalf("monotonic transport clock moved backwards")
	}
}

func TestProgressApplyUsageGating(t *testing.T) {
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	progress.Apply(ACPProgressEvent{Kind: ACPEventPromptSent}, base)
	progress.Apply(ACPProgressEvent{Kind: ACPEventUsageUpdate, UsageIncreased: false}, base.Add(time.Second))
	if progress.LastMeaningfulProgressAt != base {
		t.Fatalf("non-increasing usage must not count as meaningful progress")
	}
	progress.Apply(ACPProgressEvent{Kind: ACPEventUsageUpdate, UsageIncreased: true}, base.Add(2*time.Second))
	if progress.LastMeaningfulProgressAt != base.Add(2*time.Second) {
		t.Fatalf("increasing usage must count as meaningful progress")
	}
	if progress.LastSessionUpdateAt != base.Add(2*time.Second) {
		t.Fatalf("usage update must refresh session activity")
	}
}

func TestProgressToolAccountingOverflow(t *testing.T) {
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	for index := 0; index < maxTrackedActiveTools+5; index++ {
		callID := "tool-" + strconv.Itoa(index)
		progress.Apply(ACPProgressEvent{Kind: ACPEventToolCall, Tool: &ACPToolProgress{CallID: callID, Kind: ACPToolKindExecute, Status: ACPToolStatusPending}}, base)
	}
	if progress.ActiveToolCount != maxTrackedActiveTools {
		t.Fatalf("active tool count = %d, want bounded %d", progress.ActiveToolCount, maxTrackedActiveTools)
	}
	if !progress.ToolOverflow {
		t.Fatal("tool overflow flag must be set")
	}
	if len(progress.toolStates) != maxTrackedActiveTools {
		t.Fatalf("tracked tool states = %d, want bounded %d", len(progress.toolStates), maxTrackedActiveTools)
	}
	// An untracked overflow terminal cannot decrement the tracked count.
	progress.Apply(ACPProgressEvent{Kind: ACPEventToolCallUpdate, Tool: &ACPToolProgress{CallID: "tool-20", Status: ACPToolStatusTerminal}}, base.Add(time.Second))
	if progress.ActiveToolCount != maxTrackedActiveTools {
		t.Fatalf("overflow terminal decremented tracked count to %d", progress.ActiveToolCount)
	}
	// Terminal tracked calls release their map entry for future tools.
	progress.Apply(ACPProgressEvent{Kind: ACPEventToolCallUpdate, Tool: &ACPToolProgress{CallID: "tool-0", Status: ACPToolStatusTerminal}}, base.Add(2*time.Second))
	progress.Apply(ACPProgressEvent{Kind: ACPEventToolCall, Tool: &ACPToolProgress{CallID: "tool-new", Kind: ACPToolKindRead, Status: ACPToolStatusPending}}, base.Add(3*time.Second))
	if progress.ActiveToolCount != maxTrackedActiveTools || len(progress.toolStates) != maxTrackedActiveTools {
		t.Fatalf("replacement tool count/states = %d/%d", progress.ActiveToolCount, len(progress.toolStates))
	}
	if err := progress.Validate(); err != nil {
		t.Fatalf("overflow projection must validate: %v", err)
	}
}

func TestProgressToolAccountingPerCallID(t *testing.T) {
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	tool := func(callID string, status ACPToolStatus) *ACPToolProgress {
		return &ACPToolProgress{CallID: callID, Kind: ACPToolKindExecute, Status: status}
	}
	// A repeated tool_call for the same call ID must not double-count.
	progress.Apply(ACPProgressEvent{Kind: ACPEventToolCall, Tool: tool("a", ACPToolStatusPending)}, base)
	progress.Apply(ACPProgressEvent{Kind: ACPEventToolCall, Tool: tool("a", ACPToolStatusPending)}, base.Add(time.Second))
	if progress.ActiveToolCount != 1 {
		t.Fatalf("duplicate tool_call count = %d, want 1", progress.ActiveToolCount)
	}
	// A second parallel tool counts independently.
	progress.Apply(ACPProgressEvent{Kind: ACPEventToolCall, Tool: tool("b", ACPToolStatusPending)}, base.Add(2*time.Second))
	if progress.ActiveToolCount != 2 {
		t.Fatalf("parallel tool count = %d, want 2", progress.ActiveToolCount)
	}
	// Both run in parallel; phases stay tool_running.
	progress.Apply(ACPProgressEvent{Kind: ACPEventToolCallUpdate, Tool: tool("a", ACPToolStatusRunning)}, base.Add(3*time.Second))
	progress.Apply(ACPProgressEvent{Kind: ACPEventToolCallUpdate, Tool: tool("b", ACPToolStatusRunning)}, base.Add(4*time.Second))
	if progress.Phase != ACPPhaseToolRunning || progress.ActiveToolCount != 2 {
		t.Fatalf("parallel running phase/count = %s/%d", progress.Phase, progress.ActiveToolCount)
	}
	// A duplicated terminal update for the same call ID must not decrement twice.
	progress.Apply(ACPProgressEvent{Kind: ACPEventToolCallUpdate, Tool: tool("a", ACPToolStatusTerminal)}, base.Add(5*time.Second))
	progress.Apply(ACPProgressEvent{Kind: ACPEventToolCallUpdate, Tool: tool("a", ACPToolStatusTerminal)}, base.Add(6*time.Second))
	if progress.ActiveToolCount != 1 {
		t.Fatalf("duplicate terminal count = %d, want 1", progress.ActiveToolCount)
	}
	// One tool still active: phase remains tool_running.
	if progress.Phase != ACPPhaseToolRunning {
		t.Fatalf("phase after one terminal = %s, want tool_running", progress.Phase)
	}
	// The last terminal returns to agent_processing.
	progress.Apply(ACPProgressEvent{Kind: ACPEventToolCallUpdate, Tool: tool("b", ACPToolStatusTerminal)}, base.Add(7*time.Second))
	if progress.ActiveToolCount != 0 || progress.Phase != ACPPhaseAgentProcessing {
		t.Fatalf("final terminal phase/count = %s/%d", progress.Phase, progress.ActiveToolCount)
	}
}

func TestProgressToolWireStatusMapping(t *testing.T) {
	for _, test := range []struct {
		wire string
		want ACPToolStatus
		ok   bool
	}{
		{"pending", ACPToolStatusPending, true},
		{"in_progress", ACPToolStatusRunning, true},
		{"completed", ACPToolStatusTerminal, true},
		{"failed", ACPToolStatusTerminal, true},
		{"running", "", false},
		{"terminal", "", false},
		{"", "", false},
		{"bogus", "", false},
	} {
		got, ok := ACPToolStatusFromWire(test.wire)
		if ok != test.ok || got != test.want {
			t.Fatalf("ACPToolStatusFromWire(%q) = %q/%v, want %q/%v", test.wire, got, ok, test.want, test.ok)
		}
	}
}

func TestProgressToolWireKindMapping(t *testing.T) {
	for _, test := range []struct {
		wire string
		want ACPToolKind
	}{
		{"read", ACPToolKindRead},
		{"execute", ACPToolKindExecute},
		{"other", ACPToolKindOther},
		{"future_extension", ACPToolKindOther},
	} {
		if got := ACPToolKindFromWire(test.wire); got != test.want {
			t.Fatalf("ACPToolKindFromWire(%q) = %q, want %q", test.wire, got, test.want)
		}
	}
}

func TestProgressProcessSignalsDoNotRefreshTransport(t *testing.T) {
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	// Host-generated signals must never move the transport clock.
	progress.Apply(ACPProgressEvent{Kind: ACPEventProcessStarted, PID: 42}, base.Add(time.Second))
	progress.Apply(ACPProgressEvent{Kind: ACPEventPromptSent}, base.Add(2*time.Second))
	progress.Apply(ACPProgressEvent{Kind: ACPEventProcessFailed, ErrorClass: "acp_process_exit"}, base.Add(3*time.Second))
	if !progress.LastTransportActivityAt.IsZero() {
		t.Fatalf("host-generated signals refreshed transport activity: %v", progress.LastTransportActivityAt)
	}
	if progress.Phase != ACPPhaseFailed {
		t.Fatalf("phase after process failure = %s, want failed", progress.Phase)
	}
	// A real inbound frame moves the clock; the failure time stays visible.
	progress.Apply(ACPProgressEvent{Kind: ACPEventThoughtChunk}, base.Add(4*time.Second))
	if progress.LastTransportActivityAt != base.Add(4*time.Second) {
		t.Fatalf("inbound frame transport = %v", progress.LastTransportActivityAt)
	}
}

func TestProgressProcessFailedDoesNotRegressTerminalPhases(t *testing.T) {
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	for _, stopReason := range []string{ACPStopReasonEndTurn, ACPStopReasonCancelled} {
		progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
		progress.Apply(ACPProgressEvent{Kind: ACPEventPromptResponse, StopReason: stopReason}, base)
		progress.Apply(ACPProgressEvent{Kind: ACPEventProcessFailed, ErrorClass: "acp_prompt_failed"}, base.Add(time.Second))
		want := ACPPhaseCompleted
		if stopReason == ACPStopReasonCancelled {
			want = ACPPhaseCancelled
		}
		if progress.Phase != want {
			t.Fatalf("process failure regressed %s to %s", want, progress.Phase)
		}
	}
}

func TestProgressStopReasonPhases(t *testing.T) {
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	progress.Apply(ACPProgressEvent{Kind: ACPEventPromptResponse, StopReason: ACPStopReasonCancelled}, base)
	if progress.Phase != ACPPhaseCancelled {
		t.Fatalf("cancelled stop reason phase = %s", progress.Phase)
	}
	progress = ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	progress.Apply(ACPProgressEvent{Kind: ACPEventPromptResponse, StopReason: ACPStopReasonMaxTokens}, base)
	if progress.Phase != ACPPhaseFailed {
		t.Fatalf("max_tokens stop reason phase = %s, want failed", progress.Phase)
	}
	progress = ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	progress.Apply(ACPProgressEvent{Kind: ACPEventProcessFailed, ErrorClass: "acp_process_exit"}, base)
	if progress.Phase != ACPPhaseFailed {
		t.Fatalf("process failure phase = %s, want failed", progress.Phase)
	}
}

func TestProgressValidateAllowlists(t *testing.T) {
	valid := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1, Phase: ACPPhaseToolRunning,
		LastEventKind: ACPEventToolCallUpdate, LastToolKind: ACPToolKindExecute,
		LastToolStatus: ACPToolStatusRunning, StopReason: ACPStopReasonEndTurn}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid projection rejected: %v", err)
	}
	invalid := []ExternalAgentJobProgress{
		{JobID: "", Phase: ACPPhaseStarting},
		{JobID: "job_1", Attempt: -1, Phase: ACPPhaseStarting},
		{JobID: "job_1", Phase: ACPProgressPhase("bogus")},
		{JobID: "job_1", Phase: ACPPhaseStarting, LastEventKind: ACPEventKind("bogus")},
		{JobID: "job_1", Phase: ACPPhaseStarting, ActiveToolCount: 99},
		{JobID: "job_1", Phase: ACPPhaseStarting, LastToolStatus: ACPToolStatus("bogus")},
		{JobID: "job_1", Phase: ACPPhaseStarting, LastToolKind: ACPToolKind("bogus")},
		{JobID: "job_1", Phase: ACPPhaseStarting, StopReason: "bogus"},
	}
	for index, projection := range invalid {
		if err := projection.Validate(); err == nil {
			t.Fatalf("invalid projection %d must be rejected", index)
		}
	}
}
