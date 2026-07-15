// Package cliprotocol defines the versioned newline-delimited JSON (NDJSON)
// "cli-v1" wire contract exchanged between the local-agent agent_cli provider
// adapter and a mapper (shim) process such as the OpenCode shim.
//
// This package is dependency-free within the project: it depends only on the
// Go standard library. It owns the request, event, result, and error schema
// plus their validation. It never performs process I/O.
package cliprotocol

// Protocol is the stable identifier carried by every cli-v1 message.
const Protocol = "local-agent.agent-cli"

// Version is the current cli-v1 protocol version. Backward-incompatible wire
// changes require a new version.
const Version = 1

// Request methods.
const (
	MethodDescribe = "describe"
	MethodValidate = "validate"
	MethodRun      = "run"
)

// Response types.
const (
	TypeDescription = "description"
	TypeValidated   = "validated"
	TypeActivity    = "activity"
	TypeResult      = "result"
	TypeError       = "error"
)

// Message roles.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Description capabilities and normalized activity values implemented by v1.
const (
	CapabilityText          = "text"
	ActivityKindTool        = "tool"
	ActivityStatusCompleted = "completed"
	ActivityStatusError     = "error"
)

// Approval modes carried on a profile.
const (
	ApprovalReject = "reject"
	ApprovalAuto   = "auto"
)

// Error codes. Slack only ever receives the configured public model error; the
// redacted local error may carry these codes and bounded diagnostics.
const (
	CodeInvalidRequest    = "invalid_request"
	CodeUnsupported       = "unsupported"
	CodeExecutableMissing = "executable_not_found"
	CodeProtocolError     = "protocol_error"
	CodeProcessFailed     = "process_failed"
	CodeTimeout           = "timeout"
	CodeNoResponse        = "no_response"
)

// FinishReasonStop is the only finish reason emitted in v1.
const FinishReasonStop = "stop"

// Profile is the portable CLI behavior selection sent to the shim. It is a
// subset of the declarative agent_cli profile: it never carries a project,
// path, or working directory.
type Profile struct {
	Model    string `json:"model"`
	Agent    string `json:"agent,omitempty"`
	Approval string `json:"approval,omitempty"`
	Variant  string `json:"variant,omitempty"`
}

// Message is one turn of the bounded outer conversation transcript.
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Project is one entry of the trusted, canonical workspace registry.
type Project struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Workspace is the complete trusted project registry plus the working
// directory the CLI should start in.
type Workspace struct {
	WorkingDirectory string    `json:"working_directory"`
	Projects         []Project `json:"projects"`
}

// Request is a host -> shim message. Exactly one method-specific terminal
// response (plus optional run activity) is expected in reply.
type Request struct {
	Protocol          string     `json:"protocol"`
	Version           int        `json:"version"`
	ID                string     `json:"id"`
	Method            string     `json:"method"`
	Profile           *Profile   `json:"profile,omitempty"`
	SystemInstruction string     `json:"system_instruction,omitempty"`
	Messages          []Message  `json:"messages,omitempty"`
	Workspace         *Workspace `json:"workspace,omitempty"`
}

// Usage is optional diagnostic-only token accounting in v1.
type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// Response is any shim -> host message. The Type discriminator selects which
// fields are meaningful. A single line only ever represents one type.
type Response struct {
	Protocol string `json:"protocol"`
	Version  int    `json:"version"`
	ID       string `json:"id"`
	Type     string `json:"type"`

	// description: Name is the shim/provider name. activity: Name is the
	// normalized native action name.
	Name         string   `json:"name,omitempty"`
	ShimVersion  string   `json:"shim_version,omitempty"`
	CLIVersion   string   `json:"cli_version,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`

	// activity
	Kind   string `json:"kind,omitempty"`
	Status string `json:"status,omitempty"`

	// result
	Text         string `json:"text,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`

	// error
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

// IsTerminal reports whether a response type ends a request exchange. Only
// activity is non-terminal.
func IsTerminal(responseType string) bool {
	switch responseType {
	case TypeDescription, TypeValidated, TypeResult, TypeError:
		return true
	default:
		return false
	}
}

// TerminalTypeForMethod returns the successful terminal response type expected
// for a given request method, and whether the method is known.
func TerminalTypeForMethod(method string) (string, bool) {
	switch method {
	case MethodDescribe:
		return TypeDescription, true
	case MethodValidate:
		return TypeValidated, true
	case MethodRun:
		return TypeResult, true
	default:
		return "", false
	}
}
