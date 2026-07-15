// Package agentcli adapts an external agent CLI (through a cli-v1 shim
// subprocess) to ADK's model.LLM boundary. The CLI is a complete nested agent:
// ADK receives only its final text, never portable function calls.
package agentcli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/cliprotocol"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var (
	// ErrStreamingUnsupported is returned because a CLI run is one complete
	// external agent invocation.
	ErrStreamingUnsupported = errors.New("streaming model responses are not supported by agent CLI providers")
	// ErrToolsUnsupported is returned before process launch when ADK supplies
	// tool declarations or function-calling configuration.
	ErrToolsUnsupported = errors.New("ADK tools and function calling are not supported by agent CLI providers")
	// ErrUnsupportedPart indicates non-text content (function history, images,
	// audio, or binary parts).
	ErrUnsupportedPart = errors.New("only text model content is supported by agent CLI providers")
	// ErrUnsupportedConfig indicates generation settings that cli-v1 cannot
	// represent.
	ErrUnsupportedConfig = errors.New("generation settings are not supported by agent CLI providers")
	// ErrNoUserTurn indicates a request whose transcript does not end in a user
	// message.
	ErrNoUserTurn = errors.New("agent CLI request must end in a user message")
)

// ShimError is a terminal cli-v1 error returned by the mapper process.
type ShimError struct {
	Code      string
	Message   string
	Retryable bool
	Cause     error
}

func (e *ShimError) Error() string {
	return fmt.Sprintf("agent CLI shim error %s: %s", e.Code, e.Message)
}

// Unwrap preserves context cancellation and deadline classification.
func (e *ShimError) Unwrap() error { return e.Cause }

// ProtocolViolation reports malformed or out-of-contract shim output.
type ProtocolViolation struct {
	Reason string
}

func (e *ProtocolViolation) Error() string {
	return "agent CLI protocol violation: " + e.Reason
}

// SelfCommand selects the currently running executable as the shim command.
const SelfCommand = "self"

// ResolveCommand resolves a declarative shim command into an executable path.
// "self" resolves through os.Executable; anything else through exec.LookPath.
func ResolveCommand(command string) (string, error) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return "", errors.New("shim command must not be empty")
	}
	if trimmed == SelfCommand {
		executable, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("resolve current executable for shim command %q: %w", SelfCommand, err)
		}
		return executable, nil
	}
	resolved, err := exec.LookPath(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve shim command %q: %w", trimmed, err)
	}
	return resolved, nil
}

// Config wires one agent_cli resolved model into a subprocess-backed LLM.
type Config struct {
	// Command is the resolved absolute shim executable path.
	Command string
	// Args are static trusted arguments configured in provider YAML.
	Args []string
	// Profile is the portable CLI profile forwarded verbatim to the shim.
	Profile cliprotocol.Profile
	// Workspace is the complete canonical project registry. Projects are
	// sorted deterministically by name before every request.
	Workspace cliprotocol.Workspace
	// ContextLimits bounds the serialized transcript (messages and Unicode
	// code points) before process launch.
	ContextLimits domain.ContextLimits
	// WorkingDir is the canonical application root used as the shim process
	// working directory.
	WorkingDir string

	// MaxStdoutBytes bounds aggregate protocol stdout. Zero applies a default.
	MaxStdoutBytes int
	// MaxLineBytes bounds one NDJSON line. Zero applies a default.
	MaxLineBytes int
	// MaxStderrBytes bounds captured diagnostics. Zero applies a default.
	MaxStderrBytes int

	// Logger receives bounded diagnostic events. Optional.
	Logger port.Logger
	// Sanitize redacts known credentials before diagnostic text reaches errors
	// or logs. Operational composition should provide secure.Redactor.String.
	Sanitize func(string) string
}

const (
	defaultMaxStdoutBytes = 4 << 20
	defaultMaxLineBytes   = 1 << 20
	defaultMaxStderrBytes = 8 << 10
	defaultWaitDelay      = 5 * time.Second
)

// LLM implements ADK's model.LLM through one shim subprocess per model call.
type LLM struct {
	command        string
	args           []string
	profile        cliprotocol.Profile
	workspace      cliprotocol.Workspace
	contextLimits  domain.ContextLimits
	workingDir     string
	maxStdoutBytes int
	maxLineBytes   int
	maxStderrBytes int
	logger         port.Logger
	sanitize       func(string) string
	newID          func(method string) string
}

var _ model.LLM = (*LLM)(nil)

// New validates configuration and constructs the adapter.
func New(cfg Config) (*LLM, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("agent CLI shim command is required")
	}
	if strings.TrimSpace(cfg.Profile.Model) == "" {
		return nil, errors.New("agent CLI profile model is required")
	}
	if strings.TrimSpace(cfg.WorkingDir) == "" {
		return nil, errors.New("agent CLI working directory is required")
	}
	workspace := cfg.Workspace
	workspace.Projects = append([]cliprotocol.Project(nil), workspace.Projects...)
	sort.Slice(workspace.Projects, func(i, j int) bool {
		return workspace.Projects[i].Name < workspace.Projects[j].Name
	})
	probe := cliprotocol.NewRequest("startup-validate", cliprotocol.MethodValidate)
	profile := cfg.Profile
	probe.Profile = &profile
	probe.Workspace = &workspace
	if err := cliprotocol.ValidateRequest(probe); err != nil {
		return nil, fmt.Errorf("agent CLI configuration: %w", err)
	}

	llm := &LLM{
		command:        cfg.Command,
		args:           append([]string(nil), cfg.Args...),
		profile:        cfg.Profile,
		workspace:      workspace,
		contextLimits:  cfg.ContextLimits,
		workingDir:     cfg.WorkingDir,
		maxStdoutBytes: valueOrDefault(cfg.MaxStdoutBytes, defaultMaxStdoutBytes),
		maxLineBytes:   valueOrDefault(cfg.MaxLineBytes, defaultMaxLineBytes),
		maxStderrBytes: valueOrDefault(cfg.MaxStderrBytes, defaultMaxStderrBytes),
		logger:         cfg.Logger,
		sanitize:       cfg.Sanitize,
		newID:          randomRequestID,
	}
	return llm, nil
}

func valueOrDefault(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func randomRequestID(method string) string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return method + "-" + fmt.Sprint(time.Now().UnixNano())
	}
	return method + "-" + hex.EncodeToString(buffer)
}

// Name returns the native CLI model reference.
func (l *LLM) Name() string {
	if l == nil {
		return ""
	}
	return l.profile.Model
}

// Describe performs a cli-v1 describe exchange without a model call.
func (l *LLM) Describe(ctx context.Context) (cliprotocol.Response, error) {
	request := cliprotocol.NewRequest(l.newID(cliprotocol.MethodDescribe), cliprotocol.MethodDescribe)
	return l.exchange(ctx, request)
}

// Validate performs a profile-aware cli-v1 validate exchange without a model
// call.
func (l *LLM) Validate(ctx context.Context) error {
	request := cliprotocol.NewRequest(l.newID(cliprotocol.MethodValidate), cliprotocol.MethodValidate)
	profile := l.profile
	workspace := l.workspace
	request.Profile = &profile
	request.Workspace = &workspace
	_, err := l.exchange(ctx, request)
	return err
}

// GenerateContent converts one ADK request into one shim run and yields at most
// one ADK text response.
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
		runRequest, err := l.runRequest(request)
		if err != nil {
			yield(nil, err)
			return
		}
		terminal, err := l.exchange(ctx, runRequest)
		if err != nil {
			yield(nil, err)
			return
		}
		yield(&model.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromText(terminal.Text)},
			},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// runRequest converts the supported ADK text subset into a bounded cli-v1 run
// request. All unsupported inputs fail before process launch.
func (l *LLM) runRequest(request *model.LLMRequest) (cliprotocol.Request, error) {
	if request == nil {
		return cliprotocol.Request{}, errors.New("ADK model request is nil")
	}
	if len(request.Tools) > 0 {
		return cliprotocol.Request{}, ErrToolsUnsupported
	}
	systemInstruction := ""
	if request.Config != nil {
		if err := rejectUnsupportedConfig(request.Config); err != nil {
			return cliprotocol.Request{}, err
		}
		if request.Config.SystemInstruction != nil {
			text, err := textOnly(request.Config.SystemInstruction)
			if err != nil {
				return cliprotocol.Request{}, fmt.Errorf("convert system instruction: %w", err)
			}
			systemInstruction = text
		}
	}

	history := make([]domain.Message, 0, len(request.Contents))
	for index, content := range request.Contents {
		if content == nil {
			return cliprotocol.Request{}, fmt.Errorf("content %d: %w", index, ErrUnsupportedPart)
		}
		role, err := messageRole(content.Role)
		if err != nil {
			return cliprotocol.Request{}, fmt.Errorf("content %d: %w", index, err)
		}
		text, err := textOnly(content)
		if err != nil {
			return cliprotocol.Request{}, fmt.Errorf("content %d: %w", index, err)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		history = append(history, domain.Message{Role: role, Content: text})
	}
	if len(history) == 0 {
		return cliprotocol.Request{}, errors.New("ADK model request contains no text messages")
	}
	if history[len(history)-1].Role != domain.RoleUser {
		return cliprotocol.Request{}, ErrNoUserTurn
	}

	bounded := history
	if l.contextLimits.MaxMessages > 0 && l.contextLimits.MaxChars > 0 {
		bounded = domain.LimitMessages(history, l.contextLimits)
	}
	if len(bounded) == 0 || bounded[len(bounded)-1].Role != domain.RoleUser {
		return cliprotocol.Request{}, ErrNoUserTurn
	}

	messages := make([]cliprotocol.Message, 0, len(bounded))
	for _, message := range bounded {
		role := cliprotocol.RoleUser
		if message.Role == domain.RoleAssistant {
			role = cliprotocol.RoleAssistant
		}
		messages = append(messages, cliprotocol.Message{Role: role, Text: message.Content})
	}

	runRequest := cliprotocol.NewRequest(l.newID(cliprotocol.MethodRun), cliprotocol.MethodRun)
	profile := l.profile
	workspace := l.workspace
	runRequest.Profile = &profile
	runRequest.Workspace = &workspace
	runRequest.SystemInstruction = systemInstruction
	runRequest.Messages = messages
	if err := cliprotocol.ValidateRequest(runRequest); err != nil {
		return cliprotocol.Request{}, err
	}
	return runRequest, nil
}

func rejectUnsupportedConfig(cfg *genai.GenerateContentConfig) error {
	if len(cfg.Tools) > 0 || cfg.ToolConfig != nil {
		return ErrToolsUnsupported
	}
	if cfg.ResponseSchema != nil || cfg.ResponseJsonSchema != nil || (cfg.ResponseMIMEType != "" && cfg.ResponseMIMEType != "text/plain") {
		return fmt.Errorf("%w: structured response formats", ErrUnsupportedConfig)
	}

	// Only SystemInstruction and the default/text/plain response MIME type are
	// representable by cli-v1. Reflection makes newly added SDK fields fail
	// closed instead of silently changing nested-agent behavior.
	value := reflect.ValueOf(*cfg)
	typeOfConfig := value.Type()
	for index := 0; index < value.NumField(); index++ {
		name := typeOfConfig.Field(index).Name
		switch name {
		case "SystemInstruction", "ResponseMIMEType", "ResponseSchema", "ResponseJsonSchema", "Tools", "ToolConfig":
			continue
		}
		if !value.Field(index).IsZero() {
			return fmt.Errorf("%w: %s", ErrUnsupportedConfig, name)
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
		if part == nil {
			return "", ErrUnsupportedPart
		}
		if part.FunctionCall != nil || part.FunctionResponse != nil || part.ToolCall != nil || part.ToolResponse != nil {
			return "", ErrUnsupportedPart
		}
		if part.InlineData != nil || part.FileData != nil || part.CodeExecutionResult != nil ||
			part.ExecutableCode != nil || part.VideoMetadata != nil || part.MediaResolution != nil ||
			part.Thought || len(part.ThoughtSignature) > 0 || len(part.PartMetadata) > 0 {
			return "", ErrUnsupportedPart
		}
		text.WriteString(part.Text)
	}
	return text.String(), nil
}
