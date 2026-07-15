package cliprotocol

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// NewRequest builds a request envelope with the protocol and version set.
func NewRequest(id, method string) Request {
	return Request{Protocol: Protocol, Version: Version, ID: id, Method: method}
}

// response builders -----------------------------------------------------------

func newResponse(id, responseType string) Response {
	return Response{Protocol: Protocol, Version: Version, ID: id, Type: responseType}
}

// NewDescription builds a successful describe terminal response.
func NewDescription(id, name, shimVersion, cliVersion string, capabilities []string) Response {
	resp := newResponse(id, TypeDescription)
	resp.Name = name
	resp.ShimVersion = shimVersion
	resp.CLIVersion = cliVersion
	resp.Capabilities = capabilities
	return resp
}

// NewValidated builds a successful validate terminal response.
func NewValidated(id string) Response {
	return newResponse(id, TypeValidated)
}

// NewActivity builds a non-terminal run activity event.
func NewActivity(id, kind, name, status string) Response {
	resp := newResponse(id, TypeActivity)
	resp.Kind = kind
	resp.Name = name
	resp.Status = status
	return resp
}

// NewResult builds a successful run terminal response.
func NewResult(id, text string) Response {
	resp := newResponse(id, TypeResult)
	resp.Text = text
	resp.FinishReason = FinishReasonStop
	return resp
}

// NewError builds a terminal error response.
func NewError(id, code, message string, retryable bool) Response {
	resp := newResponse(id, TypeError)
	resp.Code = code
	resp.Message = message
	resp.Retryable = retryable
	return resp
}

// encode / decode -------------------------------------------------------------

// EncodeLine marshals a value to one NDJSON line terminated by '\n'.
func EncodeLine(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode cli-v1 message: %w", err)
	}
	encoded = append(encoded, '\n')
	return encoded, nil
}

// DecodeRequest unmarshals and structurally validates one request line.
func DecodeRequest(line []byte) (Request, error) {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return Request{}, fmt.Errorf("decode cli-v1 request: %w", err)
	}
	if err := ValidateRequest(req); err != nil {
		return Request{}, err
	}
	return req, nil
}

// DecodeResponse unmarshals and validates the envelope of one response line.
// It does not enforce id matching; the caller compares ids.
func DecodeResponse(line []byte) (Response, error) {
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, fmt.Errorf("decode cli-v1 response: %w", err)
	}
	if err := ValidateResponse(resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}

// validation ------------------------------------------------------------------

func validateEnvelope(protocol string, version int, id string) error {
	if protocol != Protocol {
		return fmt.Errorf("cli-v1: unexpected protocol %q", protocol)
	}
	if version != Version {
		return fmt.Errorf("cli-v1: unsupported version %d (expected %d)", version, Version)
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("cli-v1: id must not be empty")
	}
	return nil
}

// ValidateRequest checks envelope and method-specific request fields.
func ValidateRequest(req Request) error {
	if err := validateEnvelope(req.Protocol, req.Version, req.ID); err != nil {
		return err
	}
	switch req.Method {
	case MethodDescribe:
		return nil
	case MethodValidate:
		if req.Profile == nil {
			return fmt.Errorf("cli-v1 validate: profile is required")
		}
		if err := validateProfile(*req.Profile); err != nil {
			return err
		}
		if req.Workspace == nil {
			return fmt.Errorf("cli-v1 validate: workspace is required")
		}
		return validateWorkspace(*req.Workspace)
	case MethodRun:
		if req.Profile == nil {
			return fmt.Errorf("cli-v1 run: profile is required")
		}
		if err := validateProfile(*req.Profile); err != nil {
			return err
		}
		if err := validateMessages(req.Messages); err != nil {
			return err
		}
		if req.Workspace == nil {
			return fmt.Errorf("cli-v1 run: workspace is required")
		}
		return validateWorkspace(*req.Workspace)
	default:
		return fmt.Errorf("cli-v1: unknown method %q", req.Method)
	}
}

func validateProfile(profile Profile) error {
	if strings.TrimSpace(profile.Model) == "" {
		return fmt.Errorf("cli-v1: profile model must not be empty")
	}
	switch profile.Approval {
	case "", ApprovalReject, ApprovalAuto:
	default:
		return fmt.Errorf("cli-v1: profile approval must be %q or %q", ApprovalReject, ApprovalAuto)
	}
	return nil
}

func validateMessages(messages []Message) error {
	if len(messages) == 0 {
		return fmt.Errorf("cli-v1 run: messages must not be empty")
	}
	for index, message := range messages {
		switch message.Role {
		case RoleUser, RoleAssistant:
		default:
			return fmt.Errorf("cli-v1 run: message %d has unsupported role %q", index, message.Role)
		}
		if strings.TrimSpace(message.Text) == "" {
			return fmt.Errorf("cli-v1 run: message %d text must not be empty", index)
		}
	}
	if messages[len(messages)-1].Role != RoleUser {
		return fmt.Errorf("cli-v1 run: final message must have user role")
	}
	return nil
}

func validateWorkspace(workspace Workspace) error {
	if strings.TrimSpace(workspace.WorkingDirectory) == "" {
		return fmt.Errorf("cli-v1: workspace working_directory must not be empty")
	}
	if !isCanonicalAbs(workspace.WorkingDirectory) {
		return fmt.Errorf("cli-v1: workspace working_directory must be a clean absolute path")
	}
	if len(workspace.Projects) == 0 {
		return fmt.Errorf("cli-v1: workspace must contain at least one project")
	}
	seen := make(map[string]struct{}, len(workspace.Projects))
	workingDirectoryRegistered := false
	for index, project := range workspace.Projects {
		if strings.TrimSpace(project.Name) == "" {
			return fmt.Errorf("cli-v1: project %d name must not be empty", index)
		}
		if _, exists := seen[project.Name]; exists {
			return fmt.Errorf("cli-v1: duplicate project name %q", project.Name)
		}
		seen[project.Name] = struct{}{}
		if !isCanonicalAbs(project.Path) {
			return fmt.Errorf("cli-v1: project %q path must be a clean absolute path", project.Name)
		}
		if project.Path == workspace.WorkingDirectory {
			workingDirectoryRegistered = true
		}
	}
	if !workingDirectoryRegistered {
		return fmt.Errorf("cli-v1: working_directory must equal one project path")
	}
	return nil
}

func isCanonicalAbs(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	return filepath.Clean(path) == path
}

// ValidateResponse checks envelope and type-specific response fields.
func ValidateResponse(resp Response) error {
	if err := validateEnvelope(resp.Protocol, resp.Version, resp.ID); err != nil {
		return err
	}
	switch resp.Type {
	case TypeDescription:
		if !isSafeDiagnosticToken(resp.Name) {
			return fmt.Errorf("cli-v1 description: name must be a bounded diagnostic token")
		}
		if !isSafeDiagnosticToken(resp.ShimVersion) {
			return fmt.Errorf("cli-v1 description: shim_version must be a bounded diagnostic token")
		}
		if !isSafeDiagnosticToken(resp.CLIVersion) {
			return fmt.Errorf("cli-v1 description: cli_version must be a bounded diagnostic token")
		}
		return validateCapabilities(resp.Capabilities)
	case TypeValidated:
		return nil
	case TypeActivity:
		if resp.Kind != ActivityKindTool {
			return fmt.Errorf("cli-v1 activity: kind must be %q", ActivityKindTool)
		}
		if !isSafeDiagnosticToken(resp.Name) {
			return fmt.Errorf("cli-v1 activity: name must be a bounded diagnostic token")
		}
		if resp.Status != ActivityStatusCompleted && resp.Status != ActivityStatusError {
			return fmt.Errorf("cli-v1 activity: status must be %q or %q", ActivityStatusCompleted, ActivityStatusError)
		}
		return nil
	case TypeResult:
		if strings.TrimSpace(resp.Text) == "" {
			return fmt.Errorf("cli-v1 result: text must not be empty")
		}
		if resp.FinishReason != FinishReasonStop {
			return fmt.Errorf("cli-v1 result: finish_reason must be %q", FinishReasonStop)
		}
		return nil
	case TypeError:
		if !validErrorCode(resp.Code) {
			return fmt.Errorf("cli-v1 error: code is not defined by version 1")
		}
		if strings.TrimSpace(resp.Message) == "" {
			return fmt.Errorf("cli-v1 error: message must not be empty")
		}
		return nil
	default:
		return fmt.Errorf("cli-v1: unknown response type %q", resp.Type)
	}
}

func validateCapabilities(capabilities []string) error {
	if len(capabilities) == 0 {
		return fmt.Errorf("cli-v1 description: capabilities must not be empty")
	}
	seen := make(map[string]struct{}, len(capabilities))
	hasText := false
	for _, capability := range capabilities {
		if !isSafeDiagnosticToken(capability) {
			return fmt.Errorf("cli-v1 description: capability must be a bounded diagnostic token")
		}
		if _, duplicate := seen[capability]; duplicate {
			return fmt.Errorf("cli-v1 description: duplicate capability %q", capability)
		}
		seen[capability] = struct{}{}
		hasText = hasText || capability == CapabilityText
	}
	if !hasText {
		return fmt.Errorf("cli-v1 description: %q capability is required", CapabilityText)
	}
	return nil
}

func validErrorCode(code string) bool {
	switch code {
	case CodeInvalidRequest, CodeUnsupported, CodeExecutableMissing, CodeProtocolError,
		CodeProcessFailed, CodeTimeout, CodeNoResponse:
		return true
	default:
		return false
	}
}

// isSafeDiagnosticToken prevents child-controlled descriptive fields from
// becoming arbitrary multiline content when included in operational output.
func isSafeDiagnosticToken(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') {
			continue
		}
		switch char {
		case '-', '_', '.', ':', '/':
			continue
		default:
			return false
		}
	}
	return true
}
