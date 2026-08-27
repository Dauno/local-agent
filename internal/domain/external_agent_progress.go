package domain

import (
	"errors"
	"time"
)

// ExternalAgentProgressPhase is the best host-visible description of the current prompt
// work. It is the latest safe phase, never a claim about provider internals.
type ExternalAgentProgressPhase string

const (
	ExternalAgentPhaseStarting          ExternalAgentProgressPhase = "starting"
	ExternalAgentPhaseSessionReady      ExternalAgentProgressPhase = "session_ready"
	ExternalAgentPhaseAgentProcessing   ExternalAgentProgressPhase = "agent_processing"
	ExternalAgentPhasePlanning          ExternalAgentProgressPhase = "planning"
	ExternalAgentPhaseToolPending       ExternalAgentProgressPhase = "tool_pending"
	ExternalAgentPhaseWaitingPermission ExternalAgentProgressPhase = "waiting_permission"
	ExternalAgentPhaseToolRunning       ExternalAgentProgressPhase = "tool_running"
	ExternalAgentPhaseResponding        ExternalAgentProgressPhase = "responding"
	ExternalAgentPhaseCompleted         ExternalAgentProgressPhase = "completed"
	ExternalAgentPhaseCancelled         ExternalAgentProgressPhase = "cancelled"
	ExternalAgentPhaseFailed            ExternalAgentProgressPhase = "failed"
)

// ValidExternalAgentProgressPhase reports whether phase belongs to the bounded allowlist.
func ValidExternalAgentProgressPhase(phase ExternalAgentProgressPhase) bool {
	switch phase {
	case ExternalAgentPhaseStarting, ExternalAgentPhaseSessionReady, ExternalAgentPhaseAgentProcessing,
		ExternalAgentPhasePlanning, ExternalAgentPhaseToolPending, ExternalAgentPhaseWaitingPermission,
		ExternalAgentPhaseToolRunning, ExternalAgentPhaseResponding, ExternalAgentPhaseCompleted,
		ExternalAgentPhaseCancelled, ExternalAgentPhaseFailed:
		return true
	default:
		return false
	}
}

// ExternalAgentErrorClass is the bounded, redaction-safe classification of a
// process_failed event: a closed set of causes, never the raw stderr text or
// provider error message, so it stays safe to persist and to surface to an
// operator or the model.
type ExternalAgentErrorClass string

const (
	ExternalAgentErrorClassLineTooLarge   ExternalAgentErrorClass = "protocol_line_too_large"
	ExternalAgentErrorClassStdoutTooLarge ExternalAgentErrorClass = "protocol_stdout_too_large"
	ExternalAgentErrorClassProviderFailed ExternalAgentErrorClass = "provider_reported_failure"
	ExternalAgentErrorClassProcessExit    ExternalAgentErrorClass = "process_exit"
	ExternalAgentErrorClassTimeout        ExternalAgentErrorClass = "timeout"
	ExternalAgentErrorClassNoResponse     ExternalAgentErrorClass = "no_response"
)

// ValidExternalAgentErrorClass reports whether class belongs to the bounded
// allowlist. An empty class is valid: no failure has been recorded yet.
func ValidExternalAgentErrorClass(class ExternalAgentErrorClass) bool {
	switch class {
	case "", ExternalAgentErrorClassLineTooLarge, ExternalAgentErrorClassStdoutTooLarge,
		ExternalAgentErrorClassProviderFailed, ExternalAgentErrorClassProcessExit,
		ExternalAgentErrorClassTimeout, ExternalAgentErrorClassNoResponse:
		return true
	default:
		return false
	}
}

// ExternalAgentProgressHealth is an orthogonal interpretation of recency and
// transport/process state. No health value changes job state.
type ExternalAgentProgressHealth string

const (
	ExternalAgentHealthActive          ExternalAgentProgressHealth = "active"
	ExternalAgentHealthQuiet           ExternalAgentProgressHealth = "quiet"
	ExternalAgentHealthPossiblyStalled ExternalAgentProgressHealth = "possibly_stalled"
	ExternalAgentHealthDisconnected    ExternalAgentProgressHealth = "disconnected"
	ExternalAgentHealthTerminal        ExternalAgentProgressHealth = "terminal"
)

// ExternalAgentToolStatus is the bounded internal status retained from tool updates.
type ExternalAgentToolStatus string

const (
	ExternalAgentToolStatusPending  ExternalAgentToolStatus = "pending"
	ExternalAgentToolStatusRunning  ExternalAgentToolStatus = "running"
	ExternalAgentToolStatusTerminal ExternalAgentToolStatus = "terminal"
)

func ValidExternalAgentToolStatus(status ExternalAgentToolStatus) bool {
	switch status {
	case ExternalAgentToolStatusPending, ExternalAgentToolStatusRunning, ExternalAgentToolStatusTerminal:
		return true
	default:
		return false
	}
}

// ExternalAgentToolStatusFromWire maps a provider status to the bounded internal
// status. Supported values are pending, in_progress, completed, and failed; unknown
// values are not retained (the frame still refreshes session activity).
func ExternalAgentToolStatusFromWire(value string) (ExternalAgentToolStatus, bool) {
	switch value {
	case "pending":
		return ExternalAgentToolStatusPending, true
	case "in_progress":
		return ExternalAgentToolStatusRunning, true
	case "completed", "failed":
		return ExternalAgentToolStatusTerminal, true
	default:
		return "", false
	}
}

// ExternalAgentToolKind is the bounded tool category retained from tool calls.
type ExternalAgentToolKind string

const (
	ExternalAgentToolKindRead    ExternalAgentToolKind = "read"
	ExternalAgentToolKindEdit    ExternalAgentToolKind = "edit"
	ExternalAgentToolKindDelete  ExternalAgentToolKind = "delete"
	ExternalAgentToolKindMove    ExternalAgentToolKind = "move"
	ExternalAgentToolKindSearch  ExternalAgentToolKind = "search"
	ExternalAgentToolKindExecute ExternalAgentToolKind = "execute"
	ExternalAgentToolKindThink   ExternalAgentToolKind = "think"
	ExternalAgentToolKindFetch   ExternalAgentToolKind = "fetch"
	ExternalAgentToolKindOther   ExternalAgentToolKind = "other"
)

func ValidExternalAgentToolKind(kind ExternalAgentToolKind) bool {
	switch kind {
	case ExternalAgentToolKindRead, ExternalAgentToolKindEdit, ExternalAgentToolKindDelete, ExternalAgentToolKindMove,
		ExternalAgentToolKindSearch, ExternalAgentToolKindExecute, ExternalAgentToolKindThink,
		ExternalAgentToolKindFetch, ExternalAgentToolKindOther:
		return true
	default:
		return false
	}
}

// ExternalAgentToolKindFromWire maps unknown and extension kinds to the bounded "other"
// category. An omitted kind is handled by the caller because tool_call defaults
// to other while a partial tool_call_update should preserve the previous kind.
func ExternalAgentToolKindFromWire(value string) ExternalAgentToolKind {
	kind := ExternalAgentToolKind(value)
	if ValidExternalAgentToolKind(kind) {
		return kind
	}
	return ExternalAgentToolKindOther
}

// ExternalAgentEventKind is the bounded content-free classification of an
// external-agent event. Raw content never becomes part of these values.
type ExternalAgentEventKind string

const (
	ExternalAgentEventProcessStarted      ExternalAgentEventKind = "process_started"
	ExternalAgentEventInitializeResponse  ExternalAgentEventKind = "initialize_response"
	ExternalAgentEventSessionNew          ExternalAgentEventKind = "session_new"
	ExternalAgentEventPromptSent          ExternalAgentEventKind = "prompt_sent"
	ExternalAgentEventPlan                ExternalAgentEventKind = "plan"
	ExternalAgentEventToolCall            ExternalAgentEventKind = "tool_call"
	ExternalAgentEventToolCallUpdate      ExternalAgentEventKind = "tool_call_update"
	ExternalAgentEventPermissionRequested ExternalAgentEventKind = "permission_requested"
	ExternalAgentEventPermissionResponded ExternalAgentEventKind = "permission_responded"
	ExternalAgentEventMessageChunk        ExternalAgentEventKind = "message_chunk"
	ExternalAgentEventThoughtChunk        ExternalAgentEventKind = "thought_chunk"
	ExternalAgentEventUsageUpdate         ExternalAgentEventKind = "usage_update"
	ExternalAgentEventConfigOptionUpdate  ExternalAgentEventKind = "config_option_update"
	ExternalAgentEventUnknownNotification ExternalAgentEventKind = "unknown_notification"
	ExternalAgentEventTransportActivity   ExternalAgentEventKind = "transport_activity"
	ExternalAgentEventPromptResponse      ExternalAgentEventKind = "prompt_response"
	ExternalAgentEventProcessFailed       ExternalAgentEventKind = "process_failed"
)

func ValidExternalAgentEventKind(kind ExternalAgentEventKind) bool {
	switch kind {
	case ExternalAgentEventProcessStarted, ExternalAgentEventInitializeResponse, ExternalAgentEventSessionNew,
		ExternalAgentEventPromptSent, ExternalAgentEventPlan, ExternalAgentEventToolCall, ExternalAgentEventToolCallUpdate,
		ExternalAgentEventPermissionRequested, ExternalAgentEventPermissionResponded,
		ExternalAgentEventMessageChunk, ExternalAgentEventThoughtChunk, ExternalAgentEventUsageUpdate,
		ExternalAgentEventConfigOptionUpdate, ExternalAgentEventUnknownNotification,
		ExternalAgentEventTransportActivity, ExternalAgentEventPromptResponse, ExternalAgentEventProcessFailed:
		return true
	default:
		return false
	}
}

// ExternalAgentToolProgress is the bounded tool identity retained from a tool update.
// Tool title, input, output, locations, and command data are excluded.
type ExternalAgentToolProgress struct {
	CallID string                  `json:"call_id"`
	Kind   ExternalAgentToolKind   `json:"kind"`
	Status ExternalAgentToolStatus `json:"status"`
}

// ExternalAgentProgressEvent is a content-free event emitted from the original
// job-owned stream. It never carries prompt text,
// tool arguments or output, thoughts, plans, paths, or raw frames.
type ExternalAgentProgressEvent struct {
	Kind ExternalAgentEventKind
	// Tool carries bounded identity for tool events; nil otherwise.
	Tool *ExternalAgentToolProgress
	// PermissionPending is the new pending state for permission transitions.
	PermissionPending bool
	// StopReason is the bounded terminal stop reason of a prompt response.
	StopReason string
	// ErrorClass is the bounded host-owned classification for process failure.
	ErrorClass ExternalAgentErrorClass
	// PID is present only for the process_started event and is never durable.
	PID int
	// UsageIncreased is set by the host recorder to gate repeated usage
	// updates: only increasing bounded counters count as meaningful progress.
	UsageIncreased bool
}

// transport reports whether the event reflects receipt of an inbound frame.
// Host-generated signals (prompt sent, permission response sent, process
// start, process failure) never refresh transport activity: a silent process
// that fails must not erase how long it was actually silent.
func (e ExternalAgentProgressEvent) transport() bool {
	switch e.Kind {
	case ExternalAgentEventPromptSent, ExternalAgentEventPermissionResponded,
		ExternalAgentEventProcessStarted, ExternalAgentEventProcessFailed:
		return false
	default:
		return true
	}
}

// session reports whether the event is attributable to the expected session.
func (e ExternalAgentProgressEvent) session() bool {
	switch e.Kind {
	case ExternalAgentEventPlan, ExternalAgentEventToolCall, ExternalAgentEventToolCallUpdate,
		ExternalAgentEventMessageChunk, ExternalAgentEventThoughtChunk, ExternalAgentEventUsageUpdate,
		ExternalAgentEventConfigOptionUpdate, ExternalAgentEventUnknownNotification:
		return true
	default:
		return false
	}
}

// ExternalAgentJobProgress is the durable content-free live projection of one
// external-agent job. It never contains provider payloads.
type ExternalAgentJobProgress struct {
	JobID                    string
	Attempt                  int
	Phase                    ExternalAgentProgressPhase
	LastEventKind            ExternalAgentEventKind
	LastTransportActivityAt  time.Time
	LastSessionUpdateAt      time.Time
	LastMeaningfulProgressAt time.Time
	PromptStartedAt          time.Time
	ActiveToolCount          int
	LastToolCallID           string
	LastToolKind             ExternalAgentToolKind
	LastToolStatus           ExternalAgentToolStatus
	ToolOverflow             bool
	PendingPermission        bool
	StopReason               string
	ErrorClass               ExternalAgentErrorClass
	CreatedAt                time.Time
	UpdatedAt                time.Time
	// toolStates is in-memory-only per-CallID accounting used by Apply while
	// live recording. It is never persisted, loaded, or validated; after a
	// restart the durable projection reports only the last known count.
	toolStates map[string]ExternalAgentToolStatus
}

const maxTrackedActiveTools = 16

// Validate enforces the bounded allowlists of the durable projection.
func (p ExternalAgentJobProgress) Validate() error {
	if p.JobID == "" {
		return errProgressInvalid("job ID is required")
	}
	if p.Attempt < 0 {
		return errProgressInvalid("attempt is negative")
	}
	if !ValidExternalAgentProgressPhase(p.Phase) {
		return errProgressInvalid("invalid progress phase")
	}
	if p.LastEventKind != "" && !ValidExternalAgentEventKind(p.LastEventKind) {
		return errProgressInvalid("invalid last event kind")
	}
	if p.ActiveToolCount < 0 || p.ActiveToolCount > maxTrackedActiveTools {
		return errProgressInvalid("active tool count is out of bounds")
	}
	if p.LastToolStatus != "" && !ValidExternalAgentToolStatus(p.LastToolStatus) {
		return errProgressInvalid("invalid last tool status")
	}
	if p.LastToolCallID != "" && len(p.LastToolCallID) > 256 {
		return errProgressInvalid("last tool call ID is out of bounds")
	}
	if p.LastToolKind != "" && !ValidExternalAgentToolKind(p.LastToolKind) {
		return errProgressInvalid("invalid last tool kind")
	}
	if p.StopReason != "" && !validStopReason(p.StopReason) {
		return errProgressInvalid("invalid stop reason")
	}
	if !ValidExternalAgentErrorClass(p.ErrorClass) {
		return errProgressInvalid("invalid error class")
	}
	return nil
}

func errProgressInvalid(message string) error {
	return &ExternalAgentError{Code: ExternalAgentErrorProgressInvalid, Err: errors.New(message)}
}

func validStopReason(reason string) bool {
	switch reason {
	case ExternalAgentStopReasonEndTurn, ExternalAgentStopReasonCancelled, ExternalAgentStopReasonMaxTokens, ExternalAgentStopReasonRefusal:
		return true
	default:
		return false
	}
}

// Apply folds one content-free event into the in-memory projection with
// monotonic activity clocks. Tool accounting is per-CallID: a repeated
// tool_call or a duplicated terminal update can never double-count or
// decrement another tool. Repeated chunks never produce durable writes per
// event; the host recorder owns write throttling.
func (p *ExternalAgentJobProgress) Apply(event ExternalAgentProgressEvent, now time.Time) {
	if p == nil {
		return
	}
	now = now.UTC()
	if event.Kind != "" {
		p.LastEventKind = event.Kind
	}
	if event.transport() {
		p.LastTransportActivityAt = maxTime(p.LastTransportActivityAt, now)
	}
	if event.session() {
		p.LastSessionUpdateAt = maxTime(p.LastSessionUpdateAt, now)
	}
	if p.eventMeaningful(event) {
		p.LastMeaningfulProgressAt = maxTime(p.LastMeaningfulProgressAt, now)
	}
	switch event.Kind {
	case ExternalAgentEventProcessStarted, ExternalAgentEventInitializeResponse:
		p.Phase = ExternalAgentPhaseStarting
	case ExternalAgentEventSessionNew:
		p.Phase = ExternalAgentPhaseSessionReady
	case ExternalAgentEventPromptSent:
		p.Phase = ExternalAgentPhaseAgentProcessing
		if p.PromptStartedAt.IsZero() {
			p.PromptStartedAt = now
		}
	case ExternalAgentEventPlan:
		p.Phase = ExternalAgentPhasePlanning
	case ExternalAgentEventToolCall:
		p.applyToolEvent(event.Tool)
		p.Phase = p.toolPhase(event.Tool)
	case ExternalAgentEventToolCallUpdate:
		p.applyToolEvent(event.Tool)
		if event.Tool != nil {
			switch event.Tool.Status {
			case ExternalAgentToolStatusRunning:
				p.Phase = ExternalAgentPhaseToolRunning
			case ExternalAgentToolStatusTerminal:
				if p.ActiveToolCount == 0 {
					p.Phase = ExternalAgentPhaseAgentProcessing
				}
			}
		}
	case ExternalAgentEventPermissionRequested:
		p.PendingPermission = true
		p.Phase = ExternalAgentPhaseWaitingPermission
	case ExternalAgentEventPermissionResponded:
		p.PendingPermission = false
		p.Phase = ExternalAgentPhaseAgentProcessing
	case ExternalAgentEventMessageChunk:
		p.Phase = ExternalAgentPhaseResponding
	case ExternalAgentEventPromptResponse:
		switch event.StopReason {
		case ExternalAgentStopReasonEndTurn:
			p.Phase = ExternalAgentPhaseCompleted
		case ExternalAgentStopReasonCancelled:
			p.Phase = ExternalAgentPhaseCancelled
		default:
			p.Phase = ExternalAgentPhaseFailed
		}
		p.StopReason = event.StopReason
	case ExternalAgentEventProcessFailed:
		// A terminal prompt response is authoritative: a later process
		// failure signal must never regress completed or cancelled, and the
		// class it carries only describes a failure that is actually
		// recorded.
		if p.Phase != ExternalAgentPhaseCompleted && p.Phase != ExternalAgentPhaseCancelled {
			p.Phase = ExternalAgentPhaseFailed
			if event.ErrorClass != "" {
				p.ErrorClass = event.ErrorClass
			}
		}
	}
}

// applyToolEvent maintains bounded per-CallID accounting for active tools.
// Terminal entries are removed, and overflow IDs are never inserted, so
// cumulative tool traffic cannot grow memory or decrement a count it did not
// increment. ToolOverflow means the capped count may be incomplete.
func (p *ExternalAgentJobProgress) applyToolEvent(tool *ExternalAgentToolProgress) {
	if tool == nil || tool.CallID == "" {
		return
	}
	p.LastToolCallID = tool.CallID
	if tool.Kind != "" {
		p.LastToolKind = tool.Kind
	}
	p.LastToolStatus = tool.Status
	if p.toolStates == nil {
		p.toolStates = make(map[string]ExternalAgentToolStatus)
	}
	_, seen := p.toolStates[tool.CallID]
	switch tool.Status {
	case ExternalAgentToolStatusPending, ExternalAgentToolStatusRunning:
		if !seen {
			if len(p.toolStates) >= maxTrackedActiveTools {
				p.ToolOverflow = true
				return
			}
		}
		p.toolStates[tool.CallID] = tool.Status
	case ExternalAgentToolStatusTerminal:
		if seen {
			delete(p.toolStates, tool.CallID)
		}
	}
	p.ActiveToolCount = len(p.toolStates)
}

func (p *ExternalAgentJobProgress) toolPhase(tool *ExternalAgentToolProgress) ExternalAgentProgressPhase {
	if tool == nil {
		return p.Phase
	}
	switch tool.Status {
	case ExternalAgentToolStatusPending:
		return ExternalAgentPhaseToolPending
	case ExternalAgentToolStatusRunning:
		return ExternalAgentPhaseToolRunning
	case ExternalAgentToolStatusTerminal:
		if p.ActiveToolCount == 0 {
			return ExternalAgentPhaseAgentProcessing
		}
		return ExternalAgentPhaseToolRunning
	default:
		return p.Phase
	}
}

// eventMeaningful reports whether the event indicates observable task
// advancement. Tool updates are meaningful only on a status transition of the
// specific call ID, never on the last global status.
func (p *ExternalAgentJobProgress) eventMeaningful(event ExternalAgentProgressEvent) bool {
	switch event.Kind {
	case ExternalAgentEventProcessStarted, ExternalAgentEventInitializeResponse, ExternalAgentEventSessionNew,
		ExternalAgentEventPromptSent, ExternalAgentEventPermissionRequested,
		ExternalAgentEventPermissionResponded, ExternalAgentEventMessageChunk, ExternalAgentEventThoughtChunk,
		ExternalAgentEventPromptResponse, ExternalAgentEventProcessFailed:
		return true
	case ExternalAgentEventToolCall:
		if event.Tool == nil {
			return true
		}
		previous, seen := p.toolStates[event.Tool.CallID]
		return !seen || previous == ExternalAgentToolStatusTerminal
	case ExternalAgentEventToolCallUpdate:
		if event.Tool == nil {
			return false
		}
		previous, seen := p.toolStates[event.Tool.CallID]
		if !seen {
			return true
		}
		return previous != event.Tool.Status
	case ExternalAgentEventUsageUpdate:
		return event.UsageIncreased
	default:
		return false
	}
}

func maxTime(left, right time.Time) time.Time {
	if left.After(right) {
		return left
	}
	return right
}
