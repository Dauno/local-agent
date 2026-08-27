// Package agentcli adapts a declarative external agent CLI to ADK's model.LLM
// boundary. The CLI is a nested agent and returns final text only.
package agentcli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var (
	ErrStreamingUnsupported = errors.New("streaming model responses are not supported by agent CLI providers")
	ErrToolsUnsupported     = errors.New("ADK tools and function calling are not supported by agent CLI providers")
	ErrUnsupportedPart      = errors.New("only text model content is supported by agent CLI providers")
	ErrUnsupportedConfig    = errors.New("generation settings are not supported by agent CLI providers")
	ErrNoUserTurn           = errors.New("agent CLI request must end in a user message")
)

const (
	CodeInvalidRequest    = "invalid_request"
	CodeUnsupported       = "unsupported"
	CodeExecutableMissing = "executable_not_found"
	CodeProcessFailed     = "process_failed"
	CodeTimeout           = "timeout"
	CodeNoResponse        = "no_response"
	// CodeLineTooLarge and CodeStdoutTooLarge distinguish a local adapter
	// bound from an actual process crash: both are protocol violations, but
	// only these two are fixed by raising MaxLineBytes/MaxStdoutBytes rather
	// than by the CLI or the network.
	CodeLineTooLarge   = "protocol_line_too_large"
	CodeStdoutTooLarge = "protocol_stdout_too_large"
	// CodeProviderFailure means the CLI ran to completion and reported its own
	// failure through the stream (stream.failure matched), as opposed to the
	// adapter losing the process (CodeProcessFailed).
	CodeProviderFailure = "provider_reported_failure"
)

// CLIError classifies a direct native CLI failure.
type CLIError struct {
	Code      string
	Message   string
	Retryable bool
	Cause     error
}

func (e *CLIError) Error() string { return fmt.Sprintf("agent CLI error %s: %s", e.Code, e.Message) }
func (e *CLIError) Unwrap() error { return e.Cause }

// ProtocolViolation reports a malformed or oversized stream. Code is empty
// for violations that only ever mean a broken stream (malformed NDJSON, an
// event after the terminal event, an unresolved session ID); it is set to
// CodeLineTooLarge or CodeStdoutTooLarge for the two violations that are
// actually a local adapter bound, not a CLI-side problem.
type ProtocolViolation struct {
	Reason string
	Code   string
}

func (e *ProtocolViolation) Error() string { return "agent CLI stream violation: " + e.Reason }

type Description struct {
	Name       string
	CLIVersion string
}

type Config struct {
	Command        string
	Provider       agentdef.Provider
	Profile        agentdef.Profile
	Workspace      domain.Workspace
	ContextLimits  domain.ContextLimits
	WorkingDir     string
	MaxStdoutBytes int
	MaxLineBytes   int
	MaxStderrBytes int
	Logger         port.Logger
	Sanitize       func(string) string
}

const (
	// defaultMaxLineBytes bounds one NDJSON event. A single tool-output event
	// can exceed the old 1 MiB bound. The separate stdout limit keeps the whole
	// exchange bounded while allowing several large activity events.
	defaultMaxStdoutBytes = 16 << 20
	defaultMaxLineBytes   = 4 << 20
	defaultMaxStderrBytes = 8 << 10
	defaultWaitDelay      = 5 * time.Second
)

type LLM struct {
	command        string
	provider       agentdef.Provider
	profile        agentdef.Profile
	workspace      domain.Workspace
	contextLimits  domain.ContextLimits
	workingDir     string
	maxStdoutBytes int
	maxLineBytes   int
	maxStderrBytes int
	logger         port.Logger
	sanitize       func(string) string
}

var _ model.LLM = (*LLM)(nil)

func ResolveCommand(command string) (string, error) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return "", errors.New("agent CLI executable must not be empty")
	}
	resolved, err := exec.LookPath(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve agent CLI executable %q: %w", trimmed, err)
	}
	return resolved, nil
}

func New(cfg Config) (*LLM, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("agent CLI executable is required")
	}
	if strings.TrimSpace(cfg.Profile.Model) == "" {
		return nil, errors.New("agent CLI profile model is required")
	}
	if strings.TrimSpace(cfg.WorkingDir) == "" {
		return nil, errors.New("agent CLI working directory is required")
	}
	if cfg.Provider.Version == nil || cfg.Provider.Invocation == nil || cfg.Provider.Stream == nil {
		return nil, errors.New("agent CLI descriptor is incomplete")
	}
	workspace := cfg.Workspace
	workspace.Projects = append([]domain.Project(nil), workspace.Projects...)
	slices.SortFunc(workspace.Projects, func(a, b domain.Project) int { return cmp.Compare(a.Name, b.Name) })
	return &LLM{
		command: cfg.Command, provider: cfg.Provider, profile: cfg.Profile, workspace: workspace,
		contextLimits: cfg.ContextLimits, workingDir: cfg.WorkingDir,
		maxStdoutBytes: valueOrDefault(cfg.MaxStdoutBytes, defaultMaxStdoutBytes),
		maxLineBytes:   valueOrDefault(cfg.MaxLineBytes, defaultMaxLineBytes),
		maxStderrBytes: valueOrDefault(cfg.MaxStderrBytes, defaultMaxStderrBytes),
		logger:         cfg.Logger, sanitize: cfg.Sanitize,
	}, nil
}

func valueOrDefault(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func (l *LLM) Name() string {
	if l == nil {
		return ""
	}
	return l.profile.Model
}

// Describe probes only the native version. It makes no model call.
func (l *LLM) Describe(ctx context.Context) (Description, error) {
	version, err := l.probeVersion(ctx)
	if err != nil {
		return Description{}, err
	}
	return Description{Name: l.provider.Name, CLIVersion: version}, nil
}

// Validate checks the native version at startup. Preconditions are not checked
// here: they describe the selected project, and no project is chosen until a
// caller delegates. See checkPreconditions.
func (l *LLM) Validate(ctx context.Context) error {
	_, err := l.probeVersion(ctx)
	return err
}

// checkPreconditions runs every declared precondition against the project the
// caller selected. It runs for each delegation because the answer changes with
// the workspace.
func (l *LLM) checkPreconditions(ctx context.Context, workingDir string) error {
	for _, precondition := range l.provider.Preconditions {
		args := substituteArgs(precondition.Command, map[string]string{"workdir": workingDir})
		if len(args) == 0 {
			return &CLIError{Code: CodeInvalidRequest, Message: "descriptor precondition.command resolved to no command"}
		}
		output, err := l.captureIn(ctx, workingDir, args[0], args[1:])
		if err != nil || strings.TrimSpace(output) != precondition.Expect {
			return &CLIError{Code: CodeInvalidRequest, Message: precondition.Message, Cause: err}
		}
	}
	return nil
}

func (l *LLM) GenerateContent(ctx context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stream {
			yield(nil, ErrStreamingUnsupported)
			return
		}
		if l == nil {
			yield(nil, errors.New("agent CLI model is nil"))
			return
		}
		run, err := l.runRequest(request)
		if err != nil {
			yield(nil, err)
			return
		}
		text, err := l.exchange(ctx, run)
		if err != nil {
			yield(nil, err)
			return
		}
		yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText(text)}}, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

type request struct {
	systemInstruction string
	messages          []domain.Message
	// workingDir is the canonical path of the project the caller selected. It
	// is resolved from the registry for every call, never taken from the
	// process working directory.
	workingDir string
	project    string
}

// delegation is the argument object an agent CLI leaf exposes to its caller.
// ADK validates it against the declared input schema and hands it over as JSON
// in the user turn, so the project name always comes from a schema-checked
// field rather than from free text.
type delegation struct {
	Project string `json:"project"`
	Task    string `json:"task"`
}

// resolveProject maps a caller-supplied project name onto a registered path.
// The name is untrusted, so it is only ever used as a map key. A path is never
// built from it.
func (l *LLM) resolveProject(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", &CLIError{Code: CodeInvalidRequest, Message: "project must not be empty; " + l.registeredProjects()}
	}
	for _, project := range l.workspace.Projects {
		if project.Name == trimmed {
			return project.Path, nil
		}
	}
	return "", &CLIError{Code: CodeInvalidRequest, Message: fmt.Sprintf("project %q is not registered; %s", trimmed, l.registeredProjects())}
}

func (l *LLM) registeredProjects() string {
	names := make([]string, 0, len(l.workspace.Projects))
	for _, project := range l.workspace.Projects {
		names = append(names, project.Name)
	}
	if len(names) == 0 {
		return "no projects are registered"
	}
	return "registered projects: " + strings.Join(names, ", ")
}

func (l *LLM) runRequest(modelRequest *model.LLMRequest) (request, error) {
	if modelRequest == nil {
		return request{}, errors.New("ADK model request is nil")
	}
	if len(modelRequest.Tools) > 0 {
		return request{}, ErrToolsUnsupported
	}
	systemInstruction := ""
	if modelRequest.Config != nil {
		if err := rejectUnsupportedConfig(modelRequest.Config); err != nil {
			return request{}, err
		}
		if modelRequest.Config.SystemInstruction != nil {
			text, err := textOnly(modelRequest.Config.SystemInstruction)
			if err != nil {
				return request{}, fmt.Errorf("convert system instruction: %w", err)
			}
			systemInstruction = text
		}
	}
	history := make([]domain.Message, 0, len(modelRequest.Contents))
	for index, content := range modelRequest.Contents {
		if content == nil {
			return request{}, fmt.Errorf("content %d: %w", index, ErrUnsupportedPart)
		}
		role, err := messageRole(content.Role)
		if err != nil {
			return request{}, fmt.Errorf("content %d: %w", index, err)
		}
		text, err := textOnly(content)
		if err != nil {
			return request{}, fmt.Errorf("content %d: %w", index, err)
		}
		if strings.TrimSpace(text) != "" {
			history = append(history, domain.Message{Role: role, Content: text})
		}
	}
	if len(history) == 0 {
		return request{}, errors.New("ADK model request contains no text messages")
	}
	if history[len(history)-1].Role != domain.RoleUser {
		return request{}, ErrNoUserTurn
	}
	if l.contextLimits.MaxMessages > 0 && l.contextLimits.MaxChars > 0 {
		history = domain.LimitMessages(history, l.contextLimits)
	}
	if len(history) == 0 || history[len(history)-1].Role != domain.RoleUser {
		return request{}, ErrNoUserTurn
	}
	last := len(history) - 1
	selected, err := parseDelegation(history[last].Content)
	if err != nil {
		return request{}, err
	}
	workingDir, err := l.resolveProject(selected.Project)
	if err != nil {
		return request{}, err
	}
	// Only the task text reaches the CLI. The project name was routing data.
	history[last].Content = selected.Task
	return request{
		systemInstruction: systemInstruction,
		messages:          history,
		workingDir:        workingDir,
		project:           selected.Project,
	}, nil
}

// parseDelegation reads the {project, task} object that ADK serializes from the
// leaf's declared input schema. A caller that sends anything else is rejected
// rather than defaulted, because a default would pick a workspace on its own.
func parseDelegation(content string) (delegation, error) {
	var selected delegation
	if err := json.Unmarshal([]byte(content), &selected); err != nil {
		return delegation{}, &CLIError{Code: CodeInvalidRequest, Message: "agent CLI request must be a JSON object with project and task"}
	}
	if strings.TrimSpace(selected.Task) == "" {
		return delegation{}, &CLIError{Code: CodeInvalidRequest, Message: "task must not be empty"}
	}
	return selected, nil
}

func rejectUnsupportedConfig(cfg *genai.GenerateContentConfig) error {
	if len(cfg.Tools) > 0 || cfg.ToolConfig != nil {
		return ErrToolsUnsupported
	}
	if cfg.ResponseSchema != nil || cfg.ResponseJsonSchema != nil || (cfg.ResponseMIMEType != "" && cfg.ResponseMIMEType != "text/plain") {
		return fmt.Errorf("%w: structured response formats", ErrUnsupportedConfig)
	}
	value := reflect.ValueOf(*cfg)
	typeOfConfig := value.Type()
	for index := 0; index < value.NumField(); index++ {
		switch typeOfConfig.Field(index).Name {
		case "SystemInstruction", "ResponseMIMEType", "ResponseSchema", "ResponseJsonSchema", "Tools", "ToolConfig":
			continue
		}
		if !value.Field(index).IsZero() {
			return fmt.Errorf("%w: %s", ErrUnsupportedConfig, typeOfConfig.Field(index).Name)
		}
	}
	return nil
}

func messageRole(role string) (domain.Role, error) {
	switch role {
	case "", string(genai.RoleUser):
		return domain.RoleUser, nil
	case string(genai.RoleModel):
		return domain.RoleAssistant, nil
	default:
		return "", fmt.Errorf("unsupported ADK role %q: %w", role, ErrUnsupportedPart)
	}
}

func textOnly(content *genai.Content) (string, error) {
	if content == nil {
		return "", ErrUnsupportedPart
	}
	var text strings.Builder
	for _, part := range content.Parts {
		if part == nil || part.FunctionCall != nil || part.FunctionResponse != nil || part.ToolCall != nil || part.ToolResponse != nil || part.InlineData != nil || part.FileData != nil ||
			part.CodeExecutionResult != nil ||
			part.ExecutableCode != nil ||
			part.VideoMetadata != nil ||
			part.MediaResolution != nil ||
			part.Thought ||
			len(part.ThoughtSignature) > 0 ||
			len(part.PartMetadata) > 0 {
			return "", ErrUnsupportedPart
		}
		text.WriteString(part.Text)
	}
	return text.String(), nil
}
