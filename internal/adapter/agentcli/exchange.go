package agentcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/cliprotocol"
)

// exchange runs the shim for exactly one request and returns its terminal
// response. The whole subprocess tree is terminated on cancellation.
func (l *LLM) exchange(ctx context.Context, request cliprotocol.Request) (cliprotocol.Response, error) {
	line, err := cliprotocol.EncodeLine(request)
	if err != nil {
		return cliprotocol.Response{}, err
	}

	cmd := exec.CommandContext(ctx, l.command, l.args...)
	cmd.Dir = l.workingDir
	cmd.Env = os.Environ()
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = defaultWaitDelay

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return cliprotocol.Response{}, fmt.Errorf("open shim stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return cliprotocol.Response{}, fmt.Errorf("open shim stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return cliprotocol.Response{}, fmt.Errorf("open shim stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return cliprotocol.Response{}, classifyStartError(l.command, err)
	}

	go func() {
		defer stdin.Close()
		_, _ = stdin.Write(line)
	}()

	stderrCh := make(chan diagnosticSummary, 1)
	go func() {
		stderrCh <- readDiagnostic(stderr, l.maxStderrBytes)
	}()

	terminal, readErr := l.readStdout(stdout, request.Method, request.ID)
	if readErr != nil {
		// A parser/bound failure stops consumption early. Kill before waiting so
		// a child that keeps writing cannot block forever on a full stdout pipe.
		_ = killProcessGroup(cmd)
		_, _ = io.Copy(io.Discard, stdout)
	}
	stderrDiagnostic := <-stderrCh
	waitErr := cmd.Wait()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return cliprotocol.Response{}, &ShimError{
			Code:      cliprotocol.CodeTimeout,
			Message:   "agent CLI call cancelled",
			Retryable: errors.Is(ctxErr, context.DeadlineExceeded),
			Cause:     ctxErr,
		}
	}

	if readErr != nil {
		return cliprotocol.Response{}, l.annotateProtocol(readErr, stderrDiagnostic)
	}

	if terminal == nil {
		if waitErr != nil {
			return cliprotocol.Response{}, &ShimError{
				Code:    cliprotocol.CodeProcessFailed,
				Message: "shim exited without a terminal message" + processExitSuffix(cmd) + diagnosticSuffix("stderr", stderrDiagnostic),
			}
		}
		return cliprotocol.Response{}, &ShimError{
			Code:    cliprotocol.CodeNoResponse,
			Message: "shim exited successfully without a terminal message" + diagnosticSuffix("stderr", stderrDiagnostic),
		}
	}

	if terminal.Type == cliprotocol.TypeError {
		return cliprotocol.Response{}, &ShimError{
			Code:      strings.TrimSpace(terminal.Code),
			Message:   "shim reported an error" + opaqueDetailSuffix("message", terminal.Message) + diagnosticSuffix("stderr", stderrDiagnostic),
			Retryable: terminal.Retryable,
		}
	}

	if waitErr != nil {
		return cliprotocol.Response{}, &ShimError{
			Code:    cliprotocol.CodeProcessFailed,
			Message: "shim reported success but exited non-zero" + processExitSuffix(cmd) + diagnosticSuffix("stderr", stderrDiagnostic),
		}
	}

	return *terminal, nil
}

// readStdout drains the shim's protocol stream to EOF, enforcing bounds and
// requiring exactly one terminal message. Activity events are logged only.
func (l *LLM) readStdout(reader io.Reader, method, id string) (*cliprotocol.Response, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), l.maxLineBytes)

	var terminal *cliprotocol.Response
	total := 0
	for scanner.Scan() {
		lineBytes := scanner.Bytes()
		total += len(lineBytes) + 1
		if total > l.maxStdoutBytes {
			return nil, &ProtocolViolation{Reason: fmt.Sprintf("stdout exceeded %d bytes", l.maxStdoutBytes)}
		}
		if strings.TrimSpace(string(lineBytes)) == "" {
			continue
		}
		var resp cliprotocol.Response
		if err := json.Unmarshal(lineBytes, &resp); err != nil {
			return nil, &ProtocolViolation{Reason: fmt.Sprintf("malformed NDJSON line: %v", err)}
		}
		if resp.Protocol != cliprotocol.Protocol {
			return nil, &ProtocolViolation{Reason: "unexpected protocol identifier"}
		}
		if resp.Version != cliprotocol.Version {
			return nil, &ProtocolViolation{Reason: fmt.Sprintf("unsupported protocol version %d", resp.Version)}
		}
		if resp.ID != id {
			return nil, &ProtocolViolation{Reason: "mismatched response id"}
		}

		switch {
		case cliprotocol.IsTerminal(resp.Type):
			if terminal != nil {
				return nil, &ProtocolViolation{Reason: "more than one terminal message"}
			}
			if err := cliprotocol.ValidateResponse(resp); err != nil {
				return nil, &ProtocolViolation{Reason: err.Error()}
			}
			expected, _ := cliprotocol.TerminalTypeForMethod(method)
			if resp.Type != expected && resp.Type != cliprotocol.TypeError {
				return nil, &ProtocolViolation{Reason: fmt.Sprintf("unexpected terminal %q for method %q", resp.Type, method)}
			}
			captured := resp
			terminal = &captured
		case resp.Type == cliprotocol.TypeActivity:
			if method != cliprotocol.MethodRun {
				return nil, &ProtocolViolation{Reason: fmt.Sprintf("activity is not allowed for method %q", method)}
			}
			if err := cliprotocol.ValidateResponse(resp); err != nil {
				return nil, &ProtocolViolation{Reason: err.Error()}
			}
			l.logActivity(resp)
		default:
			// Unknown non-terminal event: ignore for forward compatibility.
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, &ProtocolViolation{Reason: fmt.Sprintf("shim emitted a line longer than %d bytes", l.maxLineBytes)}
		}
		return nil, &ProtocolViolation{Reason: fmt.Sprintf("read shim stdout: %v", err)}
	}
	return terminal, nil
}

func (l *LLM) logActivity(resp cliprotocol.Response) {
	if l.logger == nil {
		return
	}
	l.logger.Debug("agent CLI native activity",
		"kind", resp.Kind,
		"status", resp.Status)
}

type diagnosticSummary struct {
	bytes     int64
	limit     int64
	truncated bool
}

func readDiagnostic(reader io.Reader, limit int) diagnosticSummary {
	if limit <= 0 {
		limit = defaultMaxStderrBytes
	}
	written, _ := io.Copy(io.Discard, reader)
	return diagnosticSummary{bytes: written, limit: int64(limit), truncated: written > int64(limit)}
}

func classifyStartError(command string, err error) error {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return &ShimError{
			Code:    cliprotocol.CodeExecutableMissing,
			Message: fmt.Sprintf("shim executable %q not found or not runnable: %v", command, err),
		}
	}
	return &ShimError{
		Code:    cliprotocol.CodeProcessFailed,
		Message: fmt.Sprintf("start shim %q: %v", command, err),
	}
}

func (l *LLM) annotateProtocol(err error, stderrDiagnostic diagnosticSummary) error {
	var violation *ProtocolViolation
	if errors.As(err, &violation) {
		return &ShimError{
			Code:    cliprotocol.CodeProtocolError,
			Message: l.sanitizeText(violation.Reason) + diagnosticSuffix("stderr", stderrDiagnostic),
		}
	}
	return err
}

func (l *LLM) sanitizeText(text string) string {
	text = strings.Map(func(value rune) rune {
		if value < 0x20 && value != '\t' {
			return ' '
		}
		return value
	}, text)
	if l != nil && l.sanitize != nil {
		text = l.sanitize(text)
	}
	return strings.TrimSpace(text)
}

func diagnosticSuffix(label string, diagnostic diagnosticSummary) string {
	if diagnostic.bytes == 0 {
		return ""
	}
	if diagnostic.truncated {
		return fmt.Sprintf(" (%s omitted; more than %d bytes)", label, diagnostic.limit)
	}
	return fmt.Sprintf(" (%s omitted; %d bytes)", label, diagnostic.bytes)
}

func opaqueDetailSuffix(label, value string) string {
	if value == "" {
		return ""
	}
	return fmt.Sprintf(" (%s omitted; %d bytes)", label, len(value))
}

func processExitSuffix(cmd *exec.Cmd) string {
	if cmd != nil && cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			return fmt.Sprintf(" (exit code %d)", code)
		}
	}
	return " (terminated)"
}
