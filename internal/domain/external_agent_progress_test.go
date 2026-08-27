package domain

import (
	"strconv"
	"testing"
	"time"
)

func TestProgressApplyLifecyclePhases(t *testing.T) {
	now := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	apply := func(event ExternalAgentProgressEvent) {
		now = now.Add(time.Second)
		progress.Apply(event, now)
	}
	apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventProcessStarted, PID: 42})
	if progress.Phase != ExternalAgentPhaseStarting {
		t.Fatalf("phase after process start = %s, want starting", progress.Phase)
	}
	apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventInitializeResponse})
	apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventSessionNew})
	if progress.Phase != ExternalAgentPhaseSessionReady {
		t.Fatalf("phase after session/new = %s, want session_ready", progress.Phase)
	}
	apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventPromptSent})
	if progress.Phase != ExternalAgentPhaseAgentProcessing || progress.PromptStartedAt != now {
		t.Fatalf("phase after prompt sent = %s, prompt_started=%v", progress.Phase, progress.PromptStartedAt)
	}
	apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventPlan})
	if progress.Phase != ExternalAgentPhasePlanning {
		t.Fatalf("phase after plan = %s, want planning", progress.Phase)
	}
	apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCall, Tool: &ExternalAgentToolProgress{CallID: "tool-1", Kind: ExternalAgentToolKindExecute, Status: ExternalAgentToolStatusPending}})
	if progress.Phase != ExternalAgentPhaseToolPending || progress.ActiveToolCount != 1 {
		t.Fatalf("phase after tool_call = %s count=%d", progress.Phase, progress.ActiveToolCount)
	}
	apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventPermissionRequested, PermissionPending: true})
	if progress.Phase != ExternalAgentPhaseWaitingPermission || !progress.PendingPermission {
		t.Fatalf("phase after permission request = %s pending=%t", progress.Phase, progress.PendingPermission)
	}
	apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventPermissionResponded, PermissionPending: false})
	if progress.Phase != ExternalAgentPhaseAgentProcessing || progress.PendingPermission {
		t.Fatalf("phase after permission response = %s pending=%t", progress.Phase, progress.PendingPermission)
	}
	apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCallUpdate, Tool: &ExternalAgentToolProgress{CallID: "tool-1", Status: ExternalAgentToolStatusRunning}})
	if progress.Phase != ExternalAgentPhaseToolRunning {
		t.Fatalf("phase after tool running = %s", progress.Phase)
	}
	if progress.LastToolKind != ExternalAgentToolKindExecute {
		t.Fatalf("partial tool update replaced kind = %s", progress.LastToolKind)
	}
	apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventMessageChunk})
	if progress.Phase != ExternalAgentPhaseResponding {
		t.Fatalf("phase after message chunk = %s, want responding", progress.Phase)
	}
	apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCallUpdate, Tool: &ExternalAgentToolProgress{CallID: "tool-1", Status: ExternalAgentToolStatusTerminal}})
	if progress.Phase != ExternalAgentPhaseAgentProcessing || progress.ActiveToolCount != 0 {
		t.Fatalf("phase after tool terminal = %s count=%d, want agent_processing/0", progress.Phase, progress.ActiveToolCount)
	}
	apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventPromptResponse, StopReason: ExternalAgentStopReasonEndTurn})
	if progress.Phase != ExternalAgentPhaseCompleted || progress.StopReason != ExternalAgentStopReasonEndTurn {
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
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventPromptSent}, base.Add(time.Second))
	if !progress.LastTransportActivityAt.IsZero() {
		t.Fatalf("prompt_sent must not refresh transport activity")
	}
	// Session updates refresh transport and session clocks.
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventMessageChunk}, base.Add(2*time.Second))
	if progress.LastTransportActivityAt != base.Add(2*time.Second) || progress.LastSessionUpdateAt != base.Add(2*time.Second) {
		t.Fatalf("message chunk did not refresh transport/session clocks")
	}
	// Unknown notifications have already been attributed to the expected
	// session by the adapter, so they refresh both clocks.
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventUnknownNotification}, base.Add(3*time.Second))
	if progress.LastTransportActivityAt != base.Add(3*time.Second) || progress.LastSessionUpdateAt != base.Add(3*time.Second) {
		t.Fatalf("attributable unknown notification must refresh transport/session")
	}
	// Monotonic clocks never move backwards.
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventThoughtChunk}, base.Add(time.Second))
	if progress.LastTransportActivityAt != base.Add(3*time.Second) {
		t.Fatalf("monotonic transport clock moved backwards")
	}
}

func TestProgressApplyUsageGating(t *testing.T) {
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventPromptSent}, base)
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventUsageUpdate, UsageIncreased: false}, base.Add(time.Second))
	if progress.LastMeaningfulProgressAt != base {
		t.Fatalf("non-increasing usage must not count as meaningful progress")
	}
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventUsageUpdate, UsageIncreased: true}, base.Add(2*time.Second))
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
	for index := range maxTrackedActiveTools + 5 {
		callID := "tool-" + strconv.Itoa(index)
		progress.Apply(
			ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCall, Tool: &ExternalAgentToolProgress{CallID: callID, Kind: ExternalAgentToolKindExecute, Status: ExternalAgentToolStatusPending}},
			base,
		)
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
	progress.Apply(
		ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCallUpdate, Tool: &ExternalAgentToolProgress{CallID: "tool-20", Status: ExternalAgentToolStatusTerminal}},
		base.Add(time.Second),
	)
	if progress.ActiveToolCount != maxTrackedActiveTools {
		t.Fatalf("overflow terminal decremented tracked count to %d", progress.ActiveToolCount)
	}
	// Terminal tracked calls release their map entry for future tools.
	progress.Apply(
		ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCallUpdate, Tool: &ExternalAgentToolProgress{CallID: "tool-0", Status: ExternalAgentToolStatusTerminal}},
		base.Add(2*time.Second),
	)
	progress.Apply(
		ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCall, Tool: &ExternalAgentToolProgress{CallID: "tool-new", Kind: ExternalAgentToolKindRead, Status: ExternalAgentToolStatusPending}},
		base.Add(3*time.Second),
	)
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
	tool := func(callID string, status ExternalAgentToolStatus) *ExternalAgentToolProgress {
		return &ExternalAgentToolProgress{CallID: callID, Kind: ExternalAgentToolKindExecute, Status: status}
	}
	// A repeated tool_call for the same call ID must not double-count.
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCall, Tool: tool("a", ExternalAgentToolStatusPending)}, base)
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCall, Tool: tool("a", ExternalAgentToolStatusPending)}, base.Add(time.Second))
	if progress.ActiveToolCount != 1 {
		t.Fatalf("duplicate tool_call count = %d, want 1", progress.ActiveToolCount)
	}
	// A second parallel tool counts independently.
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCall, Tool: tool("b", ExternalAgentToolStatusPending)}, base.Add(2*time.Second))
	if progress.ActiveToolCount != 2 {
		t.Fatalf("parallel tool count = %d, want 2", progress.ActiveToolCount)
	}
	// Both run in parallel; phases stay tool_running.
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCallUpdate, Tool: tool("a", ExternalAgentToolStatusRunning)}, base.Add(3*time.Second))
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCallUpdate, Tool: tool("b", ExternalAgentToolStatusRunning)}, base.Add(4*time.Second))
	if progress.Phase != ExternalAgentPhaseToolRunning || progress.ActiveToolCount != 2 {
		t.Fatalf("parallel running phase/count = %s/%d", progress.Phase, progress.ActiveToolCount)
	}
	// A duplicated terminal update for the same call ID must not decrement twice.
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCallUpdate, Tool: tool("a", ExternalAgentToolStatusTerminal)}, base.Add(5*time.Second))
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCallUpdate, Tool: tool("a", ExternalAgentToolStatusTerminal)}, base.Add(6*time.Second))
	if progress.ActiveToolCount != 1 {
		t.Fatalf("duplicate terminal count = %d, want 1", progress.ActiveToolCount)
	}
	// One tool still active: phase remains tool_running.
	if progress.Phase != ExternalAgentPhaseToolRunning {
		t.Fatalf("phase after one terminal = %s, want tool_running", progress.Phase)
	}
	// The last terminal returns to agent_processing.
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventToolCallUpdate, Tool: tool("b", ExternalAgentToolStatusTerminal)}, base.Add(7*time.Second))
	if progress.ActiveToolCount != 0 || progress.Phase != ExternalAgentPhaseAgentProcessing {
		t.Fatalf("final terminal phase/count = %s/%d", progress.Phase, progress.ActiveToolCount)
	}
}

func TestProgressToolWireStatusMapping(t *testing.T) {
	for _, test := range []struct {
		wire string
		want ExternalAgentToolStatus
		ok   bool
	}{
		{"pending", ExternalAgentToolStatusPending, true},
		{"in_progress", ExternalAgentToolStatusRunning, true},
		{"completed", ExternalAgentToolStatusTerminal, true},
		{"failed", ExternalAgentToolStatusTerminal, true},
		{"running", "", false},
		{"terminal", "", false},
		{"", "", false},
		{"bogus", "", false},
	} {
		got, ok := ExternalAgentToolStatusFromWire(test.wire)
		if ok != test.ok || got != test.want {
			t.Fatalf("ExternalAgentToolStatusFromWire(%q) = %q/%v, want %q/%v", test.wire, got, ok, test.want, test.ok)
		}
	}
}

func TestProgressToolWireKindMapping(t *testing.T) {
	for _, test := range []struct {
		wire string
		want ExternalAgentToolKind
	}{
		{"read", ExternalAgentToolKindRead},
		{"execute", ExternalAgentToolKindExecute},
		{"other", ExternalAgentToolKindOther},
		{"future_extension", ExternalAgentToolKindOther},
	} {
		if got := ExternalAgentToolKindFromWire(test.wire); got != test.want {
			t.Fatalf("ExternalAgentToolKindFromWire(%q) = %q, want %q", test.wire, got, test.want)
		}
	}
}

func TestProgressProcessSignalsDoNotRefreshTransport(t *testing.T) {
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	// Host-generated signals must never move the transport clock.
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventProcessStarted, PID: 42}, base.Add(time.Second))
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventPromptSent}, base.Add(2*time.Second))
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventProcessFailed, ErrorClass: "acp_process_exit"}, base.Add(3*time.Second))
	if !progress.LastTransportActivityAt.IsZero() {
		t.Fatalf("host-generated signals refreshed transport activity: %v", progress.LastTransportActivityAt)
	}
	if progress.Phase != ExternalAgentPhaseFailed {
		t.Fatalf("phase after process failure = %s, want failed", progress.Phase)
	}
	// A real inbound frame moves the clock; the failure time stays visible.
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventThoughtChunk}, base.Add(4*time.Second))
	if progress.LastTransportActivityAt != base.Add(4*time.Second) {
		t.Fatalf("inbound frame transport = %v", progress.LastTransportActivityAt)
	}
}

func TestProgressProcessFailedDoesNotRegressTerminalPhases(t *testing.T) {
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	for _, stopReason := range []string{ExternalAgentStopReasonEndTurn, ExternalAgentStopReasonCancelled} {
		progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
		progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventPromptResponse, StopReason: stopReason}, base)
		progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventProcessFailed, ErrorClass: "acp_prompt_failed"}, base.Add(time.Second))
		want := ExternalAgentPhaseCompleted
		if stopReason == ExternalAgentStopReasonCancelled {
			want = ExternalAgentPhaseCancelled
		}
		if progress.Phase != want {
			t.Fatalf("process failure regressed %s to %s", want, progress.Phase)
		}
	}
}

func TestProgressProcessFailedRecordsErrorClassButNotAfterTerminal(t *testing.T) {
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventProcessFailed, ErrorClass: ExternalAgentErrorClassStdoutTooLarge}, base)
	if progress.ErrorClass != ExternalAgentErrorClassStdoutTooLarge {
		t.Fatalf("error class = %q, want %q", progress.ErrorClass, ExternalAgentErrorClassStdoutTooLarge)
	}

	// A process failure after a terminal prompt response is not authoritative:
	// its class must not overwrite a projection that already completed.
	completed := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	completed.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventPromptResponse, StopReason: ExternalAgentStopReasonEndTurn}, base)
	completed.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventProcessFailed, ErrorClass: ExternalAgentErrorClassProcessExit}, base.Add(time.Second))
	if completed.ErrorClass != "" {
		t.Fatalf("error class after a completed turn = %q, want empty", completed.ErrorClass)
	}
}

func TestProgressStopReasonPhases(t *testing.T) {
	base := time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC)
	progress := ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventPromptResponse, StopReason: ExternalAgentStopReasonCancelled}, base)
	if progress.Phase != ExternalAgentPhaseCancelled {
		t.Fatalf("cancelled stop reason phase = %s", progress.Phase)
	}
	progress = ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventPromptResponse, StopReason: ExternalAgentStopReasonMaxTokens}, base)
	if progress.Phase != ExternalAgentPhaseFailed {
		t.Fatalf("max_tokens stop reason phase = %s, want failed", progress.Phase)
	}
	progress = ExternalAgentJobProgress{JobID: "job_1", Attempt: 1}
	progress.Apply(ExternalAgentProgressEvent{Kind: ExternalAgentEventProcessFailed, ErrorClass: "acp_process_exit"}, base)
	if progress.Phase != ExternalAgentPhaseFailed {
		t.Fatalf("process failure phase = %s, want failed", progress.Phase)
	}
}

func TestProgressValidateAllowlists(t *testing.T) {
	valid := ExternalAgentJobProgress{
		JobID: "job_1", Attempt: 1, Phase: ExternalAgentPhaseToolRunning,
		LastEventKind: ExternalAgentEventToolCallUpdate, LastToolKind: ExternalAgentToolKindExecute,
		LastToolStatus: ExternalAgentToolStatusRunning, StopReason: ExternalAgentStopReasonEndTurn,
		ErrorClass: ExternalAgentErrorClassProviderFailed,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid projection rejected: %v", err)
	}
	invalid := []ExternalAgentJobProgress{
		{JobID: "", Phase: ExternalAgentPhaseStarting},
		{JobID: "job_1", Attempt: -1, Phase: ExternalAgentPhaseStarting},
		{JobID: "job_1", Phase: ExternalAgentProgressPhase("bogus")},
		{JobID: "job_1", Phase: ExternalAgentPhaseStarting, LastEventKind: ExternalAgentEventKind("bogus")},
		{JobID: "job_1", Phase: ExternalAgentPhaseStarting, ActiveToolCount: 99},
		{JobID: "job_1", Phase: ExternalAgentPhaseStarting, LastToolStatus: ExternalAgentToolStatus("bogus")},
		{JobID: "job_1", Phase: ExternalAgentPhaseStarting, LastToolKind: ExternalAgentToolKind("bogus")},
		{JobID: "job_1", Phase: ExternalAgentPhaseStarting, StopReason: "bogus"},
		{JobID: "job_1", Phase: ExternalAgentPhaseStarting, ErrorClass: ExternalAgentErrorClass("bogus")},
	}
	for index, projection := range invalid {
		if err := projection.Validate(); err == nil {
			t.Fatalf("invalid projection %d must be rejected", index)
		}
	}
}
