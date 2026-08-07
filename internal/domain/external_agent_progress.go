package domain

import (
	"errors"
	"time"
)

// ACPProgressPhase is the best ACP-visible description of the current prompt
// work. It is the latest safe phase, never a claim about provider internals.
type ACPProgressPhase string

const (
	ACPPhaseStarting          ACPProgressPhase = "starting"
	ACPPhaseSessionReady      ACPProgressPhase = "session_ready"
	ACPPhaseAgentProcessing   ACPProgressPhase = "agent_processing"
	ACPPhasePlanning          ACPProgressPhase = "planning"
	ACPPhaseToolPending       ACPProgressPhase = "tool_pending"
	ACPPhaseWaitingPermission ACPProgressPhase = "waiting_permission"
	ACPPhaseToolRunning       ACPProgressPhase = "tool_running"
	ACPPhaseResponding        ACPProgressPhase = "responding"
	ACPPhaseCompleted         ACPProgressPhase = "completed"
	ACPPhaseCancelled         ACPProgressPhase = "cancelled"
	ACPPhaseFailed            ACPProgressPhase = "failed"
)

// ValidACPProgressPhase reports whether phase belongs to the bounded allowlist.
func ValidACPProgressPhase(phase ACPProgressPhase) bool {
	switch phase {
	case ACPPhaseStarting, ACPPhaseSessionReady, ACPPhaseAgentProcessing,
		ACPPhasePlanning, ACPPhaseToolPending, ACPPhaseWaitingPermission,
		ACPPhaseToolRunning, ACPPhaseResponding, ACPPhaseCompleted,
		ACPPhaseCancelled, ACPPhaseFailed:
		return true
	default:
		return false
	}
}

// ACPProgressHealth is an orthogonal interpretation of recency and
// transport/process state. No health value changes job state.
type ACPProgressHealth string

const (
	ACPHealthActive          ACPProgressHealth = "active"
	ACPHealthQuiet           ACPProgressHealth = "quiet"
	ACPHealthPossiblyStalled ACPProgressHealth = "possibly_stalled"
	ACPHealthDisconnected    ACPProgressHealth = "disconnected"
	ACPHealthTerminal        ACPProgressHealth = "terminal"
)

func ValidACPProgressHealth(health ACPProgressHealth) bool {
	switch health {
	case ACPHealthActive, ACPHealthQuiet, ACPHealthPossiblyStalled,
		ACPHealthDisconnected, ACPHealthTerminal:
		return true
	default:
		return false
	}
}

// ACPToolStatus is the bounded internal tool status retained from ACP updates.
type ACPToolStatus string

const (
	ACPToolStatusPending  ACPToolStatus = "pending"
	ACPToolStatusRunning  ACPToolStatus = "running"
	ACPToolStatusTerminal ACPToolStatus = "terminal"
)

func ValidACPToolStatus(status ACPToolStatus) bool {
	switch status {
	case ACPToolStatusPending, ACPToolStatusRunning, ACPToolStatusTerminal:
		return true
	default:
		return false
	}
}

// ACPToolStatusFromWire maps an ACP v1 wire status to the bounded internal
// status. ACP v1 uses pending, in_progress, completed, and failed; unknown
// values are not retained (the frame still refreshes session activity).
func ACPToolStatusFromWire(value string) (ACPToolStatus, bool) {
	switch value {
	case "pending":
		return ACPToolStatusPending, true
	case "in_progress":
		return ACPToolStatusRunning, true
	case "completed", "failed":
		return ACPToolStatusTerminal, true
	default:
		return "", false
	}
}

// ACPToolKind is the bounded ACP v1 tool category retained from tool calls.
type ACPToolKind string

const (
	ACPToolKindRead    ACPToolKind = "read"
	ACPToolKindEdit    ACPToolKind = "edit"
	ACPToolKindDelete  ACPToolKind = "delete"
	ACPToolKindMove    ACPToolKind = "move"
	ACPToolKindSearch  ACPToolKind = "search"
	ACPToolKindExecute ACPToolKind = "execute"
	ACPToolKindThink   ACPToolKind = "think"
	ACPToolKindFetch   ACPToolKind = "fetch"
	ACPToolKindOther   ACPToolKind = "other"
)

func ValidACPToolKind(kind ACPToolKind) bool {
	switch kind {
	case ACPToolKindRead, ACPToolKindEdit, ACPToolKindDelete, ACPToolKindMove,
		ACPToolKindSearch, ACPToolKindExecute, ACPToolKindThink,
		ACPToolKindFetch, ACPToolKindOther:
		return true
	default:
		return false
	}
}

// ACPToolKindFromWire maps unknown and extension kinds to the bounded "other"
// category. An omitted kind is handled by the caller because tool_call defaults
// to other while a partial tool_call_update should preserve the previous kind.
func ACPToolKindFromWire(value string) ACPToolKind {
	kind := ACPToolKind(value)
	if ValidACPToolKind(kind) {
		return kind
	}
	return ACPToolKindOther
}

// ACPEventKind is the bounded content-free classification of an inbound ACP
// protocol event. Raw frame content never becomes part of these values.
type ACPEventKind string

const (
	ACPEventProcessStarted      ACPEventKind = "process_started"
	ACPEventInitializeResponse  ACPEventKind = "initialize_response"
	ACPEventSessionNew          ACPEventKind = "session_new"
	ACPEventPromptSent          ACPEventKind = "prompt_sent"
	ACPEventPlan                ACPEventKind = "plan"
	ACPEventToolCall            ACPEventKind = "tool_call"
	ACPEventToolCallUpdate      ACPEventKind = "tool_call_update"
	ACPEventPermissionRequested ACPEventKind = "permission_requested"
	ACPEventPermissionResponded ACPEventKind = "permission_responded"
	ACPEventMessageChunk        ACPEventKind = "message_chunk"
	ACPEventThoughtChunk        ACPEventKind = "thought_chunk"
	ACPEventUsageUpdate         ACPEventKind = "usage_update"
	ACPEventConfigOptionUpdate  ACPEventKind = "config_option_update"
	ACPEventUnknownNotification ACPEventKind = "unknown_notification"
	ACPEventTransportActivity   ACPEventKind = "transport_activity"
	ACPEventPromptResponse      ACPEventKind = "prompt_response"
	ACPEventProcessFailed       ACPEventKind = "process_failed"
)

func ValidACPEventKind(kind ACPEventKind) bool {
	switch kind {
	case ACPEventProcessStarted, ACPEventInitializeResponse, ACPEventSessionNew,
		ACPEventPromptSent, ACPEventPlan, ACPEventToolCall, ACPEventToolCallUpdate,
		ACPEventPermissionRequested, ACPEventPermissionResponded,
		ACPEventMessageChunk, ACPEventThoughtChunk, ACPEventUsageUpdate,
		ACPEventConfigOptionUpdate, ACPEventUnknownNotification,
		ACPEventTransportActivity, ACPEventPromptResponse, ACPEventProcessFailed:
		return true
	default:
		return false
	}
}

// ACPToolProgress is the bounded tool identity retained from a tool update.
// Tool title, input, output, locations, and command data are excluded.
type ACPToolProgress struct {
	CallID string        `json:"call_id"`
	Kind   ACPToolKind   `json:"kind"`
	Status ACPToolStatus `json:"status"`
}

func (t ACPToolProgress) Validate() bool {
	if t.CallID == "" || len(t.CallID) > 256 {
		return false
	}
	for _, r := range t.CallID {
		if r < ' ' || r == '\x7f' {
			return false
		}
	}
	// Kind may be omitted by a partial tool_call_update. The projection then
	// preserves the last known kind for this call.
	if t.Kind != "" && !ValidACPToolKind(t.Kind) {
		return false
	}
	return ValidACPToolStatus(t.Status)
}

// ACPProgressEvent is a content-free protocol event emitted by the ACP
// adapter from the original job-owned stream. It never carries prompt text,
// tool arguments or output, thoughts, plans, paths, or raw frames.
type ACPProgressEvent struct {
	Kind ACPEventKind
	// Tool carries bounded identity for tool events; nil otherwise.
	Tool *ACPToolProgress
	// PermissionPending is the new pending state for permission transitions.
	PermissionPending bool
	// StopReason is the bounded terminal stop reason of a prompt response.
	StopReason string
	// ErrorClass is the bounded host-owned classification for process failure.
	ErrorClass string
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
func (e ACPProgressEvent) transport() bool {
	switch e.Kind {
	case ACPEventPromptSent, ACPEventPermissionResponded,
		ACPEventProcessStarted, ACPEventProcessFailed:
		return false
	default:
		return true
	}
}

// session reports whether the event is attributable to the expected session.
func (e ACPProgressEvent) session() bool {
	switch e.Kind {
	case ACPEventPlan, ACPEventToolCall, ACPEventToolCallUpdate,
		ACPEventMessageChunk, ACPEventThoughtChunk, ACPEventUsageUpdate,
		ACPEventConfigOptionUpdate, ACPEventUnknownNotification:
		return true
	default:
		return false
	}
}

// ExternalAgentJobProgress is the durable content-free live projection of one
// ACP job. It never contains provider payloads.
type ExternalAgentJobProgress struct {
	JobID                    string
	Attempt                  int
	Phase                    ACPProgressPhase
	LastEventKind            ACPEventKind
	LastTransportActivityAt  time.Time
	LastSessionUpdateAt      time.Time
	LastMeaningfulProgressAt time.Time
	PromptStartedAt          time.Time
	ActiveToolCount          int
	LastToolCallID           string
	LastToolKind             ACPToolKind
	LastToolStatus           ACPToolStatus
	ToolOverflow             bool
	PendingPermission        bool
	StopReason               string
	CreatedAt                time.Time
	UpdatedAt                time.Time
	// toolStates is in-memory-only per-CallID accounting used by Apply while
	// live recording. It is never persisted, loaded, or validated; after a
	// restart the durable projection reports only the last known count.
	toolStates map[string]ACPToolStatus
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
	if !ValidACPProgressPhase(p.Phase) {
		return errProgressInvalid("invalid progress phase")
	}
	if p.LastEventKind != "" && !ValidACPEventKind(p.LastEventKind) {
		return errProgressInvalid("invalid last event kind")
	}
	if p.ActiveToolCount < 0 || p.ActiveToolCount > maxTrackedActiveTools {
		return errProgressInvalid("active tool count is out of bounds")
	}
	if p.LastToolStatus != "" && !ValidACPToolStatus(p.LastToolStatus) {
		return errProgressInvalid("invalid last tool status")
	}
	if p.LastToolCallID != "" && len(p.LastToolCallID) > 256 {
		return errProgressInvalid("last tool call ID is out of bounds")
	}
	if p.LastToolKind != "" && !ValidACPToolKind(p.LastToolKind) {
		return errProgressInvalid("invalid last tool kind")
	}
	if p.StopReason != "" && !validStopReason(p.StopReason) {
		return errProgressInvalid("invalid stop reason")
	}
	return nil
}

func errProgressInvalid(message string) error {
	return &ACPError{Code: ACPErrorProgressInvalid, Err: errors.New(message)}
}

func validStopReason(reason string) bool {
	switch reason {
	case ACPStopReasonEndTurn, ACPStopReasonCancelled, ACPStopReasonMaxTokens, ACPStopReasonRefusal:
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
func (p *ExternalAgentJobProgress) Apply(event ACPProgressEvent, now time.Time) {
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
	case ACPEventProcessStarted, ACPEventInitializeResponse:
		p.Phase = ACPPhaseStarting
	case ACPEventSessionNew:
		p.Phase = ACPPhaseSessionReady
	case ACPEventPromptSent:
		p.Phase = ACPPhaseAgentProcessing
		if p.PromptStartedAt.IsZero() {
			p.PromptStartedAt = now
		}
	case ACPEventPlan:
		p.Phase = ACPPhasePlanning
	case ACPEventToolCall:
		p.applyToolEvent(event.Tool)
		p.Phase = p.toolPhase(event.Tool)
	case ACPEventToolCallUpdate:
		p.applyToolEvent(event.Tool)
		if event.Tool != nil {
			switch event.Tool.Status {
			case ACPToolStatusRunning:
				p.Phase = ACPPhaseToolRunning
			case ACPToolStatusTerminal:
				if p.ActiveToolCount == 0 {
					p.Phase = ACPPhaseAgentProcessing
				}
			}
		}
	case ACPEventPermissionRequested:
		p.PendingPermission = true
		p.Phase = ACPPhaseWaitingPermission
	case ACPEventPermissionResponded:
		p.PendingPermission = false
		p.Phase = ACPPhaseAgentProcessing
	case ACPEventMessageChunk:
		p.Phase = ACPPhaseResponding
	case ACPEventPromptResponse:
		switch event.StopReason {
		case ACPStopReasonEndTurn:
			p.Phase = ACPPhaseCompleted
		case ACPStopReasonCancelled:
			p.Phase = ACPPhaseCancelled
		default:
			p.Phase = ACPPhaseFailed
		}
		p.StopReason = event.StopReason
	case ACPEventProcessFailed:
		// A terminal prompt response is authoritative: a later process
		// failure signal must never regress completed or cancelled.
		if p.Phase != ACPPhaseCompleted && p.Phase != ACPPhaseCancelled {
			p.Phase = ACPPhaseFailed
		}
	}
}

// applyToolEvent maintains bounded per-CallID accounting for active tools.
// Terminal entries are removed, and overflow IDs are never inserted, so
// cumulative tool traffic cannot grow memory or decrement a count it did not
// increment. ToolOverflow means the capped count may be incomplete.
func (p *ExternalAgentJobProgress) applyToolEvent(tool *ACPToolProgress) {
	if tool == nil || tool.CallID == "" {
		return
	}
	p.LastToolCallID = tool.CallID
	if tool.Kind != "" {
		p.LastToolKind = tool.Kind
	}
	p.LastToolStatus = tool.Status
	if p.toolStates == nil {
		p.toolStates = make(map[string]ACPToolStatus)
	}
	_, seen := p.toolStates[tool.CallID]
	switch tool.Status {
	case ACPToolStatusPending, ACPToolStatusRunning:
		if !seen {
			if len(p.toolStates) >= maxTrackedActiveTools {
				p.ToolOverflow = true
				return
			}
		}
		p.toolStates[tool.CallID] = tool.Status
	case ACPToolStatusTerminal:
		if seen {
			delete(p.toolStates, tool.CallID)
		}
	}
	p.ActiveToolCount = len(p.toolStates)
}

func (p *ExternalAgentJobProgress) toolPhase(tool *ACPToolProgress) ACPProgressPhase {
	if tool == nil {
		return p.Phase
	}
	switch tool.Status {
	case ACPToolStatusPending:
		return ACPPhaseToolPending
	case ACPToolStatusRunning:
		return ACPPhaseToolRunning
	case ACPToolStatusTerminal:
		if p.ActiveToolCount == 0 {
			return ACPPhaseAgentProcessing
		}
		return ACPPhaseToolRunning
	default:
		return p.Phase
	}
}
// eventMeaningful reports whether the event indicates observable task
// advancement. Tool updates are meaningful only on a status transition of the
// specific call ID, never on the last global status.
func (p *ExternalAgentJobProgress) eventMeaningful(event ACPProgressEvent) bool {
	switch event.Kind {
	case ACPEventProcessStarted, ACPEventInitializeResponse, ACPEventSessionNew,
		ACPEventPromptSent, ACPEventPermissionRequested,
		ACPEventPermissionResponded, ACPEventMessageChunk, ACPEventThoughtChunk,
		ACPEventPromptResponse, ACPEventProcessFailed:
		return true
	case ACPEventToolCall:
		if event.Tool == nil {
			return true
		}
		previous, seen := p.toolStates[event.Tool.CallID]
		return !seen || previous == ACPToolStatusTerminal
	case ACPEventToolCallUpdate:
		if event.Tool == nil {
			return false
		}
		previous, seen := p.toolStates[event.Tool.CallID]
		if !seen {
			return true
		}
		return previous != event.Tool.Status
	case ACPEventUsageUpdate:
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
