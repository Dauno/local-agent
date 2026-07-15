package opencodeshim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ErrExecutableNotFound indicates the OpenCode executable cannot be resolved.
var ErrExecutableNotFound = errors.New("opencode executable not found")

// AgentInfo is one OpenCode agent from `opencode agent list`.
type AgentInfo struct {
	Name    string
	Primary bool
}

// Process is one started `opencode run` invocation.
type Process interface {
	Stdout() io.Reader
	// Terminate stops the full OpenCode subprocess group. It is safe to call
	// after the process has already exited.
	Terminate() error
	// Wait reaps the child and returns its exit error, if any.
	Wait() error
	// Diagnostics returns content-free metadata for bounded native stderr.
	Diagnostics() ProcessDiagnostics
}

// ProcessDiagnostics is safe to include in errors because it never contains
// child-controlled stderr content.
type ProcessDiagnostics struct {
	StderrBytes     int64
	StderrTruncated bool
}

// Executor abstracts OpenCode invocations so tests can stay hermetic.
type Executor interface {
	Version(ctx context.Context) (string, error)
	ListModels(ctx context.Context) ([]string, error)
	ListAgents(ctx context.Context) ([]AgentInfo, error)
	StartRun(ctx context.Context, args []string, prompt string) (Process, error)
}

// NewExecutor creates the real PATH-resolved OpenCode executor.
func NewExecutor(executable string, bounds Bounds) Executor {
	return &realExecutor{executable: executable, bounds: bounds.withDefaults()}
}

type realExecutor struct {
	executable string
	bounds     Bounds
}

func (e *realExecutor) lookup() (string, error) {
	resolved, err := exec.LookPath(e.executable)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrExecutableNotFound, err)
	}
	return resolved, nil
}

func (e *realExecutor) capture(ctx context.Context, args ...string) (string, error) {
	resolved, err := e.lookup()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, resolved, args...)
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second
	stdout := newBoundedCapture(e.bounds.MaxRawStdoutBytes)
	stderr := newBoundedCapture(e.bounds.MaxRawStderrBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	runErr := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("opencode %s cancelled: %w", strings.Join(args, " "), ctxErr)
	}
	if stdout.Truncated() {
		return "", fmt.Errorf("opencode %s output exceeded %d bytes%s", strings.Join(args, " "), e.bounds.MaxRawStdoutBytes, diagnosticDetail(stderr.Summary()))
	}
	if runErr != nil {
		return "", fmt.Errorf("opencode %s %s%s", strings.Join(args, " "), processFailure(runErr), diagnosticDetail(stderr.Summary()))
	}
	return stdout.String(), nil
}

func (e *realExecutor) Version(ctx context.Context) (string, error) {
	output, err := e.capture(ctx, "--version")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(output, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed, nil
		}
	}
	return "", errors.New("opencode --version produced no output")
}

func (e *realExecutor) ListModels(ctx context.Context) ([]string, error) {
	output, err := e.capture(ctx, "models")
	if err != nil {
		return nil, err
	}
	return ParseModelList(output), nil
}

func (e *realExecutor) ListAgents(ctx context.Context) ([]AgentInfo, error) {
	output, err := e.capture(ctx, "agent", "list")
	if err != nil {
		return nil, err
	}
	return ParseAgentList(output), nil
}

// ParseModelList extracts provider/model identifiers from `opencode models`.
func ParseModelList(output string) []string {
	var models []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && strings.Contains(trimmed, "/") {
			models = append(models, trimmed)
		}
	}
	return models
}

var agentLinePattern = regexp.MustCompile(`^(\S.*?) \((primary|subagent)\)\s*$`)

// ParseAgentList extracts agent names and primary markers from
// `opencode agent list`. Indented permission JSON between markers is ignored.
func ParseAgentList(output string) []AgentInfo {
	var agents []AgentInfo
	for _, line := range strings.Split(output, "\n") {
		match := agentLinePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		agents = append(agents, AgentInfo{Name: match[1], Primary: match[2] == "primary"})
	}
	return agents
}

func (e *realExecutor) StartRun(ctx context.Context, args []string, prompt string) (Process, error) {
	resolved, err := e.lookup()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, resolved, args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = os.Environ()
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open opencode stdout: %w", err)
	}
	stderr := newBoundedCapture(e.bounds.MaxRawStderrBytes)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start opencode: %w", err)
	}

	return &realProcess{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

type realProcess struct {
	cmd    *exec.Cmd
	stdout io.Reader
	stderr *boundedCapture
}

func (p *realProcess) Stdout() io.Reader { return p.stdout }

func (p *realProcess) Terminate() error { return killProcessGroup(p.cmd) }

func (p *realProcess) Wait() error {
	return p.cmd.Wait()
}

func (p *realProcess) Diagnostics() ProcessDiagnostics {
	if p == nil || p.stderr == nil {
		return ProcessDiagnostics{}
	}
	return p.stderr.Summary()
}

type boundedCapture struct {
	limit int
	data  []byte
	total int64
}

func newBoundedCapture(limit int) *boundedCapture {
	return &boundedCapture{limit: limit}
}

func (b *boundedCapture) Write(data []byte) (int, error) {
	b.total += int64(len(data))
	remaining := b.limit - len(b.data)
	if remaining > len(data) {
		remaining = len(data)
	}
	if remaining > 0 {
		b.data = append(b.data, data[:remaining]...)
	}
	return len(data), nil
}

func (b *boundedCapture) String() string { return string(b.data) }

func (b *boundedCapture) Truncated() bool { return b.total > int64(b.limit) }

func (b *boundedCapture) Summary() ProcessDiagnostics {
	return ProcessDiagnostics{StderrBytes: b.total, StderrTruncated: b.Truncated()}

}

func processFailure(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return fmt.Sprintf("exited with code %d", code)
		}
		return "was terminated"
	}
	return "failed"

}

func diagnosticDetail(diagnostic ProcessDiagnostics) string {
	if diagnostic.StderrBytes == 0 {
		return ""
	}
	if diagnostic.StderrTruncated {
		return " (native stderr omitted; bounded capture was truncated)"
	}
	return fmt.Sprintf(" (native stderr omitted; %d bytes)", diagnostic.StderrBytes)
}
